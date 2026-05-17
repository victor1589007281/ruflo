// message_filter.go — 消息过滤与轻量级压缩辅助函数。
package internal_hook

import (
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// FilterPureToolUseUnits 删除不含 text/thinking 的古老 assistant 消息及其对应的
// tool_result，以"assistant + 后续纯 tool_result user 消息"为原子单元整体保留/删除。
// keepRecent 表示至少保留最近多少个 assistant 原子单元。
func FilterPureToolUseUnits(messages []types.Message, keepRecent int) []types.Message {
	if len(messages) <= keepRecent*2 {
		return messages
	}

	// 收集所有 assistant 消息的索引（从旧到新）
	var assistantIdx []int
	for i, m := range messages {
		if m.Type == types.MessageTypeAssistant {
			assistantIdx = append(assistantIdx, i)
		}
	}

	if len(assistantIdx) <= keepRecent {
		return messages
	}

	// 标记需要删除的索引
	remove := make(map[int]bool)
	for i, ai := range assistantIdx {
		// 保留最近的 keepRecent 个 assistant
		if i >= len(assistantIdx)-keepRecent {
			break
		}
		// 检查 assistant 是否包含 reasoning (text/thinking)
		if hasReasoningContent(messages[ai]) {
			continue
		}
		// 纯 tool_use: 标记 assistant 及其后续纯 tool_result user 消息
		remove[ai] = true
		for j := ai + 1; j < len(messages); j++ {
			if messages[j].Type == types.MessageTypeAssistant {
				break
			}
			if isPureToolResultMessage(messages[j]) {
				remove[j] = true
			}
		}
	}

	if len(remove) == 0 {
		return messages
	}

	var out []types.Message
	for i, m := range messages {
		if !remove[i] {
			out = append(out, m)
		}
	}
	return out
}

// hasReasoningContent 检查消息是否包含 text 或 thinking 块。
func hasReasoningContent(msg types.Message) bool {
	for _, b := range msg.Content {
		if b.Type == types.ContentBlockText || b.Type == types.ContentBlockThinking {
			return true
		}
	}
	return false
}

// isPureToolResultMessage 检查消息是否只包含 tool_result 块。
func isPureToolResultMessage(msg types.Message) bool {
	if msg.Type != types.MessageTypeUser {
		return false
	}
	if len(msg.Content) == 0 {
		return false
	}
	for _, b := range msg.Content {
		if b.Type != types.ContentBlockToolResult {
			return false
		}
	}
	return true
}

// CompressMessageContent 对消息内容做轻量级压缩（LLMlingua 简化版）。
// 主要作用于古老的 tool_result，移除冗余空白、停用词，压缩代码注释。
func CompressMessageContent(msg types.Message) types.Message {
	if msg.Type != types.MessageTypeUser || len(msg.Content) == 0 {
		return msg
	}

	var changed bool
	newBlocks := make([]types.ContentBlock, len(msg.Content))
	copy(newBlocks, msg.Content)

	for i, b := range newBlocks {
		if b.Type != types.ContentBlockToolResult || b.Content == "" {
			continue
		}
		compressed := compressText(b.Content)
		if compressed != b.Content {
			newBlocks[i].Content = compressed
			changed = true
		}
	}

	if !changed {
		return msg
	}
	msg.Content = newBlocks
	return msg
}

// stopWords 为轻量级压缩使用的常见停用词集合。
var stopWords = map[string]struct{}{
	"the": {}, "is": {}, "are": {}, "was": {}, "were": {},
	"a": {}, "an": {}, "and": {}, "or": {}, "but": {},
	"in": {}, "on": {}, "at": {}, "to": {}, "for": {},
	"of": {}, "with": {}, "by": {}, "from": {}, "as": {},
	"it": {}, "this": {}, "that": {}, "these": {}, "those": {},
}

// compressText 对文本做轻量级压缩：移除多余空白、压缩常见停用词、简化代码注释。
func compressText(s string) string {
	if len(s) < 200 {
		return s
	}

	lines := strings.Split(s, "\n")
	var sb strings.Builder
	inCodeBlock := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// 代码块边界检测
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			sb.WriteString(line)
			sb.WriteByte('\n')
			continue
		}

		if inCodeBlock {
			// 代码块内：保留语法，压缩注释
			if strings.HasPrefix(trimmed, "// ") && len(trimmed) > 50 {
				// 保留短注释，压缩长注释
				sb.WriteString(line)
				sb.WriteByte('\n')
				continue
			}
			sb.WriteString(line)
			sb.WriteByte('\n')
			continue
		}

		// 自然语言部分：移除停用词、压缩空白
		if trimmed == "" {
			continue // 删除空行
		}
		fields := strings.Fields(trimmed)
		var kept []string
		for _, w := range fields {
			lw := strings.ToLower(strings.TrimRight(w, ",.!?;:"))
			if _, ok := stopWords[lw]; ok && len(kept) > 0 {
				continue
			}
			kept = append(kept, w)
		}
		if len(kept) > 0 {
			sb.WriteString(strings.Join(kept, " "))
			sb.WriteByte('\n')
		}
	}

	result := sb.String()
	if len(result) < len(s)*7/10 {
		return result
	}
	return s
}
