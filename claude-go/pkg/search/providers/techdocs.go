package providers

// techdocs.go —— 技术文档聚合源。
//
// 目录里有一百多个官方文档站与工程社区, 但**不为每个站写适配器**: 所有站都
// 通过同一个 Exa 匿名 MCP 端点检索, 只是按 query 命中的产品把检索限定到对应
// 域名 (site:a OR site:b ...)。这样新增一个站只需要在 techdocs/catalog.go
// 加一行, 不需要新的爬虫、配额与熔断逻辑。
//
// 两层约束缺一不可:
//   - site: 提示负责"召回偏向正确的站";
//   - 返回后按目录域名硬过滤负责"把搜索引擎越界的结果清掉" (实测中文查询
//     带 site: 仍会夹带 2 条站外结果)。
//
// 没有任何产品名/组信号命中时直接返回空, 不发请求: 技术文档源不应该为了
// "也许相关"占掉单次检索仅有的两席之一。

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/anthropic/claude-go/pkg/search"
	"github.com/anthropic/claude-go/pkg/search/techdocs"
)

// techDocsProvider 是 Exa MCP 之上的技术文档聚合源。
type techDocsProvider struct {
	*mcpProvider
}

// newTechDocs 构造技术文档源 (Exa 匿名端点, 直连, L2, 权重 1.0)。
func newTechDocs(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	p, err := newMCPProvider("techdocs", cfg, pc, eg, "https://mcp.exa.ai/mcp", search.TierL2, 1.0)
	if err != nil {
		return nil, err
	}
	mp, ok := p.(*mcpProvider)
	if !ok {
		return nil, search.Wrap("techdocs", search.ErrUnsupported, errors.New("unexpected MCP backend type"))
	}
	return &techDocsProvider{mcpProvider: mp}, nil
}

// Search 按目录匹配域名后执行站内检索。
//
// 调用方显式给了 allowed_domains 时完全尊重调用方: 那是产品侧更清楚的
// 约束, 目录不参与, 也不做目录域名硬过滤 (Manager 会按原请求过滤)。
func (p *techDocsProvider) Search(ctx context.Context, req search.SearchRequest) ([]search.Result, error) {
	if len(req.AllowedDomains) > 0 {
		return p.mcpProvider.Search(ctx, req)
	}

	m := techdocs.MatchQuery(req.Query)
	if !m.Matched() {
		return nil, nil
	}

	sub := req
	sub.AllowedDomains = m.Domains
	out, err := p.mcpProvider.Search(ctx, sub)
	if err != nil {
		return nil, err
	}
	filtered := make([]search.Result, 0, len(out))
	for _, r := range out {
		if !urlInDomains(r.URL, m.Domains) {
			continue
		}
		r.Rank = len(filtered) + 1
		filtered = append(filtered, r)
	}
	return filtered, nil
}

// urlInDomains 判断 URL 是否落在目录域名内。
//
// 目录里绝大多数是纯域名 (后缀匹配即含子域); 少数条目带路径前缀
// (github.com/seaweedfs、aws.amazon.com/blogs), 这些要求 scheme+host+path
// 前缀都匹配, 避免把整个 github.com / aws.amazon.com 放进来。
func urlInDomains(raw string, domains []string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	lowerURL := strings.ToLower(raw)
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if strings.Contains(d, "/") {
			if strings.HasPrefix(lowerURL, "https://"+d) || strings.HasPrefix(lowerURL, "http://"+d) {
				return true
			}
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}
