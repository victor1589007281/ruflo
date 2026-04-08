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

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// TokenThreshold 触发自动压缩的 token 阈值比例 (当前 context 占比)
const TokenThreshold = 0.8

// CompactMaxOutputTokens 压缩请求的最大输出 token
const CompactMaxOutputTokens = 8192

// Compactor 上下文压缩器
type Compactor struct {
	apiClient     *api.Client
	maxContextTokens int // 模型最大上下文窗口
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
func (c *Compactor) runCompaction(ctx context.Context, messages []types.Message, model string) ([]types.Message, error) {
	// 保护尾部 (最近 4 条消息不被压缩)
	tailCount := 4
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
