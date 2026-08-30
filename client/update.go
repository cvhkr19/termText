package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"termtext/internal/protocol"
)

const (
	sidebarWidth     = 24
	focusGutterWidth = 1             // reserved column between sidebar and main pane, see renderFocusGutter
	inputRows        = 3             // visible textarea rows, before border
	inputHeight      = inputRows + 2 // bordered box: top border + inputRows + bottom border
	statusHeight     = 1             // always reserved, even when blank, so layout doesn't jump
	footerHeight     = 1

	// typingIdleTimeout: how long idle before telling the server we stopped typing.
	typingIdleTimeout = 3 * time.Second

	// pasteEnterGracePeriod: an Enter this soon after paste activity is
	// treated as a literal newline, never a send. It exists only to catch
	// a trailing newline the *terminal* appends once the pasted block has
	// finished arriving — which lands within a few ms of it, since it's
	// part of the same injected sequence.
	//
	// Was 1500ms, sized for uncertainty about how long a terminal might
	// take to settle. That's long enough to feel broken: paste a
	// paragraph, press Enter to send, and nothing happens. The backlog
	// check (see inputBacklog) now covers the burst itself precisely, so
	// this only has to span the gap between the queue draining and a
	// trailing newline landing. Short enough that a deliberate Enter —
	// which needs human reaction time first — is never caught.
	pasteEnterGracePeriod = 300 * time.Millisecond

	// pasteBacklogThreshold: how many input events must already be queued
	// behind a keystroke before it's treated as machine-injected (see
	// inputBacklog). Not zero, because a keypress produces both a
	// key-down and a key-up record and the matching key-up can still be
	// in the queue while its key-down is handled — so a small backlog is
	// normal even for genuine typing. A pasted block queues far more than
	// this. Erring slightly high risks missing a 1-2 character paste;
	// erring low would swallow real Enters, so this sits just above the
	// key-up noise floor.
	pasteBacklogThreshold = 4

	// historyPageSize is the page size for every history_request.
	historyPageSize = 50
)

// titleBarHeight is how many rows renderTitleBar returns: 3 on Windows
// (see its comment), 2 elsewhere.
var titleBarHeight = func() int {
	if runtime.GOOS == "windows" {
		return 3
	}
	return 2
}()

// typingIdleMsg fires typingIdleTimeout after the keystroke that
// scheduled it. gen guards against a superseded timer.
type typingIdleMsg struct {
	conversationID int64
	gen            int
}

func typingIdleCmd(conversationID int64, gen int) tea.Cmd {
	return tea.Tick(typingIdleTimeout, func(time.Time) tea.Msg {
		return typingIdleMsg{conversationID: conversationID, gen: gen}
	})
}

// Update is the Bubble Tea entry point. Connection/WebSocket events are
// handled here regardless of screen; everything else routes by screen.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Windows can report the console's scroll-buffer height (not the
		// visible window) as a spontaneous resize — treat "same width,
		// height blew up" as that bug, not a real resize, or layout
		// blows up for rows that aren't on screen.
		if m.ready && msg.Width == m.width && m.height > 0 && msg.Height > m.height*3 {
			return m, nil
		}
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.ready = true
		// Force a full repaint — bubbletea's incremental diff can carry
		// stale state across a layout change this size.
		return m, tea.ClearScreen

	case wsConnectedMsg:
		return m.handleConnected(msg)
	case wsConnectErrMsg:
		return m.handleConnectErr(msg)
	case wsDisconnectedMsg:
		return m.handleDisconnected(msg)
	case reconnectMsg:
		return m.handleReconnectTick(msg)

	case incomingContactListMsg:
		return m.handleContactList(msg)
	case incomingContactRequestMsg:
		return m.handleContactRequest(msg)
	case incomingContactAcceptMsg:
		return m.handleContactAccept(msg)
	case incomingContactDeclineMsg:
		return m.handleContactDecline(msg)
	case incomingMessageMsg:
		return m.handleIncomingMessage(msg)
	case incomingMessageAckMsg:
		return m.handleMessageAck(msg)
	case incomingTypingMsg:
		return m.handleIncomingTyping(msg)
	case incomingPresenceMsg:
		return m.handleIncomingPresence(msg)
	case incomingErrorMsg:
		m.notice = "error: " + msg.Message
		return m, nil
	case incomingAckDeliveredMsg:
		m.markTick(msg.MessageID, tickDelivered)
		return m, nil
	case incomingAckReadMsg:
		m.markTick(msg.MessageID, tickRead)
		return m, nil
	case incomingHistoryResponseMsg:
		return m.handleHistoryResponse(msg)
	case fileUploadedMsg:
		return m.handleFileUploaded(msg)
	case fileDownloadedMsg:
		return m.handleFileDownloaded(msg)
	}

	if m.screen == screenAuth {
		return m.updateAuth(msg)
	}
	return m.updateChat(msg)
}

