package protocol

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestQueueEnqueueAckAndReload(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.json")
	queue := NewQueue(queuePath)

	firstPayload, _ := json.Marshal(map[string]string{"id": "c1", "status": "running"})
	env := NewEnvelope()
	env.MessageID = "m1"
	env.ServerID = "srv-1"
	env.Entity = EntityContainer
	env.Action = ActionStatusChanged
	env.Payload = firstPayload

	item, err := queue.Enqueue(env)
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	if item.Envelope.Seq != 1 {
		t.Fatalf("expected seq=1, got %d", item.Envelope.Seq)
	}
	if queue.PendingCount() != 1 {
		t.Fatalf("expected pending=1, got %d", queue.PendingCount())
	}

	if err := queue.Acknowledge(1); err != nil {
		t.Fatalf("ack failed: %v", err)
	}
	if queue.PendingCount() != 0 {
		t.Fatalf("expected pending=0 after ack, got %d", queue.PendingCount())
	}

	reloaded := NewQueue(queuePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if reloaded.PendingCount() != 0 {
		t.Fatalf("expected reloaded pending=0, got %d", reloaded.PendingCount())
	}
}

// Unacknowledged events must survive an agent restart, otherwise a crash
// between send and ack silently loses dashboard state.
func TestQueueReloadKeepsUnacknowledgedItems(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.json")
	queue := NewQueue(queuePath)

	for index := 0; index < 3; index++ {
		envelope := NewEnvelope()
		envelope.Entity = EntityContainer
		envelope.Action = ActionStatusChanged
		if _, err := queue.Enqueue(envelope); err != nil {
			t.Fatalf("enqueue failed: %v", err)
		}
	}
	if err := queue.Acknowledge(1); err != nil {
		t.Fatalf("ack failed: %v", err)
	}

	reloaded := NewQueue(queuePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if reloaded.PendingCount() != 2 {
		t.Fatalf("expected pending=2 after reload, got %d", reloaded.PendingCount())
	}
	if reloaded.AckUptoSeq() != 1 {
		t.Fatalf("expected ack_upto=1 after reload, got %d", reloaded.AckUptoSeq())
	}
	due := reloaded.DueItems(10)
	if len(due) != 2 || due[0].Envelope.Seq != 2 || due[1].Envelope.Seq != 3 {
		t.Fatalf("expected seq 2 and 3 to be due, got %+v", due)
	}
	// A restarted agent must not reuse sequence numbers.
	next, err := reloaded.Enqueue(NewEnvelope())
	if err != nil {
		t.Fatalf("enqueue after reload failed: %v", err)
	}
	if next.Envelope.Seq != 4 {
		t.Fatalf("expected the next seq to be 4, got %d", next.Envelope.Seq)
	}
}

func TestQueueCoalesceUnsent(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.json")
	queue := NewQueue(queuePath)
	first := NewEnvelope()
	first.MessageID = "m1"
	first.Entity = EntityContainer
	first.Action = ActionStatusChanged
	first.Payload = []byte(`{"id":"c1","status":"running"}`)
	if _, err := queue.EnqueueCoalesced(first, "container:c1"); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	second := NewEnvelope()
	second.MessageID = "m2"
	second.Entity = EntityContainer
	second.Action = ActionStatusChanged
	second.Payload = []byte(`{"id":"c1","status":"stopped"}`)
	item, err := queue.EnqueueCoalesced(second, "container:c1")
	if err != nil {
		t.Fatalf("coalesce failed: %v", err)
	}
	if queue.PendingCount() != 1 {
		t.Fatalf("expected coalesced pending=1, got %d", queue.PendingCount())
	}
	if string(item.Envelope.Payload) != string(second.Payload) {
		t.Fatalf("expected replaced payload")
	}
}

func TestQueueRetryScheduling(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.json")
	queue := NewQueue(queuePath)
	env := NewEnvelope()
	env.MessageID = "m-retry"
	env.ServerID = "srv-1"
	env.Entity = EntityRuntime
	env.Action = ActionSnapshot
	if _, err := queue.Enqueue(env); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	due := queue.DueItems(10)
	if len(due) != 1 {
		t.Fatalf("expected one due item, got %d", len(due))
	}
	seq := due[0].Envelope.Seq
	if err := queue.MarkAttempt(seq, nil); err != nil {
		t.Fatalf("mark attempt failed: %v", err)
	}
	dueAfter := queue.DueItems(10)
	if len(dueAfter) != 0 {
		t.Fatalf("expected no due items immediately after backoff")
	}
}

func TestQueueResetDropsSignalsOnly(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.json")
	queue := NewQueue(queuePath)

	snapshot := NewEnvelope()
	snapshot.MessageID = "snap"
	snapshot.Entity = EntityRuntime
	snapshot.Action = ActionSnapshot
	if _, err := queue.Enqueue(snapshot); err != nil {
		t.Fatalf("enqueue snapshot failed: %v", err)
	}

	delta := NewEnvelope()
	delta.MessageID = "delta"
	delta.Entity = EntityContainer
	delta.Action = ActionStatusChanged
	delta.Class = ClassSignal
	if _, err := queue.Enqueue(delta); err != nil {
		t.Fatalf("enqueue signal failed: %v", err)
	}
	if !queue.HasPendingSnapshot() {
		t.Fatal("expected pending snapshot")
	}

	dropped, err := queue.ResetPendingForRecovery()
	if err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("expected dropped=1 signal, got %d", dropped)
	}
	if queue.PendingCount() != 1 {
		t.Fatalf("expected snapshot to remain, pending=%d", queue.PendingCount())
	}
	if !queue.HasPendingSnapshot() {
		t.Fatal("expected snapshot to remain after signal drop")
	}
}
