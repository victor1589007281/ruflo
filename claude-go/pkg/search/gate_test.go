package search

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestQualityGateAuthoritativeSingleDomain 钉住"官方单域名"例外。
//
// 这条例外是为一类真实误判加的: OTEL 的检索结果 8 条全部来自
// opentelemetry.io, 质量全绿 (有片段、语言匹配、无重复), 只因为域名数为 1
// 被判不达标, provider 返回的却是最相关的一手文档。
//
// 反向臂同样重要: 同一个博客的 3 条结果不能因为"域名少"被放行 —— 否则这条
// 例外就退化成取消域名门槛。
func TestQualityGateAuthoritativeSingleDomain(t *testing.T) {
	now := time.Now()
	mk := func(urls ...string) []Result {
		out := make([]Result, 0, len(urls))
		for _, u := range urls {
			out = append(out, Result{URL: u, Snippet: "x", Lang: "en", Published: now.Add(-time.Hour)})
		}
		return out
	}
	req := SearchRequest{Language: "en", Freshness: "any"}

	official := evaluateQualityAt(mk(
		"https://opentelemetry.io/docs/collector/",
		"https://opentelemetry.io/docs/specs/otel/",
		"https://opentelemetry.io/docs/languages/go/",
	), req, now)
	if official.Domains != 1 || official.AuthoritativeRatio != 1 {
		t.Fatalf("quality = %+v", official)
	}
	if !official.Pass(false) {
		t.Fatalf("单一官方域名应通过: %+v", official)
	}
	if got := official.gateReason(false); got != "pass" {
		t.Fatalf("gateReason = %q, want pass", got)
	}

	blog := evaluateQualityAt(mk(
		"https://random-blog.example/post-1",
		"https://random-blog.example/post-2",
		"https://random-blog.example/post-3",
	), req, now)
	if blog.Pass(false) {
		t.Fatalf("单一非权威域名不应通过: %+v", blog)
	}
	if got := blog.gateReason(false); got != "domains" {
		t.Fatalf("gateReason = %q, want domains", got)
	}

	// 80% 官方 + 20% 非官方: 按阈值放行; 60% 不放行。
	mixedPass := evaluateQualityAt(mk(
		"https://opentelemetry.io/a",
		"https://opentelemetry.io/b",
		"https://opentelemetry.io/c",
		"https://opentelemetry.io/d",
		"https://random-blog.example/post",
	), req, now)
	if !mixedPass.Pass(false) {
		t.Fatalf("官方占比 0.8 应通过: %+v", mixedPass)
	}
	mixedFail := evaluateQualityAt(mk(
		"https://opentelemetry.io/a",
		"https://opentelemetry.io/b",
		"https://opentelemetry.io/c",
		"https://random-blog.example/1",
		"https://random-blog.example/2",
	), req, now)
	if mixedFail.Pass(false) {
		t.Fatalf("官方占比 0.6 不应通过: %+v", mixedFail)
	}
}

