package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/pkg/protocol"
)

const (
	// A session that survives this long is treated as healthy, which resets
	// the reconnect backoff.
	stableSessionThreshold = 20 * time.Second
	websocketPingPeriod    = 20 * time.Second
	defaultHeartbeatPeriod = 30 * time.Second
	defaultSelfHealPeriod  = 15 * time.Minute
	containerRefreshPeriod = 30 * time.Second
	writeTimeout           = 10 * time.Second
	pingTimeout            = 8 * time.Second
)

// connectAndServe runs a single websocket session. The boolean result reports
// whether the session lived long enough to be considered stable, which resets
// the reconnect backoff in Run.
func (runtime *Runtime) connectAndServe(ctx context.Context) (bool, error) {
	conn, err := runtime.dialAPI(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	// Terminal sessions are bound to this connection's lifetime. When it ends,
	// kill their shells so they don't leak or duplicate output after reconnect.
	defer runtime.closeAllTerminals()

	sessionStartedAt := time.Now()
	_ = conn.SetReadDeadline(time.Time{})
	conn.SetPongHandler(func(string) error {
		return nil
	})

	if handshakeErr := runtime.sendSnapshot(); handshakeErr != nil {
		return runtime.wasSessionStable(sessionStartedAt), handshakeErr
	}
	runtime.log.Debug("initial handshake sent")

	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	runtime.startSessionWorkers(sessionCtx, conn)

	readErr := runtime.readServerFrames(ctx, conn)
	if isPendingAuthError(readErr) || isNonRetriable(readErr) || isDuplicateConnection(readErr) {
		return false, readErr
	}
	return runtime.wasSessionStable(sessionStartedAt), readErr
}

func (runtime *Runtime) wasSessionStable(startedAt time.Time) bool {
	return time.Since(startedAt) >= stableSessionThreshold
}

func (runtime *Runtime) dialAPI(ctx context.Context) (*websocket.Conn, error) {
	if strings.TrimSpace(runtime.config.ServerID) == "" {
		return nil, errors.New("server_id cannot be derived from token; run `tug init`")
	}
	if strings.TrimSpace(runtime.config.AgentToken) == "" {
		return nil, errors.New("agent token is missing; run `tug init`")
	}
	url := fmt.Sprintf("%s?server_id=%s", runtime.config.APIWebSocketURL, runtime.config.ServerID)
	runtime.log.Debug("dial websocket: %s", url)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+runtime.config.AgentToken)
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, url, headers)
	if err == nil {
		runtime.log.Debug("websocket connected")
		return conn, nil
	}
	if isAuthRejection(response) {
		return nil, markPendingAuth(fmt.Errorf(
			"websocket authorization rejected (status %d); token may be pending pairing in dashboard",
			response.StatusCode,
		))
	}
	return nil, fmt.Errorf("websocket dial failed: %w", err)
}

func isAuthRejection(response *http.Response) bool {
	if response == nil {
		return false
	}
	switch response.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return true
	default:
		return false
	}
}

// startSessionWorkers launches the background loops bound to one session. They
// all stop when sessionCtx is cancelled, which also closes the connection.
func (runtime *Runtime) startSessionWorkers(sessionCtx context.Context, conn *websocket.Conn) {
	go func() {
		<-sessionCtx.Done()
		_ = conn.Close()
	}()
	go runtime.periodicWSPing(sessionCtx, conn, websocketPingPeriod)
	go runtime.periodicHeartbeat(
		sessionCtx,
		conn,
		positiveOrDefault(runtime.config.HeartbeatInterval, defaultHeartbeatPeriod),
	)
	go runtime.periodicSelfHealSnapshot(
		sessionCtx,
		conn,
		positiveOrDefault(runtime.config.SelfHealInterval, defaultSelfHealPeriod),
	)
	go runtime.periodicContainerStatusRefresh(sessionCtx, conn)

	runtime.ackStateMu.Lock()
	runtime.lastAckSeq = runtime.eventQueue.AckUptoSeq()
	runtime.lastAckProgressAt = time.Now()
	runtime.ackStateMu.Unlock()
	go runtime.flushEventQueue(sessionCtx, conn)
	go runtime.periodicQueueSelfHeal(sessionCtx, conn)
}

// readServerFrames consumes the session until the connection fails or the API
// rejects the agent. It always returns a non-nil error.
func (runtime *Runtime) readServerFrames(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, message, readErr := conn.ReadMessage()
		if readErr != nil {
			runtime.logDisconnectCause(readErr)
			if isDuplicateServerClose(readErr) {
				return markDuplicateConnection(readErr)
			}
			return readErr
		}
		handled, controlErr := runtime.handleControlFrame(message)
		if controlErr != nil {
			return controlErr
		}
		if handled || runtime.consumeAck(message) {
			continue
		}
		var command protocol.Command
		if unmarshalErr := json.Unmarshal(message, &command); unmarshalErr != nil {
			runtime.log.Warn("invalid command payload: %v", unmarshalErr)
			continue
		}
		if command.Type == "" {
			runtime.log.Debug("ignoring a frame that is neither an ack nor a command")
			continue
		}
		runtime.log.Debug("received command: type=%s command_id=%s", command.Type, command.CommandID)
		go runtime.runCommand(ctx, conn, command)
	}
}

