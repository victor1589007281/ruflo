package providers

// gdelt.go —— GDELT DOC 2.0 全球新闻与广播监测检索。
//
// api.gdeltproject.org 的 DOC 2.0 端点对匿名调用开放 (官方条款: 持续流量
// 限 1 次/5 秒), 收录 100+ 语言的网页/广播/印刷新闻, 时效按小时计 —— 是
// gnews (RSS 跳转链) 与 bing_rss (长尾噪声) 之外第三条独立新闻通道, 对
// 非英语与跨境新闻的覆盖明显更好。超限返回 429 + 提示文, 由 base 的熔断
// 与 5 秒节流消化, 不会打爆上游。

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

// gdeltPace 是 GDELT 官方对持续流量的间隔要求 (5 秒/次)。
const gdeltPace = 5 * time.Second

// gdeltSeendateLayout 是 GDELT 的 YYYYMMDDHHMMSS 时间戳。
const gdeltSeendateLayout = "20060102150405"

type gdeltProvider struct {
	*base
	endpoint string
}

// newGDELT 构造 GDELT DOC 2.0 源 (直连, L2, 5 秒节流)。
func newGDELT(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://api.gdeltproject.org/api/v2/doc/doc"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("gdelt", cfg, pc, eg, search.ModeDirect, search.TierL2, 0.7)
	b.kinds = search.KindSet(search.KindSearch)
	b.setMinInterval(gdeltPace)
	return &gdeltProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 DOC 2.0 artlist JSON。
func (p *gdeltProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()
	if err := p.pace(ctx); err != nil {
		return nil, search.Wrap(p.name, search.ErrTimeout, err)
	}

	target := withQuery(p.endpoint, url.Values{
		"query":      {query},
		"mode":       {"artlist"},
		"maxrecords": {strconv.Itoa(limit)},
		"format":     {"json"},
		"sort":       {"HybridRel"},
	})
	var payload struct {
		Articles []struct {
			URL           string `json:"url"`
			Title         string `json:"title"`
			SeenDate      string `json:"seendate"`
			Domain        string `json:"domain"`
			Language      string `json:"language"`
			SourceCountry string `json:"sourcecountry"`
		} `json:"articles"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Articles))
	for _, a := range payload.Articles {
		link := strings.TrimSpace(a.URL)
		if !isHTTPURL(link) {
			continue
		}
		title := strings.TrimSpace(a.Title)
		if title == "" {
			title = link
		}
		// artlist 不带摘要; 用 域名·国家·时间 给出可核对的出处信息。
		var meta []string
		if d := strings.TrimSpace(a.Domain); d != "" {
			meta = append(meta, d)
		}
		if c := strings.TrimSpace(a.SourceCountry); c != "" {
			meta = append(meta, c)
		}
		if t := gdeltTime(a.SeenDate); !t.IsZero() {
			meta = append(meta, t.Format("2006-01-02 15:04"))
		}
		lang := strings.ToLower(strings.TrimSpace(a.Language))
		switch lang {
		case "chinese", "中文":
			lang = "zh"
		case "":
			lang = req.Language
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     title,
			Snippet:   strings.Join(meta, " · "),
			Published: gdeltTime(a.SeenDate),
			Lang:      lang,
		})
	}
	return finishResults(p.name, req, results, limit), nil
}

// gdeltTime 解析 GDELT 的 YYYYMMDDHHMMSS 时间戳; 失败返回零值。
func gdeltTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(gdeltSeendateLayout, s); err == nil {
		return t
	}
	return time.Time{}
}
