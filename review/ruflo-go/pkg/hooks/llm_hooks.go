package hooks

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// 本文件：LLM 调用拦截器（Pre/Post/Error）。Pre 查 LRU+TTL 缓存、未命中时按提供商补默认 temperature；
// Post 写回缓存并累计延迟/Token/费用，长文本可抽取粗粒度模式写入 ReasoningBank。

const (
	llmCacheMaxEntries = 1000      // 响应 LRU 最大条目数
	llmCacheTTL        = time.Hour // 缓存条目存活时间
)

// LLMMetrics LLM 调用可观测性聚合：调用次数、缓存命中、错误、累计延迟与成本。
type LLMMetrics struct {
	mu           sync.Mutex    // 保护以下字段
	Calls        int64         // 总调用次数（含缓存命中在 record 中的计数策略：Pre 中 hit/miss 各算）
	CacheHits    int64         // 缓存命中次数
	CacheMisses  int64         // 缓存未命中次数
	ErrorCalls   int64         // ErrorLLMCallHook 记录的错误次数
	TotalLatency time.Duration // 累计延迟（Post 与 Pre 命中路径分别贡献）
	TotalTokens  int64         // 累计 Token
	TotalCostUSD float64       // 累计费用（美元）
}

// record 在锁内更新一次调用统计；hit 为 true 时增加 CacheHits，否则 CacheMisses。
func (m *LLMMetrics) record(hit bool, lat time.Duration, tokens int, cost float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls++
	if hit {
		m.CacheHits++
	} else {
		m.CacheMisses++
	}
	m.TotalLatency += lat
	m.TotalTokens += int64(tokens)
	m.TotalCostUSD += cost
}

func (m *LLMMetrics) recordErrorCall() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ErrorCalls++
}

// Snapshot returns a copy of counters.
func (m *LLMMetrics) Snapshot() map[string]any {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]any{
		"calls":          m.Calls,
		"cache_hits":     m.CacheHits,
		"cache_misses":   m.CacheMisses,
		"error_calls":    m.ErrorCalls,
		"total_latency":  m.TotalLatency.String(),
		"total_tokens":   m.TotalTokens,
		"total_cost_usd": m.TotalCostUSD,
	}
}

// llmCacheEntry 单条缓存：响应体与过期时间。
type llmCacheEntry struct {
	resp      *api.LLMResponse // 缓存的 LLM 响应
	expiresAt time.Time        // 绝对过期时刻
}

// responseLRU 带 TTL 的简单 LRU：哈希表 + 双向链表头尾，容量上限 llmCacheMaxEntries。
type responseLRU struct {
	mu    sync.Mutex           // 保护结构与 map
	m     map[string]*listNode // key -> 链表节点
	head  *listNode            // 最近使用端
	tail  *listNode            // 最久未使用端
	count int                  // 当前节点数
}

// listNode LRU 链表节点。
type listNode struct {
	key   string        // 缓存键
	entry llmCacheEntry // 条目
	prev  *listNode
	next  *listNode
}

// newResponseLRU 创建空 LRU。
func newResponseLRU() *responseLRU {
	return &responseLRU{m: make(map[string]*listNode)}
}

// get 若命中且未过期则移到链表头并返回响应；过期则摘除节点。
func (c *responseLRU) get(key string) (*api.LLMResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(n.entry.expiresAt) {
		c.removeNode(n)
		delete(c.m, key)
		return nil, false
	}
	c.moveFront(n)
	resp := n.entry.resp
	return resp, true
}

// set 插入或更新键：更新过期时间并移到头；超容量时逐出 tail。
func (c *responseLRU) set(key string, resp *api.LLMResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if n, ok := c.m[key]; ok {
		n.entry = llmCacheEntry{resp: resp, expiresAt: now.Add(llmCacheTTL)}
		c.moveFront(n)
		return
	}
	for c.count >= llmCacheMaxEntries && c.tail != nil {
		c.removeNode(c.tail)
	}
	n := &listNode{key: key, entry: llmCacheEntry{resp: resp, expiresAt: now.Add(llmCacheTTL)}}
	c.m[key] = n
	c.pushFront(n)
}

// pushFront 将节点插到链表头部并递增 count。
func (c *responseLRU) pushFront(n *listNode) {
	n.prev = nil
	n.next = c.head
	if c.head != nil {
		c.head.prev = n
	}
	c.head = n
	if c.tail == nil {
		c.tail = n
	}
	c.m[n.key] = n
	c.count++
}

// removeNode 从双向链表中摘除节点并递减 count。
func (c *responseLRU) removeNode(n *listNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		c.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		c.tail = n.prev
	}
	n.prev, n.next = nil, nil
	delete(c.m, n.key)
	c.count--
}

// moveFront 将已存在节点移到头部（先 remove 再 pushFront）。
func (c *responseLRU) moveFront(n *listNode) {
	if c.head == n {
		return
	}
	c.removeNode(n)
	c.pushFront(n)
}

// clear 清空全部缓存条目。
func (c *responseLRU) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = make(map[string]*listNode)
	c.head, c.tail = nil, nil
	c.count = 0
}

