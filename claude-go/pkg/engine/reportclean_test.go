// reportclean_test.go —— 最终报告旁白清理的验收。
//
// 两类误伤必须防住:
//   - 把正文里的正常句子 ("Let me know"、引用的英文原文) 当旁白删掉;
//   - 把旁白与标题挤在同一行时, 连标题一起删掉.
package engine

import (
	"strings"
	"testing"
)

func TestCleanReportTextDropsPreambleBeforeH1(t *testing.T) {
	in := "Let me draft carefully with citations.\n\nLet me now write the report.\n\n" +
		"# 2026 年报告\n\n正文第一段。\n"
	got := CleanReportText(in)
	if strings.Contains(got, "Let me") {
		t.Fatalf("起草前言没有清掉:\n%s", got)
	}
	if !strings.HasPrefix(got, "# 2026 年报告") {
		t.Fatalf("H1 没有成为第一行:\n%s", got)
	}
	if !strings.Contains(got, "正文第一段。") {
		t.Fatalf("正文被误删:\n%s", got)
	}
}

func TestCleanReportTextKeepsInlineH1WhenMetaLine(t *testing.T) {
	in := "Let me count Chinese characters roughly; keep body ~2000 words.# 2026 年报告\n\n正文。\n"
	got := CleanReportText(in)
	if !strings.HasPrefix(got, "# 2026 年报告") {
		t.Fatalf("与旁白同行的 H1 被删掉:\n%s", got)
	}
	if strings.Contains(got, "Let me count") {
		t.Fatalf("旁白没有清掉:\n%s", got)
	}
}

