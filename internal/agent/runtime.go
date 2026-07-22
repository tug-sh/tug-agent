package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/services/agent/internal/config"
)

type Runtime struct {
	config        config.Config
	fileManager   *FileManager
	dockerManager *DockerManager
	updater       *Updater
	writeMu       sync.Mutex
	v2Queue       *durableEventQueueV2

	termMu                  sync.Mutex
	terminals               map[string]*TerminalSession
	containerDeltaStateMu   sync.Mutex
	lastContainerDeltaState map[string]string
	ackStateMu              sync.Mutex
	lastAckSeq              uint64
	lastAckProgressAt       time.Time
	lastQueueResetAt        time.Time
}

type nonRetriableError struct {
	err error
}

func (e nonRetriableError) Error() string {
	return e.err.Error()
}

func (e nonRetriableError) Unwrap() error {
	return e.err
}

func markNonRetriable(err error) error {
	if err == nil {
		return nil
	}
	return nonRetriableError{err: err}
}

func isNonRetriable(err error) bool {
	var target nonRetriableError
	return errors.As(err, &target)
}

func (r *Runtime) logReconnectRecoveryHint(reason error) {
	envPath := strings.TrimSpace(r.config.AgentEnvPath)
	if envPath == "" {
		envPath = "/etc/tug/agent.env"
	}
	log.Printf("reconnect disabled: %v", reason)
	log.Printf("recovery steps:")
	log.Printf("1) Verify agent state: sudo tug --status")
	log.Printf("2) Re-initialize token and link: sudo tug --init")
	log.Printf("3) Open generated /connect/<token> URL in dashboard to authorize this host")
	log.Printf("4) Ensure %s contains valid TUG_AGENT_TOKEN", envPath)
}

func NewRuntime(cfg config.Config) (*Runtime, error) {
	queue := newDurableEventQueueV2(cfg.ProtocolV2QueuePath)
	if cfg.ProtocolV2Enabled {
		if err := queue.load(); err != nil {
			log.Printf("cannot load protocol v2 queue: %v", err)
		}
	}
	return &Runtime{
		config:                  cfg,
		fileManager:             NewFileManager(),
		dockerManager:           NewDockerManager(),
		updater:                 NewUpdater(),
		v2Queue:                 queue,
		terminals:               make(map[string]*TerminalSession),
		lastContainerDeltaState: map[string]string{},
		lastAckProgressAt:       time.Now(),
	}, nil
}

func (r *Runtime) debugf(format string, args ...any) {
	if !r.config.Verbose {
		return
	}
	log.Printf("[agent verbose] "+format, args...)
}

func (r *Runtime) Run(ctx context.Context) error {
	if strings.TrimSpace(r.config.AgentToken) == "" || strings.TrimSpace(r.config.ServerID) == "" {
		r.logReconnectRecoveryHint(fmt.Errorf("agent is not initialized (missing or invalid token)"))
		<-ctx.Done()
		return nil
	}
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		log.Printf("connection attempt (failure_streak=%d)", consecutiveFailures)
		connected, err := r.connectAndServe(ctx)
		if err != nil {
			log.Printf("connection closed: %v", err)
			if isNonRetriable(err) {
				r.logReconnectRecoveryHint(err)
				log.Printf("auth/config error; cooling down for 5 minutes before retrying...")
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(5 * time.Minute):
				}
				consecutiveFailures = 0
				continue
			}
		}
		if connected {
			// Reset retry backoff when the socket session was stable.
			consecutiveFailures = 0
			continue
		}
		consecutiveFailures++
		waitDelay := jitteredBackoff(
			r.config.ReconnectBaseDelay,
			r.config.ReconnectMaxDelay,
			consecutiveFailures-1,
			r.config.ReconnectJitterPct,
		)
		log.Printf("reconnect scheduled in %s (failure_streak=%d)", waitDelay, consecutiveFailures)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(waitDelay):
		}
	}
}

