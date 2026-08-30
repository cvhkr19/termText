package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"termtext/internal/protocol"
)

// handleConnected starts the read/write pumps and switches to the chat screen.
func (m model) handleConnected(msg wsConnectedMsg) (tea.Model, tea.Cmd) {
	m.conn = msg.conn
	m.outbox = make(chan protocol.Envelope, outboxSize)
	go writePump(m.conn, m.outbox)
	go readLoop(m.conn, m.send)

	m.screen = screenChat
	m.connErr = ""
	m.auth.errMsg = ""
	m.auth.submitting = false
	m.reconnecting = false
	m.reconnectAttempt = 0
	// Catches up any ack_read that was read locally but never
	// successfully sent — e.g. read while disconnected. Fires on the
	// very first connect too, harmlessly (nothing is readLocally yet).
	m.resendPendingReadAcks()
	// textarea.Update ignores all input while unfocused — without this
	// Focus(), every keystroke on the chat screen is silently dropped.
	return m, m.input.Focus()
}

// handleConnectErr fires for any failed dial. A 401 means the token
// itself is bad and needs a fresh login; a 426 means this build's
// protocol_version is unsupported — the token is fine, so unlike a 401
// this doesn't clear it or send the user to the login screen, just stops
// and names the mismatch (re-logging in would only hit the same 426
// again). Anything else keeps retrying.
func (m model) handleConnectErr(msg wsConnectErrMsg) (tea.Model, tea.Cmd) {
	if m.reconnecting && msg.statusCode != http.StatusUnauthorized && msg.statusCode != http.StatusUpgradeRequired {
		m.reconnectAttempt++
		delay := reconnectDelay(m.reconnectAttempt)
		m.connErr = fmt.Sprintf("reconnect failed: %s — retrying in %s…", msg.err, delay.Round(time.Second))
		return m, reconnectCmd(m.reconnectGen, delay)
	}

	m.auth.submitting = false
	m.reconnecting = false
	m.reconnectAttempt = 0

	if msg.statusCode == http.StatusUpgradeRequired {
		// Leave m.screen and m.token untouched — whichever screen is
		// current already shows one of these two fields.
		m.auth.errMsg = msg.err.Error()
		m.connErr = msg.err.Error()
		return m, nil
	}

	m.auth.errMsg = msg.err.Error()
	m.screen = screenAuth
	// Drop it — retrying forever would just hang silently, and a token
	// the server actively rejects needs the user to act, not wait.
	m.token = ""
	return m, nil
}

// handleDisconnected fires once for any connection loss (this app never
// closes deliberately) and kicks off the first reconnect attempt.
func (m model) handleDisconnected(msg wsDisconnectedMsg) (tea.Model, tea.Cmd) {
	if msg.conn != m.conn {
		// Stale — about a connection this model has already moved past
		// (e.g. /logout closed it and switched to a fresh model before
		// readLoop noticed). Acting on it would schedule a reconnect
		// against whatever the model's current, unrelated state is.
		return m, nil
	}
	m.conn = nil
	// nil, not closed — trySend already no-ops on nil, and closing here
	// risks a send-on-closed-channel panic from an in-flight Cmd.
	m.outbox = nil

	m.reconnecting = true
	m.reconnectGen++
	m.reconnectAttempt = 1
	delay := reconnectDelay(m.reconnectAttempt)
	m.connErr = fmt.Sprintf("disconnected: %s — reconnecting in %s…", msg.err, delay.Round(time.Second))
	return m, reconnectCmd(m.reconnectGen, delay)
}

// handleReconnectTick fires when backoff elapses. A stale gen (a newer
// disconnect superseded this cycle) is ignored.
func (m model) handleReconnectTick(msg reconnectMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.reconnectGen {
		return m, nil
	}
	m.connErr = fmt.Sprintf("reconnecting… (attempt %d)", m.reconnectAttempt)
	return m, wsConnect(m.server, m.token)
}

// handleContactList replaces the sidebar with the server's list, merged
// (not replaced) against existing contacts so unread counts,
// history-pagination state, and the active conversation survive a reconnect.
func (m model) handleContactList(msg incomingContactListMsg) (tea.Model, tea.Cmd) {
	prior := make(map[string]contact, len(m.contacts))
	for _, c := range m.contacts {
		prior[c.username] = c
	}
	var prevActiveUsername string
	if c, ok := m.activeContact(); ok {
		prevActiveUsername = c.username
	}

	contacts := make([]contact, len(msg.Contacts))
	for i, c := range msg.Contacts {
		merged := contact{
			username:       c.Username,
			conversationID: c.ConversationID,
			online:         c.Status == "online",
		}
		if old, ok := prior[c.Username]; ok {
			merged.unread = old.unread
			merged.historyLoaded = old.historyLoaded
			merged.historyLoading = old.historyLoading
			merged.historyExhausted = old.historyExhausted
		}
		contacts[i] = merged
	}
	m.contacts = contacts

	m.active = 0
	for i, c := range contacts {
		if c.username == prevActiveUsername {
			m.active = i
			break
		}
	}

	// Doesn't go through setActive — trigger the initial history fetch explicitly.
	m.maybeLoadHistory()
	m.refreshViewport()
	return m, nil
}

