package providers

// weibo_test.go —— m.weibo.cn 匿名搜索源的卡片解析与字段映射测试。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/search"
)

// withWeiboEndpoint 把搜索端点指向 httptest, 测试结束后还原。
func withWeiboEndpoint(t *testing.T, raw string) {
	t.Helper()
	old := weiboSearchAPI
	weiboSearchAPI = raw
	t.Cleanup(func() { weiboSearchAPI = old })
}

// TestWeiboParsesCards 钉住 containerid 参数、cards/card_group 展开、
// 广告卡片丢弃 (无 bid)、HTML 剥离与字段映射。
func TestWeiboParsesCards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("containerid"); got != "100103type=1&q=三体" {
			t.Errorf("containerid = %q", got)
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "claude-go-search") {
			t.Errorf("UA = %q", r.Header.Get("User-Agent"))
		}
		fmt.Fprint(w, `{"ok":1,"data":{"cards":[
			{"card_type":9,"mblog":{
				"id":1001,"bid":"Pq1Ab2C3d",
				"text":"三体电视剧 <a href='/search?containerid=100103type%3D1%26q%3D三体'>#三体#</a> 太好看了<br/>第二集封神",
				"created_at":"Sat Nov 15 10:00:00 +0800 2026",
				"user":{"screen_name":"书虫小王"},
				"reposts_count":12,"comments_count":34,"attitudes_count":56}},
			{"card_type":11,"mblog":{"id":1002,"bid":"","text":"广告推广内容","user":{"screen_name":"ad"}}},
			{"card_group":[
				{"mblog":{"id":1003,"bid":"Ef4Gh5I6j","text":"刚刷完三体结局 意难平","created_at":"bad time","user":{"screen_name":"剧评人"}}},
				{"mblog":{"id":1001,"bid":"Pq1Ab2C3d","text":"dup","user":{"screen_name":"书虫小王"}}}
			]}
		]}}`)
	}))
	defer srv.Close()
	withWeiboEndpoint(t, srv.URL)

	p, err := newWeibo(search.DefaultConfig(), search.ProviderConfig{Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := p.Search(context.Background(), search.SearchRequest{Query: "三体", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// 广告 (无 bid) 被丢, 重复 ID 去重: 2 条唯一条目。
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	byURL := map[string]search.Result{}
	for _, r := range results {
		byURL[r.URL] = r
	}
	first := byURL["https://m.weibo.cn/status/Pq1Ab2C3d"]
	if first.URL == "" {
		t.Fatalf("status link missing: %+v", byURL)
	}
	if !strings.HasPrefix(first.Title, "@书虫小王: ") || !strings.Contains(first.Title, "第二集封神") {
		t.Errorf("title = %q", first.Title)
	}
	// HTML 标签 (话题链接/ <br>) 必须剥干净。
	if strings.ContainsAny(first.Title, "<>") || strings.Contains(first.Title, "#三体#") && strings.Contains(first.Title, "href") {
		t.Errorf("title has raw HTML: %q", first.Title)
	}
	if !strings.Contains(first.Snippet, "转发12") || !strings.Contains(first.Snippet, "评论34") || !strings.Contains(first.Snippet, "赞56") {
		t.Errorf("snippet = %q, want 互动数", first.Snippet)
	}
	if first.Published.Format("2006-01-02 15:04") != "2026-11-15 10:00" {
		t.Errorf("published = %v", first.Published)
	}
	if first.Lang != "zh" || first.Provider != "weibo" {
		t.Errorf("lang/provider = %q/%q", first.Lang, first.Provider)
	}
	second := byURL["https://m.weibo.cn/status/Ef4Gh5I6j"]
	if second.URL == "" {
		t.Fatal("card_group entry missing")
	}
	// created_at 解析失败时 Published 为零值但不丢结果。
	if !second.Published.IsZero() {
		t.Errorf("bad created_at should yield zero Published, got %v", second.Published)
	}
}

// TestWeiboEmptyCards 钉住无匹配时返回空结果而不是错误 (搜不到是正常的)。
func TestWeiboEmptyCards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":1,"data":{"cards":[]}}`)
	}))
	defer srv.Close()
	withWeiboEndpoint(t, srv.URL)

	p, err := newWeibo(search.DefaultConfig(), search.ProviderConfig{Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := p.Search(context.Background(), search.SearchRequest{Query: "nothing", Limit: 4})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %+v, want empty", results)
	}
}
