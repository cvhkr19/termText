package main

import (
	"encoding/json"
	"errors"
	"log"

	"termtext/internal/protocol"
	"termtext/server/store"
)

// handleContactRequest implements /add <username>.
func handleContactRequest(hub *Hub, st *store.Store, from *Client, raw json.RawMessage) {
	target, ok := resolveContactTarget(st, from, raw)
	if !ok {
		return
	}

	status, err := st.RequestContact(from.id, target.ID)
	switch {
	case errors.Is(err, store.ErrSelfContact):
		from.trySend(errEnvelope("can't add yourself"))
		return
	case err != nil:
		log.Printf("request contact %s -> %s: %v", from.username, target.Username, err)
		from.trySend(errEnvelope("internal error"))
		return
	}

	switch status {
	case "pending":
		// Live-push if online; otherwise replayed via
		// pushPendingContactRequests on next connect.
		env, _ := protocol.Encode(protocol.TypeContactRequest, protocol.ContactPayload{Username: from.username})
		hub.route <- routedEnvelope{to: target.Username, env: env}

	case "accepted":
		// Mutual add: target already requested us, so auto-accept both sides.
		notifyContactAccepted(hub, st, from, target)
	}
}

// handleContactAccept implements /accept <username>.
func handleContactAccept(hub *Hub, st *store.Store, from *Client, raw json.RawMessage) {
	requester, ok := resolveContactTarget(st, from, raw)
	if !ok {
		return
	}

	if err := st.AcceptContact(from.id, requester.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			from.trySend(errEnvelope("no pending request from " + requester.Username))
			return
		}
		log.Printf("accept contact %s <- %s: %v", from.username, requester.Username, err)
		from.trySend(errEnvelope("internal error"))
		return
	}

	// Notify both sides so each sidebar updates live.
	notifyContactAccepted(hub, st, from, requester)
}

// notifyContactAccepted tells both sides they're now contacts, handing
// each the conversation_id to start messaging.
func notifyContactAccepted(hub *Hub, st *store.Store, from *Client, other store.User) {
	conv, err := st.GetOrCreateConversation(from.id, other.ID)
	if err != nil {
		log.Printf("get or create conversation %s<->%s: %v", from.username, other.Username, err)
		return
	}

	toSelf, _ := protocol.Encode(protocol.TypeContactAccept, protocol.ContactAcceptPayload{
		Username: other.Username, ConversationID: conv.ID,
	})
	from.trySend(toSelf)

	toOther, _ := protocol.Encode(protocol.TypeContactAccept, protocol.ContactAcceptPayload{
		Username: from.username, ConversationID: conv.ID,
	})
	hub.route <- routedEnvelope{to: other.Username, env: toOther}
}

// handleContactDecline implements /decline <username>.
func handleContactDecline(hub *Hub, st *store.Store, from *Client, raw json.RawMessage) {
	requester, ok := resolveContactTarget(st, from, raw)
	if !ok {
		return
	}

	if err := st.DeclineContact(from.id, requester.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			from.trySend(errEnvelope("no pending request from " + requester.Username))
			return
		}
		log.Printf("decline contact %s <- %s: %v", from.username, requester.Username, err)
		from.trySend(errEnvelope("internal error"))
		return
	}

	// Notify the original requester only — we never had this contact.
	env, _ := protocol.Encode(protocol.TypeContactDecline, protocol.ContactPayload{Username: from.username})
	hub.route <- routedEnvelope{to: requester.Username, env: env}
}

// resolveContactTarget looks up payload.Username, sending an error
// envelope and returning ok=false on any failure.
func resolveContactTarget(st *store.Store, from *Client, raw json.RawMessage) (store.User, bool) {
	var payload protocol.ContactPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		from.trySend(errEnvelope("malformed contact payload: " + err.Error()))
		return store.User{}, false
	}

	target, err := st.GetUserByUsername(payload.Username)
	if errors.Is(err, store.ErrNotFound) {
		from.trySend(errEnvelope("no such user " + payload.Username))
		return store.User{}, false
	}
	if err != nil {
		log.Printf("lookup %q: %v", payload.Username, err)
		from.trySend(errEnvelope("internal error"))
		return store.User{}, false
	}
	return target, true
}

// pushContactList sends c the accepted-contacts sidebar, with each
// contact's live online/offline status from the hub.
func pushContactList(hub *Hub, st *store.Store, c *Client) {
	contacts, err := st.AcceptedContacts(c.id)
	if err != nil {
		log.Printf("load contacts for %s: %v", c.username, err)
		return
	}

	usernames := make([]string, len(contacts))
	for i, u := range contacts {
		usernames[i] = u.Username
	}
	online := hub.OnlineStatus(usernames)

	statuses := make([]protocol.ContactStatus, 0, len(contacts))
	for _, u := range contacts {
		conv, err := st.GetOrCreateConversation(c.id, u.ID)
		if err != nil {
			log.Printf("get or create conversation %s<->%s: %v", c.username, u.Username, err)
			continue
		}
		status := "offline"
		if online[u.Username] {
			status = "online"
		}
		statuses = append(statuses, protocol.ContactStatus{
			Username: u.Username, Status: status, ConversationID: conv.ID,
		})
	}

	env, err := protocol.Encode(protocol.TypeContactList, protocol.ContactListPayload{Contacts: statuses})
	if err != nil {
		log.Printf("encode contact_list: %v", err)
		return
	}
	c.trySend(env)
}

// pushPendingContactRequests replays contact_request envelopes missed
// while c was offline — the pending row is the durable record.
func pushPendingContactRequests(st *store.Store, c *Client) {
	pending, err := st.PendingIncoming(c.id)
	if err != nil {
		log.Printf("load pending requests for %s: %v", c.username, err)
		return
	}
	for _, requester := range pending {
		env, err := protocol.Encode(protocol.TypeContactRequest, protocol.ContactPayload{Username: requester.Username})
		if err != nil {
			log.Printf("encode contact_request: %v", err)
			continue
		}
		c.trySend(env)
	}
}
