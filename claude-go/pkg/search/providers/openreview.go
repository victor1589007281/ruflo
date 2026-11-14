package providers

// openreview.go —— OpenReview 论文检索（免 key）。
//
// 覆盖 ICLR/NeurIPS/ICML 等会议的投稿、评审与正式版本，适合「某方向最近
// 有什么论文 / 哪个会议在讨论什么」类查询。OpenReview 的搜索索引还收录了
// DBLP 的整库记录（domain=DBLP.org），因此在 DBLP 自身被 Anubis 反爬挡住
// 时，它也是数据库/系统领域会议论文的可用入口。
//
// 只走公开检索 API，不登录、不读评审正文：
// https://api2.openreview.net/notes/search?term=...&source=forum&limit=...
//
// 结果里的 content 是 {"title":{"value":...}} 形式的嵌套对象（API v2），
// 解析时必须取 .value；漏掉这一层会得到一串空标题。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/anthropic/claude-go/pkg/search"
)

type openReviewProvider struct {
	*base
	endpoint string
}

// openReviewValue 是 API v2 的 {"value": ...} 包装。
type openReviewValue struct {
	Value string `json:"value"`
}

// openReviewContent 只声明检索结果里用得到的字段。
type openReviewContent struct {
	Title    openReviewValue   `json:"title"`
	Abstract openReviewValue   `json:"abstract"`
	Venue    openReviewValue   `json:"venue"`
	HTML     openReviewValue   `json:"html"`
	URL      openReviewValue   `json:"url"`
	PDF      openReviewValue   `json:"pdf"`
	Authors  openReviewAuthors `json:"authors"`
}

// openReviewAuthors 兼容 API 里出现的两种作者形状：
//   - 会议投稿: {"value":["Ada L","Bob"]}
//   - Public_Article: {"value":[{"fullname":"Mingyue Li","username":"..."}]}
//
// 只声明 []string 会在第二种记录上整页解码失败（一次 Public_Article 命中
// 就让整轮检索报 unavailable），所以这里必须自定义解码。
type openReviewAuthors struct {
	Value []string
}

func (a *openReviewAuthors) UnmarshalJSON(data []byte) error {
	var wrapped struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return err
	}
	if len(wrapped.Value) == 0 || string(wrapped.Value) == "null" {
		return nil
	}
	var names []string
	if err := json.Unmarshal(wrapped.Value, &names); err == nil {
		a.Value = names
		return nil
	}
	var objs []struct {
		Fullname string `json:"fullname"`
	}
	if err := json.Unmarshal(wrapped.Value, &objs); err == nil {
		for _, o := range objs {
			if n := strings.TrimSpace(o.Fullname); n != "" {
				a.Value = append(a.Value, n)
			}
		}
		return nil
	}
	// 兼容单个对象或裸字符串；都不像就交回原始错误。
	var obj struct {
		Fullname string `json:"fullname"`
	}
	if err := json.Unmarshal(wrapped.Value, &obj); err == nil && strings.TrimSpace(obj.Fullname) != "" {
		a.Value = []string{strings.TrimSpace(obj.Fullname)}
		return nil
	}
	var single string
	if err := json.Unmarshal(wrapped.Value, &single); err == nil && strings.TrimSpace(single) != "" {
		a.Value = []string{strings.TrimSpace(single)}
		return nil
	}
	return errors.New("openreview: 无法识别的 authors.value 形状")
}

