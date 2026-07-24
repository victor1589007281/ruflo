package internal_hook

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

func TestTraceCaptureTurnSpan(t *testing.T) {
	store := tracestore.New(statestore.NewMemStore())
	h := NewTraceCaptureHook(store)

	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-1", NodeID: "draft", TurnID: "t0"})
	hc := &HookContext{
		Ctx:            ctx,
		Phase:          PhasePostTurn,
		Model:          "kimi:k3",
		StopReason:     "completed",
		TurnUserIntent: "写一篇文章",
		TurnCount:      2,
		AssistantBlocks: []types.ContentBlock{
			{Type: types.ContentBlockText, Text: "好的, 这是文章正文"},
		},
	}
	if _, err := h.Execute(hc); err != nil {
		t.Fatal(err)
	}

	spans, err := store.ReadRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 || spans[0].Kind != "turn" {
		t.Fatalf("应有 1 个 turn span: %+v", spans)
	}
	sp := spans[0]
	if sp.NodeID != "draft" || sp.TurnID != "t0" {
		t.Errorf("trace 字段丢失: %+v", sp)
	}
	if in, _ := store.Resolve(sp.InputRef); in != "写一篇文章" {
		t.Errorf("用户意图未采集: %q", in)
	}
	if out, _ := store.Resolve(sp.OutputRef); out != "好的, 这是文章正文" {
		t.Errorf("assistant 产出未采集: %q", out)
	}
	if sp.Attrs["stop_reason"] != "completed" || sp.Attrs["model"] != "kimi:k3" {
		t.Errorf("attrs 错误: %+v", sp.Attrs)
	}
}

func TestTraceCaptureToolCallSpan(t *testing.T) {
	store := tracestore.New(statestore.NewMemStore())
	h := NewTraceCaptureHook(store)

	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-2", TurnID: "t1"})
	hc := &HookContext{
		Ctx:   ctx,
		Phase: PhasePostToolUse,
		ToolUseBlocks: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, ID: "tu1", Name: "Read", Input: json.RawMessage(`{"path":"/x"}`)},
		},
		ToolResults: []types.Message{
			{Content: []types.ContentBlock{
				{Type: types.ContentBlockToolResult, ToolUseID: "tu1", Content: "文件全部内容", IsError: false},
			}},
		},
	}
	if _, err := h.Execute(hc); err != nil {
		t.Fatal(err)
	}
	spans, _ := store.ReadRun("run-2")
	if len(spans) != 1 || spans[0].Kind != "tool_call" {
		t.Fatalf("应有 1 个 tool_call span: %+v", spans)
	}
	sp := spans[0]
	if sp.Name != "Read" {
		t.Errorf("工具名错误: %q", sp.Name)
	}
	// 关键: tool_result 完整内容被采集 (修复 transcript 缺 tool_result)
	if out, _ := store.Resolve(sp.OutputRef); out != "文件全部内容" {
		t.Errorf("tool_result 正文未采集: %q", out)
	}
	if in, _ := store.Resolve(sp.InputRef); in != `{"path":"/x"}` {
		t.Errorf("工具入参未采集: %q", in)
	}
	if sp.Attrs["has_result"] != true || sp.Attrs["is_error"] != false {
		t.Errorf("attrs 错误: %+v", sp.Attrs)
	}
}

func TestTraceCaptureNilStore(t *testing.T) {
	h := NewTraceCaptureHook(nil)
	// 不 panic, 无副作用
	if _, err := h.Execute(&HookContext{Ctx: context.Background(), Phase: PhasePostTurn}); err != nil {
		t.Fatal(err)
	}
}
