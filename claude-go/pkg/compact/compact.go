// Package compact 实现上下文压缩 (context compaction) 系统。
// 对应 TS 源码: review/claude/src/services/compact/compact.ts
//                review/claude/src/services/compact/autoCompact.ts
//                review/claude/src/services/compact/microCompact.ts
//
// 上下文压缩是 Claude Code 处理长对话的关键机制。
// 当对话 token 数接近模型上下文窗口限制时，
// 系统会调用模型将历史对话压缩成简洁的摘要。
//
// 压缩层次:
//   1. MicroCompact - 细粒度: 截断过大的工具输出 (如大文件内容)
//   2. AutoCompact - 粗粒度: 将整段对话历史压缩为摘要
//   3. ReactiveCompact - 应急: 当 API 返回 prompt_too_long 时触发
//
// 压缩后的消息结构:
//   [CompactBoundaryMessage, SummaryMessage, ...recent_messages]
//   CompactBoundary 标记压缩点，之前的消息被摘要替代。
package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// TokenThreshold 触发自动压缩的 token 阈值比例 (当前 context 占比)
const TokenThreshold = 0.8

// CompactMaxOutputTokens 压缩请求的最大输出 token
const CompactMaxOutputTokens = 8192

// Compactor 上下文压缩器
//
// # 13.6-P1 F6: compaction 事件锁 (docforge planning-dsh-adopt)
//
// 规划语义: 「压缩期间事件写入阻塞排队，杜绝折叠竞态」。此前 pkg/compact 与引擎
// 主循环只有时序约定 (同 goroutine 内顺序执行), 无显式锁语义 —— 跨 goroutine 的
// 事件落盘 (llm_call/node Span 来自团队并发路径) 与折叠窗口交错时, 轨迹上会出现
// 折叠后事件链与摘要边界错序的窗口。现在把该约定物化为显式锁:
//
//   - 折叠窗口: runCompaction 全程 (含 PreCompact 蒸馏与摘要 LLM 调用) 持
//     eventMu 写锁; 并发折叠在同锁上自然串行。
//   - 事件写入门: GateEventWrite 让"折叠后才有意义的事件写入"在窗口内阻塞排队,
//     窗口关闭后按 Lock 顺序放行 —— 事件链要么完整在折叠前, 要么完整在折叠后,
//     不会横跨折叠边界。
//
// Compactor 是 per-engine 实例 (feishu 主会话/嵌套代理/团队 runner 各一份), 锁
// 作用域即单引擎, 与 feishu per-chat busy 串行化正交。存储层 (tracestore/
// statestore) 本身线程安全, 此锁补的是顺序性语义而非数据损坏防护。
type Compactor struct {
	apiClient        *api.Client
	maxContextTokens int // 模型最大上下文窗口

	// eventMu 折叠窗口锁 (F6): runCompaction 持写锁; 事件写入侧经
	// GateEventWrite 持读锁阻塞排队。RWMutex 而非 Mutex: 无折叠时事件写入
	// 零阻塞 (读锁共享), 只有窗口真正打开时才出现排队。
	eventMu      sync.RWMutex
	compacting   atomic.Bool  // 窗口状态位: 写锁内置位, Compacting() 快照精确
	compactions  atomic.Int64 // 折叠窗口累计打开次数 (观测用)
	eventsQueued atomic.Int64 // 事件写入过门总次数 (观测用; 排队与否见窗口期时延)

	// PreCompact 蒸馏: 压缩前将关键信息提取到持久记忆
	PreCompactFn func(facts []string, source string)
}

// Compacting 报告折叠窗口当前是否打开 (测试/观测用; 快照语义, 返回后可能变化)。
// 状态位在写锁内置位/清除, 与窗口开关精确同步。
func (c *Compactor) Compacting() bool {
	if c == nil {
		return false
	}
	return c.compacting.Load()
}

// Stats 返回 F6 锁的观测计数: compactions=折叠窗口打开总次数,
// events_queued=事件写入过门总次数。
func (c *Compactor) Stats() (compactions, eventsQueued int64) {
	if c == nil {
		return 0, 0
	}
	return c.compactions.Load(), c.eventsQueued.Load()
}

