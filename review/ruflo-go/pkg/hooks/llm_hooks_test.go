package hooks

import (
	"errors"
	"testing"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

func TestErrorLLMCallHook(t *testing.T) {
	t.Parallel()
	b := NewLLMHookBundle(nil)
	b.ErrorLLMCallHook("anthropic", "claude-3", errors.New("rate limited"))
	snap := b.MetricsSnapshot()
	if snap["error_calls"].(int64) != 1 {
		t.Fatalf("error_calls: %#v", snap["error_calls"])
	}
}

func TestClearCache(t *testing.T) {
	t.Parallel()
	b := NewLLMHookBundle(nil)
	req := &api.LLMRequest{
		Provider: api.LLMProviderAnthropic,
		Model:    "claude",
		Messages: []api.LLMMessage{{Role: "user", Content: "ping"}},
	}
	_, _, hit1 := b.PreLLMCallHook(req)
	if hit1 {
		t.Fatal("expected first call cache miss")
	}
	key := generateCacheKey(req.Provider, req.Model, req.Messages)
	b.PostLLMCallHook(key, &api.LLMResponse{Text: "pong", Usage: api.LLMUsage{TotalTokens: 3}}, time.Millisecond, 0)
	_, _, hit2 := b.PreLLMCallHook(req)
	if !hit2 {
		t.Fatal("expected cache hit after PostLLMCallHook")
	}
	b.ClearCache()
	_, _, hit3 := b.PreLLMCallHook(req)
	if hit3 {
		t.Fatal("expected miss after ClearCache")
	}
}
