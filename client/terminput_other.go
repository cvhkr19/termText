//go:build !windows

package main

// shiftEnterSupported drives the footer hint (see renderFooter) — false
// here, so the footer advertises alt+enter/ctrl+j rather than promising a
// Shift+Enter that platformShiftHeld below can't detect.
const shiftEnterSupported = false

// platformShiftHeld always reports false off Windows.
//
// There's no portable equivalent of GetAsyncKeyState here: a Unix
// terminal app sees only the byte stream its terminal chose to send, with
// no side channel to query physical key state, so if the terminal didn't
// encode the modifier there's nothing left to recover. Terminals
// supporting the Kitty keyboard protocol do encode Shift+Enter
// distinctly, but bubbletea v1.3.10 neither negotiates nor parses it, so
// those sequences would arrive as unrecognized CSI input rather than a
// usable key.
//
// The portable fallback stays what it always was: alt+enter / ctrl+j for
// a newline (see initialModel's InsertNewline binding), or a
// terminal-level remap of Shift+Enter onto one of those.
func platformShiftHeld() bool { return false }

// platformInputBacklog always reports 0 off Windows — and nothing is lost
// by that. The backlog check exists to substitute for bubbletea's Paste
// flag on Windows, where the Console API path never sets it; here the
// ANSI byte-stream parser handles bracketed paste natively and sets Paste
// correctly, so msg.Paste alone is already the right (and more precise)
// signal.
func platformInputBacklog() int { return 0 }
