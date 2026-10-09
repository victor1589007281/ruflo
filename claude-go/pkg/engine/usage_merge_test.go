// usage_merge_test.go — 用量在 message_start / message_delta 两帧间的合并语义。
//
// 这是 P0-1 的**输入可信性**前提 (规划 21.4.2 点名的硬依赖: "如果 token 记账本身是
// 黑洞, 预算闸门就没有可信输入 —— 闸门会按一个低估了 70% 的数字来判")。
//
// 背景: queryLoop 收集流式用量时, message_start 与 message_delta 两帧都写同一个
// `usage` 变量, 而原实现是**整体覆盖**:
//
//	message_start: usage = event.Message.Usage   // {input, cache_read, cache_creation}
//	message_delta: usage = event.Usage           // {output}
//
// 两条协议下后果完全不同:
//
//	openai / openai-responses —— 翻译器把 message_start 的 usage 造成空对象 {},
//	  全部真实用量挂在 message_delta 上, 覆盖无害 (实测 5755 条成功 Span 里
//	  input>0 占 99.9%, 与这条推断一致)。
//	anthropic (Protocol 的零值, 即**默认值**) —— message_start 带 input/cache,
//	  message_delta 只带 output_tokens。覆盖会把输入侧整个丢掉, 累计用量只剩输出。
//
// 一个默认协议上的静默丢数, 且丢的正是最大的一项 (输入通常是输出的 20~50 倍)。
// 因此本文件钉住的判据是: **无论上游把用量拆在哪几帧里, 累计值必须等于两帧之和**。
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// splitUsageUpstream 按 **原生 Anthropic 协议** 发用量: input/cache 走 message_start,
// output 走 message_delta。这正是被覆盖语义丢掉输入的那种上游。
func splitUsageUpstream(in, cacheRead, out int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write(append([]byte("data: "), append(b, '\n')...))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		write(types.StreamDelta{
			Type: "message_start",
			Message: &types.APIResponse{ID: "m1", Model: "m", Usage: &types.Usage{
				InputTokens:          in,
				CacheReadInputTokens: cacheRead,
				OutputTokens:         1, // Anthropic 的 message_start 惯例: 输出从 1 起
			}},
		})
		write(types.StreamDelta{
			Type:  "content_block_start",
			Index: 0,
			ContentBlock: &types.ContentBlock{Type: types.ContentBlockText},
		})
		write(types.StreamDelta{
			Type: "content_block_delta", Index: 0,
			Delta: &types.DeltaContent{Type: "text_delta", Text: "好"},
		})
		write(types.StreamDelta{Type: "content_block_stop", Index: 0})
		write(types.StreamDelta{
			Type:  "message_delta",
			Usage: &types.Usage{OutputTokens: out}, // 只带输出
			Delta: &types.DeltaContent{StopReason: "end_turn"},
		})
	})
}

func runSplitUsage(t *testing.T, in, cacheRead, out int) types.Usage {
	t.Helper()
	srv := httptest.NewServer(splitUsageUpstream(in, cacheRead, out))
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(), RetryCount: 0}
	promptMgr := prompt.NewManager(t.TempDir())
	promptMgr.OverridePrompt = "你是执行者"

	e := NewQueryEngine(&Config{Model: "m", MaxTurns: 3},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, promptMgr)

	ch := e.SubmitMessage(context.Background(), "干活")
	for range ch {
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	return e.CumulativeUsage
}

// 判据 1 (主判据): 累计用量必须等于两帧之和, 而不是"最后那一帧"。
//
// 失败形态是静默的: run 照常成功、终态正常, 只是账少记了输入侧 —— 而输入通常是
// 输出的一到两个数量级, 于是任何按累计值判的闸门都会形同虚设。
func TestUsage_MergesBothFrames(t *testing.T) {
	got := runSplitUsage(t, 10_000, 3_000, 400)

	if got.InputTokens != 10_000 {
		t.Fatalf("InputTokens = %d, 期望 10000 —— message_start 的输入用量被 message_delta 覆盖掉了",
			got.InputTokens)
	}
	if got.CacheReadInputTokens != 3_000 {
		t.Fatalf("CacheReadInputTokens = %d, 期望 3000 —— 缓存读用量同样被覆盖", got.CacheReadInputTokens)
	}
	if got.OutputTokens != 400 {
		t.Fatalf("OutputTokens = %d, 期望 400 —— 输出侧以 message_delta 为准", got.OutputTokens)
	}
}

// 判据 2: 两帧都报全量时, 不该出现重复累加。
//
// 这条守的是修法的反面: 若"合并"被写成"相加", openai 协议下 (翻译器把全量塞在
// message_delta, 且 message_start 是空对象) 不会出问题, 但任何两帧都报量的上游会
// 直接把用量翻倍 —— 那比丢数更糟, 因为闸门会提前误杀正常 run。
func TestUsage_NoDoubleCountWhenBothFramesFull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write(append([]byte("data: "), append(b, '\n')...))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		full := &types.Usage{InputTokens: 500, OutputTokens: 60}
		write(types.StreamDelta{Type: "message_start",
			Message: &types.APIResponse{ID: "m1", Model: "m", Usage: full}})
		write(types.StreamDelta{Type: "content_block_start", Index: 0,
			ContentBlock: &types.ContentBlock{Type: types.ContentBlockText}})
		write(types.StreamDelta{Type: "content_block_stop", Index: 0})
		write(types.StreamDelta{Type: "message_delta", Usage: full,
			Delta: &types.DeltaContent{StopReason: "end_turn"}})
	}))
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client(), RetryCount: 0}
	promptMgr := prompt.NewManager(t.TempDir())
	promptMgr.OverridePrompt = "你是执行者"
	e := NewQueryEngine(&Config{Model: "m", MaxTurns: 3}, client, reg,
		hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeAcceptEdits), nil, promptMgr)

	ch := e.SubmitMessage(context.Background(), "干活")
	for range ch {
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.CumulativeUsage.InputTokens != 500 {
		t.Fatalf("InputTokens = %d, 期望 500 —— 两帧都报全量时被重复累加了",
			e.CumulativeUsage.InputTokens)
	}
	if e.CumulativeUsage.OutputTokens != 60 {
		t.Fatalf("OutputTokens = %d, 期望 60", e.CumulativeUsage.OutputTokens)
	}
}
