package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"tug.sh/services/agent/internal/config"
)

type initData struct {
	ServerID      string
	HostName      string
	ServerName    string
	AgentToken    string
	AgentEnvPath  string
	ConnectionURL string
}

func runStart(cfg config.Config) error {
	initialized := strings.TrimSpace(cfg.ServerID) != "" && strings.TrimSpace(cfg.AgentToken) != ""
	if !initialized {
		fmt.Println("No connection configuration found. Initiating new connection setup...")
		return runInit(cfg)
	}

	fmt.Println("Starting tug agent service in background...")
	msg := tryStartAgentService()
	fmt.Printf("   %s\n", msg)
	return nil
}

func runInit(cfg config.Config) error {
	data, err := generateInitData(cfg)
	if err != nil {
		return err
	}

	if err := writeAgentEnv(data.AgentEnvPath, cfg, data.AgentToken); err != nil {
		return err
	}
	restartMsg := tryRestartAgentService()

	// ANSI color formatting for high visibility
	boldCyan := "\033[1;36m"
	boldGreen := "\033[1;32m"
	boldYellow := "\033[1;33m"
	boldWhite := "\033[1;37m"
	reset := "\033[0m"

	fmt.Println()
	fmt.Printf("%s✔ Agent initialized successfully!%s\n\n", boldGreen, reset)
	fmt.Printf("%s━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n", boldCyan, reset)
	fmt.Printf(" %sOPEN THIS URL TO CONNECT YOUR VPS TO THE DASHBOARD:%s\n", boldYellow, reset)
	fmt.Printf(" %s%s%s\n", boldWhite, data.ConnectionURL, reset)
	fmt.Printf("%s━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n", boldCyan, reset)
	fmt.Println()
	if restartMsg != "" {
		fmt.Printf("   %s%s%s\n\n", boldGreen, restartMsg, reset)
	}

	return nil
}

func generateInitData(cfg config.Config) (initData, error) {
	hostName, err := os.Hostname()
	if err != nil {
		hostName = "localhost"
	}
	serverName := slugify(hostName)

	// Always create a new server_id on init so deleted/blocked IDs are not reused.
	serverID, idErr := newServerID()
	if idErr != nil {
		return initData{}, idErr
	}
	agentToken, err := generateAgentToken(serverID)
	if err != nil {
		return initData{}, err
	}

	dashboardBase := cfg.DashboardURL
	if dashboardBase == "" {
		dashboardBase = "https://app.tug.sh"
	}
	dashboardBase = strings.TrimSuffix(dashboardBase, "/")

	connectionURL := fmt.Sprintf(
		"%s/connect/%s",
		dashboardBase,
		url.PathEscape(agentToken),
	)

	return initData{
		ServerID:      serverID,
		HostName:      hostName,
		ServerName:    serverName,
		AgentToken:    agentToken,
		AgentEnvPath:  cfg.AgentEnvPath,
		ConnectionURL: connectionURL,
	}, nil
}

func tryStartAgentService() string {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return tryStartAgentDetachedFallback("systemctl not available")
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	_ = exec.Command("systemctl", "enable", "tug-agent.service").Run()
	if output, err := exec.Command("systemctl", "restart", "tug-agent.service").CombinedOutput(); err != nil {
		return fmt.Sprintf("systemctl restart failed: %s; run: sudo systemctl restart tug-agent", strings.TrimSpace(string(output)))
	}
	return "tug-agent.service started."
}

func tryRestartAgentService() string {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return tryStartAgentDetachedFallback("systemctl not available")
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	_ = exec.Command("systemctl", "enable", "tug-agent.service").Run()
	if output, err := exec.Command("systemctl", "restart", "tug-agent.service").CombinedOutput(); err != nil {
		return fmt.Sprintf("systemctl restart failed: %s; run: sudo systemctl restart tug-agent", strings.TrimSpace(string(output)))
	}
	return "tug-agent.service restarted."
}

