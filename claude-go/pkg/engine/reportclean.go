// reportclean.go —— 交付前清掉正文里的"自我旁白"。
//
// 实测最终报告里会漏出起草过程:
//
//	Let me draft carefully with citations.
//	Hmm judgment 5's "2-3pp" number — I don't have a source. Remove or mark ...
//	诚实：TSMC 不披露产能 ...
//	Let me write it now. Careful: no meta language, start with #.
//
// 提示词已经禁止过程说明, 但模型仍会把草稿思路留在正文。这些都是**纯噪声**:
// 删掉不改变任何事实或判断, 留着却让报告像未完成的草稿。所以这里做确定性清理,
// 而不是再花一轮让模型重写 (重写可能顺手删掉真正的证据)。
package engine

import (
	"regexp"
	"strings"
)

var (
	// 一眼就是旁白的行首。
	metaAlwaysRE = regexp.MustCompile(`(?i)^\s*(?:hmm\b|wait[,.]|note to self\b|actually,\s*i\b)`)
	// 只有带"写/数/报告"等语境时才判定为旁白, 避免误删正文里的 "Let me know"。
	metaLeadRE = regexp.MustCompile(`(?i)^\s*(?:let me|let's|i'll|i will|i need to|i should|now i)\b`)
	metaCtxRE  = regexp.MustCompile(`(?i)(write|draft|count|report|revise|verify|check|start|say|source|meta language|character)`)
	// 句中出现的旁白短语。
	metaInline = []string{
		"I'll say", "I don't have a source", "Remove or mark",
		"let me count", "字数统计", "字数：", "字数:",
	}
	// 起草计划段落的信号。实测最终文本会是"计划 + 报告"的拼接:
	//
	//	现在有足够证据。剩一次写报告。预算已超，立即成文。
	//	报告结构：
	//	# 标题：美联储 ...
	//	结论（3-5 条）：
	//	字数 1500-2500，证据表 15-25 行。
	//	证据表行（每条 URL 完整）：
	//	...
	//	## 一、结论先行
	//
	// 这些行本身不含任何事实, 留着会让交付物看起来像草稿。识别到信号后整段
	// 丢弃, 而不是逐行删 (逐行删会留下残缺的小标题)。
	planSignalRE = regexp.MustCompile(`(?i)(报告结构|证据表行|预算已超|现在有足够证据|立即成文|字数\s*[:：]?\s*[0-9]|剩\s*[一两二三四五六七八九十0-9]+\s*次写)`)
	// 英文起草计划的行首信号。2026-10-25 cmb 交付物在 H1 与第一个 "## " 之间
	// 夹了 112 行英文计划:
	//
	//	Now write the report. I have enough. Let me compile evidence rows (15-25). Sources:
	//	U1: s3gw report PDF (multiple rows)
	//	Rows plan (20):
	//	That's ~24 rows. Good, but 正文 1500-2500 字.
	//	Now key analytical points (3-5 判断):
	//	计算校验：
	//	Write the report now. Ensure first line is "# ..." heading.
	//
	// 这些行没有一条命中 planSignalRE(中文), 于是整段被当成导语保留, 交付物
	// 正文从 1774 字涨到 3578 字。英文计划用英文信号识别。
	//
	// planStrongRE 的行首词几乎不可能出现在成稿导语里; planWeakRE 弱一些
	// (例如 Sources:), 只在较长区段(>=6 行)里才算数。
	planStrongRE = regexp.MustCompile(`(?im)^\s*(?:now write\b|i have enough\b|let me compile\b|u[0-9]+\s*[:：]|rows? plan\b|now key\b|write the report now\b|evidence table rows?\b|update evidence\.md\b|计算校验|写作计划)`)
	planWeakRE   = regexp.MustCompile(`(?im)^\s*(?:sources?\s*[:：]|check\s*[:：]|word count\b|aim\s*~?\s*[0-9]+\s*(?:字|chars?|words?)\b|that'?s\s*~?\s*[0-9]+\s+(?:rows|urls)\b|证据行)`)
)

