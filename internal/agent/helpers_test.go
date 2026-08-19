package agent

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"tug.sh/services/agent/internal/config"
	"tug.sh/services/agent/internal/docker"
	"tug.sh/services/agent/internal/logging"
	"tug.sh/services/agent/internal/protocol"
)

var errBoom = errors.New("boom")

// newTestRuntime builds a runtime with local state only: no websocket, no
// docker calls, and a silent logger so test output stays readable.
func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	stateDir := t.TempDir()

	return &Runtime{
		config: config.Config{
			ServerID:          "srv-test",
			WorkspaceID:       "ws-test",
			AgentToken:        "token-test",
			ProtocolV2Enabled: true,
		},
		log:                     logging.New(logging.LevelError),
		dockerManager:           docker.NewManager(),
		eventQueue:              protocol.NewQueue(filepath.Join(stateDir, "queue.json")),
		commandInbox:            newCommandInbox(filepath.Join(stateDir, "inbox.json")),
		terminals:               make(map[string]*TerminalSession),
		lastContainerDeltaState: map[string]string{},
		lastAckProgressAt:       time.Now(),
	}
}
