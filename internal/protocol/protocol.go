// Package protocol defines the JSON wire format shared by server and
// client. See PROTOCOL.md for the full spec.
package protocol

import "encoding/json"

// ProtocolVersion is the wire-format version this build speaks, sent by
// the client as a query param on every WebSocket upgrade and checked
// once at connect time — see PROTOCOL.md. Bump this whenever a change
// here would break compatibility with a build on the other end still
// speaking the old version.
const ProtocolVersion = 1

// SentAtLayout is RFC3339 with a *fixed*-width fractional second — not
// time.RFC3339Nano, which trims trailing zeros and would desync the
// history "before" cursor's plain-string ordering.
const SentAtLayout = "2006-01-02T15:04:05.000000000Z07:00"

// MaxUploadBytes is the default /upload cap and the client's local
// pre-flight check. The server's actual cap is configurable
// (-max-upload-size) and always the authority.
const MaxUploadBytes = 25 << 20 // 25MB

// EnvelopeType identifies the shape of an Envelope's Payload.
type EnvelopeType string

const (
	TypeMessage         EnvelopeType = "message"
	TypeMessageAck      EnvelopeType = "message_ack"
	TypeTyping          EnvelopeType = "typing"
	TypePresence        EnvelopeType = "presence"
	TypeAckDelivered    EnvelopeType = "ack_delivered"
	TypeAckRead         EnvelopeType = "ack_read"
	TypeContactRequest  EnvelopeType = "contact_request"
	TypeContactAccept   EnvelopeType = "contact_accept"
	TypeContactDecline  EnvelopeType = "contact_decline"
	TypeContactList     EnvelopeType = "contact_list"
	TypeHistoryRequest  EnvelopeType = "history_request"
	TypeHistoryResponse EnvelopeType = "history_response"
	TypeError           EnvelopeType = "error"
)

// Envelope is the wire frame in both directions. Payload stays raw
// JSON so the hub can route on Type without a full unmarshal.
type Envelope struct {
	Type    EnvelopeType    `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// MessagePayload is a chat message addressed by conversation_id.
// ClientMsgID (client->server only) is how the sender later correlates
// message_ack, since the server never echoes a message back to its own
// sender. FileID/FileName/FileSize (set together) make this a file
// reference instead of plain text.
type MessagePayload struct {
	ID             int64  `json:"id,omitempty"`
	ConversationID int64  `json:"conversation_id"`
	ClientMsgID    int64  `json:"client_msg_id,omitempty"`
	From           string `json:"from,omitempty"`
	Body           string `json:"body"`
	SentAt         string `json:"sent_at,omitempty"`
	FileID         string `json:"file_id,omitempty"`
	FileName       string `json:"file_name,omitempty"`
	FileSize       int64  `json:"file_size,omitempty"`
}

// MessageAckPayload maps ClientMsgID to the server-assigned ServerID.
// Sent immediately after persist, always before ack_delivered/ack_read —
// it's the only way the sender learns which optimistic message this is.
type MessageAckPayload struct {
	ClientMsgID int64  `json:"client_msg_id"`
	ServerID    int64  `json:"server_id"`
	SentAt      string `json:"sent_at"`
}

// TypingPayload is a live-only, unpersisted typing indicator.
type TypingPayload struct {
	ConversationID int64  `json:"conversation_id"`
	IsTyping       bool   `json:"is_typing"`
	From           string `json:"from,omitempty"`
}

// PresencePayload reports a user's online/offline transition, pushed to
// their accepted contacts. Not tied to any one conversation.
type PresencePayload struct {
	Username string `json:"username"`
	Status   string `json:"status"` // "online" or "offline"
}

// AckPayload names the message an ack_delivered/ack_read envelope concerns.
type AckPayload struct {
	MessageID int64 `json:"message_id"`
}

// HistoryRequestPayload asks for a page of history. Before is an
// RFC3339 cursor (empty = most recent); Limit is capped server-side.
type HistoryRequestPayload struct {
	ConversationID int64  `json:"conversation_id"`
	Before         string `json:"before,omitempty"`
	Limit          int    `json:"limit,omitempty"`
}

// HistoryMessage is one entry in a HistoryResponsePayload.
type HistoryMessage struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	Body      string `json:"body"`
	SentAt    string `json:"sent_at"`
	Delivered bool   `json:"delivered"`
	Read      bool   `json:"read"`
	FileID    string `json:"file_id,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	FileSize  int64  `json:"file_size,omitempty"`
}

// HistoryResponsePayload is a page of messages, newest-to-oldest.
type HistoryResponsePayload struct {
	ConversationID int64            `json:"conversation_id"`
	Messages       []HistoryMessage `json:"messages"`
}

// ErrorPayload is sent back to the originating connection only.
type ErrorPayload struct {
	Message string `json:"message"`
}

// ContactPayload names the other user by username — used for
// contact_request/contact_accept/contact_decline in both directions.
type ContactPayload struct {
	Username string `json:"username"`
}

// ContactAcceptPayload also hands over the ConversationID both sides
// need — accepting always has one ready (see GetOrCreateConversation).
type ContactAcceptPayload struct {
	Username       string `json:"username"`
	ConversationID int64  `json:"conversation_id"`
}

// ContactStatus is one entry in a ContactListPayload.
type ContactStatus struct {
	Username       string `json:"username"`
	Status         string `json:"status"` // "online" or "offline"
	ConversationID int64  `json:"conversation_id"`
}

// ContactListPayload is pushed right after WebSocket auth succeeds.
type ContactListPayload struct {
	Contacts []ContactStatus `json:"contacts"`
}

// Encode wraps a payload value into an Envelope with its JSON encoded.
func Encode(t EnvelopeType, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Type: t, Payload: raw}, nil
}