func (m model) updateChat(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	// Two independent signals arm the paste window, because neither
	// covers both of bubbletea's input paths:
	//
	//   - msg.Paste is the terminal's own bracketed-paste report. It's
	//     precise, but bubbletea only ever sets it in its ANSI
	//     byte-stream parser — the Windows Console path builds KeyMsgs
	//     without it, so on Windows it is always false.
	//   - inputBacklog covers exactly that gap: a pasted block lands in
	//     the console input buffer all at once and is drained 16 events
	//     at a time, so input still queued behind this key means the key
	//     was machine-injected, not typed.
	//
	// Note this is not the inter-keystroke timing heuristic that used to
	// live here — that one measured how fast Update() *processed* keys,
	// which a slow render or GC pause could stretch until ordinary typing
	// looked like a burst. Queue depth isn't affected by how long we take
	// to react to it.
	case tea.KeyMsg:
		if msg.Paste || inputBacklog() >= pasteBacklogThreshold {
			m.lastPasteAt = time.Now()
		}

		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit

		case "tab":
			m.cyclePane()
			return m, nil

		case "up", "down", "pgup", "pgdown":
			return m.routeNavigationKey(msg)

		// "o" downloads the focused file, but only with the chat pane
		// focused — otherwise fall through and type the letter.
		case "o":
			if m.focusedPane == paneChat {
				return m.downloadFocusedFile()
			}

		case "enter":
			// Shift+Enter inserts a newline instead of sending. This
			// can't be a `case "shift+enter"` above: bubbletea drops the
			// Shift modifier for Enter before this switch ever runs, so
			// the live physical key state is asked for directly instead
			// — see shiftHeld. Checked before the paste window below
			// because it's an explicit, deliberate modifier press, which
			// outranks any inference about what's arriving.
			if shiftHeld() {
				m.input.InsertRune('\n')
				return m, nil
			}
			if m.duringOrJustAfterPaste() {
				// A literal newline, never a send. InsertNewline no
				// longer binds "enter" (see initialModel), so this
				// applies it directly.
				//
				// Deliberately does NOT push lastPasteAt forward. It used
				// to, to cover paste chunks still arriving — but that made
				// the window self-renewing, so someone pressing Enter
				// again because nothing happened just kept extending their
				// own block, and it never broke through. The extension is
				// redundant now anyway: genuine paste activity re-arms the
				// window on its own via the Paste flag / backlog check
				// above, which runs for every key including this Enter.
				m.input.InsertRune('\n')
				return m, nil
			}
			return m.handleSubmit()
		}

	case typingIdleMsg:
		if msg.gen == m.typingGen && m.typingActive && m.typingConvID == msg.conversationID {
			m.sendTyping(msg.conversationID, false)
			m.typingActive = false
		}
		return m, nil
	}

	if !m.ready {
		return m, nil
	}

	var cmds []tea.Cmd
	var cmd tea.Cmd
	if mouseMsg, isMouse := msg.(tea.MouseMsg); isMouse {
		// A left click also sets focus directly, alongside Tab-cycling.
		if mouseMsg.Action == tea.MouseActionPress && mouseMsg.Button == tea.MouseButtonLeft {
			if p, ok := m.paneAt(mouseMsg.X, mouseMsg.Y); ok {
				m.focusedPane = p
				m.syncPaneFocus()
			}
		}
		// Wheel scroll targets chat history only — never the compose box.
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	} else {
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
	}

	// Gate on compose focus — otherwise browsing keys re-arm typing
	// from a stale draft.
	if _, isKey := msg.(tea.KeyMsg); isKey && m.focusedPane == paneCompose {
		cmds = append(cmds, m.handleTypingKeystroke())
	}

	m.maybeLoadMoreHistory()

	return m, tea.Batch(cmds...)
}

