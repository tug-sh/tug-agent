package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/pkg/protocol"
	"tug.sh/services/agent/internal/config"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// TestAgentSendsSnapshotOnConnect checks the shape of the first thing an agent
// says: a runtime snapshot envelope, at the current protocol version, with the
// machine's own facts in the payload.
func TestAgentSendsSnapshotOnConnect(t *testing.T) {
	received := make(chan protocol.Envelope, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if token != "test-token" || r.URL.Query().Get("server_id") != "test-server-id" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var envelope protocol.Envelope
			if err := json.Unmarshal(message, &envelope); err != nil {
				continue
			}
			if envelope.Entity == protocol.EntityRuntime && envelope.Action == protocol.ActionSnapshot {
				received <- envelope
				return
			}
		}
	}))
	defer server.Close()

	runtime, err := NewRuntime(config.Config{
		ServerID:        "test-server-id",
		AgentToken:      "test-token",
		APIWebSocketURL: "ws" + strings.TrimPrefix(server.URL, "http"),
		OutboxPath:      t.TempDir() + "/queue.json",
		Verbose:         false,
	})
	if err != nil {
		t.Fatalf("cannot create the runtime: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = runtime.Run(ctx) }()

	select {
	case envelope := <-received:
		if !envelope.VersionMatches() {
			t.Fatalf("the snapshot claims protocol version %q", envelope.ProtocolVersion)
		}
		if envelope.ServerID != "test-server-id" {
			t.Fatalf("the snapshot names server %q", envelope.ServerID)
		}
		if envelope.Seq == 0 {
			t.Fatal("a snapshot is a fact and must carry a sequence number")
		}
		var snapshot protocol.Handshake
		if err := json.Unmarshal(envelope.Payload, &snapshot); err != nil {
			t.Fatalf("cannot read the snapshot payload: %v", err)
		}
		if snapshot.HostName == "" {
			t.Fatal("the snapshot carries no hostname")
		}
	case <-ctx.Done():
		t.Fatal("no snapshot arrived before the deadline")
	}
}
