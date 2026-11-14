package search

// router.go —— 零模型调用的查询规划与源选择。
//
// 两段职责:
//  1. PlanQuery: 用规则识别语言/意图/时效, 拆出 1–3 条子查询。规则保持保守,
//     只处理信号明确的查询; 复杂语义留给上层 agent 自己换关键词。
//  2. selectProviders + sortProviders: 先按配置/层级/健康/配额过滤, 再按
//     "意图偏好 -> tier -> 权重 -> 名字"排序。排序只决定谁先被调度, 不排除
//     表外源; 真正的并发上限由 scheduler.go 的 maxSourceConcurrency 控制。
import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Intent 是查询意图, 决定源顺序与时延偏好。
type Intent string

const (
	IntentNavigational Intent = "navigational"
	IntentFactual      Intent = "factual"
	IntentAcademic     Intent = "academic"
	IntentCode         Intent = "code"
	IntentNews         Intent = "news"
	IntentLocal        Intent = "local"
	// IntentData 是统计 / 年报 / 行业报告类查询。它和 news 的区别在于:
	// news 要最新几天的报道, data 要的是可核对的口径与原文, 来源也完全不同
	// (统计机构、上市公司披露、国际组织文档)。共用一套源顺序会让两类查询
	// 互相挤掉对方最好的源。
	IntentData Intent = "data"
)

// QueryPlan 是零模型调用的查询规划结果, 对应设计文档 §5.1。
type QueryPlan struct {
	Raw        string
	Language   string // zh | en | other
	Intent     Intent
	Freshness  string // any | day | week | month | year
	SubQueries []string
	SiteHints  []string
	// ScopedDomains 是从生态品牌词推导出的域名限定 (如微信公众号 -> mp.weixin.qq.com)。
	// 它只表达"查询在找某个内容生态的文章"; Manager 在调用方未显式指定
	// AllowedDomains 时把它套进请求, providers 据此追加 site: 提示, 融合前
	// 仍由 Manager 做硬过滤。显式 allowed_domains 始终优先于推导值。
	ScopedDomains []string
}

// maxSourceConcurrency 是单次检索最多同时查询的源数。
const maxSourceConcurrency = 2

// 子查询切分: 支持中英文的"和 / 与 / 对比 / vs"。
var subQuerySplitter = regexp.MustCompile(`(?i)\s+(?:vs\.?|versus)\s+|\s*(?:对比|和|与)\s*`)

// kYearPattern 匹配 19xx/20xx 年份。
var yearPattern = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)

// ecosystemRegistry 把明确的生态品牌词映射到其公开文章域名。
//
// 命中后检索自动限定到该生态的域名 (调用方显式 allowed_domains 优先), 走
// 通用源的索引与 site: 提示, 而不是抓取生态自己的搜索页: 搜狗微信搜索与
// 头条搜索的 robots.txt 都禁止搜索路径, 且前者结果链接受签名保护、后者
// 详情页是 JS 挑战, 均不适合无人值守生产抓取 (2026-11 实测结论)。
// 匹配保持保守 —— 必须是连续的品牌词, 避免把"今日(的)头条新闻"这类
// 普通说法误判成"今日头条"app 生态。
var ecosystemRegistry = []struct {
	tokens []string
	domain string
}{
	{[]string{"微信公众号", "微信文章", "公众号文章", "mp.weixin.qq.com"}, "mp.weixin.qq.com"},
	{[]string{"今日头条", "头条号", "头条文章", "toutiao.com"}, "toutiao.com"},
}

