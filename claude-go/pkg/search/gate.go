package search

// gate.go —— 检索质量门。
//
// 质量门回答的是"这批结果够不够支撑一次调研", 不负责排序, 也不改写结果:
// 指标由 EvaluateQuality 在**融合前**的原始结果上计算, 这样多源重叠、重复
// URL 与域名集中度才是真实值; 融合后重复已被 RRF 去掉, 再算就恒为 0。
//
// 门禁失败不会让 Search 报错, 只把 passed/reason 放进响应, 由调用方决定
// 是否降级或继续检索。
import (
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Quality 是搜索结果的质量指标, 对应设计文档 §5.4。
//
// 指标只用于判断"够不够用", 不参与排序; 排序由 RRF 负责。
type Quality struct {
	Domains        int     // 独立域名数
	HasSnippet     bool    // 是否至少有一条片段
	FreshRatio     float64 // 已发布时间落在时效窗口内的比例
	LangMatch      float64 // 语言与请求匹配的比例
	Consensus      float64 // 被两个以上 provider 同时命中的 URL 比例
	DuplicateRatio float64 // 重复 canonical URL 的比例
	// AuthoritativeRatio 结果中来自官方/一手域名的比例。
	//
	// 只用于域名多样性那一条的例外: 官方文档/监管/统计站点本身就只有一两个
	// 域名, 要求 >=3 个独立域名会把"8 条全部来自 opentelemetry.io"判成不达标。
	// 它不降低片段/语言/重复/时效的门槛 —— 那几项与"来源是否权威"无关。
	AuthoritativeRatio float64
}

// Pass 判断质量是否达标。
//
// 阈值来自设计文档 §5.4: 域名 >= 3、有片段、语言匹配 >= 0.6、
// 重复率 <= 0.5, 需要时效时新鲜比例 >= 0.3。金融链路不走这个门。
//
// 域名例外: 官方/一手来源占 >=80% 时, 单域名也视为达标。判据不是"域名少就放行"
// —— 3 条来自同一个博客仍然失败 (见 IsAuthoritativeDomain)。
func (q Quality) Pass(freshnessRequired bool) bool {
	return (q.Domains >= 3 || q.AuthoritativeRatio >= 0.8) &&
		q.HasSnippet &&
		q.LangMatch >= 0.6 &&
		q.DuplicateRatio <= 0.5 &&
		(q.FreshRatio >= 0.3 || !freshnessRequired)
}

// EvaluateQuality 按请求计算质量指标。
//
// results 是融合前的原始结果, 这样 DuplicateRatio 才能反映多源重叠;
// 若传融合后的结果, 重复已被 RRF 去掉, 该指标会恒为 0。
func EvaluateQuality(results []Result, req SearchRequest) Quality {
	return evaluateQualityAt(results, req, time.Now())
}

// evaluateQualityAt 是可注入 now 的实现, 便于测试时效指标。
func evaluateQualityAt(results []Result, req SearchRequest, now time.Time) Quality {
	q := Quality{LangMatch: 1}
	if len(results) == 0 {
		q.LangMatch = 0
		return q
	}

	domains := make(map[string]struct{})
	canonicalCount := make(map[string]int)
	providerURLs := make(map[string]map[string]struct{})
	window := freshnessWindow(req.Freshness)
	var fresh int
	var langMatch int
	var authoritative int

	for _, r := range results {
		host := canonicalHost(r.URL)
		if host != "" {
			domains[host] = struct{}{}
			if IsAuthoritativeDomain(host) {
				authoritative++
			}
		}
		canon := CanonicalURL(r.URL)
		canonicalCount[canon]++
		if r.Provider != "" {
			set := providerURLs[canon]
			if set == nil {
				set = make(map[string]struct{})
				providerURLs[canon] = set
			}
			set[r.Provider] = struct{}{}
		}

		if strings.TrimSpace(r.Snippet) != "" {
			q.HasSnippet = true
		}
		if !r.Published.IsZero() && !r.Published.Before(now.Add(-window)) {
			fresh++
		}
		if languageMatches(r, req.Language) {
			langMatch++
		}
	}

	q.Domains = len(domains)
	// 以下四个比例都以"原始结果条数"为分母; DuplicateRatio 等价于
	// 1 - 唯一 canonical URL 数/总条数, 因此 0.5 表示一半是重复。
	q.AuthoritativeRatio = float64(authoritative) / float64(len(results))
	q.FreshRatio = float64(fresh) / float64(len(results))
	q.LangMatch = float64(langMatch) / float64(len(results))
	q.DuplicateRatio = float64(len(results)-len(canonicalCount)) / float64(len(results))

	// Consensus 以**唯一 URL 数**为分母 (不是总条数): 它衡量"有多少比例的资源
	// 被两个以上源同时命中", 重复条目不应该让共识看起来更高。
	var consensus int
	for _, providers := range providerURLs {
		if len(providers) >= 2 {
			consensus++
		}
	}
	q.Consensus = float64(consensus) / float64(len(canonicalCount))
	return q
}

// freshnessRequired 判断本次请求是否必须满足时效比例。
func freshnessRequired(freshness string) bool {
	switch strings.ToLower(strings.TrimSpace(freshness)) {
	case "day", "week":
		return true
	default:
		return false
	}
}

// freshnessWindow 返回"新鲜"的判定窗口。
func freshnessWindow(freshness string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(freshness)) {
	case "day":
		return 24 * time.Hour
	case "week":
		return 7 * 24 * time.Hour
	case "month":
		return 30 * 24 * time.Hour
	case "year":
		return 365 * 24 * time.Hour
	default:
		return 30 * 24 * time.Hour
	}
}

