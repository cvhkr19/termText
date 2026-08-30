// Command client is the termtext TUI client (Bubble Tea).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	serverFlag := flag.String("server", "", "server address as host:port, or a full URL like https://chat.example.com (default: saved config, or localhost:8080)")
	tlsFlag := flag.Bool("tls", false, "reach the server over https:// and wss:// instead of http:// and ws:// (implied by an https/wss -server URL; default: saved config)")
	newFlag := flag.Bool("new", false, "use a fresh throwaway config instead of ~/.chattui/config.json, for running multiple independent clients locally")
	registrationCodeFlag := flag.String("registration-code", os.Getenv("TERMTEXT_REGISTRATION_CODE"), "registration code required by servers with self-registration gated; only sent when registering, never saved to config (env: TERMTEXT_REGISTRATION_CODE)")
	flag.Parse()

	path, err := resolveConfigPath(*newFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "termtext client: resolving config path:", err)
		os.Exit(1)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "termtext client: reading config:", err)
		os.Exit(1)
	}

	server := cfg.ServerAddr
	if *serverFlag != "" {
		server = *serverFlag
	}
	host, schemeSecure, hadScheme := splitScheme(server)
	if host == "" {
		host = "localhost:8080"
	}

	// TLS precedence, lowest first: saved config, then -tls if actually
	// passed, then an explicit scheme in -server.
	secure := cfg.TLS
	if flagPassed("tls") {
		secure = *tlsFlag
	}
	if hadScheme {
		secure = schemeSecure
	}

	ep := endpoint{host: host, secure: secure}
	cfg.ServerAddr = host
	cfg.TLS = secure

	// p is assigned after initialModel — its send closure only calls
	// p.Send later, from a goroutine, once p exists.
	var p *tea.Program
	m := initialModel(func(msg tea.Msg) { p.Send(msg) }, cfg, ep, path)
	m.registrationCode = *registrationCodeFlag
	if *newFlag {
		// --new always needs register, never login — the server has
		// never seen this identity.
		m.auth.mode = authRegister
	}
	// WithMouseCellMotion enables real wheel-scroll events — without it,
	// terminals simulate scroll as arrow keys, which would also move the
	// compose box's cursor on every scroll tick.
	p = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "termtext client:", err)
		os.Exit(1)
	}
}

// resolveConfigPath is ~/.chattui/config.json normally, or a fresh
// temp path with --new — a disposable identity per invocation.
func resolveConfigPath(fresh bool) (string, error) {
	if !fresh {
		return defaultConfigPath()
	}
	dir, err := os.MkdirTemp("", "chattui-*")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// flagPassed reports whether name was actually passed, not just at its
// zero default. See the TLS precedence comment in main.
func flagPassed(name string) bool {
	passed := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = true
		}
	})
	return passed
}
