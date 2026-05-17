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
