package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type authMode int

const (
	authLogin authMode = iota
	authRegister
)

// authFieldCount is how many inputs the auth screen cycles through —
// see authModel.focus.
const authFieldCount = 3

// authModel is the login/register screen shown when there's no valid
// saved token, or after /logout.
type authModel struct {
	mode     authMode
	server   textinput.Model
	username textinput.Model
	password textinput.Model
	focus    int // 0 = server, 1 = username, 2 = password

	submitting bool
	errMsg     string
}

// newAuthModel prefills the server field with serverPrefill (the
// currently-configured endpoint, rendered with an explicit scheme —
// see endpoint.schemeString) so submitting without touching it just
// reconnects to the same place, while still leaving it editable for
// someone who wants to point at a different server entirely.
func newAuthModel(serverPrefill string) authModel {
	s := textinput.New()
	s.Placeholder = "server address, e.g. chat.example.com"
	s.CharLimit = 256
	s.SetValue(serverPrefill)
	s.Focus()

	u := textinput.New()
	u.Placeholder = "username"
	u.CharLimit = 64

	p := textinput.New()
	p.Placeholder = "password"
	p.CharLimit = 128
	p.EchoMode = textinput.EchoPassword
	p.EchoCharacter = '•'

	return authModel{server: s, username: u, password: p}
}

// authResultMsg is what a submitted login/register attempt resolves to.
// See doAuth.
type authResultMsg struct {
	username string
	token    string
	err      error
}

// updateAuth handles input on the auth screen.
func (m model) updateAuth(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "tab", "down":
			m.auth.focus = (m.auth.focus + 1) % authFieldCount
			m.auth.focusInputs()
			return m, nil

		case "shift+tab", "up":
			m.auth.focus = (m.auth.focus - 1 + authFieldCount) % authFieldCount
			m.auth.focusInputs()
			return m, nil

		case "ctrl+t":
			if m.auth.mode == authLogin {
				m.auth.mode = authRegister
			} else {
				m.auth.mode = authLogin
			}
			m.auth.errMsg = ""
			return m, nil

		case "enter":
			if m.auth.submitting {
				// Already in flight — Update() processes one message at
				// a time, so this only guards against a second Enter
				// landing before the first request's authResultMsg
				// comes back, not a real race. Without it, two requests
				// for the same credentials can both reach the server,
				// and their responses can arrive in either order: an
				// earlier, slower request's real success can land right
				// after a later, faster one's rate-limit rejection,
				// making a 429 look like it "went through anyway".
				return m, nil
			}
			return m.submitAuth()
		}

	case authResultMsg:
		m.auth.submitting = false
		if msg.err != nil {
			m.auth.errMsg = msg.err.Error()
			return m, nil
		}

		m.me = msg.username
		m.token = msg.token
		cfg := config{Token: msg.token, Username: msg.username, ServerAddr: m.server.host, TLS: m.server.secure}
		if err := cfg.save(m.configPath); err != nil {
			// Not fatal — surfaced, but login still succeeds.
			m.auth.errMsg = "saved session but couldn't write config: " + err.Error()
		}
		m.auth.errMsg = "connecting…"
		return m, wsConnect(m.server, m.token)
	}

	var cmd tea.Cmd
	switch m.auth.focus {
	case 0:
		m.auth.server, cmd = m.auth.server.Update(msg)
	case 1:
		m.auth.username, cmd = m.auth.username.Update(msg)
	default:
		m.auth.password, cmd = m.auth.password.Update(msg)
	}
	return m, cmd
}

func (a *authModel) focusInputs() {
	a.server.Blur()
	a.username.Blur()
	a.password.Blur()
	switch a.focus {
	case 0:
		a.server.Focus()
	case 1:
		a.username.Focus()
	default:
		a.password.Focus()
	}
}

func (m model) submitAuth() (tea.Model, tea.Cmd) {
	serverInput := strings.TrimSpace(m.auth.server.Value())
	username := strings.TrimSpace(m.auth.username.Value())
	password := m.auth.password.Value()
	if serverInput == "" || username == "" || password == "" {
		m.auth.errMsg = "server, username, and password are all required"
		return m, nil
	}

	host, secure, _ := splitScheme(serverInput)
	if host == "" {
		m.auth.errMsg = "server address is required"
		return m, nil
	}
	// Takes effect immediately, not just on success — a failed login
	// against a newly-typed server shouldn't silently retry the old one.
	m.server = endpoint{host: host, secure: secure}

	m.auth.submitting = true
	m.auth.errMsg = ""
	return m, doAuth(m.server, m.auth.mode, username, password, m.registrationCode)
}

// doAuth runs the HTTP round trip on its own goroutine as a tea.Cmd.
// registrationCode is only ever sent on the /register path.
func doAuth(server endpoint, mode authMode, username, password, registrationCode string) tea.Cmd {
	return func() tea.Msg {
		path := "/login"
		if mode == authRegister {
			path = "/register"
		}
		token, err := httpAuth(server, path, username, password, registrationCode)
		return authResultMsg{username: username, token: token, err: err}
	}
}

// logoutCmd best-effort revokes token server-side. Fire-and-forget: the
// UI has already dropped back to the login screen by the time this
// runs, and the server expires the session on its own after SessionTTL
// even if this never lands (offline, server unreachable, etc.).
func logoutCmd(server endpoint, token string) tea.Cmd {
	return func() tea.Msg {
		req, err := http.NewRequest(http.MethodPost, server.httpURL("/logout"), nil)
		if err != nil {
			return nil
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return nil
	}
}

func httpAuth(server endpoint, path, username, password, registrationCode string) (string, error) {
	fields := map[string]string{"username": username, "password": password}
	if path == "/register" && registrationCode != "" {
		fields["registration_code"] = registrationCode
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(server.httpURL(path), "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("connect to %s: %w", server, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = resp.Status
		}
		return "", fmt.Errorf("%s", msg)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return result.Token, nil
}

func (m model) viewAuth() string {
	title := "termtext — log in"
	toggleHint := "ctrl+t: switch to register"
	if m.auth.mode == authRegister {
		title = "termtext — register"
		toggleHint = "ctrl+t: switch to login"
	}

	labelStyle := lipgloss.NewStyle().Foreground(colorDim)
	fields := lipgloss.JoinVertical(lipgloss.Left,
		labelStyle.Render("server"),
		m.auth.server.View(),
		"",
		labelStyle.Render("username"),
		m.auth.username.View(),
		"",
		labelStyle.Render("password"),
		m.auth.password.View(),
	)

	status := labelStyle.Render("enter: submit  •  tab: next field  •  " + toggleHint + "  •  ctrl+c: quit")
	if m.auth.submitting {
		status = lipgloss.NewStyle().Foreground(colorAccent).Render("submitting…")
	}
	if m.auth.errMsg != "" {
		status = lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render(m.auth.errMsg)
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorBorder).
		Padding(1, 3).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.NewStyle().Bold(true).Render(title),
			"",
			fields,
			"",
			status,
		))

	w, h := m.width, m.height
	if w == 0 {
		w, h = 80, 24
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}
