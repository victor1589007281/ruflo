// cache.go — Prompt Cache 结构化构建器 (G1)。
//
// 对标:
//   - Anthropic Prompt Caching (官方文档): 将稳定前缀标记可缓存, 后端复用 KV cache
//   - OpenAI cached_tokens: 响应中透传缓存命中量
//   - Kimi K2.5 "长上下文保真" 架构: 稳定前缀 + 动态后缀 分离布局
//
// 当前 QueryEngine 每轮都重新拼装 system prompt + tool_defs + memory@t0,
// 即使内容不变, 也无法享受后端的 prefix caching 优惠。本组件的职责:
//
//  1. 识别"静态前缀"(系统提示 / 工具定义 / 项目上下文 / 首轮记忆)
//  2. 计算稳定 hash 用于判断"前缀是否命中本地缓存"
//  3. 上报命中/未命中到 EngineMetrics, 供 dashboard 分析成本
//
// 本地 hash 命中 ≠ 后端 cache 命中, 但在稳定前缀的前提下, 后端命中率近似等于本地。
// 当 Anthropic API 支持显式 cache_control 字段时, 本组件的 Static() 输出可直接贴标。
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PromptCacheBuilder 稳定前缀 + 动态后缀 prompt 构建器。
type PromptCacheBuilder struct {
	mu          sync.Mutex
	prefixHash  string
	lastBuiltAt time.Time

	// 指标 (也可由外部 EngineMetrics 汇总)
	localHits   atomic.Int64
	localMisses atomic.Int64
}

// NewPromptCacheBuilder 构造新的 cache builder。
func NewPromptCacheBuilder() *PromptCacheBuilder {
	return &PromptCacheBuilder{}
}

// Build 将静态部分 + 动态部分拼装为最终 system prompt 列表。
//
// 参数:
//   - staticParts: 稳定前缀, 建议包含 (按顺序):
//       1. 系统角色 prompt
//       2. 工具定义 (工具列表)
//       3. 项目约束 (cwd, platform, style guide)
//       4. 记忆注入 (turn 0 的 MemoryStore retrieve)
//   - dynamicParts: 本轮变化内容, 建议为空或仅放最新 user hint
//
// 返回:
//   - merged: 合并后的 []string, 可直接传给 APIClient.StreamMessage
//   - cacheHit: 本地判断稳定前缀是否未变 (近似后端缓存命中)
func (b *PromptCacheBuilder) Build(staticParts []string, dynamicParts []string) (merged []string, cacheHit bool) {
	if b == nil {
		// 零值降级 — 仍然可用, 只是不追踪命中率
		return append(append([]string{}, staticParts...), dynamicParts...), false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	h := hashParts(staticParts)
	if h == b.prefixHash && b.prefixHash != "" {
		cacheHit = true
		b.localHits.Add(1)
	} else {
		cacheHit = false
		b.localMisses.Add(1)
		b.prefixHash = h
	}
	b.lastBuiltAt = time.Now()

	merged = make([]string, 0, len(staticParts)+len(dynamicParts))
	merged = append(merged, staticParts...)
	merged = append(merged, dynamicParts...)
	return merged, cacheHit
}

// HitRate 返回本地命中率 (0-1)。
func (b *PromptCacheBuilder) HitRate() float64 {
	if b == nil {
		return 0
	}
	h := b.localHits.Load()
	m := b.localMisses.Load()
	total := h + m
	if total == 0 {
		return 0
	}
	return float64(h) / float64(total)
}

// Stats 返回内部计数快照。
func (b *PromptCacheBuilder) Stats() (hits, misses int64) {
	if b == nil {
		return 0, 0
	}
	return b.localHits.Load(), b.localMisses.Load()
}

// PrefixHash 返回当前稳定前缀哈希 (调试用)。
func (b *PromptCacheBuilder) PrefixHash() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.prefixHash
}

// Reset 清空前缀指纹 (调试/测试用)。
func (b *PromptCacheBuilder) Reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.prefixHash = ""
	b.mu.Unlock()
}

// hashParts 计算 parts 的 SHA256 (hex 前 16 字符足矣)。
func hashParts(parts []string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0x1f}) // unit separator, 防止 ["a","b"] 与 ["ab",""] 碰撞
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// SplitStaticDynamic 是一个辅助函数, 将 PromptManager 返回的 system prompt 列表
// 启发式切分为 "静态前缀 + 动态后缀"。
//
// 启发式规则:
//   - 如果只有一条, 整体视为 static
//   - 否则最后一条若包含 turn-sensitive 标记 ("<context_summary>", "<recent_memory>") 归入 dynamic
//   - 其余归入 static
func SplitStaticDynamic(system []string) (static, dynamic []string) {
	if len(system) == 0 {
		return nil, nil
	}
	if len(system) == 1 {
		return system, nil
	}
	for i, s := range system {
		lower := strings.ToLower(s)
		if isDynamicMarker(lower) {
			dynamic = append(dynamic, s)
		} else {
			_ = i
			static = append(static, s)
		}
	}
	// 兜底: 如果切分后 static 为空, 整体当 static
	if len(static) == 0 {
		return system, nil
	}
	return static, dynamic
}

func isDynamicMarker(lower string) bool {
	markers := []string{
		"<context_summary>",
		"<recent_memory>",
		"<prior_experience>",
		"<soft_stop_hint>",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
