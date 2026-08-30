package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"

	"termtext/internal/protocol"
)

const outboxSize = 32

// reconnectBaseDelay/reconnectMaxDelay bound the backoff: 1s, 2s, 4s...
// capped at 30s. Retries continue indefinitely except on a 401.
const (
	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 30 * time.Second
)

// reconnectDelay is the backoff for the Nth reconnect attempt (1-indexed).
func reconnectDelay(attempt int) time.Duration {
	d := reconnectBaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= reconnectMaxDelay {
			return reconnectMaxDelay
		}
	}
	return d
}

// reconnectMsg fires after backoff elapses. gen guards against a stale
// timer from a superseded disconnect cycle.
type reconnectMsg struct {
	gen int
}

func reconnectCmd(gen int, delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return reconnectMsg{gen: gen}
	})
}

// wsConnectedMsg/wsConnectErrMsg/wsDisconnectedMsg mirror
// server/hub.go's register/unregister lifecycle for this one connection.
type wsConnectedMsg struct {
	conn *websocket.Conn
}

type wsConnectErrMsg struct {
	err error
	// statusCode is the rejected upgrade's HTTP status (0 if the dial
	// never got a response) — 401 means the token itself is bad, not retryable.
	statusCode int
}

type wsDisconnectedMsg struct {
	err error
	// conn identifies which connection this came from — see
	// handleDisconnected. /logout closes m.conn deliberately and moves
	// to a brand new model before readLoop's blocked ReadMessage even
	// notices; without this, that now-stale disconnect event would land
	// on the fresh login screen's model and kick off a pointless
	// reconnect with an empty token.
	conn *websocket.Conn
}

// One tea.Msg type per WebSocket envelope type, converted 1:1 from the
// protocol payload (see readLoop).
type (
	incomingMessageMsg         protocol.MessagePayload
	incomingMessageAckMsg      protocol.MessageAckPayload
	incomingTypingMsg          protocol.TypingPayload
	incomingPresenceMsg        protocol.PresencePayload
	incomingAckDeliveredMsg    protocol.AckPayload
	incomingAckReadMsg         protocol.AckPayload
	incomingContactRequestMsg  protocol.ContactPayload
	incomingContactAcceptMsg   protocol.ContactAcceptPayload
	incomingContactDeclineMsg  protocol.ContactPayload
	incomingContactListMsg     protocol.ContactListPayload
	incomingHistoryResponseMsg protocol.HistoryResponsePayload
	incomingErrorMsg           protocol.ErrorPayload
)

// wsConnect dials as a tea.Cmd, on its own goroutine. protocol_version
// rides as a query param, alongside the auth token in the header — the
// server checks it once here, not per envelope (see PROTOCOL.md).
func wsConnect(server endpoint, token string) tea.Cmd {
	return func() tea.Msg {
		header := http.Header{"Authorization": {"Bearer " + token}}
		url := server.wsURL("/ws") + "?protocol_version=" + strconv.Itoa(protocol.ProtocolVersion)
		conn, resp, err := websocket.DefaultDialer.Dial(url, header)
		if err != nil {
			if resp != nil {
				return wsConnectErrMsg{err: fmt.Errorf("connect to %s: %s", server, upgradeErrorBody(resp)), statusCode: resp.StatusCode}
			}
			return wsConnectErrMsg{err: fmt.Errorf("connect to %s: %w", server, err)}
		}
		return wsConnectedMsg{conn: conn}
	}
}

// upgradeErrorBody reads a rejected upgrade's plain-text body (see
// http.Error in server/client.go) — e.g. "unsupported protocol_version
// 2, server speaks 1" — falling back to the HTTP status line if the
// body is empty or unreadable.
func upgradeErrorBody(resp *http.Response) string {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if msg := strings.TrimSpace(string(body)); msg != "" {
		return msg
	}
	return resp.Status
}

// readLoop is the only goroutine that calls conn.ReadMessage. Dispatches
// each envelope as a typed tea.Msg via send.
func readLoop(conn *websocket.Conn, send func(tea.Msg)) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			send(wsDisconnectedMsg{err: err, conn: conn})
			return
		}

		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			// A malformed frame is a server bug — drop it, don't crash.
			continue
		}

		switch env.Type {
		case protocol.TypeMessage:
			var p protocol.MessagePayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingMessageMsg(p))
			}
		case protocol.TypeMessageAck:
			var p protocol.MessageAckPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingMessageAckMsg(p))
			}
		case protocol.TypeTyping:
			var p protocol.TypingPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingTypingMsg(p))
			}
		case protocol.TypePresence:
			var p protocol.PresencePayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingPresenceMsg(p))
			}
		case protocol.TypeAckDelivered:
			var p protocol.AckPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingAckDeliveredMsg(p))
			}
		case protocol.TypeAckRead:
			var p protocol.AckPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingAckReadMsg(p))
			}
		case protocol.TypeContactRequest:
			var p protocol.ContactPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingContactRequestMsg(p))
			}
		case protocol.TypeContactAccept:
			var p protocol.ContactAcceptPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingContactAcceptMsg(p))
			}
		case protocol.TypeContactDecline:
			var p protocol.ContactPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingContactDeclineMsg(p))
			}
		case protocol.TypeContactList:
			var p protocol.ContactListPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingContactListMsg(p))
			}
		case protocol.TypeHistoryResponse:
			var p protocol.HistoryResponsePayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingHistoryResponseMsg(p))
			}
		case protocol.TypeError:
			var p protocol.ErrorPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				send(incomingErrorMsg(p))
			}
		}
	}
}

// writePump is the only goroutine that calls conn.WriteMessage
// (gorilla/websocket's single-writer rule). Serializes outbox onto the wire.
func writePump(conn *websocket.Conn, outbox <-chan protocol.Envelope) {
	for env := range outbox {
		raw, err := json.Marshal(env)
		if err != nil {
			continue
		}
		if conn.WriteMessage(websocket.TextMessage, raw) != nil {
			return
		}
	}
}
