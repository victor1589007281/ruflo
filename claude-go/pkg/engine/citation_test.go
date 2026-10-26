// citation_test.go —— 引用账本的验收。
//
// 这条能力的失败形态是**静默**: 编造的 URL 原样进报告, 审计没记, 或者把
// 真正打开过的 URL 误报成未验证 (模型于是被引导去引用搜索结果页)。
// 因此两条臂都要有:
//   - 工具结果/入参里出现过的 URL (含尾部标点、fragment、大小写差异) 必须算已见;
//   - 最终文本里凭空构造的 URL 必须被列出来.
package engine

import (
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestCitationLedgerNormalizesBeforeMatching(t *testing.T) {
	l := newCitationLedger()
	l.observeMessages([]types.Message{{
		Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{
			Type:    types.ContentBlockToolResult,
			Content: "来源: HTTPS://Example.COM/report.pdf. 另见 <https://example.com/a/>",
		}},
	}})

	// 最终文本里的写法不同 (小写 host、去 fragment、去尾斜杠、句末标点).
	final := "见 https://example.com/report.pdf#page=3 与 https://EXAMPLE.com/a。"
	missing, cited := l.unverified(final)
	if len(missing) != 0 {
		t.Fatalf("已见 URL 被误判为未验证: %v (cited=%d)", missing, cited)
	}
	if cited != 2 {
		t.Fatalf("cited = %d, want 2", cited)
	}
}

func TestCitationLedgerFlagsFabricatedURL(t *testing.T) {
	l := newCitationLedger()
	l.observeText("https://www.fda.gov/news-events/press-announcements/real-page")

	final := "证据: https://www.fda.gov/news-events/press-announcements/real-page\n" +
		"另: https://www.accessdata.fda.gov/drugsatfda_docs/label/2026/220934s003lbl.pdf"
	missing, _ := l.unverified(final)
	if len(missing) != 1 || !strings.Contains(missing[0], "accessdata.fda.gov") {
		t.Fatalf("凭空拼接的 accessdata URL 未被标记: %v", missing)
	}
}

// TestCitationLedgerAcceptsDOIFromBareNumber 钉住"检索给 DOI 号、报告写 doi.org
// 链接"这条合法构造不被误报。误报的代价是审计失去分辨力: 真正的编造混在
// 一堆 DOI 警告里, 没人再看。
func TestCitationLedgerAcceptsDOIFromBareNumber(t *testing.T) {
	l := newCitationLedger()
	l.observeText("原文 DOI: 10.1056/NEJMoa2500969.")
	final := "见 https://doi.org/10.1056/NEJMoa2500969 与 https://doi.org/10.9999/nope"
	missing, _ := l.unverified(final)
	if len(missing) != 1 || !strings.Contains(missing[0], "10.9999/nope") {
		t.Fatalf("应只标记账本里没有的 DOI: %v", missing)
	}
}

// TestCitationLedgerTracksOnlySuccessfulFetches 钉住"见过"与"抓取成功"的区别。
//
// 这条区分是报告可信度的分界线: 搜索结果里的 URL 可以直接抄进证据表, 但摘录
// 只能来自真正打开过的页面。把失败的 WebFetch 也算成已抓取, 审计就会把 403/404
// 的页面当合格证据 —— 那正是要拦的那类错误。
func TestCitationLedgerTracksOnlySuccessfulFetches(t *testing.T) {
	l := newCitationLedger()
	uses := []types.ContentBlock{
		{Type: types.ContentBlockToolUse, ID: "u1", Name: "WebFetch", Input: []byte(`{"url":"https://ok.example/a"}`)},
		{Type: types.ContentBlockToolUse, ID: "u2", Name: "WebFetch", Input: []byte(`{"url":"https://dead.example/b"}`)},
		{Type: types.ContentBlockToolUse, ID: "u3", Name: "Bash", Input: []byte(`{"command":"curl -sL https://pdf.example/c.pdf -o c.pdf"}`)},
		{Type: types.ContentBlockToolUse, ID: "u4", Name: "Bash", Input: []byte(`{"command":"echo https://echo.example/d"}`)},
	}
	results := []types.Message{{Type: types.MessageTypeUser, Content: []types.ContentBlock{
		{Type: types.ContentBlockToolResult, ToolUseID: "u1", Content: "page"},
		{Type: types.ContentBlockToolResult, ToolUseID: "u2", Content: "403", IsError: true},
		{Type: types.ContentBlockToolResult, ToolUseID: "u3", Content: "saved"},
		{Type: types.ContentBlockToolResult, ToolUseID: "u4", Content: "echo"},
	}}}
	l.observeFetches(uses, results)

	got := l.sampleFetched(10)
	want := []string{"https://ok.example/a", "https://pdf.example/c.pdf"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("fetched = %v, want %v", got, want)
	}

	// 四个 URL 都算"见过", 但只有两个算已抓取。
	unfetched, cited := l.unfetched("https://ok.example/a https://dead.example/b https://pdf.example/c.pdf https://echo.example/d")
	if cited != 4 || len(unfetched) != 2 {
		t.Fatalf("unfetched = %v (cited=%d), want 2 of 4", unfetched, cited)
	}
}

