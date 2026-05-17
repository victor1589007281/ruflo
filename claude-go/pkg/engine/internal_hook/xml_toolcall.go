// xml_toolcall.go - 兼容多种第三方 LLM 输出风格的工具调用回退解析器.
//
// 背景:
//   官方 Anthropic 协议中, assistant 通过原生 content_block (type=tool_use) 表达工具调用,
//   QueryEngine 在流处理阶段直接拿到 ContentBlockToolUse.
//
//   但一些第三方 / 开源大模型 (MiniMax M2.x, Qwen, GLM 等) 没有遵循 Anthropic 流协议,
//   它们把工具调用塞进了 assistant 的文本里. 实际抓到的有两种主流方言:
//
//   方言 A (XML 风格, 同 Anthropic prompt-only):
//     [minimax:tool_call]
//       [invoke name="Read"]
//         [parameter name="file_path"]/abs/path[/parameter]
//       [/invoke]
//     [/minimax:tool_call]
//   (上面用方括号示意, 实际是 XML 尖括号.)
//
//   方言 B ([TOOL_CALL] hash 风格, MiniMax M2.x 训练语料里的 CLI 风格):
//     [TOOL_CALL]
//     {tool => "Bash", args => {
//       --command "go build ./..."
//       --description "build the project"
//     }}
//     [/TOOL_CALL]
//
//   这两种情况下 stream 里全是 text_delta, QueryEngine 拿到的 toolUseBlocks 为空,
//   于是整个 turn 被当成 "assistant 已经说完了" - 但实际上模型 "以为" 自己已经调用了工具.
//
//   本文件实现了对这两种方言的回退解析: 把文本里的工具调用拆出来, 转成原生
//   ContentBlockToolUse, 让后续阶段照常 RunToolUse, 然后把 tool_result 追加回去.
package internal_hook

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================
// 方言 A: XML / Anthropic prompt-only
// ============================================================

// 包裹标签: 形如 <prefix:tool_call>...</prefix:tool_call> 或 <tool_call>...</tool_call>.
var xmlToolCallWrapperRe = regexp.MustCompile(`(?s)<(?:[a-zA-Z][a-zA-Z0-9_-]*:)?tool_call\s*>(.*?)</(?:[a-zA-Z][a-zA-Z0-9_-]*:)?tool_call\s*>`)

// <function_calls>...</function_calls> 风格 (Anthropic 文档示例).
var xmlFunctionCallsRe = regexp.MustCompile(`(?s)<function_calls\s*>(.*?)</function_calls\s*>`)

// 单次调用: <invoke name="X"> ... </invoke> (name 也允许单引号).
var xmlInvokeRe = regexp.MustCompile(`(?s)<invoke\s+name=["']([^"']+)["']\s*>(.*?)</invoke\s*>`)

// 单个参数: <parameter name="K">V</parameter>.
var xmlParameterRe = regexp.MustCompile(`(?s)<parameter\s+name=["']([^"']+)["']\s*>(.*?)</parameter\s*>`)

// ============================================================
// 方言 B: [TOOL_CALL] hash
// ============================================================

// [TOOL_CALL]...[/TOOL_CALL] 包裹.
var bracketToolCallWrapperRe = regexp.MustCompile(`(?s)\[TOOL_CALL\](.*?)\[/TOOL_CALL\]`)

// tool => "NAME" 或 'tool' => "NAME".
var bracketToolNameRe = regexp.MustCompile(`['"]?tool['"]?\s*=>\s*["']([^"']+)["']`)

// --key "value" / --key 'value' / --key "value with \"escapes\"".
// 值部分支持反斜杠转义, 内嵌的 \" 不会提前结束.
var bracketArgRe = regexp.MustCompile(`--([a-zA-Z_][a-zA-Z0-9_]*)\s+("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')`)

// ============================================================
// 公共: 工具名 / 参数名归一化
// ============================================================

