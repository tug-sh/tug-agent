package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const commandInboxCap = 256

type commandReceipt struct {
	CommandID string                 `json:"command_id"`
	Status    string                 `json:"status"`
	Result    outboundCommandResult  `json:"result,omitempty"`
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

func (c *commandInbox) load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensurePath()
	raw, err := os.ReadFile(c.path)
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
	c.items = make(map[string]commandReceipt, len(payload.Items))
	for _, item := range payload.Items {
		if strings.TrimSpace(item.CommandID) == "" {
			continue
		}
		c.items[item.CommandID] = item
	}
	return nil
}

func (c *commandInbox) get(commandID string) (commandReceipt, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[strings.TrimSpace(commandID)]
	return item, ok
}

func (c *commandInbox) markRunning(commandID string) {
	c.upsert(commandReceipt{
		CommandID: strings.TrimSpace(commandID),
		Status:    "running",
		UpdatedAt: time.Now().UnixMilli(),
	})
}

func (c *commandInbox) markResult(result outboundCommandResult) {
	status := "succeeded"
	if !result.Success {
		status = "failed"
	}
	c.upsert(commandReceipt{
		CommandID: strings.TrimSpace(result.CommandID),
		Status:    status,
		Result:    result,
		UpdatedAt: time.Now().UnixMilli(),
	})
}

func (c *commandInbox) upsert(next commandReceipt) {
	if strings.TrimSpace(next.CommandID) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]commandReceipt{}
	}
	c.items[next.CommandID] = next
	if len(c.items) > commandInboxCap {
		var oldestID string
		var oldestAt int64
		for id, item := range c.items {
			if oldestID == "" || item.UpdatedAt < oldestAt {
				oldestID = id
				oldestAt = item.UpdatedAt
			}
		}
		delete(c.items, oldestID)
	}
	_ = c.persistLocked()
}

func (c *commandInbox) ensurePath() {
	if strings.TrimSpace(c.path) != "" {
		return
	}
	c.path = filepath.Join(GetDataDir(), "agent-command-inbox.json")
}

func (c *commandInbox) persistLocked() error {
	c.ensurePath()
	items := make([]commandReceipt, 0, len(c.items))
	for _, item := range c.items {
		items = append(items, item)
	}
	payload, err := json.Marshal(struct {
		Items []commandReceipt `json:"items"`
	}{Items: items})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(c.path, payload, 0o600)
}
