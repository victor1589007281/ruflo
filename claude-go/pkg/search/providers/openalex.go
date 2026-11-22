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
	if _, err := p.doWithRetry(ctx, target, &payload); err != nil {
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

// openalexRetryBackoffs 是 429/5xx 的退避序列 (秒)。
//
// 为什么重试做在源内部而不是交给 manager:
// 2026-11-19 事故复盘 —— 早上 08:10 连续 5 次 provider_error 让熔断器打开,
// 随后的 429 又把源打进 60s 配额冷却, 冷却结束单探针再失败 → 再熔断,
// 15 分钟内 openalex 对全部意图 (含学术) 不可用, 学术查询全部降级成 partial。
// OpenAlex 官方限流是 10 req/s 的短窗口, 429/5xx 绝大多数是秒级毛刺,
// 源内重试两次 (1s+2s) 即可吸收; 真到日配额耗尽, 重试仍会 429,
// 错误交回 manager 走正常冷却, 行为不劣化。
var openalexRetryBackoffs = []time.Duration{time.Second, 2 * time.Second}

// doWithRetry 执行 GET JSON, 对 429 (ErrRateLimited) 与 5xx (ErrUnavailable)
// 按 openalexRetryBackoffs 退避重试; 其余错误种类 (封锁/参数/超时) 立即返回。
// 重试一律受 requestContext 的 deadline 约束, 超时由 ctx 掐断。
func (p *openAlexProvider) doWithRetry(ctx context.Context, target string, out any) (int, error) {
	for attempt := 0; ; attempt++ {
		status, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
			"Accept": {"application/json"},
		}, nil, out)
		if err == nil {
			return status, nil
		}
		kind := search.KindOf(err)
		// 5xx 与 429 才值得重试; doJSON 的 JSON 解码错误同样归 ErrUnavailable
		// 但 status 是 2xx, 用 status 区分, 避免对病体响应空转重试。
		retryable := kind == search.ErrRateLimited || (kind == search.ErrUnavailable && status >= 500)
		if !retryable {
			return status, err
		}
		if attempt >= len(openalexRetryBackoffs) {
			return status, err
		}
		// 剩余预算不够完成退避+一次请求时就别硬撑, 直接交回错误。
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining < openalexRetryBackoffs[attempt]+500*time.Millisecond {
				return status, err
			}
		}
		select {
		case <-ctx.Done():
			return status, err
		case <-time.After(openalexRetryBackoffs[attempt]):
		}
	}
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
