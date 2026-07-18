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
	"tug.sh/services/agent/internal/config"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func TestAgentHandshake(t *testing.T) {
	// 1. Create a dummy websocket server
	handshakeReceived := make(chan Handshake, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Expect the token and server_id in query params
		token := r.URL.Query().Get("token")
		serverID := r.URL.Query().Get("server_id")

		if token != "test-token" || serverID != "test-server-id" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("Failed to upgrade websocket: %v", err)
		}
		defer conn.Close()

		// Read the first message (should be handshake)
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read message: %v", err)
		}

		var hs Handshake
		if err := json.Unmarshal(message, &hs); err != nil {
			t.Fatalf("Failed to unmarshal handshake: %v", err)
		}

		handshakeReceived <- hs
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// 2. Initialize the agent runtime
	cfg := config.Config{
		ServerID:        "test-server-id",
		AgentToken:      "test-token",
		APIWebSocketURL: wsURL,
		WorkspaceID:     "test-workspace-id",
		Verbose:         true,
	}

	runtime, err := NewRuntime(cfg)
	if err != nil {
		t.Fatalf("Failed to create runtime: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Run agent in a goroutine
	go func() {
		_ = runtime.Run(ctx)
	}()

	// 3. Wait for the handshake payload
	select {
	case hs := <-handshakeReceived:
		if hs.Type != "handshake" {
			t.Errorf("Expected handshake type, got: %s", hs.Type)
		}
		if hs.ServerID != "test-server-id" {
			t.Errorf("Expected ServerID 'test-server-id', got: %s", hs.ServerID)
		}
		if hs.WorkspaceID != "test-workspace-id" {
			t.Errorf("Expected WorkspaceID 'test-workspace-id', got: %s", hs.WorkspaceID)
		}
		t.Logf("Successfully received handshake: %+v", hs)
	case <-ctx.Done():
		t.Fatal("Timeout waiting for handshake from agent")
	}
}
