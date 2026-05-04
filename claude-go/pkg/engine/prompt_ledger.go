package engine

import (
	"encoding/json"
	"strings"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// buildPromptComponentMetrics 从最终发送给模型的 system/messages/tools 中拆解提示词组件。
// 这是观测用的账本, 不改变实际请求内容。
func buildPromptComponentMetrics(systemPrompt []string, tools []types.APITool, messages []types.Message) api.PromptComponentMetrics {
	systemText := strings.Join(systemPrompt, "\n")
	messageText := messageTextForLedger(messages)

	metrics := api.PromptComponentMetrics{
		SystemChars:       len(systemText),
		ToolsSchemaChars:  jsonLen(tools),
		MCPToolsChars:     mcpToolsJSONLen(tools),
		SkillListingChars: countXMLBlocks(systemText, "available_skills"),
		RoleSkillsChars:   countXMLBlocks(systemText, "role_skills") + countXMLBlocks(messageText, "role_skills"),
		MemoryChars:       countMemoryChars(systemText),
		BlackboardChars:   countHandoffChars(messageText),
		PrevResultChars:   countPrevResultChars(systemText) + countPrevResultChars(messageText),
		MessagesChars:     messageChars(messages),
	}
	return metrics
}

func jsonLen(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(data)
}

func mcpToolsJSONLen(tools []types.APITool) int {
	total := 0
	for _, t := range tools {
		if strings.HasPrefix(t.Name, "mcp_") {
			total += jsonLen(t)
		}
	}
	return total
}

func messageChars(messages []types.Message) int {
	total := 0
	for _, msg := range messages {
		for _, block := range msg.Content {
			total += len(block.Text)
			total += len(block.Content)
			total += len(block.Thinking)
			total += len(block.Input)
		}
	}
	return total
}

func messageTextForLedger(messages []types.Message) string {
	var sb strings.Builder
	for _, msg := range messages {
		for _, block := range msg.Content {
			if block.Text != "" {
				sb.WriteString(block.Text)
				sb.WriteString("\n")
			}
			if block.Content != "" {
				sb.WriteString(block.Content)
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

func countXMLBlocks(text, tag string) int {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	total := 0
	search := text
	for {
		start := strings.Index(search, open)
		if start < 0 {
			return total
		}
		rest := search[start:]
		end := strings.Index(rest, close)
		if end < 0 {
			total += len(rest)
			return total
		}
		end += len(close)
		total += end
		search = rest[end:]
	}
}

func countMemoryChars(systemText string) int {
	total := 0
	total += countXMLBlocks(systemText, "relevant_memories")
	total += countXMLBlocks(systemText, "structured_memories")
	total += countXMLBlocks(systemText, "long_term_memory")
	if idx := strings.Index(systemText, "Codebase and user instructions are shown below."); idx >= 0 {
		end := len(systemText)
		for _, marker := range []string{"<long_term_memory>", "<relevant_memories>", "<structured_memories>"} {
			if next := strings.Index(systemText[idx+1:], marker); next >= 0 && idx+1+next < end {
				end = idx + 1 + next
			}
		}
		total += end - idx
	}
	return total
}

func countHandoffChars(messageText string) int {
	total := 0
	total += countSections(messageText, "## Handoff Context", "\n\n---\n\n")
	total += countSections(messageText, "### 用户近期对话上下文", "\n\n---\n\n")
	return total
}

func countPrevResultChars(text string) int {
	total := 0
	total += countSections(text, "### Output from ", "\n\n### Output from ")
	total += countSections(text, "实现产出:", "\n\n##")
	total += countSections(text, "架构设计文档:", "\n\n##")
	total += countSections(text, "技术调研/上游输入:", "\n\n##")
	return total
}

func countSections(text, marker, nextMarker string) int {
	total := 0
	search := text
	for {
		start := strings.Index(search, marker)
		if start < 0 {
			return total
		}
		rest := search[start:]
		end := strings.Index(rest[len(marker):], nextMarker)
		if end < 0 {
			total += len(rest)
			return total
		}
		end += len(marker)
		total += end
		search = rest[end:]
	}
}
