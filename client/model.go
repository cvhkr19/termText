package main

import (
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gorilla/websocket"

	"termtext/internal/protocol"
)

// tickState mirrors the ✓/✓✓/✓✓(read) progression via ack_delivered/
// ack_read. Meaningful only when mine is true.
type tickState int

const (
	tickSent tickState = iota
	tickDelivered
	tickRead
)

// message is one line of chat history. id is 0 until message_ack
// assigns the real server id (see clientMsgID, handleMessageAck).
// fileID == "" means plain text (see isFile).
//
// readLocally/readAckSent are deliberately two separate flags, not one:
// readLocally is set the moment the user views the message (see
// markConversationRead), regardless of whether telling the server about
// it actually succeeds; readAckSent is set only once that ack_read has
// actually been handed to a live connection. A message can be readLocally
// with readAckSent still false — read while offline, or the send
// otherwise failed — and resendPendingReadAcks (called on reconnect,
// see handleConnected) is what catches those up, the same way
// flushOfflineMessages catches up a delivery that failed the other way.
type message struct {
	id          int64
	clientMsgID int64
	from        string
	body        string
	sentAt      time.Time
	mine        bool
	tick        tickState
	readLocally bool
	readAckSent bool
	system      bool // local command-bar / notice-style lines, rendered distinctly

	fileID   string
	fileName string
	fileSize int64
}

func (msg message) isFile() bool { return msg.fileID != "" }

// contact is one sidebar entry from contact_list, kept live by
// contact_accept/presence. history* tracks per-conversation pagination
// state — see maybeLoadHistory/maybeLoadMoreHistory.
type contact struct {
	username       string
	conversationID int64
	online         bool
	unread         int

	historyLoaded    bool
	historyLoading   bool
	historyExhausted bool
}

// screen is which top-level view is showing.
type screen int

const (
	screenAuth screen = iota
	screenChat
)

// pane is which of the three chat-screen panes has keyboard focus.
type pane int

const (
	paneContacts pane = iota
	paneChat
	paneCompose
)

// model is the Bubble Tea root model.
type model struct {
	screen screen
	auth   authModel

	server     endpoint // where the server is, and whether to reach it over TLS — see config.go
	token      string
	me         string // my own username, learned from the auth form / saved config
	configPath string // where a successful login/register's token gets saved

	// registrationCode is sent with /register only, for servers started
	// with -registration-code/REGISTRATION_CODE. Set once from a client
	// flag in main.go — never persisted to config.json, since it's the
	// server operator's invite code, not this user's own secret.
	registrationCode string

	conn    *websocket.Conn
	outbox  chan protocol.Envelope
	send    func(tea.Msg) // == (*tea.Program).Send, injected from main so goroutines can feed the event loop
	connErr string        // set on wsDisconnectedMsg/wsConnectErrMsg, shown in the chat screen

	// Reconnect/backoff state — see events.go. reconnectGen invalidates
	// a stale scheduled retry the same way typingGen invalidates a
	// stale idle timer.
	reconnecting     bool
	reconnectAttempt int
	reconnectGen     int

	contacts []contact
	active   int // index into contacts of the open conversation
	messages map[string][]message
	typing   map[string]bool
	notice   string // transient status line: command feedback, incoming contact_request, server errors

	// pendingRequests is every username with an outstanding incoming
	// contact request, not yet accepted or declined — see
	// addPendingRequest/removePendingRequest/pendingRequestsNotice in
	// events.go. Tracked as its own list rather than shown only via the
	// single-slot notice above, so a second incoming request (or any
	// other notice-worthy event) can't silently erase the first from
	// view before it's been acted on.
	pendingRequests []string

	// nextClientMsgID is pre-incremented — 0 means "no client_msg_id".
	nextClientMsgID int64

	// Typing-indicator debounce — see handleTypingKeystroke/typingIdleCmd.
	typingActive bool
	typingConvID int64
	typingGen    int

	// lastPasteAt is when the terminal last reported a Paste-flagged key —
	// see duringOrJustAfterPaste.
	lastPasteAt time.Time

	viewport viewport.Model
	input    textarea.Model

	// focusedPane routes keystrokes — see cyclePane/paneAt/syncPaneFocus.
	focusedPane pane

	width, height           int
	sidebarWidth, mainWidth int
	ready                   bool // true once the first WindowSizeMsg has set real dimensions
}

// initialModel wires dependencies that can't be package-level state —
// send feeds events back from goroutines; server/cfg pick the first screen.
func initialModel(send func(tea.Msg), cfg config, server endpoint, configPath string) model {
	ta := textarea.New()
	ta.Placeholder = "message, or /add <username>"
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 2000
	// Enter is intercepted for send elsewhere; rebind InsertNewline to
	// alt+enter/ctrl+j. These stay bound as the portable newline keys —
	// they're the only ones that work everywhere, including off Windows
	// (see shiftkey_other.go). Shift+Enter is handled separately and does
	// not go through this binding: bubbletea v1.3.10 discards the Shift
	// modifier for Enter, so it can't be expressed as a key binding at
	// all, and updateChat queries the live physical key state instead —
	// see shiftHeld.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))

	// Don't highlight the current line — the bordered box is framing enough.
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()

	vp := viewport.New(0, 0)
	// Replace viewport's vim-style KeyMap wholesale — f/b/u/d/j/k/space
	// would otherwise fire while typing a message.
	vp.KeyMap = viewport.KeyMap{
		Up:       key.NewBinding(key.WithKeys("up")),
		Down:     key.NewBinding(key.WithKeys("down")),
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
	}

	m := model{
		screen:      screenAuth,
		auth:        newAuthModel(server.schemeString()),
		server:      server,
		configPath:  configPath,
		send:        send,
		messages:    map[string][]message{},
		typing:      map[string]bool{},
		input:       ta,
		viewport:    vp,
		focusedPane: paneCompose,
	}

	if cfg.Token != "" && cfg.Username != "" {
		m.token = cfg.Token
		m.me = cfg.Username
		m.auth.errMsg = "connecting…"
	}
	return m
}

func (m model) Init() tea.Cmd {
	if m.token != "" {
		return wsConnect(m.server, m.token)
	}
	return textinput.Blink
}

// activeContact is the open conversation, or ok=false if there are no
// contacts yet.
func (m model) activeContact() (c contact, ok bool) {
	if m.active < 0 || m.active >= len(m.contacts) {
		return contact{}, false
	}
	return m.contacts[m.active], true
}

// trySend enqueues env without blocking, reporting whether it actually
// went into the outbox — drops (returns false) on a full or nil outbox.
// Callers that need the send to eventually happen even if this attempt
// fails (see sendReadAck/resendPendingReadAcks) rely on this return
// value rather than assuming a fire-and-forget call always lands.
func (m model) trySend(env protocol.Envelope) bool {
	if m.outbox == nil {
		return false
	}
	select {
	case m.outbox <- env:
		return true
	default:
		return false
	}
}
