package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tug.sh/pkg/protocol"
	"tug.sh/services/agent/internal/sandbox"
)

const commandInboxCap = 256

type commandReceipt struct {
	CommandID string                 `json:"command_id"`
	Status    string                 `json:"status"`
	Result    protocol.CommandResult `json:"result,omitempty"`
	UpdatedAt int64                  `json:"updated_at_unix_ms"`
}

type commandInbox struct {
	mu    sync.Mutex
	path  string
	items map[string]commandReceipt
}

func newCommandInbox(path string) *commandInbox {
	inbox := &commandInbox{
		path:  path,
		items: map[string]commandReceipt{},
	}
	_ = inbox.load()
	return inbox
}

func (inbox *commandInbox) load() error {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	inbox.ensurePath()
	raw, err := os.ReadFile(inbox.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var payload struct {
		Items []commandReceipt `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	inbox.items = make(map[string]commandReceipt, len(payload.Items))
	for _, item := range payload.Items {
		if strings.TrimSpace(item.CommandID) == "" {
			continue
		}
		inbox.items[item.CommandID] = item
	}
	return nil
}

func (inbox *commandInbox) get(commandID string) (commandReceipt, bool) {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	item, ok := inbox.items[strings.TrimSpace(commandID)]
	return item, ok
}

func (inbox *commandInbox) markRunning(commandID string) {
	inbox.upsert(commandReceipt{
		CommandID: strings.TrimSpace(commandID),
		Status:    "running",
		UpdatedAt: time.Now().UnixMilli(),
	})
}

func (inbox *commandInbox) markResult(result protocol.CommandResult) {
	status := "succeeded"
	if !result.Success {
		status = "failed"
	}
	inbox.upsert(commandReceipt{
		CommandID: strings.TrimSpace(result.CommandID),
		Status:    status,
		Result:    result,
		UpdatedAt: time.Now().UnixMilli(),
	})
}

func (inbox *commandInbox) upsert(next commandReceipt) {
	if strings.TrimSpace(next.CommandID) == "" {
		return
	}
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if inbox.items == nil {
		inbox.items = map[string]commandReceipt{}
	}
	inbox.items[next.CommandID] = next
	if len(inbox.items) > commandInboxCap {
		var oldestID string
		var oldestAt int64
		for id, item := range inbox.items {
			if oldestID == "" || item.UpdatedAt < oldestAt {
				oldestID = id
				oldestAt = item.UpdatedAt
			}
		}
		delete(inbox.items, oldestID)
	}
	_ = inbox.persistLocked()
}

func (inbox *commandInbox) ensurePath() {
	if strings.TrimSpace(inbox.path) != "" {
		return
	}
	inbox.path = filepath.Join(sandbox.DataDir(), "agent-command-inbox.json")
}

func (inbox *commandInbox) persistLocked() error {
	inbox.ensurePath()
	items := make([]commandReceipt, 0, len(inbox.items))
	for _, item := range inbox.items {
		items = append(items, item)
	}
	payload, err := json.Marshal(struct {
		Items []commandReceipt `json:"items"`
	}{Items: items})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(inbox.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(inbox.path, payload, 0o600)
}
