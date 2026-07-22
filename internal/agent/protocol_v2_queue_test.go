package agent

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestDurableEventQueueV2EnqueueAckAndReload(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.json")
	q := newDurableEventQueueV2(queuePath)

	firstPayload, _ := json.Marshal(map[string]string{"id": "c1", "status": "running"})
	env := newOutboundEnvelopeV2()
	env.MessageID = "m1"
	env.ServerID = "srv-1"
	env.Entity = entityContainer
	env.Action = actionStatusChanged
	env.Payload = firstPayload

	item, err := q.enqueue(env)
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	if item.Envelope.Seq != 1 {
		t.Fatalf("expected seq=1, got %d", item.Envelope.Seq)
	}
	if q.pendingCount() != 1 {
		t.Fatalf("expected pending=1, got %d", q.pendingCount())
	}

	if err := q.acknowledge(1); err != nil {
		t.Fatalf("ack failed: %v", err)
	}
	if q.pendingCount() != 0 {
		t.Fatalf("expected pending=0 after ack, got %d", q.pendingCount())
	}

	reloaded := newDurableEventQueueV2(queuePath)
	if err := reloaded.load(); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if reloaded.pendingCount() != 0 {
		t.Fatalf("expected reloaded pending=0, got %d", reloaded.pendingCount())
	}
}

func TestDurableEventQueueV2RetryScheduling(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.json")
	q := newDurableEventQueueV2(queuePath)
	env := newOutboundEnvelopeV2()
	env.MessageID = "m-retry"
	env.ServerID = "srv-1"
	env.Entity = entityRuntime
	env.Action = actionSnapshot
	if _, err := q.enqueue(env); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	due := q.dueItems(10)
	if len(due) != 1 {
		t.Fatalf("expected one due item, got %d", len(due))
	}
	seq := due[0].Envelope.Seq
	if err := q.markAttempt(seq, nil); err != nil {
		t.Fatalf("mark attempt failed: %v", err)
	}
	dueAfter := q.dueItems(10)
	if len(dueAfter) != 0 {
		t.Fatalf("expected no due items immediately after backoff")
	}
}

