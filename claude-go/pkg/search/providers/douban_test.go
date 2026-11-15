package providers

// douban_test.go —— 豆瓣 subject_suggest 双端点合并与字段映射测试。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/search"
)

// withDoubanEndpoints 把两个 suggest 端点指向 httptest, 测试结束后还原。
func withDoubanEndpoints(t *testing.T, book, movie string) {
	t.Helper()
	origB, origM := doubanBookSuggest, doubanMovieSuggest
	doubanBookSuggest, doubanMovieSuggest = book, movie
	t.Cleanup(func() { doubanBookSuggest, doubanMovieSuggest = origB, origM })
}

// TestDoubanMergesBookAndMovie 钉住双端点并发查询、字段归并与去重。
func TestDoubanMergesBookAndMovie(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/book", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "三体" {
			t.Errorf("book q = %q", r.URL.Query().Get("q"))
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "claude-go-search") {
			t.Errorf("UA should carry claude-go-search identity, got %q", r.Header.Get("User-Agent"))
		}
		fmt.Fprint(w, `[{"id":"2567698","title":"三体","url":"https://book.douban.com/subject/2567698/","year":"2008","type":"b","author_name":"刘慈欣"}]`)
	})
	mux.HandleFunc("/movie", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":"26266893","title":"流浪地球","sub_title":"The Wandering Earth","url":"https://movie.douban.com/subject/26266893/?suggest=三体","year":"2019","type":"movie"},{"id":"2567698","title":"三体 (书影重复)","url":"https://book.douban.com/subject/2567698/","year":"2008","type":"b"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withDoubanEndpoints(t, srv.URL+"/book", srv.URL+"/movie")

	p, err := newDouban(search.DefaultConfig(), search.ProviderConfig{Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := p.Search(context.Background(), search.SearchRequest{Query: "三体", Limit: 8})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// 电影端点返回的图书条目与图书端点同 URL, 必须被去重: 2 个唯一条目。
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (dup book entry merged by URL)", len(results))
	}
	byURL := map[string]search.Result{}
	for _, r := range results {
		byURL[r.URL] = r
	}
	// 电影端点的 ?suggest= 追踪参数必须剥掉。
	movie := byURL["https://movie.douban.com/subject/26266893/"]
	if movie.URL == "" {
		t.Fatalf("movie entry missing or tracking param not stripped: %+v", byURL)
	}
	if !strings.Contains(movie.Snippet, "电影") || !strings.Contains(movie.Snippet, "2019") {
		t.Errorf("movie snippet = %q, want type+year", movie.Snippet)
	}
	book := byURL["https://book.douban.com/subject/2567698/"]
	if book.URL == "" {
		t.Fatal("book entry missing")
	}
	if !strings.Contains(book.Snippet, "图书") || !strings.Contains(book.Snippet, "刘慈欣") {
		t.Errorf("book snippet = %q, want type+author", book.Snippet)
	}
	if book.Lang != "zh" || book.Provider != "douban" {
		t.Errorf("lang/provider = %q/%q", book.Lang, book.Provider)
	}
}

// TestDoubanOneSiteDownStillMerges 钉住单站点故障降级为空结果而不是整次失败。
func TestDoubanOneSiteDownStillMerges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "book") {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `[{"id":"1","title":"活着","url":"https://movie.douban.com/subject/1/","year":"1994","type":"movie"}]`)
	}))
	defer srv.Close()
	withDoubanEndpoints(t, srv.URL+"/book", srv.URL+"/movie")

	p, err := newDouban(search.DefaultConfig(), search.ProviderConfig{Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := p.Search(context.Background(), search.SearchRequest{Query: "活着", Limit: 4})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Title != "活着" {
		t.Fatalf("results = %+v, want the movie entry only", results)
	}
}

// TestDoubanBothSitesFail 钉住双端点全挂时返回错误 (由 base 熔断计数)。
func TestDoubanBothSitesFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	withDoubanEndpoints(t, srv.URL+"/book", srv.URL+"/movie")

	p, err := newDouban(search.DefaultConfig(), search.ProviderConfig{Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Search(context.Background(), search.SearchRequest{Query: "x", Limit: 1}); err == nil {
		t.Fatal("both endpoints failing must surface an error")
	}
}
