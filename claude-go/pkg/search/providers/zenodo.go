package providers

// zenodo.go —— Zenodo 开放获取记录检索（免 key）。
//
// Zenodo 由 CERN 运营，承载数据集、技术报告、软件与会议材料；与 DataCite
// 有重叠但收录面不同，二者同时保留可以发现只在一个库里登记的材料。
// API: https://zenodo.org/api/records?q=...
//
// 频率：公共 API，按 1 req/s 节流。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

type zenodoProvider struct {
	*base
	endpoint string
}

// newZenodo 构造 Zenodo 源（直连，L2）。
func newZenodo(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://zenodo.org/api/records"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("zenodo", cfg, pc, eg, search.ModeDirect, search.TierL2, 0.9)
	b.kinds = search.KindSet(search.KindSearch)
	b.setMinInterval(time.Second)
	return &zenodoProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 /api/records?q=...
func (p *zenodoProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
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

	target := withQuery(p.endpoint, url.Values{
		"q":    {query},
		"size": {strconv.Itoa(limit)},
		"sort": {"bestmatch"},
	})
	var payload struct {
		Hits struct {
			Hits []struct {
				ID      int64  `json:"id"`
				DOILink string `json:"doi_url"`
				Links   struct {
					SelfHTML string `json:"self_html"`
				} `json:"links"`
				Metadata struct {
					Title           string `json:"title"`
					Description     string `json:"description"`
					PublicationDate string `json:"publication_date"`
					Language        string `json:"language"`
					ResourceType    struct {
						Title string `json:"title"`
					} `json:"resource_type"`
				} `json:"metadata"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if _, err := doJSON(cctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Hits.Hits))
	for _, it := range payload.Hits.Hits {
		link := firstNonEmpty(strings.TrimSpace(it.DOILink), strings.TrimSpace(it.Links.SelfHTML))
		if link == "" && it.ID > 0 {
			link = fmt.Sprintf("https://zenodo.org/records/%d", it.ID)
		}
		if link == "" {
			continue
		}
		title := strings.Join(strings.Fields(it.Metadata.Title), " ")
		if title == "" {
			title = link
		}
		snippet := stripHTML(it.Metadata.Description)
		if rt := strings.TrimSpace(it.Metadata.ResourceType.Title); rt != "" {
			snippet = strings.TrimSpace(rt + "：" + snippet)
		}
		results = append(results, search.Result{
			URL:       upgradeHTTPS(link),
			Title:     title,
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Published: parseTimeAny(it.Metadata.PublicationDate),
			Lang:      normalizeLangField(it.Metadata.Language),
		})
	}
	return finishResults(p.name, req, results, limit), nil
}