// CleanReportText 删除最终报告里的过程旁白, 保留全部正文与证据表。
//
// 处理范围:
//  1. 第一个一级标题之前的起草前言 (仅当前言里出现旁白信号);
//  2. 独立的旁白行 (Hmm / Let me ... / Note to self ...);
//  3. "诚实：" 这类自述式提示 → 改为 "局限：" (保留信息, 只去掉旁白语气);
//  4. 连续 3 行以上的空行压成 1 行。
//
// 代码块内的内容不动: 那可能是被引用的原文。
func CleanReportText(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	orig := text
	if strings.Contains(text, "\r\n") {
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	hadTrailing := strings.HasSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	changed := strings.Contains(orig, "\r\n")

	// 1) 第一个 H1 之前的起草前言。只有出现旁白信号时才丢; 否则可能是正常的
	//    标题/摘要排版, 不能因为"没有 # 开头"就整段删。
	firstH1 := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "# ") {
			firstH1 = i
			break
		}
	}
	if firstH1 > 0 && hasMetaSignal(lines[:firstH1]) {
		lines = lines[firstH1:]
		changed = true
	}

	// 1b) H1 与第一个二级标题之间的"提纲/计划"块。实测模型会在标题下面先写
	//     "结论（3-5 条）：/字数 1500-2500，证据表 15-25 行。/证据表行：" 再写
	//     真正的正文。只有出现计划信号才整段丢, 正常报告的导语不动。
	if h1 := indexH1(lines); h1 >= 0 {
		next := -1
		for i := h1 + 1; i < len(lines); i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
				next = i
				break
			}
		}
		if next > h1+1 && (hasMetaSignal(lines[h1+1:next]) || hasPlanRegion(lines[h1+1:next])) {
			rest := append([]string{""}, lines[next:]...)
			lines = append(lines[:h1+1], rest...)
			changed = true
		}
	}

	// 2) 逐行清理, 跳过代码块。
	out := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		if kept, drop := cleanMetaLine(line); drop {
			if kept != "" {
				out = append(out, kept)
			}
			changed = true
			continue
		}
		out = append(out, line)
	}

	// 3) 压缩空行。
	compact := make([]string, 0, len(out))
	blank := 0
	for _, line := range out {
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				changed = true
				continue
			}
		} else {
			blank = 0
		}
		compact = append(compact, line)
	}

	// 没命中任何规则时**原样返回**: 报告是交付物, 不做无谓的空白重排, 否则
	// 无法用逐字节比较验证"这次清理没动正文"。
	if !changed {
		return orig
	}
	cleaned := strings.TrimSpace(strings.Join(compact, "\n"))
	if hadTrailing {
		cleaned += "\n"
	}
	return cleaned
}

// EnsureReportTitle 保证报告第一行是一级标题。
//
// 提示词要求"第一行必须是 # 标题", 但实测仍会有报告直接从 "## 结论" 开头
// (cmb / solid / sbom)。这时不编造内容, 直接从任务描述里的 "主题：" 取标题
// ——那本来就是用户给定的题目; 取不到才退回中性标题。
func EnsureReportTitle(text, task string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	title := titleFromTask(task)
	if title == "" {
		title = "调研报告"
	}
	lines := strings.Split(text, "\n")
	if h1 := indexH1(lines); h1 >= 0 {
		// 实测还有 "# 标题" 这种占位 H1 (cpi / tsmc): 结构上"有一级标题",
		// 内容上等于没有。只替换明确的占位词, 真标题一个字都不动。
		cur := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[h1]), "# "))
		if !placeholderTitle(cur) {
			return text
		}
		lines[h1] = "# " + title
		return strings.Join(lines, "\n")
	}
	return "# " + title + "\n\n" + strings.TrimLeft(text, "\n")
}

// placeholderTitle 判断 H1 是否只是占位标签。
func placeholderTitle(s string) bool {
	s = strings.ToLower(strings.TrimSpace(strings.TrimRight(s, ":：")))
	switch s {
	case "标题", "报告", "调研报告", "研究报告", "title", "report":
		return true
	}
	return false
}

