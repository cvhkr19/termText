package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
	"unicode/utf8"

	"termtext/server/auth"
	"termtext/server/store"
)

const (
	// maxUsernameLen caps a username at 32 runes (not bytes).
	maxUsernameLen = 32

	// maxPasswordBytes: bcrypt hard-rejects >72 bytes — validate here so
	// it's a clean 400, not an opaque 500 from HashPassword.
	maxPasswordBytes = 72

	// maxCredentialsBody bounds the JSON body so an unauthenticated
	// caller can't force unbounded buffering.
	maxCredentialsBody = 4 << 10
)

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// RegistrationCode is only checked by registerHandler, and only when
	// the server was started with -registration-code/REGISTRATION_CODE.
	RegistrationCode string `json:"registration_code"`
}

type tokenResponse struct {
	Token string `json:"token"`
}

// registerHandler builds /register. requiredCode is the server's
// -registration-code/REGISTRATION_CODE setting — empty means open
// self-registration, matching every existing deployment.
func registerHandler(st *store.Store, requiredCode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		creds, ok := decodeCredentials(w, r)
		if !ok {
			return
		}

		// Constant-time: this is a shared secret, so a length- or
		// early-exit-based comparison would leak how much of it a guess
		// got right. Checked before hashing so a wrong code costs
		// nothing — bcrypt is deliberately slow and shouldn't run for a
		// request that was always going to be rejected.
		if requiredCode != "" && subtle.ConstantTimeCompare([]byte(creds.RegistrationCode), []byte(requiredCode)) != 1 {
			http.Error(w, "invalid registration code", http.StatusForbidden)
			return
		}

		hash, err := auth.HashPassword(creds.Password)
		if err != nil {
			log.Printf("hash password: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		userID, err := st.CreateUser(creds.Username, hash)
		switch {
		case errors.Is(err, store.ErrUsernameTaken):
			http.Error(w, "username already taken", http.StatusConflict)
			return
		case err != nil:
			log.Printf("create user: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		token, err := issueSession(st, userID)
		if err != nil {
			log.Printf("issue session: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, tokenResponse{Token: token})
	}
}

func loginHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		creds, ok := decodeCredentials(w, r)
		if !ok {
			return
		}

		user, err := st.GetUserByUsername(creds.Username)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Same response as a bad password — don't leak whether the username exists.
			http.Error(w, "invalid username or password", http.StatusUnauthorized)
			return
		case err != nil:
			log.Printf("get user: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !auth.CheckPassword(user.PasswordHash, creds.Password) {
			http.Error(w, "invalid username or password", http.StatusUnauthorized)
			return
		}

		token, err := issueSession(st, user.ID)
		if err != nil {
			log.Printf("issue session: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, tokenResponse{Token: token})
	}
}

// logoutHandler revokes the caller's own session token. Idempotent —
// logging out twice is not an error.
func logoutHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authenticateHTTP(st, w, r); !ok {
			return
		}
		if err := st.DeleteSession(bearerToken(r)); err != nil {
			log.Printf("delete session: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeCredentials(w http.ResponseWriter, r *http.Request) (credentials, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCredentialsBody)

	var creds credentials
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "malformed JSON body", http.StatusBadRequest)
		return credentials{}, false
	}
	if creds.Username == "" || creds.Password == "" {
		http.Error(w, "username and password are required", http.StatusBadRequest)
		return credentials{}, false
	}
	if utf8.RuneCountInString(creds.Username) > maxUsernameLen {
		http.Error(w, fmt.Sprintf("username must be at most %d characters", maxUsernameLen), http.StatusBadRequest)
		return credentials{}, false
	}
	if len(creds.Password) > maxPasswordBytes {
		http.Error(w, fmt.Sprintf("password must be at most %d bytes", maxPasswordBytes), http.StatusBadRequest)
		return credentials{}, false
	}
	return creds, true
}

func issueSession(st *store.Store, userID int64) (string, error) {
	token, err := auth.GenerateToken()
	if err != nil {
		return "", err
	}
	if err := st.CreateSession(userID, token, time.Now().Add(store.SessionTTL)); err != nil {
		return "", err
	}
	return token, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
