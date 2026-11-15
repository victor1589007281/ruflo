package providers

// douban.go —— 豆瓣图书/电影条目检索 (subject_suggest 端点)。
//
// 豆瓣没有公开搜索 API; movie.douban.com 与 book.douban.com 的搜索框共用
// /j/subject_suggest 这个 JSON 端点, 匿名可用 (需浏览器 UA 与 Referer,
// 2026-11-15 本机实测)。它只返回最相关的少量条目 (标题/年份/条目页链接),
// 正好匹配"这本书/这部电影怎么样"类查询 —— 评分与评论在条目页里, 由 fetch
// 侧直接抓 (条目页匿名 200)。
//
// 反爬口径: 豆瓣对匿名 IP 有频率限制且无公开配额文档, 所以 weight 给低、
// 只在文化意图 (router 的 IntentCulture) 下排到最前; 连续失败由 base 熔断
// 兜底, 不会拖垮同一出口的其它源。

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/search"
)

// doubanBookSuggest / doubanMovieSuggest 是两个站点的搜索框端点。
// 包级变量 (非常量) 以便测试用 httptest 覆写。
var (
	doubanBookSuggest  = "https://book.douban.com/j/subject_suggest"
	doubanMovieSuggest = "https://movie.douban.com/j/subject_suggest"
)

type doubanProvider struct {
	*base
}

// doubanItem 兼容两个端点的字段差异: 电影端点给 img/sub_title,
// 图书端点给 pic/author_name。
type doubanItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	SubTitle string `json:"sub_title"`
	URL      string `json:"url"`
	Year     string `json:"year"`
	Type     string `json:"type"`
	Author   string `json:"author_name"`
}

// newDouban 构造豆瓣源 (直连, L2, 低权重 — 只在文化意图被调度)。
// UA 用 search.BrowserStyleUA (浏览器形态但保留 claude-go-search 身份):
// 豆瓣对该端点拒绝明显的爬虫 UA (返回 404/403), 固定单标识不轮换。
func newDouban(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	b := newBase("douban", cfg, pc, eg, search.ModeDirect, search.TierL2, 0.6)
	b.kinds = search.KindSet(search.KindSearch)
	b.ua = search.BrowserStyleUA
	return &doubanProvider{base: b}, nil
}

// Search 同时查图书与电影两个 suggest 端点并合并。
//
// 查询词通常无法预先分清要书还是影 (「三体」既是书也是电影), 单端点会
// 漏掉另一半; 两个端点是独立站点, 并发各一次, 合并后按站内排序截断。
func (p *doubanProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	type endpointResult struct {
		items []doubanItem
		err   error
	}
	resultsCh := make(chan endpointResult, 2)
	var wg sync.WaitGroup
	for _, ep := range []struct{ baseURL, referer string }{
		{doubanBookSuggest, "https://book.douban.com/"},
		{doubanMovieSuggest, "https://movie.douban.com/"},
	} {
		wg.Add(1)
		go func(ep string, referer string) {
			defer wg.Done()
			items, err := p.suggest(ctx, ep, referer, query)
			resultsCh <- endpointResult{items: items, err: err}
		}(ep.baseURL, ep.referer)
	}
	wg.Wait()
	close(resultsCh)

	var merged []doubanItem
	var firstErr error
	for r := range resultsCh {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		merged = append(merged, r.items...)
	}
	if len(merged) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, nil
	}

	// 两个端点会各自返回对方站点的条目 (电影端点搜书名也给图书条目, 但
	// 缺 author_name), 所以去重时保留 snippet 更全的那条; 存活条目按首次
	// 出现顺序输出, 与 goroutine 完成顺序无关。
	type candidate struct {
		link string
		res  search.Result
	}
	parsed := make([]candidate, 0, len(merged))
	for _, it := range merged {
		link := strings.TrimSpace(it.URL)
		if !isHTTPURL(link) {
			continue
		}
		// 电影端点的 URL 带 ?suggest= 追踪参数, 剥掉回条目页本身。
		if u, err := url.Parse(link); err == nil {
			u.RawQuery = ""
			link = u.String()
		}
		title := strings.TrimSpace(it.Title)
		if sub := strings.TrimSpace(it.SubTitle); sub != "" && sub != title {
			title = title + " " + sub
		}
		var meta []string
		switch strings.TrimSpace(it.Type) {
		case "b", "book":
			meta = append(meta, "图书")
		case "movie":
			meta = append(meta, "电影")
		case "tv":
			meta = append(meta, "剧集")
		}
		if y := strings.TrimSpace(it.Year); y != "" {
			meta = append(meta, y)
		}
		if a := strings.TrimSpace(it.Author); a != "" {
			meta = append(meta, a)
		}
		parsed = append(parsed, candidate{link: link, res: search.Result{
			URL:     link,
			Title:   title,
			Snippet: strings.Join(meta, " · "),
			Lang:    "zh",
		}})
	}
	best := map[string]search.Result{}
	order := make([]string, 0, len(parsed))
	for _, c := range parsed {
		if old, dup := best[c.link]; dup {
			if len(c.res.Snippet) > len(old.Snippet) {
				best[c.link] = c.res
			}
			continue
		}
		best[c.link] = c.res
		order = append(order, c.link)
	}
	results := make([]search.Result, 0, len(order))
	for _, link := range order {
		results = append(results, best[link])
	}
	return finishResults(p.name, req, results, limit), nil
}

// suggest 请求单个端点并解码条目数组。空结果与 404 都视为"该站点无匹配",
// 返回空列表而不是错误 —— 图书站查电影名为空是正常的。
func (p *doubanProvider) suggest(ctx context.Context, endpoint, referer, query string) ([]doubanItem, error) {
	target := withQuery(endpoint, url.Values{"q": {query}})
	var items []doubanItem
	_, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept":  {"application/json"},
		"Referer": {referer},
	}, nil, &items)
	if err != nil {
		if search.KindOf(err) == search.ErrBlocked || search.KindOf(err) == search.ErrRateLimited {
			return nil, err
		}
		// 站点级故障 (DNS/5xx) 降级为空结果: 另一站点仍可能命中。
		return nil, nil
	}
	return items, nil
}