// TestCitationLedgerBashTemplateCountsAsFetched 实测 fed 的 FRED 取数写成
// `for s in UNRATE PAYEMS ...; curl "...csv?id=$s"`: 命令里没有具体参数,
// 报告却会引用 `...csv?id=UNRATE`。旧逻辑把 5 条真实取数全报成"从未返回过"
// (疑似编造), 噪音会淹没真正的编造告警。
func TestCitationLedgerBashTemplateCountsAsFetched(t *testing.T) {
	l := newCitationLedger()
	uses := []types.ContentBlock{{
		Type: types.ContentBlockToolUse, ID: "u1", Name: "Bash",
		Input: []byte(`{"command":"for s in UNRATE PAYEMS; do curl -s \"https://fred.example/graph/fredgraph.csv?id=$s&cosd=2025-01-01\" -o $s.csv; done"}`),
	}}
	results := []types.Message{{Type: types.MessageTypeUser, Content: []types.ContentBlock{
		{Type: types.ContentBlockToolResult, ToolUseID: "u1", Content: "saved"},
	}}}
	l.observeFetches(uses, results)

	final := "见 https://fred.example/graph/fredgraph.csv?id=UNRATE 与 " +
		"https://fred.example/graph/fredgraph.csv?id=CPIAUCSL&cosd=2025-01-01"
	missing, cited := l.unverified(final)
	if len(missing) != 0 {
		t.Fatalf("同端点变量取数被误报为未验证: %v (cited=%d)", missing, cited)
	}
	unfetched, _ := l.unfetched(final)
	if len(unfetched) != 0 {
		t.Fatalf("同端点变量取数被误报为未抓取: %v", unfetched)
	}

	// 模板只覆盖同路径, 换个端点仍要报。
	other := "https://fred.example/graph/other.csv?id=UNRATE"
	if m, _ := l.unverified(other); len(m) != 1 {
		t.Fatalf("不同路径不应被模板放过: %v", m)
	}
}

// TestCitationLedgerObservesToolUseInputs 钉住 WebFetch URL 只出现在入参这件事。
func TestCitationLedgerObservesToolUseInputs(t *testing.T) {
	l := newCitationLedger()
	l.observeToolUses([]types.ContentBlock{{
		Type:  types.ContentBlockToolUse,
		Name:  "WebFetch",
		Input: []byte(`{"url":"https://opentelemetry.io/docs/collector/"}`),
	}})
	missing, _ := l.unverified("见 https://opentelemetry.io/docs/collector/")
	if len(missing) != 0 {
		t.Fatalf("WebFetch 入参 URL 应算已见: %v", missing)
	}
}

// TestCitationURLStopsAtAdjacentChineseProse 钉住 URL 与中文正文贴在一起时
// 的边界。真实报告里 `URL）说明（下一个URL）` 极常见; 旧正则会把整句中文
// 吞进 URL, 审计于是报出一条永远匹配不上的"链接", 引用核验提示也变成乱码。
func TestCitationURLStopsAtAdjacentChineseProse(t *testing.T) {
	text := "见 https://www.fx168news.com/article/1075841）域名解析失败；" +
		"中国经营网解读（http://www.cb.com.cn/index/show/gszx/cv/cv135324831333）返回"
	got := citationURLRE.FindAllString(text, -1)
	want := []string{
		"https://www.fx168news.com/article/1075841",
		"http://www.cb.com.cn/index/show/gszx/cv/cv135324831333",
	}
	if len(got) != len(want) {
		t.Fatalf("URL 数量 = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条 = %q, want %q (中文被吞进 URL)", i, got[i], want[i])
		}
	}
}

