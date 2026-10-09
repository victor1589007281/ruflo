// trace_compaction_test.go —— H2-1 的端到端判据。
//
// 方案 §21 H2-1 的验收原文: **「跑一个会压缩的 run, 压缩相关 span 数 > 0, 且能
// 读出压缩前后 token 数两者都非零」**。
//
// pkg/compact/compaction_stats_test.go 钉的是"数算对了"; 这里钉的是"数**真的落到
// 了轨迹上**"。两层必须分开, 因为只测前者时, 把 SetCompactionSink 从
// registerInternalHooks 里删掉不影响任何一条断言 —— 而线上效果是 trajectory 里
// 一条 compaction Span 都没有, 正是这次改造要消灭的那个静默黑洞。
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// compactionSpansOf 从轨迹里滤出 compaction Span。
func compactionSpansOf(t *testing.T, store *tracestore.Store, runID string) []tracestore.Span {
	t.Helper()
	spans, err := store.ReadRun(runID)
	if err != nil {
		t.Fatalf("ReadRun(%s): %v", runID, err)
	}
	var out []tracestore.Span
	for _, sp := range spans {
		if sp.Kind == tracestore.KindCompaction {
			out = append(out, sp)
		}
	}
	return out
}

// attrInt 从 attrs 里取一个整数读数。
//
// attrs 走 JSON 落盘再读回, 所以数值是 float64 —— 直接类型断言成 int 会静默失败
// (得到 0), 而 0 恰好也是本测试要排除的值, 于是断言会以"读数没落盘"的假象通过。
// 这里把两种形态都认掉。
func attrInt(t *testing.T, sp tracestore.Span, key string) int {
	t.Helper()
	v, ok := sp.Attrs[key]
	if !ok {
		t.Fatalf("compaction Span 的 attrs 里没有 %q: %+v", key, sp.Attrs)
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			t.Fatalf("attr %q 不是整数: %v", key, v)
		}
		return int(i)
	default:
		t.Fatalf("attr %q 的类型是 %T, 期望数值", key, v)
		return 0
	}
}

// TestH21_FoldSpanLandsWithBothTokenCounts 核心端到端判据。
//
// 场景: 一轮真实 queryLoop + 一个假上游, 消息历史远超阈值 ⇒ PhasePreCompact 里
// AutoCompactHook 触发真实折叠。断言轨迹里出现 compaction Span, 且**前后 token
// 两者都非零**。
func TestH21_FoldSpanLandsWithBothTokenCounts(t *testing.T) {
	mid := tracestore.New(statestore.NewMemStore())

	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// 摘要请求与正常轮次共用这个假上游: 都回一段纯文本即可。摘要文本给得
		// 够长, 好让 summary_tokens 是个有意义的非零值。
		_ = json.NewEncoder(w).Encode(types.APIResponse{
			ID: "m", Type: "message", Role: "assistant", Model: "m",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: strings.Repeat("摘要内容。", 200)}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer srv.Close()

	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client(), RetryCount: 0}
	// maxContextTokens=3000 ⇒ 阈值 2400; 下面的历史 10×2000/4 = 5000, 稳稳越过。
	comp := compact.NewCompactor(client, 3000)
	e := NewQueryEngine(&Config{MaxTurns: 1, Cwd: t.TempDir()}, client, tool.NewRegistry(), nil, nil, comp, prompt.NewManager(t.TempDir()))
	e.TraceStore = mid
	e.RefreshHooks()

	// 装配断言前置: TraceStore 非 nil 却没接上 sink 的话, 下面会因为"轨迹里没有
	// compaction Span"而报一个说不出所以然的错。这里先把接线本身钉住。
	if !e.HasCompactionSink() {
		t.Fatal("registerInternalHooks 没把 CompactionSink 接到折叠 hook 上 —— " +
			"折叠照常发生, 但轨迹里一条读数都没有")
	}

	// 10 轮 (20 条) × 2000 字符 = 40000 字符 / 4 = 10000 tokens, 阈值 2400 越过。
	e.Messages = bigEngineHistory(10, 2000)

	for range e.SubmitMessage(context.Background(), "") {
	}

	spans := compactionSpansOf(t, mid, "")
	if len(spans) == 0 {
		t.Fatalf("轨迹里没有 compaction Span —— 折叠发生了但没留痕。共写入 %d 条 span",
			countSpans(t, mid))
	}
	sp := spans[0]
	if sp.Kind != tracestore.KindCompaction {
		t.Fatalf("span kind = %q", sp.Kind)
	}
	if sp.Name != "fold" {
		t.Fatalf("compaction span name = %q, 期望 fold (PhasePreCompact 里的整段折叠)", sp.Name)
	}

	before := attrInt(t, sp, "tokens_before")
	after := attrInt(t, sp, "tokens_after")
	// 这就是验收判据本身。
	if before <= 0 {
		t.Fatalf("tokens_before = %d, 判据要求非零", before)
	}
	if after <= 0 {
		t.Fatalf("tokens_after = %d, 判据要求非零 —— 0 与'口径漏算'不可区分", after)
	}
	// 对照臂: 两个数不能相同, 否则上面两条非零断言可以被"两个都填同一个值"骗过。
	if before == after {
		t.Fatalf("tokens_before == tokens_after == %d, 读数没有反映折叠效果", before)
	}
	if got := attrInt(t, sp, "messages_compacted"); got <= 0 {
		t.Fatalf("messages_compacted = %d, 折叠过就该为正", got)
	}
	if got := attrInt(t, sp, "summary_tokens"); got <= 0 {
		t.Fatalf("summary_tokens = %d, 摘要非空就该为正", got)
	}
}

