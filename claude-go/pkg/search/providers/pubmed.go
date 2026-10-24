package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/anthropic/claude-go/pkg/search"
)

// pubmedProvider 通过 NCBI E-utilities 的 esearch + esummary 两步检索。
type pubmedProvider struct {
	*base
	baseURL string
	apiKey  string
}

// pubmedArticleURL 是文献详情页前缀。
const pubmedArticleURL = "https://pubmed.ncbi.nlm.nih.gov/"

// newPubMed 构造 PubMed 源 (直连, L2)。
func newPubMed(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("pubmed", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	return &pubmedProvider{base: b, baseURL: strings.TrimRight(endpoint, "/"), apiKey: strings.TrimSpace(pc.APIKey)}, nil
}

// Search 先 esearch 拿 ID 列表, 再 esummary 取元数据。
func (p *pubmedProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	ids, err := p.searchIDs(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	results, err := p.summaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	return finishResults(p.name, req, results, limit), nil
}

// searchIDs 调用 esearch.fcgi 并返回 PMID 列表。
func (p *pubmedProvider) searchIDs(ctx context.Context, query string, limit int) ([]string, error) {
	params := url.Values{
		"db":      {"pubmed"},
		"term":    {query},
		"retmax":  {strconv.Itoa(limit)},
		"retmode": {"json"},
		"tool":    {"claude-go-search"},
	}
	if p.apiKey != "" {
		params.Set("api_key", p.apiKey)
	}
	target := withQuery(p.baseURL+"/esearch.fcgi", params)

	var payload struct {
		Result struct {
			IDList []string `json:"idlist"`
		} `json:"esearchresult"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Result.IDList, nil
}

// pubmedItem 是 esummary 的单条记录。
type pubmedItem struct {
	UID         string `json:"uid"`
	Title       string `json:"title"`
	PubDate     string `json:"pubdate"`
	Source      string `json:"source"`
	FullJournal string `json:"fulljournalname"`
	ELocationID string `json:"elocationid"`
	Authors     []struct {
		Name string `json:"name"`
	} `json:"authors"`
}

// summaries 调用 esummary.fcgi 并把记录转成 search.Result。
func (p *pubmedProvider) summaries(ctx context.Context, ids []string) ([]search.Result, error) {
	params := url.Values{
		"db":      {"pubmed"},
		"id":      {strings.Join(ids, ",")},
		"retmode": {"json"},
		"tool":    {"claude-go-search"},
	}
	if p.apiKey != "" {
		params.Set("api_key", p.apiKey)
	}
	target := withQuery(p.baseURL+"/esummary.fcgi", params)

	var payload struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	order := ids
	var uids []string
	if raw, ok := payload.Result["uids"]; ok {
		if err := json.Unmarshal(raw, &uids); err == nil && len(uids) > 0 {
			order = uids
		}
	}

	results := make([]search.Result, 0, len(order))
	for _, id := range order {
		raw, ok := payload.Result[id]
		if !ok {
			continue
		}
		var item pubmedItem
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = fmt.Sprintf("PubMed %s", id)
		}
		results = append(results, search.Result{
			URL:       pubmedArticleURL + url.PathEscape(id) + "/",
			Title:     title,
			Snippet:   pubmedSnippet(item),
			Published: parseTimeAny(item.PubDate),
			Lang:      "en",
		})
	}
	return results, nil
}

// pubmedSnippet 组合作者、期刊与电子定位号。
func pubmedSnippet(item pubmedItem) string {
	var parts []string
	if len(item.Authors) > 0 {
		names := make([]string, 0, 3)
		for i, a := range item.Authors {
			if i >= 3 {
				names = append(names, "et al")
				break
			}
			if name := strings.TrimSpace(a.Name); name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			parts = append(parts, strings.Join(names, ", "))
		}
	}
	journal := firstNonEmpty(strings.TrimSpace(item.FullJournal), strings.TrimSpace(item.Source))
	if journal != "" {
		parts = append(parts, journal)
	}
	if loc := strings.TrimSpace(item.ELocationID); loc != "" {
		parts = append(parts, loc)
	}
	return snippetText(strings.Join(parts, ". "), maxSnippetRunes)
}
