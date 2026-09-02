package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// migrationDocsURL points at the manual section that explains the target's SSH
// requirements. Migration failures reference it instead of dumping every knob a
// user might have to touch into the error itself.
const migrationDocsURL = "https://tug.sh/docs/4-security-and-connectivity#migrating-containers-between-servers"

// sshNoise drops the lines ssh prints that are not the reason for a failure,
// such as the "Permanently added ... to the list of known hosts" warning that
// StrictHostKeyChecking=no always emits, so the message shows the actual reply.
func sshNoise(output string) string {
	lines := strings.Split(output, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Warning: Permanently added") {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, "; ")
}

// migrationTargetUser resolves the account the ephemeral key is installed for
// on the target: either an explicitly requested user, the user the agent itself
// runs as, or an auto-detected sudo/docker user when root login is disabled.
func migrationTargetUser(preferredUser string) (username string, sshDir string, uid int, gid int) {
	preferredUser = strings.TrimSpace(preferredUser)
	if preferredUser != "" {
		if u, err := user.Lookup(preferredUser); err == nil {
			var uID, gID int
			_, _ = fmt.Sscanf(u.Uid, "%d", &uID)
			_, _ = fmt.Sscanf(u.Gid, "%d", &gID)
			return u.Username, filepath.Join(u.HomeDir, ".ssh"), uID, gID
		}
		// Fallback for specified user if lookup fails
		return preferredUser, filepath.Join("/home", preferredUser, ".ssh"), -1, -1
	}

	if u, err := user.Current(); err == nil && u.Username != "root" {
		var uID, gID int
		_, _ = fmt.Sscanf(u.Uid, "%d", &uID)
		_, _ = fmt.Sscanf(u.Gid, "%d", &gID)
		return u.Username, filepath.Join(u.HomeDir, ".ssh"), uID, gID
	}

	// If running as root, check if root SSH login is allowed
	rootAllowed := isRootSSHAllowed()
	if !rootAllowed {
		if uName, uSSHDir, uID, gID := findStandardSudoUser(); uName != "" {
			return uName, uSSHDir, uID, gID
		}
	}

	return "root", "/root/.ssh", 0, 0
}

func findStandardSudoUser() (string, string, int, int) {
	commonUsers := []string{"ubuntu", "debian", "centos", "admin", "tug", "ec2-user", "fedora", "alma", "rocky"}
	for _, name := range commonUsers {
		if u, err := user.Lookup(name); err == nil {
			var uID, gID int
			_, _ = fmt.Sscanf(u.Uid, "%d", &uID)
			_, _ = fmt.Sscanf(u.Gid, "%d", &gID)
			return u.Username, filepath.Join(u.HomeDir, ".ssh"), uID, gID
		}
	}

	// Scan /home for user directories
	if entries, err := os.ReadDir("/home"); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				if u, err := user.Lookup(entry.Name()); err == nil {
					var uID, gID int
					_, _ = fmt.Sscanf(u.Uid, "%d", &uID)
					_, _ = fmt.Sscanf(u.Gid, "%d", &gID)
					return u.Username, filepath.Join(u.HomeDir, ".ssh"), uID, gID
				}
			}
		}
	}

	return "", "", -1, -1
}

func isRootSSHAllowed() bool {
	cmd := execCommandContext(context.Background(), "sshd", "-T")
	if out, err := cmd.Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "permitrootlogin") {
				val := strings.ToLower(strings.TrimSpace(parts[1]))
				return val != "no"
			}
		}
	}

	// Fallback to checking config files directly
	if data, err := os.ReadFile("/etc/ssh/sshd_config"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) >= 2 && strings.EqualFold(parts[0], "PermitRootLogin") {
				return strings.ToLower(parts[1]) != "no"
			}
		}
	}

	return true
}

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

