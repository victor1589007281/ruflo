package providers

import (
	"strings"

	"github.com/anthropic/claude-go/pkg/search"
)

// searchQueryWithDomainHints 把调用方域名约束传给通用搜索引擎。
//
// Parallel/Exa/Bing 都理解 site: 语法；该提示只改善召回，不是安全边界。
// Manager 仍会对返回 URL 执行 allowed/blocked 硬过滤。专业 API（arxiv、
// OpenAlex 等）不调用这里，避免把搜索引擎语法误发给不支持它的服务。
func searchQueryWithDomainHints(query string, req search.SearchRequest) string {
	query = strings.TrimSpace(query)
	allowed := make([]string, 0, len(req.AllowedDomains))
	for _, domain := range req.AllowedDomains {
		if domain = strings.TrimSpace(domain); domain != "" {
			allowed = append(allowed, "site:"+domain)
		}
	}
	if len(allowed) == 1 {
		query += " " + allowed[0]
	} else if len(allowed) > 1 {
		query += " (" + strings.Join(allowed, " OR ") + ")"
	}
	for _, domain := range req.BlockedDomains {
		if domain = strings.TrimSpace(domain); domain != "" {
			query += " -site:" + domain
		}
	}
	return strings.TrimSpace(query)
}
