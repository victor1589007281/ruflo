// 本文件实现密码哈希（每密码独立盐 + 多次 SHA-256 迭代拉伸）、随机字节令牌、以及 HMAC-SHA256 签名与常数时间校验。
// 说明：生产环境密码存储更推荐使用 bcrypt/argon2；此处为轻量可审计实现。随机 Token 与 HMAC 可满足会话密钥、API 防篡改等场景。
package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

// PasswordHasher 使用每密码随机盐与固定次数的 SHA-256 链式迭代增加暴力破解成本，缓解彩虹表与弱口令离线破解。
type PasswordHasher struct {
	iterations int // 迭代轮数，默认 10000
	saltLen    int // 盐字节长度，默认 32
}

// NewPasswordHasher 返回默认迭代次数与盐长度的哈希器。
func NewPasswordHasher() *PasswordHasher {
	return &PasswordHasher{iterations: 10000, saltLen: 32}
}

// Hash 生成随机盐，对 password||salt 做 pbkdf2 式链式 SHA-256，返回 "saltHex:hashHex" 便于落库。
func (h *PasswordHasher) Hash(password string) (string, error) {
	salt := make([]byte, h.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := h.pbkdf2(password, salt)
	return fmt.Sprintf("%s:%s", hex.EncodeToString(salt), hex.EncodeToString(hash)), nil
}

// Verify 解析存储串，用相同盐与迭代重算摘要，subtle.ConstantTimeCompare 比较防时序旁路。
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

// pbkdf2 单块派生：非标准 PBKDF2 多块扩展，而是固定 32 字节链式哈希，计算量随 iterations 线性增长。
func (h *PasswordHasher) pbkdf2(password string, salt []byte) []byte {
	result := sha256.Sum256(append([]byte(password), salt...))
	for i := 1; i < h.iterations; i++ {
		result = sha256.Sum256(result[:])
	}
	return result[:]
}

// splitOnce 线性扫描首个 sep，O(n)；用于解析仅含一个 ':' 的存储格式，避免 strings.Split 丢弃尾部空段语义问题。
func splitOnce(s string, sep byte) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}

// TokenGenerator 零状态占位类型：方法集提供命名空间，便于与 PasswordHasher 并列使用。
type TokenGenerator struct{}

// Generate 使用 crypto/rand.Read 填满 n 字节；n<=0 返回错误。熵源质量依赖操作系统 CSPRNG。
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

// HMACSHA256Sign 计算 MAC = H(K ‖ opad, H(K ‖ ipad, message))（标准 HMAC 构造），输出小写十六进制字符串便于 HTTP 头或 JSON 传输。
func HMACSHA256Sign(key, message []byte) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write(message)
	return hex.EncodeToString(m.Sum(nil))
}

// HMACSHA256Valid 先解码期望 MAC，再计算本地 MAC 的十六进制并解码为字节（双次 hex 为与 Sign 对齐）；
// 长度对齐后 ConstantTimeCompare。注意：勿将 macHex 来自用户时跳过长度检查（已处理）。
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
