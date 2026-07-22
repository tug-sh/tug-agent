package config

import (
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AgentVersion        string
	ServerID            string
	WorkspaceID         string
	AgentToken          string
	APIWebSocketURL     string
	DashboardURL        string
	AgentEnvPath        string
	Verbose             bool
	TrafficProfile      string
	HeartbeatInterval   time.Duration
	SelfHealInterval    time.Duration
	ReconnectBaseDelay  time.Duration
	ReconnectMaxDelay   time.Duration
	ReconnectJitterPct  int
	ProtocolV2Enabled   bool
	ProtocolV2QueuePath string
}

const defaultAgentVersion = "1.0.0"
const (
	defaultTrafficProfile = "default"
	debugTrafficProfile   = "debug"
)

func Load() Config {
	agentToken := envOrDefault("TUG_AGENT_TOKEN", "")
	serverID := parseServerIDFromToken(agentToken)
	if serverID == "" {
		// Compatibility fallback for legacy token format.
		serverID = envOrDefault("TUG_SERVER_ID", "")
	}
	debugProfileEnabled := envBoolOrDefault("TUG_AGENT_DEBUG_PROFILE", false)
	trafficProfile := normalizeTrafficProfile(envOrDefault("TUG_AGENT_TRAFFIC_PROFILE", defaultTrafficProfile))
	if debugProfileEnabled {
		trafficProfile = debugTrafficProfile
	}
	heartbeatDefault := 30 * time.Second
	selfHealDefault := 15 * time.Minute
	if trafficProfile == debugTrafficProfile {
		heartbeatDefault = 15 * time.Second
		selfHealDefault = 5 * time.Minute
	}
	reconnectBaseDefault := 1 * time.Second
	reconnectMaxDefault := 30 * time.Second
	reconnectJitterDefault := 20
	cfg := Config{
		AgentVersion:        envOrDefault("TUG_AGENT_VERSION", defaultAgentVersion),
		ServerID:            serverID,
		WorkspaceID:         "",
		AgentToken:          agentToken,
		APIWebSocketURL:     envOrDefault("TUG_API_WS_URL", "wss://api.tug.sh/ws/agents"),
		DashboardURL:        envOrDefault("TUG_DASHBOARD_URL", "https://app.tug.sh"),
		AgentEnvPath:        envOrDefault("TUG_AGENT_ENV_PATH", "/etc/tug/agent.env"),
		Verbose:             envBoolOrDefault("TUG_VERBOSE", true),
		TrafficProfile:      trafficProfile,
		HeartbeatInterval:   envDurationOrDefault("TUG_AGENT_HEARTBEAT_INTERVAL", heartbeatDefault),
		SelfHealInterval:    envDurationOrDefault("TUG_AGENT_SELF_HEAL_INTERVAL", selfHealDefault),
		ReconnectBaseDelay:  envDurationOrDefault("TUG_AGENT_RECONNECT_BASE_DELAY", reconnectBaseDefault),
		ReconnectMaxDelay:   envDurationOrDefault("TUG_AGENT_RECONNECT_MAX_DELAY", reconnectMaxDefault),
		ReconnectJitterPct:  envIntOrDefault("TUG_AGENT_RECONNECT_JITTER_PCT", reconnectJitterDefault),
		ProtocolV2Enabled:   envBoolOrDefault("TUG_PROTOCOL_V2_ENABLED", true),
		ProtocolV2QueuePath: envOrDefault("TUG_PROTOCOL_V2_QUEUE_PATH", ""),
	}
	if cfg.HeartbeatInterval < 5*time.Second {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.SelfHealInterval < cfg.HeartbeatInterval {
		cfg.SelfHealInterval = cfg.HeartbeatInterval * 2
	}
	if cfg.ReconnectBaseDelay < 250*time.Millisecond {
		cfg.ReconnectBaseDelay = 250 * time.Millisecond
	}
	if cfg.ReconnectMaxDelay < cfg.ReconnectBaseDelay {
		cfg.ReconnectMaxDelay = cfg.ReconnectBaseDelay
	}
	if cfg.ReconnectJitterPct < 0 {
		cfg.ReconnectJitterPct = 0
	}
	if cfg.ReconnectJitterPct > 50 {
		cfg.ReconnectJitterPct = 50
	}
	return cfg
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

func envDurationOrDefault(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envIntOrDefault(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func normalizeTrafficProfile(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case debugTrafficProfile:
		return debugTrafficProfile
	default:
		return defaultTrafficProfile
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
