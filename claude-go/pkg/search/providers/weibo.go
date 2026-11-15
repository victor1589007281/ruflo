package providers

// weibo.go —— 微博热搜/舆论检索 (m.weibo.cn 匿名搜索 API)。
//
// 微博没有开放搜索 API, 官方移动端 m.weibo.cn 的搜索框走
// /api/container/getIndex?containerid=100103type=1&q=... 这个 JSON 端点,
// 匿名可用 (需 Referer 与浏览器形态 UA, 2026-11-15 经 mcp-server-weibo
// 匿名实测同形态请求返回真实结果)。它给的是博文卡片 (正文/作者/互动数),
// 正好补"中文实时舆论/热搜"这个缺口 —— 知乎/贴吧匿名已死, 微博是中文社媒
// 生态里唯一还开着的匿名口 (见调研结论)。
//
// 反爬口径: 匿名搜索有频率限制且无公开配额文档 (连续请求会触发 418),
// 所以 weight 给低、只在新闻意图的候选中段被调度; 连续失败由 base 熔断
// 兜底。正文里的话题/ @链接是站内 HTML 标签, 剥掉后取纯文本。

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/search"
)

// weiboSearchAPI 是 m.weibo.cn 移动端搜索框端点。包级变量 (非常量) 以便
// 测试用 httptest 覆写。
var weiboSearchAPI = "https://m.weibo.cn/api/container/getIndex"

// weiboCreatedAtLayout 是微博的时间格式 "Sat Nov 15 12:00:00 +0800 2026"。
const weiboCreatedAtLayout = "Mon Jan 2 15:04:05 -0700 2006"

// weiboStatusBase 是博文详情页前缀; 移动端页匿名可读, 由 fetch 侧直抓。
const weiboStatusBase = "https://m.weibo.cn/status/"

type weiboProvider struct {
	*base
}

// weiboPost 是 cards[].mblog 的字段子集。Text 是带站内标签的 HTML。
type weiboPost struct {
	ID      int64  `json:"id"`
	Bid     string `json:"bid"`
	Text    string `json:"text"`
	Source  string `json:"source"`
	Geo     string `json:"geo"`
	Created string `json:"created_at"`
	User    struct {
		ScreenName string `json:"screen_name"`
	} `json:"user"`
	Reposts   int `json:"reposts_count"`
	Comments  int `json:"comments_count"`
	Attitudes int `json:"attitudes_count"`
}

// newWeibo 构造微博源 (直连, L2, 低权重)。UA 用 search.BrowserStyleUA。
func newWeibo(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	b := newBase("weibo", cfg, pc, eg, search.ModeDirect, search.TierL2, 0.6)
	b.kinds = search.KindSet(search.KindSearch)
	b.ua = search.BrowserStyleUA
	return &weiboProvider{base: b}, nil
}

// Search 调 m.weibo.cn 搜索并展开博文卡片。
//
// 返回结构是 cards 数组: 有的卡片自带 mblog (card_type=9), 有的把多条
// 博文藏在 card_group 里。广告/推荐卡片没有 bid, 直接丢弃 —— 没有 bid
// 拼不出详情页链接, 留着也是死结果。
func (p *weiboProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	containerid := "100103type=1&q=" + query
	target := withQuery(weiboSearchAPI, url.Values{"containerid": {containerid}})
	var payload struct {
		OK   int `json:"ok"`
		Data struct {
			Cards []struct {
				Mblog     *weiboPost `json:"mblog"`
				CardGroup []struct {
					Mblog *weiboPost `json:"mblog"`
				} `json:"card_group"`
			} `json:"cards"`
		} `json:"data"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept":  {"application/json"},
		"Referer": {"https://m.weibo.cn/"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	posts := make([]*weiboPost, 0, limit)
	seen := map[int64]struct{}{}
	collect := func(m *weiboPost) {
		if m == nil || m.Bid == "" {
			return
		}
		if _, dup := seen[m.ID]; dup {
			return
		}
		seen[m.ID] = struct{}{}
		posts = append(posts, m)
	}
	for _, card := range payload.Data.Cards {
		collect(card.Mblog)
		for _, g := range card.CardGroup {
			collect(g.Mblog)
		}
		if len(posts) >= limit {
			break
		}
	}

	results := make([]search.Result, 0, len(posts))
	for _, m := range posts {
		text := weiboPlainText(m.Text)
		if text == "" {
			continue
		}
		link := weiboStatusBase + m.Bid
		if !isHTTPURL(link) {
			continue
		}
		// Title 给"@作者: 内容开头", Snippet 给互动数据 —— 舆论场景里
		// "谁在说什么"比"什么时候发的"更需要第一眼看到。
		title := "@" + m.User.ScreenName + ": " + weiboTruncate(text, 48)
		var meta []string
		if t := weiboTime(m.Created); !t.IsZero() {
			meta = append(meta, t.Format("2006-01-02 15:04"))
		}
		if m.Reposts > 0 {
			meta = append(meta, "转发"+strconv.Itoa(m.Reposts))
		}
		if m.Comments > 0 {
			meta = append(meta, "评论"+strconv.Itoa(m.Comments))
		}
		if m.Attitudes > 0 {
			meta = append(meta, "赞"+strconv.Itoa(m.Attitudes))
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     title,
			Snippet:   strings.Join(meta, " · "),
			Published: weiboTime(m.Created),
			Lang:      "zh",
		})
	}
	return finishResults(p.name, req, results, limit), nil
}

// weiboPlainText 剥掉正文里的站内 HTML (话题链接/ @链接/ <br>), 保留纯文本。
// stripHTML 把标签换成空格, 中文场景下会引入多余空白, 这里再收敛一次。
func weiboPlainText(s string) string {
	return strings.Join(strings.Fields(stripHTML(s)), " ")
}

// weiboTruncate 按 rune 截断标题, 超长补省略号。
func weiboTruncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "…"
}

// weiboTime 解析微博 created_at; 失败返回零值。
func weiboTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(weiboCreatedAtLayout, s); err == nil {
		return t
	}
	return time.Time{}
}
