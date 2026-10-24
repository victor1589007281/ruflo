package providers

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// stackExchangeProvider 通过 StackExchange API 搜索问答。
type stackExchangeProvider struct {
	*base
	endpoint string
	apiKey   string
}

// newStackExchange 构造 StackExchange 源 (直连, L2)。
func newStackExchange(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://api.stackexchange.com/2.3/search/advanced"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("stackexchange", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	return &stackExchangeProvider{base: b, endpoint: endpoint, apiKey: strings.TrimSpace(pc.APIKey)}, nil
}

// Search 调用 /2.3/search/advanced。
func (p *stackExchangeProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	params := url.Values{
		"order":    {"desc"},
		"sort":     {"relevance"},
		"q":        {query},
		"site":     {"stackoverflow"},
		"pagesize": {strconv.Itoa(limit)},
		"filter":   {"default"},
	}
	if p.apiKey != "" {
		params.Set("key", p.apiKey)
	}
	target := withQuery(p.endpoint, params)

	var payload struct {
		Items []struct {
			Title        string   `json:"title"`
			Link         string   `json:"link"`
			Body         string   `json:"body"`
			Score        int      `json:"score"`
			CreationDate int64    `json:"creation_date"`
			Tags         []string `json:"tags"`
		} `json:"items"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Items))
	for _, it := range payload.Items {
		link := strings.TrimSpace(it.Link)
		if !isHTTPURL(link) {
			continue
		}
		snippet := snippetText(it.Body, maxSnippetRunes)
		if len(it.Tags) > 0 {
			tags := "[" + strings.Join(it.Tags, ", ") + "]"
			snippet = strings.TrimSpace(tags + " " + snippet)
		}
		var published time.Time
		if it.CreationDate > 0 {
			published = time.Unix(it.CreationDate, 0).UTC()
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     snippetText(html.UnescapeString(it.Title), 200),
			Snippet:   snippet,
			Score:     float64(it.Score),
			Published: published,
			Lang:      "en",
		})
	}
	return finishResults(p.name, req, results, limit), nil
}