// canonicalHost 返回 URL 的规范化 host (去 www. 前缀, 小写)。
func canonicalHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	return host
}

// authoritativeSuffixes 官方/监管/标准类域名后缀。按 label 边界匹配:
// "sec.gov" 命中 ".gov", "fakegov" 不命中。
var authoritativeSuffixes = []string{
	".gov", ".gov.cn", ".gov.uk", ".gov.au", ".go.jp", ".gob.es", ".gouv.fr",
	".govt.nz", ".gc.ca", ".admin.ch", ".europa.eu", ".int",
}

// authoritativeHosts 没有官方后缀、但本身就是一手来源的站点。
//
// 这张表只服务于"域名多样性例外": 漏掉一个站点最多是退回旧的 >=3 域名判据
// (行为不变), 误加一个非权威站点才会真正放低门槛。因此只收标准组织、统计/
// 监管机构、官方文档站与交易所披露平台。
var authoritativeHosts = map[string]struct{}{
	"opentelemetry.io": {},
	"w3.org":           {},
	"ietf.org":         {},
	"rfc-editor.org":   {},
	"iana.org":         {},
	"unicode.org":      {},
	"doi.org":          {},
	"crossref.org":     {},
	"imf.org":          {},
	"worldbank.org":    {},
	"oecd.org":         {},
	"bis.org":          {},
	"un.org":           {},
	"wto.org":          {},
	"iea.org":          {},
	"unctad.org":       {},
	// 交易所/上市公司披露平台: 财报与公告的一手来源。
	"sse.com.cn":    {},
	"szse.cn":       {},
	"hkex.com.hk":   {},
	"cninfo.com.cn": {},
}

// IsAuthoritativeDomain 判断 host 是否属于官方/一手来源。
//
// host 先经 canonicalHost 的同一套归一 (小写、去 www.); 传入原始 URL 也可以。
// 公司投资者关系子域 (investor./ir.) 视为一手来源 —— 财报原文就在那里, 域名
// 列表无法穷举所有公司。
func IsAuthoritativeDomain(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(host, "://") {
		host = canonicalHost(host)
	}
	host = strings.TrimPrefix(host, "www.")
	if host == "" {
		return false
	}
	if _, ok := authoritativeHosts[host]; ok {
		return true
	}
	if strings.HasPrefix(host, "investor.") || strings.HasPrefix(host, "ir.") ||
		strings.Contains(host, ".investor.") || strings.Contains(host, ".ir.") {
		return true
	}
	for _, suffix := range authoritativeSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// languageMatches 判断结果是否匹配请求语言。
//
// 优先信任 Result.Lang; 为空时从标题与片段里推断: 汉字比例高视为 zh,
// 否则视为 en。请求语言为空时一律视为匹配。
//
// 口径是二分的: 请求 zh 只接受 zh, 其余语言 (含 en/other) 只排除 zh。
// 这意味着 en 请求会接受法语等非中文结果 —— 宁可放宽也不要把跨语言的一手
// 资料误判为不匹配; 真正需要严格语言匹配时应由 provider 侧的语言参数保证。
func languageMatches(r Result, want string) bool {
	want = normalizeLang(want)
	if want == "" {
		return true
	}
	got := normalizeLang(r.Lang)
	if got == "" {
		got = detectLanguage(r.Title + " " + r.Snippet)
	}
	if want == "zh" {
		return got == "zh"
	}
	return got != "zh"
}

// normalizeLang 把语言标签归一为 zh/en, 其它原样小写。
func normalizeLang(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "zh") {
		return "zh"
	}
	if strings.HasPrefix(s, "en") {
		return "en"
	}
	return s
}

// detectLanguage 用汉字占比做最小语言判定。
func detectLanguage(s string) string {
	var han, letters int
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			han++
			letters++
		case unicode.IsLetter(r), unicode.IsDigit(r):
			letters++
		}
	}
	if letters == 0 {
		return ""
	}
	if float64(han)/float64(letters) >= 0.2 {
		return "zh"
	}
	return "en"
}

// gateReason 返回质量门失败原因, 通过时返回 "pass"。
func (q Quality) gateReason(freshness bool) string {
	switch {
	case q.Domains < 3 && q.AuthoritativeRatio < 0.8:
		return "domains"
	case !q.HasSnippet:
		return "snippet"
	case q.LangMatch < 0.6:
		return "lang"
	case q.DuplicateRatio > 0.5:
		return "duplicate"
	case freshness && q.FreshRatio < 0.3:
		return "fresh"
	default:
		return "pass"
	}
}
