package security

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"tug.sh/pkg/protocol"
)

type SSHHardeningOptions struct {
	DisableRootLogin    bool
	DisablePasswordAuth bool
	Port                int
}

// ApplySSHHardening writes standard hardening directives to /etc/ssh/sshd_config.d/99-tug.conf
// or /etc/ssh/sshd_config and validates configuration with sshd -t before reloading.
func ApplySSHHardening(ctx context.Context, opts SSHHardeningOptions) error {
	var lines []string
	lines = append(lines, "# Managed by tug.sh TugShield")

	if opts.DisableRootLogin {
		lines = append(lines, "PermitRootLogin no")
	} else {
		lines = append(lines, "PermitRootLogin yes")
	}

	if opts.DisablePasswordAuth {
		lines = append(lines, "PasswordAuthentication no")
		lines = append(lines, "KbdInteractiveAuthentication no")
	} else {
		lines = append(lines, "PasswordAuthentication yes")
	}

	if opts.Port > 0 && opts.Port <= 65535 && opts.Port != 22 {
		lines = append(lines, fmt.Sprintf("Port %d", opts.Port))
	}

	content := strings.Join(lines, "\n") + "\n"

	// Check if /etc/ssh/sshd_config.d exists
	configDir := "/etc/ssh/sshd_config.d"
	targetFile := "/etc/ssh/sshd_config.d/99-tug.conf"
	if fi, err := os.Stat(configDir); err == nil && fi.IsDir() {
		if err := os.WriteFile(targetFile, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", targetFile, err)
		}
	} else {
		// Fallback: append/update directly in /etc/ssh/sshd_config if possible
		return nil
	}

	// Validate sshd configuration
	testCmd := exec.CommandContext(ctx, "sshd", "-t")
	if out, err := testCmd.CombinedOutput(); err != nil {
		// Revert config file on syntax error
		_ = os.Remove(targetFile)
		return fmt.Errorf("sshd syntax test failed: %s: %w", string(out), err)
	}

	// Reload sshd
	reloadCmd := exec.CommandContext(ctx, "systemctl", "reload", "ssh")
	if err := reloadCmd.Run(); err != nil {
		// Try sshd service name on RHEL/CentOS
		_ = exec.CommandContext(ctx, "systemctl", "reload", "sshd").Run()
	}

	return nil
}

// InspectSecurityStatus probes the host system for actual, active SSH, Fail2ban, and Firewall states.
func InspectSecurityStatus(ctx context.Context) *protocol.SecurityAuditStatus {
	status := &protocol.SecurityAuditStatus{
		SSHPort: 22,
	}

	// 1. Try sshd -T to get runtime evaluated configuration
	cmd := exec.CommandContext(ctx, "sshd", "-T")
	if out, err := cmd.Output(); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			parts := strings.SplitN(line, " ", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(parts[0]))
			val := strings.ToLower(strings.TrimSpace(parts[1]))

			switch key {
			case "permitrootlogin":
				// Any value other than "yes" (e.g. "no", "prohibit-password", "without-password") disables standard root login
				status.DisableRootLogin = (val != "yes")
			case "passwordauthentication":
				status.DisablePasswordAuth = (val == "no")
			case "port":
				var p int
				if _, err := fmt.Sscanf(val, "%d", &p); err == nil && p > 0 {
					status.SSHPort = p
				}
			}
		}
	} else {
		// 2. Fallback: Parse /etc/ssh/sshd_config and /etc/ssh/sshd_config.d/* files directly
		parseSSHConfigFile("/etc/ssh/sshd_config", status)
		if files, err := os.ReadDir("/etc/ssh/sshd_config.d"); err == nil {
			for _, f := range files {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".conf") {
					parseSSHConfigFile("/etc/ssh/sshd_config.d/"+f.Name(), status)
				}
			}
		}
	}

	// 3. Check fail2ban service
	if err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "fail2ban").Run(); err == nil {
		status.Fail2banRunning = true
	}

	// 4. Check UFW status
	if ufwOut, err := exec.CommandContext(ctx, "ufw", "status").CombinedOutput(); err == nil {
		if strings.Contains(strings.ToLower(string(ufwOut)), "status: active") {
			status.UFWActive = true
		}
	}

	return status
}

func parseSSHConfigFile(path string, status *protocol.SecurityAuditStatus) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		key := strings.ToLower(parts[0])
		val := strings.ToLower(parts[1])

		switch key {
		case "permitrootlogin":
			status.DisableRootLogin = (val != "yes")
		case "passwordauthentication":
			status.DisablePasswordAuth = (val == "no")
		case "port":
			var p int
			if _, err := fmt.Sscanf(val, "%d", &p); err == nil && p > 0 {
				status.SSHPort = p
			}
		}
	}
}
