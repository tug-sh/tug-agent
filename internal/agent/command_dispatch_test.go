package agent

import (
	"context"
	"encoding/json"
	"testing"

	"tug.sh/pkg/protocol"
)

func TestIsStreamCommandType(t *testing.T) {
	streaming := []string{"terminal_start", "terminal_input", "terminal_resize", "terminal_stop", "container_logs_tail"}
	for _, commandType := range streaming {
		if !protocol.IsStreamingCommand(commandType) {
			t.Errorf("%s should be a streaming command", commandType)
		}
	}
	for _, commandType := range []string{"deploy", "fs_read", ""} {
		if protocol.IsStreamingCommand(commandType) {
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

// Every command the API is able to send must have a handler here. This is the
// check that the two sides of the protocol still agree, and it is worth more
// than any individual handler test: a command with no handler is accepted,
// acknowledged and then quietly does nothing.
func TestEveryProtocolCommandHasAHandler(t *testing.T) {
	for _, commandType := range protocol.AllCommands {
		handler, registered := commandHandlers[commandType]
		if !registered {
			t.Errorf("the API can send %q and this agent would ignore it", commandType)
			continue
		}
		if handler == nil {
			t.Errorf("command %q has a nil handler", commandType)
		}
	}
}

// The converse: a handler for something the API cannot send is dead code, and
// the sign of a command that was renamed on one side only.
func TestNoHandlerAnswersACommandNobodySends(t *testing.T) {
	known := make(map[string]bool, len(protocol.AllCommands))
	for _, commandType := range protocol.AllCommands {
		known[commandType] = true
	}
	for commandType := range commandHandlers {
		if !known[commandType] {
			t.Errorf("nothing sends %q, so its handler is unreachable", commandType)
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
