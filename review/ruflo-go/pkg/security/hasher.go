package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

// PasswordHasher applies an iterated SHA-256 stretching scheme with per-password salt.
type PasswordHasher struct {
	iterations int
	saltLen    int
}

// NewPasswordHasher returns a hasher with default iteration count and salt length.
func NewPasswordHasher() *PasswordHasher {
	return &PasswordHasher{iterations: 10000, saltLen: 32}
}

// Hash returns "saltHex:hashHex" suitable for storage.
func (h *PasswordHasher) Hash(password string) (string, error) {
	salt := make([]byte, h.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := h.pbkdf2(password, salt)
	return fmt.Sprintf("%s:%s", hex.EncodeToString(salt), hex.EncodeToString(hash)), nil
}

// Verify checks password against a stored "saltHex:hashHex" string.
func (h *PasswordHasher) Verify(password, stored string) bool {
	parts := splitOnce(stored, ':')
	if len(parts) != 2 {
		return false
	}
	salt, err := hex.DecodeString(parts[0])
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	hash := h.pbkdf2(password, salt)
	if len(hash) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(hash, expected) == 1
}

func (h *PasswordHasher) pbkdf2(password string, salt []byte) []byte {
	result := sha256.Sum256(append([]byte(password), salt...))
	for i := 1; i < h.iterations; i++ {
		result = sha256.Sum256(result[:])
	}
	return result[:]
}

func splitOnce(s string, sep byte) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}

// TokenGenerator produces random byte tokens for sessions and CSRF secrets.
type TokenGenerator struct{}

// Generate returns n cryptographically random bytes.
func (TokenGenerator) Generate(n int) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("security: invalid token length")
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// HMACSHA256Sign returns hex-encoded HMAC-SHA256(key, message).
func HMACSHA256Sign(key, message []byte) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write(message)
	return hex.EncodeToString(m.Sum(nil))
}

// HMACSHA256Valid reports whether macHex is a valid HMAC-SHA256 of message under key.
func HMACSHA256Valid(key, message []byte, macHex string) bool {
	want, err := hex.DecodeString(macHex)
	if err != nil {
		return false
	}
	got, _ := hex.DecodeString(HMACSHA256Sign(key, message))
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}
