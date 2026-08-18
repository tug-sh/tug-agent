package agent

import (
	"crypto/rand"
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

const (
	eventClassFact    = "fact"
	eventClassSignal  = "signal"
	queueCompactEvery = 32
)

type queuedEnvelopeV2 struct {
	Envelope     outboundEventEnvelopeV2 `json:"envelope"`
	RetryCount   int                     `json:"retry_count"`
	LastAttempt  int64                   `json:"last_attempt_unix_ms"`
	NextAttempt  int64                   `json:"next_attempt_unix_ms"`
	CreatedAt    int64                   `json:"created_at_unix_ms"`
	LastError    string                  `json:"last_error,omitempty"`
	CoalesceKey  string                  `json:"coalesce_key,omitempty"`
}

type durableEventQueueV2 struct {
	mu           sync.Mutex
	path         string
	logPath      string
	streamID     string
	nextSeq      uint64
	ackUpto      uint64
	itemsBySeq   map[uint64]*queuedEnvelopeV2
	acksSince    int
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
	return fmt.Sprintf("%x", buffer)
}

func (q *durableEventQueueV2) ensurePath() {
	if strings.TrimSpace(q.path) == "" {
		q.path = filepath.Join(GetDataDir(), "agent-protocol-v2-queue.json")
	}
	ext := filepath.Ext(q.path)
	base := strings.TrimSuffix(q.path, ext)
	if ext == "" {
		base = q.path
	}
	q.logPath = base + ".ndjson"
}

func (q *durableEventQueueV2) load() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ensurePath()
	if err := q.loadMetaLocked(); err != nil {
		if !os.IsNotExist(err) {
			if fallbackErr := q.loadLegacyJSONLocked(); fallbackErr == nil {
				return q.compactLocked()
			}
			return err
		}
		if fallbackErr := q.loadLegacyJSONLocked(); fallbackErr == nil {
			return q.compactLocked()
		}
	}
	if err := q.loadLogLocked(); err != nil && !os.IsNotExist(err) {
		if fallbackErr := q.loadLegacyJSONLocked(); fallbackErr == nil {
			return q.compactLocked()
		}
		return err
	}
	return nil
}

func (q *durableEventQueueV2) loadMetaLocked() error {
	raw, err := os.ReadFile(q.path)
	if err != nil {
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
	if len(payload.Items) > 0 {
		q.itemsBySeq = make(map[uint64]*queuedEnvelopeV2, len(payload.Items))
		for _, item := range payload.Items {
			copied := item
			if copied.Envelope.Seq <= q.ackUpto {
				continue
			}
			q.itemsBySeq[copied.Envelope.Seq] = &copied
			if copied.Envelope.Seq >= q.nextSeq {
				q.nextSeq = copied.Envelope.Seq + 1
			}
		}
	}
	return nil
}

func (q *durableEventQueueV2) loadLegacyJSONLocked() error {
	return q.loadMetaLocked()
}

func (q *durableEventQueueV2) loadLogLocked() error {
	raw, err := os.ReadFile(q.logPath)
	if err != nil {
		return err
	}
	if q.itemsBySeq == nil {
		q.itemsBySeq = make(map[uint64]*queuedEnvelopeV2)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var item queuedEnvelopeV2
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		if item.Envelope.Seq == 0 || item.Envelope.Seq <= q.ackUpto {
			continue
		}
		q.itemsBySeq[item.Envelope.Seq] = &item
		if item.Envelope.Seq >= q.nextSeq {
			q.nextSeq = item.Envelope.Seq + 1
		}
	}
	return nil
}

func (q *durableEventQueueV2) persistMetaLocked() error {
	payload := struct {
		StreamID string `json:"stream_id"`
		NextSeq  uint64 `json:"next_seq"`
		AckUpto  uint64 `json:"ack_upto"`
	}{
		StreamID: q.streamID,
		NextSeq:  q.nextSeq,
		AckUpto:  q.ackUpto,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(q.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(q.path, raw, 0o600)
}

func (q *durableEventQueueV2) appendLogLocked(item queuedEnvelopeV2) error {
	if err := os.MkdirAll(filepath.Dir(q.logPath), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(q.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(raw, '\n'))
	return err
}

func (q *durableEventQueueV2) compactLocked() error {
	items := make([]queuedEnvelopeV2, 0, len(q.itemsBySeq))
	for _, item := range q.itemsBySeq {
		if item.Envelope.Seq <= q.ackUpto {
			continue
		}
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Envelope.Seq < items[j].Envelope.Seq
	})
	if err := os.MkdirAll(filepath.Dir(q.logPath), 0o755); err != nil {
		return err
	}
	tmp := q.logPath + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	for _, item := range items {
		raw, marshalErr := json.Marshal(item)
		if marshalErr != nil {
			_ = file.Close()
			return marshalErr
		}
		if _, writeErr := file.Write(append(raw, '\n')); writeErr != nil {
			_ = file.Close()
			return writeErr
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, q.logPath); err != nil {
		return err
	}
	q.acksSince = 0
	return q.persistMetaLocked()
}

func (q *durableEventQueueV2) persistLocked() error {
	return q.compactLocked()
}

func (q *durableEventQueueV2) enqueue(env outboundEventEnvelopeV2) (queuedEnvelopeV2, error) {
	return q.enqueueCoalesced(env, "")
}

func (q *durableEventQueueV2) enqueueCoalesced(env outboundEventEnvelopeV2, coalesceKey string) (queuedEnvelopeV2, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ensurePath()
	if strings.TrimSpace(env.Class) == "" {
		env.Class = eventClassFact
	}
	coalesceKey = strings.TrimSpace(coalesceKey)
	if coalesceKey != "" {
		for _, existing := range q.itemsBySeq {
			if existing.CoalesceKey != coalesceKey || existing.LastAttempt != 0 {
				continue
			}
			existing.Envelope.Payload = env.Payload
			existing.Envelope.MessageID = env.MessageID
			existing.Envelope.SentAtUnixMS = env.SentAtUnixMS
			existing.Envelope.Class = env.Class
			_ = q.appendLogLocked(*existing)
			return *existing, q.persistMetaLocked()
		}
	}
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
		CoalesceKey: coalesceKey,
	}
	q.itemsBySeq[item.Envelope.Seq] = &item
	if err := q.appendLogLocked(item); err != nil {
		return queuedEnvelopeV2{}, err
	}
	if err := q.persistMetaLocked(); err != nil {
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
	q.acksSince++
	if len(q.itemsBySeq) == 0 || q.acksSince >= queueCompactEvery {
		return q.compactLocked()
	}
	return q.persistMetaLocked()
}

func (q *durableEventQueueV2) dueItems(limit int) []queuedEnvelopeV2 {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UnixMilli()
	items := make([]queuedEnvelopeV2, 0, len(q.itemsBySeq))
	for _, item := range q.itemsBySeq {
		if item.Envelope.Seq <= q.ackUpto {
			continue
		}
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
		return q.persistMetaLocked()
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
	dropped := 0
	for seq, item := range q.itemsBySeq {
		if strings.EqualFold(strings.TrimSpace(item.Envelope.Class), eventClassSignal) {
			delete(q.itemsBySeq, seq)
			dropped++
		}
	}
	if dropped == 0 {
		return 0, nil
	}
	return dropped, q.compactLocked()
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
