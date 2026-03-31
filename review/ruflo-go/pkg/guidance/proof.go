package guidance

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// ProofEnvelope is one link in an append-only proof chain.
type ProofEnvelope struct {
	Hash         string         `json:"hash"`
	PreviousHash string         `json:"previous_hash"`
	Payload      map[string]any `json:"payload"`
	Timestamp    time.Time      `json:"timestamp"`
	Signature    string         `json:"signature,omitempty"`
}

type proofWire struct {
	PreviousHash string         `json:"previous_hash"`
	Payload      map[string]any `json:"payload"`
	Timestamp    time.Time      `json:"timestamp"`
}

// ProofChain is an HMAC-signed hash chain of envelopes.
type ProofChain struct {
	secret   []byte
	chain    []ProofEnvelope
	lastHash string
}

// NewProofChain creates a chain using the given UTF-8 secret for HMAC-SHA256.
func NewProofChain(secret string) *ProofChain {
	return &ProofChain{secret: []byte(secret), lastHash: genesisHash()}
}

func genesisHash() string {
	h := sha256.Sum256([]byte("ruflo-proof-genesis"))
	return hex.EncodeToString(h[:])
}

// Append adds a payload envelope, chaining hashes and signing with HMAC(secret, hash || canonicalJSON).
func (c *ProofChain) Append(payload map[string]any) (ProofEnvelope, error) {
	if c == nil {
		return ProofEnvelope{}, fmt.Errorf("guidance: nil proof chain")
	}
	if payload == nil {
		payload = map[string]any{}
	}
	ts := time.Now().UTC()
	body, err := json.Marshal(proofWire{PreviousHash: c.lastHash, Payload: payload, Timestamp: ts})
	if err != nil {
		return ProofEnvelope{}, err
	}
	pre := append([]byte(c.lastHash), body...)
	sum := sha256.Sum256(pre)
	h := hex.EncodeToString(sum[:])
	sig := signEnvelope(c.secret, h, body)
	env := ProofEnvelope{
		Hash:         h,
		PreviousHash: c.lastHash,
		Payload:      payload,
		Timestamp:    ts,
		Signature:    sig,
	}
	c.chain = append(c.chain, env)
	c.lastHash = h
	return env, nil
}

func signEnvelope(secret []byte, hashHex string, canonical []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(hashHex))
	_, _ = mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks full chain integrity: hash links and HMAC signatures.
func (c *ProofChain) Verify() bool {
	if c == nil || len(c.chain) == 0 {
		return true
	}
	prev := genesisHash()
	for i := range c.chain {
		env := c.chain[i]
		if env.PreviousHash != prev {
			return false
		}
		body, err := json.Marshal(proofWire{PreviousHash: env.PreviousHash, Payload: env.Payload, Timestamp: env.Timestamp})
		if err != nil {
			return false
		}
		sum := sha256.Sum256(append([]byte(env.PreviousHash), body...))
		want := hex.EncodeToString(sum[:])
		if want != env.Hash {
			return false
		}
		sigWant := signEnvelope(c.secret, env.Hash, body)
		a, e1 := hex.DecodeString(sigWant)
		b, e2 := hex.DecodeString(env.Signature)
		if e1 != nil || e2 != nil || len(a) != len(b) {
			return false
		}
		if subtle.ConstantTimeCompare(a, b) != 1 {
			return false
		}
		prev = env.Hash
	}
	return prev == c.lastHash
}

// Envelopes returns a defensive copy of the chain.
func (c *ProofChain) Envelopes() []ProofEnvelope {
	if c == nil {
		return nil
	}
	out := make([]ProofEnvelope, len(c.chain))
	copy(out, c.chain)
	return out
}
