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

// npmProvider 通过 npm Registry 搜索包。
type npmProvider struct {
	*base
	endpoint string
}

// npmPackageURL 是无 links 时的兜底页面。
const npmPackageURL = "https://www.npmjs.com/package/"

// newNPM 构造 npm Registry 源 (直连, L2)。
func newNPM(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://registry.npmjs.org/-/v1/search"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("npm", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	return &npmProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 /-/v1/search。
func (p *npmProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	target := withQuery(p.endpoint, url.Values{
		"text": {query},
		"size": {strconv.Itoa(limit)},
	})
	var payload struct {
		Objects []struct {
			Package struct {
				Name        string `json:"name"`
				Version     string `json:"version"`
				Description string `json:"description"`
				Date        string `json:"date"`
				Links       struct {
					NPM        string `json:"npm"`
					Homepage   string `json:"homepage"`
					Repository string `json:"repository"`
				} `json:"links"`
			} `json:"package"`
			Score struct {
				Final float64 `json:"final"`
			} `json:"score"`
		} `json:"objects"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Objects))
	for _, obj := range payload.Objects {
		pkg := obj.Package
		name := strings.TrimSpace(pkg.Name)
		if name == "" {
			continue
		}
		link := firstNonEmpty(strings.TrimSpace(pkg.Links.NPM), strings.TrimSpace(pkg.Links.Homepage), strings.TrimSpace(pkg.Links.Repository))
		if !isHTTPURL(link) {
			link = npmPackageURL + url.PathEscape(name)
		}
		snippet := strings.TrimSpace(pkg.Description)
		if v := strings.TrimSpace(pkg.Version); v != "" {
			snippet = strings.TrimSpace(fmt.Sprintf("%s · v%s", snippet, v))
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     name,
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Score:     obj.Score.Final,
			Published: parseTimeAny(pkg.Date),
			Lang:      "en",
		})
	}
	return finishResults(p.name, req, results, limit), nil
}