// TestCitationURLStopsAtBacktick 钉住 markdown 行内代码边界: 报告写成
// `https://www.bls.gov/news.release/cpi.nr0.htm` 时, 反引号不能进 URL。
func TestCitationURLStopsAtBacktick(t *testing.T) {
	got := citationURLRE.FindAllString("见 `https://www.bls.gov/news.release/cpi.nr0.htm` 的表格", -1)
	if len(got) != 1 || got[0] != "https://www.bls.gov/news.release/cpi.nr0.htm" {
		t.Fatalf("反引号被并进 URL: %v", got)
	}
}

// TestCitationDOIKeepsParentheses 钉住另一侧: 不能为了截断中文把 DOI 里
// 合法的圆括号也截掉 (Elsevier 的 DOI 常见 `S0140-6736(20)30183-5`)。
func TestCitationDOIKeepsParentheses(t *testing.T) {
	l := newCitationLedger()
	l.observeText("DOI: 10.1016/S0140-6736(20)30183-5。")
	missing, _ := l.unverified("见 https://doi.org/10.1016/S0140-6736(20)30183-5")
	if len(missing) != 0 {
		t.Fatalf("带圆括号的 DOI 被误判为未验证: %v", missing)
	}
}

// TestSearchEngineCitations 钉住"结果页不是来源": 结果页 URL 随查询变化,
// 也不是一手材料; 它只能用来发现下一跳。
func TestSearchEngineCitations(t *testing.T) {
	l := newCitationLedger()
	text := "见 https://html.duckduckgo.com/html/?q=cpi 与 https://www.bing.com/search?q=pce " +
		"以及 https://www.bls.gov/news.release/cpi.nr0.htm"
	got := l.searchEngineCitations(text)
	if len(got) != 2 {
		t.Fatalf("searchEngineCitations = %v, want 2", got)
	}
	for _, u := range got {
		if strings.Contains(u, "bls.gov") {
			t.Fatalf("一手来源被误判为搜索引擎: %v", got)
		}
	}
}

// TestSeenOnlyExcludesFabricated 钉住两档计数的分界: 从未出现过的 URL 是
// "编造", 不能算进"只在搜索结果中出现"。
func TestSeenOnlyExcludesFabricated(t *testing.T) {
	l := newCitationLedger()
	l.observeText("https://seen.example/a")
	unfetched, _ := l.unfetched("https://seen.example/a https://ghost.example/x")
	seen := l.seenOnly(unfetched)
	if len(unfetched) != 2 {
		t.Fatalf("unfetched = %v, want 2", unfetched)
	}
	if len(seen) != 1 || seen[0] != "https://seen.example/a" {
		t.Fatalf("seenOnly = %v, want 仅 seen.example", seen)
	}
}

func TestCitationLedgerSampleIsSortedAndCapped(t *testing.T) {
	l := newCitationLedger()
	for _, u := range []string{
		"https://c.example/3", "https://a.example/1",
		"https://b.example/2", "https://d.example/4",
	} {
		l.observeText(u)
	}
	got := l.sample(3)
	if len(got) != 3 {
		t.Fatalf("sample cap = %d, want 3", len(got))
	}
	if got[0] != "https://a.example/1" || got[1] != "https://b.example/2" || got[2] != "https://c.example/3" {
		t.Fatalf("sample 未排序: %v", got)
	}
}

func TestFinalizeNoticeIncludesVerifiedURLs(t *testing.T) {
	msg := finalizeNotice(true, 1, []string{"https://a.example/1", "https://b.example/2"}, "")
	if len(msg.Content) != 1 {
		t.Fatalf("unexpected content: %+v", msg.Content)
	}
	text := msg.Content[0].Text
	for _, want := range []string{"最后一轮", "https://a.example/1", "只能", "不要根据申请号"} {
		if !strings.Contains(text, want) {
			t.Fatalf("收口提示缺少 %q:\n%s", want, text)
		}
	}
	// 清单为空时不应出现空清单标题.
	noList := finalizeNotice(false, 2, nil, "")
	if strings.Contains(noList.Content[0].Text, "URL 如下") {
		t.Fatalf("空清单不该输出 URL 段: %s", noList.Content[0].Text)
	}
}
