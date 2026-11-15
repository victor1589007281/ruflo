package providers

// factory.go —— 配置键到 provider 构造函数的注册表。
//
// 只登记本包实现的无 key / 免鉴权源; 付费源 (exa/tavily/firecrawl/jina/...)
// 必须在配置里给了 APIKey 才会被构造。构造顺序按配置键排序, 保证每次启动的
// 源列表一致 —— 否则路由排序在 tier/权重相同时会出现不可复现的名次漂移。
import (
	"log/slog"
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
	"techdocs":      newTechDocs, // 官方文档 + 工程社区, 按产品名限定域名
	"openalex":      newOpenAlex,
	"crossref":      newCrossref,
	"pubmed":        newPubMed,
	"hn":            newHN,
	"npm":           newNPM,
	"pypi":          newPyPI,

	// 专业公开数据源（免 key）。都只做检索，不做抓取；频率由 base.pace 控制。
	"worldbank":  newWorldBank,  // 世界银行文档与年报
	"datacite":   newDataCite,   // 研究数据集 / 机构报告 DOI
	"zenodo":     newZenodo,     // CERN 开放获取记录
	"doaj":       newDOAJ,       // 开放获取期刊
	"arxiv":      newArxiv,      // 预印本
	"openreview": newOpenReview, // 会议投稿（含 DBLP 索引）
	"cninfo":     newCNInfo,     // 巨潮资讯：A 股年报与公告
	"gov":        newGov,        // 中国政府网：国务院政策文件库（政策/法规/部委文件）

	// 新闻与文化生态（免 key）。gdelt 官方限 1 次/5 秒（构造器内置节流）;
	// douban 只命中文化意图 (router 的 IntentCulture), 平时不参与调度;
	// weibo 走 m.weibo.cn 匿名搜索 (浏览器形态 UA, 2026-11 实测), 匿名有
	// 频控无公开配额, 连续失败由 base 熔断兜底。
	"gdelt":  newGDELT,
	"douban": newDouban,
	"weibo":  newWeibo,
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
			// 配置启用、目录也登记了, 却没有构造函数 —— 这是"看上去可用、
			// 实际永远不调用"的死路径。以前这里静默 continue, 只能靠人工
			// 对比配置/注册表才发现; 现在至少让部署日志说清楚。
			// 只对目录里的源告警: 调用方可能自行追加注册表之外的源
			// (如 AskForge 的 so_html/bing_html), 那些由调用方装配。
			if meta, advertised := search.ProviderCatalog[name]; advertised {
				slog.Warn("search: source is enabled but has no implementation in this build; skipped",
					"provider", name, "label", meta.Label, "auth", meta.Auth)
			}
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
