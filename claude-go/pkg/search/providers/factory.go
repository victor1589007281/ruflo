package providers

import (
	"sort"

	"github.com/anthropic/claude-go/pkg/search"
)

// providerCtor 是单源构造函数。
type providerCtor func(search.Config, search.ProviderConfig, *search.Egress) (search.Provider, error)

// registry 把配置键映射到构造函数。
//
// 只登记本包实现的无 key / 免鉴权源; 付费源不在此表, 未配置 key 时
// 自然不会被构造。
var registry = map[string]providerCtor{
	"parallel":      newParallel,
	"exa_anon":      newExa,
	"bing_rss":      newBingRSS,
	"wikipedia":     newWikipedia,
	"gnews":         newGNews,
	"github":        newGitHub,
	"stackexchange": newStackExchange,
	"openalex":      newOpenAlex,
	"crossref":      newCrossref,
	"pubmed":        newPubMed,
	"hn":            newHN,
	"npm":           newNPM,
	"pypi":          newPyPI,
}

// requiresAPIKey 列出没有 key 就不应构造的付费源。
var requiresAPIKey = map[string]bool{
	"exa":       true,
	"tavily":    true,
	"firecrawl": true,
	"jina":      true,
	"linkup":    true,
	"serper":    true,
	"brave":     true,
}

// Default 按配置构造检索源。
//
// 只返回 cfg.Providers 中同时满足 Enabled=true、Disabled=false 且构造成功的源;
// 没有 key 的付费源与未知名字都被跳过。返回顺序按配置键排序, 保证可复现。
func Default(cfg search.Config, eg *search.Egress) []search.Provider {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]search.Provider, 0, len(names))
	for _, name := range names {
		pc := cfg.Providers[name]
		if !pc.Enabled || pc.Disabled {
			continue
		}
		if requiresAPIKey[name] && pc.APIKey == "" {
			continue
		}
		ctor, ok := registry[name]
		if !ok {
			continue
		}
		p, err := ctor(cfg, pc, eg)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}
