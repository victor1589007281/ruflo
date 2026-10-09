package internal_hook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// BuildPromptComponentMetrics 从最终发送给模型的 system/messages/tools 中拆解提示词组件。
// 这是观测用的账本, 不改变实际请求内容。
func BuildPromptComponentMetrics(systemPrompt []string, tools []types.APITool, messages []types.Message) api.PromptComponentMetrics {
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
		MessagesChars:     MessageChars(messages),
		// 工具段摘要 (D4.2): 长度量不出"换了同样大的三件工具", 见那个字段的说明。
		// 传的是**最终下发**的那份 tools —— 与 ToolsSchemaChars 同源, 两者对不上
		// 就说明这里量错了对象。
		ToolsSchemaDigest: ToolsSurfaceDigest(tools),
	}
	return metrics
}

// ToolsSurfaceDigest 算工具段的顺序敏感摘要。
//
// 顺序敏感是**必须的**: 前缀缓存看的是字节序列, 把同样一组工具换个顺序下发,
// 内容集合没变而前缀变了 —— 摘要若与顺序无关, 就会把这一次真实的缓存失效
// 报成"没变"。所以这里按切片顺序逐条喂进哈希。
//
// 字段之间用 \x00 分隔 (而不是直接拼接): 否则 description 末尾多一个字符、
// 下一个 name 少一个字符的两次请求会算出同一个摘要。
func ToolsSurfaceDigest(tools []types.APITool) string {
	if len(tools) == 0 {
		return ""
	}
	h := sha256.New()
	for _, t := range tools {
		h.Write([]byte(t.Name))
		h.Write([]byte{0})
		h.Write([]byte(t.Description))
		h.Write([]byte{0})
		h.Write(t.InputSchema)
		h.Write([]byte{0, 1}) // 条与条之间的另一个分隔符, 防止跨条拼接歧义
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
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

func MessageChars(messages []types.Message) int {
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
