package search

import "testing"

// TestPlanQueryRoutesTechDocs 钉住技术文档意图的优先级边界:
//   - 具体产品名/组信号 -> IntentTech;
//   - 论文/政策/统计/代码/新闻信号仍然优先, 不被文档源顶掉;
//   - 带年份的版本查询保留 year 级时效, 但不是 week。
func TestPlanQueryRoutesTechDocs(t *testing.T) {
	cases := []struct {
		query  string
		intent Intent
		fresh  string
	}{
		{query: "postgresql vacuum 原理", intent: IntentTech},
		{query: "数据库 索引 事务", intent: IntentTech},
		{query: "kubernetes 1.34 release date 2025", intent: IntentTech, fresh: "year"},
		{query: "kafka vs pulsar 对比", intent: IntentTech},
		{query: "数据库市场规模", intent: IntentData},
		{query: "postgres 论文", intent: IntentAcademic},
		{query: "golang panic: nil map", intent: IntentCode},
		{query: "苹果公司最新财报", intent: IntentData},
		{query: "今天有什么新闻", intent: IntentNews, fresh: "week"},
	}
	for _, tc := range cases {
		plan := PlanQuery(tc.query)
		if plan.Intent != tc.intent {
			t.Errorf("%q intent = %s, want %s", tc.query, plan.Intent, tc.intent)
		}
		if tc.fresh != "" && plan.Freshness != tc.fresh {
			t.Errorf("%q freshness = %s, want %s", tc.query, plan.Freshness, tc.fresh)
		}
	}

	for _, lang := range []string{"zh", "en"} {
		order := preferenceOrder(IntentTech, lang)
		if len(order) == 0 || order[0] != "techdocs" {
			t.Errorf("IntentTech(%s) 首位 = %v, want techdocs", lang, order)
		}
	}
}

// TestSortProvidersTechFirst 钉住 IntentTech 的候选排序: techdocs 必须排在
// 通用源之前 —— 单次检索只调度前 2 个源, 排在第三位等于永远不查。
func TestSortProvidersTechFirst(t *testing.T) {
	cfg := DefaultConfig()
	providers := []Provider{
		newFake("parallel", TierL0, nil),
		newFake("techdocs", TierL2, nil),
		newFake("exa_anon", TierL0, nil),
	}
	sortProviders(providers, QueryPlan{Intent: IntentTech, Language: "en"}, cfg)
	if got := providers[0].Name(); got != "techdocs" {
		t.Fatalf("首位 = %s, want techdocs (order: %v)", got, names(providers))
	}
}

// names 提取 provider 名称, 失败信息用。
func names(providers []Provider) []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		out = append(out, p.Name())
	}
	return out
}
