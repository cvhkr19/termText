package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerTokenReadsHeaderOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		url    string
		want   string
	}{
		{"bearer header", "Bearer abc123", "/ws", "abc123"},
		{"no header", "", "/ws", ""},
		{"wrong scheme", "Basic abc123", "/ws", ""},
		{"scheme case must match", "bearer abc123", "/ws", ""},
		{"missing space", "Bearerabc123", "/ws", ""},
		{"header wins over query", "Bearer fromheader", "/ws?token=fromquery", "fromheader"},

		// The removed fallback: a token in the query string must no longer
		// authenticate anything, because a URL is the part of a request
		// that predictably lands in access logs and Referer headers.
		{"query param alone is ignored", "", "/ws?token=fromquery", ""},
		{"query param on upload is ignored", "", "/upload?token=fromquery", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if got := bearerToken(req); got != tc.want {
				t.Errorf("bearerToken() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOriginChecker(t *testing.T) {
	allowed := []string{"https://chat.example.com", "https://admin.example.com"}

	for _, tc := range []struct {
		name    string
		allowed []string
		origin  string
		want    bool
	}{
		// Native clients — the TUI, curl, wscat — send no Origin at all.
		// There's no ambient credential outside a browser for an attacker
		// to ride on, so this has to stay allowed or every real client
		// breaks.
		{"absent origin is allowed", allowed, "", true},
		{"absent origin allowed even with an empty allowlist", nil, "", true},

		{"listed origin", allowed, "https://chat.example.com", true},
		{"second listed origin", allowed, "https://admin.example.com", true},
		{"scheme is compared case-insensitively", allowed, "HTTPS://CHAT.EXAMPLE.COM", true},

		{"unlisted origin", allowed, "https://evil.example", false},
		{"right host, wrong scheme", allowed, "http://chat.example.com", false},
		{"right host, unexpected port", allowed, "https://chat.example.com:8443", false},
		{"suffix attack", allowed, "https://chat.example.com.evil.example", false},
		{"prefix attack", allowed, "https://evil.example/chat.example.com", false},

		// The secure default: with nothing configured, no browser origin
		// is trusted.
		{"empty allowlist refuses any browser origin", nil, "https://chat.example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ws", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := originChecker(tc.allowed)(req); got != tc.want {
				t.Errorf("originChecker(%v) for origin %q = %v, want %v", tc.allowed, tc.origin, got, tc.want)
			}
		})
	}
}

func TestSplitOrigins(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"only whitespace", "   ", nil},
		{"only commas", ",,,", nil},
		{"single", "https://a.example", []string{"https://a.example"}},
		{"multiple", "https://a.example,https://b.example", []string{"https://a.example", "https://b.example"}},
		{"whitespace trimmed", " https://a.example , https://b.example ", []string{"https://a.example", "https://b.example"}},
		{"trailing comma dropped", "https://a.example,", []string{"https://a.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitOrigins(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitOrigins(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("splitOrigins(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// An empty entry surviving into the allowlist would match the empty Origin
// header and re-open exactly what originChecker exists to close, so this
// pins the two functions' behavior together rather than separately.
func TestSplitOriginsNeverYieldsAMatchAllEntry(t *testing.T) {
	for _, in := range []string{"", " ", ",", " , ", "https://a.example,,"} {
		origins := splitOrigins(in)
		req := httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.Header.Set("Origin", "https://evil.example")
		if originChecker(origins)(req) {
			t.Errorf("-allowed-origins %q produced an allowlist that accepts any origin: %v", in, origins)
		}
	}
}
