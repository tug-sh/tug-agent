package main

import (
	"os"
	"path/filepath"
	"testing"

	"tug.sh/services/agent/internal/config"
)

func TestRunStart_Unconfigured(t *testing.T) {
	tempDir := t.TempDir()
	envPath := filepath.Join(tempDir, "agent.env")

	cfg := config.Config{
		ServerID:     "",
		AgentToken:   "",
		AgentEnvPath: envPath,
		DashboardURL: "https://app.tug.sh",
	}

	err := runStart(cfg)
	if err != nil {
		t.Fatalf("expected runStart to succeed when unconfigured, got: %v", err)
	}

	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		t.Errorf("expected environment file to be created at %s", envPath)
	}
}

func TestRunStart_Configured(t *testing.T) {
	tempDir := t.TempDir()
	envPath := filepath.Join(tempDir, "agent.env")

	cfg := config.Config{
		ServerID:     "srv_test_1234",
		AgentToken:   "agtv2.c3J2X3Rlc3RfMTIzNA.1234567890abcdef",
		AgentEnvPath: envPath,
	}

	err := runStart(cfg)
	if err != nil {
		t.Fatalf("expected runStart to succeed when configured, got: %v", err)
	}
}

func TestAgentVersion(t *testing.T) {
	cfg := config.Load()
	if cfg.AgentVersion != "1.0.4" {
		t.Errorf("expected AgentVersion to be 1.0.4, got: %s", cfg.AgentVersion)
	}
}