// duringOrJustAfterPaste reports whether an Enter right now should be
// a literal newline instead of a send — true during a paste and for
// pasteEnterGracePeriod after it.
func (m model) duringOrJustAfterPaste() bool {
	return time.Since(m.lastPasteAt) < pasteEnterGracePeriod
}

// layout recomputes every size-dependent field from the current terminal
// dimensions. Called on every WindowSizeMsg (including resizes, not just
// startup).
func (m *model) layout() {
	m.sidebarWidth = sidebarWidth
	// Reserve the sidebar's border column and the focus gutter, or
	// sidebarWidth+mainWidth overflows the terminal.
	m.mainWidth = m.width - m.sidebarWidth - 1 - focusGutterWidth
	if m.mainWidth < 20 {
		m.mainWidth = 20
	}

	viewportHeight := m.height - titleBarHeight - inputHeight - statusHeight - footerHeight
	if viewportHeight < 3 {
		viewportHeight = 3
	}

	m.viewport.Width = m.mainWidth
	m.viewport.Height = viewportHeight

	// inputBoxStyle.Width includes its own padding; textarea's SetWidth
	// is text-only (Prompt == "" needs no further reservation).
	innerWidth := m.mainWidth - 2 /* border */ - 2 /* padding */
	if innerWidth < 1 {
		innerWidth = 1
	}
	m.input.SetWidth(innerWidth)
	m.input.SetHeight(inputRows)

	m.refreshViewport()
}

// cyclePane advances focus forward one step: Contacts → Chat → Compose →
// Contacts. Forward-only, per the 3-pane design — no reverse binding is
// needed when the cycle is this short.
func (m *model) cyclePane() {
	switch m.focusedPane {
	case paneContacts:
		m.focusedPane = paneChat
	case paneChat:
		m.focusedPane = paneCompose
	default: // paneCompose
		m.focusedPane = paneContacts
	}
	m.syncPaneFocus()
}

