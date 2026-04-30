package modelconfig

import (
	"os"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

func buildMetricsRegistry() *ProviderRegistry {
	r := NewProviderRegistry()
	r.RegisterProvider(ProviderConfig{
		Name: "dashscope",
		Models: map[string]ModelConfig{
			"dashscope:qwen3.6-plus": {},
			"dashscope:qwen3.5-plus": {},
		},
	})
	return r
}

func TestAliasMetricsCollector_RecordAndGet(t *testing.T) {
	c := NewAliasMetricsCollector(buildMetricsRegistry(), t.TempDir())

	c.Record(api.LLMCallRecord{
		Model:        "qwen3.6-plus",
		Status:       "success",
		InputTokens:  1000,
		OutputTokens: 500,
		TotalTokens:  1500,
		DurationSec:  2.5,
	})

	c.Record(api.LLMCallRecord{
		Model:        "qwen3.6-plus",
		Status:       "error",
		ErrorKind:    "rate_limit",
		InputTokens:  0,
		OutputTokens: 0,
		TotalTokens:  0,
		DurationSec:  1.0,
	})

	c.Record(api.LLMCallRecord{
		Model:        "qwen3.5-plus",
		Status:       "success",
		InputTokens:  200,
		OutputTokens: 100,
		TotalTokens:  300,
		DurationSec:  0.5,
	})

	// 验证 dashscope:qwen3.6-plus
	m := c.Get("dashscope:qwen3.6-plus")
	if m == nil {
		t.Fatal("expected dashscope:qwen3.6-plus metrics")
	}
	if m.Calls != 2 {
		t.Fatalf("expected 2 calls, got %d", m.Calls)
	}
	if m.Errors != 1 {
		t.Fatalf("expected 1 error, got %d", m.Errors)
	}
	if m.RateLimitErrors != 1 {
		t.Fatalf("expected 1 rate limit error, got %d", m.RateLimitErrors)
	}
	if m.InputTokens != 1000 {
		t.Fatalf("expected 1000 input tokens, got %d", m.InputTokens)
	}
	if m.TotalDurationSec != 3.5 {
		t.Fatalf("expected 3.5s duration, got %f", m.TotalDurationSec)
	}

	// 验证 dashscope:qwen3.5-plus
	m = c.Get("dashscope:qwen3.5-plus")
	if m == nil || m.Calls != 1 {
		t.Fatalf("expected dashscope:qwen3.5-plus with 1 call, got %+v", m)
	}

	// 验证汇总
	summary := c.GetSummary()
	if summary.Calls != 3 {
		t.Fatalf("expected 3 total calls, got %d", summary.Calls)
	}
	if summary.InputTokens != 1200 {
		t.Fatalf("expected 1200 total input tokens, got %d", summary.InputTokens)
	}
}

func TestAliasMetricsCollector_UnknownModel(t *testing.T) {
	c := NewAliasMetricsCollector(buildMetricsRegistry(), t.TempDir())

	c.Record(api.LLMCallRecord{
		Model:        "unknown-model",
		Status:       "success",
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
		DurationSec:  1.0,
	})

	m := c.Get("unknown-model")
	if m == nil || m.Calls != 1 {
		t.Fatalf("expected unknown-model with 1 call, got %+v", m)
	}
}

func TestAliasMetricsCollector_ErrorKinds(t *testing.T) {
	c := NewAliasMetricsCollector(buildMetricsRegistry(), t.TempDir())

	tests := []struct {
		kind            string
		expectRateLimit int64
		expectTimeout   int64
		expectOverload  int64
		expectOther     int64
	}{
		{"rate_limit", 1, 0, 0, 0},
		{"timeout", 0, 1, 0, 0},
		{"overloaded", 0, 0, 1, 0},
		{"client", 0, 0, 0, 1},
		{"server", 0, 0, 0, 1},
		{"", 0, 0, 0, 1},
	}

	for _, tc := range tests {
		c.Record(api.LLMCallRecord{
			Model:     "qwen3.6-plus",
			Status:    "error",
			ErrorKind: tc.kind,
		})
	}

	m := c.Get("dashscope:qwen3.6-plus")
	if m == nil {
		t.Fatal("expected metrics")
	}
	if m.RateLimitErrors != 1 {
		t.Fatalf("expected 1 rate_limit, got %d", m.RateLimitErrors)
	}
	if m.TimeoutErrors != 1 {
		t.Fatalf("expected 1 timeout, got %d", m.TimeoutErrors)
	}
	if m.OverloadedErrors != 1 {
		t.Fatalf("expected 1 overloaded, got %d", m.OverloadedErrors)
	}
	if m.OtherErrors != 3 {
		t.Fatalf("expected 3 other errors, got %d", m.OtherErrors)
	}
}

func TestAliasMetricsCollector_Snapshot(t *testing.T) {
	c := NewAliasMetricsCollector(buildMetricsRegistry(), t.TempDir())

	c.Record(api.LLMCallRecord{
		Model:       "qwen3.6-plus",
		Status:      "success",
		DurationSec: 1.0,
	})

	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries (1 alias + _total), got %d", len(snap))
	}
	if snap["dashscope:qwen3.6-plus"] == nil {
		t.Fatal("expected dashscope:qwen3.6-plus in snapshot")
	}
	if snap["_total"] == nil {
		t.Fatal("expected _total in snapshot")
	}
}

func TestAliasMetricsCollector_Reset(t *testing.T) {
	c := NewAliasMetricsCollector(buildMetricsRegistry(), t.TempDir())

	c.Record(api.LLMCallRecord{
		Model:  "qwen3.6-plus",
		Status: "success",
	})

	if c.Get("dashscope:qwen3.6-plus").Calls != 1 {
		t.Fatal("expected 1 call before reset")
	}

	c.Reset()

	if c.Get("dashscope:qwen3.6-plus") != nil {
		t.Fatal("expected nil after reset")
	}
	if c.GetSummary().Calls != 0 {
		t.Fatalf("expected 0 total calls after reset, got %d", c.GetSummary().Calls)
	}
}

func TestAliasMetricsCollector_CacheTokens(t *testing.T) {
	c := NewAliasMetricsCollector(buildMetricsRegistry(), t.TempDir())

	c.Record(api.LLMCallRecord{
		Model:               "qwen3.6-plus",
		Status:              "success",
		CacheReadTokens:     5000,
		CacheCreationTokens: 2000,
		InputTokens:         1000,
		OutputTokens:        500,
		TotalTokens:         8500,
	})

	m := c.Get("dashscope:qwen3.6-plus")
	if m.CacheReadTokens != 5000 {
		t.Fatalf("expected 5000 cache read tokens, got %d", m.CacheReadTokens)
	}
	if m.CacheCreateTokens != 2000 {
		t.Fatalf("expected 2000 cache create tokens, got %d", m.CacheCreateTokens)
	}
	if m.TotalTokens != 8500 {
		t.Fatalf("expected 8500 total tokens, got %d", m.TotalTokens)
	}
}

func TestAliasMetricsCollector_JSONLPersistence(t *testing.T) {
	c := NewAliasMetricsCollector(buildMetricsRegistry(), t.TempDir())

	c.Record(api.LLMCallRecord{
		Model:       "qwen3.6-plus",
		Status:      "success",
		InputTokens: 100,
		OutputTokens: 50,
		TotalTokens: 150,
		DurationSec: 1.0,
		Timestamp:   time.Now(),
	})

	// 文件应该已创建
	if _, err := os.Stat(c.outPath); os.IsNotExist(err) {
		t.Fatal("expected JSONL file to exist")
	}
}
