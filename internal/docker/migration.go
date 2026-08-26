package docker

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CheckMigrationReachable confirms, from the source machine, that the target's
// SSH port can be opened. It is the preflight for a direct migration: if the
// source cannot reach the target here (target behind NAT, firewalled, or wrong
// address), the migration is refused up front instead of failing part-way with
// a broken pipe.
func CheckMigrationReachable(ctx context.Context, targetIP string, targetSSHPort int) ([]string, error) {
	targetIP = strings.TrimSpace(targetIP)
	if targetIP == "" {
		return nil, fmt.Errorf("target_ip is required")
	}
	if targetSSHPort <= 0 {
		targetSSHPort = 22
	}
	addr := net.JoinHostPort(targetIP, strconv.Itoa(targetSSHPort))

	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf(
			"target %s is not reachable from this server: %w. Direct migration needs a routable path to the target's SSH port; a target behind NAT or a firewall cannot be migrated to this way",
			addr, err,
		)
	}
	_ = conn.Close()
	return []string{fmt.Sprintf("Target %s is reachable over SSH.", addr)}, nil
}

// PrepareMigrationTargetKey adds the ephemeral public key to authorized_keys on target VPS
func PrepareMigrationTargetKey(ctx context.Context, ephemeralPublicKey string) error {
	ephemeralPublicKey = strings.TrimSpace(ephemeralPublicKey)
	if ephemeralPublicKey == "" {
		return fmt.Errorf("ephemeral_key is required")
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = "/root"
	}
	sshDir := filepath.Join(homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return fmt.Errorf("failed to create .ssh directory: %w", err)
	}

	authKeysPath := filepath.Join(sshDir, "authorized_keys")
	f, err := os.OpenFile(authKeysPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open authorized_keys: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString("\n" + ephemeralPublicKey + "\n"); err != nil {
		return fmt.Errorf("failed to write key to authorized_keys: %w", err)
	}

	return nil
}

// CleanupMigrationTargetKey removes an ephemeral key from authorized_keys
func CleanupMigrationTargetKey(ctx context.Context, ephemeralPublicKey string) error {
	ephemeralPublicKey = strings.TrimSpace(ephemeralPublicKey)
	if ephemeralPublicKey == "" {
		return nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = "/root"
	}
	authKeysPath := filepath.Join(homeDir, ".ssh", "authorized_keys")
	content, err := os.ReadFile(authKeysPath)
	if err != nil {
		return nil
	}

	lines := strings.Split(string(content), "\n")
	filtered := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) != "" && !strings.Contains(l, ephemeralPublicKey) {
			filtered = append(filtered, l)
		}
	}

	return os.WriteFile(authKeysPath, []byte(strings.Join(filtered, "\n")), 0600)
}

