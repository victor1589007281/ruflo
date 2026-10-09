// trace_usage_flag_test.go — H0-1 的验收测试: 把"没报用量"与"报了 0"分开。
//
// 起因是一次**被读错的读数**: 210 个 trace 里 21226 条 llm_call 只有 6524 条带
// token 字段, 于是得出"仅 30% 带 usage, 采集侧漏了 70%"。按 status 分组重算后
// 才发现: 14697 条 error 全无 usage (调用失败, 流里根本没有 message_start),
// 6529 条 success 里 6524 条都有 (99.9%)。**没有任何采集缺口** —— 是判据把
// "失败"混进了分母。
//
// 本文件钉住修正后的语义: usage 的有无必须可区分, 且不依赖字段存在性猜测。
package engine

import (
	"context"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/types"
)

// writeAndRead 写一条 Span 后从轨迹底座读回 (用的是本包既有的 newTraceEngine)。
func writeAndRead(t *testing.T, in llmCallSpan) tracestore.Span {
	t.Helper()
	e, ts := newTraceEngine(t)
	e.writeLLMCallSpan(context.Background(), in)
	spans, err := ts.ReadRun("")
	if err != nil {
		t.Fatalf("读回轨迹失败: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("期望 1 条 Span, got %d", len(spans))
	}
	return spans[0]
}

// 判据 1: 成功的调用必须显式标记 usage_reported=true。
func TestH01_SuccessfulCallMarksUsageReported(t *testing.T) {
	sp := writeAndRead(t, llmCallSpan{
		Start: time.Now(), Model: "m", Usage: &types.Usage{
			InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 5,
		},
	})

	if !UsageReported(sp.Attrs) {
		t.Fatal("带 Usage 的调用未被标记为已上报用量")
	}
	if got := sp.Attrs["input_tokens"]; got != float64(100) {
		t.Fatalf("input_tokens = %v, 期望 100", got)
	}
}

// 判据 2 (本次修复的核心): 失败的调用必须显式标记 usage_reported=false,
// 而不是"字段缺失"。
//
// 改动前两者在 trace 里完全同形 —— 这正是"30%"这个误导数字的来源。
func TestH01_FailedCallMarksUsageNotReported(t *testing.T) {
	sp := writeAndRead(t, llmCallSpan{
		Start: time.Now(), Model: "m", Err: context.DeadlineExceeded, // Usage 为 nil
	})

	if UsageReported(sp.Attrs) {
		t.Fatal("失败的调用被标记成已上报用量 —— 会把零消耗误算进成本")
	}
	// 关键: 标记必须**存在**且为 false, 不是缺席。
	if _, present := sp.Attrs["usage_reported"]; !present {
		t.Fatal("usage_reported 字段缺席 —— '没报'与'报了 0'仍然不可区分")
	}
}

// 判据 3: 上游报了 0 与压根没报, 必须可区分。
//
// 这是 cache_creation_tokens 恒为 0 那条读数的通用形式: 该字段在
// OpenAI 兼容 / Responses 两条解析路径根本不构造 (见 pkg/api/openai.go:359,
// pkg/api/responses.go:296), 所以"恒 0"是**语义缺失**而非上游确实报了 0。
func TestH01_ZeroIsDistinctFromAbsent(t *testing.T) {
	// (a) 上游报了一份全 0 的用量 —— 属于"报了 0"。
	zero := writeAndRead(t, llmCallSpan{Start: time.Now(), Model: "m", Usage: &types.Usage{}})
	// (b) 压根没有用量 —— 属于"没报"。
	absent := writeAndRead(t, llmCallSpan{Start: time.Now(), Model: "m"})

	if !UsageReported(zero.Attrs) {
		t.Fatal("上报了全 0 用量的调用, 应判定为'已上报'")
	}
	if UsageReported(absent.Attrs) {
		t.Fatal("无用量对象的调用, 应判定为'未上报'")
	}
}

// 判据 4: 向后兼容 —— 旧 Span (没有 usage_reported 字段) 退化为看 input_tokens。
//
// 磁盘上已有的 21226 条历史 Span 都走这条路径, 不能因为新字段上线就全部变"未上报"。
func TestH01_LegacySpanFallback(t *testing.T) {
	legacyWith := map[string]any{"input_tokens": 10, "status": "success"}
	legacyWithout := map[string]any{"status": "error"}

	if !UsageReported(legacyWith) {
		t.Fatal("旧 Span 有 input_tokens 时应判定为已上报")
	}
	if UsageReported(legacyWithout) {
		t.Fatal("旧 Span 无 input_tokens 时应判定为未上报")
	}

	// 显式 false 必须压过"有 input_tokens"的退化判断。
	explicitFalse := map[string]any{"usage_reported": false, "input_tokens": 10}
	if UsageReported(explicitFalse) {
		t.Fatal("显式 usage_reported=false 被 input_tokens 的退化判断覆盖了")
	}
}
