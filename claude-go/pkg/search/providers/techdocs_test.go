package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// TestTechDocsFiltersToCatalogDomains 钉住两件事:
//  1. query 里的产品名被转成 site: 提示;
//  2. 返回结果按目录域名硬过滤, 搜索引擎越界的结果不会流进融合层。
func TestTechDocsFiltersToCatalogDomains(t *testing.T) {
	m := &mcpTestServer{
		t:     t,
		mode:  "json",
		tools: []string{"web_search_exa"},
		text: `[
			{"title":"Routine Vacuuming","url":"https://www.postgresql.org/docs/current/routine-vacuuming.html"},
			{"title":"Vacuum wiki","url":"https://wiki.postgresql.org/wiki/Vacuum"},
			{"title":"off-domain noise","url":"https://blog.example.com/postgres-vacuum"}
		]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	defer srv.Close()

	p, err := newTechDocs(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("newTechDocs: %v", err)
	}
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "postgresql vacuum 原理", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("结果 %d 条, want 2: %+v", len(res), res)
	}
	for i, r := range res {
		if !strings.Contains(r.URL, "postgresql.org") {
			t.Errorf("第 %d 条越界: %s", i, r.URL)
		}
		if r.Provider != "techdocs" {
			t.Errorf("provider = %q, want techdocs", r.Provider)
		}
		if r.Rank != i+1 {
			t.Errorf("rank = %d, want %d", r.Rank, i+1)
		}
	}

	m.mu.Lock()
	args := append([]map[string]any(nil), m.callArgs...)
	m.mu.Unlock()
	if len(args) != 1 {
		t.Fatalf("tools/call 次数 = %d, want 1", len(args))
	}
	query, _ := args[0]["query"].(string)
	if !strings.Contains(query, "site:postgresql.org") {
		t.Errorf("query 缺 site: 提示: %q", query)
	}
}

// TestTechDocsSkipsNonTechnicalQuery 钉住"不命中产品名就不发请求"。
func TestTechDocsSkipsNonTechnicalQuery(t *testing.T) {
	m := &mcpTestServer{t: t, mode: "json", tools: []string{"search"}, text: `[]`}
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	defer srv.Close()

	p, err := newTechDocs(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("newTechDocs: %v", err)
	}
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "今天天气怎么样"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("结果 = %v, want empty", res)
	}
	m.mu.Lock()
	calls := len(m.callArgs)
	m.mu.Unlock()
	if calls != 0 {
		t.Errorf("非技术查询不该发 MCP 请求, calls = %d", calls)
	}
}

// TestTechDocsLive 是可选的真实端点冒烟测试 (TECHDOCS_LIVE=1)。
//
// 单元测试只能证明"解析与过滤"逻辑; 这个测试证明 Exa 匿名端点当前仍然
// 接受 site: 语法并返回站内结果。外部端点变化时能第一时间发现。
func TestTechDocsLive(t *testing.T) {
	if os.Getenv("TECHDOCS_LIVE") == "" {
		t.Skip("set TECHDOCS_LIVE=1 to hit the real Exa endpoint")
	}
	p, err := newTechDocs(search.DefaultConfig(), search.ProviderConfig{Enabled: true}, nil)
	if err != nil {
		t.Fatalf("newTechDocs: %v", err)
	}
	cases := []struct {
		query   string
		domains []string
	}{
		{"postgresql vacuum 原理", []string{"postgresql.org"}},
		{"kafka vs pulsar 延迟对比", []string{"kafka.apache.org", "pulsar.apache.org"}},
	}
	for _, tc := range cases {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(12*time.Second))
		res, err := p.Search(ctx, search.SearchRequest{Query: tc.query, Limit: 5})
		cancel()
		if err != nil {
			t.Errorf("%q: %v", tc.query, err)
			continue
		}
		if len(res) == 0 {
			t.Errorf("%q: 没有结果", tc.query)
			continue
		}
		for _, r := range res {
			if !urlInDomains(r.URL, tc.domains) {
				t.Errorf("%q: 越界结果 %s", tc.query, r.URL)
			}
		}
		t.Logf("%q -> %d 条, 首条 %s", tc.query, len(res), res[0].URL)
	}
}

// TestURLInDomains 覆盖域名后缀与路径前缀两种目录条目。
func TestURLInDomains(t *testing.T) {
	cases := []struct {
		raw     string
		domains []string
		want    bool
	}{
		{"https://postgresql.org/docs/", []string{"postgresql.org"}, true},
		{"https://www.postgresql.org/docs/", []string{"postgresql.org"}, true},
		{"https://evilpostgresql.org/x", []string{"postgresql.org"}, false},
		{"https://github.com/seaweedfs/seaweedfs/wiki", []string{"github.com/seaweedfs"}, true},
		{"https://github.com/other/repo", []string{"github.com/seaweedfs"}, false},
		{"https://aws.amazon.com/blogs/architecture/x", []string{"aws.amazon.com/blogs"}, true},
		{"https://aws.amazon.com/s3/", []string{"aws.amazon.com/blogs"}, false},
		{"http://localhost:8080/x", []string{"example.com"}, false},
	}
	for _, tc := range cases {
		if got := urlInDomains(tc.raw, tc.domains); got != tc.want {
			t.Errorf("urlInDomains(%q, %v) = %v, want %v", tc.raw, tc.domains, got, tc.want)
		}
	}
}
