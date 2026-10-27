package search

// fetchselect.go —— 在规范化全文上执行预算截断、相关选段与 continuation。
//
// Offset/NextOffset 和 Evidence.Offset 都使用 rune 索引；Hash 始终覆盖完整
// 正文，而不是本次截断后的片段。prompt 只参与相关性评分，不会被当作
// 指令执行，也不会改写正文。相距较远的相关事实可由多段逐字摘录组成，
// 片段间只插入空行，每条证据仍按原片段的绝对 rune 区间回查。

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// tokenCountMethod 说明 estimated_content_tokens 的兼容字段现在返回什么。
// 对不解析特殊 token 的 UTF-8 文本，任意可逆 tokenizer 的每个 token 都至少
// 对应一个字节；因此字节数是无需借用其它模型词表的保守 token 上界。
const tokenCountMethod = "utf8_bytes_upper_bound"

func validateFetchRequest(req FetchRequest) error {
	if req.MaxChars < 0 || req.MaxContentTokens < 0 || req.Offset < 0 {
		return Wrap("fetch", ErrInvalid, errors.New("negative fetch budget or offset"))
	}
	return nil
}

func prepareDocument(full *Document, req FetchRequest) (*Document, []string, error) {
	if err := validateFetchRequest(req); err != nil {
		return nil, nil, err
	}
	if full == nil {
		return nil, nil, Wrap("fetch", ErrUnavailable, errors.New("empty fetched document"))
	}
	maxChars := req.MaxChars
	if maxChars == 0 {
		maxChars = defaultFetchChars
	}
	if req.MaxContentTokens > 0 {
		// rune 数不会大于 UTF-8 字节数。先用预算限制候选窗口，随后在字节
		// 边界内二次收口；不能再用 chars/4 假设中文或 emoji 的实际消耗。
		if req.MaxContentTokens < maxChars {
			maxChars = req.MaxContentTokens
		}
	}
	if maxChars <= 0 {
		return nil, nil, Wrap("fetch", ErrInvalid, errors.New("fetch content budget must be positive"))
	}

	doc := cloneDocument(full)
	runes := []rune(full.Markdown)
	doc.TotalChars = len(runes)
	doc.Evidence = nil
	if req.Offset > len(runes) {
		return nil, nil, Wrap("fetch", ErrInvalid, fmt.Errorf("offset %d exceeds document length %d", req.Offset, len(runes)))
	}
	if doc.Hash == "" {
		doc.Hash = hashDocument(full.Markdown)
	}

	start, end := req.Offset, len(runes)
	if end-start > maxChars {
		end = start + maxChars
	}
	selection := "prefix"
	var degraded []string
	var passages []runeSpan
	if req.Offset > 0 {
		selection = "continuation"
	} else if strings.TrimSpace(req.Prompt) != "" && len(runes) > maxChars {
		if selected, ok := relevantPassages(runes, req.Prompt, maxChars, req.MaxContentTokens); ok && relevantPassagesHaveBody(runes, selected) {
			passages = selected
			start, end = selected[0].start, selected[len(selected)-1].end
			selection = "relevant"
		} else {
			degraded = append(degraded, "prompt_no_match")
		}
	}
	if len(passages) == 0 {
		if req.MaxContentTokens > 0 && end > start {
			end = start + runesWithinUTF8Budget(runes[start:end], req.MaxContentTokens)
			if end == start {
				return nil, nil, Wrap("fetch", ErrInvalid, fmt.Errorf(
					"max_content_tokens %d cannot fit the next UTF-8 character at offset %d", req.MaxContentTokens, start))
			}
		}
		passages = []runeSpan{{start: start, end: end}}
	}
	doc.Markdown = joinRunePassages(runes, passages)
	doc.Offset = start
	doc.Selection = selection
	doc.Truncated = start > 0 || end < len(runes) || len(passages) > 1
	doc.NextOffset = 0
	if end < len(runes) {
		doc.NextOffset = end
	}
	doc.EstimatedTokens = len([]byte(doc.Markdown))

	// 多段摘录不是全文中的单一连续子串，不能再用 doc.Offset+拼接位置
	// 推证据 offset。每段都保留绝对原文区间，逐条精确定位。
	var quotes []string
	if strings.TrimSpace(req.Prompt) != "" {
		tokens := contentTokens(req.Prompt)
		keywords := make([]string, 0, len(tokens))
		for token := range tokens {
			keywords = append(keywords, token)
		}
		quotes = bestSentences(doc.Markdown, keywords, 3)
	}
	if len(quotes) == 0 && len(passages) > 0 {
		quote := string(runes[passages[0].start:passages[0].end])
		if utf8.RuneCountInString(quote) > maxEvidenceQuoteRunes {
			quote = string([]rune(quote)[:maxEvidenceQuoteRunes])
		}
		if strings.TrimSpace(quote) != "" {
			quotes = []string{quote}
		}
	}
	doc.Evidence = evidenceForPassages(doc, runes, passages, quotes)
	return doc, degraded, nil
}

