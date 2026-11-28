package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// TestRunUsage_AccumulatesWithoutTraceStore 是 P1 usage 段的立身之本:
// 没开轨迹底座 (TraceStore=nil) 的 run 也要出 usage —— 聚合在守卫之前。
func TestRunUsage_AccumulatesWithoutTraceStore(t *testing.T) {
	e := &QueryEngine{} // TraceStore 刻意 nil
	e.writeLLMCallSpan(context.Background(), llmCallSpan{
		Model: "m", Usage: &types.Usage{InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 5},
	})
	e.writeLLMCallSpan(context.Background(), llmCallSpan{
		Model: "m", Usage: &types.Usage{InputTokens: 200, OutputTokens: 30, CacheCreationInputTokens: 7},
	})
	e.writeLLMCallSpan(context.Background(), llmCallSpan{
		Model: "m", Err: context.DeadlineExceeded, // 失败调用: 计 calls 不计 usage
	})
	u, ok := e.RunUsageSnapshot()
	if !ok {
		t.Fatal("有调用却报 ok=false")
	}
	if u.Calls != 3 || u.UsageReported != 2 {
		t.Fatalf("calls/usage_reported = %d/%d, want 3/2", u.Calls, u.UsageReported)
	}
	if u.InputTokens != 300 || u.OutputTokens != 50 || u.CacheReadInputTokens != 5 || u.CacheCreationInputTokens != 7 {
		t.Fatalf("四项聚合错: %+v", u)
	}
}

// TestRunUsage_EmptyIsNotOK: 一次调用都没有时 ok=false —— 调用方据此整段省略
// usage 字段, 而不是发全零段冒充"没花钱"。
func TestRunUsage_EmptyIsNotOK(t *testing.T) {
	e := &QueryEngine{}
	if _, ok := e.RunUsageSnapshot(); ok {
		t.Fatal("零调用应报 ok=false")
	}
}

// TestRunUsage_ConcurrentSafe: RunIsolated 可被并发 stage 共用引擎, 累加必须持锁。
// -race 下跑 64 路并发, 计数必须精确。
func TestRunUsage_ConcurrentSafe(t *testing.T) {
	e := &QueryEngine{}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.writeLLMCallSpan(context.Background(), llmCallSpan{
				Model: "m", Usage: &types.Usage{InputTokens: 1, OutputTokens: 1},
			})
		}()
	}
	wg.Wait()
	u, _ := e.RunUsageSnapshot()
	if u.Calls != 64 || u.InputTokens != 64 {
		t.Fatalf("并发累加丢账: %+v", u)
	}
}
