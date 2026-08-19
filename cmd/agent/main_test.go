package main

import (
	"path/filepath"
	"testing"

	"tug.sh/services/agent/internal/config"
)

// An unpaired machine cannot start: there is nothing to authenticate with, and
// silently pairing itself is exactly the behaviour that let an agent invent an
// identity nobody had asked for.
func TestStartRefusesAnUnpairedMachine(t *testing.T) {
	settings := config.Config{AgentEnvPath: filepath.Join(t.TempDir(), "agent.env")}

	if err := runStart(settings); err == nil {
		t.Fatal("runStart succeeded without a pairing")
	}
}

func TestAgentVersionIsSet(t *testing.T) {
	if version := config.Load().AgentVersion; version == "" {
		t.Error("the agent reports no version, so the dashboard cannot offer updates")
	}
}
