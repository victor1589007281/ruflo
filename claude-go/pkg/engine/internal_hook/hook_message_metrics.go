// hook_message_metrics.go — 消息链指标采集内置 Hook。
//
// 设计目标：在 PhasePreRequest 阶段对 messages 做深度分析，记录消息链长度、
// token 估算、tool_result 分布、压缩/过滤效果等关键指标，为后续 MemGPT /
// 上下文优化提供数据支撑。纯观测型 hook，不修改 messages。
//
// 依赖: EngineMetrics（G10），通过原子操作 lock-free 写入。
package internal_hook

import (
	"fmt"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

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