func (r *Runtime) connectAndServe(ctx context.Context) (bool, error) {
	const stableConnectionThreshold = 20 * time.Second
	isStableSession := func(startedAt time.Time) bool {
		return time.Since(startedAt) >= stableConnectionThreshold
	}

	if strings.TrimSpace(r.config.ServerID) == "" {
		return false, markNonRetriable(fmt.Errorf("server_id cannot be derived from token; run `tug --init`"))
	}
	if strings.TrimSpace(r.config.AgentToken) == "" {
		return false, markNonRetriable(fmt.Errorf("agent token is missing; run `tug --init`"))
	}
	url := fmt.Sprintf("%s?workspace_id=%s&server_id=%s&token=%s",
		r.config.APIWebSocketURL,
		"",
		r.config.ServerID,
		r.config.AgentToken,
	)
	r.debugf("dial websocket: %s", url)

	conn, response, err := websocket.DefaultDialer.DialContext(ctx, url, http.Header{})
	if err != nil {
		if response != nil && (response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			return false, markNonRetriable(fmt.Errorf("websocket authorization rejected (status %d); run `tug --init`", response.StatusCode))
		}
		return false, fmt.Errorf("websocket dial failed: %w", err)
	}
	defer conn.Close()
	r.debugf("websocket connected")
	sessionStartedAt := time.Now()

	if err := r.sendHandshake(conn, true); err != nil {
		return isStableSession(sessionStartedAt), err
	}
	r.debugf("initial handshake sent")
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatInterval := r.config.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = 30 * time.Second
	}
	selfHealInterval := r.config.SelfHealInterval
	if selfHealInterval <= 0 {
		selfHealInterval = 15 * time.Minute
	}
	go r.periodicHeartbeat(sessionCtx, conn, heartbeatInterval)
	go r.periodicSelfHealSnapshot(sessionCtx, conn, selfHealInterval)
	go r.periodicContainerStatusRefresh(sessionCtx)
	if r.config.ProtocolV2Enabled {
		r.ackStateMu.Lock()
		r.lastAckSeq = r.v2Queue.ackUptoSeq()
		r.lastAckProgressAt = time.Now()
		r.ackStateMu.Unlock()
		go r.flushV2Queue(sessionCtx, conn)
		go r.periodicQueueSelfHeal(sessionCtx, conn)
	}

	for {
		_, message, readErr := conn.ReadMessage()
		if readErr != nil {
			return isStableSession(sessionStartedAt), readErr
		}
		var meta struct {
			Type  string `json:"type"`
			Error string `json:"error"`
		}
		if metaErr := json.Unmarshal(message, &meta); metaErr == nil && strings.EqualFold(strings.TrimSpace(meta.Type), "auth_error") {
			details := strings.TrimSpace(meta.Error)
			if details == "" {
				details = "unauthorized agent connection"
			}
			return false, markNonRetriable(fmt.Errorf("websocket authorization rejected: %s; run `tug --init`", details))
		}
		if r.config.ProtocolV2Enabled {
			var ack inboundAckV2
			if ackErr := json.Unmarshal(message, &ack); ackErr == nil && ack.isAck() {
				if persistErr := r.v2Queue.acknowledge(ack.AckUptoSeq); persistErr != nil {
					r.debugf("protocol v2 ack persist error: %v", persistErr)
				}
				r.ackStateMu.Lock()
				if ack.AckUptoSeq > r.lastAckSeq {
					r.lastAckSeq = ack.AckUptoSeq
					r.lastAckProgressAt = time.Now()
				}
				r.ackStateMu.Unlock()
				r.debugf(
					"protocol v2 ack received: stream=%s ack_upto=%d accepted=%t reason=%s pending=%d",
					ack.StreamID,
					ack.AckUptoSeq,
					ack.Accepted,
					strings.TrimSpace(ack.Reason),
					r.v2Queue.pendingCount(),
				)
				continue
			}
		}
		var command inboundCommand
		if err := json.Unmarshal(message, &command); err != nil {
			log.Printf("invalid command payload: %v", err)
			continue
		}
		r.debugf("received command: type=%s", command.Type)
		var payload json.RawMessage
		logs, err := r.executeCommand(ctx, conn, command, &payload)
		if command.CommandID != "" {
			result := outboundCommandResult{
				Type:      "command_result",
				CommandID: command.CommandID,
				Success:   err == nil,
				Logs:      logs,
				Payload:   payload,
			}
			if err != nil {
				result.Error = err.Error()
			}
			if writeErr := r.writeJSON(conn, result); writeErr != nil {
				log.Printf("cannot send command result: %v", writeErr)
			}
		}
		if err != nil {
			log.Printf("command %s failed: %v", command.Type, err)
		}
	}
}

