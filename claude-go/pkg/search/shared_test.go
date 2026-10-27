package search

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newSharedTestConfig(addr string) Config {
	cfg := DefaultConfig()
	cfg.HedgeDelay = 0
	cfg.CacheTTL = time.Minute
	cfg.RedisURL = "redis://" + addr
	cfg.RedisKeyPrefix = "search:test"
	cfg.RedisTimeout = 100 * time.Millisecond
	return cfg
}

func TestSharedSearchAcrossManagersHashedAndCloned(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := newSharedTestConfig(server.Addr())
	const secretQuery = "private launch plan xyzzy"
	result := []Result{{
		URL: "https://docs.example.com/private", Title: "original",
		Snippet: "verbatim evidence", Cost: 0,
	}}
	provider1 := newFake("stub", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return result, nil
	})
	provider2 := newFake("stub", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return result, nil
	})
	manager1 := NewManager(cfg, testEgress(t), []Provider{provider1})
	manager2 := NewManager(cfg, testEgress(t), []Provider{provider2})
	t.Cleanup(func() { _ = manager1.Close(); _ = manager2.Close() })

	req := SearchRequest{
		Query: secretQuery, OriginalQuestion: "sensitive original question",
		Limit: 3, AllowedDomains: []string{"docs.example.com"},
	}
	first, err := manager1.Search(context.Background(), req)
	if err != nil {
		t.Fatalf("first Search: %v", err)
	}
	first.Results[0].Title = "caller mutation"
	first.Providers[0] = "caller mutation"

	second, err := manager2.Search(context.Background(), req)
	if err != nil {
		t.Fatalf("second Search: %v", err)
	}
	if !second.CacheHit || provider1.callCount() != 1 || provider2.callCount() != 0 {
		t.Fatalf("shared hit=%v calls=(%d,%d)", second.CacheHit, provider1.callCount(), provider2.callCount())
	}
	if second.Results[0].Title != "original" || second.Providers[0] != "stub" {
		t.Fatalf("caller mutation reached shared cache: %+v", second)
	}
	if second.CostUSD != 0 {
		t.Fatalf("cache hit billed cost %.6f", second.CostUSD)
	}
	for _, key := range server.Keys() {
		if strings.Contains(key, secretQuery) || strings.Contains(key, "sensitive original") {
			t.Fatalf("plaintext query in Redis key: %q", key)
		}
	}
}

func TestLoadSharedConfig(t *testing.T) {
	t.Setenv("SEARCH_REDIS_URL", "redis://:not-logged@localhost:6379/2")
	t.Setenv("SEARCH_REDIS_KEY_PREFIX", "tenant:search")
	t.Setenv("SEARCH_REDIS_TIMEOUT", "75ms")
	t.Setenv("SEARCH_REDIS_STALE_TTL", "2h")
	t.Setenv("SEARCH_SHARED_BUDGET_LIMIT_USD", "1.25")
	t.Setenv("SEARCH_SHARED_BUDGET_WINDOW", "15m")
	t.Setenv("SEARCH_SHARED_BUDGET_RESERVATION_TTL", "45s")
	t.Setenv("SEARCH_PROVIDER_COST_ESTIMATES_USD", "exa=0.01,custom=0.02")
	cfg := LoadConfig()
	if cfg.RedisKeyPrefix != "tenant:search" || cfg.RedisTimeout != 75*time.Millisecond || cfg.RedisStaleTTL != 2*time.Hour {
		t.Fatalf("unexpected Redis prefix/timeout/stale: %q %v %v", cfg.RedisKeyPrefix, cfg.RedisTimeout, cfg.RedisStaleTTL)
	}
	if cfg.SharedBudgetLimitUSD != 1.25 || cfg.SharedBudgetWindow != 15*time.Minute || cfg.SharedBudgetReservationTTL != 45*time.Second {
		t.Fatalf("unexpected budget limit/window/reservation: %.2f %v %v", cfg.SharedBudgetLimitUSD, cfg.SharedBudgetWindow, cfg.SharedBudgetReservationTTL)
	}
	if cfg.Providers["exa"].CostEstimateUSD != 0.01 || cfg.Providers["custom"].CostEstimateUSD != 0.02 {
		t.Fatalf("provider estimates not loaded: %+v", cfg.Providers)
	}
	if !cfg.Providers["custom"].Enabled {
		t.Fatal("cost-only custom provider must retain default enabled semantics")
	}
}

func TestManagerCloseOwnedRedisIdempotent(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := newSharedTestConfig(server.Addr())
	manager := NewManager(cfg, testEgress(t), nil)
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if manager.shared == nil || manager.shared.client == nil {
		t.Fatal("Redis client was not installed")
	}
	if err := manager.shared.client.Ping(context.Background()).Err(); err == nil {
		t.Fatal("owned Redis client still usable after Close")
	}
}
