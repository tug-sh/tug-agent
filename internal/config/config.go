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
	LogLevel            string
	TrafficProfile      string
	HeartbeatInterval   time.Duration
	SelfHealInterval    time.Duration
	ReconnectBaseDelay  time.Duration
	ReconnectMaxDelay   time.Duration
	ReconnectJitterPct  int
	ProtocolV2Enabled   bool
	ProtocolV2QueuePath string
	RouterImage         string
	RouterNetwork       string
	RouterHTTPPort      int
	RouterHTTPSPort     int
	RouterConfigPath    string
}

// Edge router defaults. They describe the reference tug-router installation and
// can be overridden per host through the TUG_ROUTER_* variables.
const (
	defaultRouterImage      = "caddy:2"
	defaultRouterHTTPPort   = 80
	defaultRouterHTTPSPort  = 443
	defaultRouterConfigPath = "/etc/caddy/Caddyfile"
)

const defaultAgentVersion = "1.0.7"
const (
	defaultTrafficProfile = "default"
	debugTrafficProfile   = "debug"
)

// Bounds that keep a hand-edited agent.env from producing a runtime that
// hammers the API or never backs off.
const (
	minHeartbeatInterval  = 5 * time.Second
	minReconnectBaseDelay = 250 * time.Millisecond
	maxReconnectJitterPct = 50
	minPortNumber         = 1
	maxPortNumber         = 65535
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
		LogLevel:            envOrDefault("TUG_LOG_LEVEL", ""),
		TrafficProfile:      trafficProfile,
		HeartbeatInterval:   envDurationAtLeast("TUG_AGENT_HEARTBEAT_INTERVAL", heartbeatDefault, minHeartbeatInterval),
		SelfHealInterval:    envDurationAtLeast("TUG_AGENT_SELF_HEAL_INTERVAL", selfHealDefault, minHeartbeatInterval),
		ReconnectBaseDelay:  envDurationAtLeast("TUG_AGENT_RECONNECT_BASE_DELAY", reconnectBaseDefault, minReconnectBaseDelay),
		ReconnectMaxDelay:   envDurationAtLeast("TUG_AGENT_RECONNECT_MAX_DELAY", reconnectMaxDefault, minReconnectBaseDelay),
		ReconnectJitterPct:  envIntClamped("TUG_AGENT_RECONNECT_JITTER_PCT", reconnectJitterDefault, 0, maxReconnectJitterPct),
		ProtocolV2Enabled:   envBoolOrDefault("TUG_PROTOCOL_V2_ENABLED", false),
		ProtocolV2QueuePath: envOrDefault("TUG_PROTOCOL_V2_QUEUE_PATH", ""),
		RouterImage:         envOrDefault("TUG_ROUTER_IMAGE", defaultRouterImage),
		RouterNetwork:       envOrDefault("TUG_ROUTER_NETWORK", ""),
		RouterHTTPPort:      envPortOrDefault("TUG_ROUTER_HTTP_PORT", defaultRouterHTTPPort),
		RouterHTTPSPort:     envPortOrDefault("TUG_ROUTER_HTTPS_PORT", defaultRouterHTTPSPort),
		RouterConfigPath:    envOrDefault("TUG_ROUTER_CONFIG_PATH", defaultRouterConfigPath),
	}
	return withConsistentIntervals(cfg)
}

// withConsistentIntervals enforces the two rules that involve more than one
// field: self-healing must not run more often than the heartbeat, and the
// reconnect backoff cannot cap below its own starting delay.
func withConsistentIntervals(cfg Config) Config {
	if cfg.SelfHealInterval < cfg.HeartbeatInterval {
		cfg.SelfHealInterval = cfg.HeartbeatInterval * 2
	}
	if cfg.ReconnectMaxDelay < cfg.ReconnectBaseDelay {
		cfg.ReconnectMaxDelay = cfg.ReconnectBaseDelay
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

// envDurationAtLeast reads a duration and lifts anything shorter than the
// minimum, so a typo cannot turn a periodic loop into a busy loop.
func envDurationAtLeast(key string, fallback time.Duration, minimum time.Duration) time.Duration {
	value := envDurationOrDefault(key, fallback)
	if value < minimum {
		return minimum
	}
	return value
}

func envIntClamped(key string, fallback int, minimum int, maximum int) int {
	value := envIntOrDefault(key, fallback)
	switch {
	case value < minimum:
		return minimum
	case value > maximum:
		return maximum
	default:
		return value
	}
}

// envPortOrDefault falls back instead of clamping, because a port outside the
// valid range is a typo and binding the nearest legal port would surprise more
// than restoring the documented default.
func envPortOrDefault(key string, fallback int) int {
	value := envIntOrDefault(key, fallback)
	if value < minPortNumber || value > maxPortNumber {
		return fallback
	}
	return value
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
