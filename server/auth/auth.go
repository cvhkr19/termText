// Package auth handles password hashing and opaque session tokens,
// kept free of DB/HTTP so the crypto is easy to test in isolation.
package auth

import (
	"crypto/rand"
	"encoding/base64"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword bcrypt-hashes password for storage. bcrypt (not a fast
// hash) is deliberately slow — that's what makes offline brute-forcing expensive.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword reports whether password matches a hash produced by HashPassword.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// tokenBytes is 256 bits — infeasible to guess.
const tokenBytes = 32

// GenerateToken returns a random, URL-safe session token. Deliberately
// not a JWT — just an unguessable key looked up in the sessions table.
func GenerateToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
