package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"termtext/internal/protocol"
)

// newTestWSServer spins up a real listener (httptest.NewServer, not a
// ResponseRecorder — the upgrade needs a real hijackable connection) with
// /ws wired to serveWS, plus one registered user with a live session.
func newTestWSServer(t *testing.T) (wsURL string, token string) {
	t.Helper()
	st := openTestStore(t)
	hub := NewHub()
	go hub.Run()
	upgrader := newUpgrader(nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWS(hub, st, upgrader, w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	userID, err := st.CreateUser("protoveruser", "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token = "protover-token"
	if err := st.CreateSession(userID, token, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws", token
}

func dialWS(wsURL, token string) (*websocket.Conn, *http.Response, error) {
	header := http.Header{"Authorization": {"Bearer " + token}}
	return websocket.DefaultDialer.Dial(wsURL, header)
}

func TestServeWSAcceptsMatchingProtocolVersion(t *testing.T) {
	wsURL, token := newTestWSServer(t)

	conn, resp, err := dialWS(wsURL+"?protocol_version="+strconv.Itoa(protocol.ProtocolVersion), token)
	if err != nil {
		t.Fatalf("dial with the correct protocol_version failed: %v", err)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}
}

func TestServeWSRejectsMissingProtocolVersion(t *testing.T) {
	wsURL, token := newTestWSServer(t)

	_, resp, err := dialWS(wsURL, token)
	if err == nil {
		t.Fatal("expected the dial to fail with no protocol_version")
	}
	if resp == nil {
		t.Fatal("expected an HTTP response explaining the rejection, got none")
	}
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusUpgradeRequired)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "protocol_version") {
		t.Errorf("error body doesn't mention protocol_version: %q", body)
	}
}

func TestServeWSRejectsUnsupportedProtocolVersion(t *testing.T) {
	wsURL, token := newTestWSServer(t)

	_, resp, err := dialWS(wsURL+"?protocol_version=999", token)
	if err == nil {
		t.Fatal("expected the dial to fail with an unsupported protocol_version")
	}
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusUpgradeRequired)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "999") {
		t.Errorf("error body doesn't name the client's version: %q", body)
	}
	if !strings.Contains(string(body), strconv.Itoa(protocol.ProtocolVersion)) {
		t.Errorf("error body doesn't name the version the server speaks: %q", body)
	}
}

func TestServeWSRejectsMalformedProtocolVersion(t *testing.T) {
	wsURL, token := newTestWSServer(t)

	_, resp, err := dialWS(wsURL+"?protocol_version=not-a-number", token)
	if err == nil {
		t.Fatal("expected the dial to fail with a non-numeric protocol_version")
	}
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusUpgradeRequired)
	}
}
