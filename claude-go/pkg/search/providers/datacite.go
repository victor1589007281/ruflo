package providers

// datacite.go —— DataCite DOI 检索（免 key）。
//
// 研究数据集、机构报告、软件与预注册材料通常有 DataCite DOI；这些材料
// 不在通用网页检索里稳定出现，但对「数据从哪来 / 报告原文」类问题是一手来源。
// API: https://api.datacite.org/dois?query=...
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

type dataCiteProvider struct {
	*base
	endpoint string
}

// newDataCite 构造 DataCite 源（直连，L2）。
func newDataCite(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://api.datacite.org/dois"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("datacite", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	b.setMinInterval(time.Second)
	return &dataCiteProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 /dois?query=...
func (p *dataCiteProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
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
		"query":     {query},
		"page[size]": {strconv.Itoa(limit)},
	})
	var payload struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				DOI             string `json:"doi"`
				URL             string `json:"url"`
				Publisher       string `json:"publisher"`
				PublicationYear int    `json:"publicationYear"`
				Language        string `json:"language"`
				Titles          []struct {
					Title string `json:"title"`
				} `json:"titles"`
				Descriptions []struct {
					Description     string `json:"description"`
					DescriptionType string `json:"descriptionType"`
				} `json:"descriptions"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if _, err := doJSON(cctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Data))
	for _, it := range payload.Data {
		doi := strings.TrimSpace(it.Attributes.DOI)
		link := firstNonEmpty(strings.TrimSpace(it.Attributes.URL), doiURL(doi), strings.TrimSpace(it.ID))
		if link == "" {
			continue
		}
		title := ""
		if len(it.Attributes.Titles) > 0 {
			title = strings.Join(strings.Fields(it.Attributes.Titles[0].Title), " ")
		}
		if title == "" {
			title = link
		}
		snippet := ""
		for _, d := range it.Attributes.Descriptions {
			if strings.TrimSpace(d.Description) != "" {
				snippet = d.Description
				break
			}
		}
		if strings.TrimSpace(snippet) == "" && it.Attributes.Publisher != "" {
			snippet = it.Attributes.Publisher
		}
		published := time.Time{}
		if it.Attributes.PublicationYear > 0 {
			published = time.Date(it.Attributes.PublicationYear, time.January, 1, 0, 0, 0, 0, time.UTC)
		}
		results = append(results, search.Result{
			URL:       upgradeHTTPS(link),
			Title:     title,
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Published: published,
			Lang:      normalizeLangField(it.Attributes.Language),
		})
	}
	return finishResults(p.name, req, results, limit), nil
}

// doiURL 把裸 DOI 转成可点击链接。
func doiURL(doi string) string {
	doi = strings.TrimSpace(doi)
	if doi == "" {
		return ""
	}
	if strings.HasPrefix(doi, "http://") || strings.HasPrefix(doi, "https://") {
		return upgradeHTTPS(doi)
	}
	return "https://doi.org/" + doi
}
