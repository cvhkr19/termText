package main

import (
	"encoding/json"
	"errors"
	"log"

	"termtext/internal/protocol"
	"termtext/server/store"
)

// handleMessage persists a message before any live delivery attempt —
// persistence is the source of truth; live relay is best-effort on top.
func handleMessage(hub *Hub, st *store.Store, from *Client, raw json.RawMessage) {
	var payload protocol.MessagePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		from.trySend(errEnvelope("malformed message payload: " + err.Error()))
		return
	}
	if payload.Body == "" && payload.FileID == "" {
		from.trySend(errEnvelope("message must have a body or a file"))
		return
	}

	other, ok := otherParticipant(st, from, payload.ConversationID)
	if !ok {
		return
	}

	var msg store.Message
	var err error
	if payload.FileID != "" {
		file, ferr := st.GetFile(payload.FileID)
		if errors.Is(ferr, store.ErrNotFound) {
			from.trySend(errEnvelope("no such file"))
			return
		}
		if ferr != nil {
			log.Printf("get file %s: %v", payload.FileID, ferr)
			from.trySend(errEnvelope("internal error"))
			return
		}
		// A file must belong to this conversation and this uploader.
		if file.ConversationID != payload.ConversationID || file.UploaderID != from.id {
			from.trySend(errEnvelope("file does not belong to this conversation"))
			return
		}
		msg, err = st.CreateFileMessage(payload.ConversationID, from.id, payload.Body, file.FileID, file.OriginalFilename, file.Size)
	} else {
		msg, err = st.CreateMessage(payload.ConversationID, from.id, payload.Body)
	}
	if err != nil {
		log.Printf("persist message: %v", err)
		from.trySend(errEnvelope("internal error"))
		return
	}

	// Must be sent before ack_delivered/ack_read below — it's the only
	// way the sender learns the server id. See protocol.MessageAckPayload.
	ack, err := protocol.Encode(protocol.TypeMessageAck, protocol.MessageAckPayload{
		ClientMsgID: payload.ClientMsgID,
		ServerID:    msg.ID,
		SentAt:      msg.SentAt,
	})
	if err != nil {
		log.Printf("encode message_ack: %v", err)
	} else {
		from.trySend(ack)
	}

	out, err := protocol.Encode(protocol.TypeMessage, protocol.MessagePayload{
		ID:             msg.ID,
		ConversationID: msg.ConversationID,
		From:           from.username,
		Body:           msg.Body,
		SentAt:         msg.SentAt,
		FileID:         msg.FileID,
		FileName:       msg.FileName,
		FileSize:       msg.FileSize,
	})
	if err != nil {
		log.Printf("encode message: %v", err)
		return
	}

	// delivered must be buffered — see routedEnvelope.
	delivered := make(chan bool, 1)
	hub.route <- routedEnvelope{to: other.Username, env: out, delivered: delivered}
	if !<-delivered {
		return // persisted regardless; flushed on next connect
	}

	if err := st.MarkDelivered(msg.ID); err != nil {
		log.Printf("mark delivered: %v", err)
		return
	}
	deliveredAck, err := protocol.Encode(protocol.TypeAckDelivered, protocol.AckPayload{MessageID: msg.ID})
	if err != nil {
		return
	}
	from.trySend(deliveredAck)
}

