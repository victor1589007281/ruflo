package search

// evidence.go —— 逐字证据账本与抽取式答案。
//
// 不调用模型: 从已抓取文档的 Markdown 里按关键词挑一句原文, 用 strings.Index
// 回查确认它确实存在于正文, 才允许入库。Answer 只拼接这些已验证的句子, 因此
// Research 的产出不包含任何新生成的事实。
import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxEvidenceQuoteRunes 是单条证据引用的上限, 防止把整段长文塞进答案。
const maxEvidenceQuoteRunes = 500

// EvidenceLedger 是逐字证据账本, 对应设计文档 §7。
//
// 只有能在 Document.Markdown 中用 strings.Index 找到的片段才会入库;
// 校验失败直接丢弃, 不改写、不补全, 保证答案里的每句话都能回到原文。
type EvidenceLedger struct {
	items []Evidence
	seen  map[string]struct{}
	next  int
}

// NewEvidenceLedger 创建空账本。
func NewEvidenceLedger() *EvidenceLedger {
	return &EvidenceLedger{seen: make(map[string]struct{}), next: 1}
}

// Add 校验并加入一条逐字证据。
//
// quote 必须非空且是 doc.Markdown 的连续子串; 成功时 Offset 指向首次
// 出现位置（以 doc.Offset 为起点的 Unicode rune 索引）, Hash 取文档规范化正文哈希, FetchedAt 取文档抓取时间。
func (l *EvidenceLedger) Add(doc *Document, quote string) (Evidence, bool) {
	if l == nil || doc == nil {
		return Evidence{}, false
	}
	quote = strings.TrimSpace(quote)
	if quote == "" {
		return Evidence{}, false
	}
	byteOffset := strings.Index(doc.Markdown, quote)
	if byteOffset < 0 {
		return Evidence{}, false
	}
	offset := doc.Offset + utf8.RuneCountInString(doc.Markdown[:byteOffset])
	key := doc.URL + "\x00" + quote
	if _, ok := l.seen[key]; ok {
		return Evidence{}, false
	}
	l.seen[key] = struct{}{}

	hash := doc.Hash
	if hash == "" {
		hash = hashDocument(doc.Markdown)
	}
	ev := Evidence{
		ID:        "E" + itoa(l.next),
		URL:       doc.URL,
		Quote:     quote,
		Offset:    offset,
		Hash:      hash,
		FetchedAt: doc.FetchedAt,
	}
	l.next++
	l.items = append(l.items, ev)
	return ev, true
}

// AddBest 从文档中挑选与关键词最相关的一句话加入账本。
//
// 选择规则: 完整句子中命中关键词最多者; 没有命中时返回 false,
// 不用无关句子凑数。quote 截断只发生在句子内部, 因此仍是原文子串。
func (l *EvidenceLedger) AddBest(doc *Document, keywords []string) (Evidence, bool) {
	if doc == nil || strings.TrimSpace(doc.Markdown) == "" {
		return Evidence{}, false
	}
	quote := bestSentence(doc.Markdown, keywords)
	if quote == "" {
		return Evidence{}, false
	}
	return l.Add(doc, quote)
}

// Items 返回证据副本。
func (l *EvidenceLedger) Items() []Evidence {
	if l == nil {
		return nil
	}
	return append([]Evidence(nil), l.items...)
}

// Len 返回证据条数。
func (l *EvidenceLedger) Len() int {
	if l == nil {
		return 0
	}
	return len(l.items)
}

// bestSentence 返回命中关键词最多的完整句子; 无命中返回空串。
func bestSentence(text string, keywords []string) string {
	quotes := bestSentences(text, keywords, 1)
	if len(quotes) == 0 {
		return ""
	}
	return quotes[0]
}

