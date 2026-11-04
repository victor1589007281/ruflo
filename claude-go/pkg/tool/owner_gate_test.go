// owner_gate_test.go — RunToolUse 的 owner 一致性闸（Moqi R-H6）。
//
// 这组测试钉住的是"**拒绝**"这个行为，而不是"字段被传下来了"。两者的区别是
// 全部意义所在：owner 被正确携带但没人校验时，越权访问与正常访问的执行结果
// 逐字节相同，任何只看"工具跑成功了"的测试都会给绿灯。
package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// countingTool 记录自己被调用了几次，用来区分"拒绝了"与"跑了但结果看着像拒绝"。
type countingTool struct {
	calls int
}

func (t *countingTool) Name() string                           { return "echo_owner" }
func (t *countingTool) Description() string                    { return "owner gate probe" }
func (t *countingTool) InputSchema() json.RawMessage           { return json.RawMessage(`{"type":"object"}`) }
func (t *countingTool) IsReadOnly(json.RawMessage) bool        { return true }
func (t *countingTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (t *countingTool) CheckPermissions(json.RawMessage, *ToolContext) *types.PermissionResult {
	return nil
}
func (t *countingTool) Call(context.Context, json.RawMessage, *ToolContext) (*ToolResult, error) {
	t.calls++
	return &ToolResult{Content: "executed"}, nil
}

func ownerGateFixture(t *testing.T) (*countingTool, *Registry) {
	t.Helper()
	tl := &countingTool{}
	reg := NewRegistry()
	reg.Register(tl)
	return tl, reg
}

func runOwnerGate(ctx context.Context, reg *Registry, tctx *ToolContext) types.Message {
	return RunToolUse(ctx, types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: "echo_owner",
		Input: json.RawMessage(`{}`),
	}, reg, tctx, nil)
}

func firstToolResult(t *testing.T, msg types.Message) types.ContentBlock {
	t.Helper()
	if len(msg.Content) != 1 || msg.Content[0].Type != types.ContentBlockToolResult {
		t.Fatalf("期望单条 tool_result, 实得 %+v", msg.Content)
	}
	return msg.Content[0]
}

// TestOwnerGate_跨owner被拒 是核心用例：ctx 里是 alice，工具上下文里是 bob。
//
// 断言分两半，缺一半就会漏掉一类缺陷：
//   - 工具**没有**被执行（calls==0），否则闸门只是"打印了一行拒绝"；
//   - 返回的是 IsError 的 tool_result，否则协议上模型会以为工具跑成功了。
func TestOwnerGate_跨owner被拒(t *testing.T) {
	tl, reg := ownerGateFixture(t)
	ctx := types.WithOwner(context.Background(), "alice")
	msg := runOwnerGate(ctx, reg, &ToolContext{OwnerID: "bob"})

	if tl.calls != 0 {
		t.Fatalf("跨 owner 调用被执行了 %d 次 —— 闸门没有真正拦住", tl.calls)
	}
	cb := firstToolResult(t, msg)
	if !cb.IsError {
		t.Fatalf("跨 owner 拒绝必须带 IsError, 实得 %+v", cb)
	}
	if cb.ToolUseID != "tu-1" {
		t.Fatalf("拒绝结果必须回填 ToolUseID (%q), 否则协议对不上", cb.ToolUseID)
	}
}

// TestOwnerGate_同owner放行 是负对照。
//
// 没有它，"永远拒绝"的实现会让上面那条测试照样通过 —— 那时所有工具都不工作，
// 而测试全绿。
func TestOwnerGate_同owner放行(t *testing.T) {
	tl, reg := ownerGateFixture(t)
	ctx := types.WithOwner(context.Background(), "alice")
	msg := runOwnerGate(ctx, reg, &ToolContext{OwnerID: "alice"})

	if tl.calls != 1 {
		t.Fatalf("同 owner 调用应执行一次, 实得 %d", tl.calls)
	}
	cb := firstToolResult(t, msg)
	if cb.IsError || cb.Content != "executed" {
		t.Fatalf("同 owner 调用结果异常: %+v", cb)
	}
}

