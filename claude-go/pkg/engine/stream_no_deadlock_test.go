// stream_no_deadlock_test.go — 长会话下 SubmitStreamBlocks 不得死锁。
//
// 背景. SubmitStreamBlocks 同时起两个通道: 消息流 msgCh 与事件流 streamCh,
// 而 queryLoop 两个都写。但它的返回值只有 streamCh, 调用方 (cmd/claude-go 的
// REPL 与 run) 也只读 streamCh —— msgCh 没有任何读者。
//
// 后果不是"消息丢了"而是**进程挂死**: msgCh 的 50 格缓冲一满, queryLoop 就永久
// 卡在 `ch <- m` 上, 而 main 永久等在 streamCh 上。两边互等, 谁都不退出。
//
// 实测形态 (2026-09-24, retry fixture 长跑): 进程活着、CPU 0%、无网络连接,
// 协程 dump 里 queryLoop 停在 runtime.chansend。它的**产物其实已经做完了** ——
// 也就是说这不是"跑得慢", 是一个跑完了却永远不返回的进程。
//
// 为什么短会话测不出来: 缓冲是 50, 只有消息数超过 50 才会撞上。所以本测试**必须
// 让 queryLoop 产出 >50 条消息** —— 否则它在修复前后都通过, 是一条假绿的量具。
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// toolLoopUpstream 每轮都要求调用一次工具, 直到被调用 rounds 次之后才收尾。
//
// 这么写是为了**精确控制消息数**: queryLoop 每轮追加一条 assistant 消息 (带
// tool_use) 与一条 tool_result 消息, 于是 rounds 轮 ≈ 2*rounds 条消息。
// 想压过 50 格缓冲, rounds 取 40 就够了 (80 条)。
func toolLoopUpstream(rounds int) http.Handler {
	var calls int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(&calls, 1))
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write(append([]byte("data: "), append(b, '\n')...))
		}
		write(types.StreamDelta{
			Type: "message_start",
			Message: &types.APIResponse{ID: "m", Model: "m",
				Usage: &types.Usage{InputTokens: 10, OutputTokens: 1}},
		})

		if n >= rounds {
			// 收尾轮: 纯文本, 不带工具调用。
			write(types.StreamDelta{Type: "content_block_start", Index: 0,
				ContentBlock: &types.ContentBlock{Type: types.ContentBlockText}})
			write(types.StreamDelta{Type: "content_block_delta", Index: 0,
				Delta: &types.DeltaContent{Type: "text_delta", Text: "收工"}})
			write(types.StreamDelta{Type: "content_block_stop", Index: 0})
		} else {
			// 工作轮: 让模型要求调用 noop_tool。
			write(types.StreamDelta{Type: "content_block_start", Index: 0,
				ContentBlock: &types.ContentBlock{
					Type: types.ContentBlockToolUse, ID: "tu", Name: "noop_tool",
				}})
			write(types.StreamDelta{Type: "content_block_delta", Index: 0,
				Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: `{}`}})
			write(types.StreamDelta{Type: "content_block_stop", Index: 0})
		}
		write(types.StreamDelta{Type: "message_delta",
			Usage: &types.Usage{OutputTokens: 1},
			Delta: &types.DeltaContent{StopReason: "end_turn"}})
	})
}

// TestSubmitStreamBlocksDoesNotDeadlockOnLongSession 长会话必须正常收尾。
//
// 判据不是"跑得快", 而是**必须返回**。没有修复时, 这个测试会永久挂住 ——
// 所以它外面套了超时: 挂住也是一种失败, 不能靠 go test 的默认 panic 超时 (10 分钟)
// 兜底, 那是"测不出来"而不是"测出来失败"。
func TestSubmitStreamBlocksDoesNotDeadlockOnLongSession(t *testing.T) {
	// rounds 取 40 ⇒ 约 80 条消息 ⇒ 必然越过 50 格缓冲。
	// 若把缓冲调大或把 rounds 调小到 50 以下, 这条测试就退化成假绿。
	const rounds = 40
	srv := httptest.NewServer(toolLoopUpstream(rounds))
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(), RetryCount: 0}

	promptMgr := prompt.NewManager(t.TempDir())
	promptMgr.OverridePrompt = "你是执行者"
	e := NewQueryEngine(&Config{Model: "m", MaxTurns: rounds + 5},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeBypass), nil, promptMgr)

	done := make(chan string, 1)
	go func() {
		ch := e.SubmitStreamBlocks(context.Background(), "干活", nil)
		var sb strings.Builder
		for ev := range ch {
			if ev.Kind == types.StreamEventDelta && !ev.IsThinking {
				sb.WriteString(ev.DeltaText)
			}
		}
		done <- sb.String()
	}()

	select {
	case got := <-done:
		// 流正常收尾 ⇒ 消息通道有人排空, queryLoop 没有卡在发送上。
		if !strings.Contains(got, "收工") {
			t.Fatalf("流结束了但没拿到收尾文本: %q", got)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("SubmitStreamBlocks 在长会话下没有返回 —— " +
			"msgCh 没有读者, queryLoop 卡在 `ch <- m` 上, 而调用方等在 streamCh 上。" +
			"这不是慢, 是死锁: 产物已经做完, 进程永远不退出")
	}

	// 附加判据: 消息数确实超过了缓冲, 否则上面的等待根本没压到那条路径。
	e.mu.Lock()
	msgs := len(e.Messages)
	e.mu.Unlock()
	if msgs <= 50 {
		t.Fatalf("只产生了 %d 条消息, 没越过 msgCh 的 50 格缓冲 —— "+
			"这条测试没能压到死锁路径, 它在修复前后都会通过 (假绿)", msgs)
	}
}