func tryStartAgentDetachedFallback(reason string) string {
	binaryPath := ""
	if candidate, err := exec.LookPath("tug-agent"); err == nil && strings.TrimSpace(candidate) != "" {
		binaryPath = candidate
	}
	if binaryPath == "" {
		if candidate, err := exec.LookPath("tug"); err == nil && strings.TrimSpace(candidate) != "" {
			binaryPath = candidate
		}
	}
	if binaryPath == "" {
		if candidate, err := os.Executable(); err == nil && strings.TrimSpace(candidate) != "" {
			binaryPath = candidate
		}
	}
	if strings.TrimSpace(binaryPath) == "" {
		return fmt.Sprintf("%s; automatic restart unavailable. Run: sudo systemctl restart tug-agent", reason)
	}

	cmd := exec.Command(binaryPath)
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err == nil {
		defer devNull.Close()
		cmd.Stdout = devNull
		cmd.Stderr = devNull
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	}
	cmd.Stdin = nil

	if startErr := cmd.Start(); startErr != nil {
		return fmt.Sprintf("%s; fallback start failed. Run: sudo systemctl restart tug-agent", reason)
	}
	_ = cmd.Process.Release()
	return fmt.Sprintf("%s; started agent process in background using %s.", reason, binaryPath)
}

func generateAgentToken(serverID string) (string, error) {
	if strings.TrimSpace(serverID) == "" {
		return "", errors.New("server_id is required for token generation")
	}
	serverPart := base64.RawURLEncoding.EncodeToString([]byte(strings.TrimSpace(serverID)))
	randomPart, err := randomHex("", 24)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("agtv2.%s.%s", serverPart, randomPart), nil
}

func writeAgentEnv(path string, cfg config.Config, token string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("empty agent environment path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("cannot create environment directory: %w", err)
	}

	lines := []string{
		fmt.Sprintf("TUG_AGENT_TOKEN=%s", token),
	}
	if cfg.APIWebSocketURL != "wss://api.tug.sh/ws/agents" {
		lines = append(lines, fmt.Sprintf("TUG_API_WS_URL=%s", cfg.APIWebSocketURL))
	}
	lines = append(lines, "")

	content := strings.Join(lines, "\n")

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("cannot write environment file: %w", err)
	}
	return nil
}

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
		return nil, fmt.Errorf("cannot read existing environment file: %w", err)
	}
	lines := strings.Split(string(content), "\n")
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}
		values[key] = strings.TrimSpace(parts[1])
	}
	return values, nil
}

// Upper and lower case plus digits, the same shape as a YouTube video id: short
// enough to fit in a URL and still worth 71 bits over twelve characters.
const (
	serverIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	serverIDLength   = 12
)

// newServerID mints an opaque identifier for this machine. The host name is
// deliberately left out: an ID lives as long as the pairing, while a host can be
// renamed, and two unrelated machines routinely answer to the same name.
func newServerID() (string, error) {
	return randomBase62(serverIDLength)
}

func randomBase62(length int) (string, error) {
	if length <= 0 {
		return "", errors.New("invalid identifier length")
	}
	// 256 is not a multiple of 62, so the bytes above the last whole block would
	// make the first few characters of the alphabet more likely. Those are drawn
	// again instead.
	const largestUnbiasedByte = 256 - (256 % len(serverIDAlphabet))

	identifier := make([]byte, 0, length)
	buffer := make([]byte, length)
	for len(identifier) < length {
		if _, err := rand.Read(buffer); err != nil {
			return "", fmt.Errorf("cannot generate random bytes: %w", err)
		}
		for _, value := range buffer {
			if int(value) >= largestUnbiasedByte {
				continue
			}
			identifier = append(identifier, serverIDAlphabet[int(value)%len(serverIDAlphabet)])
			if len(identifier) == length {
				break
			}
		}
	}
	return string(identifier), nil
}

func randomHex(prefix string, byteLength int) (string, error) {
	if byteLength <= 0 {
		return "", errors.New("invalid random byte length")
	}

	buffer := make([]byte, byteLength)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("cannot generate random bytes: %w", err)
	}
	return prefix + hex.EncodeToString(buffer), nil
}

var nonSlugPattern = regexp.MustCompile(`[^a-z0-9-]+`)
var multiDashPattern = regexp.MustCompile(`-+`)

func slugify(value string) string {
	clean := strings.ToLower(strings.TrimSpace(value))
	clean = strings.ReplaceAll(clean, "_", "-")
	clean = strings.ReplaceAll(clean, " ", "-")
	clean = nonSlugPattern.ReplaceAllString(clean, "-")
	clean = multiDashPattern.ReplaceAllString(clean, "-")
	clean = strings.Trim(clean, "-")
	if clean == "" {
		return "server"
	}
	return clean
}
