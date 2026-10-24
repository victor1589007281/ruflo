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

// crossrefProvider 通过 Crossref 搜索 DOI 作品。
type crossrefProvider struct {
	*base
	endpoint string
}

// newCrossref 构造 Crossref 源 (直连, L2)。
func newCrossref(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://api.crossref.org/works"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("crossref", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	return &crossrefProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 /works?query=...
func (p *crossrefProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	target := withQuery(p.endpoint, url.Values{
		"query": {query},
		"rows":  {strconv.Itoa(limit)},
	})
	var payload struct {
		Message struct {
			Items []struct {
				Title          []string `json:"title"`
				URL            string   `json:"URL"`
				DOI            string   `json:"DOI"`
				Abstract       string   `json:"abstract"`
				ContainerTitle []string `json:"container-title"`
				Published      struct {
					DateParts [][]int `json:"date-parts"`
				} `json:"published"`
				Author []struct {
					Given  string `json:"given"`
					Family string `json:"family"`
				} `json:"author"`
			} `json:"items"`
		} `json:"message"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Message.Items))
	for _, it := range payload.Message.Items {
		link := strings.TrimSpace(it.URL)
		if link == "" && strings.TrimSpace(it.DOI) != "" {
			link = "https://doi.org/" + strings.TrimSpace(it.DOI)
		}
		if !isHTTPURL(link) {
			continue
		}
		title := link
		if len(it.Title) > 0 && strings.TrimSpace(it.Title[0]) != "" {
			title = strings.TrimSpace(it.Title[0])
		}
		snippet := snippetText(it.Abstract, maxSnippetRunes)
		if snippet == "" && len(it.ContainerTitle) > 0 {
			snippet = strings.Join(it.ContainerTitle, ", ")
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     title,
			Snippet:   snippet,
			Published: parseDateParts(it.Published.DateParts),
			Lang:      "en",
		})
	}
	return finishResults(p.name, req, results, limit), nil
}
