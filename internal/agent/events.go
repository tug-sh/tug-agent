package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/pkg/protocol"
)

const (
	eventFlushInterval     = 1200 * time.Millisecond
	eventFlushBatchSize    = 8
	ackStallThreshold      = 45 * time.Second
	ackStallCheckInterval  = 45 * time.Second
	queueResetCooldown     = 2 * time.Minute
	queueResetMinPending   = 8
	containerListTimeout   = 10 * time.Second
	containerRefreshBudget = 12 * time.Second
	// Container refreshes are dropped above this backlog, because the API is
	// clearly not keeping up and deltas would only pile on.
	containerRefreshBacklogLimit = 8
)

// consumeAck applies a server acknowledgement to the durable queue. It reports
// whether the frame was an ack, so the read loop can skip further decoding.
func (runtime *Runtime) consumeAck(message []byte) bool {
	var ack protocol.Ack
	if err := json.Unmarshal(message, &ack); err != nil || !ack.IsAck() {
		return false
	}
	if persistErr := runtime.eventQueue.Acknowledge(ack.AckUptoSeq); persistErr != nil {
		runtime.log.Debug("event ack persist error: %v", persistErr)
	}
	runtime.ackStateMu.Lock()
	if ack.AckUptoSeq > runtime.lastAckSeq {
		runtime.lastAckSeq = ack.AckUptoSeq
		runtime.lastAckProgressAt = time.Now()
	}
	runtime.ackStateMu.Unlock()
	runtime.log.Debug(
		"event ack received: stream=%s ack_upto=%d accepted=%t reason=%s pending=%d",
		ack.StreamID,
		ack.AckUptoSeq,
		ack.Accepted,
		strings.TrimSpace(ack.Reason),
		runtime.eventQueue.PendingCount(),
	)
	return true
}

// flushEventQueue drains due events onto the socket. A write failure means the
// socket is stale or backpressured, so it is closed to force a clean reconnect.
func (runtime *Runtime) flushEventQueue(ctx context.Context, conn *websocket.Conn) {
	runtime.log.Debug("event flush loop started (%s)", runtime.eventQueue.DebugSnapshot())
	runEvery(ctx, eventFlushInterval, func() bool {
		for _, item := range runtime.eventQueue.DueItems(eventFlushBatchSize) {
			if err := runtime.writeJSON(conn, item.Envelope); err != nil {
				_ = runtime.eventQueue.MarkAttempt(item.Envelope.Seq, err)
				return runtime.endSession(conn, fmt.Sprintf("event send failed: seq=%d", item.Envelope.Seq), err)
			}
			_ = runtime.eventQueue.MarkAttempt(item.Envelope.Seq, nil)
			if item.RetryCount == 0 || item.RetryCount%20 == 0 {
				runtime.log.Debug(
					"event sent: seq=%d retry=%d pending=%d",
					item.Envelope.Seq,
					item.RetryCount+1,
					runtime.eventQueue.PendingCount(),
				)
			}
		}
		return true
	})
}

// periodicQueueSelfHeal recovers from a server that stopped acknowledging: it
// drops disposable signals, keeps facts and re-sends a snapshot so the
// dashboard converges without a full reconnect.
func (runtime *Runtime) periodicQueueSelfHeal(ctx context.Context, conn *websocket.Conn) {
	runEvery(ctx, ackStallCheckInterval, func() bool {
		if !runtime.ackProgressStalled() {
			return true
		}
		dropped, resetErr := runtime.eventQueue.ResetPendingForRecovery()
		if resetErr != nil {
			runtime.log.Debug("event queue self-heal reset failed: %v", resetErr)
			return true
		}
		now := time.Now()
		runtime.ackStateMu.Lock()
		runtime.lastQueueResetAt = now
		runtime.lastAckProgressAt = now
		runtime.ackStateMu.Unlock()
		runtime.log.Debug(
			"event queue self-heal applied: dropped=%d pending=%d",
			dropped,
			runtime.eventQueue.PendingCount(),
		)
		if err := runtime.sendSnapshot(); err != nil {
			return runtime.endSession(conn, "event queue self-heal snapshot failed", err)
		}
		return true
	})
}

// ackProgressStalled reports whether the queue is backed up, no ack has
// advanced recently and the previous reset is old enough to try again.
func (runtime *Runtime) ackProgressStalled() bool {
	if runtime.eventQueue.PendingCount() < queueResetMinPending {
		return false
	}
	runtime.ackStateMu.Lock()
	lastAckAt := runtime.lastAckProgressAt
	lastResetAt := runtime.lastQueueResetAt
	runtime.ackStateMu.Unlock()

	now := time.Now()
	if now.Sub(lastAckAt) < ackStallThreshold {
		return false
	}
	return lastResetAt.IsZero() || now.Sub(lastResetAt) >= queueResetCooldown
}