// toolNameAlias 把 LLM 误用的别名映射到实际注册的工具名.
// MiniMax / Qwen 经常按 Claude 训练语料里的命名 (Edit/Bash) 来写, 但我们
// 实际注册的是 StrReplace/Shell.
var toolNameAlias = map[string]string{
	"Edit":      "StrReplace",
	"FileEdit":  "StrReplace",
	"Bash":      "Shell",
	"FileRead":  "Read",
	"FileWrite": "Write",
}

// paramAlias 每个工具的参数名归一化映射 (LLM 名 -> 工具实际名).
// key 是 (alias 解析后的) 工具名, value 是 (LLM 名 -> 工具名) 的映射.
//
// 例如 Read 的 schema 是 {path,...}, 但 MiniMax 按 Claude 训练语料用 file_path,
// 这里就把 file_path 改回 path.
var paramAlias = map[string]map[string]string{
	"Read": {
		"file_path": "path",
	},
	"Write": {
		"file_path": "path",
		"content":   "contents",
	},
	"StrReplace": {
		"file_path": "path",
	},
	"Glob": {
		// pattern 字段名一致
	},
	"Grep": {
		// pattern 字段名一致
	},
	"Shell": {
		// command 字段名一致
	},
}

// applyParamAlias 把 params 里使用 LLM 风格 (file_path 等) 的 key 改写成实际工具名.
// 如果同一目标 key 已经存在 (即 params 同时给了 path 和 file_path), 工具实际名优先,
// LLM 别名被丢弃以避免冲突.
func applyParamAlias(toolName string, params map[string]any) map[string]any {
	alias, ok := paramAlias[toolName]
	if !ok || len(params) == 0 {
		return params
	}
	for llmKey, realKey := range alias {
		v, hit := params[llmKey]
		if !hit {
			continue
		}
		if _, exists := params[realKey]; exists {
			// 工具实际名已经被显式传了, 丢弃 alias.
			delete(params, llmKey)
			continue
		}
		params[realKey] = v
		delete(params, llmKey)
	}
	return params
}

// resolveToolName 应用工具名别名 (Edit -> StrReplace 等).
func resolveToolName(name string) string {
	if mapped, ok := toolNameAlias[name]; ok {
		return mapped
	}
	return name
}

// buildToolUseBlock 用 params 构造一个 ContentBlockToolUse.
// 如果 params 无法 marshal 成 JSON, 返回 nil.
func buildToolUseBlock(toolName string, params map[string]any, idPrefix string, idx int) *types.ContentBlock {
	actualName := resolveToolName(toolName)
	params = applyParamAlias(actualName, params)
	input, err := json.Marshal(params)
	if err != nil {
		return nil
	}
	if idPrefix == "" {
		idPrefix = "xmltool"
	}
	return &types.ContentBlock{
		Type:  types.ContentBlockToolUse,
		ID:    fmt.Sprintf("%s_%d", idPrefix, idx),
		Name:  actualName,
		Input: json.RawMessage(input),
	}
}

// ============================================================
// 解析器入口
// ============================================================

// ExtractXMLToolCalls 扫描 assistant 文本, 把内嵌的工具调用 (方言 A + B) 转成
// ContentBlockToolUse, 同时返回去掉这些工具调用之后的剩余文本.
//
// 如果没有发现任何可识别的工具调用, 返回 (nil, text).
//
// idPrefix 用于生成稳定的 tool_use ID (例如可以传 turn 序号或 message UUID 前缀).
func ExtractXMLToolCalls(text, idPrefix string) (blocks []types.ContentBlock, cleaned string) {
	if !looksLikeToolCallText(text) {
		return nil, text
	}

	cleaned = text
	var idx int

	// 先处理 [TOOL_CALL] 风格 (避免 XML 解析器干扰它).
	bracketBlocks, c1 := extractBracketToolCalls(cleaned, idPrefix, &idx)
	cleaned = c1
	blocks = append(blocks, bracketBlocks...)

	// 再处理 XML 风格.
	xmlBlocks, c2 := extractInvokeStyleToolCalls(cleaned, idPrefix, &idx)
	cleaned = c2
	blocks = append(blocks, xmlBlocks...)

	cleaned = strings.TrimSpace(cleaned)
	return blocks, cleaned
}

