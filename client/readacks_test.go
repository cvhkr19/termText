package main

import (
	"encoding/json"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"termtext/internal/protocol"
)

func newModelWithUnreadMessage() model {
	m := initialModel(func(tea.Msg) {}, config{}, endpoint{host: "localhost:8080"}, "")
	m.screen = screenChat
	m.me = "alice"
	m.contacts = []contact{{username: "bob", conversationID: 1}}
	m.messages = map[string][]message{
		"bob": {{id: 42, from: "bob", body: "hi", sentAt: time.Now()}},
	}
	return m
}

// Reading a message while disconnected (outbox nil, matching
// handleDisconnected) must not be recorded as successfully acked — that
// was the bug: markConversationRead used to set readAckSent unconditionally,
// so a failed send due to being offline was indistinguishable from a real
// one, and nothing ever retried it.
func TestMarkConversationReadDoesNotClaimSuccessWhileDisconnected(t *testing.T) {
	m := newModelWithUnreadMessage()
	m.outbox = nil // disconnected

	m.markConversationRead("bob")

	got := m.messages["bob"][0]
	if !got.readLocally {
		t.Error("readLocally should be true — the user did view this message")
	}
	if got.readAckSent {
		t.Error("readAckSent should be false — trySend had no live outbox to send through")
	}
}

// The whole point of tracking readLocally separately: once reconnected,
// anything read-but-unacked must actually go out, without needing the
// user to revisit that conversation again.
func TestResendPendingReadAcksSendsOnceReconnected(t *testing.T) {
	m := newModelWithUnreadMessage()
	m.outbox = nil
	m.markConversationRead("bob") // "read" while offline; ack attempt fails silently

	if m.messages["bob"][0].readAckSent {
		t.Fatal("test setup invalid: readAckSent should still be false before reconnecting")
	}

	m.outbox = make(chan protocol.Envelope, outboxSize) // reconnected
	m.resendPendingReadAcks()

	if !m.messages["bob"][0].readAckSent {
		t.Error("readAckSent should be true after resendPendingReadAcks succeeds")
	}

	select {
	case env := <-m.outbox:
		if env.Type != protocol.TypeAckRead {
			t.Fatalf("envelope type = %q, want %q", env.Type, protocol.TypeAckRead)
		}
		var payload protocol.AckPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if payload.MessageID != 42 {
			t.Errorf("MessageID = %d, want 42", payload.MessageID)
		}
	default:
		t.Fatal("expected an ack_read envelope in the outbox after resendPendingReadAcks")
	}
}

// An already-successfully-acked message must not be resent — resending
// forever would be harmless to correctness but is still worth pinning so
// a future change doesn't accidentally make every reconnect replay every
// read receipt ever sent.
func TestResendPendingReadAcksSkipsAlreadyAcked(t *testing.T) {
	m := newModelWithUnreadMessage()
	m.messages["bob"][0].readLocally = true
	m.messages["bob"][0].readAckSent = true
	m.outbox = make(chan protocol.Envelope, outboxSize)

	m.resendPendingReadAcks()

	select {
	case env := <-m.outbox:
		t.Fatalf("expected nothing resent for an already-acked message, got %+v", env)
	default:
	}
}

// A message never viewed at all (readLocally false) must not be acked
// just because a reconnect happened — resendPendingReadAcks only catches
// up genuine backlog, it doesn't mark everything read.
func TestResendPendingReadAcksSkipsUnviewedMessages(t *testing.T) {
	m := newModelWithUnreadMessage() // readLocally is false by default
	m.outbox = make(chan protocol.Envelope, outboxSize)

	m.resendPendingReadAcks()

	if m.messages["bob"][0].readAckSent {
		t.Error("a message the user never viewed must not get acked by a reconnect")
	}
	select {
	case env := <-m.outbox:
		t.Fatalf("expected nothing sent for an unviewed message, got %+v", env)
	default:
	}
}