// PlanQuery 用规则生成查询计划, 全程不调用模型。
//
// 规则有意保持保守: 只在信号明确时改意图/时效, 剩余复杂 query 由 L3
// 研究模式再处理。SubQueries 最多 3 条。
func PlanQuery(query string) QueryPlan {
	raw := strings.TrimSpace(query)
	plan := QueryPlan{
		Raw:        raw,
		Language:   detectLanguage(raw),
		Intent:     IntentFactual,
		Freshness:  "any",
		SubQueries: []string{raw},
	}
	lower := strings.ToLower(raw)

	// 生态品牌词检测要先于新闻/时效分支: "今日头条" 里包含 "今日", 直接进
	// 新闻分支会强制 week 级新鲜度, 而生态文章常常不是本周发布的, 质量门
	// 会因此恒报 fresh。把命中的品牌词从后续时效探测串里剥掉 —— 既消除
	// 品牌词自身的误触发, 又保留 "今日头条最新文章" 里真正的 "最新"。
	probe := lower
	for _, eco := range ecosystemRegistry {
		for _, token := range eco.tokens {
			if strings.Contains(probe, token) {
				plan.ScopedDomains = append(plan.ScopedDomains, eco.domain)
				plan.SiteHints = append(plan.SiteHints, "site:"+eco.domain)
				probe = strings.ReplaceAll(probe, token, " ")
				break
			}
		}
	}

	// switch 的分支顺序就是优先级: 代码/学术信号优先于新闻。例如
	// "postgres 18 release notes" 同时含年份与发布词, 若不先判文档, 会被
	// 当成新闻并强制 week 级新鲜度, 质量门反而因文档日期旧而恒不过。
	switch {
	case containsAny(lower, "github", "golang", "error:", "panic:", "stackoverflow", "源代码", "源码"):
		plan.Intent = IntentCode
		plan.SiteHints = append(plan.SiteHints, "site:github.com", "site:stackoverflow.com")
	case containsAny(lower, "论文", "paper", "arxiv", "survey", "文献", "crossref", "pubmed"):
		plan.Intent = IntentAcademic
		plan.SiteHints = append(plan.SiteHints, "site:arxiv.org", "site:openalex.org")
	case isDataQuery(probe):
		// 放在新闻之前: 「2024 年统计公报」带年份, 但用户要的是统计口径,
		// 不是本周新闻; 若被年份分支判成 news, 会强制 week 级新鲜度,
		// 反而把年度公报判成不新鲜。
		plan.Intent = IntentData
	case containsAny(probe, "今天", "今日", "最新", "发布", "新闻", "news", "刚刚"):
		// 注意不含 "release": "postgres 18 release notes" 是查文档, 不是要上周新闻。
		// 把它当新闻会强制 week 级新鲜度, 质量门因文档日期旧而恒不过。
		plan.Intent = IntentNews
		plan.Freshness = "week"
	case yearPattern.MatchString(lower):
		// 带年份/版本多为时效问题, 但按"年"而非"周"要求: 版本发布说明通常不是本周的。
		plan.Intent = IntentNews
		plan.Freshness = "year"
	}

	if plan.Language == "" {
		plan.Language = "en"
	}
	if subs := splitSubQueries(raw); len(subs) > 0 {
		plan.SubQueries = subs
	}
	return plan
}

// splitSubQueries 按"和/与/对比/vs"把问题拆成 1–3 条子查询。
//
// 切分后去空白、去重、丢弃过短片段; 结果超过 3 条时保留前 3 条。
// 无法有意义切分时返回原句。
func splitSubQueries(q string) []string {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	parts := subQuerySplitter.Split(q, -1)
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{})
	for _, p := range parts {
		p = strings.TrimSpace(strings.Trim(p, "，,。.；;：:"))
		if tooShortSubQuery(p) {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
		if len(out) == 3 {
			break
		}
	}
	if len(out) == 0 {
		return []string{q}
	}
	return out
}

// tooShortSubQuery 判断切分片段是否短到不适合作为查询。
//
// 单个英文字母/数字允许保留 (如 "A vs B"), 但单个汉字不保留,
// 否则"和平"这类词被误切后会剩一个无意义的单字查询。
func tooShortSubQuery(p string) bool {
	n := utf8.RuneCountInString(p)
	if n >= 2 {
		return false
	}
	if n == 0 {
		return true
	}
	r, _ := utf8.DecodeRuneInString(p)
	return !(r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)))
}

// dataSignalRe 匹配英文统计/年报信号。
//
// 用词边界而不是子串: "gdpr" 里含 "gdp", "capital" 里含 "cpi" 之类的
// 误判会让普通技术查询被路由到统计源。
var dataSignalRe = regexp.MustCompile(`(?i)\b(gdp|cpi|ppi|pmi|statistics?|statistical|census|unemployment|labor force|employment rate|inflation|annual report|industry report|market size|data ?set|world bank|imf|oecd)\b`)

// dataSignalZH 是中文统计/年报信号。
//
// 刻意不含单独的「数据」: 「数据库」「数据结构」「大数据」都会命中, 把大量
// 技术查询误判成统计查询。只收不会有歧义的词组。
var dataSignalZH = []string{
	"统计", "年鉴", "年报", "财报", "行业报告", "研究报告", "市场规模",
	"就业率", "失业率", "人口普查", "居民消费价格", "增加值", "进出口",
	"人均可支配", "国民经济", "社会消费品零售", "固定资产投资",
	"采购经理指数", "统计公报",
	// 上市公司披露的完整说法。「年报」不是「年度报告」的子串（年度|报告），
	// 只收「年报」会让「2025 年年度报告」被年份分支判成 news，进而错过
	// 巨潮这个一手的公告检索源。
	"年度报告", "半年度报告", "季度报告", "招股说明书", "招股书",
	"业绩预告", "业绩快报", "营业收入", "营收", "净利润",
}

