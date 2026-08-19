package main

import (
	"testing"

	"tug.sh/services/agent/internal/config"
)

// Every command the help text advertises must be either a one-shot command or
// a daemon command, otherwise `tug <command>` exits as unknown.
func TestCliCommandsCoverDocumentedCommands(t *testing.T) {
	documented := []string{
		"init", "start", "status", "stop", "restart",
		"logs", "update", "disconnect", "remove", "version", "help",
	}
	table := cliCommands()
	for _, command := range documented {
		if _, exists := table[command]; !exists {
			t.Errorf("command %q is documented but not registered", command)
		}
	}
	for _, daemonCommand := range []string{"run", "daemon", "run-service", "service"} {
		if _, exists := table[daemonCommand]; exists {
			t.Errorf("daemon command %q must not be handled as a one-shot command", daemonCommand)
		}
	}
}

func TestCliCommandsRunLocally(t *testing.T) {
	cfg := config.Config{AgentVersion: "9.9.9"}
	for _, command := range []string{"help", "version"} {
		if err := cliCommands()[command].run(cfg, nil); err != nil {
			t.Errorf("%s failed: %v", command, err)
		}
	}
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no arguments", nil, ""},
		{"positional command", []string{"status"}, "status"},
		{"padded command", []string{"  init  "}, "init"},
		{"command with extra args", []string{"logs", "50"}, "logs"},
		{"flag only", []string{"--test-mode"}, ""},
		{"flag before command", []string{"-v", "status"}, ""},
		{"unknown command", []string{"frobnicate"}, "frobnicate"},
	}
	for _, testCase := range cases {
		if got := parseCommand(testCase.args); got != testCase.want {
			t.Errorf("%s: parseCommand(%v) = %q, want %q", testCase.name, testCase.args, got, testCase.want)
		}
	}
}

func TestHasToken(t *testing.T) {
	args := []string{"run", " --test-mode "}
	if !hasToken(args, "--test-mode") {
		t.Error("expected the padded token to be found")
	}
	if hasToken(args, "--verbose") {
		t.Error("did not expect an absent token to be found")
	}
	if hasToken(nil, "--test-mode") {
		t.Error("did not expect a token in an empty argument list")
	}
}

func TestParseLogsLimitIgnoresSubcommand(t *testing.T) {
	if got := parseLogsLimit([]string{"logs"}); got != defaultAgentLogLines {
		t.Errorf("parseLogsLimit() = %d, want the default %d", got, defaultAgentLogLines)
	}
	if got := parseLogsLimit([]string{"logs", "50"}); got != 50 {
		t.Errorf("parseLogsLimit() = %d, want 50", got)
	}
}