func (r *Runtime) periodicQueueSelfHeal(ctx context.Context, conn *websocket.Conn) {
	const (
		stallThreshold = 45 * time.Second
		cooldown       = 30 * time.Second
		minPending     = 8
	)
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pending := r.v2Queue.pendingCount()
			if pending < minPending {
				continue
			}
			r.ackStateMu.Lock()
			lastAckSeq := r.lastAckSeq
			lastAckAt := r.lastAckProgressAt
			lastResetAt := r.lastQueueResetAt
			r.ackStateMu.Unlock()
			now := time.Now()
			if now.Sub(lastAckAt) < stallThreshold {
				continue
			}
			if !lastResetAt.IsZero() && now.Sub(lastResetAt) < cooldown {
				continue
			}
			dropped, resetErr := r.v2Queue.resetPendingForRecovery()
			if resetErr != nil {
				r.debugf("protocol v2 self-heal reset failed: %v", resetErr)
				continue
			}
			if dropped == 0 {
				continue
			}
			r.containerDeltaStateMu.Lock()
			r.lastContainerDeltaState = map[string]string{}
			r.containerDeltaStateMu.Unlock()
			r.ackStateMu.Lock()
			r.lastQueueResetAt = now
			r.lastAckProgressAt = now
			r.ackStateMu.Unlock()
			r.debugf(
				"protocol v2 self-heal reset applied: dropped=%d last_ack_seq=%d pending_before=%d",
				dropped,
				lastAckSeq,
				pending,
			)
			if err := r.sendHandshake(conn, true); err != nil {
				r.debugf("protocol v2 self-heal handshake failed: %v", err)
			}
			_ = conn.Close()
			return
		}
	}
}

func (r *Runtime) periodicHeartbeat(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.sendHeartbeat(conn); err != nil {
				r.debugf("periodic heartbeat failed: %v", err)
				_ = conn.Close()
				return
			}
			r.debugf("periodic heartbeat sent")
		}
	}
}

func (r *Runtime) periodicSelfHealSnapshot(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.sendHandshake(conn, true); err != nil {
				r.debugf("periodic self-heal snapshot failed: %v", err)
				_ = conn.Close()
				return
			}
			r.debugf("periodic self-heal snapshot sent")
		}
	}
}

func (r *Runtime) periodicContainerStatusRefresh(ctx context.Context) {
	interval := 30 * time.Second
	if r.config.HeartbeatInterval > 0 && r.config.HeartbeatInterval < interval {
		interval = r.config.HeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.enqueueAllRunningContainerDeltas(ctx)
		}
	}
}

func (r *Runtime) sendHandshake(conn *websocket.Conn, enqueueV2Snapshot bool) error {
	hello, err := r.buildHandshake()
	if err != nil {
		return err
	}

	if err := r.writeJSON(conn, hello); err != nil {
		return fmt.Errorf("cannot write handshake: %w", err)
	}
	if r.config.ProtocolV2Enabled && enqueueV2Snapshot {
		pending := r.v2Queue.pendingCount()
		if pending > 24 {
			r.debugf("protocol v2 snapshot enqueue skipped: pending=%d", pending)
		} else {
			if err := r.enqueueV2SnapshotFromHandshake(hello); err != nil {
				r.debugf("protocol v2 snapshot enqueue failed: %v", err)
			}
		}
	}
	r.debugf("handshake payload sent: containers=%d", len(hello.Containers))
	return nil
}

func (r *Runtime) enqueueV2SnapshotFromHandshake(hello Handshake) error {
	if r.v2Queue == nil {
		return nil
	}
	rawPayload, err := json.Marshal(hello)
	if err != nil {
		return err
	}
	env := newOutboundEnvelopeV2()
	env.MessageID = fmt.Sprintf("snapshot-%d", time.Now().UnixNano())
	env.ServerID = r.config.ServerID
	env.WorkspaceID = strings.TrimSpace(hello.WorkspaceID)
	env.Entity = entityRuntime
	env.Action = actionSnapshot
	env.Payload = json.RawMessage(rawPayload)
	item, err := r.v2Queue.enqueue(env)
	if err != nil {
		return err
	}
	r.debugf(
		"protocol v2 enqueue: seq=%d entity=%s action=%s pending=%d",
		item.Envelope.Seq,
		item.Envelope.Entity,
		item.Envelope.Action,
		r.v2Queue.pendingCount(),
	)
	return nil
}

