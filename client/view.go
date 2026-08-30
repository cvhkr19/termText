package main

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Colors and styles live in styles.go, shared with auth.go.

func (m model) View() string {
	if m.screen == screenAuth {
		return m.viewAuth()
	}

	if !m.ready {
		return "termtext: waiting for terminal size…"
	}

	main := lipgloss.JoinVertical(lipgloss.Left,
		m.viewport.View(),
		m.renderStatusLine(),
		inputBoxStyle.Width(m.mainWidth-2).Render(m.input.View()),
	)

	body := lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(), m.renderFocusGutter(), main)
	return lipgloss.JoinVertical(lipgloss.Left, m.renderTitleBar(), body, m.renderFooter())
}

// renderTitleBar is the title + rule, preceded by a spacer row on
// Windows (see below). titleBarHeight must match this exactly.
func (m model) renderTitleBar() string {
	title := titleBarStyle.Render("termText v0.1.0")
	rule := titleRuleStyle.Render(strings.Repeat("─", m.width))
	if runtime.GOOS == "windows" {
		// Windows can clip the console's topmost row when not maximized
		// (confirmed empirically) — sacrifice a blank spacer row instead
		// of the title. Other platforms don't have this quirk.
		return lipgloss.JoinVertical(lipgloss.Left, "", title, rule)
	}
	return lipgloss.JoinVertical(lipgloss.Left, title, rule)
}

