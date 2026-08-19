package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/pkg/protocol"
	"tug.sh/services/agent/internal/config"
	"tug.sh/services/agent/internal/docker"
	"tug.sh/services/agent/internal/logging"
	"tug.sh/services/agent/internal/outbox"
)

var errBoom = errors.New("boom")

// newRecordingSocket gives a test a real websocket connection whose frames can
// be read back, which is the only way to observe a signal: signals bypass the
// queue by design, so there is nothing on disk to assert against.
func newRecordingSocket(t *testing.T) (*websocket.Conn, <-chan protocol.Envelope) {
	t.Helper()
	frames := make(chan protocol.Envelope, 16)
	upgrade := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := upgrade.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer peer.Close()
		for {
			_, raw, err := peer.ReadMessage()
			if err != nil {
				return
			}
			var envelope protocol.Envelope
			if err := json.Unmarshal(raw, &envelope); err == nil {
				frames <- envelope
			}
		}
	}))
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("cannot open the test socket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, frames
}

func waitForEnvelope(t *testing.T, frames <-chan protocol.Envelope) protocol.Envelope {
	t.Helper()
	select {
	case envelope := <-frames:
		return envelope
	case <-time.After(2 * time.Second):
		t.Fatal("no frame arrived before the deadline")
		return protocol.Envelope{}
	}
}

// newTestRuntime builds a runtime with local state only: no websocket, no
// docker calls, and a silent logger so test output stays readable.
func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	stateDir := t.TempDir()

	return &Runtime{
		config: config.Config{
			ServerID:   "srv-test",
			AgentToken: "token-test",
		},
		log:                     logging.New(logging.LevelError),
		dockerManager:           docker.NewManager(),
		eventQueue:              outbox.NewQueue(filepath.Join(stateDir, "queue.json")),
		commandInbox:            newCommandInbox(filepath.Join(stateDir, "inbox.json")),
		terminals:               make(map[string]*TerminalSession),
		lastContainerDeltaState: map[string]string{},
		lastAckProgressAt:       time.Now(),
	}
}