func (r *Runtime) flushV2Queue(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(1200 * time.Millisecond)
	defer ticker.Stop()
	r.debugf("protocol v2 flush loop started (%s)", r.v2Queue.debugSnapshot())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			due := r.v2Queue.dueItems(8)
			if len(due) == 0 {
				continue
			}
			for _, item := range due {
				if err := r.writeJSON(conn, item.Envelope); err != nil {
					_ = r.v2Queue.markAttempt(item.Envelope.Seq, err)
					r.debugf(
						"protocol v2 send failed: seq=%d retry=%d err=%v",
						item.Envelope.Seq,
						item.RetryCount+1,
						err,
					)
					// This socket is likely stale/backpressured. Close it to force
					// full reconnect and clean read/write loops.
					_ = conn.Close()
					break
				}
			_ = r.v2Queue.markAttempt(item.Envelope.Seq, nil)
			if item.RetryCount == 0 || item.RetryCount%20 == 0 {
				r.debugf(
					"protocol v2 sent: seq=%d retry=%d pending=%d",
					item.Envelope.Seq,
					item.RetryCount+1,
					r.v2Queue.pendingCount(),
				)
			}
			}
		}
	}
}

func (r *Runtime) enqueueV2ContainerStatusDelta(ctx context.Context, containerID string) {
	if !r.config.ProtocolV2Enabled || r.v2Queue == nil || strings.TrimSpace(containerID) == "" {
		return
	}
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	containers, err := r.dockerManager.ListContainers(listCtx)
	if err != nil {
		r.debugf("protocol v2 container delta list error: %v", err)
		return
	}
	for _, item := range containers {
		if strings.TrimSpace(item.ID) != strings.TrimSpace(containerID) {
			continue
		}
		r.enqueueV2ContainerDeltaItem(item)
		return
	}
}

func (r *Runtime) enqueueAllRunningContainerDeltas(ctx context.Context) {
	if !r.config.ProtocolV2Enabled || r.v2Queue == nil {
		return
	}
	if r.v2Queue.pendingCount() > 32 {
		r.debugf("protocol v2 container refresh skipped: pending queue is high")
		return
	}
	listCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	containers, err := r.dockerManager.ListContainersLite(listCtx)
	if err != nil {
		r.debugf("protocol v2 container delta list error: %v", err)
		return
	}
	activeIDs := make(map[string]struct{}, len(containers))
	for _, item := range containers {
		activeIDs[strings.TrimSpace(item.ID)] = struct{}{}
		r.enqueueV2ContainerDeltaItem(item)
	}
	r.containerDeltaStateMu.Lock()
	for containerID := range r.lastContainerDeltaState {
		if _, exists := activeIDs[containerID]; !exists {
			delete(r.lastContainerDeltaState, containerID)
		}
	}
	r.containerDeltaStateMu.Unlock()
}

func (r *Runtime) enqueueV2ContainerDeltaItem(item HandshakeContainer) {
	containerID := strings.TrimSpace(item.ID)
	if containerID == "" {
		return
	}
	stateKey := strings.Join([]string{
		strings.TrimSpace(item.Status),
		strings.TrimSpace(item.Name),
		strings.TrimSpace(item.Image),
		strings.TrimSpace(item.Ports),
		strings.TrimSpace(item.App),
	}, "|")
	r.containerDeltaStateMu.Lock()
	if previous, exists := r.lastContainerDeltaState[containerID]; exists && previous == stateKey {
		r.containerDeltaStateMu.Unlock()
		return
	}
	r.lastContainerDeltaState[containerID] = stateKey
	r.containerDeltaStateMu.Unlock()

	rawPayload, err := json.Marshal(map[string]any{
		"id":     containerID,
		"name":   strings.TrimSpace(item.Name),
		"status": strings.TrimSpace(item.Status),
		"image":  strings.TrimSpace(item.Image),
		"ports":  strings.TrimSpace(item.Ports),
		"app":    strings.TrimSpace(item.App),
	})
	if err != nil {
		return
	}
	env := newOutboundEnvelopeV2()
	env.MessageID = fmt.Sprintf("container-%s-%d", containerID, time.Now().UnixNano())
	env.ServerID = r.config.ServerID
	env.Entity = entityContainer
	env.Action = actionStatusChanged
	env.Payload = json.RawMessage(rawPayload)
	if _, err := r.v2Queue.enqueue(env); err != nil {
		r.debugf("protocol v2 container delta enqueue failed: %v", err)
		return
	}
}

func (r *Runtime) writeJSON(conn *websocket.Conn, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, raw)
	conn.SetWriteDeadline(time.Time{})
	return err
}

