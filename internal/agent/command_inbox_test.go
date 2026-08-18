package agent

import (
	"path/filepath"
	"testing"
)

func TestCommandInboxIdempotentReceipt(t *testing.T) {
	inbox := newCommandInbox(filepath.Join(t.TempDir(), "inbox.json"))
	inbox.markRunning("cmd-1")
	receipt, ok := inbox.get("cmd-1")
	if !ok || receipt.Status != "running" {
		t.Fatalf("expected running receipt")
	}
	inbox.markResult(outboundCommandResult{
		Type:      "command_result",
		CommandID: "cmd-1",
		Success:   true,
		Logs:      []string{"ok"},
	})
	reloaded := newCommandInbox(inbox.path)
	got, ok := reloaded.get("cmd-1")
	if !ok || got.Status != "succeeded" || !got.Result.Success {
		t.Fatalf("expected persisted succeeded receipt, got %+v ok=%v", got, ok)
	}
}
