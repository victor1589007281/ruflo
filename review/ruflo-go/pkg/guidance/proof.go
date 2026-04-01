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

// 本文件：证明链（类区块链不可篡改审计）。每条 envelope 的 Hash = SHA256(prevHash || canonicalJSON(body))；
// Signature = HMAC-SHA256(secret, hashHex || canonicalJSON)；Verify 重算并 constant-time 比较签名。

// ProofEnvelope 追加式证明链中的一环（含链式哈希与 HMAC 签名）。
type ProofEnvelope struct {
	Hash         string         `json:"hash"`                // 本节点内容哈希（十六进制）
	PreviousHash string         `json:"previous_hash"`       // 父节点哈希
	Payload      map[string]any `json:"payload"`             // 业务载荷
	Timestamp    time.Time      `json:"timestamp"`           // UTC 时间
	Signature    string         `json:"signature,omitempty"` // HMAC 签名（十六进制）
}

// proofWire JSON 序列化用的有线结构（不含 Hash/Signature，与 Verify 重算一致）。
type proofWire struct {
	PreviousHash string         `json:"previous_hash"`
	Payload      map[string]any `json:"payload"`
	Timestamp    time.Time      `json:"timestamp"`
}

// ProofChain 持有共享密钥与内存中的 envelope 切片，维护 lastHash 作为尾指针。
type ProofChain struct {
	secret   []byte          // HMAC 密钥字节
	chain    []ProofEnvelope // 追加存储
	lastHash string          // 当前链尾哈希
}

// NewProofChain 使用 UTF-8 secret 初始化链，genesis 为固定字符串的 SHA256 十六进制。
func NewProofChain(secret string) *ProofChain {
	return &ProofChain{secret: []byte(secret), lastHash: genesisHash()}
}

// genesisHash 链起点哈希（常量盐值 SHA256）。
func genesisHash() string {
	h := sha256.Sum256([]byte("ruflo-proof-genesis"))
	return hex.EncodeToString(h[:])
}

// Append 将 payload 与 prevHash、Timestamp 序列化为 proofWire，计算 Hash 与 Signature，追加到 chain 并更新 lastHash。
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

// signEnvelope 计算 HMAC-SHA256(secret, hashHex || canonical)。
func signEnvelope(secret []byte, hashHex string, canonical []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(hashHex))
	_, _ = mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify 自 genesis 起逐节校验 PreviousHash 链接、重算 Hash 与 Signature（subtle.ConstantTimeCompare）。
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

// Envelopes 返回链的浅拷贝切片（Envelope 内含 map 仍为引用类型）。
func (c *ProofChain) Envelopes() []ProofEnvelope {
	if c == nil {
		return nil
	}
	out := make([]ProofEnvelope, len(c.chain))
	copy(out, c.chain)
	return out
}