// syncPaneFocus syncs the textarea's Focus/Blur with focusedPane — the
// only pane with a real cursor.
func (m *model) syncPaneFocus() {
	if m.focusedPane == paneCompose {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

// routeNavigationKey sends up/down/pgup/pgdown to whichever component
// focusedPane owns right now.
func (m model) routeNavigationKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.focusedPane {
	case paneContacts:
		switch msg.String() {
		case "up":
			m.setActive(m.active - 1)
		case "down":
			m.setActive(m.active + 1)
		}
		return m, nil

	case paneChat:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		m.maybeLoadMoreHistory()
		return m, cmd

	default: // paneCompose
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
}

// paneAt maps a click's screen coordinates to the pane under it,
// mirroring layout()'s geometry.
func (m model) paneAt(x, y int) (p pane, ok bool) {
	if y < titleBarHeight {
		return 0, false
	}
	y -= titleBarHeight // below here, y is relative to the body's own top row

	if x < m.sidebarWidth+1 {
		return paneContacts, true
	}
	if x < m.sidebarWidth+1+focusGutterWidth {
		return 0, false
	}

	composeStart := m.viewport.Height + statusHeight
	switch {
	case y < composeStart:
		return paneChat, true
	case y < m.mainHeight():
		return paneCompose, true
	default:
		return 0, false
	}
}

// setActive switches the open conversation, clearing its unread badge
// and acking any unread messages. No-op with zero contacts.
func (m *model) setActive(i int) {
	if len(m.contacts) == 0 {
		return
	}
	if m.typingActive {
		// Switching away mid-keystroke should stop showing "typing…" on
		// the other end immediately, not linger until the idle timeout.
		m.sendTyping(m.typingConvID, false)
		m.typingActive = false
	}

	if i < 0 {
		i = 0
	}
	if i >= len(m.contacts) {
		i = len(m.contacts) - 1
	}
	m.active = i
	m.contacts[m.active].unread = 0
	m.notice = ""
	m.markConversationRead(m.contacts[m.active].username)
	m.maybeLoadHistory()
	m.refreshViewport()
}

// markConversationRead acks every unread message from contactUsername.
// readAckSent (not readLocally) is what's checked to skip a message: it
// keeps repeated calls idempotent once the ack has actually gone out, but
// still retries a message that was already viewed if its ack never
// successfully sent last time — see the message struct's own comment,
// and resendPendingReadAcks for the reconnect-triggered version of the
// same retry.
func (m *model) markConversationRead(contactUsername string) {
	msgs := m.messages[contactUsername]
	for i := range msgs {
		if msgs[i].mine || msgs[i].readAckSent || msgs[i].id == 0 {
			continue
		}
		msgs[i].readLocally = true
		if m.sendReadAck(msgs[i].id) {
			msgs[i].readAckSent = true
		}
	}
}

// resendPendingReadAcks retries ack_read for every message already
// viewed (readLocally) whose ack never actually reached a live
// connection (readAckSent still false) — read while disconnected, or
// sent right as the outbox was torn down mid-drop. Called once a fresh
// connection is up (see handleConnected), so a message read during an
// outage isn't silently lost the way markConversationRead's own
// idempotency check would otherwise have hidden it: this is the
// client-to-server mirror of flushOfflineMessages.
func (m *model) resendPendingReadAcks() {
	for _, msgs := range m.messages {
		for i := range msgs {
			if msgs[i].mine || !msgs[i].readLocally || msgs[i].readAckSent {
				continue
			}
			if m.sendReadAck(msgs[i].id) {
				msgs[i].readAckSent = true
			}
		}
	}
}

func (m *model) refreshViewport() {
	m.viewport.SetContent(m.renderConversation())
	m.viewport.GotoBottom()
}

// maybeLoadHistory fetches the most recent page the first time a
// conversation opens this session.
func (m *model) maybeLoadHistory() {
	c, ok := m.activeContact()
	if !ok || m.contacts[m.active].historyLoaded || m.contacts[m.active].historyLoading {
		return
	}
	m.requestHistory(c.conversationID, "")
	m.contacts[m.active].historyLoading = true
}

// maybeLoadMoreHistory fetches an older page once scrolled to the top.
// historyLoading/historyExhausted guard against duplicate/pointless requests.
func (m *model) maybeLoadMoreHistory() {
	if !m.viewport.AtTop() {
		return
	}
	c, ok := m.activeContact()
	if !ok {
		return
	}
	idx := m.active
	if !m.contacts[idx].historyLoaded || m.contacts[idx].historyLoading || m.contacts[idx].historyExhausted {
		return
	}
	msgs := m.messages[c.username]
	if len(msgs) == 0 {
		return
	}
	oldest := msgs[0]
	// protocol.SentAtLayout, not RFC3339Nano — see that constant's comment.
	m.requestHistory(c.conversationID, oldest.sentAt.Format(protocol.SentAtLayout))
	m.contacts[idx].historyLoading = true
}

func (m model) requestHistory(conversationID int64, before string) {
	env, err := protocol.Encode(protocol.TypeHistoryRequest, protocol.HistoryRequestPayload{
		ConversationID: conversationID,
		Before:         before,
		Limit:          historyPageSize,
	})
	if err == nil {
		m.trySend(env)
	}
}

// prependHistory adds an older page in front of existing messages —
// safe unconditionally, since a "before" cursor is always strictly older.
func (m *model) prependHistory(contactUsername string, older []message) {
	preserveScroll := false
	var oldLines, oldOffset int
	if active, ok := m.activeContact(); ok && active.username == contactUsername {
		preserveScroll = true
		oldLines = m.viewport.TotalLineCount()
		oldOffset = m.viewport.YOffset
	}

	m.messages[contactUsername] = append(older, m.messages[contactUsername]...)

	if preserveScroll {
		m.viewport.SetContent(m.renderConversation())
		newLines := m.viewport.TotalLineCount()
		m.viewport.SetYOffset(oldOffset + (newLines - oldLines))
	}
}

// mergeInitialHistory merges a conversation's first history page with
// whatever's already live-tracked (e.g. from the offline queue),
// de-duped by server id with the live copy winning on conflict.
func mergeInitialHistory(existing, history []message) []message {
	byID := make(map[int64]message, len(existing)+len(history))
	noID := make([]message, 0)

	for _, msg := range history {
		if msg.id == 0 {
			noID = append(noID, msg)
			continue
		}
		byID[msg.id] = msg
	}
	for _, msg := range existing {
		if msg.id == 0 {
			noID = append(noID, msg)
			continue
		}
		byID[msg.id] = msg // existing wins over history on conflict
	}

	merged := make([]message, 0, len(byID)+len(noID))
	for _, msg := range byID {
		merged = append(merged, msg)
	}
	merged = append(merged, noID...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].sentAt.Before(merged[j].sentAt) })
	return merged
}

