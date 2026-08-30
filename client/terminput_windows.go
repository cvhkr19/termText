//go:build windows

package main

import "golang.org/x/sys/windows"

// shiftEnterSupported drives the footer hint (see renderFooter) — Windows
// is where the physical-key query below actually works, so it's the only
// platform that advertises Shift+Enter as the newline key.
const shiftEnterSupported = true

// user32.GetAsyncKeyState reports a key's physical state at the instant
// it's called, independent of any window message queue — which is what
// makes it usable from a console app (GetKeyState, the usual alternative,
// reflects state as of the calling thread's last processed window
// message, and a console app pumps no such messages, so it would report
// stale or empty state here).
//
// NewLazySystemDLL, not NewLazyDLL: it resolves strictly out of
// System32 rather than the standard search order, so a stray user32.dll
// beside the binary or in the working directory can't be loaded instead.
// Lazy, so the DLL is only touched on the first Shift check rather than
// at process start.
var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procGetAsyncKeyState = user32.NewProc("GetAsyncKeyState")
)

// platformShiftHeld reports whether either Shift key is down right now.
//
// VK_SHIFT covers both left and right Shift. GetAsyncKeyState returns a
// SHORT whose high-order bit means "currently down" (the low bit means
// "pressed since the last call", which is deliberately not used here —
// that one would stay set from an unrelated earlier Shift press and
// misreport a plain Enter as Shift+Enter).
func platformShiftHeld() bool {
	if err := procGetAsyncKeyState.Find(); err != nil {
		// Should not happen on any real Windows install, but a missing
		// export must degrade to plain-Enter behavior, never panic.
		return false
	}
	ret, _, _ := procGetAsyncKeyState.Call(uintptr(windows.VK_SHIFT))
	// Mask to the SHORT the API actually returns before testing the
	// high-order bit — Call widens it to uintptr, leaving the upper bits
	// unspecified on 64-bit.
	return uint16(ret)&0x8000 != 0
}

// platformInputBacklog returns the number of unread records sitting in
// the console input buffer.
//
// Zero is the safe answer for every failure: when stdin isn't a console
// at all (piped input, or a `go test` run), the query fails and paste
// detection simply falls back to bubbletea's own Paste flag rather than
// misreporting a backlog that would swallow every Enter.
func platformInputBacklog() int {
	h, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return 0
	}
	var n uint32
	if err := windows.GetNumberOfConsoleInputEvents(h, &n); err != nil {
		return 0
	}
	return int(n)
}
