package security

import (
	"strings"
	"testing"
)

func TestPasswordHasher_HashAndVerify(t *testing.T) {
	t.Parallel()
	h := NewPasswordHasher()
	stored, err := h.Hash("correct horse battery staple")
	if err != nil || stored == "" {
		t.Fatalf("Hash: %v %q", err, stored)
	}
	if !h.Verify("correct horse battery staple", stored) {
		t.Fatal("Verify should succeed")
	}
}

func TestPasswordHasher_WrongPassword(t *testing.T) {
	t.Parallel()
	h := NewPasswordHasher()
	stored, err := h.Hash("secret")
	if err != nil {
		t.Fatal(err)
	}
	if h.Verify("wrong", stored) {
		t.Fatal("Verify should fail for wrong password")
	}
}

func TestTokenGenerator(t *testing.T) {
	t.Parallel()
	var g TokenGenerator
	b, err := g.Generate(32)
	if err != nil || len(b) != 32 {
		t.Fatalf("Generate: %v len=%d", err, len(b))
	}
	b2, _ := g.Generate(32)
	if string(b) == string(b2) {
		t.Fatal("expected distinct tokens")
	}
}

func TestHMACValidation(t *testing.T) {
	t.Parallel()
	key := []byte("k1")
	msg := []byte("payload")
	mac := HMACSHA256Sign(key, msg)
	if !HMACSHA256Valid(key, msg, mac) {
		t.Fatal("valid mac rejected")
	}
	if HMACSHA256Valid([]byte("other"), msg, mac) {
		t.Fatal("wrong key should fail")
	}
	if HMACSHA256Valid(key, []byte("other"), mac) {
		t.Fatal("tampered message should fail")
	}
}

func TestCredentialGenerator_APIKey(t *testing.T) {
	t.Parallel()
	g := NewCredentialGenerator()
	k, err := g.GenerateAPIKey("pk")
	if err != nil || !strings.HasPrefix(k, "pk-") || len(k) < 10 {
		t.Fatalf("api key: %v %q", err, k)
	}
}

func TestCredentialGenerator_UUID(t *testing.T) {
	t.Parallel()
	g := NewCredentialGenerator()
	u, err := g.GenerateUUID()
	if err != nil || len(u) != 36 {
		t.Fatalf("uuid: %v %q", err, u)
	}
	parts := strings.Split(u, "-")
	if len(parts) != 5 {
		t.Fatalf("uuid format: %q", u)
	}
}