// runesWithinUTF8Budget 返回能放入字节预算的最长 rune 前缀。
// 只按完整 rune 截断，避免返回非法 UTF-8；offset 因而仍是 rune 索引。
func runesWithinUTF8Budget(text []rune, budget int) int {
	used := 0
	for i, r := range text {
		size := utf8.RuneLen(r)
		if size <= 0 || used+size > budget {
			return i
		}
		used += size
	}
	return len(text)
}

type runeSpan struct {
	start int
	end   int
}

const (
	maxRelevantParagraphRunes = 240
	maxRelevantPassages       = 6
)

type relevantCandidate struct {
	span      runeSpan
	keywords  map[string]struct{}
	hits      int
	runeCount int
	byteCount int
}

// relevantPassages 在共同预算内做有界的关键词覆盖。先选能带来新信息面的
// 片段，再补相关上下文；最后恢复原文顺序。短段落保持完整，长段落按句切分，
// 避免一个作者列表或长摘要挤掉另一处答案。
func relevantPassages(text []rune, prompt string, maxChars, byteBudget int) ([]runeSpan, bool) {
	keywords := fetchPromptKeywords(prompt)
	if len(keywords) == 0 || maxChars <= 0 {
		return nil, false
	}
	candidates := buildRelevantCandidates(text, keywords, maxChars, byteBudget)
	if len(candidates) == 0 {
		return nil, false
	}

	selected := make(map[int]struct{})
	covered := make(map[string]struct{})
	passages := make([]runeSpan, 0, maxRelevantPassages)
	usedRunes, usedBytes := 0, 0
	for phase := 0; phase < 2 && len(passages) < maxRelevantPassages; phase++ {
		for len(passages) < maxRelevantPassages {
			best, bestNew, bestCost := -1, 0, 0
			for i, candidate := range candidates {
				if _, ok := selected[i]; ok {
					continue
				}
				newHits := 0
				for keyword := range candidate.keywords {
					if _, ok := covered[keyword]; !ok {
						newHits++
					}
				}
				if (phase == 0 && newHits == 0) || candidate.hits == 0 {
					continue
				}
				costRunes, costBytes := candidate.runeCount, candidate.byteCount
				if len(passages) > 0 {
					costRunes += 2
					costBytes += 2
				}
				if usedRunes+costRunes > maxChars || (byteBudget > 0 && usedBytes+costBytes > byteBudget) {
					continue
				}
				if best >= 0 {
					current := candidates[best]
					if newHits < bestNew ||
						(newHits == bestNew && candidate.hits < current.hits) ||
						(newHits == bestNew && candidate.hits == current.hits && len(keywords) > 1 && costRunes > bestCost) ||
						(newHits == bestNew && candidate.hits == current.hits && (len(keywords) == 1 || costRunes == bestCost) && candidate.span.start > current.span.start) {
						continue
					}
				}
				best, bestNew, bestCost = i, newHits, costRunes
			}
			if best < 0 {
				break
			}
			candidate := candidates[best]
			costRunes, costBytes := candidate.runeCount, candidate.byteCount
			if len(passages) > 0 {
				costRunes += 2
				costBytes += 2
			}
			usedRunes += costRunes
			usedBytes += costBytes
			selected[best] = struct{}{}
			passages = append(passages, candidate.span)
			for keyword := range candidate.keywords {
				covered[keyword] = struct{}{}
			}
		}
	}
	if len(passages) == 0 {
		return nil, false
	}
	sort.SliceStable(passages, func(i, j int) bool { return passages[i].start < passages[j].start })
	return passages, true
}

