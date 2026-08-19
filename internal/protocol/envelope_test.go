package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewEnvelopeDefaults(t *testing.T) {
	before := time.Now().UnixMilli()
	envelope := NewEnvelope()

	if envelope.ProtocolVersion != Version {
		t.Errorf("ProtocolVersion = %q, want %q", envelope.ProtocolVersion, Version)
	}
	if envelope.Class != ClassFact {
		t.Errorf("Class = %q, want %q", envelope.Class, ClassFact)
	}
	if envelope.SentAtUnixMS < before {
		t.Errorf("SentAtUnixMS = %d, want at least %d", envelope.SentAtUnixMS, before)
	}
}

func TestAckIsAck(t *testing.T) {
	cases := []struct {
		name string
		ack  Ack
		want bool
	}{
		{"current version", Ack{Type: "ack", ProtocolVersion: Version}, true},
		{"missing version", Ack{Type: "ack"}, true},
		{"padded and capitalized type", Ack{Type: " ACK ", ProtocolVersion: Version}, true},
		{"other version", Ack{Type: "ack", ProtocolVersion: "99"}, false},
		{"other type", Ack{Type: "command", ProtocolVersion: Version}, false},
		{"empty frame", Ack{}, false},
	}
	for _, testCase := range cases {
		if got := testCase.ack.IsAck(); got != testCase.want {
			t.Errorf("%s: IsAck() = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

// The envelope is the contract with the API, so its JSON keys are pinned here.
func TestEnvelopeWireFormat(t *testing.T) {
	envelope := NewEnvelope()
	envelope.MessageID = "m1"
	envelope.ServerID = "srv-1"
	envelope.Entity = EntityContainer
	envelope.Action = ActionStatusChanged
	envelope.Seq = 42
	envelope.Payload = json.RawMessage(`{"id":"c1"}`)

	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	for _, key := range []string{"protocol_version", "message_id", "server_id", "entity", "action", "seq", "sent_at_unix_ms", "class", "payload"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing %q in %s", key, raw)
		}
	}
	if decoded["entity"] != string(EntityContainer) {
		t.Errorf("entity = %v, want %q", decoded["entity"], EntityContainer)
	}
	if decoded["seq"].(float64) != 42 {
		t.Errorf("seq = %v, want 42", decoded["seq"])
	}
}

// Empty optional fields must stay off the wire so heartbeats and deltas remain
// small on constrained connections.
func TestCommandResultOmitsEmptyFields(t *testing.T) {
	raw, err := json.Marshal(CommandResult{Type: "command_result", CommandID: "c1", Success: true})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	for _, key := range []string{"error", "logs", "payload"} {
		if _, ok := decoded[key]; ok {
			t.Errorf("expected %q to be omitted, got %s", key, raw)
		}
	}
}
