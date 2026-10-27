package search

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func paidTestConfig(addr string, limit, estimate float64) Config {
	cfg := newSharedTestConfig(addr)
	cfg.MaxTier = TierL3
	cfg.MaxCostUSD = 1 // 既有 L3 入口闸门；共享窗口总额由独立字段控制。
	cfg.SharedBudgetLimitUSD = limit
	cfg.SharedBudgetWindow = time.Hour
	cfg.SharedBudgetReservationTTL = time.Hour
	cfg.Providers["paid"] = ProviderConfig{Enabled: true, Tier: TierL3, CostEstimateUSD: estimate}
	return cfg
}

func paidResult(cost float64) []Result {
	return []Result{{
		URL: "https://paid.example.com/result", Title: "paid",
		Snippet: "paid evidence", Cost: cost,
	}}
}

type unknownCostProvider struct {
	*fakeProvider
}

func (unknownCostProvider) SearchCostUSD([]Result) float64 { return math.NaN() }

func TestSharedBudgetUnknownActualCostKeepsEstimate(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := paidTestConfig(server.Addr(), 0.05, 0.05)
	base := newFake("paid", TierL3, func(context.Context, SearchRequest) ([]Result, error) {
		return paidResult(0.04), nil
	})
	provider := unknownCostProvider{base}
	manager1 := NewManager(cfg, testEgress(t), []Provider{provider})
	manager2 := NewManager(cfg, testEgress(t), []Provider{provider})
	t.Cleanup(func() { _ = manager1.Close(); _ = manager2.Close() })

	first, err := manager1.Search(context.Background(), SearchRequest{Query: "unknown cost first"})
	if err != nil || first.CostUSD != 0.05 {
		t.Fatalf("unknown cost was not conservatively estimated: resp=%+v err=%v", first, err)
	}
	if _, err := manager2.Search(context.Background(), SearchRequest{Query: "unknown cost second"}); err == nil {
		t.Fatal("unknown actual cost was refunded as zero and allowed overspend")
	}
	if base.callCount() != 1 {
		t.Fatalf("provider calls=%d, want only the admitted first call", base.callCount())
	}
}

func TestSharedBudgetConcurrentLimitAcrossManagers(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := paidTestConfig(server.Addr(), 0.10, 0.04)
	provider := newFake("paid", TierL3, func(context.Context, SearchRequest) ([]Result, error) {
		return paidResult(0.04), nil
	})
	manager1 := NewManager(cfg, testEgress(t), []Provider{provider})
	manager2 := NewManager(cfg, testEgress(t), []Provider{provider})
	t.Cleanup(func() { _ = manager1.Close(); _ = manager2.Close() })

	const requests = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	var successes atomic.Int32
	wg.Add(requests)
	for i := 0; i < requests; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			manager := manager1
			if i%2 == 1 {
				manager = manager2
			}
			resp, err := manager.Search(context.Background(), SearchRequest{Query: fmt.Sprintf("unique paid query %d", i)})
			if err == nil {
				if resp.CostUSD != 0.04 {
					t.Errorf("actual response cost=%.6f", resp.CostUSD)
				}
				successes.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if successes.Load() != 2 || provider.callCount() != 2 {
		t.Fatalf("successes=%d provider calls=%d; budget was not atomic", successes.Load(), provider.callCount())
	}
}

func TestSharedBudgetActualSettlementAndCacheHitFree(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := paidTestConfig(server.Addr(), 0.07, 0.05)
	provider1 := newFake("paid", TierL3, func(context.Context, SearchRequest) ([]Result, error) {
		return paidResult(0.02), nil
	})
	provider2 := newFake("paid", TierL3, func(context.Context, SearchRequest) ([]Result, error) {
		return paidResult(0.02), nil
	})
	manager1 := NewManager(cfg, testEgress(t), []Provider{provider1})
	manager2 := NewManager(cfg, testEgress(t), []Provider{provider2})
	t.Cleanup(func() { _ = manager1.Close(); _ = manager2.Close() })

	first, err := manager1.Search(context.Background(), SearchRequest{Query: "settle actual cost"})
	if err != nil || first.CostUSD != 0.02 {
		t.Fatalf("first resp=%+v err=%v", first, err)
	}
	cached, err := manager2.Search(context.Background(), SearchRequest{Query: "settle actual cost"})
	if err != nil || !cached.CacheHit || cached.CostUSD != 0 || provider2.callCount() != 0 {
		t.Fatalf("cached resp=%+v err=%v provider2 calls=%d", cached, err, provider2.callCount())
	}
	// 首次按 0.05 预占、按 0.02 结算后，窗口只剩 0.02 已用；
	// 新查询仍可容纳 0.05 预占。缓存命中若扣费/预占，这里会 fail-closed。
	second, err := manager2.Search(context.Background(), SearchRequest{Query: "new call after refund"})
	if err != nil || provider2.callCount() != 1 {
		t.Fatalf("actual cost was not settled: resp=%+v err=%v calls=%d", second, err, provider2.callCount())
	}
}

func TestSharedBudgetRedisFailurePaidClosedFreeContinues(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := paidTestConfig(server.Addr(), 0.10, 0.05)
	cfg.RedisTimeout = 30 * time.Millisecond
	cfg.Providers["free"] = ProviderConfig{Enabled: true, Tier: TierL0}
	paid := newFake("paid", TierL3, func(context.Context, SearchRequest) ([]Result, error) {
		return paidResult(0.05), nil
	})
	free := newFake("free", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{{URL: "https://free.example.com", Title: "free", Snippet: "free evidence"}}, nil
	})
	manager := NewManager(cfg, testEgress(t), []Provider{paid, free})
	t.Cleanup(func() { _ = manager.Close() })
	server.Close()

	resp, err := manager.Search(context.Background(), SearchRequest{Query: "Redis outage"})
	if err != nil {
		t.Fatalf("free provider should survive Redis outage: %v", err)
	}
	if paid.callCount() != 0 || free.callCount() != 1 || len(resp.Results) != 1 || resp.Results[0].Provider != "free" {
		t.Fatalf("paid=%d free=%d resp=%+v", paid.callCount(), free.callCount(), resp)
	}
	if !contains(resp.Degraded, sharedCacheDegraded) {
		t.Fatalf("Redis cache degradation was silent: %v", resp.Degraded)
	}
	cached, err := manager.Search(context.Background(), SearchRequest{Query: "Redis outage"})
	if err != nil || !cached.CacheHit || free.callCount() != 1 {
		t.Fatalf("local L1 fallback failed: resp=%+v err=%v", cached, err)
	}

	paidOnly := NewManager(cfg, testEgress(t), []Provider{paid})
	t.Cleanup(func() { _ = paidOnly.Close() })
	if _, err := paidOnly.Search(context.Background(), SearchRequest{Query: "paid must fail closed"}); err == nil {
		t.Fatal("paid provider ran without its shared budget")
	}
	if paid.callCount() != 0 {
		t.Fatalf("paid provider called %d times during Redis outage", paid.callCount())
	}
}

