package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/anthropic/claude-go/pkg/search"
)

// pypiNameRE 只匹配合法 Python 包名; 其余 query 不做猜测。
var pypiNameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// pypiProvider 用 PyPI JSON API 精确查询单个包名。
type pypiProvider struct {
	*base
	endpoint string
}

// pypiProjectURL 是无 package_url 时的兜底页面。
const pypiProjectURL = "https://pypi.org/project/"

// newPyPI 构造 PyPI 源 (直连, L2)。
func newPyPI(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://pypi.org/pypi"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("pypi", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	return &pypiProvider{base: b, endpoint: endpoint}, nil
}

// Search 对合法包名调用 /pypi/<name>/json。
func (p *pypiProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	name := strings.TrimSpace(req.Query)
	if name == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	if !pypiNameRE.MatchString(name) {
		return nil, search.Wrap(p.name, search.ErrUnsupported, fmt.Errorf("%q is not a valid PyPI package name", name))
	}
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	target := strings.TrimRight(p.endpoint, "/") + "/" + url.PathEscape(name) + "/json"
	var payload struct {
		Info struct {
			Name        string            `json:"name"`
			Version     string            `json:"version"`
			Summary     string            `json:"summary"`
			HomePage    string            `json:"home_page"`
			PackageURL  string            `json:"package_url"`
			ProjectURL  string            `json:"project_url"`
			ProjectURLs map[string]string `json:"project_urls"`
		} `json:"info"`
	}
	status, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload)
	if err != nil {
		if status == http.StatusNotFound {
			// 包不存在是合法的空结果, 不是故障。
			return nil, nil
		}
		return nil, err
	}

	link := strings.TrimSpace(payload.Info.PackageURL)
	if link == "" {
		link = strings.TrimSpace(payload.Info.ProjectURL)
	}
	if link == "" {
		for _, key := range []string{"Homepage", "Source", "Repository"} {
			if v := strings.TrimSpace(payload.Info.ProjectURLs[key]); v != "" {
				link = v
				break
			}
		}
	}
	if link == "" {
		link = strings.TrimSpace(payload.Info.HomePage)
	}
	if !isHTTPURL(link) {
		link = pypiProjectURL + url.PathEscape(name) + "/"
	}
	title := firstNonEmpty(strings.TrimSpace(payload.Info.Name), name)
	snippet := strings.TrimSpace(payload.Info.Summary)
	if v := strings.TrimSpace(payload.Info.Version); v != "" {
		snippet = strings.TrimSpace(fmt.Sprintf("%s · v%s", snippet, v))
	}
	return finishResults(p.name, req, []search.Result{{
		URL:     link,
		Title:   title,
		Snippet: snippetText(snippet, maxSnippetRunes),
		Score:   1.0,
		Lang:    "en",
	}}, 1), nil
}
