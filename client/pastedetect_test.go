package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// newReadyModel builds a real, fully-initialized model (same construction
// initialModel uses in production) sitting on the chat screen with an
// empty compose box focused, with one contact so handleSubmit's send path
// actually completes instead of bailing out on "no contacts yet" — that
// bail-out also calls input.Reset(), which would make "the input got
// cleared" ambiguous between "sent" and "inserted a newline".
func newReadyModel(t *testing.T) model {
	t.Helper()
	// Enter's meaning depends on live OS state — physical Shift position
	// and console input-queue depth (see shiftHeld/inputBacklog) — both
	// real queries on Windows. Pin them to the "plain typing, nothing
	// queued, no modifier" baseline so these tests can't be flipped by
	// whatever the machine running them happens to be doing; tests that
	// want the other paths opt in explicitly.
	stubShiftHeld(t, false)
	stubInputBacklog(t, 0)

	m := initialModel(func(tea.Msg) {}, config{}, endpoint{host: "localhost:8080"}, "")
	m.screen = screenChat
	m.ready = true
	m.focusedPane = paneCompose
	m.me = "alice"
	m.contacts = []contact{{username: "bob", conversationID: 1}}
	m.messages = map[string][]message{}
	m.input.Focus()
	return m
}

// stubShiftHeld makes the physical-Shift check deterministic for one test,
// restoring the real implementation afterwards.
func stubShiftHeld(t *testing.T, held bool) {
	t.Helper()
	prev := shiftHeld
	shiftHeld = func() bool { return held }
	t.Cleanup(func() { shiftHeld = prev })
}

// stubInputBacklog fixes the console input-queue depth for one test,
// restoring the real implementation afterwards. A value at or above
// pasteBacklogThreshold stands in for "this key was machine-injected",
// which is how a paste is recognized on Windows — see inputBacklog.
func stubInputBacklog(t *testing.T, depth int) {
	t.Helper()
	prev := inputBacklog
	inputBacklog = func() int { return depth }
	t.Cleanup(func() { inputBacklog = prev })
}

func pressKey(m model, msg tea.KeyMsg) model {
	newM, _ := m.updateChat(msg)
	return newM.(model)
}

func runeMsg(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// Ordinary typing — however fast it's delivered — must never swallow
// Enter as a newline. An earlier version tried to infer a paste from
// keys arriving suspiciously close together, but that measured the gap
// between when Update() *processed* each key, not when it was actually
// pressed: any processing hiccup (a slow render, GC, the OS delivering a
// backlog of already-typed keys at once) could make ordinarily-paced
// typing register as a "burst" and silently turn a plain Enter into an
// inserted newline instead of a send. Only the terminal's own Paste flag
// is trusted now — see the tea.KeyMsg case in updateChat.
func TestFastTypingNeverSwallowsEnter(t *testing.T) {
	m := newReadyModel(t)

	for _, r := range "abcdefgh" { // as fast as Go can dispatch them — no gap at all
		m = pressKey(m, runeMsg(r))
	}

	draft := m.input.Value()
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.messages["bob"]) != 1 || m.messages["bob"][0].body != draft {
		t.Errorf("expected Enter to send %q, got messages %+v (input now %q)", draft, m.messages["bob"], m.input.Value())
	}
}

// The terminal's own Paste flag is a real, reported signal — it must
// swallow the next Enter, unconditionally.
func TestPasteFlagSwallowsEnter(t *testing.T) {
	m := newReadyModel(t)

	pasted := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}, Paste: true}
	m = pressKey(m, pasted)

	before := m.input.Value()
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if want := before + "\n"; m.input.Value() != want {
		t.Errorf("input = %q, want %q — Enter right after a Paste-flagged key should insert a newline", m.input.Value(), want)
	}
	if len(m.messages["bob"]) != 0 {
		t.Errorf("expected nothing sent, got messages %+v", m.messages["bob"])
	}
}

// Once the paste grace period elapses, Enter goes back to sending
// normally — the swallow window doesn't last forever.
func TestEnterSendsAgainAfterPasteWindowExpires(t *testing.T) {
	m := newReadyModel(t)
	m.lastPasteAt = m.lastPasteAt.Add(-pasteEnterGracePeriod * 2) // long past

	draft := "hi"
	for _, r := range draft {
		m = pressKey(m, runeMsg(r))
	}
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.messages["bob"]) != 1 || m.messages["bob"][0].body != draft {
		t.Errorf("expected Enter to send after the paste window expired, got messages %+v", m.messages["bob"])
	}
}

