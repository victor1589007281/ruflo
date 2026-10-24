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

// Name 返回 hook 名。
func (h *MessageFilterHook) Name() string                { return "message_filter" }
// Priority 40: 早于 AutoMode(45) 与 MessageMetrics(48), 让后续 hook 看到的是过滤后的消息链。
func (h *MessageFilterHook) Priority() int               { return 40 }
// Phases 仅在 PhasePreRequest 生效。
func (h *MessageFilterHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreRequest}
}

// Execute 已停用历史改写。
//
// 原先这里做两件改写历史的事, 两件都会打碎前缀缓存:
//
//  1. FilterPureToolUseUnits(msgs, 6) —— 每轮重算的滑动窗口。窗口随 len(messages)
//     前移, 一条上一轮还在的消息这轮被删, 其后所有位置的字节整体前移;
//  2. CompressMessageContent —— 对 i < len-4 的**全部**古老 tool_result 做停用词级
//     压缩, 作用范围随消息增长而扩大, 且结果不是内容的纯函数 (依赖行数、停用词
//     命中率), 同一段文本在不同轮次可能压出不同字节。
//
// 现在两者都不再作用于历史。历史的收敛统一交给 AutoCompact 的折叠边界 (一次性、
// 纯函数占位), 本轮新增消息的瘦身交给 MicroCompactHook 的水位线区间。
//
// 返回 nil 表示"不介入", HookChain 原样透传 ctx.Messages —— 不产生覆盖写, 也就
// 不会有任何意外改写。
func (h *MessageFilterHook) Execute(ctx *HookContext) (*HookResult, error) {
	return nil, nil
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

// Name 返回 hook 名。
func (h *MaxTokensRecoveryHook) Name() string                { return "max_tokens_recovery" }
// Priority 60: 在 PhaseOnStop 里跑, 位置由停止原因判定本身决定, 与请求前 hook 无竞争。
func (h *MaxTokensRecoveryHook) Priority() int               { return 60 }
// Phases 仅在 PhaseOnStop 生效。
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

// Name 返回 hook 名。
func (h *MessageMetricsHook) Name() string                { return "message_metrics" }
// Priority 48: 在 MessageFilter(40) 之后, 统计到的是已被前置 hook 处理过的消息链。
func (h *MessageMetricsHook) Priority() int               { return 48 }
// Phases 仅在 PhasePreRequest 生效。
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

// Name 返回 hook 名。
func (h *XMLToolFallbackHook) Name() string                { return "xml_tool_fallback" }
// Priority 130: 靠后执行, 让原生 tool_use 解析先跑, 回退只在原生结果为空时才补。
func (h *XMLToolFallbackHook) Priority() int               { return 130 }
// Phases 仅在 PhasePostRequest 生效。
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
