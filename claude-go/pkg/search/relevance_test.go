package search

import "testing"

func TestFilterRelevantDropsOffTopicFallbackResults(t *testing.T) {
	results := []Result{
		{URL: "https://mem0.ai/blog/memory-layer", Title: "Mem0 memory layer for LLM agents", Snippet: "benchmark results"},
		{URL: "https://yandex.com/", Title: "Yandex", Snippet: "search engine"},
		{URL: "https://www.huya.com/", Title: "虎牙直播", Snippet: "游戏直播平台"},
	}
	got := FilterRelevant(results, "memory layer for LLM agents benchmark")
	if len(got) != 1 || got[0].URL != results[0].URL {
		t.Fatalf("只应保留相关结果, got %v", got)
	}
}

// TestFilterRelevantChineseBigrams 钉住中文 bigram: 中文查询没有空格,
// 把 "贵州茅台2026年半年报" 整段当一个 token 会永远匹配不上。
func TestFilterRelevantChineseBigrams(t *testing.T) {
	results := []Result{
		{URL: "https://www.cninfo.com.cn/a", Title: "贵州茅台2026年半年度报告", Snippet: "营业收入与批价"},
		{URL: "https://www.huya.com/", Title: "虎牙直播", Snippet: "游戏直播"},
		{URL: "https://news.example/a", Title: "白酒行业周报", Snippet: "渠道库存与批价"},
	}
	got := FilterRelevant(results, "贵州茅台 2026 半年报 批价 渠道库存")
	if len(got) != 2 {
		t.Fatalf("应保留茅台财报与白酒周报, got %v", got)
	}
	for _, r := range got {
		if r.URL == "https://www.huya.com/" {
			t.Fatal("直播站不应命中白酒调研查询")
		}
	}
}

func TestFilterRelevantNumericOnlyQueryIsNotFiltered(t *testing.T) {
	results := []Result{
		{URL: "https://a.example/1", Title: "anything"},
		{URL: "https://b.example/2", Title: "something else"},
	}
	// 纯数字查询没有内容词: 过滤没有判据, 必须原样放行.
	if got := FilterRelevant(results, "2026"); len(got) != len(results) {
		t.Fatalf("纯数字查询不应过滤, got %d/%d", len(got), len(results))
	}
	if got := FilterRelevant(results, ""); len(got) != len(results) {
		t.Fatalf("空查询不应过滤, got %d/%d", len(got), len(results))
	}
}

// TestFilterRelevantLongQueryThreshold 长查询要求覆盖 25%:
// 只命中一个词的结果可能是擦边, 不足以证明同一话题。
func TestFilterRelevantLongQueryThreshold(t *testing.T) {
	results := []Result{
		{URL: "https://a.example/1", Title: "kubernetes", Snippet: "minor fixes"},
		{URL: "https://b.example/2", Title: "kubernetes release notes", Snippet: "scheduler and ingress changes"},
	}
	got := FilterRelevant(results, "kubernetes release notes scheduler ingress autoscaling")
	if len(got) != 1 || got[0].URL != "https://b.example/2" {
		t.Fatalf("8 词查询应要求 >=2 个内容词, got %v", got)
	}
}
