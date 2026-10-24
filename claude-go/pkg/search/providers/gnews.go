package providers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/anthropic/claude-go/pkg/search"
)

// gnewsProvider 通过 Google News RSS 提供时效类结果。
//
// 设计文档 §3.5: 直连超时, 必须经代理; 302 由 http.Client 自动跟随。
type gnewsProvider struct {
	*base
	endpoint string
}

// newGNews 构造 Google News RSS 源 (代理, L2)。
func newGNews(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://news.google.com/rss/search"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("gnews", cfg, pc, eg, search.ModeProxy, search.TierL2, 0.7)
	b.kinds = search.KindSet(search.KindSearch)
	return &gnewsProvider{base: b, endpoint: endpoint}, nil
}

// Search 抓取并解析 Google News RSS。
func (p *gnewsProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	target := withQuery(p.endpoint, url.Values{
		"q":    {query},
		"hl":   {"zh-CN"},
		"gl":   {"CN"},
		"ceid": {"CN:zh-Hans"},
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
		if !isHTTPURL(link) {
			continue
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     snippetText(it.Title, 200),
			Snippet:   snippetText(it.Description, maxSnippetRunes),
			Published: parseTimeAny(it.PubDate),
			Lang:      "zh",
		})
	}
	return finishResults(p.name, req, results, limit), nil
}
