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

func TestFinalizeInstructionDeliversJSONInsteadOfHandoff(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, -1, "unused")
	e := newStateEngine(t, u, cfg, t.TempDir())
	const finalJSON = `{"subject":"feat: 保留结构化收尾","files":[],"warnings":["未读完的 diff 已标注"]}`

	// 假上游默认永远回 tool_use。只有真正撤掉工具的那一次业务请求才改回
	// JSON；如果实现仍先跑满 max_turns，接下来的 handoff 即使同样无工具，
	// 也只能返回交接说明，测试会随 RunIsolated 的错误一起失败。
	u.onRequest = func(turn int, req seenRequest) {
		if len(req.ToolNames) == 0 {
			u.toolTurns = turn
			u.text = finalJSON
		}
	}
	got, err := e.RunIsolated(context.Background(), "总结提交", IsolatedRunOptions{
		MaxTurns:            3,
		FinalizeTurns:       1,
		FinalizeInstruction: "只输出 STRICT_JSON_CONTRACT 约定的 JSON，不要 Markdown。",
		Stage:               id.Stage,
	})
	if err != nil {
		t.Fatalf("结构化收尾不应进入 max_turns/handoff: %v (text=%q)", err, got)
	}
	if got != finalJSON || !json.Valid([]byte(got)) {
		t.Fatalf("最终交付不是预期 JSON: %q", got)
	}
	reqs := u.requests()
	if len(reqs) != 3 {
		t.Fatalf("应在 3 次业务请求内完成，不应再有 handoff，实际 %d 次", len(reqs))
	}
	last := reqs[len(reqs)-1]
	if last.Handoff || len(last.ToolNames) != 0 {
		t.Fatalf("最后一次必须是业务收尾且无工具: %+v", last)
	}
	if !strings.Contains(last.Flat, "STRICT_JSON_CONTRACT") {
		t.Fatalf("调用方输出契约没有进入实际请求: %q", last.Flat)
	}
	if strings.Contains(last.Flat, "一级标题") {
		t.Fatalf("默认 Markdown 报告文案污染了 JSON 收尾: %q", last.Flat)
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
