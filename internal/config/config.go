package config

import (
	"encoding/base64"
	"os"
	"strings"
)

type Config struct {
	AgentVersion    string
	ServerID        string
	WorkspaceID     string
	AgentToken      string
	APIWebSocketURL string
	DashboardURL    string
	AgentEnvPath    string
	Verbose         bool
}

const defaultAgentVersion = "0.1.0"

func Load() Config {
	agentToken := envOrDefault("TUG_AGENT_TOKEN", "")
	serverID := parseServerIDFromToken(agentToken)
	if serverID == "" {
		// Compatibility fallback for legacy token format.
		serverID = envOrDefault("TUG_SERVER_ID", "")
	}
	return Config{
		AgentVersion:    defaultAgentVersion,
		ServerID:        serverID,
		WorkspaceID:     "",
		AgentToken:      agentToken,
		APIWebSocketURL: envOrDefault("TUG_API_WS_URL", "wss://api.tug.sh/ws/agents"),
		DashboardURL:    envOrDefault("TUG_DASHBOARD_URL", "https://app.tug.sh"),
		AgentEnvPath:    envOrDefault("TUG_AGENT_ENV_PATH", "/etc/tug/agent.env"),
		Verbose:         envBoolOrDefault("TUG_VERBOSE", true),
	}
}

func envOrDefault(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func envBoolOrDefault(key string, fallback bool) bool {
	value := os.Getenv(key)
	switch value {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "no", "NO", "off", "OFF":
		return false
	default:
		return fallback
	}
}

func parseServerIDFromToken(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] != "agtv2" {
		return ""
	}
	rawServerID, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(rawServerID))
}
