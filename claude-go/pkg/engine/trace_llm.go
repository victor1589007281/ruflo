// trace_llm.go —— llm_call Span 采集 (design/03 §4.1 E1 的第 4 种 Kind)。
//
// # 为什么单独补这一种
//
// TraceStore 此前只写出 turn / tool_call / node 三种 Kind, 于是 E1 的验收项
// 「任一 run 可从 Span 完整还原 prompt→response→tool→结果链」不成立: turn Span
// 记的是"用户意图 → assistant 全文", 中间**真正发给模型的那份 prompt**(system +
// 完整消息数组 + 工具 schema) 一个字都没留。而 §1.3 列的"action 完整文本缺失"
// 恰恰指的就是它 —— llm.jsonl 只有计数, transcript 只有消息, 谁都没有请求正文。
//
// 一轮 turn 可能对应多次 llm_call (重试 / fallback 换模型 / PTL 压缩后重发),
// 所以 llm_call 必须是独立粒度而不能折叠进 turn: 学习管线要能看出"这个产出是第
// 三次重试才拿到的", 那与一次就成功的产出不是同一个 action。
//
// # 成本
//
// prompt 正文经 tracestore.MakeRef 走内容寻址: system prompt / 工具 schema 在同一
// 个 run 里逐轮完全重复, Blob 天然去重, 落盘量远小于"每轮一份全文"的直觉估计。
// 采样率仍由 CLAUDE_GO_TRACE_SAMPLE 统一控制 (元数据恒全采)。
package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

// llmCallSpan 一次 LLM 调用的轨迹素材。
//
// 刻意用一个结构体而不是 10 个位置参数: 两个调用点 (流式 queryLoop 与
// RunIsolated) 能给出的字段不完全一样, 结构体让缺省字段自然为零值, 而位置参数
// 会逼着调用点传一串 nil/""。
type llmCallSpan struct {
	Start      time.Time
	Model      string
	System     []string // system prompt 各段
	Messages   []types.APIMessage
	Tools      []types.APITool
	Output     string // assistant 全文 (含 tool_use 意图)
	Usage      *types.Usage
	StopReason string
	Err        error
	Stream     bool
	Attempt    int // 本 turn 内第几次调用 (0 起); 重试/fallback 时递增
	Isolated   bool
}

// writeLLMCallSpan 写一条 llm_call Span。TraceStore 未装配时是零成本 no-op。
func (e *QueryEngine) writeLLMCallSpan(ctx context.Context, in llmCallSpan) {
	if e == nil || e.TraceStore == nil {
		return
	}
	ids := trace.From(ctx)
	status := "success"
	if in.Err != nil {
		status = "error"
	}
	attrs := map[string]any{
		"status":      status,
		"model":       in.Model,
		"stream":      in.Stream,
		"stop_reason": in.StopReason,
		"attempt":     in.Attempt,
		"tools":       len(in.Tools),
		"messages":    len(in.Messages),
	}
	if in.Isolated {
		attrs["isolated"] = true
	}
	if in.Err != nil {
		attrs["error"] = truncateForAttr(in.Err.Error())
	}
	if in.Usage != nil {
		attrs["input_tokens"] = in.Usage.InputTokens
		attrs["output_tokens"] = in.Usage.OutputTokens
		attrs["cache_read_tokens"] = in.Usage.CacheReadInputTokens
		attrs["cache_creation_tokens"] = in.Usage.CacheCreationInputTokens
	}
	if ids.CallID != "" {
		attrs["trace_call"] = ids.CallID
	}
	start := in.Start
	if start.IsZero() {
		start = time.Now()
	}
	e.TraceStore.Write(tracestore.Span{
		TraceID:   ids.RunID,
		SpanID:    internal_hook.GenerateUUID(),
		ParentID:  ids.TurnID,
		Kind:      tracestore.KindLLMCall,
		Name:      in.Model,
		NodeID:    ids.NodeID,
		TurnID:    ids.TurnID,
		InputRef:  e.TraceStore.MakeRef(renderLLMRequest(in.System, in.Messages, in.Tools)),
		OutputRef: e.TraceStore.MakeRef(in.Output),
		Attrs:     attrs,
		TS:        start.UnixMilli(),
		DurMS:     time.Since(start).Milliseconds(),
	})
}

// renderLLMRequest 把"真正发出去的那份请求"渲染成可还原的纯文本。
//
// 用人可读的分段文本而不是原始 JSON: 学习管线 (蒸馏/回放/导出) 消费的是 prompt
// 语义, 而 JSON 转义会让同一段 system prompt 在不同轮里字节不同, 破坏 Blob 去重。
// 工具只记名字与 schema 长度 —— 全量 schema 每轮重复且极长, 而"这轮暴露了哪些
// 工具"才是策略信息 (design/03 §4.4 policy_decision 的一部分)。
func renderLLMRequest(system []string, messages []types.APIMessage, tools []types.APITool) string {
	var b strings.Builder
	for i, s := range system {
		if strings.TrimSpace(s) == "" {
			continue
		}
		b.WriteString("=== system[")
		b.WriteString(itoa(i))
		b.WriteString("] ===\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	if len(tools) > 0 {
		b.WriteString("=== tools ===\n")
		for _, t := range tools {
			b.WriteString("- ")
			b.WriteString(t.Name)
			b.WriteString(" (schema ")
			b.WriteString(itoa(len(t.InputSchema)))
			b.WriteString("B)\n")
		}
	}
	for _, m := range messages {
		b.WriteString("=== ")
		b.WriteString(m.Role)
		b.WriteString(" ===\n")
		b.WriteString(renderAPIContent(m.Content))
		b.WriteString("\n")
	}
	return b.String()
}

// renderAPIContent APIMessage.Content 是 json.RawMessage, 实际形态可能是裸字符串
// 也可能是 content block 数组 (两种都是 Anthropic 合法形态), 统一渲染成文本。
//
// 解析失败时**原样返回 JSON** 而不是返回空: 轨迹的用途是还原真实请求, 宁可留一份
// 不好看的 JSON, 也不能因为解析器不认识某种新块类型就把正文丢掉。
func renderAPIContent(content json.RawMessage) string {
	raw := strings.TrimSpace(string(content))
	if raw == "" || raw == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []types.ContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return raw
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case types.ContentBlockText:
			parts = append(parts, b.Text)
		case types.ContentBlockThinking:
			parts = append(parts, "[thinking] "+b.Thinking)
		case types.ContentBlockToolUse:
			parts = append(parts, "[tool_use "+b.Name+"] "+string(b.Input))
		case types.ContentBlockToolResult:
			tag := "[tool_result " + b.ToolUseID + "]"
			if b.IsError {
				tag = "[tool_result_error " + b.ToolUseID + "]"
			}
			parts = append(parts, tag+" "+b.Content)
		default:
			parts = append(parts, "["+string(b.Type)+"]")
		}
	}
	return strings.Join(parts, "\n")
}

func truncateForAttr(s string) string {
	const max = 300
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// itoa 避免为两处整数拼接引入 strconv (本文件其余部分不需要它)。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
