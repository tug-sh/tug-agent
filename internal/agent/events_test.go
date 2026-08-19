package agent

import (
	"encoding/json"
	"testing"
	"time"

	"tug.sh/pkg/protocol"
)

func TestConsumeAckAdvancesQueue(t *testing.T) {
	runtime := newTestRuntime(t)
	for index := 0; index < 3; index++ {
		if _, err := runtime.eventQueue.Enqueue(protocol.NewEnvelope(protocol.EntityRuntime, protocol.ActionSnapshot)); err != nil {
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

}

func TestSnapshotIsQueuedOnceWhileOneIsPending(t *testing.T) {
	runtime := newTestRuntime(t)

	if err := runtime.sendSnapshot(); err != nil {
		t.Fatalf("cannot queue a snapshot: %v", err)
	}
	if runtime.eventQueue.PendingCount() != 1 {
		t.Fatalf("pending = %d, want 1", runtime.eventQueue.PendingCount())
	}
	if !runtime.eventQueue.HasPendingSnapshot() {
		t.Fatal("expected the snapshot to be recognized as pending")
	}

	// A second snapshot on top of a pending one would only grow the backlog.
	if err := runtime.sendSnapshot(); err != nil {
		t.Fatalf("cannot queue a snapshot: %v", err)
	}
	if runtime.eventQueue.PendingCount() != 1 {
		t.Fatalf("pending = %d, want the duplicate snapshot to be skipped", runtime.eventQueue.PendingCount())
	}
}

func TestContainerDeltaSkipsUnchangedState(t *testing.T) {
	runtime := newTestRuntime(t)
	conn, frames := newRecordingSocket(t)
	container := protocol.HandshakeContainer{ID: "c1", Name: "web", Image: "nginx", Ports: "80", Status: "running"}

	runtime.publishContainerDelta(conn, container)
	first := waitForEnvelope(t, frames)
	if !first.IsSignal() {
		t.Fatalf("class = %q, want a signal so it never enters the queue", first.Class)
	}
	if first.Seq != 0 {
		t.Fatalf("seq = %d, want a signal to carry none", first.Seq)
	}
	if runtime.eventQueue.PendingCount() != 0 {
		t.Fatalf("pending = %d, want a delta to stay out of the durable queue", runtime.eventQueue.PendingCount())
	}

	runtime.publishContainerDelta(conn, container)
	container.Status = "stopped"
	runtime.publishContainerDelta(conn, container)

	// The unchanged repeat is dropped, so the next frame is the status change.
	second := waitForEnvelope(t, frames)
	var delta protocol.ContainerDelta
	if err := json.Unmarshal(second.Payload, &delta); err != nil {
		t.Fatalf("cannot decode the delta payload: %v", err)
	}
	if delta.Status != "stopped" {
		t.Fatalf("status = %q, want the changed state and no repeat in between", delta.Status)
	}
}

func TestContainerDeltaIgnoresBlankID(t *testing.T) {
	runtime := newTestRuntime(t)
	conn, frames := newRecordingSocket(t)

	runtime.publishContainerDelta(conn, protocol.HandshakeContainer{ID: "  ", Name: "web"})

	select {
	case envelope := <-frames:
		t.Fatalf("a container without an id must not be reported, got %+v", envelope)
	case <-time.After(200 * time.Millisecond):
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
		if _, err := runtime.eventQueue.Enqueue(protocol.NewEnvelope(protocol.EntityRuntime, protocol.ActionSnapshot)); err != nil {
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
