package agent

import (
	"fmt"

	"tug.sh/pkg/protocol"
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
		if err := security.ApplyFail2ban(request.ctx, true); err != nil {
			return nil, fmt.Errorf("failed to configure fail2ban: %w", err)
		}
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

func (runtime *Runtime) handleCheckFail2ban(request commandRequest) ([]string, error) {
	installed := security.IsFail2banInstalled()
	running := security.IsFail2banRunning(request.ctx)
	request.setPayload(protocol.Fail2banStatus{
		Installed: installed,
		Running:   running,
	})
	return []string{fmt.Sprintf("Fail2ban status: installed=%v, running=%v", installed, running)}, nil
}

func (runtime *Runtime) handleInstallFail2ban(request commandRequest) ([]string, error) {
	if err := security.ApplyFail2ban(request.ctx, true); err != nil {
		return nil, fmt.Errorf("failed to install and enable fail2ban: %w", err)
	}
	request.setPayload(protocol.Fail2banStatus{
		Installed: true,
		Running:   true,
	})
	return []string{"Fail2ban installed and enabled successfully."}, nil
}
