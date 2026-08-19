package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"

	"tug.sh/pkg/protocol"
)

// Everything the agent sends is an envelope. There is no second, unframed path
// any more: the previous agent wrote handshakes and results straight onto the
// socket while also queueing envelopes, and the two disagreed about field names
// often enough to produce results the API silently dropped.

// emitFact queues a message that must arrive. It is written to disk first and
// resent until acknowledged, so a socket that dies mid-write costs nothing.
func (runtime *Runtime) emitFact(entity protocol.Entity, action protocol.Action, messageID string, payload any) error {
	envelope, err := runtime.envelope(entity, action, messageID, payload)
	if err != nil {
		return err
	}
	if _, err := runtime.eventQueue.Enqueue(envelope); err != nil {
		return fmt.Errorf("cannot queue %s %s: %w", entity, action, err)
	}
	return nil
}

// emitSignal sends something that is worthless once late: a heartbeat, progress
// on a command that will report its own result, a byte of terminal output. It
// bypasses the queue entirely, so it never holds up the sequence.
func (runtime *Runtime) emitSignal(
	conn *websocket.Conn,
	entity protocol.Entity,
	action protocol.Action,
	payload any,
) error {
	envelope, err := runtime.envelope(entity, action, "", payload)
	if err != nil {
		return err
	}
	return runtime.writeJSON(conn, envelope.AsSignal())
}

func (runtime *Runtime) envelope(
	entity protocol.Entity,
	action protocol.Action,
	messageID string,
	payload any,
) (protocol.Envelope, error) {
	envelope := protocol.NewEnvelope(entity, action)
	envelope.ServerID = runtime.config.ServerID
	envelope.MessageID = orGenerated(messageID, entity, action)
	return envelope.WithPayload(payload)
}

func orGenerated(messageID string, entity protocol.Entity, action protocol.Action) string {
	if messageID != "" {
		return messageID
	}
	return fmt.Sprintf("%s-%s-%d", entity, action, time.Now().UnixNano())
}

// writeJSON serialises and sends a frame. All writers share one mutex because
// gorilla allows a single concurrent writer per connection.
func (runtime *Runtime) writeJSON(conn *websocket.Conn, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	runtime.writeMu.Lock()
	defer runtime.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	err = conn.WriteMessage(websocket.TextMessage, raw)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}