// handleAckRead marks a message read (implying delivered) and relays
// the ack to the sender if online.
func handleAckRead(hub *Hub, st *store.Store, from *Client, raw json.RawMessage) {
	var payload protocol.AckPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		from.trySend(errEnvelope("malformed ack_read payload: " + err.Error()))
		return
	}

	msg, err := st.GetMessage(payload.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		from.trySend(errEnvelope("no such message"))
		return
	}
	if err != nil {
		log.Printf("get message %d: %v", payload.MessageID, err)
		from.trySend(errEnvelope("internal error"))
		return
	}
	if msg.SenderID == from.id {
		from.trySend(errEnvelope("cannot mark your own message as read"))
		return
	}

	conv, err := st.GetConversation(msg.ConversationID)
	if err != nil {
		log.Printf("get conversation %d: %v", msg.ConversationID, err)
		from.trySend(errEnvelope("internal error"))
		return
	}
	if otherID, ok := conv.OtherParticipant(from.id); !ok || otherID != msg.SenderID {
		from.trySend(errEnvelope("not a participant in this conversation"))
		return
	}

	if err := st.MarkRead(msg.ID); err != nil {
		log.Printf("mark read: %v", err)
		from.trySend(errEnvelope("internal error"))
		return
	}

	sender, err := st.GetUserByID(msg.SenderID)
	if err != nil {
		log.Printf("get user %d: %v", msg.SenderID, err)
		return
	}
	env, err := protocol.Encode(protocol.TypeAckRead, protocol.AckPayload{MessageID: msg.ID})
	if err != nil {
		return
	}
	hub.route <- routedEnvelope{to: sender.Username, env: env}
}

// handleHistoryRequest returns a newest-to-oldest page of past
// messages for pagination.
func handleHistoryRequest(st *store.Store, from *Client, raw json.RawMessage) {
	var payload protocol.HistoryRequestPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		from.trySend(errEnvelope("malformed history_request payload: " + err.Error()))
		return
	}

	conv, err := st.GetConversation(payload.ConversationID)
	if errors.Is(err, store.ErrNotFound) {
		from.trySend(errEnvelope("no such conversation"))
		return
	}
	if err != nil {
		log.Printf("get conversation %d: %v", payload.ConversationID, err)
		from.trySend(errEnvelope("internal error"))
		return
	}
	if _, ok := conv.OtherParticipant(from.id); !ok {
		from.trySend(errEnvelope("not a participant in this conversation"))
		return
	}

	page, err := st.HistoryPage(payload.ConversationID, payload.Before, payload.Limit)
	if err != nil {
		log.Printf("history page: %v", err)
		from.trySend(errEnvelope("internal error"))
		return
	}

	messages := make([]protocol.HistoryMessage, len(page))
	for i, m := range page {
		messages[i] = protocol.HistoryMessage{
			ID:        m.ID,
			From:      m.SenderUsername,
			Body:      m.Body,
			SentAt:    m.SentAt,
			Delivered: m.Delivered,
			Read:      m.Read,
			FileID:    m.FileID,
			FileName:  m.FileName,
			FileSize:  m.FileSize,
		}
	}

	env, err := protocol.Encode(protocol.TypeHistoryResponse, protocol.HistoryResponsePayload{
		ConversationID: payload.ConversationID,
		Messages:       messages,
	})
	if err != nil {
		log.Printf("encode history_response: %v", err)
		return
	}
	from.trySend(env)
}

// flushOfflineMessages replays messages that arrived while c was
// offline. delivered=false in messages *is* the queue — no separate table.
func flushOfflineMessages(hub *Hub, st *store.Store, c *Client) {
	msgs, err := st.UndeliveredMessagesFor(c.id)
	if err != nil {
		log.Printf("load undelivered messages for %s: %v", c.username, err)
		return
	}

	for _, m := range msgs {
		env, err := protocol.Encode(protocol.TypeMessage, protocol.MessagePayload{
			ID:             m.ID,
			ConversationID: m.ConversationID,
			From:           m.SenderUsername,
			Body:           m.Body,
			SentAt:         m.SentAt,
			FileID:         m.FileID,
			FileName:       m.FileName,
			FileSize:       m.FileSize,
		})
		if err != nil {
			log.Printf("encode message: %v", err)
			continue
		}
		c.trySend(env)

		if err := st.MarkDelivered(m.ID); err != nil {
			log.Printf("mark delivered: %v", err)
			continue
		}

		// Best-effort: tell the sender it landed, if they're online.
		sender, err := st.GetUserByID(m.SenderID)
		if err != nil {
			continue
		}
		ack, err := protocol.Encode(protocol.TypeAckDelivered, protocol.AckPayload{MessageID: m.ID})
		if err != nil {
			continue
		}
		hub.route <- routedEnvelope{to: sender.Username, env: ack}
	}
}
