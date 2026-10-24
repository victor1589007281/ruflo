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

// hnProvider 通过 HN Algolia 搜索 Hacker News 条目与评论。
type hnProvider struct {
	*base
	endpoint string
}

// hnItemURL 是单条目的兜底链接。
const hnItemURL = "https://news.ycombinator.com/item?id="

// newHN 构造 HN Algolia 源 (直连, L2)。
func newHN(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://hn.algolia.com/api/v1/search"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("hn", cfg, pc, eg, search.ModeDirect, search.TierL2, 0.7)
	b.kinds = search.KindSet(search.KindSearch)
	return &hnProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 /api/v1/search。
func (p *hnProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	target := withQuery(p.endpoint, url.Values{
		"query":       {query},
		"hitsPerPage": {strconv.Itoa(limit)},
	})
	var payload struct {
		Hits []struct {
			ObjectID    string `json:"objectID"`
			Title       string `json:"title"`
			StoryTitle  string `json:"story_title"`
			URL         string `json:"url"`
			StoryURL    string `json:"story_url"`
			CommentText string `json:"comment_text"`
			StoryText   string `json:"story_text"`
			CreatedAt   string `json:"created_at"`
			Points      int    `json:"points"`
			NumComments int    `json:"num_comments"`
		} `json:"hits"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Hits))
	for _, hit := range payload.Hits {
		link := firstNonEmpty(strings.TrimSpace(hit.URL), strings.TrimSpace(hit.StoryURL))
		if !isHTTPURL(link) {
			if hit.ObjectID == "" {
				continue
			}
			link = hnItemURL + url.QueryEscape(hit.ObjectID)
		}
		title := firstNonEmpty(strings.TrimSpace(hit.Title), strings.TrimSpace(hit.StoryTitle))
		if title == "" {
			title = fmt.Sprintf("Hacker News item %s", hit.ObjectID)
		}
		snippet := snippetText(firstNonEmpty(hit.CommentText, hit.StoryText), maxSnippetRunes)
		if snippet == "" && (hit.Points > 0 || hit.NumComments > 0) {
			snippet = fmt.Sprintf("%d points, %d comments", hit.Points, hit.NumComments)
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     title,
			Snippet:   snippet,
			Score:     float64(hit.Points),
			Published: parseTimeAny(hit.CreatedAt),
			Lang:      "en",
		})
	}
	return finishResults(p.name, req, results, limit), nil
}
