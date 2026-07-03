// AdvisorTool — 无参数顾问工具，对齐 Claude Code 官方 advisor tool 语义。
// 设计文档: docs/advisor-tool-design.md
//
// 官方 advisor 是服务端工具 (beta: advisor-tool-2026-03-01)；claude-go 后端为
// OpenAI 兼容模型，无服务端支持，故在客户端模拟：主模型调用 advisor() 时，
// harness 把 ToolContext.Messages 全量序列化为 transcript，用独立的强模型
// Client 发起一次无工具调用，建议文本作为 tool_result 返回主循环。
//
// 关键语义 (对齐官方):
//   - 工具无任何输入参数 → 主模型无法用问题框架带偏 advisor，也无法只转发部分上下文
//   - advisor 调用不带工具 → 结构上不可能递归
//   - advisor 任何失败都降级为 IsError tool_result，绝不中断主循环
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// AdvisorOptions advisor 工具的护栏配置。
// 官方由服务端承担预算控制；claude-go 必须自建。
type AdvisorOptions struct {
	// MaxCallsPerSession 单会话 (单 AdvisorTool 实例) 最大调用次数，0 使用默认值 8。
	MaxCallsPerSession int
	// CooldownTurns 两次模型主动调用之间至少间隔的 assistant 轮数，0 使用默认值 2。
	CooldownTurns int
	// MaxTranscriptTokens transcript 序列化的 token 预算 (chars/4 估算)，0 使用默认值 60000。
	MaxTranscriptTokens int
	// MaxOutputTokens advisor 回复的最大输出 token，0 使用默认值 2000。
	MaxOutputTokens int
}

func (o AdvisorOptions) withDefaults() AdvisorOptions {
	if o.MaxCallsPerSession <= 0 {
		o.MaxCallsPerSession = 8
	}
	if o.CooldownTurns < 0 {
		o.CooldownTurns = 0
	} else if o.CooldownTurns == 0 {
		o.CooldownTurns = 2
	}
	if o.MaxTranscriptTokens <= 0 {
		o.MaxTranscriptTokens = 60000
	}
	if o.MaxOutputTokens <= 0 {
		o.MaxOutputTokens = 2000
	}
	return o
}

// ErrAdvisorBudgetExhausted 预算护栏 sentinel：本会话 advisor 调用次数已达上限。
var ErrAdvisorBudgetExhausted = fmt.Errorf("advisor budget exhausted")

// advisorSystemPrompt advisor 人设 (设计文档 3.2(3))。
const advisorSystemPrompt = `你是一位资深技术顾问。另一个 AI agent 正在执行任务，它把完整工作轨迹转发给你，请求方向性指导。你看到的是它的任务目标、每一次工具调用和每一个结果。

审视并简洁输出（不超过400字）：
1. 方向判断：当前路径是否通向任务目标？有没有更省的路？
2. 风险：它正在依赖的哪个假设最可疑？哪个证据被它低估或误读了？
3. 下一步：接下来 1-3 步具体应该做什么（可执行，不要泛泛而谈）。
4. 若它认为任务已完成：指出遗漏的验证或未覆盖的边界，或确认可以收工。

不要复述轨迹。不要客套。直接给判断。`

// advisorDescription 主模型看到的工具描述 — 移植官方 "# Advisor Tool" 引导 prompt
// (Claude Code 2.1.120 二进制提取原文，见设计文档 1.3)。
// 该文本经 prompt.Manager 随工具描述进入 system prompt，是官方设计的核心 IP。
const advisorDescription = `Consult a stronger reviewer model for guidance. This tool takes NO parameters -- when you call advisor(), your entire conversation history is automatically forwarded. The advisor sees the task, every tool call you've made, every result you've seen.

Call advisor BEFORE substantive work -- before writing, before committing to an interpretation, before building on an assumption. If the task requires orientation first (finding files, fetching a source, seeing what's there), do that, then call advisor. Orientation is not substantive work. Writing, editing, and declaring an answer are.

Also call advisor:
- When you believe the task is complete. BEFORE this call, make your deliverable durable: write the file, save the result, commit the change. The advisor call takes time; if the session ends during it, a durable result persists and an unwritten one doesn't.
- When stuck -- errors recurring, approach not converging, results that don't fit.
- When considering a change of approach.

On tasks longer than a few steps, call advisor at least once before committing to an approach and once before declaring done. On short reactive tasks where the next action is dictated by tool output you just read, you don't need to keep calling -- the advisor adds most of its value on the first call, before the approach crystallizes.

Give the advice serious weight. If you follow a step and it fails empirically, or you have primary-source evidence that contradicts a specific claim (the file says X, the paper states Y), adapt. A passing self-test is not evidence the advice is wrong -- it's evidence your test doesn't check what the advice is checking.

If you've already retrieved data pointing one way and the advisor points another: don't silently switch. Surface the conflict in one more advisor call -- "I found X, you suggest Y, which constraint breaks the tie?" The advisor saw your evidence but may have underweighted it; a reconcile call is cheaper than committing to the wrong branch.`

