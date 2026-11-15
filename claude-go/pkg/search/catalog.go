package search

// catalog.go —— 检索源的**分类目录** (面板按分类分组展示数据源)。
//
// 为什么单独一张表而不是每个 provider 自带: 生效快照 providers.json 由本包写出,
// 而 provider 实现包 (pkg/search/providers) 反过来 import 本包 —— 元数据若写在
// 实现包里就进不了快照。把"名字 → 分类/用途"集中在这里, 与本包 config.go 决定
// 启用哪些源的代码同处一地, 新增源时两处一起改; providers 包有一条测试比对
// 注册表与本表, 漏登记会直接失败 (而不是在面板上悄悄掉进"其他")。
//
// 分类的划分口径是**问题类型**, 不是数据格式: 用户想知道"这个问题该去哪个专业库",
// 而不是"这是 JSON 还是 XML"。所以学术论文与开放数据集分开, 财经公告单列。
// Category 是稳定 slug (前端按它排序/上色), CategoryName 是展示名 —— 只发一次
// 中文名, 免得前端再维护一份翻译表。

import "github.com/anthropic/claude-go/pkg/search/techdocs"

// SiteMeta 是聚合源收录的单个文档站 (目前只有 techdocs 使用)。
//
// 放在快照里的意义: 面板要能展开看到"这个源到底覆盖哪些站", 而不是只显示
// 一个「114 个站」的数字。数据来自 techdocs 目录, 不在前端再维护一份。
type SiteMeta struct {
	Name      string   `json:"name"`
	Group     string   `json:"group"`
	GroupName string   `json:"groupName"`
	URL       string   `json:"url"`
	Domains   []string `json:"domains,omitempty"`
}

// ProviderMeta 描述一个检索源的分类与用途。
type ProviderMeta struct {
	Category     string     `json:"category"`     // 稳定 slug: general/social/code/techdocs/academic/data/finance/gov/reference
	CategoryName string     `json:"categoryName"` // 展示名
	Label        string     `json:"label"`        // 源的可读名 (面板第一列)
	Desc         string     `json:"desc"`         // 一句话: 收录什么、适合什么问题
	Auth         string     `json:"auth"`         // free=免 key | key=需 API Key
	Sites        []SiteMeta `json:"sites,omitempty"`
}

// CategoryOrder 是面板分组的展示顺序: 通用在前 (最常用), 社媒与专业库按认知热度排。
var CategoryOrder = []string{"general", "social", "academic", "data", "finance", "gov", "code", "techdocs", "reference"}

