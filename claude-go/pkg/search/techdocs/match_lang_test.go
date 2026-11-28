package techdocs

import "testing"

// TestMatchQueryLangProductionQueries 用 2026-11-28 生产里真实挂掉的三条英文查询
// 钉住修复效果。
//
// 这三条在旧实现下都被兜底到 [lwn.net phoronix.com infoq.cn juejin.cn], 实际
// 召回是 10 条中文转载、域名数 2, 质量门判 domains 不过 —— 门没错, 是兜底把
// 英文问题指向了中文站。断言比 TestMatchQueryLangFallback 更贴生产: 用的是
// 当天原样的 query 串。
func TestMatchQueryLangProductionQueries(t *testing.T) {
	zhDomains := map[string]bool{}
	for _, s := range Sites {
		if s.Lang != "zh" {
			continue
		}
		for _, d := range s.Domains {
			zhDomains[d] = true
		}
	}

	queries := []string{
		"context engineering agent harness engineering blog November 2026",
		"engineering blog post Nov 24 2026 OR Nov 25 2026",
		"engineering blog production agent lessons learned context engineering",
	}
	for _, q := range queries {
		m := MatchQueryLang(q, "en")
		if !m.Matched() || !m.Fallback {
			t.Fatalf("%q 应走组兜底: %+v", q, m)
		}
		for _, d := range m.Domains {
			if zhDomains[d] {
				t.Errorf("%q 的兜底名单混进中文站 %q: %v", q, d, m.Domains)
			}
		}
		if len(m.Domains) < MaxFallbackSites {
			t.Errorf("%q 兜底名额没填满 (%d/%d): %v", q, len(m.Domains), MaxFallbackSites, m.Domains)
		}
	}
}