// looksLikeToolCallText 做粗略的关键字检测, 避免对绝大多数 "纯回答" 文本浪费正则.
func looksLikeToolCallText(s string) bool {
	if s == "" {
		return false
	}
	return strings.Contains(s, "<invoke") ||
		strings.Contains(s, "tool_call") ||
		strings.Contains(s, "function_calls") ||
		strings.Contains(s, "[TOOL_CALL]")
}

// extractInvokeStyleToolCalls 处理方言 A.
func extractInvokeStyleToolCalls(text, idPrefix string, idx *int) (blocks []types.ContentBlock, cleaned string) {
	cleaned = text
	var innerScans []string

	// 1) 先抽取所有包裹的内部内容, 并从 cleaned 中删掉这些包裹整体.
	for _, re := range []*regexp.Regexp{xmlToolCallWrapperRe, xmlFunctionCallsRe} {
		captured := re.FindAllStringSubmatch(cleaned, -1)
		for _, m := range captured {
			if len(m) >= 2 {
				innerScans = append(innerScans, m[1])
			}
		}
		cleaned = re.ReplaceAllString(cleaned, "")
	}

	// 2) cleaned 里残留的裸 invoke (例如没有包裹) 也参与解析, 然后删掉.
	innerScans = append(innerScans, cleaned)
	cleaned = xmlInvokeRe.ReplaceAllString(cleaned, "")

	for _, inner := range innerScans {
		for _, im := range xmlInvokeRe.FindAllStringSubmatch(inner, -1) {
			name := strings.TrimSpace(im[1])
			if name == "" {
				continue
			}
			body := im[2]

			params := map[string]any{}
			for _, pm := range xmlParameterRe.FindAllStringSubmatch(body, -1) {
				key := strings.TrimSpace(pm[1])
				val := pm[2]
				// 去掉一对紧贴标签的换行 (常见格式化), 保留内部空白.
				val = strings.TrimPrefix(val, "\r")
				val = strings.TrimPrefix(val, "\n")
				val = strings.TrimSuffix(val, "\r")
				val = strings.TrimSuffix(val, "\n")
				params[key] = coerceXMLValue(val)
			}

			blk := buildToolUseBlock(name, params, idPrefix+"_xml", *idx)
			if blk == nil {
				continue
			}
			blocks = append(blocks, *blk)
			*idx++
		}
	}
	return blocks, cleaned
}

// extractBracketToolCalls 处理方言 B ([TOOL_CALL] hash).
func extractBracketToolCalls(text, idPrefix string, idx *int) (blocks []types.ContentBlock, cleaned string) {
	cleaned = text
	matches := bracketToolCallWrapperRe.FindAllStringSubmatch(cleaned, -1)
	if len(matches) == 0 {
		return nil, cleaned
	}

	for _, m := range matches {
		body := m[1]
		nameM := bracketToolNameRe.FindStringSubmatch(body)
		if len(nameM) < 2 {
			continue
		}
		name := strings.TrimSpace(nameM[1])
		if name == "" {
			continue
		}

		params := map[string]any{}
		for _, am := range bracketArgRe.FindAllStringSubmatch(body, -1) {
			key := strings.TrimSpace(am[1])
			raw := am[2]
			val, ok := decodeQuotedString(raw)
			if !ok {
				continue
			}
			params[key] = coerceXMLValue(val)
		}

		blk := buildToolUseBlock(name, params, idPrefix+"_brk", *idx)
		if blk == nil {
			continue
		}
		blocks = append(blocks, *blk)
		*idx++
	}

	cleaned = bracketToolCallWrapperRe.ReplaceAllString(cleaned, "")
	return blocks, cleaned
}

