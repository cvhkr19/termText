package main

// Terminal-input facts bubbletea (v1.3.10) doesn't surface, queried from
// the OS instead. Both helpers here exist for the same underlying reason:
// bubbletea has two completely separate input paths — an ANSI byte-stream
// parser (Unix, and Windows only when stdin isn't a real console) and a
// Windows Console API reader — and the Windows one drops information the
// ANSI one preserves. Anything verified against only one of those paths
// says nothing about the other.
//
// Both are package-level vars rather than direct calls so tests can
// substitute deterministic stand-ins: the real implementations depend on
// live OS state (physical key position, console input queue depth) that
// an automated test can't stage.

// shiftHeld reports whether a Shift key is physically down right now.
//
// bubbletea discards the Shift modifier for Enter specifically, so
// "shift+enter" can never arrive as a distinct tea.KeyMsg. The
// information isn't unavailable, just absent from the key event: the
// Windows decoder reads console records whose ControlKeyState carries
// SHIFT_PRESSED and consults that bit for Tab (KeyShiftTab) and the arrow
// keys, but returns a bare KeyEnter for VK_RETURN without looking. So the
// modifier is recovered by asking the OS for live key state instead.
var shiftHeld = platformShiftHeld

// inputBacklog reports how many input events are already queued behind
// the one being handled — the signal used to recognize a paste.
//
// Necessary because bubbletea's Paste flag is set in exactly one place,
// detectBracketedPaste, which lives in the ANSI byte-stream parser. The
// Windows Console path constructs KeyMsg{Type, Runes, Alt} and never
// populates Paste at all, so on Windows msg.Paste is *always* false and
// bracketed-paste detection is structurally unreachable.
//
// A queue depth is a structural signal rather than a timing guess: a
// pasted block is injected into the console input buffer all at once, and
// bubbletea drains it only 16 events at a time (PeekNConsoleInputs), so a
// large backlog is still pending while each chunk is processed. A human
// pressing Enter has finished typing, leaving nothing queued behind it.
// This deliberately replaces an earlier inter-keystroke *timing*
// heuristic that measured gaps between when Update() processed keys —
// which a slow render or GC pause could stretch, misreading ordinary
// typing as a burst.
var inputBacklog = platformInputBacklog
