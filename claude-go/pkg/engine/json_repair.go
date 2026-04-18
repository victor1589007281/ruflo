// json_repair.go — 工具输入 JSON 修复 (G5)。
//
// 对标:
//   - Claude 4.6 Grammar Constrained Decoding: 模型侧语法受限
//   - 社区 json-repair / jsonrepair.js: 本地修复尾逗号/缺引号/截断
//   - Qwen 3.6 JSONParser 双层校验: wire-schema + app-schema
//
// 当 LLM 流式输出 tool_use input 时, 少数情况会产生:
//   - 尾随逗号 `{"a":1,}`
//   - 截断 `{"a":"val`
//   - 单引号 `{'a': 'b'}`
//   - markdown 包裹 ```json {...} ```
//
// 当前 QueryEngine 直接 `json.RawMessage(currentToolInput.String())`, 不校验也不修复,
// 把问题推给具体工具的 UnmarshalInput, 造成可恢复错误被误报为工具失败。
//
// 本组件提供一个短 fallback chain:
//   valid? → strip markdown → fix trailing commas → close brackets → swap single→double quotes → fail
package engine

import (
	"encoding/json"
	"strings"
	"unicode"
)

// JSONRepair 工具输入 JSON 修复器。
// 字段预留给后续可配置 (如严格模式, 允许的修复策略子集)。
type JSONRepair struct {
	EnableMarkdownStrip   bool
	EnableTrailingComma   bool
	EnableBracketClose    bool
	EnableSingleQuoteSwap bool
}

// NewJSONRepair 构造启用全部策略的修复器。
func NewJSONRepair() *JSONRepair {
	return &JSONRepair{
		EnableMarkdownStrip:   true,
		EnableTrailingComma:   true,
		EnableBracketClose:    true,
		EnableSingleQuoteSwap: true,
	}
}

// RepairResult 描述一次修复的结果。
type RepairResult struct {
	OK       bool
	Method   string // "direct" | "strip_markdown" | "trailing_comma" | "bracket_close" | "quote_swap" | "combined"
	Original string
	Repaired string
	Changed  bool
}

// Try 对输入做 fallback 尝试, 返回合法 JSON 字节和修复结果。
// 失败时 out 保持与 input 相同 (方便上层直接 fallthrough)。
func (r *JSONRepair) Try(input []byte) (out []byte, res RepairResult) {
	if r == nil {
		r = NewJSONRepair()
	}
	res.Original = string(input)

	if len(input) == 0 {
		// 空输入视为合法 (工具可按需校验)
		res.OK = true
		res.Method = "direct"
		res.Repaired = ""
		return input, res
	}

	if json.Valid(input) {
		res.OK = true
		res.Method = "direct"
		res.Repaired = res.Original
		return input, res
	}

	candidate := string(input)
	method := "combined"

	if r.EnableMarkdownStrip {
		stripped := stripMarkdownFence(candidate)
		if stripped != candidate {
			candidate = stripped
			method = "strip_markdown"
			if json.Valid([]byte(candidate)) {
				res.OK = true
				res.Method = method
				res.Repaired = candidate
				res.Changed = true
				return []byte(candidate), res
			}
		}
	}

	if r.EnableTrailingComma {
		fixed := removeTrailingCommas(candidate)
		if fixed != candidate {
			candidate = fixed
			method = "trailing_comma"
			if json.Valid([]byte(candidate)) {
				res.OK = true
				res.Method = method
				res.Repaired = candidate
				res.Changed = true
				return []byte(candidate), res
			}
		}
	}

	if r.EnableSingleQuoteSwap {
		swapped := swapSingleQuotes(candidate)
		if swapped != candidate {
			candidate = swapped
			method = "quote_swap"
			if json.Valid([]byte(candidate)) {
				res.OK = true
				res.Method = method
				res.Repaired = candidate
				res.Changed = true
				return []byte(candidate), res
			}
		}
	}

	if r.EnableBracketClose {
		closed := closeUnbalancedBrackets(candidate)
		if closed != candidate {
			candidate = closed
			method = "bracket_close"
			if json.Valid([]byte(candidate)) {
				res.OK = true
				res.Method = method
				res.Repaired = candidate
				res.Changed = true
				return []byte(candidate), res
			}
		}
	}

	// 组合尝试 (上面所有步骤都失败时, 再跑一次累积版)
	combined := candidate
	if r.EnableTrailingComma {
		combined = removeTrailingCommas(combined)
	}
	if r.EnableBracketClose {
		combined = closeUnbalancedBrackets(combined)
	}
	if json.Valid([]byte(combined)) {
		res.OK = true
		res.Method = "combined"
		res.Repaired = combined
		res.Changed = true
		return []byte(combined), res
	}

	res.OK = false
	res.Method = method
	res.Repaired = candidate
	res.Changed = candidate != res.Original
	return input, res
}

// stripMarkdownFence 剥离 ```json ... ``` 包裹。
func stripMarkdownFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	// 跳过首行 fence
	nl := strings.IndexByte(t, '\n')
	if nl < 0 {
		return s
	}
	inner := t[nl+1:]
	if end := strings.LastIndex(inner, "```"); end >= 0 {
		inner = inner[:end]
	}
	return strings.TrimSpace(inner)
}

// removeTrailingCommas 移除对象/数组内的尾随逗号。
// 简易状态机: 跟踪是否在字符串内, 不在时遇到 `,` 后跟 `}`/`]` 则删除逗号。
func removeTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	var prev rune
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if inStr {
			b.WriteRune(r)
			if r == '"' && prev != '\\' {
				inStr = false
			}
			prev = r
			continue
		}
		if r == '"' {
			inStr = true
			b.WriteRune(r)
			prev = r
			continue
		}
		if r == ',' {
			// lookahead: skip whitespace, 看是否为 } 或 ]
			j := i + 1
			for j < len(runes) && unicode.IsSpace(runes[j]) {
				j++
			}
			if j < len(runes) && (runes[j] == '}' || runes[j] == ']') {
				// 丢弃这个逗号
				prev = r
				continue
			}
		}
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}

// closeUnbalancedBrackets 在字符串外尾部补齐缺失的 } 或 ]。
// 会一并关闭未闭合的字符串 (简单地在末尾补 `"`)。
func closeUnbalancedBrackets(s string) string {
	inStr := false
	var prev rune
	stack := make([]rune, 0, 8)
	for _, r := range s {
		if inStr {
			if r == '"' && prev != '\\' {
				inStr = false
			}
			prev = r
			continue
		}
		switch r {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 && stack[len(stack)-1] == r {
				stack = stack[:len(stack)-1]
			}
		}
		prev = r
	}
	var b strings.Builder
	b.WriteString(s)
	if inStr {
		b.WriteByte('"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteRune(stack[i])
	}
	return b.String()
}

// swapSingleQuotes 把字符串外的 `'` 替换成 `"`。
// 简易状态机, 不处理嵌套转义的边界情况 (失败时下一轮还会尝试 bracket_close)。
func swapSingleQuotes(s string) string {
	if !strings.ContainsRune(s, '\'') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inDouble := false
	var prev rune
	for _, r := range s {
		if r == '"' && prev != '\\' {
			inDouble = !inDouble
			b.WriteRune(r)
			prev = r
			continue
		}
		if !inDouble && r == '\'' {
			b.WriteRune('"')
			prev = r
			continue
		}
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}