func (runtime *Runtime) publishContainerStatus(ctx context.Context, conn *websocket.Conn, containerID string) {
	if strings.TrimSpace(containerID) == "" {
		return
	}
	listCtx, cancel := context.WithTimeout(ctx, containerListTimeout)
	defer cancel()
	containers, err := runtime.dockerManager.ListContainers(listCtx)
	if err != nil {
		runtime.log.Debug("container delta list error: %v", err)
		return
	}
	for _, item := range containers {
		if strings.TrimSpace(item.ID) != strings.TrimSpace(containerID) {
			continue
		}
		runtime.publishContainerDelta(conn, item)
		return
	}
}

func (runtime *Runtime) publishAllContainerStatuses(ctx context.Context, conn *websocket.Conn) {
	listCtx, cancel := context.WithTimeout(ctx, containerRefreshBudget)
	defer cancel()
	containers, err := runtime.dockerManager.ListContainersLite(listCtx)
	if err != nil {
		runtime.log.Debug("container delta list error: %v", err)
		return
	}

	hasNew := false
	runtime.containerDeltaStateMu.Lock()
	for _, item := range containers {
		id := strings.TrimSpace(item.ID)
		if _, exists := runtime.lastContainerDeltaState[id]; !exists {
			hasNew = true
			break
		}
	}
	runtime.containerDeltaStateMu.Unlock()

	if hasNew {
		_ = runtime.sendSnapshot()
		return
	}

	activeIDs := make(map[string]struct{}, len(containers))
	for _, item := range containers {
		activeIDs[strings.TrimSpace(item.ID)] = struct{}{}
		runtime.publishContainerDelta(conn, item)
	}
	runtime.forgetMissingContainers(activeIDs)
}

// forgetMissingContainers drops delta state for containers that no longer
// exist, so a recreated container with the same id emits a fresh delta.
func (runtime *Runtime) forgetMissingContainers(activeIDs map[string]struct{}) {
	runtime.containerDeltaStateMu.Lock()
	defer runtime.containerDeltaStateMu.Unlock()
	for containerID := range runtime.lastContainerDeltaState {
		if _, exists := activeIDs[containerID]; !exists {
			delete(runtime.lastContainerDeltaState, containerID)
		}
	}
}

// publishContainerDelta reports a container change, skipping containers whose
// observable state is unchanged since the last one.
//
// A delta is a signal. It is never queued, because the periodic refresh below
// re-reports every container within half a minute and a full snapshot repairs
// anything that slipped through: resending a stale status would be worse than
// dropping it. The old agent queued these and then spent its recovery logic
// throwing them back out again.
func (runtime *Runtime) publishContainerDelta(conn *websocket.Conn, item protocol.HandshakeContainer) {
	containerID := strings.TrimSpace(item.ID)
	if containerID == "" || !runtime.containerStateChanged(containerID, item) {
		return
	}

	delta := protocol.ContainerDelta{
		ID:     containerID,
		Name:   strings.TrimSpace(item.Name),
		Status: strings.TrimSpace(item.Status),
		Image:  strings.TrimSpace(item.Image),
		Ports:  strings.TrimSpace(item.Ports),
		App:    strings.TrimSpace(item.App),
	}
	if err := runtime.emitSignal(conn, protocol.EntityContainer, protocol.ActionStatusChanged, delta); err != nil {
		runtime.log.Debug("cannot send a container delta: %v", err)
		// The state cache is rolled back so the next pass tries again rather
		// than deciding nothing has changed.
		runtime.forgetContainerState(containerID)
	}
}

func (runtime *Runtime) forgetContainerState(containerID string) {
	runtime.containerDeltaStateMu.Lock()
	defer runtime.containerDeltaStateMu.Unlock()
	delete(runtime.lastContainerDeltaState, containerID)
}

func (runtime *Runtime) containerStateChanged(containerID string, item protocol.HandshakeContainer) bool {
	stateKey := strings.Join([]string{
		strings.TrimSpace(item.Status),
		strings.TrimSpace(item.Name),
		strings.TrimSpace(item.Image),
		strings.TrimSpace(item.Ports),
		strings.TrimSpace(item.App),
	}, "|")

	runtime.containerDeltaStateMu.Lock()
	defer runtime.containerDeltaStateMu.Unlock()
	if previous, exists := runtime.lastContainerDeltaState[containerID]; exists && previous == stateKey {
		return false
	}
	runtime.lastContainerDeltaState[containerID] = stateKey
	return true
}
