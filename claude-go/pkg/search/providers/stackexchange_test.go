package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/search"
)

// TestStackExchangeQueriesSpecializedSite 钉住多站检索:
// 技术问题默认查 SO + 一个按信号选出的专业站, 结果合并去重;
// 显式 allowed_domains 时只查命中的子站。
func TestStackExchangeQueriesSpecializedSite(t *testing.T) {
	var mu sync.Mutex
	var sites []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site := r.URL.Query().Get("site")
		mu.Lock()
		sites = append(sites, site)
		mu.Unlock()
		host := map[string]string{
			"stackoverflow":      "stackoverflow.com",
			"dba":                "dba.stackexchange.com",
			"unix":               "unix.stackexchange.com",
			"serverfault":        "serverfault.com",
			"networkengineering": "networkengineering.stackexchange.com",
		}[site]
		if host == "" {
			t.Errorf("未知 site: %q", site)
			host = "example.com"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[{"title":"%s question","link":"https://%s/q/1","body":"<p>answer</p>","score":3,"creation_date":1600000000,"tags":["sql"]}]}`, site, host)
	}))
	defer srv.Close()

	cfg := search.DefaultConfig()
	p, err := newStackExchange(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("newStackExchange: %v", err)
	}

	res, err := p.Search(context.Background(), search.SearchRequest{
		Query: "postgres index deadlock", Limit: 5,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("结果 %d 条, want 2 (SO + dba): %+v", len(res), res)
	}
	mu.Lock()
	got := append([]string(nil), sites...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "stackoverflow" || got[1] != "dba" {
		t.Fatalf("查询站 = %v, want [stackoverflow dba]", got)
	}

	// 显式域名约束只允许 dba: 不应再查 SO。
	mu.Lock()
	sites = nil
	mu.Unlock()
	if _, err := p.Search(context.Background(), search.SearchRequest{
		Query: "postgres index deadlock", Limit: 5,
		AllowedDomains: []string{"dba.stackexchange.com"},
	}); err != nil {
		t.Fatalf("Search with allowed domains: %v", err)
	}
	mu.Lock()
	got = append([]string(nil), sites...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "dba" {
		t.Fatalf("显式约束下查询站 = %v, want [dba]", got)
	}
}

// TestStackExchangeNoAllowedSite 钉住"调用方限定的域名不在 SE 里时不绕过约束"。
func TestStackExchangeNoAllowedSite(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer srv.Close()

	p, err := newStackExchange(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("newStackExchange: %v", err)
	}
	res, err := p.Search(context.Background(), search.SearchRequest{
		Query: "go", AllowedDomains: []string{"example.com"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("结果 = %v, want empty", res)
	}
	if calls != 0 {
		t.Errorf("不该请求 StackExchange, calls = %d", calls)
	}
}