// TestH21_NoCompactionNoSpan 对照臂: 历史很短 (不触发折叠) 时**一条 compaction
// Span 都不该有**。
//
// 少了这一臂, "span 数 > 0"无法把"折叠发生了"与"每次 PreCompact 都无脑发一条"
// 区分开 —— 而后者会让"这次 run 折叠过几次"这个读数恒等于 turn 数。
func TestH21_NoCompactionNoSpan(t *testing.T) {
	mid := tracestore.New(statestore.NewMemStore())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.APIResponse{
			ID: "m", Type: "message", Role: "assistant", Model: "m",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "ok"}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer srv.Close()

	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client(), RetryCount: 0}
	comp := compact.NewCompactor(client, 300000) // 阈值极高
	e := NewQueryEngine(&Config{MaxTurns: 1, Cwd: t.TempDir()}, client, tool.NewRegistry(), nil, nil, comp, prompt.NewManager(t.TempDir()))
	e.TraceStore = mid
	e.RefreshHooks()
	e.Messages = bigEngineHistory(3, 100)

	for range e.SubmitMessage(context.Background(), "") {
	}

	if spans := compactionSpansOf(t, mid, ""); len(spans) != 0 {
		t.Fatalf("没触发折叠却写了 %d 条 compaction Span —— span 数不再等价于折叠次数", len(spans))
	}
	// 对照: 这个 run 确实跑过 (否则"零条 span"是因为什么都没发生)。
	if countSpans(t, mid) == 0 {
		t.Fatal("整个 run 一条 span 都没有, 本对照臂测的是个空场景")
	}
}

// bigEngineHistory 造一段超过折叠阈值的历史。
//
// 最后一条刻意是 assistant: queryLoop 从消息尾部往前找"待回应的最后一条", 尾部是
// user 的话它会被当成新一轮输入重新发一遍, 与本测试要看的折叠读数无关。
func bigEngineHistory(n, chars int) []types.Message {
	msgs := make([]types.Message, 0, n+1)
	for i := 0; i < n; i++ {
		msgs = append(msgs, types.Message{
			Type:    types.MessageTypeUser,
			UUID:    "u" + string(rune('a'+i%26)),
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: strings.Repeat("x", chars)}},
		})
		msgs = append(msgs, types.Message{
			Type:    types.MessageTypeAssistant,
			UUID:    "a" + string(rune('a'+i%26)),
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: strings.Repeat("y", chars)}},
		})
	}
	return msgs
}

func countSpans(t *testing.T, store *tracestore.Store) int {
	t.Helper()
	spans, err := store.ReadRun("")
	if err != nil {
		return 0
	}
	return len(spans)
}
