package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"termtext/server/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// postJSON drives a handler with a raw body string — raw rather than
// marshalled, so the malformed-JSON and oversized-body cases can be
// expressed directly.
func postJSON(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h(rec, req)
	return rec
}

func TestDecodeCredentialsRejectsBadInput(t *testing.T) {
	// decodeCredentials writes the error response itself and reports
	// ok=false, so a bare handler around it is enough to observe both.
	handler := func(w http.ResponseWriter, r *http.Request) {
		if _, ok := decodeCredentials(w, r); ok {
			w.WriteHeader(http.StatusOK)
		}
	}

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"valid", `{"username":"alice","password":"hunter2"}`, http.StatusOK},
		{"malformed json", `{"username":`, http.StatusBadRequest},
		{"not json at all", `alice:hunter2`, http.StatusBadRequest},
		{"missing username", `{"password":"hunter2"}`, http.StatusBadRequest},
		{"missing password", `{"username":"alice"}`, http.StatusBadRequest},
		{"empty username", `{"username":"","password":"hunter2"}`, http.StatusBadRequest},

		{
			"username at the cap",
			fmt.Sprintf(`{"username":%q,"password":"hunter2"}`, strings.Repeat("a", maxUsernameLen)),
			http.StatusOK,
		},
		{
			"username one over the cap",
			fmt.Sprintf(`{"username":%q,"password":"hunter2"}`, strings.Repeat("a", maxUsernameLen+1)),
			http.StatusBadRequest,
		},
		{
			// Counted in runes, so a 32-character non-ASCII name fits even
			// though it's well over 32 bytes encoded.
			"multibyte username at the cap",
			fmt.Sprintf(`{"username":%q,"password":"hunter2"}`, strings.Repeat("é", maxUsernameLen)),
			http.StatusOK,
		},
		{
			// The case that used to reach bcrypt and come back as a 500.
			"password one byte over bcrypt's limit",
			fmt.Sprintf(`{"username":"alice","password":%q}`, strings.Repeat("a", maxPasswordBytes+1)),
			http.StatusBadRequest,
		},
		{
			"password exactly at bcrypt's limit",
			fmt.Sprintf(`{"username":"alice","password":%q}`, strings.Repeat("a", maxPasswordBytes)),
			http.StatusOK,
		},
		{
			"body far over the size cap",
			fmt.Sprintf(`{"username":"alice","password":"x","junk":%q}`, strings.Repeat("a", maxCredentialsBody*2)),
			http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, handler, tc.body)
			if rec.Code != tc.want {
				t.Errorf("got %d, want %d (body: %s)", rec.Code, tc.want, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// An over-long password has to be reported as a bad request naming the
// field, not as the opaque 500 bcrypt's own error used to produce.
func TestRegisterRejectsOverLongPasswordWithoutHashing(t *testing.T) {
	st := openTestStore(t)

	body := fmt.Sprintf(`{"username":"alice","password":%q}`, strings.Repeat("a", 100))
	rec := postJSON(t, registerHandler(st, ""), body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "password") {
		t.Errorf("error should name the offending field, got %q", strings.TrimSpace(rec.Body.String()))
	}
	// The user must not have been created on the way to failing.
	if _, err := st.GetUserByUsername("alice"); err == nil {
		t.Error("a rejected registration should not have created the user")
	}
}

func TestRegisterLoginLogoutLifecycle(t *testing.T) {
	st := openTestStore(t)
	const creds = `{"username":"alice","password":"hunter2"}`

	rec := postJSON(t, registerHandler(st, ""), creds)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: got %d, want 201 (%s)", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var registered tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if registered.Token == "" {
		t.Fatal("register returned an empty token")
	}

	// The issued token must actually authenticate.
	if _, err := st.GetUserByToken(registered.Token); err != nil {
		t.Fatalf("token from register does not resolve to a user: %v", err)
	}

	// Registering the same name again is a conflict, not a second account.
	if rec := postJSON(t, registerHandler(st, ""), creds); rec.Code != http.StatusConflict {
		t.Errorf("duplicate register: got %d, want 409", rec.Code)
	}

	// Wrong password and unknown user must be indistinguishable, or /login
	// becomes a username-enumeration oracle.
	wrongPassword := postJSON(t, loginHandler(st), `{"username":"alice","password":"wrong"}`)
	unknownUser := postJSON(t, loginHandler(st), `{"username":"nobody","password":"hunter2"}`)
	if wrongPassword.Code != http.StatusUnauthorized || unknownUser.Code != http.StatusUnauthorized {
		t.Errorf("got %d and %d, want 401 for both", wrongPassword.Code, unknownUser.Code)
	}
	if wrongPassword.Body.String() != unknownUser.Body.String() {
		t.Errorf("responses differ and leak whether the user exists: %q vs %q",
			strings.TrimSpace(wrongPassword.Body.String()), strings.TrimSpace(unknownUser.Body.String()))
	}

	rec = postJSON(t, loginHandler(st), creds)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d, want 200 (%s)", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var loggedIn tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &loggedIn); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if loggedIn.Token == registered.Token {
		t.Error("login reissued the same token as register; each session should get its own")
	}

	// Logging out revokes only the token presented, leaving the other
	// session alone — the point of storing sessions per token.
	logoutRec := httptest.NewRecorder()
	logoutReq := httptest.NewRequest(http.MethodPost, "/logout", nil)
	logoutReq.Header.Set("Authorization", "Bearer "+loggedIn.Token)
	logoutHandler(st)(logoutRec, logoutReq)

	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("logout: got %d, want 204 (%s)", logoutRec.Code, strings.TrimSpace(logoutRec.Body.String()))
	}
	if _, err := st.GetUserByToken(loggedIn.Token); err == nil {
		t.Error("the logged-out token still authenticates")
	}
	if _, err := st.GetUserByToken(registered.Token); err != nil {
		t.Errorf("logging out one session revoked another: %v", err)
	}
}

func TestLogoutRequiresAuthentication(t *testing.T) {
	st := openTestStore(t)

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"unknown token", "Bearer nonexistent-token"},
		{"wrong scheme", "Basic YWxpY2U6aHVudGVyMg=="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/logout", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			logoutHandler(st)(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("got %d, want 401", rec.Code)
			}
		})
	}
}

// A server started with a registration code must refuse anyone who
// doesn't supply the exact same one, and must not create the account
// on the way to refusing it.
func TestRegisterEnforcesRegistrationCode(t *testing.T) {
	st := openTestStore(t)
	handler := registerHandler(st, "letmein")

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"correct code", `{"username":"alice","password":"hunter2","registration_code":"letmein"}`, http.StatusCreated},
		{"wrong code", `{"username":"bob","password":"hunter2","registration_code":"nope"}`, http.StatusForbidden},
		{"missing code", `{"username":"carol","password":"hunter2"}`, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, handler, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d (%s)", rec.Code, tc.want, strings.TrimSpace(rec.Body.String()))
			}
		})
	}

	if _, err := st.GetUserByUsername("bob"); err == nil {
		t.Error("a request with the wrong registration code should not have created the user")
	}
	if _, err := st.GetUserByUsername("carol"); err == nil {
		t.Error("a request with no registration code should not have created the user")
	}
}

func TestAuthHandlersRejectNonPost(t *testing.T) {
	st := openTestStore(t)

	for name, h := range map[string]http.HandlerFunc{
		"register": registerHandler(st, ""),
		"login":    loginHandler(st),
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/"+name, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s via GET: got %d, want 405", name, rec.Code)
		}
	}
}