// isDataQuery 判断查询是否在找统计口径 / 年报 / 行业报告。
func isDataQuery(lower string) bool {
	for _, s := range dataSignalZH {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return dataSignalRe.MatchString(lower)
}

// containsAny 判断 s 是否包含任意一个子串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// selectProviders 过滤并排序候选源。
//
// 过滤顺序: 配置禁用 -> MaxTier -> 付费不可达 -> 能力位 -> 本地健康 ->
// 限流冷却。代理不可用不会剔除 proxy 源, 而是记 proxy_down: 出口层
// Client(ModeProxy) 会自动回落到直连 (设计约束见任务书)。
func selectProviders(providers []Provider, plan QueryPlan, cfg Config, eg *Egress, quota *quotaTracker) ([]Provider, []string) {
	candidates := make([]Provider, 0, len(providers))
	var degraded []string
	proxyDown := false

	for _, p := range providers {
		if p == nil {
			continue
		}
		if !providerEnabled(p, cfg) {
			continue
		}
		if p.Tier() > cfg.MaxTier {
			continue
		}
		if p.Tier() == TierL3 && cfg.MaxCostUSD <= 0 {
			continue // 付费层默认不可达
		}
		if !p.Kinds().Has(KindSearch) {
			continue
		}
		if !p.Healthy() {
			// 退避/熔断中的源不参与本次检索, 但必须让调用方看见 —— 否则
			// 结果看起来像"本来就只有这一个源", 召回变差却无从归因。
			degraded = append(degraded, "provider_unhealthy:"+p.Name())
			continue
		}
		if quota != nil && quota.cooling(p.Name()) {
			degraded = append(degraded, "quota_exhausted:"+p.Name())
			continue
		}
		if pin := providerPin(p, cfg, eg); pin == ModeProxy {
			// 未配置代理时走直连是预期行为, 不记 proxy_down; 否则每次
			// 无代理运行都会把整条 proxy 源标成降级, 掩盖真正的代理故障。
			if eg != nil && eg.HasProxy() && !eg.Healthy() {
				proxyDown = true
			}
		}
		candidates = append(candidates, p)
	}

	if proxyDown {
		degraded = append(degraded, "proxy_down")
	}
	sortProviders(candidates, plan, cfg)
	return candidates, degraded
}

// providerEnabled 判断 provider 是否被配置启用。
//
// Config.Providers 里未出现的源视为启用 (使用构造时默认值); 出现时以
// Enabled/Disabled 为准。
func providerEnabled(p Provider, cfg Config) bool {
	pc, ok := cfg.Providers[p.Name()]
	if !ok {
		return true
	}
	if pc.Disabled {
		return false
	}
	return pc.Enabled
}

// providerPin 返回 provider 的出口模式。
//
// 优先级: provider 自声明 > 配置项 > Egress 默认模式。
func providerPin(p Provider, cfg Config, eg *Egress) Mode {
	if pinned, ok := p.(EgressPinned); ok {
		if m := pinned.EgressMode(); m != "" {
			return m
		}
	}
	if pc, ok := cfg.Providers[p.Name()]; ok && pc.Egress != "" {
		return pc.Egress
	}
	if eg != nil {
		return eg.Mode()
	}
	return ModeDirect
}

// sortProviders 按意图偏好、层级、权重、名字排序候选源。
//
// 偏好表只是排序提示, 不硬性排除: 表外源仍会按 tier/权重参与。
func sortProviders(candidates []Provider, plan QueryPlan, cfg Config) {
	order := preferenceOrder(plan.Intent, plan.Language)
	if custom, ok := intentPriority(cfg.IntentPriority, plan); ok {
		// 意图级覆盖优先于全局覆盖: 统计/学术查询需要把专业源排到通用源前面,
		// 而全局列表是按日常事实查询的额度情况调的。
		order = custom
	} else if len(cfg.ProviderPriority) > 0 {
		// 调用方显式给了优先级就以它为准: 产品侧知道自己的检索负载与各源
		// 的免费额度, 固定偏好表无法覆盖。表外源仍在候选里, 只是排在后面。
		order = cfg.ProviderPriority
	}
	index := make(map[string]int, len(order))
	for i, name := range order {
		index[name] = i
	}
	weights := providerWeights(candidates, cfg)

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		ai, aok := index[a.Name()]
		bi, bok := index[b.Name()]
		if aok != bok {
			return aok
		}
		if aok && ai != bi {
			return ai < bi
		}
		if a.Tier() != b.Tier() {
			return a.Tier() < b.Tier()
		}
		if weights[a.Name()] != weights[b.Name()] {
			return weights[a.Name()] > weights[b.Name()]
		}
		return a.Name() < b.Name()
	})
}

