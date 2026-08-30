package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEndpointURLs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ep       endpoint
		httpPath string
		wantHTTP string
		wantWS   string
	}{
		{
			// The zero value has to stay plaintext, so nothing about the
			// existing local-server workflow changes.
			name:     "plaintext by default",
			ep:       endpoint{host: "localhost:8080"},
			httpPath: "/login",
			wantHTTP: "http://localhost:8080/login",
			wantWS:   "ws://localhost:8080/ws",
		},
		{
			name:     "secure switches both scheme pairs",
			ep:       endpoint{host: "chat.example.com", secure: true},
			httpPath: "/login",
			wantHTTP: "https://chat.example.com/login",
			wantWS:   "wss://chat.example.com/ws",
		},
		{
			name:     "explicit port is preserved",
			ep:       endpoint{host: "chat.example.com:8443", secure: true},
			httpPath: "/upload",
			wantHTTP: "https://chat.example.com:8443/upload",
			wantWS:   "wss://chat.example.com:8443/ws",
		},
		{
			name:     "path with an embedded id",
			ep:       endpoint{host: "localhost:8080"},
			httpPath: "/download/3fa2c1d4-9b7e-4a1c-8e2f-1a2b3c4d5e6f",
			wantHTTP: "http://localhost:8080/download/3fa2c1d4-9b7e-4a1c-8e2f-1a2b3c4d5e6f",
			wantWS:   "ws://localhost:8080/ws",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ep.httpURL(tc.httpPath); got != tc.wantHTTP {
				t.Errorf("httpURL(%q) = %q, want %q", tc.httpPath, got, tc.wantHTTP)
			}
			if got := tc.ep.wsURL("/ws"); got != tc.wantWS {
				t.Errorf("wsURL(\"/ws\") = %q, want %q", got, tc.wantWS)
			}
		})
	}
}

// String is what lands in "connect to %s failed", where the scheme is
// noise — so it stays the bare host either way.
func TestEndpointStringOmitsScheme(t *testing.T) {
	for _, ep := range []endpoint{
		{host: "chat.example.com"},
		{host: "chat.example.com", secure: true},
	} {
		if got := ep.String(); got != "chat.example.com" {
			t.Errorf("String() = %q, want %q", got, "chat.example.com")
		}
	}
}

func TestSplitScheme(t *testing.T) {
	for _, tc := range []struct {
		in            string
		wantHost      string
		wantSecure    bool
		wantHadScheme bool
	}{
		{"localhost:8080", "localhost:8080", false, false},
		{"chat.example.com", "chat.example.com", false, false},
		{"", "", false, false},

		{"https://chat.example.com", "chat.example.com", true, true},
		{"wss://chat.example.com", "chat.example.com", true, true},
		{"https://chat.example.com:8443", "chat.example.com:8443", true, true},

		// An explicit http:// has to be distinguishable from "no scheme
		// given" — it's a deliberate request for plaintext, and it should
		// override a saved TLS setting rather than defer to it.
		{"http://localhost:8080", "localhost:8080", false, true},
		{"ws://localhost:8080", "localhost:8080", false, true},

		// A trailing slash is what you get from copying a URL out of a
		// browser; it must not become part of the host.
		{"https://chat.example.com/", "chat.example.com", true, true},
	} {
		host, secure, hadScheme := splitScheme(tc.in)
		if host != tc.wantHost || secure != tc.wantSecure || hadScheme != tc.wantHadScheme {
			t.Errorf("splitScheme(%q) = (%q, %v, %v), want (%q, %v, %v)",
				tc.in, host, secure, hadScheme, tc.wantHost, tc.wantSecure, tc.wantHadScheme)
		}
	}
}

// TLS is part of how to reach an address, so it has to survive a save/load
// round trip — otherwise a saved https session would silently come back up
// as plaintext on the next launch.
func TestConfigRoundTripsTLS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	want := config{Token: "tok", Username: "alice", ServerAddr: "chat.example.com", TLS: true}
	if err := want.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := loadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// A config written before the tls field existed has no such key; it must
// load as plaintext rather than failing.
func TestLoadConfigWithoutTLSFieldDefaultsToPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"token":"tok","username":"alice","server_addr":"localhost:8080"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TLS {
		t.Error("a config with no tls key should load as plaintext")
	}
	if cfg.ServerAddr != "localhost:8080" || cfg.Token != "tok" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoadConfigMissingFileIsNotAnError(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("a missing config is the normal first-launch state, got %v", err)
	}
	if cfg != (config{}) {
		t.Errorf("expected a zero config, got %+v", cfg)
	}
}

// save has to create ~/.chattui itself — on a first launch the directory
// doesn't exist yet, and the token has nowhere to land if it doesn't.
func TestConfigSaveCreatesParentDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")

	if err := (config{Token: "tok"}).save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Valid JSON on the way back out, checked independently of
	// loadConfig's own decoding.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	if probe["token"] != "tok" {
		t.Errorf("token = %v, want tok", probe["token"])
	}
}
