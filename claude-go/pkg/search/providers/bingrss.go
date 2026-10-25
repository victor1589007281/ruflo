package providers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/anthropic/claude-go/pkg/search"
)

// bingRSSProvider 通过 Bing 的 RSS 输出做兜底检索。
//
// 设计文档 §3.5: 直连可达但结果质量一般, 只作最后兜底。
type bingRSSProvider struct {
	*base
	endpoint string
}

// newBingRSS 构造 Bing RSS 源 (直连, L2)。
func newBingRSS(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://www.bing.com/search"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("bing_rss", cfg, pc, eg, search.ModeDirect, search.TierL2, 0.6)
	b.kinds = search.KindSet(search.KindSearch)
	return &bingRSSProvider{base: b, endpoint: endpoint}, nil
}

// Search 抓取并解析 Bing RSS。
func (p *bingRSSProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	target := withQuery(p.endpoint, url.Values{
		"q":      {query},
		"format": {"rss"},
		"count":  {strconv.Itoa(limit)},
	})
	data, _, _, err := doRequest(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/rss+xml, application/xml;q=0.9, */*;q=0.8"},
	}, nil)
	if err != nil {
		return nil, err
	}
	items, err := parseRSS(p.name, data)
	if err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(items))
	for _, it := range items {
		link := strings.TrimSpace(it.Link)
		if link == "" {
			link = strings.TrimSpace(it.GUID)
		}
		if !isHTTPURL(link) {
			continue
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     snippetText(it.Title, 200),
			Snippet:   snippetText(it.Description, maxSnippetRunes),
			Published: parseTimeAny(it.PubDate),
		})
	}
	// Bing RSS 是最后兜底: 长查询会回一批完全不同话题的页面 (实测有 Yandex
	// 首页、影视站、直播站). 它们命中质量门只会让证据表更难分辨, 所以在这里
	// 按查询内容词做一次保守过滤. 只在兜底源启用 —— 主源的语义匹配不能被
	// 词面过滤误删.
	results = search.FilterRelevant(results, req.Query)
	return finishResults(p.name, req, results, limit), nil
}
