// stream_tool_evidence_test.go — H-F1/H-F2 的**接线**验收：
// 工具结果里的 trust/source 必须从 ToolResult → tool_result 块 → StreamEvent
// 一路到达 harness，且全文通过 ToolResultFull 单独承载（摘要保持 200B）。
//
// 必须走真实引擎 + 假上游，而不是直接构造 StreamEvent：否则"引擎忘了读
// cb.Trust / 忘了填 ToolResultFull"这类缺陷完全测不到。
package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

type evidenceTool struct{}

func (evidenceTool) Name() string        { return "evidence_tool" }
func (evidenceTool) Description() string { return "evidence stream probe" }
func (evidenceTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (evidenceTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (evidenceTool) IsReadOnly(json.RawMessage) bool        { return true }
func (evidenceTool) CheckPermissions(json.RawMessage, *tool.ToolContext) *types.PermissionResult {
	return &types.PermissionResult{Behavior: types.PermissionAllow}
}
func (evidenceTool) Call(context.Context, json.RawMessage, *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{
		Content:   evidenceFull,
		Trust:     "untrusted",
		SourceURI: "https://example.com/evidence",
	}, nil
}

const evidenceFull = "EVIDENCE-BODY-" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef"

func TestStreamToolDoneCarriesFullResultTrustAndSource(t *testing.T) {
	if len(evidenceFull) <= 200 {
		t.Fatalf("夹具无效: %dB 不足以区分摘要与全文", len(evidenceFull))
	}
	up := &gateUpstream{firstFrames: toolUseFrames(toolUseFrame{
		ID: "tu-ev", Name: "evidence_tool", Input: `{}`,
	})}
	e := newGateEngine(t, up, evidenceTool{})

	ch := e.SubmitStreamBlocks(context.Background(), "抓取证据", nil)
	var done *types.StreamEvent
	for ev := range ch {
		if ev.Kind == types.StreamEventToolDone && ev.ToolUseID == "tu-ev" {
			copied := ev
			done = &copied
		}
	}
	if done == nil {
		t.Fatal("未收到 ToolDone 事件")
	}

	if done.ToolResultFull != evidenceFull {
		t.Fatalf("ToolResultFull 不是完整结果: got %dB want %dB", len(done.ToolResultFull), len(evidenceFull))
	}
	if done.ToolTrust != "untrusted" {
		t.Errorf("ToolTrust 未透传, 实得 %q", done.ToolTrust)
	}
	if done.ToolSource != "https://example.com/evidence" {
		t.Errorf("ToolSource 未透传, 实得 %q", done.ToolSource)
	}

	// 摘要口径不变：前 200B + "..."（既有 SSE 消费方依赖它）。
	wantSummary := evidenceFull[:200] + "..."
	if done.ToolResult != wantSummary {
		t.Errorf("摘要字段应保持 200B+省略号: got %q", done.ToolResult)
	}
	if len(done.ToolResult) >= len(done.ToolResultFull) {
		t.Error("ToolResult 不应被扩大成全文 (SSE 事件体积会失控)")
	}
}
