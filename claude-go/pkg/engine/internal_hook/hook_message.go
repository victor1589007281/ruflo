// hook_message.go — 消息处理相关内置 Hook。
//
// 包含:
//   - MessageFilterHook     过滤古老纯 tool_use 单元 + 轻量内容压缩
//   - MaxTokensRecoveryHook max_output_tokens 恢复（注入 continue 消息）
//   - XMLToolFallbackHook   XML 风格工具调用回退解析
package internal_hook

import (
	"fmt"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// MessageFilterHook — 消息过滤与轻量压缩
// ============================================================================

// MessageFilterHook 在 PhasePreRequest 阶段对消息历史进行轻量级整理：
// 1. 过滤不含 reasoning 的古老 assistant + tool_result 原子单元
// 2. 对古老的 user tool_result 做轻量级内容压缩
// 目的：在 API 请求前减少无意义的 token 消耗，同时保留关键信息。
type MessageFilterHook struct{}

// NewMessageFilterHook 创建 MessageFilterHook。
func NewMessageFilterHook() *MessageFilterHook {
	return &MessageFilterHook{}
}

func (h *MessageFilterHook) Name() string                { return "message_filter" }
func (h *MessageFilterHook) Priority() int               { return 40 }
func (h *MessageFilterHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreRequest}
}

// Execute 执行消息过滤与压缩。若消息数量无变化则仍返回过滤后的结果（内容可能被压缩）。
func (h *MessageFilterHook) Execute(ctx *HookContext) (*HookResult, error) {
	// 优化1: 过滤不含 reasoning 的古老 assistant + tool_result 原子单元
	messages := FilterPureToolUseUnits(ctx.Messages, 6)

	// 优化5: 对古老的 user tool_result 做轻量级内容压缩
	for i := range messages {
		if messages[i].Type == types.MessageTypeUser && i < len(messages)-4 {
			messages[i] = CompressMessageContent(messages[i])
		}
	}

	if len(messages) != len(ctx.Messages) {
		logging.For("engine").Debug("MessageFilter applied", "before", len(ctx.Messages), "after", len(messages))
	}

	return &HookResult{Messages: messages}, nil
}

// ============================================================================
// MaxTokensRecoveryHook — max_tokens 停止后恢复
// ============================================================================

// MaxTokensRecoveryHook 在 PhaseOnStop 阶段检测 stopReason 是否为 max_tokens。
// 若是且无 tool_use（即模型因输出长度限制而中断），则注入 continue 消息
// 让模型从断点继续输出，避免重复已生成的内容。
type MaxTokensRecoveryHook struct{}

// NewMaxTokensRecoveryHook 创建 MaxTokensRecoveryHook。
func NewMaxTokensRecoveryHook() *MaxTokensRecoveryHook {
	return &MaxTokensRecoveryHook{}
}

func (h *MaxTokensRecoveryHook) Name() string                { return "max_tokens_recovery" }
func (h *MaxTokensRecoveryHook) Priority() int               { return 60 }
func (h *MaxTokensRecoveryHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhaseOnStop}
}

// Execute 检查停止原因并决定是否注入 continue 消息。
func (h *MaxTokensRecoveryHook) Execute(ctx *HookContext) (*HookResult, error) {
	if ctx.StopReason != string(types.StopReasonMaxTokens) {
		return nil, nil
	}
	// 检查是否有 tool_use（如果有 tool_use，说明不是单纯因 max_tokens 停止）
	hasToolUse := false
	for _, b := range ctx.AssistantBlocks {
		if b.Type == types.ContentBlockToolUse {
			hasToolUse = true
			break
		}
	}
	if hasToolUse {
		return nil, nil
	}

	continueMsg := &types.Message{
		Type: types.MessageTypeUser,
		UUID: GenerateUUID(),
		Content: []types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: "Continue from where you left off. Do not repeat what you already said.",
		}},
		IsMeta:    true,
		CreatedAt: time.Now(),
	}

	return &HookResult{
		InjectContinue: true,
		ContinueMsg:    continueMsg,
	}, nil
}

// ============================================================================
// MessageMetricsHook — 消息链深度分析
// ============================================================================

// MessageMetricsHook 在 PhasePreRequest 阶段分析消息链结构，将统计信息写入
// EngineMetrics。它不修改 messages，只读分析。
type MessageMetricsHook struct {
	metrics *EngineMetrics
}

// NewMessageMetricsHook 创建 MessageMetricsHook。
func NewMessageMetricsHook(metrics *EngineMetrics) *MessageMetricsHook {
	return &MessageMetricsHook{metrics: metrics}
}

func (h *MessageMetricsHook) Name() string                { return "message_metrics" }
func (h *MessageMetricsHook) Priority() int               { return 48 }
func (h *MessageMetricsHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreRequest}
}