// AdvisorTool 实现 tool.Tool。每个会话 (registry) 创建独立实例，
// 调用计数与冷却状态为实例级。
type AdvisorTool struct {
	client *api.Client
	opts   AdvisorOptions

	mu                sync.Mutex
	calls             int // 累计成功发起的 advisor LLM 调用 (含 checkpoint 主动咨询)
	lastCallTurnCount int // 上次模型主动调用时的 assistant 消息数 (冷却依据)
}

// NewAdvisorTool 创建 advisor 工具。client 为 advisor 模型专用客户端
// (模型/端点可与主模型不同)；client 为 nil 时工具不可用 (Call 返回错误结果)。
func NewAdvisorTool(client *api.Client, opts AdvisorOptions) *AdvisorTool {
	return &AdvisorTool{client: client, opts: opts.withDefaults()}
}

func (t *AdvisorTool) Name() string        { return "advisor" }
func (t *AdvisorTool) Description() string { return advisorDescription }

func (t *AdvisorTool) InputSchema() json.RawMessage {
	// 无参数 (对齐官方)。additionalProperties:false 防止模型自作主张塞参数。
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

// IsConcurrencySafe 返回 false：advisor 的语义是"停下来咨询"，
// 且建议必须基于最新历史，不应与写工具并发。
func (t *AdvisorTool) IsConcurrencySafe(json.RawMessage) bool { return false }

func (t *AdvisorTool) IsReadOnly(json.RawMessage) bool { return true }

func (t *AdvisorTool) CheckPermissions(json.RawMessage, *tool.ToolContext) *types.PermissionResult {
	return nil // 只读咨询，无需确认
}

// Call 主模型主动调用入口：护栏检查 → Consult。
// 护栏拒绝返回非 error 的 tool_result (避免主模型把 error 当环境故障重试)；
// LLM 调用失败返回 IsError tool_result (对齐官方 advisor_tool_result_error)。
func (t *AdvisorTool) Call(ctx context.Context, _ json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	if t.client == nil {
		return &tool.ToolResult{Content: "Advisor unavailable (not configured)", IsError: true}, nil
	}
	var messages []types.Message
	if tctx != nil {
		messages = tctx.Messages
	}

	turnCount := countAssistantMessages(messages)
	t.mu.Lock()
	if t.calls > 0 && turnCount-t.lastCallTurnCount < t.opts.CooldownTurns {
		t.mu.Unlock()
		return &tool.ToolResult{Content: fmt.Sprintf(
			"Advisor cooldown: you consulted the advisor within the last %d turns. Act on the previous advice first; consult again after making progress.",
			t.opts.CooldownTurns)}, nil
	}
	t.mu.Unlock()

	advice, err := t.Consult(ctx, messages)
	if err == ErrAdvisorBudgetExhausted {
		return &tool.ToolResult{Content: "Advisor budget exhausted for this session; proceed with your own judgment."}, nil
	}
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("Advisor unavailable (%v)", err), IsError: true}, nil
	}

	t.mu.Lock()
	t.lastCallTurnCount = turnCount
	t.mu.Unlock()
	return &tool.ToolResult{Content: advice}, nil
}

// Consult 把 messages 序列化为 transcript 并咨询 advisor 模型。
// 供 Call (模型主动) 与 engine 的 AdvisorCheckpointHook (harness 主动, Phase 3) 共用；
// 两路共享 MaxCallsPerSession 预算，冷却仅约束模型主动调用。
// transcript 超长触发 prompt too long 时对半折减重试一次。
func (t *AdvisorTool) Consult(ctx context.Context, messages []types.Message) (string, error) {
	if t.client == nil {
		return "", fmt.Errorf("not configured")
	}
	t.mu.Lock()
	if t.calls >= t.opts.MaxCallsPerSession {
		t.mu.Unlock()
		return "", ErrAdvisorBudgetExhausted
	}
	t.calls++
	t.mu.Unlock()

	mctx := api.WithLLMMetrics(ctx, api.LLMMetricsContext{Source: "advisor_tool", Purpose: "advisor"})
	budget := t.opts.MaxTranscriptTokens
	advice, err := t.consultOnce(mctx, messages, budget)
	if err != nil && isPromptTooLong(err) {
		advice, err = t.consultOnce(mctx, messages, budget/2)
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(advice) == "" {
		// 对齐官方空结果占位符语义
		return "[Advisor response]", nil
	}
	return advice, nil
}

func (t *AdvisorTool) consultOnce(ctx context.Context, messages []types.Message, transcriptBudget int) (string, error) {
	transcript := BuildAdvisorTranscript(messages, transcriptBudget)
	apiMsgs := []types.APIMessage{{
		Role: "user",
		Content: mustMarshalBlocks([]types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: transcript,
		}}),
	}}
	// tools=nil → advisor 无法调用工具，结构上防递归
	resp, err := t.client.SendMessage(ctx, apiMsgs, []string{advisorSystemPrompt}, nil, t.opts.MaxOutputTokens)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == types.ContentBlockText {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

// CallCount 返回累计 advisor 咨询次数 (含 checkpoint 主动咨询)。
func (t *AdvisorTool) CallCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func isPromptTooLong(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*api.PromptTooLongError); ok {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "prompt is too long") || strings.Contains(msg, "prompt_too_long") ||
		strings.Contains(msg, "context length") || strings.Contains(msg, "context_length_exceeded") ||
		strings.Contains(msg, "maximum context")
}

