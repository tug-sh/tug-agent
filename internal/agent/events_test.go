package agent

import (
	"encoding/json"
	"testing"
	"time"

	"tug.sh/services/agent/internal/protocol"
)

func TestConsumeAckAdvancesQueue(t *testing.T) {
	runtime := newTestRuntime(t)
	for index := 0; index < 3; index++ {
		if _, err := runtime.eventQueue.Enqueue(protocol.NewEnvelope()); err != nil {
			t.Fatalf("enqueue failed: %v", err)
		}
	}

	ack := protocol.Ack{Type: "ack", ProtocolVersion: protocol.Version, AckUptoSeq: 2, Accepted: true}
	raw, _ := json.Marshal(ack)
	if !runtime.consumeAck(raw) {
		t.Fatal("expected the ack to be consumed")
	}
	if runtime.eventQueue.PendingCount() != 1 {
		t.Fatalf("pending = %d, want 1", runtime.eventQueue.PendingCount())
	}
	if runtime.lastAckSeq != 2 {
		t.Fatalf("lastAckSeq = %d, want 2", runtime.lastAckSeq)
	}
}

func TestConsumeAckIgnoresOtherFrames(t *testing.T) {
	runtime := newTestRuntime(t)

	if runtime.consumeAck([]byte(`{"type":"command_result"}`)) {
		t.Error("a command result must not be treated as an ack")
	}
	if runtime.consumeAck([]byte("not json")) {
		t.Error("a malformed frame must not be treated as an ack")
	}

	runtime.config.ProtocolV2Enabled = false
	if runtime.consumeAck([]byte(`{"type":"ack","ack_upto_seq":1}`)) {
		t.Error("acks must be ignored when the event queue is disabled")
	}
}

func TestEnqueueSnapshotEvent(t *testing.T) {
	runtime := newTestRuntime(t)
	runtime.enqueueSnapshotEvent(protocol.Handshake{Type: "handshake", ServerID: "srv-test", WorkspaceID: "ws-test"})

	if runtime.eventQueue.PendingCount() != 1 {
		t.Fatalf("pending = %d, want 1", runtime.eventQueue.PendingCount())
	}
	if !runtime.eventQueue.HasPendingSnapshot() {
		t.Fatal("expected the snapshot to be recognized as pending")
	}

	// A second snapshot on top of a pending one would only grow the backlog.
	runtime.enqueueSnapshotEvent(protocol.Handshake{Type: "handshake"})
	if runtime.eventQueue.PendingCount() != 1 {
		t.Fatalf("pending = %d, want the duplicate snapshot to be skipped", runtime.eventQueue.PendingCount())
	}
}

func TestEnqueueContainerDeltaSkipsUnchangedState(t *testing.T) {
	runtime := newTestRuntime(t)
	container := protocol.HandshakeContainer{ID: "c1", Name: "web", Image: "nginx", Ports: "80", Status: "running"}

	runtime.enqueueContainerDelta(container)
	if runtime.eventQueue.PendingCount() != 1 {
		t.Fatalf("pending = %d, want 1", runtime.eventQueue.PendingCount())
	}

	runtime.enqueueContainerDelta(container)
	if runtime.eventQueue.PendingCount() != 1 {
		t.Fatalf("pending = %d, want the unchanged container to be skipped", runtime.eventQueue.PendingCount())
	}

	// A status change is coalesced onto the pending delta for the same id.
	container.Status = "stopped"
	runtime.enqueueContainerDelta(container)
	if runtime.eventQueue.PendingCount() != 1 {
		t.Fatalf("pending = %d, want the delta to be coalesced", runtime.eventQueue.PendingCount())
	}
	due := runtime.eventQueue.DueItems(10)
	if len(due) != 1 {
		t.Fatalf("expected one due item, got %d", len(due))
	}
	var payload map[string]any
	if err := json.Unmarshal(due[0].Envelope.Payload, &payload); err != nil {
		t.Fatalf("cannot decode the delta payload: %v", err)
	}
	if payload["status"] != "stopped" {
		t.Fatalf("payload status = %v, want the latest state", payload["status"])
	}
	if due[0].Envelope.Class != protocol.ClassSignal {
		t.Fatalf("class = %q, want %q so recovery may drop it", due[0].Envelope.Class, protocol.ClassSignal)
	}
}

func TestEnqueueContainerDeltaIgnoresBlankID(t *testing.T) {
	runtime := newTestRuntime(t)
	runtime.enqueueContainerDelta(protocol.HandshakeContainer{ID: "  ", Name: "web"})

	if runtime.eventQueue.PendingCount() != 0 {
		t.Fatal("a container without an id must not be enqueued")
	}
}

// Removed containers are forgotten so a recreated one emits a fresh delta.
func TestForgetMissingContainers(t *testing.T) {
	runtime := newTestRuntime(t)
	runtime.lastContainerDeltaState = map[string]string{"c1": "running", "c2": "running"}

	runtime.forgetMissingContainers(map[string]struct{}{"c1": {}})

	if _, exists := runtime.lastContainerDeltaState["c2"]; exists {
		t.Fatal("expected the missing container to be forgotten")
	}
	if _, exists := runtime.lastContainerDeltaState["c1"]; !exists {
		t.Fatal("expected the active container to be kept")
	}
}

func TestAckProgressStalled(t *testing.T) {
	runtime := newTestRuntime(t)

	if runtime.ackProgressStalled() {
		t.Fatal("an empty queue is never stalled")
	}

	for index := 0; index < queueResetMinPending; index++ {
		if _, err := runtime.eventQueue.Enqueue(protocol.NewEnvelope()); err != nil {
			t.Fatalf("enqueue failed: %v", err)
		}
	}
	if runtime.ackProgressStalled() {
		t.Fatal("a recent ack means the stream is still healthy")
	}

	runtime.lastAckProgressAt = time.Now().Add(-2 * ackStallThreshold)
	if !runtime.ackProgressStalled() {
		t.Fatal("expected a stalled stream to be detected")
	}

	// A reset that just happened must not be repeated immediately.
	runtime.lastQueueResetAt = time.Now()
	if runtime.ackProgressStalled() {
		t.Fatal("expected the reset cooldown to be respected")
	}
}
