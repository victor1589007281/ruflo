// hook_trace.go — TraceStore 轨迹采集内置 Hook (design/03 §4.1 E1)。
//
// 采集两类 Span:
//   - tool_call (PhasePostToolUse): 工具名 + 完整入参 + 完整结果 (修复 transcript
//     缺 tool_result: design/03 §1.3)。入参/结果配对经 tool_use_id。
//   - turn (PhasePostTurn): 用户意图 + assistant 全文 + stop_reason/工具数/model。
//
// trace 四元组从 ctx.Ctx 的 trace.From 读 (engine 在调 LLM 前注入 TurnID,
// teams/workflow 注入 RunID/NodeID)。正文经 tracestore.MakeRef 内联/Blob 分流。
package internal_hook

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

// TraceCaptureHook 把每轮/每工具调用写入 TraceStore。
type TraceCaptureHook struct {
	store *tracestore.Store
}

// NewTraceCaptureHook 创建。store 为 nil 时 hook 不注册 (调用方守卫)。
func NewTraceCaptureHook(store *tracestore.Store) *TraceCaptureHook {
	return &TraceCaptureHook{store: store}
}

func (h *TraceCaptureHook) Name() string  { return "trace_capture" }
func (h *TraceCaptureHook) Priority() int { return 5 } // 观测型, 靠后但先于 turn_metrics 无所谓
func (h *TraceCaptureHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePostToolUse, PhasePostTurn}
}

// Execute 按相位写 Span。
func (h *TraceCaptureHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.store == nil {
		return nil, nil
	}
	ids := trace.From(ctx.Ctx)
	now := time.Now()

	switch ctx.Phase {
	case PhasePostToolUse:
		h.captureToolCalls(ctx, ids, now)
	case PhasePostTurn:
		h.captureTurn(ctx, ids, now)
	}
	return nil, nil
}

// captureToolCalls 为本轮每个工具调用写一个 tool_call Span。
// 入参来自 ToolUseBlocks (tool_use), 结果来自 ToolResults (tool_result), 经 tool_use_id 配对。
func (h *TraceCaptureHook) captureToolCalls(ctx *HookContext, ids trace.IDs, now time.Time) {
	if len(ctx.ToolUseBlocks) == 0 {
		return
	}
	// 建 tool_use_id → 结果文本 的映射
	resultByID := map[string]struct {
		text  string
		isErr bool
	}{}
	for _, m := range ctx.ToolResults {
		for _, b := range m.Content {
			if b.Type == types.ContentBlockToolResult {
				resultByID[b.ToolUseID] = struct {
					text  string
					isErr bool
				}{text: b.Content, isErr: b.IsError}
			}
		}
	}
	for _, tu := range ctx.ToolUseBlocks {
		if tu.Type != types.ContentBlockToolUse {
			continue
		}
		inputJSON := string(tu.Input)
		res, hasRes := resultByID[tu.ID]
		attrs := map[string]any{
			"trace_call": ids.CallID,
			"has_result": hasRes,
		}
		if hasRes {
			attrs["is_error"] = res.isErr
		}
		h.store.Write(tracestore.Span{
			TraceID:   ids.RunID,
			SpanID:    GenerateUUID(),
			ParentID:  ids.TurnID,
			Kind:      tracestore.KindToolCall,
			Name:      tu.Name,
			NodeID:    ids.NodeID,
			TurnID:    ids.TurnID,
			InputRef:  h.store.MakeRef(inputJSON),
			OutputRef: h.store.MakeRef(res.text),
			Attrs:     attrs,
			TS:        now.UnixMilli(),
		})
	}
}

// captureTurn 写本轮 turn Span: 用户意图 → assistant 全文。
func (h *TraceCaptureHook) captureTurn(ctx *HookContext, ids trace.IDs, now time.Time) {
	assistantText := joinTextBlocks(ctx.AssistantBlocks)
	if assistantText == "" && ctx.AssistantMsg != nil {
		assistantText = joinTextBlocks(ctx.AssistantMsg.Content)
	}
	attrs := map[string]any{
		"stop_reason": ctx.StopReason,
		"tool_calls":  len(ctx.TurnToolSigs),
		"model":       ctx.Model,
		"turn_count":  ctx.TurnCount,
	}
	var dur int64
	if !ctx.TurnStart.IsZero() {
		dur = time.Since(ctx.TurnStart).Milliseconds()
	}
	turnName := ids.TurnID
	if turnName == "" {
		turnName = "turn"
	}
	h.store.Write(tracestore.Span{
		TraceID:   ids.RunID,
		SpanID:    GenerateUUID(),
		Kind:      tracestore.KindTurn,
		Name:      turnName,
		NodeID:    ids.NodeID,
		TurnID:    ids.TurnID,
		InputRef:  h.store.MakeRef(ctx.TurnUserIntent),
		OutputRef: h.store.MakeRef(assistantText),
		Attrs:     attrs,
		TS:        now.UnixMilli(),
		DurMS:     dur,
	})
}

// JoinAssistantText 导出 joinTextBlocks, 供 pkg/engine 的 llm_call Span 复用。
//
// 刻意不在 engine 侧另写一份: turn Span 与 llm_call Span 的 Output 必须用同一套
// "assistant 产出文本化"规则 (尤其 tool_use 的呈现), 否则学习管线拿两者做 diff
// 会看到纯属格式差异的假变更。
func JoinAssistantText(blocks []types.ContentBlock) string { return joinTextBlocks(blocks) }

// joinTextBlocks 拼接 content blocks 里的 text/thinking (thinking 以标注前缀保留)。
func joinTextBlocks(blocks []types.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case types.ContentBlockText:
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case types.ContentBlockToolUse:
			// 工具调用意图也纳入 assistant 产出 (还原 action)
			parts = append(parts, "[tool_use "+b.Name+"] "+compactJSON(b.Input))
		}
	}
	return strings.Join(parts, "\n")
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
