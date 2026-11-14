package search

import (
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
