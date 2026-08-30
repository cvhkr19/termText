package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// endpoint is host:port plus whether to reach it over TLS. All URLs go
// through httpURL/wsURL so enabling TLS is a config change, not an
// edit to every call site.
type endpoint struct {
	host   string // host:port, no scheme
	secure bool   // https/wss rather than http/ws
}

// httpURL builds an absolute URL for a plain-HTTP endpoint.
func (e endpoint) httpURL(path string) string {
	scheme := "http"
	if e.secure {
		scheme = "https"
	}
	return scheme + "://" + e.host + path
}

// wsURL builds the WebSocket URL — same host/TLS decision, ws/wss scheme.
func (e endpoint) wsURL(path string) string {
	scheme := "ws"
	if e.secure {
		scheme = "wss"
	}
	u := url.URL{Scheme: scheme, Host: e.host, Path: path}
	return u.String()
}

// String is the bare host:port, for user-facing error messages.
func (e endpoint) String() string { return e.host }

// schemeString renders host:port with an explicit scheme prefix, so
// prefilling the auth screen's server field with it and later
// re-parsing via splitScheme always recovers the same secure value —
// even if the user submits without touching the field at all.
func (e endpoint) schemeString() string {
	if e.secure {
		return "https://" + e.host
	}
	return "http://" + e.host
}

// splitScheme pulls an optional scheme off a -server value. hadScheme
// is reported separately so an explicit "http://" can override -tls
// and the saved config.
func splitScheme(s string) (host string, secure, hadScheme bool) {
	for _, p := range []struct {
		prefix string
		secure bool
	}{
		{"https://", true},
		{"wss://", true},
		{"http://", false},
		{"ws://", false},
	} {
		if strings.HasPrefix(s, p.prefix) {
			return strings.TrimSuffix(strings.TrimPrefix(s, p.prefix), "/"), p.secure, true
		}
	}
	return s, false, false
}

// config is the on-disk shape of ~/.chattui/config.json (or a
// throwaway path with --new). Username rides along with Token since
// the server never returns it on its own.
type config struct {
	Token      string `json:"token"`
	Username   string `json:"username"`
	ServerAddr string `json:"server_addr"`

	// TLS travels with the address — a saved https session must come
	// back up as https without repeating -tls.
	TLS bool `json:"tls"`
}

func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".chattui", "config.json"), nil
}

// loadConfig returns a zero config (not an error) if path doesn't exist.
func loadConfig(path string) (config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return config{}, nil
		}
		return config{}, err
	}

	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (c config) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
