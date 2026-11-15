package providers

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/search"
)

// TestRegistryHasCatalogEntry 钉住"实现与目录不漂移"。
//
// 目录表在 pkg/search (那里写生效快照), 注册表在这里。新增一个源却忘了登记分类
// 不会编译失败, 只会在面板上掉进"其他" —— 这条测试让漏登记当场变红。
func TestRegistryHasCatalogEntry(t *testing.T) {
	for name := range registry {
		meta, ok := search.ProviderCatalog[name]
		if !ok {
			t.Errorf("注册表里的源 %q 在 search.ProviderCatalog 中没有条目", name)
			continue
		}
		if meta.Category == "" {
			t.Errorf("源 %q 的目录条目缺分类", name)
		}
	}
	// 注: requiresAPIKey 里还有 firecrawl/jina/linkup/serper/brave —— 本包没有
	// 它们的构造器, 也不会出现在 cfg.Providers 里, 所以不要求目录条目。
}

// TestRegistryRegisteredInLoadConfig 钉住注册表的另一端: 有构造函数的源,
// LoadConfig 必须把它写进 cfg.Providers 并启用。
//
// 原缺陷正是这一端断了: router 的意图计划点名 arxiv/doaj/datacite/zenodo/
// worldbank/cninfo, registry 里也有构造函数, 但 LoadConfig 没登记, Default()
// 遍历 cfg.Providers 时根本碰不到它们 —— 计划里的名字被静默跳过。表现是
// 学术/数据类查询永远只有通用网页结果, 而单测只测构造函数时全绿。
//
// 这条测试放在 providers 包而不是 search 包, 是为了直接遍历 registry;
// search 包 import 本包会成环。
func TestRegistryRegisteredInLoadConfig(t *testing.T) {
	t.Setenv("SEARCH_STATE_DIR", t.TempDir()) // 隔离真实 sources.json 的启停覆盖

	cfg := search.LoadConfig()
	for name := range registry {
		pc, ok := cfg.Providers[name]
		if !ok {
			t.Errorf("注册表里的源 %q 未登记进 cfg.Providers: 构造器永远不会被调用", name)
			continue
		}
		if !pc.Enabled {
			t.Errorf("注册表里的源 %q 登记了但默认未启用", name)
		}
	}
}
