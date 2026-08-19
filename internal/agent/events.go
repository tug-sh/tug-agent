package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/services/agent/internal/protocol"
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

// newEvent builds an envelope already stamped with this agent's identity, so
// every producer emits the same routing fields.
func (runtime *Runtime) newEvent(
	entity protocol.Entity,
	action protocol.Action,
	messageID string,
) protocol.Envelope {
	envelope := protocol.NewEnvelope()
	envelope.MessageID = messageID
	envelope.ServerID = runtime.config.ServerID
	envelope.Entity = entity
	envelope.Action = action
	return envelope
}

// consumeAck applies a server acknowledgement to the durable queue. It reports
// whether the frame was an ack, so the read loop can skip further decoding.
func (runtime *Runtime) consumeAck(message []byte) bool {
	if !runtime.config.ProtocolV2Enabled {
		return false
	}
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

func (runtime *Runtime) enqueueSnapshotEvent(hello protocol.Handshake) {
	if runtime.eventQueue == nil {
		return
	}
	pending := runtime.eventQueue.PendingCount()
	if pending > snapshotBacklogLimit || runtime.eventQueue.HasPendingSnapshot() {
		runtime.log.Debug("snapshot enqueue skipped: pending=%d", pending)
		return
	}
	rawPayload, err := json.Marshal(hello)
	if err != nil {
		runtime.log.Debug("snapshot enqueue failed: %v", err)
		return
	}
	envelope := runtime.newEvent(protocol.EntityRuntime, protocol.ActionSnapshot, fmt.Sprintf("snapshot-%d", time.Now().UnixNano()))
	envelope.WorkspaceID = strings.TrimSpace(hello.WorkspaceID)
	envelope.Payload = rawPayload
	item, err := runtime.eventQueue.Enqueue(envelope)
	if err != nil {
		runtime.log.Debug("snapshot enqueue failed: %v", err)
		return
	}
	runtime.log.Debug(
		"event enqueued: seq=%d entity=%s action=%s pending=%d",
		item.Envelope.Seq,
		item.Envelope.Entity,
		item.Envelope.Action,
		runtime.eventQueue.PendingCount(),
	)
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
		if runtime.eventQueue.HasPendingSnapshot() {
			return true
		}
		if err := runtime.sendHandshake(conn, true); err != nil {
			return runtime.endSession(conn, "event queue self-heal handshake failed", err)
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

func (runtime *Runtime) enqueueContainerStatusDelta(ctx context.Context, containerID string) {
	if !runtime.config.ProtocolV2Enabled || runtime.eventQueue == nil || strings.TrimSpace(containerID) == "" {
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
		runtime.enqueueContainerDelta(item)
		return
	}
}

func (runtime *Runtime) enqueueAllRunningContainerDeltas(ctx context.Context) {
	if !runtime.config.ProtocolV2Enabled || runtime.eventQueue == nil {
		return
	}
	if runtime.eventQueue.PendingCount() > containerRefreshBacklogLimit {
		runtime.log.Debug("container refresh skipped: pending queue is high")
		return
	}
	listCtx, cancel := context.WithTimeout(ctx, containerRefreshBudget)
	defer cancel()
	containers, err := runtime.dockerManager.ListContainersLite(listCtx)
	if err != nil {
		runtime.log.Debug("container delta list error: %v", err)
		return
	}
	activeIDs := make(map[string]struct{}, len(containers))
	for _, item := range containers {
		activeIDs[strings.TrimSpace(item.ID)] = struct{}{}
		runtime.enqueueContainerDelta(item)
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

// enqueueContainerDelta emits a container change, skipping containers whose
// observable state is unchanged since the last delta.
func (runtime *Runtime) enqueueContainerDelta(item protocol.HandshakeContainer) {
	containerID := strings.TrimSpace(item.ID)
	if containerID == "" {
		return
	}
	if !runtime.containerStateChanged(containerID, item) {
		return
	}

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
	envelope := runtime.newEvent(
		protocol.EntityContainer,
		protocol.ActionStatusChanged,
		fmt.Sprintf("container-%s-%d", containerID, time.Now().UnixNano()),
	)
	envelope.Class = protocol.ClassSignal
	envelope.Payload = rawPayload
	if _, err := runtime.eventQueue.EnqueueCoalesced(envelope, "container:"+containerID); err != nil {
		runtime.log.Debug("container delta enqueue failed: %v", err)
	}
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