// ProviderCatalog 是全部已实现检索源的元数据表。
//
// 只列 providers 注册表里真实存在的源 (加上待配 key 的付费源): 面板把"配置里
// 有但构造失败"的源也展示出来, 并标成未生效 —— 这比隐藏它更有用。
var ProviderCatalog = map[string]ProviderMeta{
	// ── 通用网页检索 ──
	"parallel": {Category: "general", CategoryName: "通用网页", Label: "Parallel", Auth: "free",
		Desc: "通用网页检索 (免费匿名 MCP 端点), 中英文覆盖均衡, 是默认主力源"},
	"exa_anon": {Category: "general", CategoryName: "通用网页", Label: "Exa 匿名", Auth: "free",
		Desc: "Exa 的免 key 端点, 语义相似度检索偏强, 适合概念型查询"},
	"exa": {Category: "general", CategoryName: "通用网页", Label: "Exa", Auth: "key",
		Desc: "神经/语义检索, 需要 EXA_API_KEY"},
	"tavily": {Category: "general", CategoryName: "通用网页", Label: "Tavily", Auth: "key",
		Desc: "面向 AI 的检索 API, 需要 TAVILY_API_KEY"},
	"bing_rss": {Category: "general", CategoryName: "通用网页", Label: "Bing RSS", Auth: "free",
		Desc: "必应结果的 RSS 入口, 补充通用源漏掉的长尾网页"},
	"gnews": {Category: "general", CategoryName: "通用网页", Label: "Google News", Auth: "free",
		Desc: "新闻时效检索 (RSS, 走代理出口), 查「最近发生了什么」"},
	"gdelt": {Category: "general", CategoryName: "通用网页", Label: "GDELT", Auth: "free",
		Desc: "全球新闻与广播监测 (100+ 语言, 小时级), 补非英语/跨境新闻; 官方限 1 次/5 秒 (内置节流)"},

	// ── 中文社媒生态 ──
	"douban": {Category: "social", CategoryName: "社媒生态", Label: "豆瓣", Auth: "free",
		Desc: "中文图书/电影条目检索, 命中文化类问题时排最前; 评分与评论在条目页由抓取直读"},
	"weibo": {Category: "social", CategoryName: "社媒生态", Label: "微博", Auth: "free",
		Desc: "m.weibo.cn 匿名搜索: 中文实时舆论/热搜/博文讨论, 新闻意图候选中段调度; 评论与转发明细在详情页由抓取直读"},

	// ── 学术论文与预印本 ──
	"openalex": {Category: "academic", CategoryName: "学术论文", Label: "OpenAlex", Auth: "free",
		Desc: "跨学科论文元数据: 作者/机构/引用关系, 适合找人找脉络"},
	"crossref": {Category: "academic", CategoryName: "学术论文", Label: "Crossref", Auth: "free",
		Desc: "DOI 注册库, 期刊论文的权威元数据 (标题/作者/卷期)"},
	"pubmed": {Category: "academic", CategoryName: "学术论文", Label: "PubMed", Auth: "free",
		Desc: "生物医学文献, 含摘要与 MeSH 主题词"},
	"arxiv": {Category: "academic", CategoryName: "学术论文", Label: "arXiv", Auth: "free",
		Desc: "预印本原始版本, 查最新研究首选; 官方条款要求 3 秒/次 (内置节流)"},
	"openreview": {Category: "academic", CategoryName: "学术论文", Label: "OpenReview", Auth: "free",
		Desc: "ICLR/NeurIPS/ICML 等会议投稿与评审; 索引含 DBLP 整库"},
	"doaj": {Category: "academic", CategoryName: "学术论文", Label: "DOAJ", Auth: "free",
		Desc: "开放获取期刊文章, 通常能直接拿到全文链接"},

	// ── 开放数据与研究材料 ──
	"datacite": {Category: "data", CategoryName: "开放数据", Label: "DataCite", Auth: "free",
		Desc: "研究数据集/机构报告/软件的 DOI —— 数据从哪来的一手出处"},
	"zenodo": {Category: "data", CategoryName: "开放数据", Label: "Zenodo", Auth: "free",
		Desc: "CERN 运营的开放获取库: 数据集/技术报告/软件, 与 DataCite 互补"},
	"worldbank": {Category: "data", CategoryName: "开放数据", Label: "世界银行", Auth: "free",
		Desc: "国别经济备忘录/行业发展报告/项目文件等一手材料"},

	// ── 财经与上市公司公告 ──
	"cninfo": {Category: "finance", CategoryName: "财经公告", Label: "巨潮资讯", Auth: "free",
		Desc: "证监会指定披露平台: A 股年报/招股书/重大公告, 返回详情页而非 PDF"},

	// ── 政府公告与政策法规 ──
	"gov": {Category: "gov", CategoryName: "政府公告", Label: "国务院政策文件库", Auth: "free",
		Desc: "国发/国办发/国函/部委文件原文 (含发文字号), 查政策口径与监管规定的一手来源"},

	// ── 代码与开发者社区 ──
	"github": {Category: "code", CategoryName: "代码社区", Label: "GitHub", Auth: "free",
		Desc: "仓库/代码/issue 检索; 配 GitHub token 后配额更高"},
	"stackexchange": {Category: "code", CategoryName: "代码社区", Label: "Stack Exchange", Auth: "free",
		Desc: "Stack Overflow 等技术问答, 报错与用法问题命中率高"},
	"hn": {Category: "code", CategoryName: "代码社区", Label: "Hacker News", Auth: "free",
		Desc: "技术社区讨论, 看工程界风向与真实踩坑经验"},
	"npm": {Category: "code", CategoryName: "代码社区", Label: "npm", Auth: "free",
		Desc: "JS 包检索: 版本/维护状态/仓库地址"},
	"pypi": {Category: "code", CategoryName: "代码社区", Label: "PyPI", Auth: "free",
		Desc: "Python 包检索: 版本/依赖/项目主页"},

	// ── 技术文档与工程社区 ──
	"techdocs": {Category: "techdocs", CategoryName: "技术文档", Label: "技术文档", Auth: "free",
		Desc:  "官方文档与一线工程社区, 按产品名自动限定域名 (数据库/云/OS/存储/网络/中间件)",
		Sites: techdocsSiteMeta()},

	// ── 百科与背景知识 ──
	"wikipedia": {Category: "reference", CategoryName: "百科背景", Label: "维基百科", Auth: "free",
		Desc: "定义与背景知识 (走代理出口), 适合给概念打底"},
}

// ProviderMetaOf 取某源的元数据; 未登记的名字返回零值 (面板归入"其他")。
func ProviderMetaOf(name string) ProviderMeta {
	return ProviderCatalog[name]
}

// techdocsSiteMeta 把 techdocs 目录转成面板快照结构。
//
// 复制而非直接导出 techdocs.Site: 目录未来可能加抓取频率、认证方式等内部
// 字段, 快照只暴露面板需要的五项, 避免内部字段意外泄漏到 providers.json。
func techdocsSiteMeta() []SiteMeta {
	out := make([]SiteMeta, 0, len(techdocs.Sites))
	for _, s := range techdocs.Sites {
		out = append(out, SiteMeta{
			Name:      s.Name,
			Group:     string(s.Group),
			GroupName: techdocs.GroupNames[s.Group],
			URL:       s.Docs,
			Domains:   append([]string(nil), s.Domains...),
		})
	}
	return out
}