type inboundCommand struct {
	Type                 string              `json:"type"`
	CommandID            string              `json:"command_id"`
	WorkspaceID          string              `json:"workspace_id"`
	ServerID             string              `json:"server_id"`
	BinaryURL            string              `json:"binary_url"`
	Image                string              `json:"image"`
	CleanDockerResources bool                `json:"clean_docker_resources"`
	ContainerID          string              `json:"container_id"`
	TargetContainerName  string              `json:"target_container_name"`
	TargetContainerID    string              `json:"target_container_id"`
	TargetPort           int                 `json:"target_port"`
	Action               string              `json:"action"`
	RemoveVolumes        bool                `json:"remove_volumes"`
	RemoveImage          bool                `json:"remove_image"`
	Domain               string              `json:"domain"`
	NetworkName          string              `json:"network_name"`
	Content              string              `json:"content"`
	Summary              string              `json:"summary"`
	ConfigID             string              `json:"config_id"`
	ProjectID            string              `json:"project_id"`
	ComposeContent       string              `json:"compose_content,omitempty"`
	RepoURL              string              `json:"repo_url"`
	Branch               string              `json:"branch"`
	FilePath             string              `json:"file_path"`
	FileType             string              `json:"file_type"`
	Command              string              `json:"command"`
	TerminalID           string              `json:"terminal_id"`
	Rows                 uint16              `json:"rows"`
	Cols                 uint16              `json:"cols"`
	Payload              string              `json:"payload"`
	Tail                 int                 `json:"tail"`
	Schedules            []agentCronSchedule `json:"schedules"`
}

type outboundCommandResult struct {
	Type      string          `json:"type"`
	CommandID string          `json:"command_id"`
	Success   bool            `json:"success"`
	Error     string          `json:"error,omitempty"`
	Logs      []string        `json:"logs,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type outboundHeartbeat struct {
	Type         string `json:"type"`
	ServerID     string `json:"server_id"`
	WorkspaceID  string `json:"workspace_id,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	SentAtUnix   int64  `json:"sent_at_unix"`
}

type agentCronSchedule struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ServerID    string `json:"server_id"`
	Name        string `json:"name"`
	Command     string `json:"command"`
	Description string `json:"description"`
	Preset      string `json:"preset"`
	Expression  string `json:"expression"`
	Timezone    string `json:"timezone"`
	Enabled     bool   `json:"enabled"`
	Source      string `json:"source"`
	UpdatedAt   string `json:"updated_at"`
}

type outboundCronSchedulesSnapshot struct {
	Type      string              `json:"type"`
	Workspace string              `json:"workspace_id"`
	ServerID  string              `json:"server_id"`
	Schedules []agentCronSchedule `json:"schedules"`
}

func (r *Runtime) sendHeartbeat(conn *websocket.Conn) error {
	heartbeat := outboundHeartbeat{
		Type:         "heartbeat",
		ServerID:     strings.TrimSpace(r.config.ServerID),
		WorkspaceID:  strings.TrimSpace(r.config.WorkspaceID),
		AgentVersion: strings.TrimSpace(r.config.AgentVersion),
		SentAtUnix:   time.Now().Unix(),
	}
	if err := r.writeJSON(conn, heartbeat); err != nil {
		return fmt.Errorf("cannot write heartbeat: %w", err)
	}
	return nil
}

func jitteredBackoff(base, max time.Duration, step int, jitterPct int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if max < base {
		max = base
	}
	if step < 0 {
		step = 0
	}
	delay := base
	for i := 0; i < step; i++ {
		if delay >= max {
			delay = max
			break
		}
		delay = delay * 2
		if delay > max {
			delay = max
		}
	}
	if jitterPct <= 0 {
		return delay
	}
	jitterMax := delay * time.Duration(jitterPct) / 100
	if jitterMax <= 0 {
		return delay
	}
	// Randomize in range [-jitterMax, +jitterMax].
	rangeSize := jitterMax*2 + 1
	n, err := rand.Int(rand.Reader, big.NewInt(int64(rangeSize)))
	if err != nil {
		return delay
	}
	jitter := time.Duration(n.Int64()) - jitterMax
	adjusted := delay + jitter
	if adjusted < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	if adjusted > max {
		return max
	}
	return adjusted
}

