package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

const testRSS = `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0"><channel>
<item><title>第一 条</title><link>https://news.example/1</link>
<description>&lt;b&gt;摘要&lt;/b&gt; 一</description><pubDate>Mon, 02 Jan 2006 15:04:05 GMT</pubDate></item>
<item><title>Second</title><link>https://news.example/2</link>
<description>summary two</description><pubDate>2024-05-06T07:08:09Z</pubDate></item>
</channel></rss>`

// TestBingRSSSearch 覆盖 Bing RSS 的 URL 构造与条目解析。
func TestBingRSSSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("format") != "rss" {
			t.Errorf("missing format=rss: %s", r.URL.RawQuery)
		}
		if q.Get("q") != "news" {
			t.Errorf("unexpected q: %q", q.Get("q"))
		}
		if q.Get("count") != "3" {
			t.Errorf("unexpected count: %q", q.Get("count"))
		}
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		fmt.Fprint(w, testRSS)
	}))
	defer srv.Close()

	p, err := newBingRSS(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/search"}, nil)
	if err != nil {
		t.Fatalf("newBingRSS: %v", err)
	}
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "news", Limit: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if res[0].Title != "第一 条" || res[0].Snippet != "摘要 一" {
		t.Errorf("unexpected result 0: %+v", res[0])
	}
	if !res[0].Published.Equal(time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)) {
		t.Errorf("unexpected published: %v", res[0].Published)
	}
	if res[1].Published.IsZero() {
		t.Errorf("RFC3339 pubDate not parsed: %+v", res[1])
	}
	if res[0].Provider != "bing_rss" || res[0].Rank != 1 {
		t.Errorf("provider/rank missing: %+v", res[0])
	}
}

func TestBingRSSSearchAddsDomainHints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Query().Get("q"), "news site:news.example -site:ads.example"; got != want {
			t.Errorf("q=%q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, testRSS)
	}))
	defer srv.Close()
	p, err := newBingRSS(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/search"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Search(context.Background(), search.SearchRequest{
		Query: "news", AllowedDomains: []string{"news.example"}, BlockedDomains: []string{"ads.example"},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestBingRSSFiltersOffTopic 钉住兜底源的词面过滤。
//
// Bing 在长查询上会回完全不同话题的页面; 过滤只应在 bing_rss 这类兜底源
// 生效, 且必须保留真正命中查询内容词的那条。
func TestBingRSSFiltersOffTopic(t *testing.T) {
	const rss = `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0"><channel>
<item><title>无关影视站</title><link>https://video.example/1</link><description>花都影视</description></item>
<item><title>Go 1.24 release notes</title><link>https://go.dev/doc/go1.24</link><description>golang release notes</description></item>
</channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		fmt.Fprint(w, rss)
	}))
	defer srv.Close()

	p, err := newBingRSS(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/search"}, nil)
	if err != nil {
		t.Fatalf("newBingRSS: %v", err)
	}
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "golang release notes"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].URL != "https://go.dev/doc/go1.24" {
		t.Fatalf("off-topic result not filtered: %+v", res)
	}
}

// TestBingRSSRateLimited 覆盖 429 分类。
func TestBingRSSRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p, _ := newBingRSS(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/search"}, nil)
	_, err := p.Search(context.Background(), search.SearchRequest{Query: "q"})
	if search.KindOf(err) != search.ErrRateLimited {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
}

// TestGNewsFollowsRedirectAndParses 覆盖 302 跟随与 Google News 参数。
func TestGNewsFollowsRedirectAndParses(t *testing.T) {
	var finalQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/rss/search", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final?"+r.URL.RawQuery, http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		finalQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, testRSS)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, err := newGNews(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/rss/search"}, nil)
	if err != nil {
		t.Fatalf("newGNews: %v", err)
	}
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "科技"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if finalQuery.Get("hl") != "zh-CN" || finalQuery.Get("gl") != "CN" || finalQuery.Get("ceid") != "CN:zh-Hans" {
		t.Errorf("google news params missing after redirect: %v", finalQuery)
	}
	if res[0].Lang != "zh" {
		t.Errorf("want lang zh, got %q", res[0].Lang)
	}
}

// TestGNewsDropsRedirectPages 钉住 Google News 站内跳转链的过滤。
//
// RSS 里的 /rss/articles/... 打开后只是 Google News 的空壳页, 不是可引用
// 来源; 继续传给模型会诱导它把跳转链写进证据表。
func TestGNewsDropsRedirectPages(t *testing.T) {
	const rss = `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0"><channel>
<item><title>跳转页</title><link>https://news.google.com/rss/articles/abc</link><description>壳页</description></item>
<item><title>直接来源</title><link>https://example.com/story</link><description>原文</description></item>
</channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, rss)
	}))
	defer srv.Close()

	p, err := newGNews(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/rss/search"}, nil)
	if err != nil {
		t.Fatalf("newGNews: %v", err)
	}
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "科技"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].URL != "https://example.com/story" {
		t.Fatalf("redirect page not filtered: %+v", res)
	}
}