// CheckMigrationConnectivity goes beyond an open port: it opens a real SSH
// session to sshUser@target with the ephemeral key and runs a trivial remote
// command. This is what a bare TCP dial cannot see — whether sshd will actually
// accept the key. It classifies the three ways a reachable target still cannot
// be migrated to, so the dashboard can say why up front instead of failing
// mid-transfer:
//   - the port is open but the key is refused (PermitRootLogin no,
//     PubkeyAuthentication off, wrong/locked account, bad authorized_keys perms),
//   - the login works but docker is missing on the far end,
//   - the address is not routable at all.
//
// BatchMode and NumberOfPasswordPrompts=0 keep a refused key from hanging on a
// password prompt: it fails immediately with the sshd reason instead.
func CheckMigrationConnectivity(ctx context.Context, targetIP string, targetSSHPort int, sshUser, ephemeralPrivateKey string) ([]string, error) {
	targetIP = strings.TrimSpace(targetIP)
	sshUser = strings.TrimSpace(sshUser)
	ephemeralPrivateKey = strings.TrimSpace(ephemeralPrivateKey)
	if targetIP == "" {
		return nil, fmt.Errorf("target_ip is required")
	}
	if targetSSHPort <= 0 {
		targetSSHPort = 22
	}
	if sshUser == "" {
		sshUser = "root"
	}

	// Without a key there is nothing to authenticate with; fall back to the
	// reachability probe so the caller still gets a routing answer.
	if ephemeralPrivateKey == "" {
		return CheckMigrationReachable(ctx, targetIP, targetSSHPort)
	}

	// A closed/unroutable port is a routing problem, not an auth one; surface it
	// as such before attempting the login.
	if _, err := CheckMigrationReachable(ctx, targetIP, targetSSHPort); err != nil {
		return nil, err
	}

	keyPath := filepath.Join(os.TempDir(), fmt.Sprintf("tug_mig_probe_%d", time.Now().UnixNano()))
	if err := os.WriteFile(keyPath, []byte(ephemeralPrivateKey+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("failed to write probe key: %w", err)
	}
	defer os.Remove(keyPath)

	const okMarker = "TUG_MIG_OK"
	const noDockerMarker = "TUG_MIG_NODOCKER"
	remote := "if command -v docker >/dev/null 2>&1; then echo " + okMarker + "; else echo " + noDockerMarker + "; fi"

	args := []string{
		"-p", strconv.Itoa(targetSSHPort),
		"-i", keyPath,
		"-o", "BatchMode=yes",
		"-o", "NumberOfPasswordPrompts=0",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		fmt.Sprintf("%s@%s", sshUser, targetIP),
		remote,
	}

	var out bytes.Buffer
	cmd := execCommandContext(ctx, "ssh", args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	combined := strings.TrimSpace(out.String())

	if runErr == nil && strings.Contains(combined, okMarker) {
		return []string{fmt.Sprintf("SSH login to %s@%s:%d works and docker is available on the target.", sshUser, targetIP, targetSSHPort)}, nil
	}
	if strings.Contains(combined, noDockerMarker) {
		return nil, fmt.Errorf("SSH login as %s works, but docker is not installed or not in PATH on the target. See %s", sshUser, migrationDocsURL)
	}

	lower := strings.ToLower(combined)
	if strings.Contains(lower, "permission denied") || strings.Contains(lower, "publickey") {
		return nil, fmt.Errorf(
			"the target refused the migration key for user %q. On the target, allow SSH key login for that user (if it is root, set PermitRootLogin to prohibit-password). Setup guide: %s",
			sshUser, migrationDocsURL,
		)
	}

	detail := sshNoise(combined)
	if detail == "" {
		detail = "no output from ssh"
	}
	return nil, fmt.Errorf("could not open an SSH session to the target as %s: %s. Setup guide: %s", sshUser, detail, migrationDocsURL)
}

// PrepareMigrationTargetKey adds the ephemeral public key to the target user's
// authorized_keys on the target VPS and returns the username the source must log
// in as.
func PrepareMigrationTargetKey(ctx context.Context, ephemeralPublicKey string, preferredUser string) (string, error) {
	ephemeralPublicKey = strings.TrimSpace(ephemeralPublicKey)
	if ephemeralPublicKey == "" {
		return "", fmt.Errorf("ephemeral_key is required")
	}

	username, sshDir, uid, gid := migrationTargetUser(preferredUser)
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create .ssh directory: %w", err)
	}

	if uid >= 0 && gid >= 0 && os.Geteuid() == 0 {
		_ = os.Chown(sshDir, uid, gid)
	}

	authKeysPath := filepath.Join(sshDir, "authorized_keys")

	// Skip if the key is already present so repeated migrations do not pile up
	// duplicate lines in the user's authorized_keys.
	if existing, err := os.ReadFile(authKeysPath); err == nil {
		if strings.Contains(string(existing), ephemeralPublicKey) {
			return username, nil
		}
	}

	f, err := os.OpenFile(authKeysPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return "", fmt.Errorf("failed to open authorized_keys: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString("\n" + ephemeralPublicKey + "\n"); err != nil {
		return "", fmt.Errorf("failed to write key to authorized_keys: %w", err)
	}

	if uid >= 0 && gid >= 0 && os.Geteuid() == 0 {
		_ = os.Chown(authKeysPath, uid, gid)
	}

	return username, nil
}

// CleanupMigrationTargetKey removes an ephemeral key from authorized_keys
func CleanupMigrationTargetKey(ctx context.Context, ephemeralPublicKey string, preferredUser string) error {
	ephemeralPublicKey = strings.TrimSpace(ephemeralPublicKey)
	if ephemeralPublicKey == "" {
		return nil
	}

	_, sshDir, _, _ := migrationTargetUser(preferredUser)
	authKeysPath := filepath.Join(sshDir, "authorized_keys")
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
	sshUser string,
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

	// The login user is chosen by the target (the user its agent runs as), not
	// assumed to be root: the ephemeral key was installed in that user's
	// authorized_keys, and this login must match.
	sshUser = strings.TrimSpace(sshUser)
	if sshUser == "" {
		sshUser = "root"
	}

	if containerID == "" || targetIP == "" || ephemeralPrivateKey == "" {
		return fmt.Errorf("container_id, target_ip, and ephemeral_key are required")
	}
	if targetSSHPort <= 0 {
		targetSSHPort = 22
	}

	// 1. Get container info (name, image, ports, restart policy, labels)
	note("Inspecting container %s...", containerID)
	var runFlags []string
	containerName := containerID

	if inspectJSON, err := output(ctx, "inspect", containerID); err == nil {
		var inspectList []struct {
			Name   string `json:"Name"`
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
			HostConfig struct {
				RestartPolicy struct {
					Name string `json:"Name"`
				} `json:"RestartPolicy"`
				PortBindings map[string][]struct {
					HostIP   string `json:"HostIp"`
					HostPort string `json:"HostPort"`
				} `json:"PortBindings"`
				NetworkMode string `json:"NetworkMode"`
			} `json:"HostConfig"`
		}
		if err := json.Unmarshal([]byte(inspectJSON), &inspectList); err == nil && len(inspectList) > 0 {
			info := inspectList[0]
			containerName = strings.TrimPrefix(strings.TrimSpace(info.Name), "/")

			for containerPort, bindings := range info.HostConfig.PortBindings {
				for _, b := range bindings {
					if b.HostPort != "" {
						if b.HostIP != "" && b.HostIP != "0.0.0.0" {
							runFlags = append(runFlags, fmt.Sprintf("-p %s:%s:%s", b.HostIP, b.HostPort, containerPort))
						} else {
							runFlags = append(runFlags, fmt.Sprintf("-p %s:%s", b.HostPort, containerPort))
						}
					}
				}
			}

			if r := info.HostConfig.RestartPolicy.Name; r != "" && r != "no" {
				runFlags = append(runFlags, fmt.Sprintf("--restart %s", r))
			}

			for k, v := range info.Config.Labels {
				runFlags = append(runFlags, fmt.Sprintf("--label %s=%q", k, v))
			}
		}
	} else {
		name, err := output(ctx, "inspect", "--format={{.Name}}", containerID)
		if err != nil {
			return fmt.Errorf("failed to inspect container name: %w", err)
		}
		containerName = strings.TrimPrefix(strings.TrimSpace(name), "/")
	}

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

	var imageSizeBytes int64
	if szStr, err := output(ctx, "image", "inspect", "--format={{.Size}}", tempImageTag); err == nil {
		_, _ = fmt.Sscanf(strings.TrimSpace(szStr), "%d", &imageSizeBytes)
	}
	if imageSizeBytes > 0 {
		note("Committed image size: %.1f MB. Initiating SSH stream...", float64(imageSizeBytes)/(1024*1024))
	}

	// 4. Write ephemeral private key to temp file
	keyPath := fmt.Sprintf("/tmp/tug_mig_key_%s", containerID)
	if err := os.WriteFile(keyPath, []byte(ephemeralPrivateKey+"\n"), 0600); err != nil {
		return fmt.Errorf("failed to write ephemeral private key: %w", err)
	}
	defer os.Remove(keyPath)

	// 5. Stream image to target VPS and run it
	note("Streaming image to %s:%d and restoring...", targetIP, targetSSHPort)
	runFlagsStr := ""
	if len(runFlags) > 0 {
		runFlagsStr = " " + strings.Join(runFlags, " ")
	}

	remoteRunCmd := fmt.Sprintf(
		`docker load && `+
			`(docker rm -f %s 2>/dev/null || true) && `+
			`CID=$(docker run -d --name %s%s %s) && `+
			`echo "RESTORED_CID:$CID" && `+
			`sleep 2 && `+
			`STATUS=$(docker inspect --format='{{.State.Status}}' "$CID" 2>/dev/null || echo "unknown") && `+
			`echo "RESTORED_STATUS:$STATUS" && `+
			`if [ "$STATUS" != "running" ]; then `+
			`echo "CONTAINER_ERROR: container failed to start (status: $STATUS)"; `+
			`docker logs --tail 25 "$CID" 2>&1; `+
			`exit 1; `+
			`fi`,
		containerName, containerName, runFlagsStr, tempImageTag,
	)

	sshArgs := []string{
		"-p", fmt.Sprintf("%d", targetSSHPort),
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=15",
		fmt.Sprintf("%s@%s", sshUser, targetIP),
		remoteRunCmd,
	}

	cmdSSH := execCommandContext(ctx, "ssh", sshArgs...)
	cmdSave := execCommandContext(ctx, "docker", "save", tempImageTag)

	var sshOut bytes.Buffer
	var saveErr bytes.Buffer
	cmdSSH.Stdout = &sshOut
	cmdSSH.Stderr = &sshOut
	cmdSave.Stderr = &saveErr

	// Pipe docker save directly to ssh with live progress logging
	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %w", err)
	}

	pw := &progressWriter{
		target:  pipeWriter,
		total:   imageSizeBytes,
		lastLog: time.Now(),
		report:  note,
	}
	cmdSave.Stdout = pw
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

	// Wait for save to finish, then close the writer so ssh reads EOF
	saveDone := make(chan error, 1)
	go func() {
		e := cmdSave.Wait()
		pipeWriter.Close()
		saveDone <- e
	}()

	sshErr := cmdSSH.Wait()
	pipeReader.Close()
	saveWaitErr := <-saveDone

	if sshErr != nil {
		detail := strings.TrimSpace(sshOut.String())
		if detail == "" {
			detail = fmt.Sprintf("could not reach %s@%s:%d over SSH", sshUser, targetIP, targetSSHPort)
		}
		return fmt.Errorf("ssh restore on target failed: %w\n%s", sshErr, detail)
	}
	if saveWaitErr != nil {
		return fmt.Errorf("docker save failed: %w\n%s", saveWaitErr, strings.TrimSpace(saveErr.String()))
	}

	outStr := strings.TrimSpace(sshOut.String())
	var restoredCID, restoredStatus string
	for _, l := range strings.Split(outStr, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "RESTORED_CID:") {
			restoredCID = strings.TrimPrefix(l, "RESTORED_CID:")
		} else if strings.HasPrefix(l, "RESTORED_STATUS:") {
			restoredStatus = strings.TrimPrefix(l, "RESTORED_STATUS:")
		} else if strings.HasPrefix(l, "Loaded image:") {
			note("%s", l)
		}
	}

	if restoredCID != "" {
		shortCID := restoredCID
		if len(shortCID) > 12 {
			shortCID = shortCID[:12]
		}
		if restoredStatus == "" {
			restoredStatus = "running"
		}
		note("✓ Container %s successfully restored & running on %s (ID: %s, Status: %s).", containerName, targetIP, shortCID, restoredStatus)
	}

	// 8. If move mode, remove source container
	if moveMode {
		note("Removing container %s from source...", containerName)
		_, _ = combined(ctx, "rm", "-f", containerID)
	}

	return nil
}

type progressWriter struct {
	target    io.Writer
	total     int64
	current   int64
	lastLog   time.Time
	lastBytes int64
	report    func(string, ...any)
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.target.Write(p)
	pw.current += int64(n)
	now := time.Now()
	if pw.report != nil && (pw.lastLog.IsZero() || now.Sub(pw.lastLog) >= 2*time.Second) {
		elapsed := now.Sub(pw.lastLog).Seconds()
		if elapsed <= 0 {
			elapsed = 1
		}
		speedMB := float64(pw.current-pw.lastBytes) / (1024 * 1024) / elapsed
		pw.lastLog = now
		pw.lastBytes = pw.current

		curMB := float64(pw.current) / (1024 * 1024)
		if pw.total > 0 {
			totalMB := float64(pw.total) / (1024 * 1024)
			pct := float64(pw.current) / float64(pw.total) * 100
			if pct > 100 {
				pct = 100
			}
			pw.report("Transferring image: %.1f MB / %.1f MB (%.0f%%) at %.1f MB/s...", curMB, totalMB, pct, speedMB)
		} else {
			pw.report("Transferring image: %.1f MB transferred at %.1f MB/s...", curMB, speedMB)
		}
	}
	return n, err
}
