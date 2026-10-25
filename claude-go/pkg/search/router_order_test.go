// router_order_test.go —— 偏好顺序的回归。
//
// 只调度前 2 个源, 所以"谁排第二"直接决定主源被限流时整轮调研的质量。
// 2026-10-25 的固态电池任务里 parallel 被并发打限流, 旧顺序让 bing_rss +
// gnews 顶上: 26 次搜索只换回 4 个可打开页面。exa_anon 免 key、直连可用,
// 必须排在兜底源前面。
package search

import "testing"

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
