package search

import "testing"

// TestPlanQueryDetectsPolicyIntent 钉住政策/政府公告意图识别。
//
// 这类查询与 data 的分界不是"有没有数字", 而是"要的是文件原文还是统计口径":
// 「数据安全管理办法」含 data 信号词「数据」, 但正确的一手来源是国务院
// 政策文件库, 判成 data 会让巨潮 (上市公司年报) 顶上来。
func TestPlanQueryDetectsPolicyIntent(t *testing.T) {
	cases := []string{
		"数据安全管理办法 全文",
		"新能源汽车补贴政策 2026",
		"国务院关于人工智能的指导意见",
		"生成式人工智能服务管理暂行办法",
		"某地 行政审批 改革 实施方案",
		"EU AI regulation 2026 requirements",
	}
	for _, q := range cases {
		if got := PlanQuery(q).Intent; got != IntentPolicy {
			t.Errorf("query %q intent = %s, want policy", q, got)
		}
	}
}

// TestPlanQueryDoesNotOverDetectPolicy 防止「通知/规定/意见」这类日常词
// 把技术查询吞进政策意图 —— 那会白占一次检索名额 (只调度前 2 个源)。
func TestPlanQueryDoesNotOverDetectPolicy(t *testing.T) {
	cases := []string{
		"GitHub Actions 通知 配置",
		"golang 接口规范 命名",
		"数据库内核研发工程师的出路",
		"kubernetes 1.34 release date 2025",
		"golang panic: nil map",
	}
	for _, q := range cases {
		if got := PlanQuery(q).Intent; got == IntentPolicy {
			t.Errorf("query %q 被误判成 policy", q)
		}
	}
}

// TestPolicyIntentPrefersGov: 中文政策查询首选国务院政策文件库,
// 英文查询不能把只有中文文件的 gov 排在第一位。
func TestPolicyIntentPrefersGov(t *testing.T) {
	zh := preferenceOrder(IntentPolicy, "zh")
	if indexOf(zh, "gov") != 0 {
		t.Fatalf("中文 policy 首选应为 gov: %v", zh)
	}
	if indexOf(zh, "exa_anon") != 1 {
		t.Fatalf("中文 policy 第二位应为 exa_anon (政策解读仍在门户页): %v", zh)
	}
	en := preferenceOrder(IntentPolicy, "en")
	if indexOf(en, "gov") == 0 {
		t.Fatalf("英文 policy 不应首选 gov (只收中文文件): %v", en)
	}
	if indexOf(en, "worldbank") != 0 {
		t.Fatalf("英文 policy 首选应为 worldbank: %v", en)
	}
}