// LLMHookBundle 聚合 Pre/Post/Error 钩子共享的 LRU 缓存、指标与可选 ReasoningBank。
type LLMHookBundle struct {
	cache   *responseLRU   // 响应 LRU 缓存
	metrics *LLMMetrics    // 调用指标
	bank    *ReasoningBank // 可选：长响应写入粗粒度模式
}

// NewLLMHookBundle 构造钩子束；bank 可为 nil。
func NewLLMHookBundle(bank *ReasoningBank) *LLMHookBundle {
	return &LLMHookBundle{
		cache:   newResponseLRU(),
		metrics: &LLMMetrics{},
		bank:    bank,
	}
}

// generateCacheKey 对 provider、model、messages 的 JSON 序列化做 SHA256，再 Base64 编码为缓存键。
func generateCacheKey(provider api.LLMProvider, model string, messages []api.LLMMessage) string {
	h := sha256.New()
	_, _ = h.Write([]byte(string(provider)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(model))
	_, _ = h.Write([]byte{0})
	b, _ := json.Marshal(messages)
	_, _ = h.Write(b)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// getCached 从 bundle 的 LRU 读取缓存响应。
func getCached(b *LLMHookBundle, key string) (*api.LLMResponse, bool) {
	if b == nil || b.cache == nil {
		return nil, false
	}
	return b.cache.get(key)
}

// setCache 写入响应副本到 LRU（避免外部修改共享指针）。
func setCache(b *LLMHookBundle, key string, resp *api.LLMResponse) {
	if b == nil || b.cache == nil || resp == nil {
		return
	}
	cp := *resp
	b.cache.set(key, &cp)
}

// PreLLMCallHook 调用前拦截：生成 key，命中则更新指标并返回 (req, cached, true)；
// 未命中则按 Provider 为 Temperature==0 填默认温度，并 record miss。
func (b *LLMHookBundle) PreLLMCallHook(req *api.LLMRequest) (*api.LLMRequest, *api.LLMResponse, bool) {
	if req == nil {
		return nil, nil, false
	}
	key := generateCacheKey(req.Provider, req.Model, req.Messages)
	if cached, ok := getCached(b, key); ok {
		b.metrics.record(true, 0, cached.Usage.TotalTokens, 0)
		return req, cached, true
	}
	b.metrics.record(false, 0, 0, 0)
	switch req.Provider {
	case api.LLMProviderAnthropic:
		if req.Temperature == 0 {
			req.Temperature = 0.7
		}
	case api.LLMProviderOpenAI, api.LLMProviderAzure:
		if req.Temperature == 0 {
			req.Temperature = 0.8
		}
	case api.LLMProviderGoogle, api.LLMProviderOllama:
		if req.Temperature == 0 {
			req.Temperature = 0.7
		}
	}
	return req, nil, false
}

// MetricsSnapshot 返回 LLM 指标快照。
func (b *LLMHookBundle) MetricsSnapshot() map[string]any {
	if b == nil || b.metrics == nil {
		return map[string]any{}
	}
	return b.metrics.Snapshot()
}

// ErrorLLMCallHook 记录失败调用次数（provider/model/err 预留扩展，当前仅 err 未使用）。
func (b *LLMHookBundle) ErrorLLMCallHook(provider, model string, err error) {
	_ = provider
	_ = model
	_ = err
	if b == nil || b.metrics == nil {
		return
	}
	b.metrics.recordErrorCall()
}

// ClearCache 清空 LRU 中所有 LLM 响应缓存。
func (b *LLMHookBundle) ClearCache() {
	if b == nil || b.cache == nil {
		return
	}
	b.cache.clear()
}

// PostLLMCallHook 在调用成功后：累加 Calls/延迟/Token/费用，写入 LRU（key 通常与 Pre 中 generateCacheKey 一致）；
// 若绑定 bank 且响应文本超过 2048 字符，则 StorePattern 一条粗粒度 long-response 模式。
func (b *LLMHookBundle) PostLLMCallHook(key string, resp *api.LLMResponse, latency time.Duration, costUSD float64) {
	if b == nil || resp == nil {
		return
	}
	tokens := resp.Usage.TotalTokens
	if tokens == 0 {
		tokens = resp.Usage.InputTokens + resp.Usage.OutputTokens
	}
	b.metrics.mu.Lock()
	b.metrics.TotalLatency += latency
	b.metrics.TotalTokens += int64(tokens)
	b.metrics.TotalCostUSD += costUSD
	b.metrics.Calls++
	b.metrics.mu.Unlock()
	setCache(b, key, resp)
	if b.bank != nil && len(resp.Text) > 2048 {
		p := &GuidancePattern{
			Strategy:   "long-response",
			Domain:     "llm",
			Quality:    0.5,
			UsageCount: 1,
		}
		_, _ = b.bank.StorePattern(p)
	}
}

// ExtractSnippetPatterns 将长文本按行切分，取长度≥20 的非空行最多 12 条，作为轻量级模式片段。
func ExtractSnippetPatterns(text string) []string {
	const maxLines = 12
	lines := strings.Split(text, "\n")
	var out []string
	for _, ln := range lines {
		s := strings.TrimSpace(ln)
		if len(s) < 20 {
			continue
		}
		out = append(out, s)
		if len(out) >= maxLines {
			break
		}
	}
	return out
}