// GateEventWrite 把事件写入 fn 包进折叠窗口门: 窗口打开时阻塞排队, 关闭后立即
// 通过; 与进行中的折叠互斥 (读锁 vs 写锁)。
//
// fn 纪律: 只做事件落盘, 不得再经 GateEventWrite 嵌套取门, 也不得触发折叠 ——
// 否则同一 goroutine 读锁内再取写锁将死锁。fail-open: c 为 nil (未装配压缩器,
// 例如 RunIsolated 的独立小循环) 时直接执行, 与无锁语义等价。
func (c *Compactor) GateEventWrite(fn func()) {
	if c == nil || fn == nil {
		if fn != nil {
			fn()
		}
		return
	}
	c.eventMu.RLock()
	defer c.eventMu.RUnlock()
	c.eventsQueued.Add(1) // 无竞态误差: 未阻塞时也计一次"过门", 排队与否见窗口期时延
	fn()
}

// NewCompactor 创建压缩器
func NewCompactor(apiClient *api.Client, maxContextTokens int) *Compactor {
	if maxContextTokens == 0 {
		maxContextTokens = 200000 // 默认 200k
	}
	return &Compactor{
		apiClient:     apiClient,
		maxContextTokens: maxContextTokens,
	}
}

// AutoCompact 自动压缩: 当 token 使用量超过阈值时触发。
// 对应 TS: services/compact/autoCompact.ts 中的自动压缩逻辑
//
// 算法:
//   1. 估算当前 token 数 (简化: 按字符数 / 4)
//   2. 如果低于阈值 → 不压缩
//   3. 如果超过阈值 → 调用 runCompaction()
//   4. 返回 [boundary, summary, ...recent_tail]
func (c *Compactor) AutoCompact(ctx context.Context, messages []types.Message, model string) ([]types.Message, error) {
	estimatedTokens := estimateTokens(messages)
	threshold := int(float64(c.maxContextTokens) * TokenThreshold)

	if estimatedTokens < threshold {
		return nil, nil // 无需压缩
	}

	return c.runCompaction(ctx, messages, model)
}

// runCompaction 执行实际的上下文压缩。
// 对应 TS: services/compact/compact.ts 中的核心压缩逻辑
//
// 压缩策略:
//   1. 保留最近 N 条消息 (尾部保护)
//   2. 将其余消息发送给模型, 要求生成摘要
//   3. 构建新的消息序列: [boundary, summary_user_msg, ...tail]
//
// 全程处于 F6 折叠窗口 (eventMu 写锁) 内。
// SetPreCompactFn 设置 PreCompact 蒸馏回调
func (c *Compactor) SetPreCompactFn(fn func(facts []string, source string)) {
	c.PreCompactFn = fn
}

func (c *Compactor) runCompaction(ctx context.Context, messages []types.Message, model string) ([]types.Message, error) {
	// F6 折叠窗口: 写锁自蒸馏起、至摘要落定止。窗口内事件写入侧 (GateEventWrite)
	// 阻塞排队, 折叠完成后按序放行 —— 折叠竞态在此显式收口。写锁不可重入, 故
	// 窗口内绝不调 GateEventWrite, PreCompactFn 实现方亦不得 (见其纪律注释)。
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.compacting.Store(true)
	c.compactions.Add(1)
	defer c.compacting.Store(false)

	// PreCompact 蒸馏: 在压缩前提取关键信息到持久记忆
	if c.PreCompactFn != nil {
		facts := c.SmartExtractKeyFacts(ctx, messages)
		if len(facts) > 0 {
			c.PreCompactFn(facts, "pre_compact")
		}
	}

	// 保护尾部 (最近 6 条消息不被压缩, 从 4 提升)
	tailCount := 6
	if tailCount > len(messages) {
		tailCount = len(messages)
	}

	if len(messages) <= tailCount {
		return nil, nil // 消息太少，无需压缩
	}

	toCompress := messages[:len(messages)-tailCount]
	tail := messages[len(messages)-tailCount:]

	// 构建压缩提示
	var sb strings.Builder
	sb.WriteString("Please provide a concise summary of the following conversation. ")
	sb.WriteString("Focus on: key decisions made, files modified, current state of the task, and important context. ")
	sb.WriteString("Be specific about file paths, function names, and technical details.\n\n")

	for _, msg := range toCompress {
		role := string(msg.Type)
		text := extractText(msg)
		if text != "" {
			sb.WriteString(fmt.Sprintf("[%s]: %s\n\n", role, truncate(text, 2000)))
		}
	}

	compactMessages := []types.APIMessage{{
		Role:    "user",
		Content: mustMarshal([]types.ContentBlock{{Type: types.ContentBlockText, Text: sb.String()}}),
	}}

	resp, err := c.apiClient.SendMessage(ctx, compactMessages, []string{"You are a conversation summarizer."}, nil, CompactMaxOutputTokens)
	if err != nil {
		return nil, fmt.Errorf("压缩请求失败: %w", err)
	}

	summaryText := ""
	for _, block := range resp.Content {
		if block.Type == types.ContentBlockText {
			summaryText += block.Text
		}
	}

	// 构建压缩后的消息序列
	result := []types.Message{
		{
			Type:              types.MessageTypeSystem,
			UUID:              fmt.Sprintf("compact-%d", len(messages)),
			Content:           []types.ContentBlock{{Type: types.ContentBlockText, Text: "[Context compacted]"}},
			IsCompactBoundary: true,
		},
		{
			Type: types.MessageTypeUser,
			UUID: fmt.Sprintf("summary-%d", len(messages)),
			Content: []types.ContentBlock{{
				Type: types.ContentBlockText,
				Text: fmt.Sprintf("<context_summary>\n%s\n</context_summary>", summaryText),
			}},
		},
	}
	result = append(result, tail...)

	return result, nil
}