func buildRelevantCandidates(text []rune, keywords map[string]struct{}, maxChars, byteBudget int) []relevantCandidate {
	var out []relevantCandidate
	for _, paragraph := range paragraphRuneSpans(text) {
		if isLowValueNavigation(string(text[paragraph.start:paragraph.end])) {
			continue
		}
		// HTML 转 Markdown 时，正文与紧随其后的下载链接可能合并为一段。
		// 先裁掉导航后缀再决定是否切句，避免作者列表因附带的 View PDF
		// 文本超过段落阈值而被拆散，或连同后半段一起被导航过滤掉。
		paragraph = trimLowValueNavigationSuffix(text, paragraph)
		if paragraph.end <= paragraph.start {
			continue
		}
		parts := []runeSpan{paragraph}
		if paragraph.end-paragraph.start > maxRelevantParagraphRunes {
			parts = sentenceRuneSpans(text, paragraph)
		}
		for _, part := range parts {
			if isLowValueNavigation(string(text[part.start:part.end])) {
				continue
			}
			part = trimLowValueNavigationSuffix(text, part)
			part = clipRelevantPassage(text, part, keywords, maxChars, byteBudget)
			if part.end <= part.start {
				continue
			}
			matched := matchedFetchKeywords(text, part, keywords)
			if len(matched) == 0 {
				continue
			}
			fragment := string(text[part.start:part.end])
			out = append(out, relevantCandidate{
				span: part, keywords: matched, hits: len(matched),
				runeCount: part.end - part.start, byteCount: len([]byte(fragment)),
			})
		}
	}
	return out
}

// sentenceRuneSpans 只用于长段落；短作者列表等保持整段，避免缩写中的句点
// 把名字列表切断。sentenceSpans 的字节 offset 在这里统一转成 rune offset。
func sentenceRuneSpans(text []rune, paragraph runeSpan) []runeSpan {
	base := string(text[paragraph.start:paragraph.end])
	spans := sentenceSpans(base)
	out := make([]runeSpan, 0, len(spans))
	for _, span := range spans {
		start := paragraph.start + utf8.RuneCountInString(base[:span.start])
		end := paragraph.start + utf8.RuneCountInString(base[:span.end])
		if end > start {
			out = append(out, runeSpan{start: start, end: end})
		}
	}
	return out
}

func clipRelevantPassage(text []rune, span runeSpan, keywords map[string]struct{}, maxChars, byteBudget int) runeSpan {
	if span.end-span.start <= maxChars && (byteBudget <= 0 || len([]byte(string(text[span.start:span.end]))) <= byteBudget) {
		return span
	}
	lower := strings.ToLower(string(text[span.start:span.end]))
	first := -1
	for keyword := range keywords {
		if at := strings.Index(lower, keyword); at >= 0 && (first < 0 || at < first) {
			first = at
		}
	}
	hitRune := 0
	if first >= 0 {
		hitRune = utf8.RuneCountInString(lower[:first])
	}
	width := maxChars
	if span.end-span.start < width {
		width = span.end - span.start
	}
	start := span.start + hitRune - width/3
	if start < span.start {
		start = span.start
	}
	if start+width > span.end {
		start = span.end - width
	}
	end := start + width
	if byteBudget > 0 {
		end = start + runesWithinUTF8Budget(text[start:end], byteBudget)
	}
	return runeSpan{start: start, end: end}
}