// Execute 分析消息链并记录指标。
func (h *MessageMetricsHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.metrics == nil {
		return nil, nil
	}

	messages := ctx.Messages
	if len(messages) == 0 {
		return nil, nil
	}

	// ---------- 1. 消息链基础指标 ----------
	msgLen := int64(len(messages))
	h.metrics.MsgChainTotalRecorded.Add(1)
	if msgLen > h.metrics.MsgChainMaxLength.Load() {
		h.metrics.MsgChainMaxLength.Store(msgLen)
	}

	// ---------- 2. Token 估算（字符数/4，快速近似） ----------
	totalChars := 0
	for _, m := range messages {
		totalChars += messageChars(m)
	}
	estimatedTokens := int64(totalChars / 4)
	h.metrics.MsgChainTotalTokens.Add(estimatedTokens)
	if estimatedTokens > h.metrics.MsgChainMaxTokens.Load() {
		h.metrics.MsgChainMaxTokens.Store(estimatedTokens)
	}

	// ---------- 3. ToolResult 统计 ----------
	var toolResultCount, successCount, errorCount, trTotalChars int64
	for _, m := range messages {
		if m.Type != types.MessageTypeUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type != types.ContentBlockToolResult {
				continue
			}
			toolResultCount++
			trTotalChars += int64(len(b.Content))
			if b.IsError {
				errorCount++
			} else {
				successCount++
			}
		}
	}
	h.metrics.ToolResultsTotal.Add(toolResultCount)
	h.metrics.ToolResultsSuccess.Add(successCount)
	h.metrics.ToolResultsError.Add(errorCount)
	h.metrics.ToolResultsTotalChars.Add(trTotalChars)

	// ---------- 4. 模拟 FilterPureToolUseUnits 效果，记录删除数 ----------
	beforeLen := len(messages)
	filtered := FilterPureToolUseUnits(messages, 6)
	deletedUnits := int64(beforeLen - len(filtered))
	if deletedUnits > 0 {
		h.metrics.FilterDeletedUnits.Add(deletedUnits)
	}

	// ---------- 5. 模拟 CompressMessageContent 效果，记录压缩率 ----------
	var compressBefore, compressAfter int64
	for i, m := range filtered {
		if m.Type == types.MessageTypeUser && i < len(filtered)-4 {
			beforeChars := int64(messageChars(m))
			compressed := CompressMessageContent(m)
			afterChars := int64(messageChars(compressed))
			if afterChars < beforeChars {
				compressBefore += beforeChars
				compressAfter += afterChars
			}
		}
	}
	if compressBefore > 0 {
		h.metrics.CompressTotalBeforeChars.Add(compressBefore)
		h.metrics.CompressTotalAfterChars.Add(compressAfter)
	}

	logging.For("engine").Debug("MessageMetrics recorded",
		"msg_len", msgLen,
		"est_tokens", estimatedTokens,
		"tool_results", toolResultCount,
		"tr_success", successCount,
		"tr_error", errorCount,
		"filter_deleted", deletedUnits,
		"compress_ratio", fmt.Sprintf("%.1f%%", float64(compressAfter)*100/float64(compressBefore)),
	)

	return nil, nil
}

// messageChars 计算消息所有 content block 的字符数总和。
func messageChars(m types.Message) int {
	n := 0
	for _, b := range m.Content {
		n += len(b.Text)
		n += len(b.Content)
		n += len(b.Thinking)
	}
	return n
}

// EstimateTokens 快速估算一组消息的 token 数（字符数/4，适用于 ASCII/混合文本）。
func EstimateTokens(messages []types.Message) int {
	totalChars := 0
	for _, m := range messages {
		totalChars += messageChars(m)
	}
	return totalChars / 4
}

// EstimateMessageTokens 单条消息 token 估算。
func EstimateMessageTokens(m types.Message) int {
	return messageChars(m) / 4
}

// MessageTypeStats 返回消息类型分布统计。
func MessageTypeStats(messages []types.Message) map[string]int {
	stats := map[string]int{
		"user":        0,
		"assistant":   0,
		"tool_result": 0,
		"text":        0,
		"thinking":    0,
	}
	for _, m := range messages {
		switch m.Type {
		case types.MessageTypeUser:
			stats["user"]++
		case types.MessageTypeAssistant:
			stats["assistant"]++
		}
		for _, b := range m.Content {
			switch b.Type {
			case types.ContentBlockToolResult:
				stats["tool_result"]++
			case types.ContentBlockText:
				stats["text"]++
			case types.ContentBlockThinking:
				stats["thinking"]++
			}
		}
	}
	return stats
}

// ============================================================================
// XMLToolFallbackHook — XML 风格工具调用回退解析
// ============================================================================

// XMLToolFallbackHook 在 PhasePostRequest 阶段扫描 assistant 文本中的 XML 风格工具调用，
// 将其提取为 ContentBlockToolUse 并追加到 tool_use 列表中。
// 用于兼容旧模型或不支持原生 tool_use 格式的场景。
type XMLToolFallbackHook struct{}

// NewXMLToolFallbackHook 创建 XMLToolFallbackHook。
func NewXMLToolFallbackHook() *XMLToolFallbackHook {
	return &XMLToolFallbackHook{}
}

func (h *XMLToolFallbackHook) Name() string                { return "xml_tool_fallback" }
func (h *XMLToolFallbackHook) Priority() int               { return 130 }
func (h *XMLToolFallbackHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePostRequest}
}

// Execute 扫描 assistant blocks，提取 XML 工具调用并生成对应流式事件。
func (h *XMLToolFallbackHook) Execute(ctx *HookContext) (*HookResult, error) {
	if len(ctx.AssistantBlocks) == 0 {
		return nil, nil
	}

	mergedAssistant, mergedToolUse, ndelta := MergeXMLToolCalls(
		ctx.AssistantBlocks,
		ctx.ToolUseBlocks,
		fmt.Sprintf("t%d", ctx.TurnCount),
	)
	if ndelta <= 0 {
		return nil, nil
	}

	var events []types.StreamEvent
	for _, blk := range mergedToolUse[len(mergedToolUse)-ndelta:] {
		inputSummary := string(blk.Input)
		if len(inputSummary) > 200 {
			inputSummary = inputSummary[:200] + "..."
		}
		events = append(events, types.StreamEvent{
			Kind:      types.StreamEventToolStart,
			ToolName:  blk.Name,
			ToolInput: inputSummary,
		})
	}

	logging.For("engine").Debug("XML tool fallback applied", "new_tools", ndelta)

	return &HookResult{
		AssistantBlocks: mergedAssistant,
		ToolUseBlocks:   mergedToolUse,
		StreamEvents:    events,
	}, nil
}
