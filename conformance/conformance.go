// Package conformance runs the real agent runtime in the caller's process.
//
// It exists for one reason: the API must be able to test itself against the
// agent that actually ships, not against a hand written stand-in. The two used
// to keep separate copies of the protocol structs, and they drifted until the
// API was sending commands the agent had no handler for and field names the
// agent read under a different key. A test that speaks the same struct as the
// agent would not have caught that; only running the agent does.
//
// Nothing else should import this package. It is deliberately outside internal
// so that the API module can reach it, and deliberately tiny so that being
// public costs nothing.
package conformance

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tug.sh/services/agent/internal/agent"
	"tug.sh/services/agent/internal/config"
	"tug.sh/services/agent/internal/pairing"
)

// Options describes the agent a test wants to start. Everything else is taken
// from the agent's own defaults, so a test cannot accidentally exercise a
// configuration no real installation has.
type Options struct {
	// WebSocketURL is the API endpoint, for example ws://127.0.0.1:1234/ws/agents.
	WebSocketURL string
	ServerID     string
	AgentToken   string
	// StateDir holds the outbox and the command inbox. A test should point it
	// at t.TempDir() so that runs do not inherit each other's queued messages.
	StateDir string
	// HeartbeatInterval is shortened by tests that want to observe one without
	// waiting half a minute.
	HeartbeatInterval time.Duration
	Verbose           bool
}

// Agent is a running agent. It reconnects on its own, exactly as in production,
// until Stop is called.
type Agent struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func Start(options Options) (*Agent, error) {
	if strings.TrimSpace(options.WebSocketURL) == "" {
		return nil, fmt.Errorf("a websocket url is required")
	}
	if strings.TrimSpace(options.StateDir) == "" {
		return nil, fmt.Errorf("a state directory is required")
	}

	runtime, err := agent.NewRuntime(settings(options))
	if err != nil {
		return nil, fmt.Errorf("cannot build the agent runtime: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	running := &Agent{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(running.done)
		_ = runtime.Run(ctx)
	}()
	return running, nil
}

// Stop ends the agent and waits for its loops to finish, so a test that starts
// a second agent is not racing the first one's reconnect.
func (a *Agent) Stop() {
	a.once.Do(func() {
		a.cancel()
		select {
		case <-a.done:
		case <-time.After(10 * time.Second):
		}
	})
}

func settings(options Options) config.Config {
	heartbeat := options.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = time.Second
	}
	return config.Config{
		AgentVersion:      "conformance",
		ServerID:          options.ServerID,
		AgentToken:        options.AgentToken,
		APIWebSocketURL:   options.WebSocketURL,
		Verbose:           options.Verbose,
		HeartbeatInterval: heartbeat,
		// Long enough that the periodic snapshot never fires during a test: a
		// test asserting on snapshots wants the ones it caused.
		SelfHealInterval: time.Hour,
		// Reconnects must be quick, because half of what this package exists to
		// verify only happens on the second connection.
		ReconnectBaseDelay: 50 * time.Millisecond,
		ReconnectMaxDelay:  200 * time.Millisecond,
		OutboxPath:         filepath.Join(options.StateDir, "outbox.json"),
		CommandInboxPath:   filepath.Join(options.StateDir, "command-inbox.json"),
	}
}

// Claim performs the pairing exchange exactly as `tug init` does, so the API
// can check that the code it hands out is one this agent can actually redeem.
func Claim(ctx context.Context, apiBaseURL, code string) (serverID, token string, err error) {
	credential, err := pairing.Claim(ctx, apiBaseURL, pairing.Normalize(code))
	if err != nil {
		return "", "", err
	}
	return credential.ServerID, credential.AgentToken, nil
}
