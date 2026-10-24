package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/anthropic/claude-go/pkg/search"
)

// githubProvider 通过 GitHub REST 搜索仓库。
type githubProvider struct {
	*base
	endpoint string
	apiKey   string
}

// newGitHub 构造 GitHub 仓库搜索源 (直连, L2)。
//
// APIKey 可选; 未配置时按未认证的 10 req/min 限流工作。
func newGitHub(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://api.github.com/search/repositories"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("github", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	return &githubProvider{base: b, endpoint: endpoint, apiKey: strings.TrimSpace(pc.APIKey)}, nil
}

// Search 调用 /search/repositories。
func (p *githubProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	target := withQuery(p.endpoint, url.Values{
		"q":        {query},
		"per_page": {strconv.Itoa(limit)},
	})
	header := http.Header{
		"Accept":               {"application/vnd.github+json"},
		"X-GitHub-Api-Version": {"2022-11-28"},
	}
	if p.apiKey != "" {
		header.Set("Authorization", "Bearer "+p.apiKey)
	}

	var payload struct {
		Items []struct {
			FullName        string `json:"full_name"`
			HTMLURL         string `json:"html_url"`
			Description     string `json:"description"`
			Language        string `json:"language"`
			StargazersCount int    `json:"stargazers_count"`
			UpdatedAt       string `json:"updated_at"`
		} `json:"items"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, header, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Items))
	for _, it := range payload.Items {
		link := strings.TrimSpace(it.HTMLURL)
		if !isHTTPURL(link) {
			continue
		}
		snippet := strings.TrimSpace(it.Description)
		if it.StargazersCount > 0 {
			snippet = strings.TrimSpace(fmt.Sprintf("%s · ★ %d", snippet, it.StargazersCount))
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     firstNonEmpty(strings.TrimSpace(it.FullName), link),
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Published: parseTimeAny(it.UpdatedAt),
			Lang:      strings.ToLower(strings.TrimSpace(it.Language)),
		})
	}
	return finishResults(p.name, req, results, limit), nil
}
