package config

import (
	_ "embed"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed VERSION
var embeddedVersion string

var defaultAgentVersion = strings.TrimSpace(embeddedVersion)

type Config struct {
	AgentVersion    string
	ServerID        string
	AgentToken      string
	APIWebSocketURL string
	// APIBaseURL is the plain HTTP address of the same control plane. Pairing
	// happens over HTTP before there is any credential to open a socket with.
	APIBaseURL         string
	DashboardURL       string
	AgentEnvPath       string
	Verbose            bool
	LogLevel           string
	TrafficProfile     string
	HeartbeatInterval  time.Duration
	SelfHealInterval   time.Duration
	ReconnectBaseDelay time.Duration
	ReconnectMaxDelay  time.Duration
	ReconnectJitterPct int
	// OutboxPath is where undelivered messages wait, and CommandInboxPath is
	// where commands already handled are remembered so a replay after a
	// reconnect is not executed twice. An empty value means the default
	// location under the agent's data directory.
	OutboxPath       string
	CommandInboxPath string
	RouterImage      string
	RouterNetwork    string
	RouterHTTPPort   int
	RouterHTTPSPort  int
	RouterConfigPath string
}

// Edge router defaults. They describe the reference tug-router installation and
// can be overridden per host through the TUG_ROUTER_* variables.
const (
	defaultRouterImage      = "caddy:2"
	defaultRouterHTTPPort   = 80
	defaultRouterHTTPSPort  = 443
	defaultRouterConfigPath = "/etc/caddy/Caddyfile"
)

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
	// The token is opaque. It used to carry the server id base64 encoded in the
	// middle, which meant rotating a token could change which machine the agent
	// claimed to be. Both values are now written separately by `tug init`.
	agentToken := envOrDefault("TUG_AGENT_TOKEN", "")
	serverID := strings.TrimSpace(envOrDefault("TUG_SERVER_ID", ""))
	debugProfileEnabled := envBoolOrDefault("TUG_AGENT_DEBUG_PROFILE", false)
	trafficProfile := normalizeTrafficProfile(envOrDefault("TUG_AGENT_TRAFFIC_PROFILE", defaultTrafficProfile))
	if debugProfileEnabled {
		trafficProfile = debugTrafficProfile
	}
	heartbeatDefault := 15 * time.Second
	selfHealDefault := 15 * time.Minute
	if trafficProfile == debugTrafficProfile {
		heartbeatDefault = 15 * time.Second
		selfHealDefault = 5 * time.Minute
	}
	reconnectBaseDefault := 1 * time.Second
	reconnectMaxDefault := 30 * time.Second
	reconnectJitterDefault := 20
	websocketURL := envOrDefault("TUG_API_WS_URL", "wss://api.tug.sh/ws/agents")
	cfg := Config{
		AgentVersion:       envOrDefault("TUG_AGENT_VERSION", defaultAgentVersion),
		ServerID:           serverID,
		AgentToken:         agentToken,
		APIWebSocketURL:    websocketURL,
		APIBaseURL:         envOrDefault("TUG_API_URL", httpAddressOf(websocketURL)),
		DashboardURL:       envOrDefault("TUG_DASHBOARD_URL", "https://app.tug.sh"),
		AgentEnvPath:       envOrDefault("TUG_AGENT_ENV_PATH", "/etc/tug/agent.env"),
		Verbose:            envBoolOrDefault("TUG_VERBOSE", true),
		LogLevel:           envOrDefault("TUG_LOG_LEVEL", ""),
		TrafficProfile:     trafficProfile,
		HeartbeatInterval:  envDurationAtLeast("TUG_AGENT_HEARTBEAT_INTERVAL", heartbeatDefault, minHeartbeatInterval),
		SelfHealInterval:   envDurationAtLeast("TUG_AGENT_SELF_HEAL_INTERVAL", selfHealDefault, minHeartbeatInterval),
		ReconnectBaseDelay: envDurationAtLeast("TUG_AGENT_RECONNECT_BASE_DELAY", reconnectBaseDefault, minReconnectBaseDelay),
		ReconnectMaxDelay:  envDurationAtLeast("TUG_AGENT_RECONNECT_MAX_DELAY", reconnectMaxDefault, minReconnectBaseDelay),
		ReconnectJitterPct: envIntClamped("TUG_AGENT_RECONNECT_JITTER_PCT", reconnectJitterDefault, 0, maxReconnectJitterPct),
		OutboxPath:         envOrDefault("TUG_AGENT_OUTBOX_PATH", ""),
		CommandInboxPath:   envOrDefault("TUG_AGENT_COMMAND_INBOX_PATH", ""),
		RouterImage:        envOrDefault("TUG_ROUTER_IMAGE", defaultRouterImage),
		RouterNetwork:      envOrDefault("TUG_ROUTER_NETWORK", ""),
		RouterHTTPPort:     envPortOrDefault("TUG_ROUTER_HTTP_PORT", defaultRouterHTTPPort),
		RouterHTTPSPort:    envPortOrDefault("TUG_ROUTER_HTTPS_PORT", defaultRouterHTTPSPort),
		RouterConfigPath:   envOrDefault("TUG_ROUTER_CONFIG_PATH", defaultRouterConfigPath),
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

// httpAddressOf turns the socket endpoint into the HTTP root of the same
// deployment, so pointing an agent at a private control plane stays one
// setting rather than two that can disagree.
func httpAddressOf(websocketURL string) string {
	address := strings.TrimSpace(websocketURL)
	switch {
	case strings.HasPrefix(address, "wss://"):
		address = "https://" + strings.TrimPrefix(address, "wss://")
	case strings.HasPrefix(address, "ws://"):
		address = "http://" + strings.TrimPrefix(address, "ws://")
	}
	if root, _, found := strings.Cut(address, "/ws/agents"); found {
		return root
	}
	return strings.TrimRight(address, "/")
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