// Regression: pasting a multi-line paragraph must land in the compose box
// as one draft, not fire off a message per line.
//
// This broke on Windows specifically, and the reason is worth recording.
// Paste detection used to rest solely on msg.Paste — but bubbletea sets
// that flag in exactly one place, detectBracketedPaste, which lives in
// its ANSI byte-stream parser. On Windows with a real console it uses a
// completely different reader (the Console API path, readConInputs) that
// builds KeyMsg{Type, Runes, Alt} and never populates Paste at all. So
// msg.Paste is *always* false there, every embedded newline arrived as a
// plain KeyEnter, and each one sent. inputBacklog is what covers that
// path — see the tea.KeyMsg case in updateChat.
func TestMultiLinePasteDoesNotSendPerLine(t *testing.T) {
	m := newReadyModel(t)

	// A paste arrives as machine-injected keys with the rest of the block
	// still queued behind them — and crucially with Paste never set, as
	// on the Windows console path.
	stubInputBacklog(t, 200)

	for _, r := range "first line" {
		m = pressKey(m, runeMsg(r))
	}
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter}) // embedded newline
	for _, r := range "second line" {
		m = pressKey(m, runeMsg(r))
	}

	if len(m.messages["bob"]) != 0 {
		t.Fatalf("pasted paragraph sent %d message(s) instead of staying in the compose box: %+v",
			len(m.messages["bob"]), m.messages["bob"])
	}
	if want := "first line\nsecond line"; m.input.Value() != want {
		t.Errorf("input = %q, want %q", m.input.Value(), want)
	}
}

// The trailing newline many pastes end with lands after the queue has
// drained, so it can't rely on the backlog still being visible — the
// grace window opened during the paste is what has to cover it.
func TestTrailingNewlineOfPasteStillSwallowed(t *testing.T) {
	m := newReadyModel(t)

	stubInputBacklog(t, 200)
	for _, r := range "pasted text" {
		m = pressKey(m, runeMsg(r))
	}

	stubInputBacklog(t, 0) // queue drained; this Enter is the last of the block
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.messages["bob"]) != 0 {
		t.Errorf("trailing newline of a paste sent the draft: %+v", m.messages["bob"])
	}
	if want := "pasted text\n"; m.input.Value() != want {
		t.Errorf("input = %q, want %q", m.input.Value(), want)
	}
}

// The backlog signal must not fire on ordinary typing. A keypress queues
// its own key-up record, so a small nonzero backlog is normal and must
// stay below the threshold — otherwise Enter would stop sending entirely.
func TestSmallBacklogFromKeyUpDoesNotSwallowEnter(t *testing.T) {
	m := newReadyModel(t)

	stubInputBacklog(t, pasteBacklogThreshold-1)
	draft := "hi"
	for _, r := range draft {
		m = pressKey(m, runeMsg(r))
	}
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.messages["bob"]) != 1 || m.messages["bob"][0].body != draft {
		t.Errorf("expected Enter to send %q with only key-up-level backlog, got %+v", draft, m.messages["bob"])
	}
}

// Regression: the paste window must not renew itself when it swallows an
// Enter. It used to push lastPasteAt forward on every swallow, so someone
// pressing Enter again because nothing happened kept re-arming their own
// block — Enter appeared permanently dead rather than briefly delayed.
func TestSwallowedEnterDoesNotExtendPasteWindow(t *testing.T) {
	m := newReadyModel(t)

	stubInputBacklog(t, 200)
	m = pressKey(m, runeMsg('x')) // paste activity arms the window
	stubInputBacklog(t, 0)        // queue drained

	armedAt := m.lastPasteAt
	for i := 0; i < 3; i++ { // impatient repeated presses
		m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	}

	if m.lastPasteAt.After(armedAt) {
		t.Errorf("paste window was pushed forward by a swallowed Enter (armed %v, now %v) — repeated presses would never break through",
			armedAt, m.lastPasteAt)
	}
}

