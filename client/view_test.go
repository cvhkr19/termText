package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// renderMessagePlain renders msg and strips ANSI styling, so the tests
// below can check plain-text layout without depending on color codes.
func renderMessagePlain(m model, msg message) []string {
	return strings.Split(ansi.Strip(m.renderMessage(msg)), "\n")
}

// The timestamp and read tick share one line, right-aligned to the
// bubble's own right edge — the WhatsApp/iMessage corner convention,
// rather than the tick sitting on a full-width line of its own.
func TestOwnMessageTimestampAndTickShareOneRightAlignedLine(t *testing.T) {
	m := model{viewport: viewport.New(60, 20)}
	msg := message{body: "hi", sentAt: time.Date(2026, 1, 1, 23, 29, 0, 0, time.Local), mine: true, tick: tickRead}

	lines := renderMessagePlain(m, msg)
	if len(lines) != 4 { // top border, body, meta, bottom border
		t.Fatalf("got %d lines, want 4 (no separate line for the tick): %q", len(lines), lines)
	}

	metaLine := lines[2]
	trimmed := strings.TrimRight(strings.TrimLeft(metaLine, " │"), " │")
	if !strings.HasSuffix(trimmed, "23:29 ✓✓") {
		t.Errorf("meta line = %q, want it to end with %q", trimmed, "23:29 ✓✓")
	}
	// Right-aligned: exactly one trailing space (the bubble's own
	// Padding(0,1), present on every line including the border) —
	// anything more would mean alignment padding leaked onto the wrong
	// side instead of the left.
	inner := strings.Trim(metaLine, "│")
	trailing := len(inner) - len(strings.TrimRight(inner, " "))
	if trailing != 1 {
		t.Errorf("meta line has %d trailing spaces (%q), want exactly 1 (the box's own padding) — expected the text flush against the right border", trailing, metaLine)
	}
}

// Each tick state renders on that same combined line, not a separate one.
func TestAllTickStatesShareTheTimestampLine(t *testing.T) {
	m := model{viewport: viewport.New(60, 20)}
	for _, tc := range []struct {
		tick tickState
		want string
	}{
		{tickSent, "23:29 ✓"},
		{tickDelivered, "23:29 ✓✓"},
		{tickRead, "23:29 ✓✓"},
	} {
		msg := message{body: "hi", sentAt: time.Date(2026, 1, 1, 23, 29, 0, 0, time.Local), mine: true, tick: tc.tick}
		lines := renderMessagePlain(m, msg)
		if len(lines) != 4 {
			t.Fatalf("tick %v: got %d lines, want 4: %q", tc.tick, len(lines), lines)
		}
		if !strings.Contains(lines[2], tc.want) {
			t.Errorf("tick %v: meta line = %q, want it to contain %q", tc.tick, lines[2], tc.want)
		}
	}
}

// An incoming (not mine) message has no tick at all — only the sender's
// own messages carry delivery/read state.
func TestIncomingMessageHasNoTick(t *testing.T) {
	m := model{viewport: viewport.New(60, 20)}
	msg := message{body: "hi", sentAt: time.Date(2026, 1, 1, 23, 29, 0, 0, time.Local), mine: false}

	lines := renderMessagePlain(m, msg)
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4: %q", len(lines), lines)
	}
	for _, glyph := range []string{"✓", "✓✓"} {
		if strings.Contains(lines[2], glyph) {
			t.Errorf("meta line = %q, should not contain a tick glyph for an incoming message", lines[2])
		}
	}
}

// A timestamp+tick line wider than the body must still widen the whole
// bubble to fit it, staying right-aligned rather than getting clipped or
// pushed outside the border.
func TestMetaLineWiderThanBodyStillFitsAndAligns(t *testing.T) {
	m := model{viewport: viewport.New(60, 20)}
	msg := message{body: "hi", sentAt: time.Date(2026, 1, 1, 23, 29, 0, 0, time.Local), mine: true, tick: tickRead}

	lines := renderMessagePlain(m, msg)
	bodyLine := lines[1]
	metaLine := lines[2]
	if lipgloss.Width(metaLine) < lipgloss.Width("23:29 ✓✓") {
		t.Fatalf("meta line %q looks clipped", metaLine)
	}
	// Both lines must be the same rendered *display* width (the bubble
	// is one rectangular box) despite "hi" being much shorter than
	// "23:29 ✓✓" — compared via lipgloss.Width, not len, since ✓ is a
	// multi-byte character whose byte count doesn't match its column width.
	if bw, mw := lipgloss.Width(bodyLine), lipgloss.Width(metaLine); bw != mw {
		t.Errorf("body line %q and meta line %q render at different widths (%d vs %d)", bodyLine, metaLine, bw, mw)
	}
}