// handleSubmit acts on the input box: a leading "/" is a command,
// anything else sends to the open conversation.
func (m model) handleSubmit() (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(m.input.Value())
	if value == "" {
		return m, nil
	}
	m.input.Reset()

	if strings.HasPrefix(value, "/") {
		return m.runCommand(value)
	}

	m.sendChatMessage(value, "", "", 0)
	return m, nil
}

// sendChatMessage sends a message envelope and appends the same
// optimistic local copy for a typed message or a file (see runSendFile).
func (m *model) sendChatMessage(body, fileID, fileName string, fileSize int64) {
	c, ok := m.activeContact()
	if !ok {
		m.notice = "no contacts yet — /add <username> to add one"
		return
	}

	if m.typingActive && m.typingConvID == c.conversationID {
		// Sending the message ends the typing burst — no need to wait
		// for the idle timeout to tell the other side.
		m.sendTyping(c.conversationID, false)
		m.typingActive = false
	}

	m.nextClientMsgID++
	clientMsgID := m.nextClientMsgID

	env, err := protocol.Encode(protocol.TypeMessage, protocol.MessagePayload{
		ConversationID: c.conversationID,
		ClientMsgID:    clientMsgID,
		Body:           body,
		FileID:         fileID,
		FileName:       fileName,
		FileSize:       fileSize,
	})
	if err == nil {
		m.trySend(env)
	}

	// clientMsgID lets handleMessageAck (events.go) find this message
	// once the server confirms it and attach the real id.
	m.appendMessage(c.username, message{
		clientMsgID: clientMsgID, from: m.me, body: body, sentAt: time.Now(), mine: true, tick: tickSent,
		fileID: fileID, fileName: fileName, fileSize: fileSize,
	})
}

