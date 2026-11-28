package techdocs

import (
	"net/url"
	"strings"
	"testing"
)

// TestCatalogShape 钉住目录的规模、分组与字段完整性。
//
// 目录是纯数据, 最容易出的问题是"加站时漏了域名/别名"或"分组名拼错",
// 这些在编译期都发现不了, 只会让面板或多站匹配悄悄少一块。
func TestCatalogShape(t *testing.T) {
	wantGroups := map[Group]int{
		GroupDatabase:   29,
		GroupCloud:      14,
		GroupOS:         14,
		GroupStorage:    14,
		GroupNetwork:    17,
		GroupMiddleware: 6,
		GroupCommunity:  20,
	}
	gotGroups := map[Group]int{}
	names := map[string]bool{}
	for _, s := range Sites {
		gotGroups[s.Group]++
		if s.Name == "" {
			t.Error("站点缺 Name")
		}
		if names[s.Name] {
			t.Errorf("站点名重复: %q", s.Name)
		}
		names[s.Name] = true
		if _, ok := GroupNames[s.Group]; !ok {
			t.Errorf("%s 的分组 %q 没有展示名", s.Name, s.Group)
		}
		u, err := url.Parse(s.Docs)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			t.Errorf("%s 的索引地址无效: %q", s.Name, s.Docs)
		}
		if len(s.Domains) == 0 {
			t.Errorf("%s 没有 site: 域名", s.Name)
		}
		if len(s.Aliases) == 0 {
			t.Errorf("%s 没有别名, 永远不会被匹配到", s.Name)
		}
	}
	if len(Sites) != 114 {
		t.Errorf("目录共 %d 个站, want 114", len(Sites))
	}
	for g, want := range wantGroups {
		if got := gotGroups[g]; got != want {
			t.Errorf("分组 %s = %d 个站, want %d", g, got, want)
		}
	}
	if len(GroupOrder) != len(GroupNames) {
		t.Errorf("GroupOrder 有 %d 组, GroupNames 有 %d 组", len(GroupOrder), len(GroupNames))
	}
}

// TestMatchQueryProducts 覆盖产品别名、中文名与多站 OR。
func TestMatchQueryProducts(t *testing.T) {
	cases := []struct {
		query    string
		domains  []string
		fallback bool
	}{
		{query: "postgresql vacuum 原理", domains: []string{"postgresql.org"}},
		{query: "k8s network policy", domains: []string{"kubernetes.io"}},
		{query: "阿里云 RDS 备份恢复", domains: []string{"help.aliyun.com", "docs.aws.amazon.com"}},
		{query: "kafka vs pulsar 对比", domains: []string{"kafka.apache.org", "pulsar.apache.org"}},
		{query: "达梦 dm8 兼容性", domains: []string{"dameng.com"}},
		{query: "ceph osd reweight", domains: []string{"docs.ceph.com"}},
		{query: "数据库 索引 事务", domains: []string{"postgresql.org", "dev.mysql.com", "mysql.com", "mariadb.com", "sqlite.org"}, fallback: true},
		{query: "TCP 三次握手", domains: []string{"nginx.org", "docs.haproxy.org", "envoyproxy.io", "istio.io"}, fallback: true},
	}
	for _, tc := range cases {
		m := MatchQuery(tc.query)
		if !m.Matched() {
			t.Errorf("%q 没有匹配到任何域名", tc.query)
			continue
		}
		if m.Fallback != tc.fallback {
			t.Errorf("%q fallback = %v, want %v", tc.query, m.Fallback, tc.fallback)
		}
		for _, want := range tc.domains {
			if !contains(m.Domains, want) {
				t.Errorf("%q domains = %v, 缺少 %q", tc.query, m.Domains, want)
			}
		}
	}
}

// TestMatchQueryCapsDomains 钉住域名上限: 一次带太多 site: 会摊薄关键词权重。
func TestMatchQueryCapsDomains(t *testing.T) {
	m := MatchQuery("postgresql mysql mongodb redis clickhouse cassandra neo4j tidb")
	if len(m.Domains) > MaxDomains {
		t.Fatalf("domains = %d, 超过上限 %d", len(m.Domains), MaxDomains)
	}
	seen := map[string]bool{}
	for _, d := range m.Domains {
		if seen[d] {
			t.Errorf("域名重复: %s", d)
		}
		seen[d] = true
	}
}

