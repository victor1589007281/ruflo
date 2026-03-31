package security

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

const (
	lowerChars  = "abcdefghijklmnopqrstuvwxyz"
	upperChars  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digitChars  = "0123456789"
	symbolChars = "!@#$%^&*-_=+"
)

// CredentialGenerator creates API keys, passwords, and UUIDs.
type CredentialGenerator struct {
	rand io.Reader
}

// NewCredentialGenerator uses crypto/rand by default.
func NewCredentialGenerator() *CredentialGenerator {
	return &CredentialGenerator{rand: rand.Reader}
}

// GenerateAPIKey returns prefix + "-" + random hex (16 random bytes).
func (g *CredentialGenerator) GenerateAPIKey(prefix string) (string, error) {
	if prefix == "" {
		prefix = "sk"
	}
	b := make([]byte, 16)
	if _, err := g.read(b); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(b), nil
}

// GenerateSecurePassword returns a random password with mixed character classes.
func (g *CredentialGenerator) GenerateSecurePassword(length int) (string, error) {
	if length < 8 {
		length = 16
	}
	all := lowerChars + upperChars + digitChars + symbolChars
	var out strings.Builder
	out.Grow(length)
	// Ensure at least one of each class
	classes := []string{lowerChars, upperChars, digitChars, symbolChars}
	for _, cset := range classes {
		ch, err := g.pickFrom(cset)
		if err != nil {
			return "", err
		}
		out.WriteByte(ch)
	}
	for out.Len() < length {
		ch, err := g.pickFrom(all)
		if err != nil {
			return "", err
		}
		out.WriteByte(ch)
	}
	// Shuffle
	runes := []rune(out.String())
	for i := len(runes) - 1; i > 0; i-- {
		jBig := make([]byte, 1)
		if _, err := g.read(jBig); err != nil {
			return "", err
		}
		j := int(jBig[0]) % (i + 1)
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes), nil
}

// GenerateUUID returns a random UUID version 4 (RFC 4122).
func (g *CredentialGenerator) GenerateUUID() (string, error) {
	var b [16]byte
	if _, err := g.read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

func (g *CredentialGenerator) read(p []byte) (int, error) {
	r := g.rand
	if r == nil {
		r = rand.Reader
	}
	return io.ReadFull(r, p)
}

func (g *CredentialGenerator) pickFrom(set string) (byte, error) {
	if set == "" {
		return 0, fmt.Errorf("security: empty charset")
	}
	var idx [1]byte
	if _, err := g.read(idx[:]); err != nil {
		return 0, err
	}
	return set[int(idx[0])%len(set)], nil
}