// appendMessage appends msg, de-duped by server id (skipping id==0,
// every optimistic message's starting value) and bumps the unread
// badge for an incoming message to a conversation that isn't open.
// Dedup matters because history_request can race flushOfflineMessages
// over the same connection — either can arrive first.
func (m *model) appendMessage(contactUsername string, msg message) {
	if msg.id != 0 {
		for _, existing := range m.messages[contactUsername] {
			if existing.id == msg.id {
				return
			}
		}
	}

	m.messages[contactUsername] = append(m.messages[contactUsername], msg)

	if !msg.mine {
		active, ok := m.activeContact()
		if !ok || active.username != contactUsername {
			for i := range m.contacts {
				if m.contacts[i].username == contactUsername {
					m.contacts[i].unread++
					break
				}
			}
		}
	}

	m.refreshViewport()
}

// markTick applies a delivery/read ack, found by exact server id.
// Relies on handleMessageAck having already attached that id —
// guaranteed because message_ack always precedes ack_delivered/ack_read
// on the same connection's single-writer outbox.
func (m *model) markTick(messageID int64, tick tickState) {
	for _, msgs := range m.messages {
		for i := range msgs {
			if msgs[i].mine && msgs[i].id == messageID {
				msgs[i].tick = tick
				m.refreshViewport()
				return
			}
		}
	}
}

// handleTypingKeystroke sends is_typing=true on burst start, false when
// emptied, and reschedules the idle timer otherwise.
func (m *model) handleTypingKeystroke() tea.Cmd {
	c, ok := m.activeContact()
	if !ok {
		return nil
	}

	if m.input.Value() == "" {
		if m.typingActive && m.typingConvID == c.conversationID {
			m.sendTyping(c.conversationID, false)
			m.typingActive = false
		}
		return nil
	}

	if !m.typingActive || m.typingConvID != c.conversationID {
		m.sendTyping(c.conversationID, true)
		m.typingActive = true
		m.typingConvID = c.conversationID
	}

	m.typingGen++
	return typingIdleCmd(c.conversationID, m.typingGen)
}

func (m model) sendTyping(conversationID int64, isTyping bool) {
	env, err := protocol.Encode(protocol.TypeTyping, protocol.TypingPayload{
		ConversationID: conversationID,
		IsTyping:       isTyping,
	})
	if err == nil {
		m.trySend(env)
	}
}

// sendReadAck reports whether the envelope actually reached the outbox —
// callers that need to retry a failed attempt (see markConversationRead,
// resendPendingReadAcks) depend on this rather than assuming it landed.
func (m model) sendReadAck(messageID int64) bool {
	env, err := protocol.Encode(protocol.TypeAckRead, protocol.AckPayload{MessageID: messageID})
	if err != nil {
		return false
	}
	return m.trySend(env)
}

// runCommand sends the envelope for /add, /accept, /decline, starts an
// upload for /send, or drops back to the login screen for /logout.
func (m model) runCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	cmd := fields[0]
	var arg string
	if len(fields) > 1 {
		arg = fields[1]
	}

	if cmd == "/send" {
		// Not fields[1]: a local filepath can contain spaces, which
		// strings.Fields would have already split apart.
		return m.runSendFile(strings.TrimSpace(strings.TrimPrefix(input, cmd)))
	}

	if cmd == "/logout" {
		return m.doLogout()
	}

	var envType protocol.EnvelopeType
	switch cmd {
	case "/add":
		envType = protocol.TypeContactRequest
	case "/accept":
		envType = protocol.TypeContactAccept
	case "/decline":
		envType = protocol.TypeContactDecline
	default:
		m.notice = "unknown command: " + cmd
		return m, nil
	}

	if arg == "" {
		m.notice = fmt.Sprintf("usage: %s <username>", cmd)
		return m, nil
	}

	env, err := protocol.Encode(envType, protocol.ContactPayload{Username: arg})
	if err != nil {
		return m, nil
	}
	m.trySend(env)

	if cmd == "/decline" {
		// /decline gets no server confirmation (only the requester is
		// told) — resolve it locally and show it done immediately.
		m.removePendingRequest(arg)
		m.notice = m.noticeWithPendingRequests(fmt.Sprintf("declined %s", arg))
	} else {
		m.notice = fmt.Sprintf("%s %s…", strings.TrimPrefix(cmd, "/"), arg)
	}
	return m, nil
}