func (r *Runtime) executeCommand(
	ctx context.Context,
	conn *websocket.Conn,
	command inboundCommand,
	payloadOut *json.RawMessage,
) ([]string, error) {
	switch command.Type {
	case "terminal_start", "terminal_input", "terminal_resize":
		return nil, r.handleTerminalCommand(ctx, conn, command)
	case "git_deploy":
		return r.handleGitDeploy(ctx, command)
	case "fs_list":
		entries, err := r.fileManager.List(command.FilePath)
		if err != nil {
			return nil, err
		}
		type fInfo struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
			Size  int64  `json:"size"`
		}
		var list []fInfo
		for _, e := range entries {
			info, _ := e.Info()
			sz := int64(0)
			if info != nil {
				sz = info.Size()
			}
			list = append(list, fInfo{Name: e.Name(), IsDir: e.IsDir(), Size: sz})
		}
		if raw, err := json.Marshal(list); err == nil {
			*payloadOut = json.RawMessage(raw)
		}
		return []string{"Listed " + command.FilePath}, nil
	case "fs_read":
		data, err := r.fileManager.Read(command.FilePath)
		if err != nil {
			return nil, err
		}
		*payloadOut = json.RawMessage(fmt.Sprintf("%q", string(data)))
		return []string{"Read " + command.FilePath}, nil
	case "fs_write":
		if err := r.fileManager.Write(command.FilePath, []byte(command.Content), 0o640); err != nil {
			return nil, err
		}
		return []string{"Written to " + command.FilePath}, nil
	case "fs_delete":
		if err := r.fileManager.Delete(command.FilePath); err != nil {
			return nil, err
		}
		return []string{"Deleted " + command.FilePath}, nil
	case "self_update":
		return nil, r.updater.SafeUpdate(ctx, command.BinaryURL)
	case "disconnect":
		if err := RunDetachedUninstall(command.CleanDockerResources); err != nil {
			return nil, err
		}
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
		return []string{"Disconnect acknowledged. Agent process shutting down."}, nil
	case "container_action":
		if command.ContainerID == "" {
			return nil, fmt.Errorf("container_id is required")
		}
		actionCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := r.dockerManager.ControlContainer(actionCtx, command.ContainerID, command.Action, command.RemoveVolumes, command.RemoveImage); err != nil {
			return nil, err
		}
		if err := r.sendHandshake(conn, false); err != nil {
			return nil, err
		}
		r.enqueueV2ContainerStatusDelta(ctx, command.ContainerID)
		return []string{fmt.Sprintf("Container %s %s succeeded.", command.ContainerID, command.Action)}, nil
	case "server_action":
		switch command.Action {
		case "restart_docker":
			restartCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			logs, err := r.dockerManager.RestartDockerDaemon(restartCtx)
			if err != nil {
				return logs, err
			}
			return logs, nil
		case "reset_server":
			resetCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			logs, err := r.dockerManager.ScheduleServerReset(resetCtx)
			if err != nil {
				return logs, err
			}
			return logs, nil
		default:
			return nil, fmt.Errorf("unsupported server action %s", command.Action)
		}
	case "network_create":
		if strings.TrimSpace(command.NetworkName) == "" {
			return nil, fmt.Errorf("network_name is required")
		}
		createCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		logs, err := r.dockerManager.CreateNetwork(createCtx, strings.TrimSpace(command.NetworkName))
		if err != nil {
			return logs, err
		}
		if err := r.sendHandshake(conn, false); err != nil {
			return logs, err
		}
		return logs, nil
	case "network_delete":
		if strings.TrimSpace(command.NetworkName) == "" {
			return nil, fmt.Errorf("network_name is required")
		}
		deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		logs, err := r.dockerManager.DeleteNetwork(deleteCtx, strings.TrimSpace(command.NetworkName))
		if err != nil {
			return logs, err
		}
		if err := r.sendHandshake(conn, false); err != nil {
			return logs, err
		}
		return logs, nil
	case "save_compose":
		if command.ProjectID == "" {
			return nil, fmt.Errorf("project_id is required")
		}
		if strings.TrimSpace(command.Content) == "" {
			return nil, fmt.Errorf("compose content is required")
		}
		relativePath := filepath.Join("projects", command.ProjectID, "docker-compose.yml")
		if err := r.fileManager.Write(relativePath, []byte(command.Content), 0o640); err != nil {
			return nil, err
		}
		return []string{"Compose file saved."}, nil
	case "cron_schedules_apply":
		normalized := make([]agentCronSchedule, 0, len(command.Schedules))
		for _, item := range command.Schedules {
			item.WorkspaceID = strings.TrimSpace(command.WorkspaceID)
			item.ServerID = strings.TrimSpace(command.ServerID)
			item.Source = "dashboard"
			normalized = append(normalized, item)
		}
		raw, marshalErr := json.Marshal(normalized)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if err := r.fileManager.Write("schedules/schedules.json", raw, 0o640); err != nil {
			return nil, err
		}
		return []string{fmt.Sprintf("Saved %d schedule(s).", len(normalized))}, nil
	case "cron_schedules_pull":
		schedules := make([]agentCronSchedule, 0)
		raw, readErr := r.fileManager.Read("schedules/schedules.json")
		if readErr == nil {
			_ = json.Unmarshal(raw, &schedules)
		}
		serverID := strings.TrimSpace(command.ServerID)
		if serverID == "" {
			serverID = strings.TrimSpace(r.config.ServerID)
		}
		workspaceID := strings.TrimSpace(command.WorkspaceID)
		if workspaceID == "" {
			workspaceID = strings.TrimSpace(r.config.WorkspaceID)
		}
		snapshot := outboundCronSchedulesSnapshot{
			Type:      "cron_schedules_snapshot_v1",
			Workspace: workspaceID,
			ServerID:  serverID,
			Schedules: schedules,
		}
		if err := r.writeJSON(conn, snapshot); err != nil {
			return nil, err
		}
		return []string{fmt.Sprintf("Pulled %d schedule(s).", len(schedules))}, nil
	case "container_logs_tail":
		if strings.TrimSpace(command.ContainerID) == "" {
			return nil, fmt.Errorf("container_id is required")
		}
		tail := command.Tail
		if tail <= 0 {
			tail = 200
		}
		if tail > 2000 {
			tail = 2000
		}
		logs, err := r.dockerManager.GetLogsPreview(ctx, strings.TrimSpace(command.ContainerID), tail)
		if err != nil {
			return nil, err
		}
		return logs, nil
	case "containers_snapshot_pull":
		pullCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		containers, err := r.dockerManager.ListContainers(pullCtx)
		if err != nil {
			return nil, err
		}
		type containersSnapshotPayload struct {
			Containers []HandshakeContainer `json:"containers"`
		}
		if raw, marshalErr := json.Marshal(containersSnapshotPayload{Containers: containers}); marshalErr == nil {
			*payloadOut = json.RawMessage(raw)
		}
		return []string{fmt.Sprintf("Containers snapshot size: %d", len(containers))}, nil
	case "deploy":
		if command.ProjectID == "" {
			return nil, fmt.Errorf("project_id is required")
		}
		composePath, err := ResolveSandboxPath(filepath.Join("projects", command.ProjectID, "docker-compose.yml"))
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(command.ComposeContent) != "" {
			if err := os.MkdirAll(filepath.Dir(composePath), 0755); err == nil {
				_ = os.WriteFile(composePath, []byte(command.ComposeContent), 0644)
			}
		}
		deployCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		logs, deployErr := r.dockerManager.DeployCompose(deployCtx, composePath, command.Command)
		if deployErr != nil {
			return logs, deployErr
		}
		if err := r.sendHandshake(conn, false); err != nil {
			return logs, err
		}
		r.enqueueAllRunningContainerDeltas(ctx)
		return logs, nil
	case "install_tug_router":
		routerName, nameErr := r.dockerManager.GenerateTugRouterContainerName()
		if nameErr != nil {
			return nil, nameErr
		}
		deployCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		logs, deployErr := r.dockerManager.InstallTugRouter(
			deployCtx,
			routerName,
			strings.TrimSpace(command.Image),
		)
		if deployErr != nil {
			return logs, deployErr
		}
		if err := r.sendHandshake(conn, false); err != nil {
			return logs, err
		}
		r.enqueueAllRunningContainerDeltas(ctx)
		return logs, nil
	case "configure_tug_router_route":
		if strings.TrimSpace(command.Domain) == "" {
			return nil, fmt.Errorf("domain is required")
		}
		if strings.TrimSpace(command.TargetContainerName) == "" {
			return nil, fmt.Errorf("target_container_name is required")
		}
		if command.TargetPort <= 0 || command.TargetPort > 65535 {
			return nil, fmt.Errorf("target_port must be between 1 and 65535")
		}
		routerName, resolveErr := r.dockerManager.ResolveTugRouterContainerName(ctx)
		if resolveErr != nil {
			return nil, fmt.Errorf("tug-router container is not installed")
		}
		configureCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		logs, configureErr := r.dockerManager.ConfigureTugRouterRoute(
			configureCtx,
			routerName,
			strings.TrimSpace(command.Domain),
			strings.TrimSpace(command.TargetContainerID),
			strings.TrimSpace(command.TargetContainerName),
			command.TargetPort,
		)
		if configureErr != nil {
			return logs, configureErr
		}
		return logs, nil
	case "list_tug_router_routes":
		routerName, resolveErr := r.dockerManager.ResolveTugRouterContainerName(ctx)
		if resolveErr != nil {
			return nil, fmt.Errorf("tug-router container is not installed")
		}
		routes, listErr := r.dockerManager.ListTugRouterRoutes(routerName)
		if listErr != nil {
			return nil, listErr
		}
		type routesPayload struct {
			Routes any `json:"routes"`
		}
		if raw, marshalErr := json.Marshal(routesPayload{Routes: routes}); marshalErr == nil {
			*payloadOut = json.RawMessage(raw)
		}
		return []string{fmt.Sprintf("Loaded %d route(s).", len(routes))}, nil
	case "remove_tug_router_route":
		if strings.TrimSpace(command.Domain) == "" {
			return nil, fmt.Errorf("domain is required")
		}
		routerName, resolveErr := r.dockerManager.ResolveTugRouterContainerName(ctx)
		if resolveErr != nil {
			return nil, fmt.Errorf("tug-router container is not installed")
		}
		removeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		logs, removeErr := r.dockerManager.DeleteTugRouterRoute(
			removeCtx,
			routerName,
			strings.TrimSpace(command.Domain),
		)
		if removeErr != nil {
			return logs, removeErr
		}
		return logs, nil
	case "check_host_path":
		if command.FilePath == "" {
			return nil, fmt.Errorf("file_path is required")
		}
		info, err := os.Stat(command.FilePath)
		if err != nil {
			if os.IsNotExist(err) {
				type pathInfo struct {
					Exists bool `json:"exists"`
					IsDir  bool `json:"is_dir"`
				}
				raw, _ := json.Marshal(pathInfo{Exists: false, IsDir: false})
				*payloadOut = json.RawMessage(raw)
				return []string{"Path does not exist: " + command.FilePath}, nil
			}
			return nil, fmt.Errorf("path access error: %w", err)
		}
		type pathInfo struct {
			Exists bool `json:"exists"`
			IsDir  bool `json:"is_dir"`
		}
		raw, _ := json.Marshal(pathInfo{Exists: true, IsDir: info.IsDir()})
		*payloadOut = json.RawMessage(raw)
		return []string{"Checked path: " + command.FilePath}, nil
	case "get_container_mounts":
		if command.ContainerID == "" {
			return nil, fmt.Errorf("container_id is required")
		}
		out, err := exec.CommandContext(ctx, "docker", "inspect", "--format={{json .Mounts}}", command.ContainerID).Output()
		if err != nil {
			return nil, fmt.Errorf("docker inspect failed: %w", err)
		}
		*payloadOut = json.RawMessage(out)
		return []string{"Fetched container mounts for " + command.ContainerID}, nil
	default:
		return nil, nil
	}
}

