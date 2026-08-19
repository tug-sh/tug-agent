package agent

import (
	"context"
	"encoding/json"
	"testing"

	"tug.sh/services/agent/internal/protocol"
)

func TestIsStreamCommandType(t *testing.T) {
	streaming := []string{"terminal_start", "terminal_input", "terminal_resize", "terminal_stop", "container_logs_tail"}
	for _, commandType := range streaming {
		if !isStreamCommandType(commandType) {
			t.Errorf("%s should be a streaming command", commandType)
		}
	}
	for _, commandType := range []string{"deploy", "fs_read", ""} {
		if isStreamCommandType(commandType) {
			t.Errorf("%s should not be a streaming command", commandType)
		}
	}
}

// An older agent must ignore commands introduced by a newer API instead of
// failing the whole session.
func TestExecuteCommandIgnoresUnknownType(t *testing.T) {
	runtime := newTestRuntime(t)

	logs, err := runtime.executeCommand(context.Background(), nil, protocol.Command{Type: "from_the_future"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if logs != nil {
		t.Fatalf("expected no logs, got %v", logs)
	}
}

func TestEveryHandlerIsRegistered(t *testing.T) {
	for commandType, handler := range commandHandlers {
		if handler == nil {
			t.Errorf("command %q has a nil handler", commandType)
		}
	}
	for _, required := range []string{"deploy", "git_deploy", "container_action", "fs_read", "self_update"} {
		if _, ok := commandHandlers[required]; !ok {
			t.Errorf("command %q is not registered", required)
		}
	}
}

func TestCommandRequestSetPayload(t *testing.T) {
	var payload json.RawMessage
	request := commandRequest{payload: &payload}

	request.setPayload(map[string]string{"status": "ok"})
	if string(payload) != `{"status":"ok"}` {
		t.Fatalf("payload = %s, want the marshalled value", payload)
	}

	// Values that cannot be marshalled leave the textual logs as the result.
	request.setPayload(make(chan int))
	if string(payload) != `{"status":"ok"}` {
		t.Fatalf("payload = %s, want it unchanged", payload)
	}

	// A handler may run without a payload slot at all.
	commandRequest{}.setPayload("ignored")
}

func TestCommandRequestTrimsIdentifiers(t *testing.T) {
	request := commandRequest{command: protocol.Command{ContainerID: "  c1 ", Domain: " app.example.com "}}
	if got := request.containerID(); got != "c1" {
		t.Errorf("containerID() = %q, want %q", got, "c1")
	}
	if got := request.domain(); got != "app.example.com" {
		t.Errorf("domain() = %q, want %q", got, "app.example.com")
	}
}

func TestClampTailLines(t *testing.T) {
	cases := map[int]int{0: defaultLogsTailLines, -5: defaultLogsTailLines, 50: 50, 999999: maxLogsTailLines}
	for requested, want := range cases {
		if got := clampTailLines(requested); got != want {
			t.Errorf("clampTailLines(%d) = %d, want %d", requested, got, want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", " value ", "other"); got != "value" {
		t.Errorf("firstNonEmpty() = %q, want %q", got, "value")
	}
	if got := firstNonEmpty("", "   "); got != "" {
		t.Errorf("firstNonEmpty() = %q, want an empty string", got)
	}
}

func TestDescribeCommandOutput(t *testing.T) {
	if got := describeCommandOutput("done\n", nil); got != "done\n" {
		t.Errorf("real output must win, got %q", got)
	}
	if got := describeCommandOutput("  ", errBoom); got != "Command failed: boom" {
		t.Errorf("got %q, want the failure description", got)
	}
	if got := describeCommandOutput("", nil); got != "Command executed cleanly (no output)." {
		t.Errorf("got %q, want the clean-run description", got)
	}
}
