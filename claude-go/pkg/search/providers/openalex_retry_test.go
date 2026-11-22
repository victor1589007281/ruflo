package providers

// openalex_retry_test.go —— 429/5xx 源内重试的回归测试。
//
// 事故背景 (2026-11-19): 早上连续 provider_error 打开熔断, 随后的 429 又触发
// manager 60s 配额冷却, 冷却结束探针再失败 → 再熔断, openalex 长时间不可用,
// 学术意图查询全部降级成 partial。重试必须把秒级毛刺吸收在源内部。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// withOpenAlexBackoffs 压缩退避序列, 测试不必真等 1s+2s。
func withOpenAlexBackoffs(t *testing.T, d time.Duration) {
	t.Helper()
	old := openalexRetryBackoffs
	openalexRetryBackoffs = []time.Duration{d, d}
	t.Cleanup(func() { openalexRetryBackoffs = old })
}

// TestOpenAlexRetriesRateLimitedThenSucceeds: 前两次 429, 第三次 200 ——
// Search 必须成功且恰好发了 3 次请求 (初始 1 + 重试 2)。
func TestOpenAlexRetriesRateLimitedThenSucceeds(t *testing.T) {
	withOpenAlexBackoffs(t, time.Millisecond)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"rate limit exceeded"}`)
			return
		}
		fmt.Fprint(w, `{"results":[{"id":"https://openalex.org/W1","doi":"10.1/abc","display_name":"Retry Works","publication_year":2026}]}`)
	}))
	defer srv.Close()

	p, err := newOpenAlex(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/works"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := p.Search(context.Background(), search.SearchRequest{Query: "retry", Limit: 5})
	if err != nil {
		t.Fatalf("Search 应在重试后成功: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 (初始+2 重试)", calls.Load())
	}
	if len(results) != 1 || results[0].Title != "Retry Works" {
		t.Fatalf("results = %+v", results)
	}
}

// TestOpenAlexRetriesServerError: 5xx 同样重试; 第一次 500 第二次 200。
func TestOpenAlexRetriesServerError(t *testing.T) {
	withOpenAlexBackoffs(t, time.Millisecond)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"results":[{"id":"https://openalex.org/W2","display_name":"Server Retry"}]}`)
	}))
	defer srv.Close()

	p, err := newOpenAlex(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Search(context.Background(), search.SearchRequest{Query: "x", Limit: 3}); err != nil {
		t.Fatalf("5xx 后重试应成功: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

// TestOpenAlexBlockedFailsFast: 403 (非限流信号) 必须立即失败不重试 ——
// 封锁类错误重试只会加剧封禁。
func TestOpenAlexBlockedFailsFast(t *testing.T) {
	withOpenAlexBackoffs(t, time.Millisecond)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"bot detected"}`)
	}))
	defer srv.Close()

	p, err := newOpenAlex(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Search(context.Background(), search.SearchRequest{Query: "x", Limit: 3})
	if err == nil {
		t.Fatal("403 应返回错误")
	}
	if search.KindOf(err) != search.ErrBlocked {
		t.Fatalf("kind = %v, want ErrBlocked", search.KindOf(err))
	}
	if calls.Load() != 1 {
		t.Fatalf("封锁错误不应重试: calls = %d", calls.Load())
	}
}

// TestOpenAlexGivesUpAfterRetries: 持续 429 重试耗尽后把错误交回 manager
// (由其走 60s 配额冷却), 且总请求数 = 1 + len(backoffs)。
func TestOpenAlexGivesUpAfterRetries(t *testing.T) {
	withOpenAlexBackoffs(t, time.Millisecond)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p, err := newOpenAlex(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Search(context.Background(), search.SearchRequest{Query: "x", Limit: 3})
	if err == nil {
		t.Fatal("持续 429 应返回错误")
	}
	if search.KindOf(err) != search.ErrRateLimited {
		t.Fatalf("kind = %v, want ErrRateLimited", search.KindOf(err))
	}
	if want := int32(1 + len(openalexRetryBackoffs)); calls.Load() != want {
		t.Fatalf("calls = %d, want %d", calls.Load(), want)
	}
}
