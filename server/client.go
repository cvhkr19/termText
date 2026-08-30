package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"termtext/internal/protocol"
	"termtext/server/store"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 8192
	outboxSize     = 32
)

// newUpgrader builds the upgrader with its origin policy pinned to
// allowed (see -allowed-origins in main).
func newUpgrader(allowed []string) *websocket.Upgrader {
	return &websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     originChecker(allowed),
	}
}

// originChecker enforces Origin on upgrade — unconditional true here
// enables cross-site WS hijacking. Missing Origin (native clients) is OK.
func originChecker(allowed []string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		for _, a := range allowed {
			if strings.EqualFold(a, origin) {
				return true
			}
		}
		log.Printf("refusing websocket upgrade from origin %q (not in -allowed-origins)", origin)
		return false
	}
}

// checkProtocolVersion rejects an upgrade whose protocol_version query
// param is missing or doesn't match protocol.ProtocolVersion, with 426
// (Upgrade Required) and a message naming both versions — see
// PROTOCOL.md. Checked once per connection, not per envelope.
func checkProtocolVersion(w http.ResponseWriter, r *http.Request) bool {
	raw := r.URL.Query().Get("protocol_version")
	if raw == "" {
		http.Error(w, "missing protocol_version query parameter", http.StatusUpgradeRequired)
		return false
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid protocol_version %q", raw), http.StatusUpgradeRequired)
		return false
	}
	if v != protocol.ProtocolVersion {
		http.Error(w, fmt.Sprintf("unsupported protocol_version %d, server speaks %d", v, protocol.ProtocolVersion), http.StatusUpgradeRequired)
		return false
	}
	return true
}

// serveWS authenticates the token, upgrades to WebSocket, registers
// with the hub, and starts the read/write pumps.
func serveWS(hub *Hub, st *store.Store, upgrader *websocket.Upgrader, w http.ResponseWriter, r *http.Request) {
	if !checkProtocolVersion(w, r) {
		return
	}

	user, ok := authenticateHTTP(st, w, r)
	if !ok {
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error for %s: %v", user.Username, err)
		return
	}

	client := &Client{
		id:       user.ID,
		username: user.Username,
		outbox:   make(chan protocol.Envelope, outboxSize),
	}
	hub.register <- client

	go client.writePump(conn)
	go client.readPump(hub, st, conn)

	// Presence first — catch-up (contact list, missed requests/messages)
	// can take a moment and shouldn't delay it.
	broadcastPresence(hub, st, client, "online")

	// Catch-up order: contacts, pending requests, then missed messages —
	// all three just replay rows already in SQLite.
	pushContactList(hub, st, client)
	pushPendingContactRequests(st, client)
	flushOfflineMessages(hub, st, client)
}

// readPump is the only goroutine that calls conn.ReadMessage. Decodes
// each envelope and dispatches by type to the hub or contacts.go.
func (c *Client) readPump(hub *Hub, st *store.Store, conn *websocket.Conn) {
	defer func() {
		hub.unregister <- c
		broadcastPresence(hub, st, c, "offline")
		conn.Close()
	}()

	conn.SetReadLimit(maxMessageSize)
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("read error from %s: %v", c.username, err)
			}
			return
		}

		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			c.trySend(errEnvelope("malformed envelope: " + err.Error()))
			continue
		}

		switch env.Type {
		case protocol.TypeMessage:
			handleMessage(hub, st, c, env.Payload)

		case protocol.TypeAckRead:
			handleAckRead(hub, st, c, env.Payload)

		case protocol.TypeTyping:
			handleTyping(hub, st, c, env.Payload)

		case protocol.TypeHistoryRequest:
			handleHistoryRequest(st, c, env.Payload)

		case protocol.TypeContactRequest:
			handleContactRequest(hub, st, c, env.Payload)

		case protocol.TypeContactAccept:
			handleContactAccept(hub, st, c, env.Payload)

		case protocol.TypeContactDecline:
			handleContactDecline(hub, st, c, env.Payload)

		default:
			c.trySend(errEnvelope("unsupported envelope type: " + string(env.Type)))
		}
	}
}

// writePump is the only goroutine that calls conn.WriteMessage
// (gorilla/websocket's single-writer rule). Drains outbox and pings.
func (c *Client) writePump(conn *websocket.Conn) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		conn.Close()
	}()

	for {
		select {
		case env, ok := <-c.outbox:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed our outbox — replaced or dropped as slow; close cleanly.
				conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			raw, err := json.Marshal(env)
			if err != nil {
				log.Printf("encode error for %s: %v", c.username, err)
				continue
			}
			if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
				return
			}

		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// trySend delivers directly to this client's own outbox (e.g. an
// error reply). Non-blocking — drops on a full outbox.
func (c *Client) trySend(env protocol.Envelope) {
	select {
	case c.outbox <- env:
	default:
		log.Printf("hub: dropping %s to %s, outbox full", env.Type, c.username)
	}
}

func errEnvelope(msg string) protocol.Envelope {
	env, err := protocol.Encode(protocol.TypeError, protocol.ErrorPayload{Message: msg})
	if err != nil {
		// Unreachable unless encoding/json itself is broken.
		panic(err)
	}
	return env
}

// bearerToken reads the token from Authorization only — do not add a
// ?token= query fallback; URLs leak into logs/history/Referer headers.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}
