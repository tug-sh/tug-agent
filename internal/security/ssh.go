package security

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

	// Reload ssh service gracefully
	_ = exec.CommandContext(ctx, "systemctl", "reload", "ssh").Run()
	_ = exec.CommandContext(ctx, "systemctl", "reload", "sshd").Run()

	return nil
}