func (r *Runtime) buildHandshake() (Handshake, error) {
	publicIP, _ := fetchPublicIP()
	localIP := detectLocalIP()
	dockerVersion, _ := detectDockerVersion()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	containers, _ := r.dockerManager.ListContainers(ctx)
	networks, _ := r.dockerManager.ListNetworks(ctx)
	hostName, _ := os.Hostname()
	totalRAMBytes, err := detectTotalRAMBytes()
	if err != nil {
		return Handshake{}, err
	}

	diskFreeBytes, err := detectDiskFree(GetDataDir())
	if err != nil {
		return Handshake{}, err
	}
	r.debugf(
		"build handshake snapshot: host=%s local_ip=%s public_ip=%s docker=%s containers=%d",
		hostName,
		localIP,
		publicIP,
		dockerVersion,
		len(containers),
	)

	return Handshake{
		Type:          "handshake",
		ServerID:      r.config.ServerID,
		WorkspaceID:   r.config.WorkspaceID,
		HostName:      hostName,
		AgentVersion:  r.config.AgentVersion,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		CPUCores:      runtime.NumCPU(),
		RAMBytes:      totalRAMBytes,
		DiskFreeBytes: diskFreeBytes,
		LocalIP:       localIP,
		PublicIP:      publicIP,
		DockerVersion: dockerVersion,
		Networks:      networks,
		Containers:    containers,
	}, nil
}

func detectDockerVersion() (string, error) {
	cmd := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func detectLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		return ipNet.IP.String()
	}
	return ""
}

func fetchPublicIP() (string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body []byte
	body, err = ioReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}