// addPendingRequest records username as having an outstanding incoming
// request, unless it's already tracked — connect replays an already-known
// request (see pushPendingContactRequests server-side), so this has to be
// idempotent rather than assume every arrival is new.
func (m *model) addPendingRequest(username string) {
	for _, u := range m.pendingRequests {
		if u == username {
			return
		}
	}
	m.pendingRequests = append(m.pendingRequests, username)
}

// removePendingRequest drops username once its request has been resolved
// (accepted or declined) — a no-op if it wasn't tracked, which covers
// contact_accept firing for the *other* side of an accept (see
// handleContactAccept), where the named user was never in this model's
// own pendingRequests to begin with.
func (m *model) removePendingRequest(username string) {
	for i, u := range m.pendingRequests {
		if u == username {
			m.pendingRequests = append(m.pendingRequests[:i], m.pendingRequests[i+1:]...)
			return
		}
	}
}

// pendingRequestsNotice renders every currently-outstanding incoming
// request, not just the one that most recently arrived — the whole
// reason pendingRequests is tracked as a list separately from the
// single-slot notice line: accepting or declining one request must not
// erase any other still-pending one from view.
func (m model) pendingRequestsNotice() string {
	if len(m.pendingRequests) == 0 {
		return ""
	}
	parts := make([]string, len(m.pendingRequests))
	for i, u := range m.pendingRequests {
		parts[i] = fmt.Sprintf("%s wants to add you — /accept %s or /decline %s", u, u, u)
	}
	return strings.Join(parts, "   |   ")
}

// noticeWithPendingRequests combines a one-off status line (a command's
// own feedback, an accept confirmation, ...) with whatever incoming
// requests are still outstanding, so showing the one doesn't cost
// visibility of the other.
func (m model) noticeWithPendingRequests(prefix string) string {
	pending := m.pendingRequestsNotice()
	switch {
	case pending == "":
		return prefix
	case prefix == "":
		return pending
	default:
		return prefix + "   |   " + pending
	}
}

// handleContactRequest is an incoming /add from someone else. There's no
// request-queue UI — just a notice telling the user how to act on it.
func (m model) handleContactRequest(msg incomingContactRequestMsg) (tea.Model, tea.Cmd) {
	m.addPendingRequest(msg.Username)
	m.notice = m.pendingRequestsNotice()
	return m, nil
}

// handleContactAccept fires for both sides of an accept — same
// handling either way since the payload names the other party either way.
func (m model) handleContactAccept(msg incomingContactAcceptMsg) (tea.Model, tea.Cmd) {
	for _, c := range m.contacts {
		if c.username == msg.Username {
			return m, nil // already have them — a duplicate or late event
		}
	}
	// online defaults false — self-corrects on their next connect or
	// this client's own next contact_list.
	m.contacts = append(m.contacts, contact{username: msg.Username, conversationID: msg.ConversationID})
	m.removePendingRequest(msg.Username)
	m.notice = m.noticeWithPendingRequests(fmt.Sprintf("%s is now a contact", msg.Username))
	// First-ever contact is implicitly active without going through
	// setActive — trigger its history fetch explicitly.
	m.maybeLoadHistory()
	return m, nil
}

func (m model) handleContactDecline(msg incomingContactDeclineMsg) (tea.Model, tea.Cmd) {
	m.notice = fmt.Sprintf("%s declined your contact request", msg.Username)
	return m, nil
}