// TestOwnerGate_空owner归一后不误伤 守住"单用户旧行为逐字节不变"。
//
// ctx 没写 owner、tctx 也没写 —— 两者都归一成 "local"，必须放行。这条测试的
// 失效形态很隐蔽：如果实现里直接比较原始字符串而不是归一化后的值，那么
// `"" == ""` 仍然成立、测试也过，但 `" "` 与 `""` 就会被误判成越权。
func TestOwnerGate_空owner归一后不误伤(t *testing.T) {
	tl, reg := ownerGateFixture(t)
	msg := runOwnerGate(context.Background(), reg, &ToolContext{})

	if tl.calls != 1 {
		t.Fatalf("两侧都未指定 owner 时应放行 (单用户旧行为), 实得 %d 次调用", tl.calls)
	}
	if cb := firstToolResult(t, msg); cb.IsError {
		t.Fatalf("空 owner 被误判成越权: %+v", cb)
	}
}

// TestOwnerGate_空白owner等同默认 覆盖归一化的边界。
//
// 纯空白不是"另一个 owner"，它是"没填"。若归一化漏了 TrimSpace，`" "` 会与
// `"local"` 不等而被拒 —— 表现为"某个调用方把表单空值透传下来，工具就全挂了"。
func TestOwnerGate_空白owner等同默认(t *testing.T) {
	tl, reg := ownerGateFixture(t)
	ctx := types.WithOwner(context.Background(), "   ")
	msg := runOwnerGate(ctx, reg, &ToolContext{OwnerID: "local"})

	if tl.calls != 1 {
		t.Fatalf(`"   " 应归一到 "local" 而不是另一个 owner, 实得 %d 次调用`, tl.calls)
	}
	if cb := firstToolResult(t, msg); cb.IsError {
		t.Fatalf("空白 owner 被误判成越权: %+v", cb)
	}
}

// TestOwnerGate_ctx有owner但tctx为空被拒 是"忘记透传"的方向。
//
// 这一侧比反向更危险：tctx.OwnerID 为空时，工具要么退回"无主"去查全库，要么
// 归一到 local 读到单用户的旧数据。两种都不是本请求想要的，所以必须拒绝 ——
// 拒绝让装配错误当场暴露，而不是让 A 的数据静默出现在 B 的会话里。
func TestOwnerGate_ctx有owner但tctx为空被拒(t *testing.T) {
	tl, reg := ownerGateFixture(t)
	ctx := types.WithOwner(context.Background(), "alice")
	msg := runOwnerGate(ctx, reg, &ToolContext{})

	if tl.calls != 0 {
		t.Fatalf("tctx 漏填 owner 时必须拒绝执行, 实得 %d 次调用", tl.calls)
	}
	if cb := firstToolResult(t, msg); !cb.IsError {
		t.Fatalf("tctx 漏填 owner 应返回错误结果: %+v", cb)
	}
}

// TestOwnerGate_nilToolContext不panic 守住独立调用路径。
//
// 工具被单独调用（不经引擎装配）时 tctx 可能是 nil。此时没有"两个来源"可比，
// 不做判断，行为与改造前一致。
func TestOwnerGate_nilToolContext不panic(t *testing.T) {
	tl, reg := ownerGateFixture(t)
	ctx := types.WithOwner(context.Background(), "alice")
	msg := runOwnerGate(ctx, reg, nil)

	if tl.calls != 1 {
		t.Fatalf("tctx 为 nil 时不应阻拦, 实得 %d 次调用", tl.calls)
	}
	if cb := firstToolResult(t, msg); cb.IsError {
		t.Fatalf("tctx 为 nil 时不应报 owner 错误: %+v", cb)
	}
}
