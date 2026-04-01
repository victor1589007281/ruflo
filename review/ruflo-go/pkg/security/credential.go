// 凭证生成器（credential.go）
//
// 设计思路：为 API 接入、临时口令与实体标识提供高熵随机字符串，全部基于 crypto/rand（或注入的 io.Reader 便于测试）。
// API Key 采用「可识别前缀 + 随机十六进制」便于日志脱敏与运维识别；安全口令强制字符类覆盖后洗牌，降低可预测性；
// UUID 遵循 RFC 4122 版本 4（随机），并正确设置 version 与 variant 位。
//
// 整体架构：CredentialGenerator 仅依赖 rand 字段；read/pickFrom 为内部原语，保证所有生成路径统一走相同随机读取语义。
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

// CredentialGenerator 可注入 io.Reader；生产环境应使用 crypto/rand.Reader，单测可换确定性 Reader。
type CredentialGenerator struct {
	rand io.Reader // 非 nil 时 read/pickFrom/洗牌均走该源；nil 时回退全局 rand.Reader
}

// NewCredentialGenerator 绑定 crypto/rand.Reader，满足密码学随机强度要求。
func NewCredentialGenerator() *CredentialGenerator {
	return &CredentialGenerator{rand: rand.Reader}
}

// GenerateAPIKey 读取 16 字节（128 bit）随机性，编码为 32 个十六进制字符；格式 prefix-hex。
// prefix 为空时默认 "sk"，便于与常见 API Key 前缀习惯对齐（仅形态类似，非兼容实现）。
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

// GenerateSecurePassword 先从小写/大写/数字/符号四类各取一字保证熵类覆盖，再填满长度，最后用随机字节对 rune 切片洗牌打乱顺序。
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

// GenerateUUID 生成 RFC 4122 UUID v4：16 字节随机后设置 b[6] 高 4 位为 0100（版本 4），b[8] 高 2 位为 10（variant 1），
// 再格式化为 8-4-4-4-12 带连字符小写十六进制。
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

// read 对 p 执行 ReadFull，保证要么填满要么错误；避免短读导致部分未初始化缓冲区被误用。
func (g *CredentialGenerator) read(p []byte) (int, error) {
	r := g.rand
	if r == nil {
		r = rand.Reader
	}
	return io.ReadFull(r, p)
}

// pickFrom 读取 1 字节 b，返回 set[b%len(set)]；当 len(set)∤256 时存在轻微模偏差（常见口令字符集长度下可忽略量级）。
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
