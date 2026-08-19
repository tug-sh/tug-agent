package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"tug.sh/services/agent/internal/config"
)

// Pairing works one way now: the dashboard creates the server record and hands
// out both the identifier and the token, which the installer passes here.
//
// The agent used to mint its own identifier and encode it inside the token, so
// rotating a token could change which machine the agent claimed to be, and two
// hosts with the same name could collide. It no longer decides anything about
// its own identity.
func runInit(cfg config.Config, args []string) error {
	serverID, token, err := pairingArguments(args)
	if err != nil {
		return err
	}
	if err := writeAgentEnv(cfg.AgentEnvPath, cfg, serverID, token); err != nil {
		return err
	}
	restartMessage := tryRestartAgentService()

	const (
		boldGreen = "\033[1;32m"
		reset     = "\033[0m"
	)
	fmt.Printf("\n%s✔ This machine is now paired.%s\n", boldGreen, reset)
	fmt.Printf("   server id: %s\n", serverID)
	fmt.Printf("   settings:  %s\n", cfg.AgentEnvPath)
	if restartMessage != "" {
		fmt.Printf("   %s\n", restartMessage)
	}
	fmt.Println()
	return nil
}

// pairingArguments reads `tug init <server-id> <token>`. Both come from the
// command the dashboard shows when a server is added.
func pairingArguments(args []string) (serverID, token string, err error) {
	positional := make([]string, 0, 2)
	for _, arg := range args[1:] {
		if trimmed := strings.TrimSpace(arg); trimmed != "" && !strings.HasPrefix(trimmed, "-") {
			positional = append(positional, trimmed)
		}
	}
	if len(positional) < 2 {
		return "", "", errors.New(
			"usage: tug init <server-id> <token>\n" +
				"Add a server in the dashboard; it shows the command to paste here",
		)
	}
	return positional[0], positional[1], nil
}

func runStart(cfg config.Config) error {
	if strings.TrimSpace(cfg.ServerID) == "" || strings.TrimSpace(cfg.AgentToken) == "" {
		return errors.New("this machine is not paired yet; run: tug init <server-id> <token>")
	}
	fmt.Println("Starting the tug agent service...")
	fmt.Printf("   %s\n", tryStartAgentService())
	return nil
}

func tryStartAgentService() string {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return tryStartAgentDetachedFallback("systemctl is not available")
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	_ = exec.Command("systemctl", "enable", "tug-agent.service").Run()
	if output, err := exec.Command("systemctl", "restart", "tug-agent.service").CombinedOutput(); err != nil {
		return fmt.Sprintf("systemctl restart failed: %s; run: sudo systemctl restart tug-agent", strings.TrimSpace(string(output)))
	}
	return "tug-agent.service started."
}

func tryRestartAgentService() string { return tryStartAgentService() }

// tryStartAgentDetachedFallback keeps the agent usable on a box without
// systemd, such as a container used for testing.
func tryStartAgentDetachedFallback(reason string) string {
	binaryPath := firstExecutable("tug-agent", "tug")
	if binaryPath == "" {
		if candidate, err := os.Executable(); err == nil {
			binaryPath = strings.TrimSpace(candidate)
		}
	}
	if binaryPath == "" {
		return fmt.Sprintf("%s; start the agent yourself with: sudo systemctl restart tug-agent", reason)
	}

	command := exec.Command(binaryPath)
	if devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		defer devNull.Close()
		command.Stdout = devNull
		command.Stderr = devNull
	} else {
		command.Stdout = io.Discard
		command.Stderr = io.Discard
	}
	if err := command.Start(); err != nil {
		return fmt.Sprintf("%s; the fallback start failed too", reason)
	}
	_ = command.Process.Release()
	return fmt.Sprintf("%s; started %s in the background.", reason, binaryPath)
}

func firstExecutable(names ...string) string {
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil && strings.TrimSpace(path) != "" {
			return path
		}
	}
	return ""
}

func writeAgentEnv(path string, cfg config.Config, serverID, token string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("the agent environment path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("cannot create the settings directory: %w", err)
	}

	lines := []string{
		fmt.Sprintf("TUG_SERVER_ID=%s", serverID),
		fmt.Sprintf("TUG_AGENT_TOKEN=%s", token),
	}
	if cfg.APIWebSocketURL != "" && cfg.APIWebSocketURL != defaultWebSocketURL {
		lines = append(lines, fmt.Sprintf("TUG_API_WS_URL=%s", cfg.APIWebSocketURL))
	}
	lines = append(lines, "")

	// The file holds a bearer token, so it is written for root only.
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return fmt.Errorf("cannot write the settings file: %w", err)
	}
	return nil
}

const defaultWebSocketURL = "wss://api.tug.sh/ws/agents"

func readAgentEnv(path string) (map[string]string, error) {
	values := map[string]string{}
	if strings.TrimSpace(path) == "" {
		return values, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return values, nil
		}
		return nil, fmt.Errorf("cannot read the settings file: %w", err)
	}
	for _, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) == "" {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values, nil
}
