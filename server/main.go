// Command server is the termtext relay server: a WebSocket hub all
// clients connect outbound to. See PROTOCOL.md for the wire format.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"termtext/server/store"
)

func main() {
	// -addr/-db/-uploads/-max-upload-size/-allowed-origins/-registration-code
	// all fall back to an env var of the same shape; flag wins if both are
	// set. That's what makes a bare `docker run -e PORT=... -e ...` a
	// complete deployment, with no entrypoint script rewriting flags.
	addr := flag.String("addr", ":"+envOrDefault("PORT", "8080"), "HTTP/WebSocket listen address (env: PORT, a bare port number e.g. 8080)")
	dbPath := flag.String("db", envOrDefault("DATABASE_PATH", "termtext.db"), "path to the SQLite database file (env: DATABASE_PATH)")
	uploadsDir := flag.String("uploads", envOrDefault("UPLOAD_DIR", "uploads"), "directory to store uploaded files in (env: UPLOAD_DIR)")
	maxUploadSize := flag.String("max-upload-size", envOrDefault("MAX_UPLOAD_SIZE", "25MB"), "per-file upload size cap, e.g. 25MB, 500KB, 1GB (env: MAX_UPLOAD_SIZE)")
	allowedOrigins := flag.String("allowed-origins", envOrDefault("ALLOWED_ORIGINS", ""), "comma-separated browser origins allowed to open a WebSocket, e.g. https://chat.example.com (env: ALLOWED_ORIGINS; empty refuses all browser origins — native clients send no Origin and are unaffected)")
	registrationCode := flag.String("registration-code", envOrDefault("REGISTRATION_CODE", ""), "if set, /register requires this exact code in the request body (env: REGISTRATION_CODE; empty allows open self-registration)")
	flag.Parse()

	maxUploadBytes, err := parseSize(*maxUploadSize)
	if err != nil {
		log.Fatalf("invalid -max-upload-size/MAX_UPLOAD_SIZE %q: %v", *maxUploadSize, err)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Housekeeping only — expiry is enforced on every lookup regardless.
	if n, err := st.DeleteExpiredSessions(); err != nil {
		log.Printf("prune expired sessions: %v", err)
	} else if n > 0 {
		log.Printf("pruned %d expired session(s)", n)
	}

	if err := os.MkdirAll(*uploadsDir, 0o755); err != nil {
		log.Fatalf("create uploads dir: %v", err)
	}

	hub := NewHub()
	go hub.Run()

	origins := splitOrigins(*allowedOrigins)
	upgrader := newUpgrader(origins)

	// Shared across /register and /login so alternating them can't
	// dodge the limit.
	authLimiter := newRateLimiter(authRateBurst, authRateRefill, authRateIdleTTL)

	mux := http.NewServeMux()
	mux.HandleFunc("/register", authLimiter.limit(registerHandler(st, *registrationCode)))
	mux.HandleFunc("/login", authLimiter.limit(loginHandler(st)))
	mux.HandleFunc("POST /logout", logoutHandler(st))
	mux.HandleFunc("POST /upload", uploadHandler(st, *uploadsDir, maxUploadBytes))
	mux.HandleFunc("GET /download/{file_id}", downloadHandler(st))
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWS(hub, st, upgrader, w, r)
	})

	log.Printf("termtext server listening on %s (db: %s, uploads: %s, max upload: %s, allowed origins: %s, registration: %s)",
		*addr, *dbPath, *uploadsDir, humanizeBytes(maxUploadBytes), describeOrigins(origins), describeRegistration(*registrationCode))
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// envOrDefault reads key from the environment, falling back to def.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitOrigins parses -allowed-origins, dropping empty entries so a
// trailing comma can't produce a match-all "" entry.
func splitOrigins(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// describeOrigins renders the allowlist for the startup log.
func describeOrigins(origins []string) string {
	if len(origins) == 0 {
		return "none (browser origins refused)"
	}
	return strings.Join(origins, ", ")
}

// describeRegistration renders the registration-code setting for the
// startup log, without ever printing the code itself.
func describeRegistration(code string) string {
	if code == "" {
		return "open"
	}
	return "code required"
}