// renderSidebar has no "Contacts" label — the list is self-evident.
func (m model) renderSidebar() string {
	var rows []string
	for i, c := range m.contacts {
		dot := lipgloss.NewStyle().Foreground(colorOffline).Render("●")
		if c.online {
			dot = lipgloss.NewStyle().Foreground(colorOnline).Render("●")
		}

		// Reserve 2 columns either way so rows don't shift on select.
		indicator := "  "
		name := c.username
		if i == m.active {
			indicator = activeIndicator + " "
			name = activeNameStyle.Render(c.username)
		}

		label := indicator + dot + " " + name
		if c.unread > 0 {
			badge := unreadBadgeStyle.Render(fmt.Sprintf("%d", c.unread))
			pad := m.sidebarWidth - lipgloss.Width(label) - lipgloss.Width(badge) - 2
			if pad < 1 {
				pad = 1
			}
			label += strings.Repeat(" ", pad) + badge
		}

		rows = append(rows, sidebarRowStyle.Width(m.sidebarWidth).Render(label))
	}

	borderColor := colorBorder
	if m.focusedPane == paneContacts {
		borderColor = colorAccent
	}

	return lipgloss.NewStyle().
		Width(m.sidebarWidth).
		Height(m.mainHeight()).
		BorderStyle(lipgloss.NormalBorder()).
		BorderRight(true).
		BorderForeground(borderColor).
		Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// mainHeight is the main pane's total height — viewport + status +
// input — excluding titleBarHeight.
func (m model) mainHeight() int {
	return m.viewport.Height + statusHeight + inputHeight
}

// renderFocusGutter draws an accent bar alongside whichever of
// chat/compose has focus.
func (m model) renderFocusGutter() string {
	chatRows := m.viewport.Height + statusHeight
	composeRows := inputHeight

	bar := lipgloss.NewStyle().Foreground(colorAccent).Render("▎")

	var lines []string
	for i := 0; i < chatRows; i++ {
		if m.focusedPane == paneChat {
			lines = append(lines, bar)
		} else {
			lines = append(lines, " ")
		}
	}
	for i := 0; i < composeRows; i++ {
		if m.focusedPane == paneCompose {
			lines = append(lines, bar)
		} else {
			lines = append(lines, " ")
		}
	}
	return strings.Join(lines, "\n")
}

// renderStatusLine shows a notice, else the typing indicator, else blank.
func (m model) renderStatusLine() string {
	if m.notice != "" {
		return typingStyle.Width(m.mainWidth).Render(m.notice)
	}
	if c, ok := m.activeContact(); ok && m.typing[c.username] {
		return typingStyle.Width(m.mainWidth).Render(c.username + " is typing…")
	}
	return typingStyle.Width(m.mainWidth).Render("")
}

func (m model) renderConversation() string {
	c, ok := m.activeContact()
	if !ok {
		return systemMsgStyle.Render("no contacts yet — /add <username> to add one")
	}

	msgs := m.messages[c.username]
	if len(msgs) == 0 {
		return systemMsgStyle.Render("no messages yet — say hello!")
	}

	var lines []string
	for _, msg := range msgs {
		lines = append(lines, m.renderMessage(msg))
	}
	return strings.Join(lines, "\n\n")
}

func (m model) renderMessage(msg message) string {
	if msg.system {
		return systemMsgStyle.Width(m.viewport.Width).Align(lipgloss.Center).Render(msg.body)
	}

	// No sender label — every conversation is 1:1, so alignment + bar
	// color are enough to distinguish theirs from mine.
	meta := timestampStyle.Render(msg.sentAt.Format("15:04"))
	if msg.mine {
		// Sharing the timestamp's line, not a line of its own — the
		// WhatsApp/iMessage convention of a small tick tucked into the
		// corner rather than a full-width row announcing itself.
		meta += " " + renderTick(msg.tick)
	}

	// A file is always a labeled 📎 entry, never rendered inline —
	// images included, out of scope by design.
	var lines []string
	if msg.isFile() {
		if msg.body != "" {
			lines = append(lines, msg.body)
		}
		lines = append(lines, fmt.Sprintf("📎 %s (%s)", msg.fileName, humanizeBytes(msg.fileSize)))
	} else {
		lines = append(lines, msg.body)
	}
	metaLine := len(lines)
	lines = append(lines, meta)
	content := strings.Join(lines, "\n")

	// bubbleBorder/bubblePadding mirror the bubble style's own
	// Border+Padding. lipgloss's Width includes padding but not border,
	// so a raw-text measurement needs padding added back before use.
	const (
		bubbleBorder   = 2
		bubblePadding  = 2
		bubbleMinWidth = 16
		bubbleMaxRatio = 0.75
	)

	// capWidth: ~75% of viewport minus border, floored at
	// bubbleMinWidth, hard-capped at what the viewport can fit.
	capWidth := int(float64(m.viewport.Width)*bubbleMaxRatio) - bubbleBorder
	if capWidth < bubbleMinWidth {
		capWidth = bubbleMinWidth
	}
	if hardCap := m.viewport.Width - bubbleBorder; capWidth > hardCap {
		capWidth = hardCap
	}
	if capWidth < 1 {
		capWidth = 1
	}

	// Use the natural text width under the cap so a short message hugs
	// its edge; fall back to capWidth so long content wraps via Width
	// (MaxWidth would only truncate).
	naturalText := 0
	for _, line := range strings.Split(content, "\n") {
		if w := lipgloss.Width(line); w > naturalText {
			naturalText = w
		}
	}
	contentWidth := naturalText + bubblePadding
	if contentWidth > capWidth {
		contentWidth = capWidth
	}

	// Right-align the timestamp/tick line specifically, within the
	// bubble's actual text width (contentWidth minus the padding the
	// bubble style adds around everything) — tucking it into the
	// bottom-right corner instead of leaving it flush left like the body.
	innerWidth := contentWidth - bubblePadding
	if metaWidth := lipgloss.Width(meta); innerWidth < metaWidth {
		innerWidth = metaWidth
	}
	lines[metaLine] = lipgloss.NewStyle().Width(innerWidth).Align(lipgloss.Right).Render(meta)
	content = strings.Join(lines, "\n")

	bubbleStyle := bubbleYouStyle
	if msg.mine {
		bubbleStyle = bubbleMeStyle
	}
	bubble := bubbleStyle.Width(contentWidth).Render(content)

	align := lipgloss.Left
	if msg.mine {
		align = lipgloss.Right
	}
	return lipgloss.NewStyle().Width(m.viewport.Width).Align(align).Render(bubble)
}

// renderTick shows ✓/✓✓/✓✓(read) — delivered/read share a glyph and
// differ only in color.
func renderTick(t tickState) string {
	switch t {
	case tickDelivered:
		return lipgloss.NewStyle().Foreground(colorTickGray).Render("✓✓")
	case tickRead:
		return lipgloss.NewStyle().Foreground(colorTickRead).Render("✓✓")
	default:
		return lipgloss.NewStyle().Foreground(colorTickGray).Render("✓")
	}
}

func (m model) renderFooter() string {
	if m.connErr != "" {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Width(m.width).Padding(0, 1).Render(m.connErr)
	}
	// Only one newline key is advertised, the one that works here:
	// alt+enter/ctrl+j are bound on every platform, but where Shift+Enter
	// is detectable it's what people reach for first, and naming just it
	// also keeps this already-long line shorter. See shiftEnterSupported.
	newlineHint := "alt+enter/ctrl+j newline"
	if shiftEnterSupported {
		newlineHint = "shift+enter newline"
	}
	return footerStyle.Width(m.width).Render("tab next pane  •  enter send  •  " + newlineHint + "  •  /send <file>  •  o download  •  /logout  •  esc quit")
}
