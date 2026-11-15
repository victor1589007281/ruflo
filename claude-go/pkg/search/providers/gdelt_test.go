package providers

// gdelt_test.go —— GDELT DOC 2.0 源的解析与参数测试。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// TestGDELTParsesArticles 钉住 artlist JSON → Result 的字段映射。
func TestGDELTParsesArticles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("query") != "earthquake" {
			t.Errorf("query param = %q", q.Get("query"))
		}
		if q.Get("mode") != "artlist" || q.Get("format") != "json" {
			t.Errorf("mode/format params: %v", q)
		}
		if q.Get("maxrecords") != "2" {
			t.Errorf("maxrecords = %q", q.Get("maxrecords"))
		}
		fmt.Fprint(w, `{"articles":[
			{"url":"https://example.org/quake-1","title":"Quake hits","seendate":"20261114093000","domain":"example.org","language":"English","sourcecountry":"Japan"},
			{"url":"https://example.cn/地震","title":"地震快讯","seendate":"20261114084500","domain":"example.cn","language":"Chinese","sourcecountry":"China"},
			{"url":"","title":"no-url dropped","seendate":"","domain":"","language":"","sourcecountry":""}
		]}`)
	}))
	defer srv.Close()

	p, err := newGDELT(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := p.Search(context.Background(), search.SearchRequest{Query: "earthquake", Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (no-url entry dropped)", len(results))
	}
	if results[0].URL != "https://example.org/quake-1" || results[0].Title != "Quake hits" {
		t.Errorf("first result = %+v", results[0])
	}
	if results[0].Published.Format("2006-01-02 15:04") != "2026-11-14 09:30" {
		t.Errorf("seendate parse = %v", results[0].Published)
	}
	if results[0].Snippet == "" {
		t.Error("snippet (domain·country·time) must not be empty")
	}
	if results[1].Lang != "zh" {
		t.Errorf("Chinese language should normalize to zh, got %q", results[1].Lang)
	}
	if results[1].Provider != "gdelt" || results[1].Rank != 2 {
		t.Errorf("provider/rank = %q/%d", results[1].Provider, results[1].Rank)
	}
}

// TestGDELTRateLimitOpensCircuit 钉住 429 被归为 ErrRateLimited 并计入熔断。
func TestGDELTRateLimitOpensCircuit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Please limit requests to one every 5 seconds", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p, err := newGDELT(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.(*gdeltProvider).setMinInterval(0) // 关掉 5s 节流, 不然三个请求要 10 秒
	for i := 0; i < 3; i++ {
		if _, err := p.Search(context.Background(), search.SearchRequest{Query: "x", Limit: 1}); search.KindOf(err) != search.ErrRateLimited {
			t.Fatalf("attempt %d: kind = %v, want ErrRateLimited", i, search.KindOf(err))
		}
	}
	if p.Healthy() {
		t.Error("circuit should be open after 3 consecutive 429s")
	}
}

// TestGDELTPacesRequests 钉住 5 秒节流的占位语义: 两个连续请求的第二
// 个至少等待一个间隔 (用可注入的短间隔等价验证, 真实 5s 常量不参与测试)。
func TestGDELTPacesRequests(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"articles":[]}`)
	}))
	defer srv.Close()

	p, err := newGDELT(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gp := p.(*gdeltProvider)
	gp.setMinInterval(50 * time.Millisecond)

	start := time.Now()
	if _, err := p.Search(context.Background(), search.SearchRequest{Query: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Search(context.Background(), search.SearchRequest{Query: "b"}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("second request not paced: %v", elapsed)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}
