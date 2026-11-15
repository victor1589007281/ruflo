package search

// fetch_ua_test.go —— 抓取 UA 按域名选择的测试 (公众号文章需要浏览器形态 UA)。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchUserAgentByHost 钉住 UA 选择规则: browserUAHosts 命中的域名用
// 浏览器形态 UA (仍带 claude-go-search 身份), 其余保持配置里的诚实爬虫 UA。
func TestFetchUserAgentByHost(t *testing.T) {
	if got := fetchUserAgent("claude-go-bot/1.0", "mp.weixin.qq.com"); got != BrowserStyleUA {
		t.Errorf("weixin UA = %q, want BrowserStyleUA", got)
	}
	if got := fetchUserAgent("claude-go-bot/1.0", "example.com"); got != "claude-go-bot/1.0" {
		t.Errorf("other UA = %q, want cfg UA", got)
	}
}

// TestFetchUsesBrowserUAForListedHost 端到端钉住: 命中名单的 host 实际收到
// 浏览器形态 UA。把环回地址临时加进名单, 用 httptest 断言请求头。
func TestFetchUsesBrowserUAForListedHost(t *testing.T) {
	allowAnyFetchURL(t)
	browserUAHosts["127.0.0.1"] = true
	t.Cleanup(func() { delete(browserUAHosts, "127.0.0.1") })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != BrowserStyleUA {
			t.Errorf("UA = %q, want %q", ua, BrowserStyleUA)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body><article><p>公众号文章正文。</p></article></body></html>`)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.UserAgent = "claude-go-bot/1.0"
	m := NewManager(cfg, testEgress(t), nil)
	defer m.Close()

	resp, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/s/xxx"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(resp.Document.Markdown, "公众号文章正文") {
		t.Errorf("markdown = %q", resp.Document.Markdown)
	}
}