// decodeQuotedString 把形如 "abc\nde\"f" 或 'abc' 的引号字符串解码成实际内容.
// 返回 (内容, true) 成功; (空, false) 失败.
func decodeQuotedString(raw string) (string, bool) {
	if len(raw) < 2 {
		return "", false
	}
	first, last := raw[0], raw[len(raw)-1]
	if first != last || (first != '"' && first != '\'') {
		return "", false
	}
	if first == '"' {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err == nil {
			return s, true
		}
		// 退化处理: 去掉首尾引号, 解开少量常见 escape (用于 JSON 拒绝的极端情况).
		inner := raw[1 : len(raw)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		inner = strings.ReplaceAll(inner, `\n`, "\n")
		inner = strings.ReplaceAll(inner, `\t`, "\t")
		return inner, true
	}
	// 单引号: 简单去引号 + 解 \' 与 \\.
	inner := raw[1 : len(raw)-1]
	inner = strings.ReplaceAll(inner, `\'`, `'`)
	inner = strings.ReplaceAll(inner, `\\`, `\`)
	return inner, true
}

// coerceXMLValue 把字符串值尝试还原成结构化值 (true/false/JSON 对象 / 数组),
// 其余保持字符串. 这样 file_path / 多行 content 这种纯文本不会被破坏,
// 但 enabled=true 之类的布尔值能正确传入工具.
func coerceXMLValue(s string) any {
	t := strings.TrimSpace(s)
	switch t {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if (strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}")) ||
		(strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]")) {
		var v any
		if err := json.Unmarshal([]byte(t), &v); err == nil {
			return v
		}
	}
	return s
}

// MergeXMLToolCalls 在 turn 结束阶段调用: 扫描 assistantBlocks 里的所有 text
// 块, 若发现工具调用则:
//   - 在文本块原地用清洗后的文本替换 (空文本块会被丢弃)
//   - 把新解出的 ContentBlockToolUse 追加进 assistantBlocks
//   - 同步追加进 toolUseBlocks
//
// 即使 ExtractXMLToolCalls 没有提取出可执行的工具调用 (例如模型只是输出了
// 残缺的 <minimax:tool_call>...</minimax:tool_call> 包裹), 只要 cleaned 文本
// 发生了变化, 也会用 cleaned 替换原文本块, 以便上层校验器不会把残留的 XML
// 噪声判定为伪工具调用.
//
// 返回更新后的 assistantBlocks 与 toolUseBlocks, 以及本次新解析出的工具调用数.
// 若 ndelta == 0 但仍有 text 被清洗, 上层调用方仍应使用返回的 assistantBlocks.
func MergeXMLToolCalls(assistantBlocks, toolUseBlocks []types.ContentBlock, idPrefix string) (newAssistant, newToolUse []types.ContentBlock, ndelta int) {
	if len(assistantBlocks) == 0 {
		return assistantBlocks, toolUseBlocks, 0
	}

	updated := make([]types.ContentBlock, 0, len(assistantBlocks))
	var collected []types.ContentBlock
	var sanitized bool

	for _, blk := range assistantBlocks {
		if blk.Type != types.ContentBlockText || blk.Text == "" {
			updated = append(updated, blk)
			continue
		}
		newCalls, cleaned := ExtractXMLToolCalls(blk.Text, fmt.Sprintf("%s_%d", idPrefix, len(updated)))
		if len(newCalls) == 0 {
			// 没提取出 tool_use, 但 cleaned 可能已经把 <minimax:tool_call> 等噪声
			// 标签剥离了; 如果发生变化, 仍用 cleaned 覆盖, 完全为空就丢弃.
			if cleaned == blk.Text {
				updated = append(updated, blk)
				continue
			}
			sanitized = true
			if strings.TrimSpace(cleaned) != "" {
				textBlk := blk
				textBlk.Text = cleaned
				updated = append(updated, textBlk)
			}
			continue
		}
		// 保留 (可能被清空) 的解释性文字; 完全为空则丢弃.
		if strings.TrimSpace(cleaned) != "" {
			textBlk := blk
			textBlk.Text = cleaned
			updated = append(updated, textBlk)
		}
		collected = append(collected, newCalls...)
	}

	if len(collected) == 0 {
		if !sanitized {
			return assistantBlocks, toolUseBlocks, 0
		}
		// 没有新的 tool_use, 但有文本被清洗过, 返回 updated 让上层使用清洁文本.
		return updated, toolUseBlocks, 0
	}

	updated = append(updated, collected...)
	toolUseBlocks = append(toolUseBlocks, collected...)
	return updated, toolUseBlocks, len(collected)
}
