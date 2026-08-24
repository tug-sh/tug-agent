package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

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

// MigrateContainerToTarget exports, transfers via SCP/SSH, and restores a container on a target VPS.
func (manager *Manager) MigrateContainerToTarget(
	ctx context.Context,
	containerID string,
	targetIP string,
	targetSSHPort int,
	ephemeralPrivateKey string,
	moveMode bool,
) error {
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
	name, err := output(ctx, "inspect", "--format={{.Name}}", containerID)
	if err != nil {
		return fmt.Errorf("failed to inspect container name: %w", err)
	}
	containerName := strings.TrimPrefix(strings.TrimSpace(name), "/")

	// 2. Stop container on source
	_, _ = combined(ctx, "stop", containerID)

	// 3. Commit container to temporary image tag
	tempImageTag := fmt.Sprintf("tug-migrated-%s:%d", containerID, time.Now().Unix())
	if _, err := combined(ctx, "commit", containerID, tempImageTag); err != nil {
		return fmt.Errorf("failed to commit container image: %w", err)
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
	sshArgs := []string{
		"-p", fmt.Sprintf("%d", targetSSHPort),
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		fmt.Sprintf("root@%s", targetIP),
		fmt.Sprintf("docker load && docker run -d --name %s %s", containerName, tempImageTag),
	}
	
	cmdSSH := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmdSave := exec.CommandContext(ctx, "docker", "save", tempImageTag)
	
	// Pipe docker save directly to ssh
	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %w", err)
	}
	cmdSave.Stdout = pipeWriter
	cmdSSH.Stdin = pipeReader
	
	// We only need CombinedOutput from ssh to see if load/run failed
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
	
	// Wait for save to finish, then close writer so ssh knows EOF
	go func() {
		_ = cmdSave.Wait()
		pipeWriter.Close()
	}()
	
	if err := cmdSSH.Wait(); err != nil {
		return fmt.Errorf("ssh restore on target failed: %w", err)
	}

	// 8. If move mode, remove source container
	if moveMode {
		_, _ = combined(ctx, "rm", "-f", containerID)
	}

	return nil
}
