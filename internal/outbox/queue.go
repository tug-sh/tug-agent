// Package outbox is the agent's durable send queue.
//
// Facts are written to disk before they go on the wire and stay there until the
// API acknowledges them, so a dropped connection or a restarted agent does not
// lose a command result. Signals never reach this package.
package outbox

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

	"tug.sh/pkg/protocol"
	"tug.sh/services/agent/internal/sandbox"
)

var retryDelaySchedule = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
	30 * time.Second,
}

// The log is rewritten from scratch once this many acknowledgements have
// landed, which keeps the append-only file from growing without bound.
const queueCompactEvery = 32

type QueuedEnvelope struct {
	Envelope    protocol.Envelope `json:"envelope"`
	RetryCount  int               `json:"retry_count"`
	LastAttempt int64             `json:"last_attempt_unix_ms"`
	NextAttempt int64             `json:"next_attempt_unix_ms"`
	CreatedAt   int64             `json:"created_at_unix_ms"`
	LastError   string            `json:"last_error,omitempty"`
	CoalesceKey string            `json:"coalesce_key,omitempty"`
}

type Queue struct {
	mu         sync.Mutex
	path       string
	logPath    string
	streamID   string
	nextSeq    uint64
	ackUpto    uint64
	itemsBySeq map[uint64]*QueuedEnvelope
	acksSince  int
}

func NewQueue(path string) *Queue {
	return &Queue{
		path:       path,
		streamID:   randomStreamID(),
		nextSeq:    1,
		itemsBySeq: make(map[uint64]*QueuedEnvelope),
	}
}

func randomStreamID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("stream-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", buffer)
}

func (queue *Queue) ensurePath() {
	if strings.TrimSpace(queue.path) == "" {
		queue.path = filepath.Join(sandbox.DataDir(), "agent-protocol-v2-queue.json")
	}
	ext := filepath.Ext(queue.path)
	base := strings.TrimSuffix(queue.path, ext)
	if ext == "" {
		base = queue.path
	}
	queue.logPath = base + ".ndjson"
}

