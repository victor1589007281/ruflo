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

// wikiPageBase 是词条页 URL 前缀。
const wikiPageBase = "https://zh.wikipedia.org/wiki/"

// wikipediaProvider 通过 MediaWiki 搜索 API 提供实体/百科类结果。
//
// 设计文档 §3.5: 直连超时, 必须经代理; 只声明 Search, 不声明 Fetch。
type wikipediaProvider struct {
	*base
	endpoint string
}

// newWikipedia 构造 Wikipedia API 源 (代理, L2)。
func newWikipedia(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://zh.wikipedia.org/w/api.php"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("wikipedia", cfg, pc, eg, search.ModeProxy, search.TierL2, 0.8)
	b.kinds = search.KindSet(search.KindSearch)
	return &wikipediaProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 MediaWiki list=search 接口。
func (p *wikipediaProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	target := withQuery(p.endpoint, url.Values{
		"action":   {"query"},
		"list":     {"search"},
		"format":   {"json"},
		"srsearch": {query},
		"srlimit":  {strconv.Itoa(limit)},
	})
	var payload struct {
		Query struct {
			Search []struct {
				Title     string `json:"title"`
				Snippet   string `json:"snippet"`
				Timestamp string `json:"timestamp"`
			} `json:"search"`
		} `json:"query"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Query.Search))
	for _, hit := range payload.Query.Search {
		title := strings.TrimSpace(hit.Title)
		if title == "" {
			continue
		}
		page := wikiPageBase + url.PathEscape(strings.ReplaceAll(title, " ", "_"))
		results = append(results, search.Result{
			URL:       page,
			Title:     title,
			Snippet:   snippetText(hit.Snippet, maxSnippetRunes),
			Published: parseTimeAny(hit.Timestamp),
			Lang:      "zh",
		})
	}
	return finishResults(p.name, req, results, limit), nil
}
