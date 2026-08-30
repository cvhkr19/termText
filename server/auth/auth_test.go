package auth

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("HashPassword returned the plaintext unchanged")
	}
	if !CheckPassword(hash, "correct horse battery staple") {
		t.Error("CheckPassword rejected the password it was just given")
	}
}

func TestCheckPasswordRejectsWrongInputs(t *testing.T) {
	hash, err := HashPassword("hunter2")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	for _, tc := range []struct {
		name     string
		hash     string
		password string
	}{
		{"wrong password", hash, "hunter3"},
		{"empty password", hash, ""},
		{"case differs", hash, "Hunter2"},
		{"trailing space", hash, "hunter2 "},
		{"garbage hash", "not-a-bcrypt-hash", "hunter2"},
		{"empty hash", "", "hunter2"},
	} {
		if CheckPassword(tc.hash, tc.password) {
			t.Errorf("%s: CheckPassword accepted a mismatch", tc.name)
		}
	}
}

// The salt is per-call, not per-password, so the same password hashed
// twice must not produce the same stored string — otherwise identical
// passwords would be visibly identical in the users table, and one
// precomputed table would crack every account that shares a password.
func TestHashPasswordSaltsEachCall(t *testing.T) {
	first, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if first == second {
		t.Error("hashing the same password twice produced identical hashes; salt is not per-call")
	}
	if !CheckPassword(first, "same password") || !CheckPassword(second, "same password") {
		t.Error("both independently-salted hashes should still verify")
	}
}

// bcrypt refuses passwords over 72 bytes outright rather than silently
// truncating them. This test pins that behavior because it's the reason
// decodeCredentials caps the field at maxPasswordBytes: without that cap,
// this error surfaces to the caller as an opaque 500.
func TestHashPasswordRejectsOverBcryptLimit(t *testing.T) {
	if _, err := HashPassword(strings.Repeat("a", 72)); err != nil {
		t.Fatalf("72 bytes should be accepted, got %v", err)
	}

	_, err := HashPassword(strings.Repeat("a", 73))
	if err == nil {
		t.Fatal("expected 73 bytes to be rejected by bcrypt")
	}
	if err != bcrypt.ErrPasswordTooLong {
		t.Errorf("expected bcrypt.ErrPasswordTooLong, got %v", err)
	}
}

func TestGenerateTokenIsUnguessable(t *testing.T) {
	const iterations = 200

	seen := make(map[string]bool, iterations)
	for i := 0; i < iterations; i++ {
		token, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if seen[token] {
			t.Fatalf("GenerateToken returned a duplicate token after %d calls", i)
		}
		seen[token] = true

		// 32 random bytes in unpadded base64url is 43 characters. Checking
		// the encoded length catches a shortened tokenBytes, which would
		// weaken every session at once and is otherwise invisible.
		if len(token) != 43 {
			t.Fatalf("token %q has length %d, want 43 (32 raw bytes, base64url unpadded)", token, len(token))
		}
		// URL-safe means no '+', '/', or '=' — the token travels in an
		// Authorization header today, but the encoding choice is what
		// keeps it safe to put anywhere else without escaping.
		if strings.ContainsAny(token, "+/=") {
			t.Fatalf("token %q contains characters outside the URL-safe alphabet", token)
		}
	}
}
