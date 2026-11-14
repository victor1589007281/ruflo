package providers

// worldbank.go —— 世界银行文档与报告检索（免 key）。
//
// 覆盖 World Development Report、国别经济备忘录、行业评估、项目文件等
// 一手公开材料，是「经济年报 / 行业报告」类问题的权威来源。
// API: https://search.worldbank.org/api/v2/wds
//
// 频率：官方未给硬性额度，但这是共享的公共检索端点；按 1 req/s 节流，
// 结果交给 Manager 的 LRU 缓存（默认 10 分钟）复用。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

type worldBankProvider struct {
	*base
	endpoint string
}

// newWorldBank 构造世界银行文档源（直连，L2）。
func newWorldBank(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://search.worldbank.org/api/v2/wds"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("worldbank", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.1)
	b.kinds = search.KindSet(search.KindSearch)
	b.setMinInterval(time.Second)
	return &worldBankProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 WDS 文档检索。
func (p *worldBankProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
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

	results, err := p.query(cctx, query, limit)
	if err != nil {
		return nil, err
	}
	// WDS 的 qterm 是严格 AND：长查询里多一个年份就可能把总命中打成 0
	// （实测 "world bank annual report on inflation 2024" = 0，去掉年份后
	// = 3706）。只在空结果时去掉年份重试一次，避免正常查询多打一次请求。
	if len(results) == 0 {
		if alt := withoutYears(query); alt != query {
			if more, err := p.query(cctx, alt, limit); err == nil {
				results = more
			}
		}
	}
	return finishResults(p.name, req, results, limit), nil
}

// query 执行一次 WDS 检索并转换结果。
func (p *worldBankProvider) query(ctx context.Context, query string, limit int) ([]search.Result, error) {
	target := withQuery(p.endpoint, url.Values{
		"format":      {"json"},
		"qterm":       {query},
		"rows":        {strconv.Itoa(limit)},
		"fl":          {"docdt,display_title,abstracts,url,url_friendly_title,pdfurl,lang"},
		"os":          {"0"},
		"count_exact": {"true"},
	})
	var payload struct {
		Documents orderedJSONObjects `json:"documents"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Documents.raw))
	for _, raw := range payload.Documents.raw {
		var doc struct {
			DisplayTitle string            `json:"display_title"`
			Abstracts    map[string]string `json:"abstracts"`
			DocDT        string            `json:"docdt"`
			URL          string            `json:"url"`
			FriendlyURL  string            `json:"url_friendly_title"`
			PDFURL       string            `json:"pdfurl"`
			Lang         string            `json:"lang"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		link := firstNonEmpty(strings.TrimSpace(doc.URL), strings.TrimSpace(doc.FriendlyURL), strings.TrimSpace(doc.PDFURL))
		if link == "" {
			continue
		}
		link = upgradeHTTPS(link)
		title := strings.Join(strings.Fields(doc.DisplayTitle), " ")
		if title == "" {
			title = link
		}
		snippet := ""
		for _, v := range doc.Abstracts {
			if strings.TrimSpace(v) != "" {
				snippet = v
				break
			}
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     title,
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Published: parseTimeAny(doc.DocDT),
			Lang:      normalizeLangField(doc.Lang),
		})
	}
	return results, nil
}

// yearTokenRE 匹配独立的四位年份；WDS 对它按 AND 处理，长查询里一个年份
// 就可能把命中数压到 0。
var yearTokenRE = regexp.MustCompile(`\b(19|20)\d{2}\b`)

// withoutYears 去掉查询里的年份词，用于空结果时的一次性放宽。
func withoutYears(q string) string {
	out := strings.Join(strings.Fields(yearTokenRE.ReplaceAllString(q, " ")), " ")
	return strings.TrimSpace(out)
}

// orderedJSONObjects 按 JSON 对象里的出现顺序保留值。
//
// 世界银行的 documents 是「id -> 文档」的对象，Go 的 map 会打乱顺序，而
// 这个顺序就是相关性排序；用 token 流逐项取出才能保住 Rank。
type orderedJSONObjects struct {
	raw []json.RawMessage
}

// UnmarshalJSON 实现有序对象解析。
func (o *orderedJSONObjects) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := keyTok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		// facets 与文档同级的元数据，不是检索结果。
		if key == "facets" {
			continue
		}
		o.raw = append(o.raw, raw)
	}
	_, err = dec.Token()
	return err
}

// upgradeHTTPS 把公开站点的 http 链接升到 https，避免 WebFetch 出站降级。
func upgradeHTTPS(link string) string {
	if strings.HasPrefix(link, "http://") {
		return "https://" + strings.TrimPrefix(link, "http://")
	}
	return link
}

// normalizeLangField 只接受 zh/en 两种值，其余留空交给上层判断。
func normalizeLangField(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "zh", "chi", "zho":
		return "zh"
	case "en", "eng":
		return "en"
	default:
		return ""
	}
}
