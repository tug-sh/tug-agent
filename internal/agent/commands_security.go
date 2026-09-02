package agent

import (
	"fmt"

	"tug.sh/services/agent/internal/security"
)

func (runtime *Runtime) handleApplySecurityHardening(request commandRequest) ([]string, error) {
	cmd := request.command
	err := security.ApplySSHHardening(request.ctx, security.SSHHardeningOptions{
		DisableRootLogin:    cmd.DisableRootLogin,
		DisablePasswordAuth: cmd.DisablePasswordAuth,
		Port:                cmd.SSHPort,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to apply SSH hardening: %w", err)
	}

	if cmd.Fail2banEnabled {
		_ = security.ApplyFail2ban(request.ctx, true)
	} else {
		_ = security.ApplyFail2ban(request.ctx, false)
	}

	return []string{"Security hardening settings applied successfully to host."}, nil
}

func (runtime *Runtime) handleApplyFirewallRule(request commandRequest) ([]string, error) {
	cmd := request.command
	err := security.ApplyFirewallRule(request.ctx, security.FirewallRule{
		Port:     cmd.FirewallPort,
		Protocol: cmd.FirewallProtocol,
		Action:   cmd.FirewallAction,
		SourceIP: cmd.FirewallSourceIP,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to apply firewall rule: %w", err)
	}
	return []string{fmt.Sprintf("Firewall rule %s %s/%s applied.", cmd.FirewallAction, cmd.FirewallPort, cmd.FirewallProtocol)}, nil
}

func (runtime *Runtime) handleDeleteFirewallRule(request commandRequest) ([]string, error) {
	cmd := request.command
	err := security.DeleteFirewallRule(request.ctx, security.FirewallRule{
		Port:     cmd.FirewallPort,
		Protocol: cmd.FirewallProtocol,
		Action:   cmd.FirewallAction,
		SourceIP: cmd.FirewallSourceIP,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to delete firewall rule: %w", err)
	}
	return []string{fmt.Sprintf("Firewall rule %s %s/%s deleted.", cmd.FirewallAction, cmd.FirewallPort, cmd.FirewallProtocol)}, nil
}
