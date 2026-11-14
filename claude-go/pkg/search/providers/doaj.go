package providers

// doaj.go —— DOAJ 开放获取期刊文章检索（免 key）。
//
// DOAJ 的 robots.txt 明确允许检索与 AI 引用（Content-signal: search=yes），
// 只禁止 /search、/account 等交互路径；这里用的是 /api/search/articles，
// 不在禁止列表里。开放获取全文链接可直接交给 WebFetch 读取。
// API: https://doaj.org/api/search/articles/{query}?pageSize=...
//
// 频率：公共 API，按 1 req/s 节流。

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

type doajProvider struct {
	*base
	endpoint string
}

// newDOAJ 构造 DOAJ 源（直连，L2）。
func newDOAJ(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://doaj.org/api/search/articles/"
	}
	// 端点末尾必须保留 /，查询词会作为 path 段追加。
	raw = strings.TrimRight(raw, "/") + "/"
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("doaj", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	b.setMinInterval(time.Second)
	return &doajProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 DOAJ 文章检索。
func (p *doajProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
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

	target := p.endpoint + url.PathEscape(query) + "?" + url.Values{
		"pageSize": {strconv.Itoa(limit)},
	}.Encode()
	var payload struct {
		Results []struct {
			BibJSON struct {
				Title    string `json:"title"`
				Abstract string `json:"abstract"`
				Year     int    `json:"year"`
				Link     []struct {
					URL  string `json:"url"`
					Type string `json:"type"`
				} `json:"link"`
				Identifier []struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"identifier"`
				Journal struct {
					Title string `json:"title"`
				} `json:"journal"`
			} `json:"bibjson"`
		} `json:"results"`
	}
	if _, err := doJSON(cctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Results))
	for _, it := range payload.Results {
		link := ""
		for _, l := range it.BibJSON.Link {
			if strings.TrimSpace(l.URL) != "" {
				link = l.URL
				break
			}
		}
		if link == "" {
			for _, id := range it.BibJSON.Identifier {
				if strings.EqualFold(id.Type, "doi") {
					link = doiURL(id.ID)
					break
				}
			}
		}
		if link == "" {
			continue
		}
		title := strings.Join(strings.Fields(it.BibJSON.Title), " ")
		if title == "" {
			title = link
		}
		snippet := it.BibJSON.Abstract
		if j := strings.TrimSpace(it.BibJSON.Journal.Title); j != "" {
			snippet = strings.TrimSpace(j + "：" + snippet)
		}
		published := time.Time{}
		if it.BibJSON.Year > 0 {
			published = time.Date(it.BibJSON.Year, time.January, 1, 0, 0, 0, 0, time.UTC)
		}
		results = append(results, search.Result{
			URL:       upgradeHTTPS(link),
			Title:     title,
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Published: published,
		})
	}
	return finishResults(p.name, req, results, limit), nil
}
