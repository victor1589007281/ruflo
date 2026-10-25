// relevance.go —— 兜底源的词面相关性过滤。
//
// 背景: Bing RSS 在长英文/中文查询上经常回一批完全无关的结果 (实测同一批
// 8 条里有 Yandex 首页、花都影视、虎牙直播)。它们不是"低质量", 是**另一个
// 话题**; 进了融合会污染质量门, 进了证据表会变成编造引用的入口。
//
// 这里只做保守的词面过滤, 不试图做语义相关性:
//   - 查询没有内容词 (纯数字/符号) 时不过滤;
//   - 结果必须命中至少一个内容词; 内容词 >=4 时要求覆盖 25%;
//   - 数字 (年份、版本号) 不算内容词 —— 否则 "2026" 会让所有新闻都过关.
//
// 过滤只在 bing_rss 这类"最后兜底"源上启用。主源 (exa/parallel) 的相关性由
// 排序与 snippet 负责, 在这里统一过滤会把语义匹配但用词不同的结果误删。
package search

import (
	"strings"
	"unicode"
)

// FilterRelevant 丢弃与查询没有任何内容词交集的结果。
//
// 返回新切片; 输入为空或查询没有内容词时原样返回 (同一个底层数组)。
func FilterRelevant(results []Result, query string) []Result {
	tokens := contentTokens(query)
	if len(tokens) == 0 || len(results) == 0 {
		return results
	}
	need := 1
	if len(tokens) >= 4 {
		// ceil(25%): 4 词 -> 1, 8 词 -> 2, 12 词 -> 3.
		need = (len(tokens) + 3) / 4
	}

	kept := make([]Result, 0, len(results))
	for _, r := range results {
		hay := strings.ToLower(r.Title + " " + r.Snippet + " " + r.URL)
		matched := 0
		for t := range tokens {
			if strings.Contains(hay, t) {
				matched++
			}
		}
		if matched >= need {
			kept = append(kept, r)
		}
	}
	return kept
}

// contentTokens 抽取查询里的内容词。
//
// 拉丁/数字串按非字母数字切词; 纯数字丢弃 (年份/版本号不构成话题)。
// 连续汉字生成 bigram (中文没有空格, 整段当一个词会让 "贵州茅台2026年半年报"
// 变成一个永远匹配不上的超长 token); 单个汉字保留。
func contentTokens(query string) map[string]struct{} {
	tokens := make(map[string]struct{})
	var word []rune
	var han []rune

	flushWord := func() {
		if len(word) == 0 {
			return
		}
		w := strings.ToLower(string(word))
		word = word[:0]
		if _, stop := stopwords[w]; len(w) < 2 || isAllDigits(w) || stop {
			return
		}
		tokens[w] = struct{}{}
	}
	flushHan := func() {
		if len(han) == 0 {
			return
		}
		if len(han) == 1 {
			if _, stop := stopwords[string(han)]; !stop {
				tokens[string(han)] = struct{}{}
			}
		} else {
			for i := 0; i+1 < len(han); i++ {
				bg := string(han[i : i+2])
				if _, stop := stopwords[bg]; !stop {
					tokens[bg] = struct{}{}
				}
			}
		}
		han = han[:0]
	}

	for _, r := range query {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r), unicode.IsDigit(r):
			flushHan()
			word = append(word, r)
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return tokens
}

// isAllDigits 判断字符串是否全为 ASCII 数字。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// stopwords 只收"不指向任何话题"的高频词。
//
// 收得越多越容易误删 (例如 "release" 在版本调研里是内容词), 所以宁少勿多:
// 这里的目的只是避免 the/的/最新 这类词让无关结果过关。
var stopwords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "and": {}, "or": {}, "of": {}, "for": {},
	"to": {}, "in": {}, "on": {}, "with": {}, "from": {}, "by": {}, "at": {},
	"as": {}, "is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {},
	"it": {}, "its": {}, "this": {}, "that": {}, "these": {}, "those": {},
	"what": {}, "how": {}, "which": {}, "who": {}, "when": {}, "where": {},
	"latest": {}, "new": {}, "news": {}, "update": {}, "updates": {},
	"的": {}, "了": {}, "与": {}, "和": {}, "及": {}, "在": {}, "是": {},
	"有": {}, "对": {}, "为": {}, "从": {}, "到": {}, "中": {},
	"最新": {}, "什么": {}, "如何": {}, "哪些": {}, "怎么": {},
}
