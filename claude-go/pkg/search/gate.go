package search

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
}

// Pass 判断质量是否达标。
//
// 阈值来自设计文档 §5.4: 域名 >= 3、有片段、语言匹配 >= 0.6、
// 重复率 <= 0.5, 需要时效时新鲜比例 >= 0.3。金融链路不走这个门。
func (q Quality) Pass(freshnessRequired bool) bool {
	return q.Domains >= 3 &&
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

	for _, r := range results {
		host := canonicalHost(r.URL)
		if host != "" {
			domains[host] = struct{}{}
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
	q.FreshRatio = float64(fresh) / float64(len(results))
	q.LangMatch = float64(langMatch) / float64(len(results))
	q.DuplicateRatio = float64(len(results)-len(canonicalCount)) / float64(len(results))

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

// languageMatches 判断结果是否匹配请求语言。
//
// 优先信任 Result.Lang; 为空时从标题与片段里推断: 汉字比例高视为 zh,
// 否则视为 en。请求语言为空时一律视为匹配。
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
	case q.Domains < 3:
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