// doLogout drops back to a fresh login screen — e.g. to switch to a
// different server, or a different account on this one. Rebuilds the
// model via initialModel rather than hand-clearing each field, so
// contacts/messages/pending-requests all reset the same way a real
// fresh launch would, with no stale state from the old session left
// behind. The server field on the new login screen still prefills with
// the server just left, purely as a convenience — nothing here assumes
// they're staying on it.
func (m model) doLogout() (tea.Model, tea.Cmd) {
	if m.conn != nil {
		m.conn.Close()
	}
	logout := logoutCmd(m.server, m.token)

	cfg := config{ServerAddr: m.server.host, TLS: m.server.secure}
	cfg.save(m.configPath) // best-effort — a failed write here just means the next launch re-prompts for login too, not a data loss

	newM := initialModel(m.send, cfg, m.server, m.configPath)
	newM.registrationCode = m.registrationCode
	// initialModel has no way to know the terminal size — that's only
	// ever delivered via a real tea.WindowSizeMsg, which Bubble Tea
	// fires once at startup and on actual resizes, neither of which
	// happens just because Update() returned a different model. Without
	// carrying it over, the new screen renders at 0x0 (see viewAuth's
	// fallback) and stays !ready — frozen — until the user happens to
	// resize the window.
	newM.width, newM.height = m.width, m.height
	newM.layout()
	newM.ready = m.ready
	return newM, tea.Batch(logout, tea.ClearScreen)
}

// runSendFile stats and size-checks path locally, then uploads as a
// tea.Cmd. The local size check is a fast-fail heuristic — the server's
// own configured cap is the real limit.
func (m model) runSendFile(path string) (tea.Model, tea.Cmd) {
	if path == "" {
		m.notice = "usage: /send <local filepath>"
		return m, nil
	}
	c, ok := m.activeContact()
	if !ok {
		m.notice = "no contacts yet — /add <username> to add one"
		return m, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		m.notice = fmt.Sprintf("/send: %v", err)
		return m, nil
	}
	if info.IsDir() {
		m.notice = fmt.Sprintf("/send: %s is a directory", path)
		return m, nil
	}
	if info.Size() > protocol.MaxUploadBytes {
		m.notice = fmt.Sprintf("/send: %s is over the %s limit", filepath.Base(path), humanizeBytes(protocol.MaxUploadBytes))
		return m, nil
	}

	m.notice = fmt.Sprintf("uploading %s…", filepath.Base(path))
	return m, uploadFileCmd(m.server, m.token, c.conversationID, path)
}

// handleFileUploaded hands a successful upload to sendChatMessage, the
// same path a typed message takes.
func (m model) handleFileUploaded(msg fileUploadedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.notice = fmt.Sprintf("upload failed: %v", msg.err)
		return m, nil
	}
	m.notice = ""
	m.sendChatMessage("", msg.fileID, msg.fileName, msg.fileSize)
	return m, nil
}

// downloadFocusedFile downloads the most recent file message in the
// active conversation.
func (m model) downloadFocusedFile() (tea.Model, tea.Cmd) {
	c, ok := m.activeContact()
	if !ok {
		return m, nil
	}
	msgs := m.messages[c.username]
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].isFile() {
			m.notice = fmt.Sprintf("downloading %s…", msgs[i].fileName)
			return m, downloadFileCmd(m.server, m.token, msgs[i].fileID, msgs[i].fileName)
		}
	}
	m.notice = "no file in this conversation to download"
	return m, nil
}

// handleFileDownloaded reports the saved path — nothing is launched,
// see downloadFileCmd.
func (m model) handleFileDownloaded(msg fileDownloadedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.notice = fmt.Sprintf("download failed: %v", msg.err)
		return m, nil
	}
	m.notice = fmt.Sprintf("saved to %s", msg.path)
	return m, nil
}