func TestCleanReportTextDropsSelfTalkAndRelabelsHonest(t *testing.T) {
	in := "# 报告\n\n结论。\n\n" +
		"Hmm judgment 5's number — I don't have a source. Remove or mark.\n\n" +
		"诚实：TSMC 不披露产能。\n\n" +
		"Let me write it now. Careful: no meta language.\n\n" +
		"局限与未解问题。\n"
	got := CleanReportText(in)
	for _, bad := range []string{"Hmm", "I don't have a source", "Let me write", "诚实"} {
		if strings.Contains(got, bad) {
			t.Fatalf("残留旁白 %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "局限：TSMC 不披露产能。") {
		t.Fatalf("诚实：应改标为局限：并保留信息:\n%s", got)
	}
	if !strings.Contains(got, "结论。") || !strings.Contains(got, "局限与未解问题。") {
		t.Fatalf("正文被误删:\n%s", got)
	}
}

func TestCleanReportTextPreservesCodeFenceAndNormalEnglish(t *testing.T) {
	in := "# 报告\n\n```\nHmm this is quoted output\nLet me write it now.\n```\n\n" +
		"如对方所说，Let me know 是礼貌用语。\n"
	got := CleanReportText(in)
	if !strings.Contains(got, "Hmm this is quoted output") || !strings.Contains(got, "Let me write it now.") {
		t.Fatalf("代码块内容被误删:\n%s", got)
	}
	if !strings.Contains(got, "Let me know 是礼貌用语") {
		t.Fatalf("正文里的 Let me know 被误删:\n%s", got)
	}
}

// TestCleanReportTextDropsDraftingPlan 实测 fed 那份报告把"计划 + 报告"一起
// 交付了: 前 11 行是数据核对笔记, 然后 "报告结构：", H1 写成 "# 标题：...",
// 再列 "字数 1500-2500" 和 "证据表行" 草稿, 最后才是真正的正文。
// 这些行不含任何事实, 必须在交付前清掉, 但 H1 的真标题要留下。
func TestCleanReportTextDropsDraftingPlan(t *testing.T) {
	in := "H.15 一手数据非常有价值：\n- EFFR 2026-10-22 = 3.88%\n\n" +
		"现在有足够证据。剩一次写报告。预算已超，立即成文。\n\n" +
		"报告结构：\n" +
		"# 标题：美联储 2026 年 10 月政策核验\n" +
		"结论（3-5 条）：\n1. 事实基线。\n2. 资产负债表。\n\n" +
		"字数 1500-2500，证据表 15-25 行。\n\n" +
		"证据表行（每条 URL 完整）：\n1. 2026 年日历\n2. 加息 25bp\n\n" +
		"## 一、结论先行\n\n真正的正文。\n"
	got := CleanReportText(in)
	for _, bad := range []string{"H.15 一手数据", "报告结构", "字数 1500", "证据表行", "预算已超", "标题："} {
		if strings.Contains(got, bad) {
			t.Fatalf("残留起草计划 %q:\n%s", bad, got)
		}
	}
	if !strings.HasPrefix(got, "# 美联储 2026 年 10 月政策核验\n") {
		t.Fatalf("真标题没有成为第一行:\n%s", got)
	}
	if !strings.Contains(got, "## 一、结论先行") || !strings.Contains(got, "真正的正文。") {
		t.Fatalf("正文被误删:\n%s", got)
	}
}

// TestCleanReportTextDropsEnglishDraftingPlan 实测 cmb 交付物: H1 之后到第一个
// "## " 之间夹了 112 行英文计划(URL 清单 / Rows plan / 计算校验), 中文信号
// 一条都不命中, 于是被当成导语整段交付。这里按同样的形状钉住。
func TestCleanReportTextDropsEnglishDraftingPlan(t *testing.T) {
	in := `# 招商银行 2026 年半年度经营情况

Good — cnstock page confirms 摘要 content verbatim including 资本充足率.

Now write the report. I have enough. Let me compile evidence rows (15-25). Sources:
U1: s3gw report PDF (multiple rows)
U2: s3gw QA PDF
U3: nbd.com.cn (NFRA data reprint)
U4: zqrb.cn (证券日报 industry NIM)

That's 6 URLs, 5 domains.

Rows plan (20):
1. 营收1781.81亿 +4.83% [U1 p.7]
2. 归母净利764.45亿 +2.02%; 净利润769.19亿 [U1]

Now key analytical points (3-5 判断):
1. 营收增速高于利润。

计算校验：
- 营收 1781.81亿 vs 1699.69亿 → +82.12亿 (+4.83% ✓)
Check: 82.12 - 17.41 - 45.54 ≈ 18.3 ✓ roughly.

Write the report now. Ensure first line is "# ..." heading.

## 一、结论（先给判断）

1. **营收动能修复快于利润。** 真正的正文。

## 二、证据表

| # | 结论 | 来源 URL |
|---|---|---|
| 1 | 营收 | https://s3gw.cmbimg.com/a.pdf |
`
	got := CleanReportText(in)
	for _, bad := range []string{"Good —", "Now write", "U1:", "Rows plan", "计算校验", "Write the report now"} {
		if strings.Contains(got, bad) {
			t.Fatalf("残留英文起草计划 %q:\n%s", bad, got)
		}
	}
	if !strings.HasPrefix(got, "# 招商银行 2026 年半年度经营情况\n\n## 一、结论") {
		t.Fatalf("H1 之后应直接接正文:\n%s", got)
	}
	if !strings.Contains(got, "真正的正文。") || !strings.Contains(got, "https://s3gw.cmbimg.com/a.pdf") {
		t.Fatalf("正文或证据表被误删:\n%s", got)
	}
}

// TestCleanReportTextKeepsShortIntro 短导语(Sources: 一行)不能被当成计划删掉。
func TestCleanReportTextKeepsShortIntro(t *testing.T) {
	in := "# 报告\n\nSources: 公司年报与监管披露。\n\n## 结论\n\n正文。\n"
	if got := CleanReportText(in); got != in {
		t.Fatalf("短导语被误删:\n%s", got)
	}
}

// TestCleanReportTextKeepsLongIntroWithOneWeakSignal 较长的正常导语里出现一行
// "Sources:" 不足以判成英文起草计划 —— 弱信号要至少两处。
func TestCleanReportTextKeepsLongIntroWithOneWeakSignal(t *testing.T) {
	in := "# 报告\n\n" +
		"本报告覆盖三个问题。\n\n" +
		"第一，营收结构。\n\n" +
		"第二，息差变化。\n\n" +
		"第三，资产质量。\n\n" +
		"Sources: 公司年报与监管披露。\n\n" +
		"## 结论\n\n正文。\n"
	if got := CleanReportText(in); got != in {
		t.Fatalf("正常长导语被误删:\n%s", got)
	}
}

func TestEnsureReportTitleFromTask(t *testing.T) {
	task := "请做一份可核验的中文调研报告，主题：招商银行 2026 年半年度经营情况——营业收入/净利润、净息差。\n\n要求：..."
	got := EnsureReportTitle("## 结论\n\n正文。\n", task)
	if !strings.HasPrefix(got, "# 招商银行 2026 年半年度经营情况\n\n## 结论") {
		t.Fatalf("标题没有从任务主题补齐:\n%s", got)
	}
}

func TestEnsureReportTitleKeepsExistingH1(t *testing.T) {
	in := "# 已有标题\n\n正文。\n"
	if got := EnsureReportTitle(in, "主题：别的题目"); got != in {
		t.Fatalf("已有 H1 被改写:\n%s", got)
	}
}

// TestEnsureReportTitleReplacesPlaceholder 实测 cpi / tsmc 的 H1 就是字面量
// "# 标题" —— 有 H1 结构, 但没有标题内容。要换成任务主题, 不能原样交付。
func TestEnsureReportTitleReplacesPlaceholder(t *testing.T) {
	got := EnsureReportTitle("# 标题\n\n## 一、结论\n正文。\n", "主题：美国 2026 年通胀与货币政策路径——CPI/PCE。")
	if !strings.HasPrefix(got, "# 美国 2026 年通胀与货币政策路径\n\n## 一、结论") {
		t.Fatalf("占位标题没有替换:\n%s", got)
	}
}

func TestEnsureReportTitleFallsBack(t *testing.T) {
	got := EnsureReportTitle("## 结论\n", "没有任何主题标记")
	if !strings.HasPrefix(got, "# 调研报告\n\n") {
		t.Fatalf("缺少兜底标题:\n%s", got)
	}
}

func TestCleanReportTextLeavesCleanReportAlone(t *testing.T) {
	in := "# 标题\n\n## 结论\n\n正文。\n\n| # | 结论 | 来源 URL |\n|---|---|---|\n| 1 | x | https://a.example/1 |\n"
	got := CleanReportText(in)
	if got != in {
		t.Fatalf("干净报告被改写:\n--- want ---\n%s\n--- got ---\n%s", in, got)
	}
}