// handleIncomingMessage appends a relayed message. The sender's own
// copy is appended optimistically in handleSubmit instead.
func (m model) handleIncomingMessage(msg incomingMessageMsg) (tea.Model, tea.Cmd) {
	// The server always stamps sent_at in UTC; Parse preserves that
	// location on the result, and Format later just renders whatever
	// zone the value carries — without .Local() this would display in
	// UTC instead of the viewer's own zone, unlike the sender's own
	// optimistic copy (a bare time.Now(), already local — see
	// sendChatMessage), which is what made this show up as two
	// different times for the same message on either end.
	sentAt, err := time.Parse(protocol.SentAtLayout, msg.SentAt)
	if err != nil {
		sentAt = time.Now()
	} else {
		sentAt = sentAt.Local()
	}
	m.appendMessage(msg.From, message{id: msg.ID, from: msg.From, body: msg.Body, sentAt: sentAt, fileID: msg.FileID, fileName: msg.FileName, fileSize: msg.FileSize})

	// Already open — ack read immediately rather than on next switch-away/back.
	if active, ok := m.activeContact(); ok && active.username == msg.From {
		m.markConversationRead(msg.From)
	}
	return m, nil
}

// handleMessageAck attaches the server id to the optimistic message
// this ack confirms, found by clientMsgID.
func (m model) handleMessageAck(msg incomingMessageAckMsg) (tea.Model, tea.Cmd) {
	for _, msgs := range m.messages {
		for i := range msgs {
			if msgs[i].mine && msgs[i].clientMsgID == msg.ClientMsgID {
				msgs[i].id = msg.ServerID
				return m, nil
			}
		}
	}
	return m, nil
}

func (m model) handleIncomingTyping(msg incomingTypingMsg) (tea.Model, tea.Cmd) {
	m.typing[msg.From] = msg.IsTyping
	return m, nil
}

// handleHistoryResponse applies a history page to the contact named by
// conversation_id. See maybeLoadHistory/mergeInitialHistory/prependHistory.
func (m model) handleHistoryResponse(msg incomingHistoryResponseMsg) (tea.Model, tea.Cmd) {
	idx := -1
	for i, c := range m.contacts {
		if c.conversationID == msg.ConversationID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return m, nil // no longer (or never) a known contact — ignore
	}
	contactUsername := m.contacts[idx].username
	wasInitialLoad := !m.contacts[idx].historyLoaded

	m.contacts[idx].historyLoading = false
	m.contacts[idx].historyLoaded = true
	if len(msg.Messages) < historyPageSize {
		// Fewer than a full page means there's nothing older left.
		m.contacts[idx].historyExhausted = true
	}

	if len(msg.Messages) == 0 {
		return m, nil
	}

	// Reverse into chronological order (server sends newest-to-oldest).
	entries := make([]message, len(msg.Messages))
	for i, hm := range msg.Messages {
		// See handleIncomingMessage's comment — same UTC-vs-local fix.
		sentAt, err := time.Parse(protocol.SentAtLayout, hm.SentAt)
		if err != nil {
			sentAt = time.Now()
		} else {
			sentAt = sentAt.Local()
		}
		mine := hm.From == m.me
		tick := tickSent
		if hm.Read {
			tick = tickRead
		} else if hm.Delivered {
			tick = tickDelivered
		}
		entries[len(msg.Messages)-1-i] = message{
			id:     hm.ID,
			from:   hm.From,
			body:   hm.Body,
			sentAt: sentAt,
			mine:   mine,
			tick:   tick,
			// Already read server-side — nothing to (re)send for it, and
			// markConversationRead/resendPendingReadAcks only touch
			// entries where readAckSent is still false.
			readLocally: mine || hm.Read,
			readAckSent: mine || hm.Read,
			fileID:      hm.FileID,
			fileName:    hm.FileName,
			fileSize:    hm.FileSize,
		}
	}

	if wasInitialLoad {
		// Can overlap with messages already flushed live before this
		// conversation was opened — merge, don't assume no overlap.
		m.messages[contactUsername] = mergeInitialHistory(m.messages[contactUsername], entries)
		if active, ok := m.activeContact(); ok && active.username == contactUsername {
			m.refreshViewport() // jump to the bottom — the normal, expected place to open a conversation
		}
	} else {
		// Strictly older than everything loaded — no overlap possible.
		m.prependHistory(contactUsername, entries)
	}

	// Newly-revealed history might include messages we haven't
	// acknowledged as read yet, if this conversation is already open.
	if active, ok := m.activeContact(); ok && active.username == contactUsername {
		m.markConversationRead(contactUsername)
	}

	return m, nil
}

func (m model) handleIncomingPresence(msg incomingPresenceMsg) (tea.Model, tea.Cmd) {
	for i := range m.contacts {
		if m.contacts[i].username == msg.Username {
			m.contacts[i].online = msg.Status == "online"
			break
		}
	}
	if msg.Status != "online" {
		// A mid-burst disconnect never sends its own is_typing=false —
		// clear it here or "typing…" sticks until they type again.
		delete(m.typing, msg.Username)
	}
	return m, nil
}
