package main

import (
	"errors"
	"net/http"
	"testing"
)

// A 426 (protocol version mismatch) must not be treated like a 401: the
// token is fine, so re-login can't fix it, and forcing the user back to
// the login screen mid-chat would be a needless disruption on top of a
// problem login can't solve anyway.
func TestHandleConnectErrProtocolMismatchDuringReconnectPreservesSessionAndScreen(t *testing.T) {
	m := model{
		screen:           screenChat,
		token:            "still-valid-token",
		reconnecting:     true,
		reconnectAttempt: 3,
	}
	msg := wsConnectErrMsg{
		err:        errors.New("connect to host: unsupported protocol_version 2, server speaks 1"),
		statusCode: http.StatusUpgradeRequired,
	}

	newM, cmd := m.handleConnectErr(msg)
	got := newM.(model)

	if got.token != "still-valid-token" {
		t.Errorf("token = %q, want it preserved", got.token)
	}
	if got.screen != screenChat {
		t.Errorf("screen = %v, want it to stay screenChat, not forced to the login screen", got.screen)
	}
	if got.reconnecting {
		t.Error("reconnecting should stop on a version mismatch, not keep retrying forever")
	}
	if got.connErr != msg.err.Error() {
		t.Errorf("connErr = %q, want %q", got.connErr, msg.err.Error())
	}
	if cmd != nil {
		t.Error("expected no further reconnect Cmd to be scheduled")
	}
}

// The very first connect (a saved token, before any chat screen has ever
// shown) hits the same 426 path but starts from screenAuth — same
// contract: token and screen both survive.
func TestHandleConnectErrProtocolMismatchOnInitialConnectPreservesToken(t *testing.T) {
	m := model{
		screen: screenAuth,
		token:  "saved-token",
	}
	msg := wsConnectErrMsg{
		err:        errors.New("connect to host: unsupported protocol_version 2, server speaks 1"),
		statusCode: http.StatusUpgradeRequired,
	}

	newM, _ := m.handleConnectErr(msg)
	got := newM.(model)

	if got.token != "saved-token" {
		t.Errorf("token = %q, want it preserved — re-login can't fix an outdated client", got.token)
	}
	if got.auth.errMsg != msg.err.Error() {
		t.Errorf("auth.errMsg = %q, want %q", got.auth.errMsg, msg.err.Error())
	}
}

// Pins the contrast with a 426: a 401 means the token itself is bad, so
// clearing it and sending the user to a fresh login is correct there.
// If this test and the two above it ever start failing together, the two
// error paths have probably been merged back into one by mistake.
func TestHandleConnectErrUnauthorizedClearsTokenAndReturnsToLogin(t *testing.T) {
	m := model{
		screen:       screenChat,
		token:        "bad-token",
		reconnecting: true,
	}
	msg := wsConnectErrMsg{
		err:        errors.New("connect to host: invalid or expired token"),
		statusCode: http.StatusUnauthorized,
	}

	newM, _ := m.handleConnectErr(msg)
	got := newM.(model)

	if got.token != "" {
		t.Errorf("token = %q, want it cleared after a 401", got.token)
	}
	if got.screen != screenAuth {
		t.Errorf("screen = %v, want screenAuth after a 401", got.screen)
	}
}

func TestHandleConnectErrTransientFailureKeepsRetryingDuringReconnect(t *testing.T) {
	m := model{
		screen:           screenChat,
		token:            "still-valid-token",
		reconnecting:     true,
		reconnectAttempt: 1,
	}
	msg := wsConnectErrMsg{
		err:        errors.New("connect to host: connection refused"),
		statusCode: 0,
	}

	newM, cmd := m.handleConnectErr(msg)
	got := newM.(model)

	if !got.reconnecting {
		t.Error("a transient failure mid-reconnect should keep retrying, not give up")
	}
	if got.token != "still-valid-token" {
		t.Errorf("token = %q, want it preserved during a retry", got.token)
	}
	if cmd == nil {
		t.Error("expected a scheduled retry Cmd")
	}
}