func TestIsAuthoritativeDomain(t *testing.T) {
	cases := map[string]bool{
		"https://www.sec.gov/Archives/edgar/data": true,
		"https://ecb.europa.eu/press":             true,
		"https://www.who.int/news":                true,
		"https://pbc.gov.cn/goutongjiaoliu":       true,
		"https://www.gov.cn/zhengce/zhengceku/":   true, // apex 去 www. 后是 gov.cn, 后缀判据不覆盖
		"https://opentelemetry.io/docs":           true,
		"https://investor.nvidia.com/":            true,
		"https://ir.byd.com/":                     true,
		"https://random-blog.example/post":        false,
		"https://fakegov.example":                 false,
		"https://notgov":                          false,
	}
	for host, want := range cases {
		if got := IsAuthoritativeDomain(host); got != want {
			t.Fatalf("IsAuthoritativeDomain(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestQualityGateProfessionalDomains 钉住数据库/AI 调研的两类真实误判：
// ClickHouse 的 changelog 全在 clickhouse.com，ICLR 论文全在
// proceedings.iclr.cc / iclr.cc。它们都是官方一手页，不能因为域名集中
// 被判 domains 不达标；非官方博客仍必须失败。
func TestQualityGateProfessionalDomains(t *testing.T) {
	now := time.Now()
	mk := func(urls ...string) []Result {
		out := make([]Result, 0, len(urls))
		for _, u := range urls {
			out = append(out, Result{URL: u, Snippet: "x", Lang: "en", Published: now.Add(-time.Hour)})
		}
		return out
	}
	req := SearchRequest{Language: "en", Freshness: "any"}

	cases := map[string][]Result{
		"clickhouse": mk(
			"https://clickhouse.com/docs/resources/changelogs/oss/2025",
			"https://clickhouse.com/blog/clickhouse-2025-roundup",
			"https://clickhouse.com/docs/en/whats-new",
		),
		"iclr": mk(
			"https://proceedings.iclr.cc/paper_files/paper/2025/hash/a-Abstract-Conference.html",
			"https://iclr.cc/virtual/2025/social/37580",
			"https://openreview.net/forum?id=abc123",
		),
		"postgresql": mk(
			"https://www.postgresql.org/docs/current/release-18.html",
			"https://www.postgresql.org/about/press/presskit18/en",
			"https://www.postgresql.org/docs/current/release-18-notes.html",
		),
	}
	for name, results := range cases {
		q := evaluateQualityAt(results, req, now)
		if !q.Pass(false) || q.gateReason(false) != "pass" {
			t.Errorf("%s: 官方单域名应通过: %+v reason=%s", name, q, q.gateReason(false))
		}
	}

	blog := evaluateQualityAt(mk(
		"https://random-blog.example/clickhouse",
		"https://random-blog.example/postgres",
		"https://random-blog.example/iclr",
	), req, now)
	if blog.Pass(false) || blog.gateReason(false) != "domains" {
		t.Errorf("非官方单域名不应通过: %+v reason=%s", blog, blog.gateReason(false))
	}
}

// TestQualityGateUndatedResults 钉住时效门的"无日期≠过期"口径 (2026-11-26)。
//
// 事故背景: MCP 系 provider (parallel/exa) 常不返回 published, 旧口径把
// 无日期一律计为不新鲜, freshness=week 的学术查询约 2/3 挂在 fresh 上,
// 而结果本身有 10 条可用 —— 红灯全是口径误报, 淹没了真问题。
//
// 三个臂:
//  1. 全部无日期 → 时效门不适用, 通过 (dated=0 < minDatedForFreshGate);
//  2. 有日期样本不足 (2 条过期 + 其余无日期) → 样本太小不判, 通过;
//  3. 有日期样本足够且确实过期 (3 条全旧) → 仍然必须挂 fresh。
func TestQualityGateUndatedResults(t *testing.T) {
	now := time.Now()
	// 每条独立域名, 避免域名多样性门 (<3) 先于时效门触发。
	mkDated := func(n int, age time.Duration) []Result {
		out := make([]Result, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, Result{
				URL: fmt.Sprintf("https://d%d.example.com/post", i), Snippet: "x", Lang: "en",
				Published: now.Add(-age),
			})
		}
		return out
	}
	mkUndated := func(n int) []Result {
		out := make([]Result, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, Result{
				URL: fmt.Sprintf("https://u%d.example.com/post", i), Snippet: "x", Lang: "en",
			})
		}
		return out
	}
	req := SearchRequest{Language: "en", Freshness: "week"}

	// 臂 1: 全部无日期。
	allUndated := evaluateQualityAt(mkUndated(8), req, now)
	if allUndated.DatedResults != 0 {
		t.Fatalf("DatedResults = %d, want 0", allUndated.DatedResults)
	}
	if !allUndated.Pass(true) || allUndated.gateReason(true) != "pass" {
		t.Errorf("全部无日期不应挂 fresh: %+v reason=%s", allUndated, allUndated.gateReason(true))
	}

	// 臂 2: 2 条过期 + 6 条无日期 —— 有日期样本 < 3, 门不判。
	sparse := evaluateQualityAt(append(mkDated(2, 90*24*time.Hour), mkUndated(6)...), req, now)
	if sparse.DatedResults != 2 {
		t.Fatalf("DatedResults = %d, want 2", sparse.DatedResults)
	}
	if !sparse.Pass(true) || sparse.gateReason(true) != "pass" {
		t.Errorf("有日期样本不足时不应挂 fresh: %+v reason=%s", sparse, sparse.gateReason(true))
	}

	// 臂 3: 3 条有日期且全过期 —— 真旧, 必须挂 fresh。
	stale := evaluateQualityAt(append(mkDated(3, 90*24*time.Hour), mkUndated(5)...), req, now)
	if stale.DatedResults != 3 {
		t.Fatalf("DatedResults = %d, want 3", stale.DatedResults)
	}
	if stale.Pass(true) || stale.gateReason(true) != "fresh" {
		t.Errorf("有日期样本足够且全过期必须挂 fresh: %+v reason=%s", stale, stale.gateReason(true))
	}
	// 同一批结果不要求时效时应通过。
	if !stale.Pass(false) {
		t.Errorf("不要求时效时过期结果应通过: %+v", stale)
	}

	// 臂 4: 分母口径 —— 4 条有日期 2 条新鲜, FreshRatio = 2/4 = 0.5 (不是 2/10)。
	mixed := evaluateQualityAt(append(
		append(mkDated(2, time.Hour), mkDated(2, 90*24*time.Hour)...), mkUndated(6)...), req, now)
	if mixed.DatedResults != 4 || mixed.FreshRatio != 0.5 {
		t.Errorf("FreshRatio 分母应只算有日期条数: dated=%d ratio=%v", mixed.DatedResults, mixed.FreshRatio)
	}
	if !mixed.Pass(true) {
		t.Errorf("2/4 新鲜 (0.5 >= 0.3) 应通过: %+v", mixed)
	}
}

// freshGateHint 是时效门失败时随响应返回的"下一步指令"。钉住三件事:
//  1. 提示必须带上实况数字 (dated/fresh/最新一篇的天数), 否则模型无法判断;
//  2. week 失败时建议放宽到 month, day 失败时放宽到 week (只升一档);
//  3. 必须给"窗口内无新增也是结论"的退路, 避免模型无条件重搜烧配额。
func TestFreshGateHint(t *testing.T) {
	now := time.Now()
	mk := func(n int, age time.Duration, prefix string) []Result {
		out := make([]Result, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, Result{
				URL: fmt.Sprintf("https://%s%d.example.com/post", prefix, i), Snippet: "x", Lang: "en",
				Published: now.Add(-age),
			})
		}
		return out
	}
	req := SearchRequest{Language: "en", Freshness: "week"}
	// 9 条 12 天前 + 1 条 2 天前: dated=10, fresh=1 (0.1 < 0.3) → fresh 门挂。
	results := append(mk(9, 12*24*time.Hour, "old"), mk(1, 2*24*time.Hour, "new")...)
	q := evaluateQualityAt(results, req, now)
	if q.gateReason(true) != "fresh" {
		t.Fatalf("前置: 应挂 fresh, got %s (%+v)", q.gateReason(true), q)
	}
	if q.FreshResults != 1 || q.NewestPublished.IsZero() {
		t.Fatalf("FreshResults/NewestPublished 未填充: %+v", q)
	}
	hint := freshGateHint(q, "week", now)
	for _, want := range []string{"10 条有日期", "仅 1 条", "7 天", "freshness=month", "数据缺口"} {
		if !strings.Contains(hint, want) {
			t.Errorf("week 提示缺少 %q: %s", want, hint)
		}
	}

	// day 失败只放宽一档到 week, 不跳到 month。
	reqDay := SearchRequest{Language: "en", Freshness: "day"}
	qd := evaluateQualityAt(append(mk(3, 48*time.Hour, "old"), mk(1, time.Hour, "new")...), reqDay, now)
	if qd.gateReason(true) != "fresh" {
		t.Fatalf("前置: day 应挂 fresh, got %s", qd.gateReason(true))
	}
	hintDay := freshGateHint(qd, "day", now)
	if !strings.Contains(hintDay, "freshness=week") || strings.Contains(hintDay, "freshness=month") {
		t.Errorf("day 提示应只放宽一档到 week: %s", hintDay)
	}
}
