package providers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// openAlexProvider 通过 OpenAlex 搜索学术作品。
type openAlexProvider struct {
	*base
	endpoint string
}

// newOpenAlex 构造 OpenAlex 源 (直连, L2)。
func newOpenAlex(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://api.openalex.org/works"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("openalex", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	return &openAlexProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 /works?search=...
func (p *openAlexProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	target := withQuery(p.endpoint, url.Values{
		"search":   {query},
		"per-page": {strconv.Itoa(limit)},
	})
	var payload struct {
		Results []struct {
			ID              string `json:"id"`
			DOI             string `json:"doi"`
			DisplayName     string `json:"display_name"`
			PublicationDate string `json:"publication_date"`
			PublicationYear int    `json:"publication_year"`
			PrimaryLocation struct {
				LandingPageURL string `json:"landing_page_url"`
			} `json:"primary_location"`
			AbstractInvertedIndex map[string][]int `json:"abstract_inverted_index"`
		} `json:"results"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Results))
	for _, it := range payload.Results {
		link := firstNonEmpty(strings.TrimSpace(it.DOI), strings.TrimSpace(it.PrimaryLocation.LandingPageURL), strings.TrimSpace(it.ID))
		if link == "" {
			continue
		}
		if strings.HasPrefix(link, "10.") {
			link = "https://doi.org/" + link
		}
		title := strings.TrimSpace(it.DisplayName)
		if title == "" {
			title = link
		}
		snippet := abstractFromInverted(it.AbstractInvertedIndex)
		if snippet == "" && it.PublicationYear > 0 {
			snippet = strconv.Itoa(it.PublicationYear)
		}
		published := parseTimeAny(it.PublicationDate)
		if published.IsZero() && it.PublicationYear > 0 {
			published = time.Date(it.PublicationYear, time.January, 1, 0, 0, 0, 0, time.UTC)
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     title,
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Published: published,
			Lang:      "en",
		})
	}
	return finishResults(p.name, req, results, limit), nil
}

// abstractFromInverted 从 OpenAlex 的倒排索引还原摘要。
func abstractFromInverted(idx map[string][]int) string {
	if len(idx) == 0 {
		return ""
	}
	maxPos := 0
	for _, positions := range idx {
		for _, pos := range positions {
			if pos > maxPos {
				maxPos = pos
			}
		}
	}
	words := make([]string, maxPos+1)
	for word, positions := range idx {
		for _, pos := range positions {
			if pos >= 0 && pos < len(words) {
				words[pos] = word
			}
		}
	}
	return strings.Join(words, " ")
}
