package search

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/search/techdocs"
)

// TestLoadConfigEnablesProfessionalSources 钉住"专业库真的被启用"。
//
// 曾经的缺陷: router.go 的意图计划点名 arxiv/doaj/datacite/zenodo/worldbank/
// cninfo, 但 LoadConfig 没把这些名字写进 cfg.Providers —— 构造器根本没被调用,
// 计划里的名字被静默跳过。表现是学术/数据/公告类问题永远只有通用网页结果,
// 而代码看上去"路由是对的"。这条测试把"计划里有"与"真的能构造"绑在一起。
func TestLoadConfigEnablesProfessionalSources(t *testing.T) {
	t.Setenv("SEARCH_STATE_DIR", t.TempDir()) // 隔离真实 sources.json 覆盖

	cfg := LoadConfig()
	for name, wantWeight := range map[string]float64{
		"arxiv": 1.0, "doaj": 1.0, "datacite": 1.0,
		"zenodo": 0.9, "worldbank": 1.1, "cninfo": 1.0,
		"openreview": 1.0, "gov": 1.1, "techdocs": 1.0,
	} {
		p, ok := cfg.Providers[name]
		if !ok {
			t.Fatalf("%s 未登记进 cfg.Providers: 路由计划点名了它, 但构造不出来", name)
		}
		if !p.Enabled {
			t.Errorf("%s 默认未启用", name)
		}
		if p.Weight != wantWeight {
			// 权重与 provider 内置默认值不一致时, 面板显示的权重不是实际生效值。
			t.Errorf("%s 权重 = %v, want %v (须与 provider 内置默认一致)", name, p.Weight, wantWeight)
		}
	}
}

// TestProviderCatalogComplete 钉住"配置里出现的每个源都能在目录里查到分类"。
//
// 缺分类的源不会报错, 只会在面板上掉进"其他" —— 用户看到的就是一张没解释的表。
func TestProviderCatalogComplete(t *testing.T) {
	t.Setenv("SEARCH_STATE_DIR", t.TempDir())
	cfg := LoadConfig()
	for name := range cfg.Providers {
		meta, ok := ProviderCatalog[name]
		if !ok {
			t.Errorf("源 %q 没有目录元数据 (catalog.go)", name)
			continue
		}
		if meta.Category == "" || meta.CategoryName == "" || meta.Label == "" || meta.Desc == "" {
			t.Errorf("源 %q 的元数据不完整: %+v", name, meta)
		}
		if meta.Auth != "free" && meta.Auth != "key" {
			t.Errorf("源 %q 的 auth = %q, 只允许 free|key", name, meta.Auth)
		}
	}
	// 分类展示顺序必须覆盖目录里出现的每个分类, 否则新分类会被前端排到末尾。
	order := map[string]bool{}
	for _, c := range CategoryOrder {
		order[c] = true
	}
	for name, meta := range ProviderCatalog {
		if !order[meta.Category] {
			t.Errorf("源 %q 的分类 %q 不在 CategoryOrder 里", name, meta.Category)
		}
	}
}

// TestEffectiveSnapshotCarriesCatalog 钉住面板真正读到的那份文件带分类。
//
// 生效快照是面板唯一的数据源: 元数据只存在于 Go 内存里等于没做。
func TestEffectiveSnapshotCarriesCatalog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEARCH_STATE_DIR", dir)

	cfg := DefaultConfig()
	cfg.Providers["arxiv"] = ProviderConfig{Enabled: true, Egress: ModeDirect, Weight: 1.0, Tier: TierL2}
	cfg.Providers["cninfo"] = ProviderConfig{Enabled: true, Egress: ModeDirect, Weight: 1.0, Tier: TierL2}

	fake := newFake("arxiv", TierL2, func(context.Context, SearchRequest) ([]Result, error) { return nil, nil })
	WriteEffectiveSources(cfg, []Provider{fake})

	raw, err := os.ReadFile(filepath.Join(dir, "providers.json"))
	if err != nil {
		t.Fatalf("读 providers.json: %v", err)
	}
	var snap EffectiveSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("解析 providers.json: %v", err)
	}
	if len(snap.CategoryOrder) == 0 {
		t.Error("快照缺 categoryOrder")
	}
	got := map[string]EffectiveSource{}
	for _, s := range snap.Sources {
		got[s.Name] = s
	}
	arxiv, ok := got["arxiv"]
	if !ok {
		t.Fatal("快照里没有 arxiv")
	}
	if arxiv.Category != "academic" || arxiv.CategoryName != "学术论文" {
		t.Errorf("arxiv 分类 = %q/%q, want academic/学术论文", arxiv.Category, arxiv.CategoryName)
	}
	if arxiv.Desc == "" || arxiv.Label == "" || arxiv.Auth != "free" {
		t.Errorf("arxiv 元数据不完整: %+v", arxiv)
	}
	// 只有登记在册且真的构造出来的源才算 enabled —— 这里只给了 arxiv 一个实现。
	if !arxiv.Enabled || !arxiv.Healthy {
		t.Error("arxiv 应显示为已启用且健康")
	}
	if c := got["cninfo"]; c.Enabled || c.Healthy {
		t.Error("cninfo 没有对应实现, 不该显示为已启用")
	}
	if c := got["cninfo"]; c.Category != "finance" {
		t.Errorf("cninfo 分类 = %q, want finance (即使未构造也要有分类, 面板要解释它是什么)", c.Category)
	}
}

// TestEffectiveSnapshotCarriesTechDocsSites 钉住面板展开清单的数据来源:
// providers.json 必须带全部站点, 而不是前端另维护一份目录。
func TestEffectiveSnapshotCarriesTechDocsSites(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEARCH_STATE_DIR", dir)

	cfg := DefaultConfig()
	cfg.Providers["techdocs"] = ProviderConfig{Enabled: true, Egress: ModeDirect, Weight: 1.0, Tier: TierL2}
	fake := newFake("techdocs", TierL2, nil)
	WriteEffectiveSources(cfg, []Provider{fake})

	raw, err := os.ReadFile(filepath.Join(dir, "providers.json"))
	if err != nil {
		t.Fatalf("读 providers.json: %v", err)
	}
	var snap EffectiveSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("解析 providers.json: %v", err)
	}
	var tech *EffectiveSource
	for i := range snap.Sources {
		if snap.Sources[i].Name == "techdocs" {
			tech = &snap.Sources[i]
			break
		}
	}
	if tech == nil {
		t.Fatal("快照里没有 techdocs")
	}
	if tech.Category != "techdocs" || tech.CategoryName != "技术文档" {
		t.Errorf("techdocs 分类 = %q/%q", tech.Category, tech.CategoryName)
	}
	if len(tech.Sites) != len(techdocs.Sites) || len(tech.Sites) != 114 {
		t.Fatalf("sites = %d, want %d", len(tech.Sites), len(techdocs.Sites))
	}
	for _, s := range tech.Sites {
		if s.Name == "" || s.GroupName == "" || s.URL == "" {
			t.Errorf("站点元数据不完整: %+v", s)
		}
	}
}
