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

// jsonLen 返回 v 序列化后的字节数; 序列化失败记 0 (账本按"量不出就是 0"处理, 不中断观测)。
func jsonLen(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(data)
}

// mcpToolsJSONLen 统计名字以 mcp_ 开头的工具 schema 总字节数 (MCP 接入的工具单列一格)。
func mcpToolsJSONLen(tools []types.APITool) int {
	total := 0
	for _, t := range tools {
		if strings.HasPrefix(t.Name, "mcp_") {
			total += jsonLen(t)
		}
	}
	return total
}

// MessageChars 累加 messages 里各内容块 (text/content/thinking/input) 的字节数。
//
// 注意: 这里量的是**原始字段字节**, 不含 JSON 包装与工具调用的 name/id —— 与 ToolsSchemaChars
// 的口径不同, 两个数不可直接相加当"总 prompt 大小"。
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

// messageTextForLedger 把 messages 拼成一段纯文本, 供按标签计数的辅助函数扫描。
//
// 注意: 只取 text 与 content 两类块, thinking/input 不计入 —— 账本要量的是"发给模型的可见文本"
// 里塞了多少记忆/交接内容, 思维链与工具入参不是这些标签的载体。
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

// countXMLBlocks 统计 text 中 <tag>...</tag> 成对标签所占的字符总数 (含标签本身)。
//
// 算法解读：
//
//	原理：账本要把"这段 system prompt 里有多少字符是技能清单/记忆块"量出来。做法是反复
//	      找下一个开标签, 再从那里找到最近的闭标签, 把整段长度计入, 然后从闭标签之后继续。
//	关键不变量：每轮都必须**越过**闭标签再继续 (search = rest[end:]), 否则下一次 Index 会
//	      在同一个开标签上命中, 死循环。这个不变量由 end += len(close) 保证。
//	代价与边界：纯字符串扫描, O(文本长度)。闭标签缺失时**把剩余全部计入**而不是报错或记 0 ——
//	      截断的 prompt 本身就是异常, 把它的体量记满比记 0 更容易在观测里暴露问题。
//	设计权衡：不做真正的 XML 解析 —— 这段文本是拼出来的 (可能被截断、可能嵌套不合法),
//	      严格解析会在这些情况下抛错, 而账本是观测路径, 必须永不失败。
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

// countMemoryChars 统计 system prompt 里各类记忆块所占字符数。
//
// 除了三种 XML 记忆标签, 还单独量"Codebase and user instructions are shown below."这句引导语
// 到其后第一个记忆标签之间的那一段。
//
// 注意: 若这段文本里**根本没有**后续记忆标签, 上面那句的 end 会保持 len(systemText),
// 于是从引导语到 system prompt 末尾的全部内容都会被计成 MemoryChars —— 明显高估。
// 依赖此数的告警阈值需要容忍这种过计。
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

// countHandoffChars 统计消息里"交接上下文"段的字符数 (黑板的两个已知小节标题)。
func countHandoffChars(messageText string) int {
	total := 0
	total += countSections(messageText, "## Handoff Context", "\n\n---\n\n")
	total += countSections(messageText, "### 用户近期对话上下文", "\n\n---\n\n")
	return total
}

// countPrevResultChars 统计上一阶段产出/文档类小节占用的字符数 (四类已知标题)。
func countPrevResultChars(text string) int {
	total := 0
	total += countSections(text, "### Output from ", "\n\n### Output from ")
	total += countSections(text, "实现产出:", "\n\n##")
	total += countSections(text, "架构设计文档:", "\n\n##")
	total += countSections(text, "技术调研/上游输入:", "\n\n##")
	return total
}

// countSections 统计以 marker 起、到下一个 nextMarker 为止的各小节所占字符数 (含两端标记)。
//
// 算法解读：
//
//	原理：与 countXMLBlocks 同形, 只是"结束"从闭标签换成了**下一个同级小节的开头**——
//	      这类小节 (Output from / Handoff Context / 实现产出) 没有闭合标签, 边界只能靠下一个
//	      同级标题确定, 所以 nextMarker 是调用方按各标题的排布分别传进来的。
//	关键不变量：每轮必须越过本次边界再继续搜索 (search = rest[end:]), 且 end 的算法是
//	      "marker 长度 + nextMarker 在 rest[len(marker):] 中的相对位置" —— 少加 len(marker)
//	      会让搜索窗口原地打转, 直接死循环。
//	代价与边界：末节没有 nextMarker 时把剩余全部计入 (记满而非记 0), 与 countXMLBlocks 的
//	      降级方向一致; 同样要求标题串在文本中不重叠出现。
//	设计权衡：不合并进 countXMLBlocks 复用同一份代码 —— 两者的结束判据 (闭标签 vs 下一个标题)
//	      与计数边界都不同, 强行统一只会让两处语义互相污染。
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
