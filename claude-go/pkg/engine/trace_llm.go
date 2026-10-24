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
	"github.com/anthropic/claude-go/pkg/textutil"
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
	// Handoff 标记这条 span 是 H3-3 的交接请求 (run 到了上限、让模型写下"进度 +
	// 下一步"的那一次调用)。
	//
	// 为什么要单独标: 它长得像一次普通的 llm_call (同一套请求/响应字段), 但语义
	// 完全不同 —— 它不推进任务, 且**只在 run 临终时**出现。不标的话, "这个 run 多
	// 花了 5% token"在账本上无从解释, 而看见一次带工具的普通调用还会让人以为
	// run 是被别的东西打断的。
	Handoff bool
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
	// Handoff 与 "tools": 0 的关系是**充分不必要**: 交接请求天然不带工具, 但不带
	// 工具的调用不止它一个 (见 RunIsolated 里 DisableTools 的思考型角色)。所以这枚
	// 标记不能用 tools==0 推出来, 必须显式落 —— 账本里"这一次是临终交代"与
	// "这一次是别的无工具调用"分不开时, 交接的成本就统计不出来。
	if in.Handoff {
		attrs["handoff"] = true
	}
	if in.Err != nil {
		attrs["error"] = truncateForAttr(in.Err.Error())
	}
	// usage_reported 把"上游没报用量"与"上游报了 0"分开。
	//
	// 此前只有 in.Usage != nil 时才写 token 字段, 于是两种完全不同的情况
	// 在 trace 里长得一模一样: 调用**失败**(流没有 message_start/message_delta,
	// usage 天然为 nil) 与 provider **不上报用量**。实测 21226 条 llm_call 里
	// 14697 条是 error (全部无 usage), 6529 条 success 里 6524 条有 —— 按整体
	// 统计会得到"仅 30% 带 usage"这个**误导性**数字, 让人以为采集侧漏了 70%。
	//
	// 显式标记之后, "带用量比例"这类判据可以正确地按 status=success 分组计算,
	// 不必再靠猜。同理 cache_creation_tokens: OpenAI 兼容与 Responses 两条
	// 解析路径根本不构造该字段 (见 pkg/api/openai.go / responses.go), 所以
	// 它恒为 0 是**语义缺失**, 不是"上游报了 0" —— 这也是靠本标记区分的。
	attrs["usage_reported"] = in.Usage != nil
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
	// F6 compaction 事件锁 (planning-dsh-adopt): llm_call Span 属"折叠后才有意义
	// 的事件链", 落盘经折叠窗口门 —— 折叠窗口内阻塞排队, 窗口关闭后按序放行, 杜绝
	// Span 链横跨折叠边界。queryLoop 与 RunIsolated 两个调用点共用此收口; Compactor
	// 未装配时门是直通 no-op (fail-open)。RunIsolated 持有写锁的场景不存在 (独立
	// 循环不触发折叠), 无重入死锁面。
	e.Compactor.GateEventWrite(func() {
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

// truncateForAttr 把字符串截到 300 字符以内并以 … 收尾, 用于 span 属性值(避免超长属性撑爆轨迹)。
func truncateForAttr(s string) string {
	const max = 300
	return textutil.Truncate(s, max, "…")
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

// UsageReported 判定一条 llm_call Span 是否带上游上报的用量。
//
// 存在的意义是把"没报"与"报了 0"分开 (见 writeLLMCallSpan 里的 usage_reported)。
// 消费方 (预算闸门 / 看板 / 成本核算) 应当用本函数过滤, 而不是直接看
// input_tokens 是否存在 —— 后者会把失败的调用误算成"零消耗"。
func UsageReported(attrs map[string]any) bool {
	v, ok := attrs["usage_reported"]
	if !ok {
		// 旧 Span 没有该字段: 退化为"有 input_tokens 即认为报过"。
		// 兼容路径, 新 Span 一律显式带 usage_reported。
		_, hasInput := attrs["input_tokens"]
		return hasInput
	}
	b, ok := v.(bool)
	return ok && b
}
