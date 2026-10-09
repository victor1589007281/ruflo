package engine

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// P0「去装饰」的验收测试。
//
// 病灶: hookOut.AdditionalContext 在本次改动前全仓只有一个消费方 (PreToolUse),
// 其余 20+ 处只读 Decision/Reason ⇒ 挂在 PreTurn/PreRequest/PostCompact 上的
// prompt 型 hook **跑了、返回了、文本静默消失**。
//
// 这组测试守三件事:
//  1. 正: 内容确实进了消息序列, 且**是会被投递的 user 消息** (不是 IsMeta);
//  2. 负: 同一内容不重复注入 (否则每轮都在改 prompt 前缀, 前缀缓存整段失效);
//  3. 负: 空内容不注入 (注入一条空 user 消息会白白改变前缀, 是纯亏损)。

func userText(t *testing.T, m types.Message) string {
	t.Helper()
	for _, b := range m.Content {
		if b.Type == types.ContentBlockText {
			return b.Text
		}
	}
	return ""
}

// TestInjectHookContext_InjectsAsDeliverableUserMessage 是**正**判据。
//
// 关键断言是 IsMeta == false: 若注入成 meta 消息, 组装 API 请求时会被过滤掉,
// 于是"接线"变成了新的装饰 —— 这正是这条链要防的形态。
func TestInjectHookContext_InjectsAsDeliverableUserMessage(t *testing.T) {
	e := &QueryEngine{}
	msgs := []types.Message{{Type: types.MessageTypeUser, Content: []types.ContentBlock{
		{Type: types.ContentBlockText, Text: "原始问题"},
	}}}

	out := e.injectHookContext(msgs, &types.HookOutput{AdditionalContext: "  项目约定: 用 tabs  "}, "PreTurn")

	if len(out) != 2 {
		t.Fatalf("应追加一条消息, 实际 len=%d", len(out))
	}
	last := out[len(out)-1]
	if last.IsMeta {
		t.Fatal("注入的消息是 IsMeta —— 它会被组装 API 时过滤掉, 等于没接线")
	}
	if last.Type != types.MessageTypeUser {
		t.Fatalf("注入的消息类型应为 user, 实际 %v", last.Type)
	}
	if got := userText(t, last); got != "项目约定: 用 tabs" {
		t.Fatalf("注入内容应已 TrimSpace, 实际 %q", got)
	}
}

// TestInjectHookContext_DoesNotDuplicate 是**负**判据 (前缀缓存):
// 同一个 hook 每轮返回同样的内容时, 不能每轮都追加一条。
func TestInjectHookContext_DoesNotDuplicate(t *testing.T) {
	e := &QueryEngine{}
	msgs := []types.Message{{Type: types.MessageTypeUser, Content: []types.ContentBlock{
		{Type: types.ContentBlockText, Text: "原始问题"},
	}}}
	hookOut := &types.HookOutput{AdditionalContext: "同一条约束"}

	once := e.injectHookContext(msgs, hookOut, "PreTurn")
	twice := e.injectHookContext(once, hookOut, "PreTurn")

	if len(twice) != len(once) {
		t.Fatalf("相同内容被重复注入: %d → %d (会让 prompt 前缀每轮都变, 缓存整段失效)", len(once), len(twice))
	}
}

// TestInjectHookContext_EmptyIsNoOp 是**负**判据:
// 空内容/空白内容不得注入 —— 一条空 user 消息白白改变前缀, 收益为零。
func TestInjectHookContext_EmptyIsNoOp(t *testing.T) {
	e := &QueryEngine{}
	base := []types.Message{{Type: types.MessageTypeUser, Content: []types.ContentBlock{
		{Type: types.ContentBlockText, Text: "原始问题"},
	}}}

	for _, c := range []string{"", "   ", "\n\t "} {
		out := e.injectHookContext(base, &types.HookOutput{AdditionalContext: c}, "PreTurn")
		if len(out) != len(base) {
			t.Fatalf("空内容 %q 不该注入, 实际 len=%d", c, len(out))
		}
	}
	// nil HookOutput 同样不注入 (hook 没返回时调用方不必判空)
	if out := e.injectHookContext(base, nil, "PreTurn"); len(out) != len(base) {
		t.Fatalf("nil HookOutput 不该注入, 实际 len=%d", len(out))
	}
}

// TestInjectHookContext_MetaDoesNotMaskRealMessage 是**负**判据 (去重的边界):
// meta 消息不参与去重。否则一条 IsMeta 的同文消息会"代表"已经注入过, 而它自己
// 根本不会被投递 —— 结果是该约束一次都没到达模型, 却显示为"已注入"。
func TestInjectHookContext_MetaDoesNotMaskRealMessage(t *testing.T) {
	e := &QueryEngine{}
	msgs := []types.Message{
		{Type: types.MessageTypeUser, IsMeta: true, Content: []types.ContentBlock{
			{Type: types.ContentBlockText, Text: "要注入的约束"},
		}},
	}

	out := e.injectHookContext(msgs, &types.HookOutput{AdditionalContext: "要注入的约束"}, "PreTurn")

	if len(out) != 2 {
		t.Fatal("序列里只有一条 meta 同文消息时, 仍应注入 —— meta 不投递, 不能代表已注入")
	}
}