// MigrateContainerToTarget exports, transfers via SCP/SSH, and restores a
// container on a target VPS. report, when set, receives a line for each step so
// the caller can stream progress into the task history; it also carries the
// output of the remote side, which is where the reason for a failure usually
// lives.
func (manager *Manager) MigrateContainerToTarget(
	ctx context.Context,
	containerID string,
	targetIP string,
	targetSSHPort int,
	ephemeralPrivateKey string,
	moveMode bool,
	report func(string),
) error {
	note := func(format string, args ...any) {
		if report != nil {
			report(fmt.Sprintf(format, args...))
		}
	}

	containerID = strings.TrimSpace(containerID)
	targetIP = strings.TrimSpace(targetIP)
	ephemeralPrivateKey = strings.TrimSpace(ephemeralPrivateKey)

	if containerID == "" || targetIP == "" || ephemeralPrivateKey == "" {
		return fmt.Errorf("container_id, target_ip, and ephemeral_key are required")
	}
	if targetSSHPort <= 0 {
		targetSSHPort = 22
	}

	// 1. Get container info (name, image, status)
	note("Inspecting container %s...", containerID)
	name, err := output(ctx, "inspect", "--format={{.Name}}", containerID)
	if err != nil {
		return fmt.Errorf("failed to inspect container name: %w", err)
	}
	containerName := strings.TrimPrefix(strings.TrimSpace(name), "/")

	// 2. Stop container on source
	note("Stopping container %s on source...", containerName)
	_, _ = combined(ctx, "stop", containerID)

	// 3. Commit container to temporary image tag
	tempImageTag := fmt.Sprintf("tug-migrated-%s:%d", containerID, time.Now().Unix())
	note("Committing container to image %s...", tempImageTag)
	if out, err := combined(ctx, "commit", containerID, tempImageTag); err != nil {
		return fmt.Errorf("failed to commit container image: %w\n%s", err, strings.TrimSpace(out))
	}
	defer func() {
		_, _ = combined(ctx, "rmi", tempImageTag)
	}()

	// 4. Write ephemeral private key to temp file
	keyPath := fmt.Sprintf("/tmp/tug_mig_key_%s", containerID)
	if err := os.WriteFile(keyPath, []byte(ephemeralPrivateKey+"\n"), 0600); err != nil {
		return fmt.Errorf("failed to write ephemeral private key: %w", err)
	}
	defer os.Remove(keyPath)

	// 5. Stream image to target VPS and run it
	note("Streaming image to %s:%d and restoring...", targetIP, targetSSHPort)
	sshArgs := []string{
		"-p", fmt.Sprintf("%d", targetSSHPort),
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=15",
		fmt.Sprintf("root@%s", targetIP),
		fmt.Sprintf("docker load && docker run -d --name %s %s", containerName, tempImageTag),
	}

	cmdSSH := execCommandContext(ctx, "ssh", sshArgs...)
	cmdSave := execCommandContext(ctx, "docker", "save", tempImageTag)

	// The remote side speaks over stdout/stderr: "docker load" prints the image
	// it read, "docker run" prints the new id, and any failure prints why. All
	// of it is captured so a broken migration is diagnosable from the logs
	// rather than a bare "exit status 1".
	var sshOut bytes.Buffer
	var saveErr bytes.Buffer
	cmdSSH.Stdout = &sshOut
	cmdSSH.Stderr = &sshOut
	cmdSave.Stderr = &saveErr

	// Pipe docker save directly to ssh
	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %w", err)
	}
	cmdSave.Stdout = pipeWriter
	cmdSSH.Stdin = pipeReader

	if err := cmdSave.Start(); err != nil {
		pipeReader.Close()
		pipeWriter.Close()
		return fmt.Errorf("failed to start docker save: %w", err)
	}

	if err := cmdSSH.Start(); err != nil {
		pipeReader.Close()
		pipeWriter.Close()
		return fmt.Errorf("failed to start ssh transfer: %w", err)
	}

	// Wait for save to finish, then close the writer so ssh reads EOF. The save
	// error is kept rather than dropped: a save that dies mid-stream leaves ssh
	// to fail on truncated input, and the real cause is on this side.
	saveDone := make(chan error, 1)
	go func() {
		e := cmdSave.Wait()
		pipeWriter.Close()
		saveDone <- e
	}()

	sshErr := cmdSSH.Wait()
	// ssh is done with its stdin; dropping our end unblocks a save still trying
	// to write into a pipe nobody reads, so it fails fast instead of hanging.
	pipeReader.Close()
	saveWaitErr := <-saveDone

	// ssh is checked first on purpose. When the remote side is unreachable or
	// refuses the command, ssh dies early and closes the pipe, and docker save
	// then reports a "broken pipe" that is only a symptom. Surfacing the save
	// error first is what hid the real reason (a bad target host, a refused
	// connection, no docker on the far end) behind "broken pipe".
	if sshErr != nil {
		detail := strings.TrimSpace(sshOut.String())
		if detail == "" {
			detail = fmt.Sprintf("could not reach root@%s:%d over SSH", targetIP, targetSSHPort)
		}
		return fmt.Errorf("ssh restore on target failed: %w\n%s", sshErr, detail)
	}
	if saveWaitErr != nil {
		return fmt.Errorf("docker save failed: %w\n%s", saveWaitErr, strings.TrimSpace(saveErr.String()))
	}
	if out := strings.TrimSpace(sshOut.String()); out != "" {
		note("%s", out)
	}

	// 8. If move mode, remove source container
	if moveMode {
		note("Removing container %s from source...", containerName)
		_, _ = combined(ctx, "rm", "-f", containerID)
	}

	return nil
}
