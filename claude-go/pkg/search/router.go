package search

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
)

// QueryPlan 是零模型调用的查询规划结果, 对应设计文档 §5.1。
type QueryPlan struct {
	Raw        string
	Language   string // zh | en | other
	Intent     Intent
	Freshness  string // any | day | week | month | year
	SubQueries []string
	SiteHints  []string
}

// maxSourceConcurrency 是单次检索最多同时查询的源数。
const maxSourceConcurrency = 2

// 子查询切分: 支持中英文的"和 / 与 / 对比 / vs"。
var subQuerySplitter = regexp.MustCompile(`(?i)\s+(?:vs\.?|versus)\s+|\s*(?:对比|和|与)\s*`)

// kYearPattern 匹配 19xx/20xx 年份。
var yearPattern = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)

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

	switch {
	case containsAny(lower, "github", "golang", "error:", "panic:", "stackoverflow", "源代码", "源码"):
		plan.Intent = IntentCode
		plan.SiteHints = []string{"site:github.com", "site:stackoverflow.com"}
	case containsAny(lower, "论文", "paper", "arxiv", "survey", "文献", "crossref", "pubmed"):
		plan.Intent = IntentAcademic
		plan.SiteHints = []string{"site:arxiv.org", "site:openalex.org"}
	case containsAny(lower, "今天", "今日", "最新", "发布", "新闻", "news", "刚刚"):
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
			if eg == nil || !eg.HasProxy() || !eg.Healthy() {
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

// preferenceOrder 返回意图相关的源偏好顺序。
func preferenceOrder(intent Intent, language string) []string {
	zh := normalizeLang(language) == "zh"
	switch intent {
	case IntentCode:
		return []string{"exa", "parallel", "github", "stackexchange", "exa_anon", "tavily", "bing_rss"}
	case IntentAcademic:
		return []string{"exa", "parallel", "openalex", "crossref", "pubmed", "arxiv", "exa_anon", "tavily"}
	case IntentNews:
		return []string{"parallel", "tavily", "bing_rss", "gnews", "exa", "exa_anon"}
	}
	if zh {
		return []string{"parallel", "exa", "tavily", "baidu", "sogou", "bing_rss", "wikipedia", "exa_anon"}
	}
	return []string{"parallel", "exa", "exa_anon", "tavily", "bing_rss", "wikipedia", "gnews"}
}

// isHan 判断字符是否汉字。
func isHan(r rune) bool { return unicode.Is(unicode.Han, r) }
