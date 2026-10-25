// runner_finalize_test.go —— FinalizeTurns 的验收。
//
// 这条能力的失败形态是"看起来跑完了": 模型在最后一轮仍然回 tool_use, 引擎执行完
// 工具后循环自然结束, 返回一段中间文本 + max_turns 错误, 或者更糟 —— 返回上一轮
// 的计划文字而没有任何错误。两种情况下上游都拿到一份不是报告的东西。
//
// 因此判据落在**实际发出去的请求**上, 不落在"代码里有一个 if":
//   - 剩余 2 轮那次请求里必须有收口提醒;
//   - 最后一轮请求的 tools 必须为空 (以请求体字节为准);
//   - 最后一轮模型仍然回 tool_use 时, 该工具**不得被执行** (计数器).
package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// finalizeCountTool 与 noopTool 同名, 用来数"工具真的被执行了几次"。
type finalizeCountTool struct{ calls *atomic.Int32 }

func (finalizeCountTool) Name() string { return "noop_tool" }
func (finalizeCountTool) Description() string {
	return "Counting no-op tool for finalization test."
}
func (finalizeCountTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (finalizeCountTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (finalizeCountTool) IsReadOnly(_ json.RawMessage) bool        { return true }
func (finalizeCountTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (c finalizeCountTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	c.calls.Add(1)
	return &tool.ToolResult{Content: "noop ok"}, nil
}

func TestFinalizeTurnsStopsToolsAndForcesText(t *testing.T) {
	id, cfg := stateIdentity()
	// -1 = 永远回 tool_use: 没有执行层收口时, 这个 run 只会以 max_turns 结束。
	u := newStateUpstream(t, -1, "unused")
	e := newStateEngine(t, u, cfg, t.TempDir())

	var calls atomic.Int32
	e.Tools.Register(finalizeCountTool{calls: &calls})

	got, err := e.RunIsolated(context.Background(), "写一份报告", IsolatedRunOptions{
		MaxTurns:      4,
		FinalizeTurns: 2,
		Stage:         id.Stage,
	})
	if err != nil {
		t.Fatalf("最后一轮应当收口成功, 却返回错误: %v (text=%q)", err, got)
	}
	if got != "第3轮思考" {
		t.Fatalf("最终文本应来自最后一轮, got %q", got)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("工具应只在前 3 轮执行, 实际执行 %d 次 —— 最后一轮的 tool_use 被漏放了", n)
	}

	reqs := u.requests()
	if len(reqs) != 4 {
		t.Fatalf("应发出 4 次请求, 实际 %d 次", len(reqs))
	}
	// turn 2 = 剩余 2 轮 (MaxTurns=4); turn 3 = 最后一轮。
	if !strings.Contains(reqs[2].Flat, "收口提醒") {
		t.Fatalf("剩余 2 轮的请求里没有收口提醒: %q", reqs[2].Flat)
	}
	if !strings.Contains(reqs[3].Flat, "最后一轮") {
		t.Fatalf("最后一轮的请求里没有最后通牒: %q", reqs[3].Flat)
	}
	if len(reqs[3].ToolNames) != 0 {
		t.Fatalf("最后一轮必须不暴露任何工具, 实际发出 %v", reqs[3].ToolNames)
	}
}

// TestFinalizeTurnsClampsToMaxTurns 钉住 FinalizeTurns > MaxTurns 的钳位。
//
// 不钳位时 "剩余 == FinalizeTurns" 一次都命不中, 收口提醒静默失效 ——
// 配置看起来生效了, 行为与不配完全一样。
func TestFinalizeTurnsClampsToMaxTurns(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, -1, "unused")
	e := newStateEngine(t, u, cfg, t.TempDir())

	got, err := e.RunIsolated(context.Background(), "写一份报告", IsolatedRunOptions{
		MaxTurns:      2,
		FinalizeTurns: 5,
		Stage:         id.Stage,
	})
	if err != nil {
		t.Fatalf("钳位后最后一轮应收口成功: %v (text=%q)", err, got)
	}
	reqs := u.requests()
	if len(reqs) != 2 {
		t.Fatalf("应发出 2 次请求, 实际 %d 次", len(reqs))
	}
	// 钳位到 2 后, 第一轮就是"剩余 2 轮", 必须收到收口提醒。
	if !strings.Contains(reqs[0].Flat, "收口提醒") {
		t.Fatalf("第一轮缺少收口提醒: %q", reqs[0].Flat)
	}
	if len(reqs[1].ToolNames) != 0 {
		t.Fatalf("最后一轮必须不暴露工具, 实际 %v", reqs[1].ToolNames)
	}
}