// TestWikipediaSearch 覆盖 JSON 解析、HTML 清洗与词条 URL 拼接。
func TestWikipediaSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("action") != "query" || q.Get("list") != "search" || q.Get("format") != "json" {
			t.Errorf("unexpected query: %v", q)
		}
		if q.Get("srsearch") != "Go 语言" || q.Get("srlimit") != "2" {
			t.Errorf("unexpected search params: %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"query":{"search":[
			{"title":"Go语言","snippet":"<span class=\"searchmatch\">Go</span> 是 &quot;编译&quot; 语言","timestamp":"2024-01-02T03:04:05Z"},
			{"title":"Go 并发","snippet":"goroutine","timestamp":"2023-06-07T00:00:00Z"}
		]}}`)
	}))
	defer srv.Close()

	p, err := newWikipedia(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/w/api.php"}, nil)
	if err != nil {
		t.Fatalf("newWikipedia: %v", err)
	}
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "Go 语言", Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if res[0].Snippet != `Go 是 "编译" 语言` {
		t.Errorf("snippet not cleaned: %q", res[0].Snippet)
	}
	if res[0].URL != "https://zh.wikipedia.org/wiki/Go%E8%AF%AD%E8%A8%80" {
		t.Errorf("unexpected page url: %s", res[0].URL)
	}
	if res[1].URL != "https://zh.wikipedia.org/wiki/Go_%E5%B9%B6%E5%8F%91" {
		t.Errorf("space should become underscore: %s", res[1].URL)
	}
	if res[0].Lang != "zh" {
		t.Errorf("want lang zh, got %q", res[0].Lang)
	}
}

// TestWikipediaBlocked 覆盖 403 挑战/封锁分类。
func TestWikipediaBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html><body>captcha challenge</body></html>")
	}))
	defer srv.Close()

	p, _ := newWikipedia(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/w/api.php"}, nil)
	_, err := p.Search(context.Background(), search.SearchRequest{Query: "q"})
	if search.KindOf(err) != search.ErrBlocked {
		t.Fatalf("want ErrBlocked, got %v", err)
	}
}

// TestWikipediaFetchUnsupported 确认 Wikipedia 只声明 Search。
func TestWikipediaFetchUnsupported(t *testing.T) {
	p, err := newWikipedia(search.DefaultConfig(), search.ProviderConfig{}, nil)
	if err != nil {
		t.Fatalf("newWikipedia: %v", err)
	}
	if p.Kinds().Has(search.KindFetch) {
		t.Fatal("wikipedia must not declare Fetch")
	}
	if _, err := p.Fetch(context.Background(), search.FetchRequest{URL: "https://zh.wikipedia.org/wiki/X"}); search.KindOf(err) != search.ErrUnsupported {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}