// bestSentences 返回最多 limit 条高分句子。评分时保持原文顺序作为平局依据，
// 选出后再恢复文档顺序，让 Evidence ID 与 Offset 稳定且易于模型核对。
func bestSentences(text string, keywords []string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	type candidate struct {
		span sentenceSpan
		hits int
	}
	var candidates []candidate
	seen := make(map[string]struct{})
	for _, span := range sentenceSpans(text) {
		sentence := text[span.start:span.end]
		if _, duplicate := seen[sentence]; duplicate {
			continue
		}
		seen[sentence] = struct{}{}
		hits := keywordHits(sentence, keywords)
		if hits > 0 {
			candidates = append(candidates, candidate{span: span, hits: hits})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].hits > candidates[j].hits
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].span.start < candidates[j].span.start
	})
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		quote := text[candidate.span.start:candidate.span.end]
		if utf8.RuneCountInString(quote) > maxEvidenceQuoteRunes {
			quote, _ = truncateRunes(quote, maxEvidenceQuoteRunes)
		}
		if quote = strings.TrimSpace(quote); quote != "" {
			out = append(out, quote)
		}
	}
	return out
}

// keywordHits 统计句子命中的不同关键词数量。
func keywordHits(sentence string, keywords []string) int {
	lower := strings.ToLower(sentence)
	hits := 0
	for _, kw := range keywords {
		if kw != "" && strings.Contains(lower, kw) {
			hits++
		}
	}
	return hits
}

// extractKeywords 从问题中抽取用于句子匹配的关键词。
//
// 英文按字母/数字切 token (保留长度 >= 2); 中文连续段额外拆成 2 字
// shingle, 这样"苹果公司"能匹配到"苹果"或"公司"所在的句子。
func extractKeywords(question string) []string {
	question = strings.ToLower(strings.TrimSpace(question))
	if question == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if utf8.RuneCountInString(s) < 2 {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	var token []rune
	flush := func() {
		if len(token) == 0 {
			return
		}
		s := string(token)
		add(s)
		// 连续汉字再拆 bigram。
		runes := []rune(s)
		if len(runes) >= 2 && allHan(runes) {
			for i := 0; i+1 < len(runes); i++ {
				add(string(runes[i : i+2]))
			}
		}
		token = token[:0]
	}

	for _, r := range question {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			token = append(token, r)
		} else {
			flush()
		}
	}
	flush()

	if len(out) == 0 {
		add(question)
	}
	if len(out) == 0 {
		out = []string{question}
	}
	return out
}

// allHan 判断 rune 片段是否全为汉字。
func allHan(runes []rune) bool {
	for _, r := range runes {
		if !unicode.Is(unicode.Han, r) {
			return false
		}
	}
	return true
}

// sentenceSpan 是句子在原文中的字节区间, 右端不含。
type sentenceSpan struct {
	start int
	end   int
}

// sentenceSpans 按句末标点与换行切句, 并去掉首尾空白。
func sentenceSpans(text string) []sentenceSpan {
	var out []sentenceSpan
	start := -1
	for i, r := range text {
		if start < 0 {
			if unicode.IsSpace(r) {
				continue
			}
			start = i
		}
		if isSentenceEnd(r) {
			out = append(out, trimSpan(text, start, i+utf8.RuneLen(r)))
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, trimSpan(text, start, len(text)))
	}
	return out
}

// trimSpan 把区间两端空白去掉; 全空白时返回空区间。
func trimSpan(text string, start, end int) sentenceSpan {
	for start < end {
		r, size := utf8.DecodeRuneInString(text[start:end])
		if !unicode.IsSpace(r) {
			break
		}
		start += size
	}
	for end > start {
		r, size := utf8.DecodeLastRuneInString(text[start:end])
		if !unicode.IsSpace(r) {
			break
		}
		end -= size
	}
	return sentenceSpan{start: start, end: end}
}

// isSentenceEnd 判断是否句末标点。
func isSentenceEnd(r rune) bool {
	switch r {
	case '.', '!', '?', '。', '！', '？', '；', ';', '\n':
		return true
	default:
		return false
	}
}

// BuildAnswer 按证据顺序拼接抽取式摘要。
//
// 只拼接已通过逐字校验的引用, 不生成新事实, 也不改写原文。
func BuildAnswer(evidence []Evidence) string {
	parts := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		if q := strings.TrimSpace(ev.Quote); q != "" {
			parts = append(parts, q)
		}
	}
	return strings.Join(parts, "\n")
}

// hashDocument 返回规范化正文的 SHA-256。
func hashDocument(markdown string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(markdown)))
	return hex.EncodeToString(sum[:])
}

// itoa 是无依赖的小整数转换, 避免为拼 Evidence ID 引入 fmt。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
