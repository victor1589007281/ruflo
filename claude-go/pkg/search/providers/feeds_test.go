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
		if q.Get("q") != "golang 检索" {
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
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "golang 检索", Limit: 3})
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
