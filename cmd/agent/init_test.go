package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tug.sh/services/agent/internal/config"
)

func TestInitNeedsBothHalvesOfThePairing(t *testing.T) {
	for _, args := range [][]string{
		{"init"},
		{"init", "onlyTheServerID"},
	} {
		if _, _, err := pairingArguments(args); err == nil {
			t.Errorf("pairingArguments(%v) succeeded, want the usage message", args)
		}
	}
}

func TestInitReadsThePairingPositionally(t *testing.T) {
	serverID, token, err := pairingArguments([]string{"init", "abc123", "tug_deadbeef"})
	if err != nil {
		t.Fatalf("pairingArguments returned %v", err)
	}
	if serverID != "abc123" || token != "tug_deadbeef" {
		t.Fatalf("got %q %q, want the two positional arguments", serverID, token)
	}
}

// The settings file is the only place the pairing lives, and the daemon reads
// both halves from it. Writing one without the other leaves an agent that
// cannot say who it is.
func TestWriteAgentEnvStoresBothHalves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	if err := writeAgentEnv(path, config.Config{APIWebSocketURL: defaultWebSocketURL}, "abc123", "tug_deadbeef"); err != nil {
		t.Fatalf("writeAgentEnv returned %v", err)
	}

	values, err := readAgentEnv(path)
	if err != nil {
		t.Fatalf("readAgentEnv returned %v", err)
	}
	if values["TUG_SERVER_ID"] != "abc123" {
		t.Errorf("TUG_SERVER_ID = %q", values["TUG_SERVER_ID"])
	}
	if values["TUG_AGENT_TOKEN"] != "tug_deadbeef" {
		t.Errorf("TUG_AGENT_TOKEN = %q", values["TUG_AGENT_TOKEN"])
	}
	if _, overridden := values["TUG_API_WS_URL"]; overridden {
		t.Error("the default endpoint was written out, which pins the agent to today's URL")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("cannot stat the settings file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600 for a file holding a token", mode)
	}
}

func TestWriteAgentEnvKeepsANonDefaultEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	settings := config.Config{APIWebSocketURL: "ws://127.0.0.1:8080/ws/agents"}
	if err := writeAgentEnv(path, settings, "abc123", "tug_deadbeef"); err != nil {
		t.Fatalf("writeAgentEnv returned %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the settings file: %v", err)
	}
	if !strings.Contains(string(content), settings.APIWebSocketURL) {
		t.Errorf("the settings file lost the endpoint the agent was started with:\n%s", content)
	}
}
