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

const (
	llmCacheMaxEntries = 1000
	llmCacheTTL        = time.Hour
)

// LLMMetrics aggregates call observability.
type LLMMetrics struct {
	mu           sync.Mutex
	Calls        int64
	CacheHits    int64
	CacheMisses  int64
	ErrorCalls   int64
	TotalLatency time.Duration
	TotalTokens  int64
	TotalCostUSD float64
}

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

type llmCacheEntry struct {
	resp      *api.LLMResponse
	expiresAt time.Time
}

// responseLRU is a simple LRU with TTL (capacity llmCacheMaxEntries).
type responseLRU struct {
	mu    sync.Mutex
	m     map[string]*listNode
	head  *listNode
	tail  *listNode
	count int
}

type listNode struct {
	key   string
	entry llmCacheEntry
	prev  *listNode
	next  *listNode
}

func newResponseLRU() *responseLRU {
	return &responseLRU{m: make(map[string]*listNode)}
}

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

func (c *responseLRU) moveFront(n *listNode) {
	if c.head == n {
		return
	}
	c.removeNode(n)
	c.pushFront(n)
}

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

// LLMHookBundle wires pre/post LLM hooks with shared cache and metrics.
type LLMHookBundle struct {
	cache   *responseLRU
	metrics *LLMMetrics
	bank    *ReasoningBank
}

// NewLLMHookBundle constructs hooks with optional reasoning bank for pattern extraction.
func NewLLMHookBundle(bank *ReasoningBank) *LLMHookBundle {
	return &LLMHookBundle{
		cache:   newResponseLRU(),
		metrics: &LLMMetrics{},
		bank:    bank,
	}
}

// generateCacheKey builds a stable base64 key from provider, model, and messages.
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

func getCached(b *LLMHookBundle, key string) (*api.LLMResponse, bool) {
	if b == nil || b.cache == nil {
		return nil, false
	}
	return b.cache.get(key)
}

func setCache(b *LLMHookBundle, key string, resp *api.LLMResponse) {
	if b == nil || b.cache == nil || resp == nil {
		return
	}
	cp := *resp
	b.cache.set(key, &cp)
}

// PreLLMCallHook checks cache, applies provider default optimizations, updates metrics on cache hit.
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

// MetricsSnapshot returns LLM hook counters for observability.
func (b *LLMHookBundle) MetricsSnapshot() map[string]any {
	if b == nil || b.metrics == nil {
		return map[string]any{}
	}
	return b.metrics.Snapshot()
}

// ErrorLLMCallHook records failed LLM call metrics (provider/model for future tagging).
func (b *LLMHookBundle) ErrorLLMCallHook(provider, model string, err error) {
	_ = provider
	_ = model
	_ = err
	if b == nil || b.metrics == nil {
		return
	}
	b.metrics.recordErrorCall()
}

// ClearCache drops all cached LLM responses.
func (b *LLMHookBundle) ClearCache() {
	if b == nil || b.cache == nil {
		return
	}
	b.cache.clear()
}

// PostLLMCallHook stores response in LRU cache, records latency/tokens/cost, extracts coarse patterns from long text.
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

// ExtractSnippetPatterns splits long assistant outputs into lightweight text patterns (line-based).
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
