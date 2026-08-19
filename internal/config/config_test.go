package config

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg := Load()

	if cfg.AgentVersion != defaultAgentVersion {
		t.Errorf("AgentVersion = %q, want %q", cfg.AgentVersion, defaultAgentVersion)
	}
	if cfg.HeartbeatInterval != 30*time.Second {
		t.Errorf("HeartbeatInterval = %s, want 30s", cfg.HeartbeatInterval)
	}
	if cfg.RouterImage != defaultRouterImage || cfg.RouterConfigPath != defaultRouterConfigPath {
		t.Errorf("unexpected router defaults: %+v", cfg)
	}
	if cfg.RouterHTTPPort != defaultRouterHTTPPort || cfg.RouterHTTPSPort != defaultRouterHTTPSPort {
		t.Errorf("unexpected router ports: %d/%d", cfg.RouterHTTPPort, cfg.RouterHTTPSPort)
	}
	if cfg.TrafficProfile != defaultTrafficProfile {
		t.Errorf("TrafficProfile = %q, want %q", cfg.TrafficProfile, defaultTrafficProfile)
	}
}

// A hand-edited agent.env must never produce a loop that hammers the API.
func TestLoadClampsIntervals(t *testing.T) {
	t.Setenv("TUG_AGENT_HEARTBEAT_INTERVAL", "100ms")
	t.Setenv("TUG_AGENT_SELF_HEAL_INTERVAL", "1s")
	t.Setenv("TUG_AGENT_RECONNECT_BASE_DELAY", "1ms")
	t.Setenv("TUG_AGENT_RECONNECT_MAX_DELAY", "1ms")

	cfg := Load()

	if cfg.HeartbeatInterval != minHeartbeatInterval {
		t.Errorf("HeartbeatInterval = %s, want the %s minimum", cfg.HeartbeatInterval, minHeartbeatInterval)
	}
	if cfg.SelfHealInterval < cfg.HeartbeatInterval {
		t.Errorf("SelfHealInterval (%s) must not be shorter than the heartbeat (%s)", cfg.SelfHealInterval, cfg.HeartbeatInterval)
	}
	if cfg.ReconnectBaseDelay != minReconnectBaseDelay {
		t.Errorf("ReconnectBaseDelay = %s, want the %s minimum", cfg.ReconnectBaseDelay, minReconnectBaseDelay)
	}
	if cfg.ReconnectMaxDelay < cfg.ReconnectBaseDelay {
		t.Errorf("ReconnectMaxDelay (%s) must not be below the base delay (%s)", cfg.ReconnectMaxDelay, cfg.ReconnectBaseDelay)
	}
}

func TestLoadIgnoresUnparsableDurations(t *testing.T) {
	t.Setenv("TUG_AGENT_HEARTBEAT_INTERVAL", "soon")
	if got := Load().HeartbeatInterval; got != 30*time.Second {
		t.Errorf("HeartbeatInterval = %s, want the 30s default", got)
	}
}

func TestLoadClampsJitter(t *testing.T) {
	t.Setenv("TUG_AGENT_RECONNECT_JITTER_PCT", "500")
	if got := Load().ReconnectJitterPct; got != maxReconnectJitterPct {
		t.Errorf("ReconnectJitterPct = %d, want %d", got, maxReconnectJitterPct)
	}

	t.Setenv("TUG_AGENT_RECONNECT_JITTER_PCT", "-10")
	if got := Load().ReconnectJitterPct; got != 0 {
		t.Errorf("ReconnectJitterPct = %d, want 0", got)
	}
}

// An out-of-range port is a typo, so the documented default is restored
// instead of binding the nearest legal port.
func TestLoadRestoresDefaultForInvalidPort(t *testing.T) {
	t.Setenv("TUG_ROUTER_HTTP_PORT", "99999")
	t.Setenv("TUG_ROUTER_HTTPS_PORT", "8443")

	cfg := Load()
	if cfg.RouterHTTPPort != defaultRouterHTTPPort {
		t.Errorf("RouterHTTPPort = %d, want %d", cfg.RouterHTTPPort, defaultRouterHTTPPort)
	}
	if cfg.RouterHTTPSPort != 8443 {
		t.Errorf("RouterHTTPSPort = %d, want 8443", cfg.RouterHTTPSPort)
	}
}

func TestLoadDebugTrafficProfile(t *testing.T) {
	t.Setenv("TUG_AGENT_DEBUG_PROFILE", "true")
	cfg := Load()

	if cfg.TrafficProfile != debugTrafficProfile {
		t.Fatalf("TrafficProfile = %q, want %q", cfg.TrafficProfile, debugTrafficProfile)
	}
	if cfg.HeartbeatInterval != 15*time.Second || cfg.SelfHealInterval != 5*time.Minute {
		t.Fatalf("debug profile should shorten the loops, got %s/%s", cfg.HeartbeatInterval, cfg.SelfHealInterval)
	}
}

func TestLoadDerivesServerIDFromToken(t *testing.T) {
	serverID := "srv_vps_1234"
	token := "agtv2." + base64.RawURLEncoding.EncodeToString([]byte(serverID)) + ".secret"
	t.Setenv("TUG_AGENT_TOKEN", token)
	t.Setenv("TUG_SERVER_ID", "srv_from_env")

	if got := Load().ServerID; got != serverID {
		t.Fatalf("ServerID = %q, want %q", got, serverID)
	}
}

func TestLoadFallsBackToLegacyServerID(t *testing.T) {
	t.Setenv("TUG_AGENT_TOKEN", "legacy-token")
	t.Setenv("TUG_SERVER_ID", "srv_legacy")

	if got := Load().ServerID; got != "srv_legacy" {
		t.Fatalf("ServerID = %q, want %q", got, "srv_legacy")
	}
}

func TestParseServerIDFromToken(t *testing.T) {
	valid := "agtv2." + base64.RawURLEncoding.EncodeToString([]byte(" srv_1 ")) + ".secret"
	if got := parseServerIDFromToken(valid); got != "srv_1" {
		t.Errorf("parseServerIDFromToken() = %q, want %q", got, "srv_1")
	}

	invalid := []string{"", "agtv1.abc.def", "agtv2.abc", "agtv2.!!!.def"}
	for _, token := range invalid {
		if got := parseServerIDFromToken(token); got != "" {
			t.Errorf("parseServerIDFromToken(%q) = %q, want an empty string", token, got)
		}
	}
}

func TestEnvBoolOrDefault(t *testing.T) {
	cases := map[string]bool{"1": true, "yes": true, "ON": true, "0": false, "no": false, "OFF": false}
	for value, want := range cases {
		t.Setenv("TUG_TEST_BOOL", value)
		if got := envBoolOrDefault("TUG_TEST_BOOL", !want); got != want {
			t.Errorf("envBoolOrDefault(%q) = %v, want %v", value, got, want)
		}
	}
	t.Setenv("TUG_TEST_BOOL", "maybe")
	if !envBoolOrDefault("TUG_TEST_BOOL", true) {
		t.Error("an unparsable value must fall back to the default")
	}
}

func TestNormalizeTrafficProfile(t *testing.T) {
	if got := normalizeTrafficProfile(" DEBUG "); got != debugTrafficProfile {
		t.Errorf("normalizeTrafficProfile() = %q, want %q", got, debugTrafficProfile)
	}
	if got := normalizeTrafficProfile("whatever"); got != defaultTrafficProfile {
		t.Errorf("normalizeTrafficProfile() = %q, want %q", got, defaultTrafficProfile)
	}
}
