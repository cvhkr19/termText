package protocol

import (
	"encoding/json"
	"testing"
)

// TestEncodeRoundTripsPayload confirms Encode/Unmarshal round-trips
// values exactly, zero and non-zero fields alike.
func TestEncodeRoundTripsPayload(t *testing.T) {
	want := MessagePayload{
		ConversationID: 7,
		ClientMsgID:    14,
		Body:           "hey",
	}

	env, err := Encode(TypeMessage, want)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if env.Type != TypeMessage {
		t.Fatalf("envelope type = %q, want %q", env.Type, TypeMessage)
	}

	// Decode envelope shape first (as readLoop does), then payload
	// (as each handler does).
	var raw Envelope
	if err := json.Unmarshal(mustMarshal(t, env), &raw); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	var got MessagePayload
	if err := json.Unmarshal(raw.Payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got != want {
		t.Fatalf("round-tripped payload = %+v, want %+v", got, want)
	}
}

// TestEncodeOmitsZeroValueFields confirms a zero-value field (ID/From
// on a client->server message, Before on a cursor-less history request)
// is omitted from the wire entirely, not sent as its zero value — see
// PROTOCOL.md.
func TestEncodeOmitsZeroValueFields(t *testing.T) {
	env, err := Encode(TypeMessage, MessagePayload{ConversationID: 7, ClientMsgID: 14, Body: "hey"})
	if err != nil {
		t.Fatalf("encode message: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(env.Payload, &fields); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, absent := range []string{"id", "from", "sent_at"} {
		if _, present := fields[absent]; present {
			t.Errorf("client->server message payload has %q key, want it omitted (got %v)", absent, fields)
		}
	}

	env, err = Encode(TypeHistoryRequest, HistoryRequestPayload{ConversationID: 7})
	if err != nil {
		t.Fatalf("encode history request: %v", err)
	}
	fields = nil
	if err := json.Unmarshal(env.Payload, &fields); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, absent := range []string{"before", "limit"} {
		if _, present := fields[absent]; present {
			t.Errorf("history_request with no cursor has %q key, want it omitted (got %v)", absent, fields)
		}
	}
	if _, present := fields["conversation_id"]; !present {
		t.Errorf("history_request is missing required conversation_id key (got %v)", fields)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