func TestSharedBudgetWindowExpiresAndTimeoutDoesNotRefund(t *testing.T) {
	t.Run("window expiry", func(t *testing.T) {
		server := miniredis.RunT(t)
		cfg := paidTestConfig(server.Addr(), 0.05, 0.05)
		cfg.SharedBudgetWindow = 50 * time.Millisecond
		provider := newFake("paid", TierL3, func(context.Context, SearchRequest) ([]Result, error) {
			return paidResult(0.05), nil
		})
		manager := NewManager(cfg, testEgress(t), []Provider{provider})
		t.Cleanup(func() { _ = manager.Close() })
		if _, err := manager.Search(context.Background(), SearchRequest{Query: "window first"}); err != nil {
			t.Fatalf("first: %v", err)
		}
		if _, err := manager.Search(context.Background(), SearchRequest{Query: "window second"}); err == nil {
			t.Fatal("second call exceeded current window")
		}
		time.Sleep(80 * time.Millisecond)
		if _, err := manager.Search(context.Background(), SearchRequest{Query: "window third"}); err != nil {
			t.Fatalf("window did not expire atomically: %v", err)
		}
		if provider.callCount() != 2 {
			t.Fatalf("provider calls=%d", provider.callCount())
		}
	})

	t.Run("unknown timeout keeps reservation", func(t *testing.T) {
		server := miniredis.RunT(t)
		cfg := paidTestConfig(server.Addr(), 0.05, 0.05)
		cfg.SearchTimeout = 40 * time.Millisecond
		provider := newFake("paid", TierL3, func(ctx context.Context, _ SearchRequest) ([]Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		manager := NewManager(cfg, testEgress(t), []Provider{provider})
		t.Cleanup(func() { _ = manager.Close() })
		if _, err := manager.Search(context.Background(), SearchRequest{Query: "unknown first"}); err == nil {
			t.Fatal("timeout unexpectedly succeeded")
		}
		if _, err := manager.Search(context.Background(), SearchRequest{Query: "unknown second"}); err == nil {
			t.Fatal("unknown timeout was refunded and allowed overspend")
		}
		if provider.callCount() != 1 {
			t.Fatalf("timeout refund allowed %d provider calls", provider.callCount())
		}
	})
}
