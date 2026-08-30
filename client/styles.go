// Shared lipgloss styling — every color/style lives here once so a
// visual tweak never means hunting for a second hardcoded copy.
package main

import "github.com/charmbracelet/lipgloss"

var (
	colorOnline    = lipgloss.Color("42")  // green
	colorOffline   = lipgloss.Color("240") // dim gray
	colorUnread    = lipgloss.Color("204") // pink badge
	colorBubbleYou = lipgloss.Color("240") // muted gray border, incoming message bubbles
	colorTickGray  = lipgloss.Color("245")
	colorDim       = lipgloss.Color("241")
	colorBorder    = lipgloss.Color("238")

	// colorAccent is the app's one accent color — swap this line to
	// re-theme everything that reads as "accented".
	colorAccent = lipgloss.Color("114")

	// Alias, not a copy — the read tick is meant to *be* the accent color.
	colorTickRead = colorAccent

	sidebarRowStyle  = lipgloss.NewStyle().Padding(0, 1)
	activeNameStyle  = lipgloss.NewStyle().Bold(true)
	activeIndicator  = lipgloss.NewStyle().Foreground(colorAccent).Render("▎")
	unreadBadgeStyle = lipgloss.NewStyle().Background(colorUnread).Foreground(lipgloss.Color("0")).Padding(0, 1)

	typingStyle    = lipgloss.NewStyle().Italic(true).Foreground(colorDim).Padding(0, 1)
	systemMsgStyle = lipgloss.NewStyle().Italic(true).Foreground(colorDim)
	timestampStyle = lipgloss.NewStyle().Foreground(colorDim)

	bubbleMeStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorAccent).Padding(0, 1)
	bubbleYouStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorBubbleYou).Padding(0, 1)

	inputBoxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorBorder).Padding(0, 1)
	footerStyle   = lipgloss.NewStyle().Foreground(colorDim).Padding(0, 1)

	// No Width/background/MarginBottom needed — the rule below is the separator.
	titleBarStyle = lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(0, 1)
	// Same border glyph as the message bubbles, for visual consistency.
	titleRuleStyle = lipgloss.NewStyle().Foreground(colorBorder)
)