// MicroCompact 微压缩: 截断过大的工具输出。
// 对应 TS: services/compact/microCompact.ts
//
// 遍历消息中的 tool_result 块，
// 如果内容超过限制则截断并添加截断提示。
func MicroCompact(messages []types.Message, maxResultChars int) []types.Message {
	if maxResultChars == 0 {
		maxResultChars = 50000
	}

	result := make([]types.Message, len(messages))
	for i, msg := range messages {
		newMsg := msg
		if msg.Type == types.MessageTypeUser {
			newBlocks := make([]types.ContentBlock, len(msg.Content))
			for j, block := range msg.Content {
				if block.Type == types.ContentBlockToolResult && len(block.Content) > maxResultChars {
					block.Content = block.Content[:maxResultChars] + "\n... (output truncated)"
				}
				newBlocks[j] = block
			}
			newMsg.Content = newBlocks
		}
		result[i] = newMsg
	}
	return result
}

// GetMessagesAfterCompactBoundary 获取 compact boundary 之后的消息。
// 对应 TS: utils/messages.ts 中的 getMessagesAfterCompactBoundary()
func GetMessagesAfterCompactBoundary(messages []types.Message) []types.Message {
	lastBoundary := -1
	for i, msg := range messages {
		if msg.IsCompactBoundary {
			lastBoundary = i
		}
	}
	if lastBoundary >= 0 {
		return messages[lastBoundary:]
	}
	return messages
}

// 辅助函数

func estimateTokens(messages []types.Message) int {
	total := 0
	for _, msg := range messages {
		for _, block := range msg.Content {
			total += len(block.Text)/4 + len(block.Content)/4
		}
	}
	return total
}