// TestMatchQueryIgnoresNonTechnical 钉住"不像技术问题就返回空"。
func TestMatchQueryIgnoresNonTechnical(t *testing.T) {
	for _, q := range []string{"今天天气怎么样", "苹果公司最新财报", "红楼梦 作者"} {
		if m := MatchQuery(q); m.Matched() {
			t.Errorf("%q 不该命中技术文档: %v", q, m.Domains)
		}
	}
}

// TestMatchesAliasWordBoundary 钉住 ASCII 别名按词边界匹配。
func TestMatchesAliasWordBoundary(t *testing.T) {
	cases := []struct {
		q, alias string
		want     bool
	}{
		{"records management", "rds", false},
		{"使用redis缓存", "redis", true},
		{"rds 慢查询", "rds", true},
		{"s3://bucket/path", "s3", true},
		{"k8s集群", "k8s", true},
		{"redis-cluster", "redis", true},
	}
	for _, tc := range cases {
		if got := matchesAlias(tc.q, tc.alias); got != tc.want {
			t.Errorf("matchesAlias(%q, %q) = %v, want %v", tc.q, tc.alias, got, tc.want)
		}
	}
}

// TestGroupNamesCoverSites 钉住每个站的分组都有中文展示名。
func TestGroupNamesCoverSites(t *testing.T) {
	for _, s := range Sites {
		name := GroupNames[s.Group]
		if strings.TrimSpace(name) == "" {
			t.Errorf("%s 的分组 %q 没有展示名", s.Name, s.Group)
		}
	}
}

// TestMatchQueryLangFallback 钉住组兜底的语言取向。
//
// 事故背景 (2026-11-28): 英文查询 "... engineering blog November 2026" 命中
// GroupCommunity 的 "engineering blog", 兜底按目录顺序取 4 个站, 其中 InfoQ
// 中文与掘金吃掉两个名额, Exa 严格按 site: 返回 → 英文问题拿到 10 条中文转载,
// 域名数 2, 质量门判 domains 不过。
//
// 三个臂:
//  1. 英文查询的兜底名单里不能出现标了 zh 的站;
//  2. 中文查询的兜底仍然先拿中文站 (不能为了修英文把中文查询弄坏);
//  3. 用户在 query 里点名站点时, 语言不参与判断 (那是用户自己的选择)。
func TestMatchQueryLangFallback(t *testing.T) {
	zhDomains := map[string]bool{}
	for _, s := range Sites {
		if s.Lang != "zh" {
			continue
		}
		for _, d := range s.Domains {
			zhDomains[d] = true
		}
	}
	if len(zhDomains) == 0 {
		t.Fatal("前置: 目录里应有标了 Lang=zh 的站")
	}

	const q = "context engineering agent harness engineering blog November 2026"
	en := MatchQueryLang(q, "en")
	if !en.Matched() {
		t.Fatalf("英文社区查询应命中组兜底: %+v", en)
	}
	for _, d := range en.Domains {
		if zhDomains[d] {
			t.Errorf("英文查询的兜底名单不该包含中文站 %q: %v", d, en.Domains)
		}
	}

	zh := MatchQueryLang("架构设计 工程实践 踩坑复盘", "zh")
	if !zh.Matched() {
		t.Fatalf("中文社区查询应命中组兜底: %+v", zh)
	}
	var firstZh bool
	for _, d := range zh.Domains {
		if zhDomains[d] {
			firstZh = true
			break
		}
	}
	if !firstZh {
		t.Errorf("中文查询应优先拿中文站: %v", zh.Domains)
	}

	// 显式点名: 英文 lang 下问掘金, 别名命中优先于语言取向。
	named := MatchQueryLang("juejin 上关于 agent harness 的文章", "en")
	if !contains(named.Domains, "juejin.cn") {
		t.Errorf("点名站点不应被语言过滤掉: %v", named.Domains)
	}
}

// contains 判断字符串切片是否含目标值。
func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
