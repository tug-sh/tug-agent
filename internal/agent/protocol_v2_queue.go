package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var retryDelaySchedule = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
	30 * time.Second,
}

type queuedEnvelopeV2 struct {
	Envelope    outboundEventEnvelopeV2 `json:"envelope"`
	RetryCount  int                     `json:"retry_count"`
	LastAttempt int64                   `json:"last_attempt_unix_ms"`
	NextAttempt int64                   `json:"next_attempt_unix_ms"`
	CreatedAt   int64                   `json:"created_at_unix_ms"`
	LastError   string                  `json:"last_error,omitempty"`
}

type durableEventQueueV2 struct {
	mu         sync.Mutex
	path       string
	streamID   string
	nextSeq    uint64
	ackUpto    uint64
	itemsBySeq map[uint64]*queuedEnvelopeV2
}

func newDurableEventQueueV2(path string) *durableEventQueueV2 {
	return &durableEventQueueV2{
		path:       path,
		streamID:   randomStreamID(),
		nextSeq:    1,
		itemsBySeq: make(map[uint64]*queuedEnvelopeV2),
	}
}

func randomStreamID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("stream-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func (q *durableEventQueueV2) ensurePath() {
	if strings.TrimSpace(q.path) != "" {
		return
	}
	q.path = filepath.Join(GetDataDir(), "agent-protocol-v2-queue.json")
}

func (q *durableEventQueueV2) load() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ensurePath()
	raw, err := os.ReadFile(q.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var payload struct {
		StreamID string             `json:"stream_id"`
		NextSeq  uint64             `json:"next_seq"`
		AckUpto  uint64             `json:"ack_upto"`
		Items    []queuedEnvelopeV2 `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if strings.TrimSpace(payload.StreamID) != "" {
		q.streamID = strings.TrimSpace(payload.StreamID)
	}
	if payload.NextSeq > 0 {
		q.nextSeq = payload.NextSeq
	}
	q.ackUpto = payload.AckUpto
	q.itemsBySeq = make(map[uint64]*queuedEnvelopeV2, len(payload.Items))
	for _, item := range payload.Items {
		copied := item
		q.itemsBySeq[item.Envelope.Seq] = &copied
		if item.Envelope.Seq >= q.nextSeq {
			q.nextSeq = item.Envelope.Seq + 1
		}
	}
	return nil
}

func (q *durableEventQueueV2) persistLocked() error {
	items := make([]queuedEnvelopeV2, 0, len(q.itemsBySeq))
	for _, item := range q.itemsBySeq {
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Envelope.Seq < items[j].Envelope.Seq
	})
	payload := struct {
		StreamID string             `json:"stream_id"`
		NextSeq  uint64             `json:"next_seq"`
		AckUpto  uint64             `json:"ack_upto"`
		Items    []queuedEnvelopeV2 `json:"items"`
	}{
		StreamID: q.streamID,
		NextSeq:  q.nextSeq,
		AckUpto:  q.ackUpto,
		Items:    items,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(q.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(q.path, raw, 0o600)
}

func (q *durableEventQueueV2) enqueue(env outboundEventEnvelopeV2) (queuedEnvelopeV2, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	env.StreamID = q.streamID
	env.Seq = q.nextSeq
	q.nextSeq++
	now := time.Now().UnixMilli()
	item := queuedEnvelopeV2{
		Envelope:    env,
		RetryCount:  0,
		LastAttempt: 0,
		NextAttempt: now,
		CreatedAt:   now,
	}
	q.itemsBySeq[item.Envelope.Seq] = &item
	if err := q.persistLocked(); err != nil {
		return queuedEnvelopeV2{}, err
	}
	return item, nil
}

func (q *durableEventQueueV2) acknowledge(ackUpto uint64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if ackUpto <= q.ackUpto {
		return nil
	}
	q.ackUpto = ackUpto
	for seq := range q.itemsBySeq {
		if seq <= ackUpto {
			delete(q.itemsBySeq, seq)
		}
	}
	return q.persistLocked()
}

func (q *durableEventQueueV2) dueItems(limit int) []queuedEnvelopeV2 {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UnixMilli()
	items := make([]queuedEnvelopeV2, 0, len(q.itemsBySeq))
	for _, item := range q.itemsBySeq {
		if item.NextAttempt <= now {
			items = append(items, *item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Envelope.Seq < items[j].Envelope.Seq
	})
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

func (q *durableEventQueueV2) markAttempt(seq uint64, writeErr error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	item, ok := q.itemsBySeq[seq]
	if !ok {
		return nil
	}
	item.LastAttempt = time.Now().UnixMilli()
	item.RetryCount++
	if writeErr != nil {
		backoff := retryDelayForAttempt(item.RetryCount)
		item.NextAttempt = time.Now().Add(backoff).UnixMilli()
		item.LastError = writeErr.Error()
		return q.persistLocked()
	}
	item.LastError = ""
	item.NextAttempt = time.Now().Add(90 * time.Second).UnixMilli()
	return nil
}

func (q *durableEventQueueV2) pendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.itemsBySeq)
}

func (q *durableEventQueueV2) ackUptoSeq() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.ackUpto
}

func (q *durableEventQueueV2) resetPendingForRecovery() (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	dropped := len(q.itemsBySeq)
	if dropped == 0 {
		return 0, nil
	}
	q.itemsBySeq = make(map[uint64]*queuedEnvelopeV2)
	if q.nextSeq > 0 {
		q.ackUpto = q.nextSeq - 1
	}
	if err := q.persistLocked(); err != nil {
		return 0, err
	}
	return dropped, nil
}

func retryDelayForAttempt(retryCount int) time.Duration {
	if retryCount <= 1 {
		return retryDelaySchedule[0]
	}
	index := retryCount - 1
	if index >= len(retryDelaySchedule) {
		return retryDelaySchedule[len(retryDelaySchedule)-1]
	}
	return retryDelaySchedule[index]
}

func (q *durableEventQueueV2) debugSnapshot() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return fmt.Sprintf("stream=%s next_seq=%d ack_upto=%d pending=%d", q.streamID, q.nextSeq, q.ackUpto, len(q.itemsBySeq))
}