// fetchPromptKeywords 统一常见英文词形，让 submission/submitted、
// proposed/propose、decide/decided 等能命中同一事实，不引入外部词典。
func fetchPromptKeywords(prompt string) map[string]struct{} {
	out := make(map[string]struct{})
	for token := range contentTokens(prompt) {
		out[fetchKeywordRoot(token)] = struct{}{}
	}
	if len(out) > 1 {
		// list 描述输出形式，paper 描述来源载体；它们不能独立证明一个
		// 候选包含答案。否则 listing/Connected Papers 等导航会与真正的
		// 作者、日期、架构片段获得相同覆盖分，并凭更短挤占内容预算。
		delete(out, "list")
		delete(out, "paper")
	}
	return out
}

func fetchKeywordRoot(token string) string {
	switch token {
	case "submission":
		return "submit"
	case "architecture", "architectural":
		return "architect"
	}
	if len(token) > 5 && strings.HasSuffix(token, "ed") {
		root := strings.TrimSuffix(token, "ed")
		if len(root) >= 2 && root[len(root)-1] == root[len(root)-2] {
			root = root[:len(root)-1]
		}
		return root
	}
	if len(token) > 4 && strings.HasSuffix(token, "s") && !strings.HasSuffix(token, "ss") {
		return strings.TrimSuffix(token, "s")
	}
	if len(token) >= 6 && strings.HasSuffix(token, "e") {
		return strings.TrimSuffix(token, "e")
	}
	return token
}

func matchedFetchKeywords(text []rune, span runeSpan, keywords map[string]struct{}) map[string]struct{} {
	lower := strings.ToLower(string(text[span.start:span.end]))
	matched := make(map[string]struct{})
	for keyword := range keywords {
		if strings.Contains(lower, keyword) || (keyword == "date" && looksLikeDate(lower)) {
			matched[keyword] = struct{}{}
		}
	}
	return matched
}

func looksLikeDate(text string) bool {
	if utf8.RuneCountInString(text) > 180 || strings.IndexFunc(text, unicode.IsDigit) < 0 {
		return false
	}
	for _, month := range []string{
		"january", "february", "march", "april", "may", "june",
		"july", "august", "september", "october", "november", "december",
	} {
		if strings.Contains(text, month) {
			return true
		}
	}
	if strings.Contains(text, "年") && strings.Contains(text, "月") &&
		(strings.Contains(text, "日") || strings.Contains(text, "号")) {
		return true
	}
	for i := 0; i+10 <= len(text); i++ {
		if text[i+4] == '-' && text[i+7] == '-' &&
			allASCIIDigits(text[i:i+4]) && allASCIIDigits(text[i+5:i+7]) && allASCIIDigits(text[i+8:i+10]) {
			return true
		}
	}
	return false
}

func allASCIIDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := range text {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

var fetchNavigationSuffixMarkers = []string{
	"view a pdf", "view pdf", "download pdf", "html (experimental)", "access paper",
}

var fetchNavigationOnlyMarkers = []string{
	"accept cookies", "cookie settings", "disable mathjax",
}

// trimLowValueNavigationSuffix 只调整候选区间，不修改规范化全文。返回值仍
// 可直接用于原文切片和绝对 evidence offset；正文前的导航由调用方过滤。
func trimLowValueNavigationSuffix(text []rune, span runeSpan) runeSpan {
	fragment := string(text[span.start:span.end])
	lower := strings.ToLower(fragment)
	cut := len(fragment)
	for _, marker := range fetchNavigationSuffixMarkers {
		if at := strings.Index(lower, marker); at >= 0 && at < cut {
			cut = at
		}
	}
	if cut == len(fragment) {
		return span
	}
	end := span.start + utf8.RuneCountInString(fragment[:cut])
	for end > span.start && unicode.IsSpace(text[end-1]) {
		end--
	}
	return runeSpan{start: span.start, end: end}
}

func isLowValueNavigation(text string) bool {
	if utf8.RuneCountInString(text) > 220 {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, marker := range fetchNavigationOnlyMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	for _, marker := range fetchNavigationSuffixMarkers {
		at := strings.Index(lower, marker)
		if at >= 0 && !hasTextBeforeNavigation(lower[:at]) {
			return true
		}
	}
	return isMetadataNavigationList(lower)
}

// hasTextBeforeNavigation 区分 "View PDF" 与 "Authors:...View PDF"。
// Markdown 的 #、- 等结构符不算正文；有实际前缀时保留前缀并裁掉导航。
func hasTextBeforeNavigation(prefix string) bool {
	return strings.IndexFunc(prefix, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}) >= 0
}

// isMetadataNavigationList 排除 Author/Venue 一类元数据导航分面，避免它们
// 仅凭查询里的 authors 挤掉真正的作者列表。普通人名或主题列表不受影响。
func isMetadataNavigationList(text string) bool {
	items := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line[0] != '-' && line[0] != '*' {
			return false
		}
		label := strings.TrimSpace(line[1:])
		switch label {
		case "author", "authors", "venue", "institution", "topic", "topics":
			items++
		default:
			return false
		}
	}
	return items >= 2
}

// relevantPassagesHaveBody 防止 "quote" 只命中 "# Quotes" 这类标题时，
// 把标题本身当成相关答案。标题与正文同时入选仍保留；若全是标题则回退
// 连续 prefix，让读者至少看到标题后的实际内容。
func relevantPassagesHaveBody(text []rune, passages []runeSpan) bool {
	for _, span := range passages {
		for _, line := range strings.Split(string(text[span.start:span.end]), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !isMarkdownHeadingLine(line) {
				return true
			}
		}
	}
	return false
}

func isMarkdownHeadingLine(line string) bool {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	return level >= 1 && level <= 6 && level < len(line) &&
		(line[level] == ' ' || line[level] == '\t')
}

// joinRunePassages 只插入空行，不加省略号或生成文字；每段自身始终是原文。
func joinRunePassages(text []rune, passages []runeSpan) string {
	var out strings.Builder
	for i, span := range passages {
		if i > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(string(text[span.start:span.end]))
	}
	return out.String()
}

func evidenceForPassages(doc *Document, full []rune, passages []runeSpan, quotes []string) []Evidence {
	out := make([]Evidence, 0, len(quotes))
	for _, raw := range quotes {
		quote := strings.TrimSpace(raw)
		if quote == "" {
			continue
		}
		for _, span := range passages {
			fragment := string(full[span.start:span.end])
			at := strings.Index(fragment, quote)
			if at < 0 {
				continue
			}
			out = append(out, Evidence{
				ID: "E" + itoa(len(out)+1), URL: doc.URL, Quote: quote,
				Offset: span.start + utf8.RuneCountInString(fragment[:at]),
				Hash:   doc.Hash, FetchedAt: doc.FetchedAt,
			})
			break
		}
	}
	return out
}

func paragraphRuneSpans(text []rune) []runeSpan {
	var out []runeSpan
	start := -1
	for i := 0; i <= len(text); i++ {
		boundary := i == len(text) || (i+1 < len(text) && text[i] == '\n' && text[i+1] == '\n')
		if !boundary {
			if start < 0 && !unicode.IsSpace(text[i]) {
				start = i
			}
			continue
		}
		if start >= 0 {
			end := i
			for end > start && unicode.IsSpace(text[end-1]) {
				end--
			}
			if end > start {
				out = append(out, runeSpan{start: start, end: end})
			}
			start = -1
		}
		if i+1 < len(text) {
			i++
		}
	}
	return out
}
