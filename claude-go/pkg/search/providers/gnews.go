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
		// Google News RSS 的 <link> 是 news.google.com/rss/articles/... 跳转页,
		// 打开后只有 Google News 空壳 (实测多份报告抓回来都是壳页)。它不是
		// 可引用来源, 继续往下传只会诱导模型把跳转链写进证据表。
		if googleNewsRedirect(link) {
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

// googleNewsRedirect 判断链接是否是 Google News 的站内跳转页。
func googleNewsRedirect(link string) bool {
	u, err := url.Parse(link)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h != "news.google.com" && h != "www.news.google.com" {
		return false
	}
	return strings.HasPrefix(u.Path, "/rss/articles")
}
