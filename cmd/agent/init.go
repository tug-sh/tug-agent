package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"tug.sh/services/agent/internal/config"
	"tug.sh/services/agent/internal/pairing"
)

// Pairing works one way now: the dashboard creates the server record, shows a
// short code, and this machine exchanges that code for a token it keeps. The
// token is never typed or pasted by anyone.
//
// The agent used to mint its own identifier and encode it inside the token, so
// rotating a token could change which machine the agent claimed to be, and two
// hosts with the same name could collide. It no longer decides anything about
// its own identity.
func runInit(cfg config.Config, args []string) error {
	serverID, token, err := resolvePairing(cfg, args)
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

// resolvePairing decides how this machine learns who it is.
//
// Two arguments are the manual route, kept for anyone restoring a machine from
// credentials they already hold. Everything else goes through a code, which is
// what the dashboard hands out.
func resolvePairing(cfg config.Config, args []string) (serverID, token string, err error) {
	positional := positionalArguments(args)
	if len(positional) >= 2 {
		return positional[0], positional[1], nil
	}

	code, err := pairingCode(cfg, positional)
	if err != nil {
		return "", "", err
	}
	credential, err := pairing.Claim(context.Background(), cfg.APIBaseURL, code)
	if err != nil {
		return "", "", err
	}
	// A deployment that answers on a different socket says so here, and the
	// answer is written into the settings file with the rest of the pairing.
	if credential.WebSocketURL != "" {
		cfg.APIWebSocketURL = credential.WebSocketURL
	}
	return credential.ServerID, credential.AgentToken, nil
}

// pairingCode takes the code from wherever it was supplied: an argument, the
// environment for unattended installs, or the person at the keyboard.
func pairingCode(cfg config.Config, positional []string) (string, error) {
	if len(positional) == 1 {
		return validCode(positional[0])
	}
	if supplied := strings.TrimSpace(os.Getenv("TUG_CODE")); supplied != "" {
		return validCode(supplied)
	}

	code, err := pairing.Ask(os.Stdout, cfg.DashboardURL)
	if errors.Is(err, pairing.ErrNoTerminal) {
		return "", fmt.Errorf(
			"there is no terminal to ask for the pairing code.\n"+
				"Add a server at %s and pass the code it shows:\n"+
				"  sudo TUG_CODE=418302 tug init",
			cfg.DashboardURL,
		)
	}
	return code, err
}

func validCode(raw string) (string, error) {
	code := pairing.Normalize(raw)
	if len(code) != pairing.CodeDigits {
		return "", fmt.Errorf("a pairing code is %d digits; got %q", pairing.CodeDigits, raw)
	}
	return code, nil
}

func positionalArguments(args []string) []string {
	positional := make([]string, 0, 2)
	for _, arg := range args[1:] {
		if trimmed := strings.TrimSpace(arg); trimmed != "" && !strings.HasPrefix(trimmed, "-") {
			positional = append(positional, trimmed)
		}
	}
	return positional
}

func runStart(cfg config.Config) error {
	if strings.TrimSpace(cfg.ServerID) == "" || strings.TrimSpace(cfg.AgentToken) == "" {
		return errors.New("this machine is not paired yet; run: sudo tug init")
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
