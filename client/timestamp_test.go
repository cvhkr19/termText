package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"termtext/internal/protocol"
)

// withFixedLocal temporarily replaces time.Local for the duration of the
// test, so the UTC-vs-local assertions below don't depend on whatever
// zone happens to be running the test.
func withFixedLocal(t *testing.T, offset time.Duration) {
	t.Helper()
	original := time.Local
	time.Local = time.FixedZone("TEST", int(offset.Seconds()))
	t.Cleanup(func() { time.Local = original })
}

// The server always stamps sent_at in UTC (see protocol.SentAtLayout and
// store.insertMessage). A received message must display in the viewer's
// own local time, exactly like the sender's own optimistic copy (a bare
// time.Now()) already does — otherwise the same message shows two
// different clock times depending on which side of the conversation is
// looking at it, offset by exactly the viewer's UTC distance.
func TestIncomingMessageDisplaysInLocalTime(t *testing.T) {
	withFixedLocal(t, 5*time.Hour+30*time.Minute) // IST, UTC+5:30

	m := initialModel(func(tea.Msg) {}, config{}, endpoint{host: "localhost:8080"}, "")
	m.me = "alice"
	m.contacts = []contact{{username: "bob", conversationID: 1}}
	m.messages = map[string][]message{}

	newM, _ := m.handleIncomingMessage(incomingMessageMsg{
		ID:             1,
		ConversationID: 1,
		From:           "bob",
		Body:           "hi",
		SentAt:         "2026-01-01T12:00:00.000000000Z", // noon UTC
	})
	m = newM.(model)

	got := m.messages["bob"][0].sentAt.Format("15:04")
	if want := "17:30"; got != want { // noon UTC + 5:30 = 17:30 local
		t.Errorf("displayed time = %q, want %q (noon UTC converted to local)", got, want)
	}
}

// Same fix, same bug, for history pages loaded on opening a conversation.
func TestHistoryResponseDisplaysInLocalTime(t *testing.T) {
	withFixedLocal(t, 5*time.Hour+30*time.Minute)

	m := initialModel(func(tea.Msg) {}, config{}, endpoint{host: "localhost:8080"}, "")
	m.me = "alice"
	m.contacts = []contact{{username: "bob", conversationID: 1}}
	m.messages = map[string][]message{}

	newM, _ := m.handleHistoryResponse(incomingHistoryResponseMsg{
		ConversationID: 1,
		Messages: []protocol.HistoryMessage{
			{ID: 1, From: "bob", Body: "hi", SentAt: "2026-01-01T12:00:00.000000000Z"},
		},
	})
	m = newM.(model)

	got := m.messages["bob"][0].sentAt.Format("15:04")
	if want := "17:30"; got != want {
		t.Errorf("displayed time = %q, want %q (noon UTC converted to local)", got, want)
	}
}

// A locally-sent message (the optimistic copy appended by sendChatMessage)
// already used a bare time.Now(), i.e. local time — this pins that it
// stays that way, so a future change doesn't accidentally introduce the
// opposite bug.
func TestOutgoingMessageDisplaysInLocalTimeToo(t *testing.T) {
	withFixedLocal(t, 5*time.Hour+30*time.Minute)

	before := time.Now()
	m := initialModel(func(tea.Msg) {}, config{}, endpoint{host: "localhost:8080"}, "")
	m.me = "alice"
	m.contacts = []contact{{username: "bob", conversationID: 1}}
	m.messages = map[string][]message{}

	m.sendChatMessage("hi", "", "", 0)

	got := m.messages["bob"][0].sentAt
	if got.Location() != time.Local {
		t.Errorf("outgoing message's sentAt location = %v, want time.Local", got.Location())
	}
	if got.Before(before.Add(-time.Second)) || got.After(time.Now().Add(time.Second)) {
		t.Errorf("outgoing message's sentAt = %v, want close to now", got)
	}
}
