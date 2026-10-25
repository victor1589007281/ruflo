// runner_searchbudget_test.go —— 检索预算软提醒的验收。
//
// 失败形态不是"少了一条日志", 而是提醒**每轮都发**: 见过一次之后模型仍可
// 继续搜, 于是每条请求都多一条用户消息、前缀缓存每轮失效, 成本反而更高。
// 判据因此落在"最终一次请求里该文案只出现一次", 而不是"代码里有个 bool"。
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

type webSearchStub struct{ calls *atomic.Int32 }

func (webSearchStub) Name() string        { return "WebSearch" }
func (webSearchStub) Description() string { return "Counts search calls." }
func (webSearchStub) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (webSearchStub) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (webSearchStub) IsReadOnly(_ json.RawMessage) bool        { return true }
func (webSearchStub) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (s webSearchStub) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	if s.calls != nil {
		s.calls.Add(1)
	}
	return &tool.ToolResult{Content: "search ok"}, nil
}

func TestSearchSoftBudgetNoticeOnceAfterThreshold(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, -1, "unused")
	u.toolName = "WebSearch"
	e := newStateEngine(t, u, cfg, t.TempDir())

	var calls atomic.Int32
	e.Tools.Register(webSearchStub{calls: &calls})

	// 上游永远回 tool_use, 这个 run 必然以 max_turns 结束 —— 本用例只关心
	// 第 19 次请求 (索引 18) 起才出现提醒, 且全程只注入一条。
	_, _ = e.RunIsolated(context.Background(), "写一份报告", IsolatedRunOptions{
		MaxTurns: 22,
		Stage:    id.Stage,
	})
	reqs := u.requests()
	// 22 个业务回合 + 1 次交接请求 (max_turns 之后发的), 交接请求不参与
	// "第几轮"的索引判断。
	if len(reqs) != 23 || !reqs[22].Handoff {
		t.Fatalf("应发出 22 次业务请求 + 1 次交接请求, 实际 %d (last handoff=%v)", len(reqs), reqs[len(reqs)-1].Handoff)
	}
	// 第 i 次请求发出前已执行 i 次 WebSearch; 第 19 次 (索引 18) 恰好到 18 次。
	if strings.Contains(reqs[17].Flat, "【检索预算提醒】") {
		t.Fatalf("第 17 次请求过早注入预算提醒: %q", reqs[17].Flat)
	}
	if !strings.Contains(reqs[18].Flat, "【检索预算提醒】") {
		t.Fatalf("第 19 次请求缺少预算提醒: %q", reqs[18].Flat)
	}
	if n := strings.Count(reqs[len(reqs)-1].Flat, "【检索预算提醒】"); n != 1 {
		t.Fatalf("预算提醒被重复注入 %d 次 (应只发一次)", n)
	}
	if calls.Load() != 22 {
		t.Fatalf("WebSearch 应执行 22 次, 实际 %d", calls.Load())
	}
}
