package providers

// arxiv.go —— arXiv 预印本检索（免 key）。
//
// 覆盖论文原始版本，适合「论文 / 预印本 / 最新研究」类查询。arXiv 的
// export 域 robots.txt 禁止爬取 HTML，但其官方 API 使用条款明确允许程序化
// 访问，并要求“每次请求间隔至少 3 秒”；这里的 minInterval 就是按该条款设的。
// 不做任何 HTML 抓取，只走 /api/query。
// API: https://export.arxiv.org/api/query

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

type arxivProvider struct {
	*base
	endpoint string
}

// newArxiv 构造 arXiv 源（直连，L2）。
func newArxiv(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://export.arxiv.org/api/query"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("arxiv", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	// arXiv API 条款：请求间隔至少 3 秒。
	b.setMinInterval(3 * time.Second)
	return &arxivProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 arXiv Atom API。
func (p *arxivProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	cctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()
	if err := p.pace(cctx); err != nil {
		return nil, search.Wrap(p.name, search.ErrTimeout, err)
	}
	limit := p.limitOf(req.Limit)

	// 用引号包住整句，避免多词被 arXiv 当成 AND 后过窄。
	q := `all:"` + strings.ReplaceAll(query, `"`, " ") + `"`
	target := withQuery(p.endpoint, url.Values{
		"search_query": {q},
		"max_results":  {strconv.Itoa(limit)},
		"sortBy":       {"relevance"},
	})
	data, _, _, err := doRequest(cctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/atom+xml"},
	}, nil)
	if err != nil {
		return nil, err
	}
	var feed struct {
		Entries []struct {
			ID        string `xml:"id"`
			Title     string `xml:"title"`
			Summary   string `xml:"summary"`
			Published string `xml:"published"`
			Authors   []struct {
				Name string `xml:"name"`
			} `xml:"author"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, search.Wrap(p.name, search.ErrUnavailable, err)
	}

	results := make([]search.Result, 0, len(feed.Entries))
	for _, it := range feed.Entries {
		link := strings.TrimSpace(it.ID)
		if link == "" {
			continue
		}
		link = upgradeHTTPS(link)
		title := strings.Join(strings.Fields(it.Title), " ")
		if title == "" {
			title = link
		}
		snippet := strings.Join(strings.Fields(it.Summary), " ")
		if len(it.Authors) > 0 {
			names := make([]string, 0, len(it.Authors))
			for _, a := range it.Authors {
				if n := strings.TrimSpace(a.Name); n != "" {
					names = append(names, n)
				}
			}
			if len(names) > 0 {
				snippet = strings.TrimSpace(strings.Join(names, ", ") + "：" + snippet)
			}
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     title,
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Published: parseTimeAny(it.Published),
			Lang:      "en",
		})
	}
	return finishResults(p.name, req, results, limit), nil
}
