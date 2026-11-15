package search

import "testing"

// TestPlanQueryDetectsCultureIntent 钉住中文图书/影视意图识别。
//
// 分界原则: 只命中明确的图书/影视信号 (豆瓣/书评/影评/ISBN/出版社/书单/
// 影单/藏书), 不把普通「电影」「书」这类词算进来 —— 「电影」出现在
// 大量新闻/技术语境里, 会白占一次调度名额。
func TestPlanQueryDetectsCultureIntent(t *testing.T) {
	cases := []string{
		"三体 书评",
		"流浪地球 影评",
		"ISBN 9787020002207 查询",
		"豆瓣 高分 电影 2024",
		"出版社 推荐 书单",
		"周末 影单",
	}
	for _, q := range cases {
		if got := PlanQuery(q).Intent; got != IntentCulture {
			t.Errorf("query %q intent = %s, want culture", q, got)
		}
	}
}

// TestPlanQueryDoesNotOverDetectCulture 防止日常用词把其它意图吞进
// culture —— 只调度前 2 个源, 误判会直接挤掉正确的一手来源。
func TestPlanQueryDoesNotOverDetectCulture(t *testing.T) {
	cases := []string{
		// 「电影/书」单独出现不算文化信号, 新闻/政策/学术查询不该被截走。
		"2026 春节档 电影 票房",
		"数据安全管理办法",
		"arrow 论文 reading list",
	}
	for _, q := range cases {
		if got := PlanQuery(q).Intent; got == IntentCulture {
			t.Errorf("query %q 被误判成 culture", q)
		}
	}
}

// TestCultureIntentPrefersDouban: 中文图书/影视查询首选豆瓣条目库;
// 英文查询保留 douban 在候选里但不占前两个调度位。
func TestCultureIntentPrefersDouban(t *testing.T) {
	zh := preferenceOrder(IntentCulture, "zh")
	if indexOf(zh, "douban") != 0 {
		t.Fatalf("中文 culture 首选应为 douban: %v", zh)
	}
	en := preferenceOrder(IntentCulture, "en")
	if i := indexOf(en, "douban"); i < 2 {
		t.Fatalf("英文 culture 不应让 douban 占前两位: %v", en)
	}
}

// TestNewsIntentKeepsGDELTBack: gdelt 全球监测在新闻意图里必须垫后
// (官方限 1 次/5 秒, 不能顶在前两个并发调度位), 但要在候选表里;
// weibo (中文实时舆论) 在 gdelt 之前、同样不占前两位。
func TestNewsIntentKeepsGDELTBack(t *testing.T) {
	order := preferenceOrder(IntentNews, "zh")
	i := indexOf(order, "gdelt")
	if i < 0 {
		t.Fatalf("gdelt 不在 news 候选表: %v", order)
	}
	if i < 2 {
		t.Fatalf("gdelt 不应占前两个调度位 (5s 节流吃不住并发): %v", order)
	}
	if w := indexOf(order, "weibo"); w < 0 {
		t.Fatalf("weibo 不在 news 候选表: %v", order)
	} else if w < 2 {
		t.Fatalf("weibo 不应占前两个调度位 (匿名频控): %v", order)
	} else if w > i {
		t.Fatalf("weibo (中文舆论) 应排在 gdelt 前: %v", order)
	}
	if indexOf(order, "douban") >= 0 {
		t.Fatalf("douban 不应出现在 news 候选表: %v", order)
	}
}
