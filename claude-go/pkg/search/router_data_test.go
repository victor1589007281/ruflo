package search

import "testing"

// TestPlanQueryDetectsDataIntent 钉住统计/年报意图识别。
//
// 这类查询必须和 news 分开: 带年份的「统计公报」若被判成 news，会被强制
// week 级新鲜度，年度公报反而过不了质量门；而它也不该走 academic 的
// 论文源。专业数据源（巨潮/世界银行）只有在 data 意图下才会排到第一。
func TestPlanQueryDetectsDataIntent(t *testing.T) {
	cases := []string{
		"2024 年国民经济和社会发展统计公报",
		"中国 2025 年 GDP 增速与 CPI 走势",
		"新能源汽车行业报告 市场规模",
		"某上市公司 2025 年年报 营收",
		"比亚迪 2025 年年度报告 营收",
		"宁德时代 2026 年半年度报告 净利润",
		"world bank annual report on inflation",
		"US census unemployment statistics by state",
		"global market size of database systems dataset",
	}
	for _, q := range cases {
		if got := PlanQuery(q).Intent; got != IntentData {
			t.Errorf("query %q intent = %s, want data", q, got)
		}
	}
}

// TestPlanQueryDoesNotOverDetectData 防止「数据」类词把普通技术查询吞掉。
func TestPlanQueryDoesNotOverDetectData(t *testing.T) {
	cases := []string{
		"数据库内核研发工程师的出路",
		"GDPR 合规对日志存储的要求",
		"capital allocation in agent companies",
		"golang panic: nil map",
		"kubernetes 1.34 release date 2025",
	}
	for _, q := range cases {
		if got := PlanQuery(q).Intent; got == IntentData {
			t.Errorf("query %q 被误判成 data", q)
		}
	}
}

// TestDataIntentPrefersProfessionalSources: 中文 data 查询先巨潮，
// 英文先世界银行，通用源仍然保底。
func TestDataIntentPrefersProfessionalSources(t *testing.T) {
	zh := preferenceOrder(IntentData, "zh")
	if indexOf(zh, "cninfo") != 0 {
		t.Fatalf("中文 data 首选应为 cninfo: %v", zh)
	}
	en := preferenceOrder(IntentData, "en")
	if indexOf(en, "worldbank") != 0 {
		t.Fatalf("英文 data 首选应为 worldbank: %v", en)
	}
	for _, name := range []string{"exa_anon", "datacite", "doaj"} {
		if indexOf(zh, name) < 0 || indexOf(en, name) < 0 {
			t.Errorf("data 偏好表缺少 %s: zh=%v en=%v", name, zh, en)
		}
	}
}

// TestIntentPriorityOverridesGlobalPriority: 意图级优先级优先于全局列表,
// 且按 "intent:lang" -> "intent" 回落。
func TestIntentPriorityOverridesGlobalPriority(t *testing.T) {
	a := newFake("exa_anon", TierL0, nil)
	b := newFake("cninfo", TierL2, nil)
	c := newFake("worldbank", TierL2, nil)
	cfg := DefaultConfig()
	cfg.ProviderPriority = []string{"exa_anon", "cninfo", "worldbank"}
	cfg.IntentPriority = map[string][]string{
		"data:zh": {"cninfo", "exa_anon"},
		"data":    {"worldbank", "exa_anon"},
	}

	got, _ := selectProviders([]Provider{a, b, c}, PlanQuery("2025 年统计公报"), cfg, nil, nil)
	if got[0].Name() != "cninfo" {
		t.Fatalf("data:zh 未生效: %v", namesOf(got))
	}

	got, _ = selectProviders([]Provider{a, b, c}, PlanQuery("world bank annual report"), cfg, nil, nil)
	if got[0].Name() != "worldbank" {
		t.Fatalf("data 未生效: %v", namesOf(got))
	}

	// 非 data 意图回落到全局 ProviderPriority。
	got, _ = selectProviders([]Provider{a, b, c}, PlanQuery("什么是向量数据库"), cfg, nil, nil)
	if got[0].Name() != "exa_anon" {
		t.Fatalf("全局优先级未生效: %v", namesOf(got))
	}
}
