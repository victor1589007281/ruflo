// Package textutil —— UTF-8 安全的字符串截断。
//
// 为什么需要它: Go 的 s[:n] 切的是**字节**。中文一个字 3 字节, 切在中间就
// 得到一个非法 UTF-8 序列 —— 它不 panic, 只是安静地把一个字劈成半个。
// 后果分三层:
//
//  1. 展示层出现乱码 (最常见, 也最容易当成"偶发");
//  2. 下游若再按 rune 处理这段字符串, 半个字符会被替换成 U+FFFD ——
//     原文静默少字, 且长度对不上, 排查时看不出是谁吃掉的;
//  3. 出站 JSON 序列化会把非法字节统一替换成 U+FFFD, 于是**同一段逻辑内容
//     在不同轮次可能渲染出不同字节**, 前缀缓存在此断裂。
//
// 第 3 条是把它当回事的原因: 截断点本身是确定的, 但截出来的字节序列是否合法
// 取决于内容 —— 中文内容里 s[:n] 几乎必然劈字, 于是同一段历史在"被截断"和
// "未被截断"两种形态间切换时, 字节序列不稳定。
//
// 两个函数对应两种既有意图, 不要混用:
//   - Truncate      长度语义 = 字符数 (常量名多为 maxChars / maxLen)
//   - TruncateBytes 长度语义 = 字节预算 (常量名多为 maxBytes, 或限制来自
//     HTTP body / context 预算这类真实的字节约束)
//
// 二者都保证返回到 rune 边界为止, 区别只在"上限怎么算"。
package textutil

import "unicode/utf8"

// Truncate 把 s 截断到最多 maxRunes 个字符, 超出时追加 suffix。
//
// maxRunes <= 0 或 s 本身不超长时原样返回 (suffix 也不追加) —— 调用方普遍用
// 它做"短字符串直接放行", 这里多贴一个 "..." 会让短字符串凭空变长。
func Truncate(s string, maxRunes int, suffix string) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + suffix
}

// TruncateBytes 把 s 截到不超过 maxBytes 字节, 超出时追加 suffix。
//
// 与 Truncate 的差别: 上限按字节算, 因此结果的长度可能比 maxBytes 略短
// (回退到最近的 rune 边界)。需要"卡住真实字节预算"时用这个 —— 比如
// tool_result 的体积上限直接决定出站请求体大小, 按字符数卡会让中文内容
// 实际占用 3 倍预算。
func TruncateBytes(s string, maxBytes int, suffix string) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	// 回退到 rune 起始字节。cut 是**字节**下标, 用它索引是安全的:
	// 循环条件先保证 cut > 0, 且 cut 永远不会越过 len(s) (因为 len(s) > maxBytes >= cut)。
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// TruncateMiddle 保留首尾、砍掉中间。用于"两头才是有用信息"的场景
// (如超长日志: 开头是命令、结尾是错误)。
func TruncateMiddle(s string, maxRunes int, marker string) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	keep := maxRunes - utf8.RuneCountInString(marker)
	if keep < 2 {
		return Truncate(s, maxRunes, marker)
	}
	head := keep / 2
	tail := keep - head
	return string(r[:head]) + marker + string(r[len(r)-tail:])
}