// titleFromTask 从 "请做一份...报告，主题：X——Y" 里取 X。
func titleFromTask(task string) string {
	t := strings.TrimSpace(task)
	found := false
	for _, marker := range []string{"主题：", "主题:"} {
		i := strings.Index(t, marker)
		if i < 0 {
			continue
		}
		t = t[i+len(marker):]
		found = true
		break
	}
	if !found {
		return ""
	}
	if cut := strings.IndexAny(t, "\n"); cut >= 0 {
		t = t[:cut]
	}
	for _, sep := range []string{"——", "—", "。", "；", ";"} {
		if cut := strings.Index(t, sep); cut >= 0 {
			t = t[:cut]
		}
	}
	// 标题控制在 10-40 字附近: 超长说明抓到了整段要求, 截到第一个分句。
	t = strings.TrimSpace(strings.TrimRight(t, "，,、 "))
	r := []rune(t)
	if len(r) > 60 {
		for i := 40; i < len(r); i++ {
			if r[i] == '，' || r[i] == ',' || r[i] == ' ' {
				r = r[:i]
				break
			}
		}
		if len(r) > 60 {
			r = r[:60]
		}
	}
	return strings.TrimSpace(string(r))
}

// indexH1 返回第一个一级标题的行号。
func indexH1(lines []string) int {
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "# ") {
			return i
		}
	}
	return -1
}

// hasMetaSignal 判断一段前言/提纲里是否有起草旁白。
func hasMetaSignal(lines []string) bool {
	for _, line := range lines {
		if metaAlwaysRE.MatchString(line) {
			return true
		}
		if metaLeadRE.MatchString(line) && metaCtxRE.MatchString(line) {
			return true
		}
		if planSignalRE.MatchString(line) {
			return true
		}
		for _, p := range metaInline {
			if strings.Contains(line, p) {
				return true
			}
		}
	}
	return false
}

// hasPlanRegion 判断 H1 与第一个 "## " 之间的区段是不是一份起草计划。
//
// 与 hasMetaSignal 分开: 中文计划信号按"行"判断就够; 英文计划里真正的
// 计划词可能只出现在少数几行, 但区段整体是逐行罗列的 URL / 行号清单。
// 因此这里要求区段本身有 3 行以上非空内容, 再看命中强/弱信号。
func hasPlanRegion(lines []string) bool {
	nonEmpty := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonEmpty++
		}
	}
	if nonEmpty < 3 {
		return false
	}
	joined := strings.Join(lines, "\n")
	if planStrongRE.MatchString(joined) {
		return true
	}
	// 弱信号单独出现不足以判计划 (正常导语也可能有一行 "Sources:")。
	// 要求较长区段里至少两处弱信号, 才当成英文起草计划。
	return nonEmpty >= 6 && len(planWeakRE.FindAllString(joined, 2)) >= 2
}

// cleanMetaLine 返回 (保留内容, 是否命中旁白)。保留内容非空时替换原行。
func cleanMetaLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	// 模型会把"标题："这个占位标签连同真标题一起写进 H1。标签本身是过程
	// 语言, 标题要留下。
	for _, p := range []string{"# 标题：", "# 标题:"} {
		if strings.HasPrefix(trimmed, p) {
			if title := strings.TrimSpace(strings.TrimPrefix(trimmed, p)); title != "" {
				return "# " + title, true
			}
		}
	}
	if metaAlwaysRE.MatchString(line) {
		return inlineH1(line), true
	}
	if metaLeadRE.MatchString(line) && metaCtxRE.MatchString(line) {
		return inlineH1(line), true
	}
	for _, p := range metaInline {
		if strings.Contains(line, p) {
			return inlineH1(line), true
		}
	}
	// "诚实：" 是模型对读者的自述, 信息本身属于局限; 只换标签, 不删内容。
	if strings.HasPrefix(trimmed, "诚实：") || strings.HasPrefix(trimmed, "诚实:") {
		return strings.Replace(line, "诚实", "局限", 1), true
	}
	return "", false
}

// inlineH1 处理"旁白与标题挤在同一行"的情况 (实测:
// `Let me count Chinese characters roughly; keep body ~2000 words.# 2026 年...`)。
// 丢掉旁白, 但从 `# ` 起的标题必须留下。
func inlineH1(line string) string {
	if idx := strings.Index(line, "# "); idx >= 0 {
		return line[idx:]
	}
	return ""
}
