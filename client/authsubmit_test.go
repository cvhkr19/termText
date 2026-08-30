package main

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func newAuthScreenModel() model {
	m := initialModel(func(tea.Msg) {}, config{}, endpoint{host: "localhost:8080"}, "")
	m.screen = screenAuth
	m.auth.username.SetValue("alice")
	m.auth.password.SetValue("hunter2")
	return m
}

// A second Enter landing before the first request's authResultMsg comes
// back must not fire a second request. Bubble Tea's Update() runs
// strictly sequentially, so two Enter presses in a row are two separate
// calls with the model's state fully updated in between — the guard only
// has to check m.auth.submitting, not worry about a true data race.
func TestDoubleEnterDoesNotSubmitTwice(t *testing.T) {
	m := newAuthScreenModel()

	newM, cmd := m.updateAuth(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	if cmd == nil {
		t.Fatal("first Enter should submit and return a doAuth Cmd")
	}
	if !m.auth.submitting {
		t.Fatal("submitting should be true while the first request is in flight")
	}

	newM, cmd = m.updateAuth(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	if cmd != nil {
		t.Error("a second Enter while already submitting must not fire another request")
	}
}

// Once the in-flight request resolves (authResultMsg), submitting must
// reset — a real retry (e.g. after a rate-limit error) has to work.
func TestEnterAfterResultResolvesSubmitsAgain(t *testing.T) {
	m := newAuthScreenModel()

	newM, _ := m.updateAuth(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)

	newM, _ = m.updateAuth(authResultMsg{username: "alice", err: errors.New("too many requests, slow down")})
	m = newM.(model)
	if m.auth.submitting {
		t.Fatal("submitting should reset once the request resolves, success or not")
	}

	newM, cmd := m.updateAuth(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	if cmd == nil {
		t.Error("a retry after the previous request resolved should submit normally")
	}
}