// intentPriority 查找意图级源顺序。
//
// 先找 "intent:lang"（如 "data:zh"），再找 "intent"。两者都没有时返回
// false，由调用方回落到全局 ProviderPriority 或内置偏好表。
func intentPriority(m map[string][]string, plan QueryPlan) ([]string, bool) {
	if len(m) == 0 {
		return nil, false
	}
	if v, ok := m[string(plan.Intent)+":"+normalizeLang(plan.Language)]; ok && len(v) > 0 {
		return v, true
	}
	if v, ok := m[string(plan.Intent)]; ok && len(v) > 0 {
		return v, true
	}
	return nil, false
}

// preferenceOrder 返回意图相关的源偏好顺序。
func preferenceOrder(intent Intent, language string) []string {
	zh := normalizeLang(language) == "zh"
	switch intent {
	case IntentCode:
		return []string{"exa", "parallel", "github", "stackexchange", "exa_anon", "tavily", "bing_rss"}
	case IntentAcademic:
		// openreview 紧跟 arxiv：两者互补——arxiv 覆盖预印本，openreview
		// 覆盖会议投稿，且其索引含 DBLP 整库（DBLP 站点本身被 Anubis 挡住）。
		return []string{"exa", "parallel", "openalex", "crossref", "pubmed", "arxiv", "openreview", "doaj", "datacite", "exa_anon", "tavily"}
	case IntentData:
		// 中文先巨潮（A 股年报首发平台），英文先世界银行（国别/行业一手报告）。
		// 两类都保留 exa_anon 作对冲：专业库只覆盖结构化材料，统计口径的
		// 解读往往还在新闻/分析页里。
		if zh {
			return []string{"cninfo", "exa_anon", "worldbank", "datacite", "doaj", "parallel", "zenodo"}
		}
		// 专业库排第一，但通用源必须先于第二个专业库：World Bank 的 qterm
		// 是严格 AND，长查询常返回 0；DataCite 的 best-match 则会给出噪声
		// 很大的 DOI。只调度前两位时，把 exa_anon 放在 second 才能保证
		// 「专业库空手而归」的那一轮仍有可读材料。
		return []string{"worldbank", "exa_anon", "datacite", "zenodo", "doaj", "cninfo", "parallel"}
	case IntentNews:
		// exa_anon 必须排在 bing_rss 前面: 只调度前 2 个源, parallel 被限流
		// (实测 6 个任务并发时 quota_exhausted) 后, 旧顺序会让 bing_rss +
		// gnews 顶上来 —— 前者噪声大, 后者只回跳转链, 整轮调研就废了。
		// exa_anon 免 key、直连可用, 实测中文时效查询召回质量接近 parallel。
		return []string{"parallel", "exa_anon", "tavily", "bing_rss", "gnews", "exa"}
	}
	// bing_rss 排最后: 它只是"兜底中的兜底", 长查询会回完全不同话题的页面
	// (实测有 Yandex/影视站/直播站)。主源 quota_exhausted 时, 百科与新闻源
	// 比它更可能给出同话题结果 —— 此前 bing 排在它们前面, 等于优先选噪声。
	if zh {
		// baidu / sogou 曾出现在偏好表里, 但 factory 从未注册这两个源 ——
		// 它们永远不可能被选中, 只会让人误以为"中文有百度兜底"。
		// 中文事实类真正可用的是 exa_anon, 必须排在 wikipedia/bing 之前。
		return []string{"parallel", "exa", "exa_anon", "tavily", "wikipedia", "bing_rss"}
	}
	return []string{"parallel", "exa", "exa_anon", "tavily", "wikipedia", "gnews", "bing_rss"}
}

// isHan 判断字符是否汉字。
func isHan(r rune) bool { return unicode.Is(unicode.Han, r) }
