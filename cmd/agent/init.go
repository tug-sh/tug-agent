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

func runInit(cfg config.Config) error {
	data, err := generateInitData(cfg)
	if err != nil {
		return err
	}

	if err := writeAgentEnv(data.AgentEnvPath, cfg, data.AgentToken); err != nil {
		return err
	}
	restartMsg := tryRestartAgentService()

	fmt.Println("========================================")
	fmt.Println(" tug.sh agent initialization completed")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Printf("Detected host    : %s\n", data.HostName)
	fmt.Printf("VPS name (slug)  : %s\n", data.ServerName)
	fmt.Println()
	fmt.Printf("Server ID        : %s\n", data.ServerID)
	fmt.Printf("Agent token      : %s\n", data.AgentToken)
	fmt.Printf("Environment file : %s\n", data.AgentEnvPath)
	fmt.Println()
	fmt.Println("Open this URL to connect the server:")
	fmt.Println(data.ConnectionURL)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("1) Open the URL above in your browser.")
	fmt.Println("2) Agent service restart was attempted automatically.")
	if restartMsg != "" {
		fmt.Printf("   %s\n", restartMsg)
	}

	return nil
}

func generateInitData(cfg config.Config) (initData, error) {
	hostName, err := os.Hostname()
	if err != nil {
		hostName = "localhost"
	}
	serverName := slugify(hostName)

	randomSuffix, randomErr := randomHex("", 4)
	if randomErr != nil {
		return initData{}, randomErr
	}
	// Always create a new server_id on init so deleted/blocked IDs are not reused.
	serverID := fmt.Sprintf("srv_%s_%s", serverName, randomSuffix)
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

func tryRestartAgentService() string {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return tryStartAgentDetachedFallback("systemctl not available")
	}
	if err := exec.Command("systemctl", "cat", "tug-agent.service").Run(); err != nil {
		return tryStartAgentDetachedFallback("tug-agent.service not found")
	}
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return tryStartAgentDetachedFallback("cannot reload systemd daemon")
	}
	if err := exec.Command("systemctl", "restart", "tug-agent.service").Run(); err != nil {
		return tryStartAgentDetachedFallback("cannot restart tug-agent.service automatically")
	}
	return "tug-agent.service restarted."
}

func tryStartAgentDetachedFallback(reason string) string {
	binaryPath := ""
	if candidate, err := exec.LookPath("tug"); err == nil && strings.TrimSpace(candidate) != "" {
		binaryPath = candidate
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

	content := strings.Join([]string{
		fmt.Sprintf("TUG_AGENT_TOKEN=%s", token),
		fmt.Sprintf("TUG_API_WS_URL=%s", cfg.APIWebSocketURL),
		"",
	}, "\n")

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
