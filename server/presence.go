package main

import (
	"encoding/json"
	"errors"
	"log"

	"termtext/internal/protocol"
	"termtext/server/store"
)

// handleTyping relays an ephemeral typing indicator to the other
// participant. Never persisted; dropped if they're offline.
func handleTyping(hub *Hub, st *store.Store, from *Client, raw json.RawMessage) {
	var payload protocol.TypingPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		from.trySend(errEnvelope("malformed typing payload: " + err.Error()))
		return
	}

	other, ok := otherParticipant(st, from, payload.ConversationID)
	if !ok {
		return
	}

	payload.From = from.username
	env, err := protocol.Encode(protocol.TypeTyping, payload)
	if err != nil {
		log.Printf("encode typing: %v", err)
		return
	}
	hub.route <- routedEnvelope{to: other.Username, env: env}
}

// broadcastPresence notifies c's accepted contacts of an online/offline
// transition. Not persisted — contact_list covers it on next connect.
func broadcastPresence(hub *Hub, st *store.Store, c *Client, status string) {
	contacts, err := st.AcceptedContacts(c.id)
	if err != nil {
		log.Printf("load contacts for presence broadcast (%s): %v", c.username, err)
		return
	}

	env, err := protocol.Encode(protocol.TypePresence, protocol.PresencePayload{Username: c.username, Status: status})
	if err != nil {
		log.Printf("encode presence: %v", err)
		return
	}
	for _, contact := range contacts {
		hub.route <- routedEnvelope{to: contact.Username, env: env}
	}
}

// otherParticipant resolves the other side of conversationID, sending
// an error envelope and returning ok=false on any failure.
func otherParticipant(st *store.Store, from *Client, conversationID int64) (store.User, bool) {
	conv, err := st.GetConversation(conversationID)
	if errors.Is(err, store.ErrNotFound) {
		from.trySend(errEnvelope("no such conversation"))
		return store.User{}, false
	}
	if err != nil {
		log.Printf("get conversation %d: %v", conversationID, err)
		from.trySend(errEnvelope("internal error"))
		return store.User{}, false
	}

	otherID, ok := conv.OtherParticipant(from.id)
	if !ok {
		from.trySend(errEnvelope("not a participant in this conversation"))
		return store.User{}, false
	}

	other, err := st.GetUserByID(otherID)
	if err != nil {
		log.Printf("get user %d: %v", otherID, err)
		from.trySend(errEnvelope("internal error"))
		return store.User{}, false
	}
	return other, true
}