// newOpenReview 构造 OpenReview 源（直连，L2）。
func newOpenReview(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://api2.openreview.net/notes/search"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("openreview", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	// 公共检索接口没有公布的配额；按 1s 保守节流，避免影响他人使用。
	b.setMinInterval(time.Second)
	return &openReviewProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用 /notes/search。
func (p *openReviewProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	target := withQuery(p.endpoint, url.Values{
		"term":   {query},
		"source": {"forum"},
		"limit":  {strconv.Itoa(limit)},
	})
	var payload struct {
		Notes []struct {
			ID         string            `json:"id"`
			Forum      string            `json:"forum"`
			Domain     string            `json:"domain"`
			PDate      int64             `json:"pdate"`
			CDate      int64             `json:"cdate"`
			Content    openReviewContent `json:"content"`
			Invitation []string          `json:"invitations"`
		} `json:"notes"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Notes))
	for _, it := range payload.Notes {
		title := strings.TrimSpace(it.Content.Title.Value)
		if title == "" {
			continue
		}
		link := openReviewLink(it.ID, it.Forum, it.Domain, it.Content)
		if link == "" {
			continue
		}
		venue := strings.TrimSpace(it.Content.Venue.Value)
		snippet := openReviewSnippet(it.Content)
		if snippet == "" {
			snippet = venue
		}
		published := time.UnixMilli(it.PDate)
		if it.PDate <= 0 && it.CDate > 0 {
			published = time.UnixMilli(it.CDate)
		}
		if published.Year() < 1900 {
			published = time.Time{}
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     stripHTML(title),
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Published: published,
			Lang:      openReviewLang(title),
		})
	}
	openReviewRerank(query, results)
	return finishResults(p.name, req, results, limit), nil
}

// openReviewRerank 按查询词覆盖率稳定重排。
//
// OpenReview 的检索是词袋匹配，实测 "large language model agents" 会把
// "Robust Language Identification for Romansh Varieties" 排第一——只因为
// 命中了 language。API 顺序不能直接当相关性用。这里只做确定性重排：
// 标题命中权重 2、摘要/会议命中权重 1，分高者在前，同分保持原顺序。
// 不加权删除零分结果（那会把只有会议名匹配的 DBLP 记录误删）。
func openReviewRerank(query string, results []search.Result) {
	tokens := openReviewTokens(query)
	if len(tokens) == 0 || len(results) < 2 {
		return
	}
	scores := make(map[int]int, len(results))
	for i, r := range results {
		title := strings.ToLower(r.Title)
		snippet := strings.ToLower(r.Snippet)
		for _, tok := range tokens {
			if strings.Contains(title, tok) {
				scores[i] += 2
			} else if strings.Contains(snippet, tok) {
				scores[i]++
			}
		}
	}
	idx := make([]int, len(results))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	out := make([]search.Result, len(results))
	for i, j := range idx {
		out[i] = results[j]
	}
	copy(results, out)
}

// openReviewTokens 把查询切成小写词元；中文整句会落成一个词元，
// 覆盖率重排对中文退化为原顺序，不会误伤。
func openReviewTokens(q string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, tok := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(tok)) < 2 || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

// openReviewLink 选一个可打开的落地页。
//
// OpenReview 自己的投稿优先给 forum 页（摘要、评审入口都在那里）；
// DBLP 记录没有 forum 语义，才退回出版商 html/url/pdf。
func openReviewLink(id, forum, domain string, c openReviewContent) string {
	id = strings.TrimSpace(id)
	forum = strings.TrimSpace(forum)
	if forum == "" {
		forum = id
	}
	isDBLP := strings.Contains(strings.ToLower(domain), "dblp")
	if isDBLP {
		for _, cand := range []string{c.HTML.Value, c.URL.Value, c.PDF.Value} {
			if v := strings.TrimSpace(cand); v != "" {
				return upgradeHTTPS(v)
			}
		}
	}
	if forum != "" {
		return "https://openreview.net/forum?id=" + url.QueryEscape(forum)
	}
	for _, cand := range []string{c.HTML.Value, c.URL.Value, c.PDF.Value} {
		if v := strings.TrimSpace(cand); v != "" {
			return upgradeHTTPS(v)
		}
	}
	return ""
}

// openReviewSnippet 组合作者、会议与摘要。
//
// DBLP 记录大多没有摘要，只靠会议名+作者也可用于核对；
// 不代表「有原文摘录」，reliability 仍由下游按来源判定。
func openReviewSnippet(c openReviewContent) string {
	parts := make([]string, 0, 3)
	if names := c.Authors.Value; len(names) > 0 {
		kept := make([]string, 0, len(names))
		for _, n := range names {
			if v := strings.TrimSpace(n); v != "" {
				kept = append(kept, v)
			}
		}
		if len(kept) > 0 {
			parts = append(parts, strings.Join(kept, ", "))
		}
	}
	if v := strings.TrimSpace(c.Venue.Value); v != "" {
		parts = append(parts, v)
	}
	if v := strings.TrimSpace(c.Abstract.Value); v != "" {
		parts = append(parts, v)
	}
	return strings.Join(parts, "：")
}

// openReviewLang 按标题字符集粗判语言；OpenReview 以英文论文为主。
func openReviewLang(title string) string {
	for _, r := range title {
		if r >= 0x4e00 && r <= 0x9fff {
			return "zh"
		}
	}
	return "en"
}
