package search

// request.go —— 工具请求边界与不可绕过的域名过滤。
//
// 域名过滤在 Manager 内、融合之后执行，而不是依赖 provider 是否理解某个
// 参数。这样任何 provider 的域外结果都不能泄漏给模型；过滤为空就是 empty，
// 绝不把未过滤结果当作兜底返回。

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

const maxSearchLimit = 50

// normalizeSearchRequest 校验并规范化会影响结果或缓存身份的字段。
func normalizeSearchRequest(req SearchRequest, defaultLimit int) (SearchRequest, error) {
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		return req, Wrap("", ErrInvalid, errors.New("empty query"))
	}
	if req.Limit < 0 || req.Limit > maxSearchLimit {
		return req, Wrap("", ErrInvalid, fmt.Errorf("limit must be between 1 and %d", maxSearchLimit))
	}
	if req.Limit == 0 {
		req.Limit = defaultLimit
	}
	if req.Limit <= 0 {
		req.Limit = 8
	}
	req.Language = normalizeLang(req.Language)
	if len(req.Language) > 35 {
		return req, Wrap("", ErrInvalid, errors.New("language tag is too long"))
	}
	req.Freshness = strings.ToLower(strings.TrimSpace(req.Freshness))
	switch req.Freshness {
	case "", "any", "day", "week", "month", "year":
	default:
		return req, Wrap("", ErrInvalid, fmt.Errorf("unsupported freshness %q", req.Freshness))
	}
	var err error
	if req.AllowedDomains, err = normalizeDomains(req.AllowedDomains); err != nil {
		return req, Wrap("", ErrInvalid, fmt.Errorf("allowed_domains: %w", err))
	}
	if req.BlockedDomains, err = normalizeDomains(req.BlockedDomains); err != nil {
		return req, Wrap("", ErrInvalid, fmt.Errorf("blocked_domains: %w", err))
	}
	req.OriginalQuestion = strings.TrimSpace(req.OriginalQuestion)
	return req, nil
}

// normalizeDomains 只接受域名本身，不接受 URL、端口或路径，避免把丢弃路径
// 误当成成功实现了调用方约束。example.com 同时包含它的子域名。
func normalizeDomains(domains []string) ([]string, error) {
	if len(domains) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(domains))
	out := make([]string, 0, len(domains))
	for _, raw := range domains {
		d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		d = strings.TrimPrefix(d, "*.")
		if d == "" || len(d) > 253 || strings.ContainsAny(d, "/:@?#\\") || strings.ContainsAny(d, " \t\r\n") {
			return nil, fmt.Errorf("invalid domain %q", raw)
		}
		if net.ParseIP(d) == nil {
			for _, label := range strings.Split(d, ".") {
				if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
					return nil, fmt.Errorf("invalid domain %q", raw)
				}
				for _, r := range label {
					if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
						return nil, fmt.Errorf("invalid domain %q", raw)
					}
				}
			}
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out, nil
}

func domainMatches(host, domain string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// filterResultDomains 同时执行允许与拒绝约束；blocked 优先于 allowed。
func filterResultDomains(results []Result, req SearchRequest) []Result {
	if len(req.AllowedDomains) == 0 && len(req.BlockedDomains) == 0 {
		return results
	}
	out := make([]Result, 0, len(results))
	for _, r := range results {
		host := canonicalHost(r.URL)
		allowed := len(req.AllowedDomains) == 0
		for _, d := range req.AllowedDomains {
			if domainMatches(host, d) {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		blocked := false
		for _, d := range req.BlockedDomains {
			if domainMatches(host, d) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, r)
		}
	}
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// responseStatus 区分有结果、部分降级、真实空结果；provider 全失败由 error
// 返回，不伪装成成功的空数组。
func responseStatus(resultCount int, degraded []string) string {
	if resultCount == 0 {
		return "empty"
	}
	if len(degraded) > 0 {
		return "partial"
	}
	return "success"
}