func extractText(msg types.Message) string {
	var parts []string
	for _, block := range msg.Content {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
		if block.Content != "" {
			parts = append(parts, block.Content)
		}
	}
	return strings.Join(parts, "\n")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func mustMarshal(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// SmartExtractKeyFacts 使用 LLM 智能提取关键事实 (Anchored Iterative Summarization)。
// 业界最佳实践: 仅对新增消息段做增量摘要, 合并到持久化状态。
// 参考: Mem0 fact-extraction + Letta sliding-window compaction
//
// 如果 LLM 调用失败, 自动回退到 ExtractKeyFacts 启发式提取。
func (c *Compactor) SmartExtractKeyFacts(ctx context.Context, messages []types.Message) []string {
	if c.apiClient == nil {
		return ExtractKeyFacts(messages)
	}

	var sb strings.Builder
	count := 0
	for _, msg := range messages {
		text := extractText(msg)
		if text == "" || msg.IsApiErrorMessage || msg.IsMeta {
			continue
		}
		role := string(msg.Type)
		sb.WriteString(fmt.Sprintf("[%s]: %s\n\n", role, truncate(text, 800)))
		count++
	}
	if count < 2 || sb.Len() < 100 {
		return ExtractKeyFacts(messages)
	}

	systemPrompt := `You are a memory extraction agent. Extract key facts from the conversation that are worth remembering for future sessions.

Output a JSON array of strings. Each string is one concise fact (1-2 sentences).

Focus on:
- Important decisions made and their rationale
- Files created/modified/deleted and why
- Technical patterns, architectures, or tools used
- User preferences and working style
- Errors encountered and how they were resolved
- Key conclusions or outcomes

Exclude: trivial acknowledgments, repeated info, generic advice.
Maximum 10 facts. Output ONLY valid JSON array, nothing else.`

	resp, err := c.apiClient.SimpleComplete(ctx, systemPrompt, "Extract key facts:\n\n"+sb.String())
	if err != nil {
		return ExtractKeyFacts(messages)
	}

	var facts []string
	resp = strings.TrimSpace(resp)
	// 尝试直接解析 JSON
	if err := json.Unmarshal([]byte(resp), &facts); err != nil {
		// 尝试从 markdown code block 中提取
		if idx := strings.Index(resp, "["); idx >= 0 {
			if end := strings.LastIndex(resp, "]"); end > idx {
				_ = json.Unmarshal([]byte(resp[idx:end+1]), &facts)
			}
		}
	}
	if len(facts) == 0 {
		return ExtractKeyFacts(messages)
	}
	return facts
}

// ExtractKeyFacts 启发式提取关键事实 (LLM 不可用时的回退方案)。
// 比原版更智能: 识别文件路径、决策语句、错误-解决对。
func ExtractKeyFacts(messages []types.Message) []string {
	var facts []string
	for _, msg := range messages {
		text := extractText(msg)
		if text == "" || msg.IsApiErrorMessage || msg.IsMeta {
			continue
		}

		switch msg.Type {
		case types.MessageTypeAssistant:
			facts = append(facts, extractStructuredFacts(text)...)
		case types.MessageTypeUser:
			if len(text) > 30 {
				facts = append(facts, "用户: "+truncate(text, 200))
			}
		}
	}

	seen := make(map[string]bool)
	var unique []string
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f != "" && !seen[f] && len(f) > 10 {
			seen[f] = true
			unique = append(unique, f)
		}
	}
	if len(unique) > 15 {
		unique = unique[:15]
	}
	return unique
}

// extractStructuredFacts 从 assistant 回复中提取结构化事实
func extractStructuredFacts(text string) []string {
	var facts []string

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 文件操作: 包含路径模式的行
		if containsFilePath(line) && len(line) < 300 {
			facts = append(facts, truncate(line, 200))
			continue
		}
		// 决策语句
		if isDecisionStatement(line) && len(line) < 300 {
			facts = append(facts, truncate(line, 200))
			continue
		}
		// 错误和解决方案
		if isErrorSolution(line) && len(line) < 300 {
			facts = append(facts, truncate(line, 200))
		}
	}

	// 补充: 如果结构化提取太少, 加入整体摘要
	if len(facts) < 2 && len(text) > 200 {
		facts = append(facts, truncate(text, 300))
	}
	return facts
}

// containsFilePath 检查是否包含文件路径
func containsFilePath(s string) bool {
	pathPatterns := []string{"/", ".go", ".ts", ".js", ".py", ".rs", ".java", ".md", ".json", ".yaml", ".toml"}
	for _, p := range pathPatterns {
		if strings.Contains(s, p) && (strings.Contains(s, "pkg/") || strings.Contains(s, "src/") ||
			strings.Contains(s, "创建") || strings.Contains(s, "修改") || strings.Contains(s, "删除") ||
			strings.Contains(s, "create") || strings.Contains(s, "modify") || strings.Contains(s, "wrote") ||
			strings.Contains(s, "updated")) {
			return true
		}
	}
	return false
}

// isDecisionStatement 检查是否是决策/结论语句
func isDecisionStatement(s string) bool {
	lower := strings.ToLower(s)
	markers := []string{
		"决定", "选择", "采用", "使用", "方案", "结论", "建议",
		"decided", "chose", "using", "approach", "conclusion", "recommend",
		"should", "will use", "best practice", "pattern",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// isErrorSolution 检查是否是错误-解决对
func isErrorSolution(s string) bool {
	lower := strings.ToLower(s)
	markers := []string{
		"error", "错误", "bug", "fix", "修复", "解决", "solution",
		"问题", "原因", "cause", "resolved", "workaround",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// ExtractText 导出 extractText 供外部使用
func ExtractText(msg types.Message) string {
	return extractText(msg)
}
