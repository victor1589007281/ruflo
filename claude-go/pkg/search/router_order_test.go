// router_order_test.go —— 偏好顺序的回归。
//
// 只调度前 2 个源, 所以"谁排第二"直接决定主源被限流时整轮调研的质量。
// 2026-10-25 的固态电池任务里 parallel 被并发打限流, 旧顺序让 bing_rss +
// gnews 顶上: 26 次搜索只换回 4 个可打开页面。exa_anon 免 key、直连可用,
// 必须排在兜底源前面。
package search

import (
	"strings"
	"testing"
)

func indexOf(order []string, name string) int {
	for i, n := range order {
		if n == name {
			return i
		}
	}
	return -1
}

func TestNewsPrefersExaAnonOverFallbacks(t *testing.T) {
	for _, lang := range []string{"zh", "en"} {
		order := preferenceOrder(IntentNews, lang)
		ia := indexOf(order, "exa_anon")
		ib := indexOf(order, "bing_rss")
		ig := indexOf(order, "gnews")
		if ia < 0 {
			t.Fatalf("%s news 偏好表缺少 exa_anon: %v", lang, order)
		}
		if ia > ib || (ig >= 0 && ia > ig) {
			t.Fatalf("%s news 仍让兜底源排在 exa_anon 前面: %v", lang, order)
		}
	}
}

func TestZhFactualHasNoPhantomProviders(t *testing.T) {
	order := preferenceOrder(IntentFactual, "zh")
	if indexOf(order, "baidu") >= 0 || indexOf(order, "sogou") >= 0 {
		t.Fatalf("偏好表包含从未实现的源: %v", order)
	}
	ia := indexOf(order, "exa_anon")
	iw := indexOf(order, "wikipedia")
	if ia < 0 || ia > iw {
		t.Fatalf("中文事实类应先试 exa_anon 再退 wikipedia: %v", order)
	}
}

// TestEcosystemTokensScopeDomains 钉住生态品牌词推导:
// 搜狗微信 / 头条搜索的 robots 禁止抓取, 生产上走通用源 + 域名限定
// (2026-11 实测 exa_anon 对 mp.weixin.qq.com / toutiao.com 召回有效),
// 所以"微信/头条生态文章"类查询必须由规划层自动限定域名。
func TestEcosystemTokensScopeDomains(t *testing.T) {
	cases := []struct {
		query string
		want  []string
	}{
		{"人工智能 微信公众号文章", []string{"mp.weixin.qq.com"}},
		{"微信文章 大模型", []string{"mp.weixin.qq.com"}},
		{"公众号文章 推荐", []string{"mp.weixin.qq.com"}},
		{"site:mp.weixin.qq.com 机器学习", []string{"mp.weixin.qq.com"}},
		{"今日头条 开源项目", []string{"toutiao.com"}},
		{"头条号 测评", []string{"toutiao.com"}},
		{"头条文章 总结", []string{"toutiao.com"}},
		{"对比微信公众号和头条号", []string{"mp.weixin.qq.com", "toutiao.com"}},
	}
	for _, tc := range cases {
		plan := PlanQuery(tc.query)
		if strings.Join(plan.ScopedDomains, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("query %q: ScopedDomains=%v, want %v", tc.query, plan.ScopedDomains, tc.want)
		}
	}
}

// TestEcosystemBrandDoesNotTriggerNewsFreshness 是误触发回归:
// "今日头条" 里的 "今日" 不能再把生态查询推进 news+week 时效分支
// (week 新鲜度会让几个月前的生态文章过不了质量门的 fresh 判据);
// 而真正的时效词 ("最新") 与普通 "今日" 说法必须照旧生效。
func TestEcosystemBrandDoesNotTriggerNewsFreshness(t *testing.T) {
	eco := PlanQuery("人工智能 今日头条文章")
	if eco.Intent == IntentNews || eco.Freshness != "any" {
		t.Fatalf("品牌词误触发新闻时效: intent=%s freshness=%s", eco.Intent, eco.Freshness)
	}
	fresh := PlanQuery("今日头条最新文章")
	if fresh.Intent != IntentNews || fresh.Freshness != "week" {
		t.Fatalf("真实时效词应照旧触发 news+week: intent=%s freshness=%s", fresh.Intent, fresh.Freshness)
	}
	if got := PlanQuery("今天有什么新闻").Freshness; got != "week" {
		t.Fatalf("普通今日说法应照旧触发 week, got %s", got)
	}
}

// TestNonEcosystemQueriesStayUnscoped 防止限定范围蔓延:
// 不含品牌词的普通查询绝不能被加上域名限定。
func TestNonEcosystemQueriesStayUnscoped(t *testing.T) {
	for _, q := range []string{"今天有什么新闻", "kubernetes 1.34 release date 2025", "golang panic: nil map", "苹果公司"} {
		if plan := PlanQuery(q); len(plan.ScopedDomains) != 0 {
			t.Fatalf("query %q 不应有域名限定, got %v", q, plan.ScopedDomains)
		}
	}
}

// TestProviderPriorityOverridesIntentOrder 钉住调用方覆盖开关:
// 产品侧一次取证会连发 15+ 条检索, Parallel 免费层第 3 次就限流,
// 固定偏好表会把主源永远钉在限流源上。显式优先级必须生效, 且表外源
// 不能被排除。
func TestProviderPriorityOverridesIntentOrder(t *testing.T) {
	probe := newFake("exa_anon", TierL0, nil)
	other := newFake("parallel", TierL0, nil)
	third := newFake("so_html", TierL2, nil)
	cfg := DefaultConfig()
	cfg.ProviderPriority = []string{"exa_anon", "so_html"}
	got, _ := selectProviders([]Provider{probe, other, third}, PlanQuery("中文 查询"), cfg, nil, nil)
	if len(got) != 3 {
		t.Fatalf("表外源被排除: %v", namesOf(got))
	}
	if got[0].Name() != "exa_anon" || got[1].Name() != "so_html" {
		t.Fatalf("自定义优先级未生效: %v", namesOf(got))
	}
	if got[2].Name() != "parallel" {
		t.Fatalf("表外源应排在最后: %v", namesOf(got))
	}
}

func namesOf(ps []Provider) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name())
	}
	return out
}