func (queue *Queue) Load() error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.ensurePath()
	// Both files are absent on the first run: the meta file holds the stream
	// position, the append log everything written since the last compaction.
	if err := queue.loadMetaLocked(); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := queue.loadLogLocked(); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (queue *Queue) loadMetaLocked() error {
	raw, err := os.ReadFile(queue.path)
	if err != nil {
		return err
	}
	var payload struct {
		StreamID string           `json:"stream_id"`
		NextSeq  uint64           `json:"next_seq"`
		AckUpto  uint64           `json:"ack_upto"`
		Items    []QueuedEnvelope `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if strings.TrimSpace(payload.StreamID) != "" {
		queue.streamID = strings.TrimSpace(payload.StreamID)
	}
	if payload.NextSeq > 0 {
		queue.nextSeq = payload.NextSeq
	}
	queue.ackUpto = payload.AckUpto
	if len(payload.Items) > 0 {
		queue.itemsBySeq = make(map[uint64]*QueuedEnvelope, len(payload.Items))
		for _, item := range payload.Items {
			copied := item
			if copied.Envelope.Seq <= queue.ackUpto {
				continue
			}
			queue.itemsBySeq[copied.Envelope.Seq] = &copied
			if copied.Envelope.Seq >= queue.nextSeq {
				queue.nextSeq = copied.Envelope.Seq + 1
			}
		}
	}
	return nil
}

func (queue *Queue) loadLogLocked() error {
	raw, err := os.ReadFile(queue.logPath)
	if err != nil {
		return err
	}
	if queue.itemsBySeq == nil {
		queue.itemsBySeq = make(map[uint64]*QueuedEnvelope)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var item QueuedEnvelope
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		if item.Envelope.Seq == 0 || item.Envelope.Seq <= queue.ackUpto {
			continue
		}
		queue.itemsBySeq[item.Envelope.Seq] = &item
		if item.Envelope.Seq >= queue.nextSeq {
			queue.nextSeq = item.Envelope.Seq + 1
		}
	}
	return nil
}

func (queue *Queue) persistMetaLocked() error {
	payload := struct {
		StreamID string `json:"stream_id"`
		NextSeq  uint64 `json:"next_seq"`
		AckUpto  uint64 `json:"ack_upto"`
	}{
		StreamID: queue.streamID,
		NextSeq:  queue.nextSeq,
		AckUpto:  queue.ackUpto,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(queue.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(queue.path, raw, 0o600)
}

func (queue *Queue) appendLogLocked(item QueuedEnvelope) error {
	if err := os.MkdirAll(filepath.Dir(queue.logPath), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(queue.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(raw, '\n'))
	return err
}

func (queue *Queue) compactLocked() error {
	items := make([]QueuedEnvelope, 0, len(queue.itemsBySeq))
	for _, item := range queue.itemsBySeq {
		if item.Envelope.Seq <= queue.ackUpto {
			continue
		}
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Envelope.Seq < items[j].Envelope.Seq
	})
	if err := os.MkdirAll(filepath.Dir(queue.logPath), 0o755); err != nil {
		return err
	}
	tmp := queue.logPath + ".tmp"
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
	if err := os.Rename(tmp, queue.logPath); err != nil {
		return err
	}
	queue.acksSince = 0
	return queue.persistMetaLocked()
}

func (queue *Queue) Enqueue(env protocol.Envelope) (QueuedEnvelope, error) {
	return queue.EnqueueCoalesced(env, "")
}

func (queue *Queue) EnqueueCoalesced(env protocol.Envelope, coalesceKey string) (QueuedEnvelope, error) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.ensurePath()
	if strings.TrimSpace(env.Class) == "" {
		env.Class = protocol.ClassFact
	}
	coalesceKey = strings.TrimSpace(coalesceKey)
	if coalesceKey != "" {
		for _, existing := range queue.itemsBySeq {
			if existing.CoalesceKey != coalesceKey || existing.LastAttempt != 0 {
				continue
			}
			existing.Envelope.Payload = env.Payload
			existing.Envelope.MessageID = env.MessageID
			existing.Envelope.SentAtUnixMS = env.SentAtUnixMS
			existing.Envelope.Class = env.Class
			_ = queue.appendLogLocked(*existing)
			return *existing, queue.persistMetaLocked()
		}
	}
	env.StreamID = queue.streamID
	env.Seq = queue.nextSeq
	queue.nextSeq++
	now := time.Now().UnixMilli()
	item := QueuedEnvelope{
		Envelope:    env,
		RetryCount:  0,
		LastAttempt: 0,
		NextAttempt: now,
		CreatedAt:   now,
		CoalesceKey: coalesceKey,
	}
	queue.itemsBySeq[item.Envelope.Seq] = &item
	if err := queue.appendLogLocked(item); err != nil {
		return QueuedEnvelope{}, err
	}
	if err := queue.persistMetaLocked(); err != nil {
		return QueuedEnvelope{}, err
	}
	return item, nil
}

func (queue *Queue) Acknowledge(ackUpto uint64) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if ackUpto <= queue.ackUpto {
		return nil
	}
	queue.ackUpto = ackUpto
	for seq := range queue.itemsBySeq {
		if seq <= ackUpto {
			delete(queue.itemsBySeq, seq)
		}
	}
	queue.acksSince++
	if len(queue.itemsBySeq) == 0 || queue.acksSince >= queueCompactEvery {
		return queue.compactLocked()
	}
	return queue.persistMetaLocked()
}

func (queue *Queue) DueItems(limit int) []QueuedEnvelope {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	now := time.Now().UnixMilli()
	items := make([]QueuedEnvelope, 0, len(queue.itemsBySeq))
	for _, item := range queue.itemsBySeq {
		if item.Envelope.Seq <= queue.ackUpto {
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

func (queue *Queue) MarkAttempt(seq uint64, writeErr error) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	item, ok := queue.itemsBySeq[seq]
	if !ok {
		return nil
	}
	item.LastAttempt = time.Now().UnixMilli()
	item.RetryCount++
	if writeErr != nil {
		backoff := retryDelayForAttempt(item.RetryCount)
		item.NextAttempt = time.Now().Add(backoff).UnixMilli()
		item.LastError = writeErr.Error()
		return queue.persistMetaLocked()
	}
	item.LastError = ""
	item.NextAttempt = time.Now().Add(90 * time.Second).UnixMilli()
	return nil
}

func (queue *Queue) PendingCount() int {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return len(queue.itemsBySeq)
}

func (queue *Queue) HasPendingSnapshot() bool {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for _, item := range queue.itemsBySeq {
		if item == nil {
			continue
		}
		if item.Envelope.Entity == protocol.EntityRuntime && item.Envelope.Action == protocol.ActionSnapshot {
			return true
		}
	}
	return false
}

func (queue *Queue) AckUptoSeq() uint64 {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return queue.ackUpto
}

func (queue *Queue) ResetPendingForRecovery() (int, error) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	dropped := 0
	for seq, item := range queue.itemsBySeq {
		if strings.EqualFold(strings.TrimSpace(item.Envelope.Class), protocol.ClassSignal) {
			delete(queue.itemsBySeq, seq)
			dropped++
		}
	}
	if dropped == 0 {
		return 0, nil
	}
	return dropped, queue.compactLocked()
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

func (queue *Queue) DebugSnapshot() string {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return fmt.Sprintf("stream=%s next_seq=%d ack_upto=%d pending=%d", queue.streamID, queue.nextSeq, queue.ackUpto, len(queue.itemsBySeq))
}