// isDuplicateServerClose reports whether the API closed this connection because
// another agent is already live on the same server_id.
func isDuplicateServerClose(err error) bool {
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr) && strings.Contains(closeErr.Text, protocol.CloseReasonDuplicateServer)
}

// logDisconnectCause explains why the session ended in terms the operator can
// act on. A bare "1006 unexpected EOF" hides whether the API deliberately
// closed the socket (for example because the same token is live on another
// machine) or the network dropped it.
func (runtime *Runtime) logDisconnectCause(err error) {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch {
		case strings.Contains(closeErr.Text, protocol.CloseReasonDuplicateServer):
			runtime.log.Warn(
				"the API rejected this connection: another agent is already live on this server_id. "+
					"Check for a cloned VPS, a copied agent.env, or a second tug agent process (close %d).",
				closeErr.Code,
			)
		case strings.Contains(closeErr.Text, protocol.CloseReasonReplaced):
			runtime.log.Warn(
				"the API replaced this connection with a newer one for the same server_id; "+
					"reconnecting. A persistent loop here means the token is live elsewhere (close %d).",
				closeErr.Code,
			)
		default:
			runtime.log.Warn("server closed the connection: %s (close %d)", closeErr.Text, closeErr.Code)
		}
		return
	}
	if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
		runtime.log.Warn("connection dropped without a close frame (network or proxy): %v", err)
		return
	}
	runtime.log.Debug("read loop ended: %v", err)
}

// handleControlFrame processes transport level frames. It reports whether the
// frame was consumed, and returns an error only when the session must end.
func (runtime *Runtime) handleControlFrame(message []byte) (bool, error) {
	var frame protocol.ControlFrame
	if err := json.Unmarshal(message, &frame); err != nil {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(frame.Type)) {
	case protocol.TypeAuthError:
		details := strings.TrimSpace(frame.Error)
		if details == "" {
			details = "unauthorized agent connection"
		}
		return true, markPendingAuth(fmt.Errorf(
			"websocket authorization rejected: %s; token may be pending pairing in dashboard",
			details,
		))
	case protocol.TypeKeepalive:
		return true, nil
	default:
		return false, nil
	}
}

// runEvery calls work on every tick until the context ends or work reports
// that the loop is done. Every session worker is built on top of it.
func runEvery(ctx context.Context, interval time.Duration, work func() bool) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !work() {
				return
			}
		}
	}
}

// endSession drops the connection so Run reconnects with a clean state, and
// reports false to stop the calling worker loop.
func (runtime *Runtime) endSession(conn *websocket.Conn, reason string, err error) bool {
	runtime.log.Debug("%s: %v", reason, err)
	_ = conn.Close()
	return false
}

func (runtime *Runtime) periodicHeartbeat(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	runEvery(ctx, interval, func() bool {
		if err := runtime.sendHeartbeat(conn); err != nil {
			return runtime.endSession(conn, "periodic heartbeat failed", err)
		}
		runtime.log.Debug("periodic heartbeat sent")
		return true
	})
}

func (runtime *Runtime) periodicSelfHealSnapshot(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	runEvery(ctx, interval, func() bool {
		if err := runtime.sendSnapshot(); err != nil {
			return runtime.endSession(conn, "periodic self-heal snapshot failed", err)
		}
		runtime.log.Debug("periodic self-heal snapshot sent")
		return true
	})
}

func (runtime *Runtime) periodicContainerStatusRefresh(ctx context.Context, conn *websocket.Conn) {
	interval := containerRefreshPeriod
	if runtime.config.HeartbeatInterval > 0 && runtime.config.HeartbeatInterval < interval {
		interval = runtime.config.HeartbeatInterval
	}
	runEvery(ctx, interval, func() bool {
		runtime.publishAllContainerStatuses(ctx, conn)
		return true
	})
}

func (runtime *Runtime) periodicWSPing(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	runEvery(ctx, interval, func() bool {
		runtime.writeMu.Lock()
		_ = conn.SetWriteDeadline(time.Now().Add(pingTimeout))
		err := conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(pingTimeout))
		_ = conn.SetWriteDeadline(time.Time{})
		runtime.writeMu.Unlock()
		if err != nil {
			return runtime.endSession(conn, "websocket ping failed", err)
		}
		return true
	})
}