func countAssistantMessages(messages []types.Message) int {
	n := 0
	for _, m := range messages {
		if m.Type == types.MessageTypeAssistant {
			n++
		}
	}
	return n
}

func mustMarshalBlocks(blocks []types.ContentBlock) json.RawMessage {
	data, err := json.Marshal(blocks)
	if err != nil {
		return json.RawMessage(`[{"type":"text","text":""}]`)
	}
	return data
}

// ============================================================================
// Transcript 序列化 (设计文档 3.2(2))
// ============================================================================

const (
	transcriptToolInputCap  = 500  // tool_use 输入截断 (字符)
	transcriptToolResultCap = 2000 // tool_result 截断 (字符, 尾部保留)
	transcriptTextCap       = 6000 // user/assistant 文本截断 (字符)
)

// BuildAdvisorTranscript 把会话历史渲染成纯文本轨迹。
// 官方语义是"整个会话历史自动转发"；有限上下文下的务实近似：
// 超出预算时保头 (任务定义) + 保尾 (最近轨迹)，中段折叠。
// maxTokens 用 chars/4 估算。
func BuildAdvisorTranscript(messages []types.Message, maxTokens int) string {
	if maxTokens <= 0 {
		maxTokens = 60000
	}
	entries := make([]string, 0, len(messages))
	for _, msg := range messages {
		if s := renderTranscriptEntry(msg); s != "" {
			entries = append(entries, s)
		}
	}
	if len(entries) == 0 {
		return "(empty conversation)"
	}

	header := "Below is the complete work trajectory of the agent requesting your guidance (oldest first):\n\n"
	budgetChars := maxTokens * 4

	total := len(header)
	for _, e := range entries {
		total += len(e) + 1
	}
	if total <= budgetChars {
		return header + strings.Join(entries, "\n")
	}

	// 保头: 首条 user (任务定义) 必留; 之后从尾部倒序回填直到预算耗尽。
	head := []string{entries[0]}
	remaining := budgetChars - len(header) - len(entries[0]) - 64 // 折叠标记余量
	var tailRev []string
	tailStart := len(entries) // entries[tailStart:] 被保留
	for i := len(entries) - 1; i >= 1; i-- {
		cost := len(entries[i]) + 1
		if cost > remaining {
			break
		}
		remaining -= cost
		tailRev = append(tailRev, entries[i])
		tailStart = i
	}
	tail := make([]string, 0, len(tailRev))
	for i := len(tailRev) - 1; i >= 0; i-- {
		tail = append(tail, tailRev[i])
	}
	omitted := tailStart - 1
	parts := head
	if omitted > 0 {
		parts = append(parts, fmt.Sprintf("[... %d messages omitted ...]", omitted))
	}
	parts = append(parts, tail...)
	return header + strings.Join(parts, "\n")
}

func renderTranscriptEntry(msg types.Message) string {
	var sb strings.Builder
	role := string(msg.Type)
	for _, block := range msg.Content {
		switch block.Type {
		case types.ContentBlockText:
			text := strings.TrimSpace(block.Text)
			if text == "" {
				continue
			}
			sb.WriteString(fmt.Sprintf("[%s] %s\n", role, truncateHead(text, transcriptTextCap)))
		case types.ContentBlockToolUse:
			input := strings.TrimSpace(string(block.Input))
			sb.WriteString(fmt.Sprintf("[tool_use] %s(%s)\n", block.Name, truncateHead(input, transcriptToolInputCap)))
		case types.ContentBlockToolResult:
			label := "tool_result"
			if block.IsError {
				label = "tool_result:ERROR"
			}
			content := strings.TrimSpace(block.Content)
			sb.WriteString(fmt.Sprintf("[%s] %s\n", label, truncateTail(content, transcriptToolResultCap)))
		case types.ContentBlockThinking:
			// thinking 跳过: 是主模型内部状态，且部分后端不允许跨模型转发
		case types.ContentBlockImage:
			sb.WriteString("[image]\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// truncateHead 保留头部。
func truncateHead(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... [+%d chars]", len(s)-max)
}

// truncateTail 保留尾部 (tool_result 的错误信息/结论通常在尾部)。
func truncateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("[%d chars truncated] ...", len(s)-max) + s[len(s)-max:]
}
