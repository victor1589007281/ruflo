// evidence_trust_test.go — H-F1/H-F2 在**执行点**的验收：
// ToolResult.Trust/SourceURI 必须落到 tool_result 块的进程内侧信道，
// 且按入参的确认声明必须被 orchestration 的确认闸读取。
package tool

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// trustTool 模拟 WebFetch/WebSearch：成功结果带 trust/source。
type trustTool struct {
	isErr bool
}

func (t *trustTool) Name() string                           { return "trust_tool" }
func (t *trustTool) Description() string                    { return "evidence trust probe" }
func (t *trustTool) InputSchema() json.RawMessage           { return json.RawMessage(`{"type":"object"}`) }
func (t *trustTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (t *trustTool) IsReadOnly(json.RawMessage) bool        { return true }
func (t *trustTool) CheckPermissions(json.RawMessage, *ToolContext) *types.PermissionResult {
	return &types.PermissionResult{Behavior: types.PermissionAllow}
}
func (t *trustTool) Call(context.Context, json.RawMessage, *ToolContext) (*ToolResult, error) {
	return &ToolResult{
		Content:   "external page body",
		IsError:   t.isErr,
		Trust:     "untrusted",
		SourceURI: "https://example.com/page",
	}, nil
}

func evidenceToolResult(t *testing.T, m types.Message) types.ContentBlock {
	t.Helper()
	for _, b := range m.Content {
		if b.Type == types.ContentBlockToolResult {
			return b
		}
	}
	t.Fatalf("消息里没有 tool_result 块: %+v", m)
	return types.ContentBlock{}
}

// TestRunToolUse_TrustAndSourceReachContentBlock 是 H-F2 的传输断言。
func TestRunToolUse_TrustAndSourceReachContentBlock(t *testing.T) {
	tl := &trustTool{}
	reg := NewRegistry()
	reg.Register(tl)
	msg := RunToolUse(context.Background(), types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: tl.Name(), Input: json.RawMessage(`{}`),
	}, reg, nil, nil)

	block := evidenceToolResult(t, msg)
	if block.Trust != "untrusted" || block.SourceURI != "https://example.com/page" {
		t.Fatalf("trust/source 未落到 tool_result 块: %+v", block)
	}

	// 侧信道绝不能进 API 请求体。
	b, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "untrusted") || strings.Contains(s, "example.com") {
		t.Fatalf("trust/source 泄漏进了 ContentBlock JSON: %s", s)
	}
	if strings.Contains(s, `"trust"`) || strings.Contains(s, `"sourceUri"`) || strings.Contains(s, `"sourceURI"`) {
		t.Fatalf("ContentBlock JSON 出现了侧信道键: %s", s)
	}
}

// TestRunToolUse_ErrorResultHasNoTrust error 结果默认不带信任标签。
func TestRunToolUse_ErrorResultHasNoTrust(t *testing.T) {
	tl := &trustTool{isErr: true}
	reg := NewRegistry()
	reg.Register(tl)
	msg := RunToolUse(context.Background(), types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: tl.Name(), Input: json.RawMessage(`{}`),
	}, reg, nil, nil)

	block := evidenceToolResult(t, msg)
	if !block.IsError {
		t.Fatal("夹具应产生 error 结果")
	}
	if block.Trust != "" || block.SourceURI != "" {
		t.Fatalf("error 结果不应携带 trust/source: %+v", block)
	}
}

// inputConfirmTool 只在入参含 "unknown" 时要求确认（模拟 WebFetch 未知域）。
type inputConfirmTool struct {
	calls atomic.Int64
}

func (t *inputConfirmTool) Name() string                           { return "input_confirm_tool" }
func (t *inputConfirmTool) Description() string                    { return "input-aware confirmation probe" }
func (t *inputConfirmTool) InputSchema() json.RawMessage           { return json.RawMessage(`{"type":"object"}`) }
func (t *inputConfirmTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (t *inputConfirmTool) IsReadOnly(json.RawMessage) bool        { return true }
func (t *inputConfirmTool) CheckPermissions(json.RawMessage, *ToolContext) *types.PermissionResult {
	return &types.PermissionResult{Behavior: types.PermissionAllow}
}
func (t *inputConfirmTool) NeedsConfirmationFor(input json.RawMessage) bool {
	return strings.Contains(string(input), "unknown")
}
func (t *inputConfirmTool) Call(context.Context, json.RawMessage, *ToolContext) (*ToolResult, error) {
	t.calls.Add(1)
	return &ToolResult{Content: "executed"}, nil
}

// TestRunToolUse_InputAwareConfirmationFailClosed 未知域未确认时必须拒绝执行，
// 确认后必须真的执行 —— 两个方向都断言，否则"恒拒绝"或"恒放行"都能骗过单边用例。
func TestRunToolUse_InputAwareConfirmationFailClosed(t *testing.T) {
	tl := &inputConfirmTool{}
	reg := NewRegistry()
	reg.Register(tl)
	block := types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: tl.Name(),
		Input: json.RawMessage(`{"url":"https://unknown.example/x"}`),
	}

	msg := RunToolUse(context.Background(), block, reg, nil, nil)
	got := evidenceToolResult(t, msg)
	if !got.IsError || !strings.Contains(got.Content, "需要确认") {
		t.Fatalf("未知域在无确认时必须被拒绝: %+v", got)
	}
	if tl.calls.Load() != 0 {
		t.Fatalf("被拒绝的调用不应执行工具, calls=%d", tl.calls.Load())
	}

	msg = RunToolUse(context.Background(), block, reg, &ToolContext{Confirmed: &fixedConfirm{ok: true}}, nil)
	got = evidenceToolResult(t, msg)
	if got.IsError || got.Content != "executed" {
		t.Fatalf("确认后应执行: %+v", got)
	}
	if tl.calls.Load() != 1 {
		t.Fatalf("确认后应恰好执行一次, calls=%d", tl.calls.Load())
	}
}

// TestNeedsConfirmationFor_UnionOfDeclarations 按入参声明与工具级声明取并集；
// 都没声明时为 false（执行闸默认不设闸，与改造前一致）。
func TestNeedsConfirmationFor_UnionOfDeclarations(t *testing.T) {
	if NeedsConfirmationFor(&inputConfirmTool{}, json.RawMessage(`{"url":"https://known.example"}`)) {
		t.Error("已知域不应要求确认")
	}
	if !NeedsConfirmationFor(&inputConfirmTool{}, json.RawMessage(`{"url":"https://unknown.example"}`)) {
		t.Error("未知域应要求确认")
	}
	if NeedsConfirmationFor(&trustTool{}, json.RawMessage(`{}`)) {
		t.Error("未声明确认的工具不应要求确认")
	}
	if NeedsConfirmationFor(nil, nil) {
		t.Error("nil 工具不应要求确认")
	}
}
