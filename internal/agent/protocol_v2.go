package agent

import (
	"encoding/json"
	"strings"
	"time"
)

const protocolVersionV2 = "2"

type eventEntity string
type eventAction string

const (
	entityServer    eventEntity = "server"
	entityContainer eventEntity = "container"
	entityRuntime   eventEntity = "runtime"
	entityNetwork   eventEntity = "network"
	entityProject   eventEntity = "project"
	entitySystem    eventEntity = "system"
)

const (
	actionSnapshot      eventAction = "snapshot"
	actionCreated       eventAction = "created"
	actionUpdated       eventAction = "updated"
	actionDeleted       eventAction = "deleted"
	actionStatusChanged eventAction = "status_changed"
	actionRequest       eventAction = "request"
)

type outboundEventEnvelopeV2 struct {
	ProtocolVersion string          `json:"protocol_version"`
	MessageID       string          `json:"message_id"`
	StreamID        string          `json:"stream_id,omitempty"`
	ServerID        string          `json:"server_id,omitempty"`
	WorkspaceID     string          `json:"workspace_id,omitempty"`
	Entity          eventEntity     `json:"entity"`
	Action          eventAction     `json:"action"`
	Seq             uint64          `json:"seq"`
	SentAtUnixMS    int64           `json:"sent_at_unix_ms"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	Trace           json.RawMessage `json:"trace,omitempty"`
}

type inboundAckV2 struct {
	ProtocolVersion string      `json:"protocol_version"`
	Type            string      `json:"type"`
	StreamID        string      `json:"stream_id,omitempty"`
	ServerID        string      `json:"server_id,omitempty"`
	AckUptoSeq      uint64      `json:"ack_upto_seq"`
	MessageID       string      `json:"message_id,omitempty"`
	Accepted        bool        `json:"accepted"`
	Reason          string      `json:"reason,omitempty"`
	Entity          eventEntity `json:"entity,omitempty"`
	Action          eventAction `json:"action,omitempty"`
	SentAtUnixMS    int64       `json:"sent_at_unix_ms"`
}

func (ack inboundAckV2) isAck() bool {
	if strings.TrimSpace(ack.ProtocolVersion) != protocolVersionV2 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(ack.Type), "ack")
}

func newOutboundEnvelopeV2() outboundEventEnvelopeV2 {
	return outboundEventEnvelopeV2{
		ProtocolVersion: protocolVersionV2,
		SentAtUnixMS:    time.Now().UnixMilli(),
	}
}