// The window has to be short enough that a deliberate Enter after a paste
// isn't perceptibly blocked — it only needs to span a terminal's trailing
// newline, which arrives within milliseconds of the block itself.
func TestPasteWindowIsShortEnoughToNotBlockDeliberateEnter(t *testing.T) {
	const humanReaction = 400 * time.Millisecond // a fast but realistic look-then-press
	if pasteEnterGracePeriod >= humanReaction {
		t.Fatalf("pasteEnterGracePeriod = %v, which would swallow a deliberate Enter pressed %v after a paste",
			pasteEnterGracePeriod, humanReaction)
	}

	m := newReadyModel(t)
	stubInputBacklog(t, 200)
	for _, r := range "pasted paragraph" {
		m = pressKey(m, runeMsg(r))
	}
	stubInputBacklog(t, 0)

	// The user reads it over, then hits Enter to send.
	m.lastPasteAt = m.lastPasteAt.Add(-humanReaction)
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.messages["bob"]) != 1 || m.messages["bob"][0].body != "pasted paragraph" {
		t.Errorf("expected Enter to send the pasted draft after %v, got %+v", humanReaction, m.messages["bob"])
	}
}

// Shift+Enter inserts a newline instead of sending.
//
// Worth explaining why this is tested via a stubbed shiftHeld rather than
// a `tea.KeyMsg` that says "shift+enter": no such KeyMsg exists.
// bubbletea v1.3.10's Key struct carries Type/Runes/Alt/Paste and no
// Shift field, has no KeyShiftEnter constant, and doesn't negotiate the
// Kitty/CSI-u protocols that would encode the modifier — so a physical
// Shift+Enter always decodes to a plain KeyEnter, and matching on
// msg.String() could never work. That dead end is real, and this app used
// to carry an unreachable `case "shift+enter"` because of it.
//
// The way out is that the modifier isn't actually unavailable, only
// absent from the *key event*: bubbletea's own Windows decoder reads
// console records whose ControlKeyState holds SHIFT_PRESSED (it uses that
// bit for Tab and the arrows) and simply ignores it for VK_RETURN. So
// updateChat asks the OS for the live physical key state instead — see
// shiftHeld. Stubbing that one function is exactly the seam a test wants:
// physical hardware state can't be staged in an automated run, and the
// real syscall is exercised separately.
func TestShiftEnterInsertsNewlineInsteadOfSending(t *testing.T) {
	m := newReadyModel(t)

	draft := "hi"
	for _, r := range draft {
		m = pressKey(m, runeMsg(r))
	}

	stubShiftHeld(t, true)
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})

	if want := draft + "\n"; m.input.Value() != want {
		t.Errorf("input = %q, want %q — Shift+Enter should insert a newline", m.input.Value(), want)
	}
	if len(m.messages["bob"]) != 0 {
		t.Errorf("expected nothing sent while Shift was held, got messages %+v", m.messages["bob"])
	}
}

// Releasing Shift restores the normal send behavior — the newline path is
// gated on the live key state, not latched by a previous Shift+Enter.
func TestEnterSendsAgainAfterShiftReleased(t *testing.T) {
	m := newReadyModel(t)

	stubShiftHeld(t, true)
	m = pressKey(m, runeMsg('a'))
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter}) // newline, not a send
	if len(m.messages["bob"]) != 0 {
		t.Fatalf("precondition failed: Shift+Enter sent a message: %+v", m.messages["bob"])
	}

	stubShiftHeld(t, false) // user lets go of Shift
	m = pressKey(m, runeMsg('b'))
	draft := m.input.Value()
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.messages["bob"]) != 1 || m.messages["bob"][0].body != draft {
		t.Errorf("expected plain Enter to send %q once Shift was released, got messages %+v", draft, m.messages["bob"])
	}
}

// A held Shift outranks the paste-swallow window — both routes lead to a
// newline, but Shift is an explicit press rather than an inference, so it
// must not depend on paste state either way.
func TestShiftEnterInsertsNewlineDuringPasteWindow(t *testing.T) {
	m := newReadyModel(t)

	m = pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}, Paste: true})
	before := m.input.Value()

	stubShiftHeld(t, true)
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})

	if want := before + "\n"; m.input.Value() != want {
		t.Errorf("input = %q, want %q", m.input.Value(), want)
	}
	if len(m.messages["bob"]) != 0 {
		t.Errorf("expected nothing sent, got messages %+v", m.messages["bob"])
	}
}
