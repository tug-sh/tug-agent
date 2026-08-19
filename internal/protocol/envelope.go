package protocol

import (
	"encoding/json"
	"strings"
	"time"
)

const Version = "2"

type Entity string
type Action string

const (
	EntityServer    Entity = "server"
	EntityContainer Entity = "container"
	EntityRuntime   Entity = "runtime"
	EntityNetwork   Entity = "network"
	EntityProject   Entity = "project"
	EntitySystem    Entity = "system"
)

const (
	ActionSnapshot      Action = "snapshot"
	ActionCreated       Action = "created"
	ActionUpdated       Action = "updated"
	ActionDeleted       Action = "deleted"
	ActionStatusChanged Action = "status_changed"
	ActionRequest       Action = "request"
)

type Envelope struct {
	ProtocolVersion string          `json:"protocol_version"`
	MessageID       string          `json:"message_id"`
	StreamID        string          `json:"stream_id,omitempty"`
	ServerID        string          `json:"server_id,omitempty"`
	WorkspaceID     string          `json:"workspace_id,omitempty"`
	Entity          Entity          `json:"entity"`
	Action          Action          `json:"action"`
	Seq             uint64          `json:"seq"`
	SentAtUnixMS    int64           `json:"sent_at_unix_ms"`
	Class           string          `json:"class,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	Trace           json.RawMessage `json:"trace,omitempty"`
}

type Ack struct {
	ProtocolVersion string `json:"protocol_version"`
	Type            string `json:"type"`
	StreamID        string `json:"stream_id,omitempty"`
	ServerID        string `json:"server_id,omitempty"`
	AckUptoSeq      uint64 `json:"ack_upto_seq"`
	MessageID       string `json:"message_id,omitempty"`
	Accepted        bool   `json:"accepted"`
	Reason          string `json:"reason,omitempty"`
	Entity          Entity `json:"entity,omitempty"`
	Action          Action `json:"action,omitempty"`
	SentAtUnixMS    int64  `json:"sent_at_unix_ms"`
}

func (ack Ack) IsAck() bool {
	if !strings.EqualFold(strings.TrimSpace(ack.Type), "ack") {
		return false
	}
	protocolVersion := strings.TrimSpace(ack.ProtocolVersion)
	return protocolVersion == "" || protocolVersion == Version
}

func NewEnvelope() Envelope {
	return Envelope{
		ProtocolVersion: Version,
		SentAtUnixMS:    time.Now().UnixMilli(),
		Class:           ClassFact,
	}
}
