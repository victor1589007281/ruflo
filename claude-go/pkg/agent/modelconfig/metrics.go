package modelconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

// AliasMetrics 单个别名维度的聚合统计。
type AliasMetrics struct {
	Alias             string    `json:"alias"`
	Provider          string    `json:"provider"`
	ProviderName      string    `json:"providerName"`
	Calls             int64     `json:"calls"`
	Errors            int64     `json:"errors"`
	RateLimitErrors   int64     `json:"rateLimitErrors"`
	TimeoutErrors     int64     `json:"timeoutErrors"`
	OverloadedErrors  int64     `json:"overloadedErrors"`
	OtherErrors       int64     `json:"otherErrors"`
	InputTokens       int64     `json:"inputTokens"`
	OutputTokens      int64     `json:"outputTokens"`
	CacheReadTokens   int64     `json:"cacheReadTokens"`
	CacheCreateTokens int64     `json:"cacheCreateTokens"`
	TotalTokens       int64     `json:"totalTokens"`
	TotalDurationSec  float64   `json:"totalDurationSec"`
	GuardWaitSec      float64   `json:"guardWaitSec"`
	LastUpdated       time.Time `json:"lastUpdated"`
}

// AliasMetricsCollector 按模型别名聚合 LLM 调用指标。
type AliasMetricsCollector struct {
	registry *ProviderRegistry
	mu       sync.RWMutex
	data     map[string]*AliasMetrics // key=alias
	summary  *AliasMetrics            // 全量汇总 (alias="_total")
	outPath  string
}

// NewAliasMetricsCollector 创建采集器。
func NewAliasMetricsCollector(registry *ProviderRegistry, stateDir string) *AliasMetricsCollector {
	c := &AliasMetricsCollector{
		registry: registry,
		data:     make(map[string]*AliasMetrics),
		summary:  &AliasMetrics{Alias: "_total"},
		outPath:  filepath.Join(stateDir, "metrics", "alias_llm.jsonl"),
	}
	// 确保目录存在
	_ = os.MkdirAll(filepath.Dir(c.outPath), 0755)
	return c
}

// Record 实现 api.LLMMetricsHook，接收单次 LLM 调用记录。
func (c *AliasMetricsCollector) Record(rec api.LLMCallRecord) {
	// 通过原始 model 名反查 alias
	alias := c.resolveAlias(rec.Model)
	provider := c.registry.LookupProviderByAlias(alias)
	providerName := rec.Model
	if alias != rec.Model {
		providerName = c.registry.LookupProviderName(alias)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 更新 alias 维度
	m := c.data[alias]
	if m == nil {
		m = &AliasMetrics{Alias: alias, Provider: provider, ProviderName: providerName}
		c.data[alias] = m
	}
	c.applyRecord(m, rec)

	// 更新汇总维度
	c.applyRecord(c.summary, rec)
	c.summary.LastUpdated = time.Now()

	// 持久化到 JSONL
	_ = c.appendJSONL(alias, rec)
}

// applyRecord 将单次记录累加到聚合指标。
func (c *AliasMetricsCollector) applyRecord(m *AliasMetrics, rec api.LLMCallRecord) {
	m.Calls++
	m.InputTokens += int64(rec.InputTokens)
	m.OutputTokens += int64(rec.OutputTokens)
	m.CacheReadTokens += int64(rec.CacheReadTokens)
	m.CacheCreateTokens += int64(rec.CacheCreationTokens)
	m.TotalTokens += int64(rec.TotalTokens)
	m.TotalDurationSec += rec.DurationSec
	m.GuardWaitSec += rec.GuardWaitSec
	m.LastUpdated = time.Now()

	if rec.Status == "error" {
		m.Errors++
		switch rec.ErrorKind {
		case "rate_limit":
			m.RateLimitErrors++
		case "timeout":
			m.TimeoutErrors++
		case "overloaded":
			m.OverloadedErrors++
		default:
			m.OtherErrors++
		}
	}
}

// resolveAlias 通过原始 model 名反查 alias。
// 策略: 先精确匹配 alias, 再遍历所有 model 的 ProviderName。
func (c *AliasMetricsCollector) resolveAlias(rawModel string) string {
	if c.registry == nil {
		return rawModel
	}
	// 直接匹配 alias
	if c.registry.GetModel(rawModel) != nil {
		return rawModel
	}
	// 遍历所有 model 的 ProviderName（alias 后半段）
	for _, alias := range c.registry.AllAliases() {
		entry := c.registry.GetModel(alias)
		if entry != nil && entry.ProviderName == rawModel {
			return alias
		}
	}
	return rawModel
}

// appendJSONL 将单次记录追加到 JSONL 文件。
func (c *AliasMetricsCollector) appendJSONL(alias string, rec api.LLMCallRecord) error {
	line := map[string]interface{}{
		"alias":             alias,
		"model":             rec.Model,
		"status":            rec.Status,
		"errorKind":         rec.ErrorKind,
		"inputTokens":       rec.InputTokens,
		"outputTokens":      rec.OutputTokens,
		"cacheReadTokens":   rec.CacheReadTokens,
		"cacheCreateTokens": rec.CacheCreationTokens,
		"totalTokens":       rec.TotalTokens,
		"durationSec":       rec.DurationSec,
		"guardWaitSec":      rec.GuardWaitSec,
		"timestamp":         time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.Marshal(line)
	f, err := os.OpenFile(c.outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\n", b)
	return err
}

// Get 获取单个 alias 的聚合指标。
func (c *AliasMetricsCollector) Get(alias string) *AliasMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m := c.data[alias]
	if m == nil {
		return nil
	}
	// 返回副本
	cp := *m
	return &cp
}

// GetAll 获取所有 alias 的指标副本。
func (c *AliasMetricsCollector) GetAll() map[string]*AliasMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]*AliasMetrics, len(c.data))
	for k, v := range c.data {
		cp := *v
		out[k] = &cp
	}
	return out
}

// GetSummary 获取汇总指标。
func (c *AliasMetricsCollector) GetSummary() *AliasMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := *c.summary
	return &cp
}

// Snapshot 返回完整的快照 (含所有 alias + 汇总)。
func (c *AliasMetricsCollector) Snapshot() map[string]*AliasMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]*AliasMetrics, len(c.data)+1)
	for k, v := range c.data {
		cp := *v
		out[k] = &cp
	}
	scp := *c.summary
	out["_total"] = &scp
	return out
}

// Reset 清空所有聚合数据 (测试用)。
func (c *AliasMetricsCollector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = make(map[string]*AliasMetrics)
	c.summary = &AliasMetrics{Alias: "_total"}
}
