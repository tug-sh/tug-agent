package security

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type FirewallRule struct {
	Port     string
	Protocol string // "tcp", "udp", "both"
	Action   string // "allow", "deny", "limit"
	SourceIP string
}

// ApplyFirewallRule executes ufw commands to enforce firewall rules on Linux VPS.
func ApplyFirewallRule(ctx context.Context, rule FirewallRule) error {
	port := strings.TrimSpace(rule.Port)
	if port == "" {
		return fmt.Errorf("port is required")
	}

	action := strings.ToLower(strings.TrimSpace(rule.Action))
	if action != "allow" && action != "deny" && action != "limit" {
		action = "allow"
	}

	protocol := strings.ToLower(strings.TrimSpace(rule.Protocol))
	if protocol == "" || protocol == "both" {
		protocol = "tcp"
	}

	args := []string{action}
	if rule.SourceIP != "" && rule.SourceIP != "0.0.0.0/0" {
		args = append(args, "from", strings.TrimSpace(rule.SourceIP), "to", "any")
	}
	args = append(args, "port", port, "proto", protocol)

	cmd := exec.CommandContext(ctx, "ufw", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ufw error: %s: %w", string(out), err)
	}

	return nil
}
