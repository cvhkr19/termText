package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Two independent incoming contact requests to the same target must both
// stay tracked and actionable: accepting the first must not silently drop
// the second from the model, even though only one string (m.notice) is
// ever visible on screen at a time.
func TestSecondPendingRequestSurvivesAcceptingTheFirst(t *testing.T) {
	m := initialModel(func(tea.Msg) {}, config{}, endpoint{host: "localhost:8080"}, "")
	m.screen = screenChat
	m.contacts = []contact{}
	m.messages = map[string][]message{}

	newM, _ := m.Update(incomingContactRequestMsg{Username: "bob"})
	m = newM.(model)
	newM, _ = m.Update(incomingContactRequestMsg{Username: "john"})
	m = newM.(model)

	if len(m.pendingRequests) != 2 {
		t.Fatalf("pendingRequests = %v, want both bob and john tracked", m.pendingRequests)
	}
	if !strings.Contains(m.notice, "bob") || !strings.Contains(m.notice, "john") {
		t.Fatalf("notice = %q, want it to mention both bob and john", m.notice)
	}

	// Accept bob's — this is the exact step the bug report describes as
	// making john's request vanish.
	newM, _ = m.Update(incomingContactAcceptMsg{Username: "bob", ConversationID: 1})
	m = newM.(model)

	if len(m.pendingRequests) != 1 || m.pendingRequests[0] != "john" {
		t.Fatalf("pendingRequests after accepting bob = %v, want just [john]", m.pendingRequests)
	}
	if !strings.Contains(m.notice, "john") {
		t.Errorf("notice = %q, want it to still mention john's still-pending request", m.notice)
	}

	// john's request must still be independently actionable.
	newM, _ = m.Update(incomingContactAcceptMsg{Username: "john", ConversationID: 2})
	m = newM.(model)
	if len(m.pendingRequests) != 0 {
		t.Errorf("pendingRequests = %v, want empty after accepting both", m.pendingRequests)
	}
	if len(m.contacts) != 2 {
		t.Fatalf("contacts = %+v, want both bob and john added", m.contacts)
	}
}

// Declining one of two pending requests (a purely local, no-server-
// confirmation action — see runCommand) must only resolve that one.
func TestDecliningOnePendingRequestLeavesTheOtherTracked(t *testing.T) {
	m := initialModel(func(tea.Msg) {}, config{}, endpoint{host: "localhost:8080"}, "")
	m.screen = screenChat
	m.ready = true
	m.contacts = []contact{}
	m.messages = map[string][]message{}

	newM, _ := m.Update(incomingContactRequestMsg{Username: "bob"})
	m = newM.(model)
	newM, _ = m.Update(incomingContactRequestMsg{Username: "john"})
	m = newM.(model)

	newM, _ = m.runCommand("/decline bob")
	m = newM.(model)

	if len(m.pendingRequests) != 1 || m.pendingRequests[0] != "john" {
		t.Fatalf("pendingRequests after declining bob = %v, want just [john]", m.pendingRequests)
	}
}

// A request replayed again on reconnect (pushPendingContactRequests
// re-announces every still-pending row on every connect) must not create
// a duplicate entry.
func TestPendingRequestReplayIsIdempotent(t *testing.T) {
	m := initialModel(func(tea.Msg) {}, config{}, endpoint{host: "localhost:8080"}, "")
	m.screen = screenChat

	newM, _ := m.Update(incomingContactRequestMsg{Username: "bob"})
	m = newM.(model)
	newM, _ = m.Update(incomingContactRequestMsg{Username: "bob"}) // replayed on a reconnect
	m = newM.(model)

	if len(m.pendingRequests) != 1 {
		t.Errorf("pendingRequests = %v, want exactly one bob entry, not a duplicate", m.pendingRequests)
	}
}
