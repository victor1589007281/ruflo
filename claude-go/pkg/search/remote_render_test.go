package search

// remote_render_test.go —— Firecrawl 远程渲染兜底的协议解析与 renderJS 链路测试。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeMCPBody 构造 Firecrawl MCP 的两层 JSON SSE 帧: 外层 result.content[0].text
// 里再包一个 scrape 结果 JSON。
func fakeMCPBody(markdown, title, pageURL, published string) string {
	inner, _ := json.Marshal(map[string]any{
		"markdown": markdown,
		"metadata": map[string]any{
			"title":     title,
			"url":       pageURL,
			"published": published,
		},
	})
	outer, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(inner)}},
		},
	})
	return "event: message\ndata: " + string(outer) + "\n\n"
}

// TestFirecrawlRendererRender 钉住对 fake MCP 端点的完整请求/响应往返:
// tools/call 形状、UA/Accept 头、keyless 不带 Authorization、SSE 帧解析、
// 两层 JSON 解码与 Document 字段映射。
func TestFirecrawlRendererRender(t *testing.T) {
	allowAnyFetchURL(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Errorf("accept = %q", r.Header.Get("Accept"))
		}
		if ua := r.Header.Get("User-Agent"); ua != "claude-go-search/1.0" {
			t.Errorf("ua = %q", ua)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("keyless 档不应带 Authorization")
		}
		var body struct {
			Method string `json:"method"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					URL     string   `json:"url"`
					Formats []string `json:"formats"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if body.Method != "tools/call" || body.Params.Name != "firecrawl_scrape" {
			t.Errorf("method/name = %q/%q", body.Method, body.Params.Name)
		}
		if body.Params.Arguments.URL != "http://example.com/page" {
			t.Errorf("arguments.url = %q", body.Params.Arguments.URL)
		}
		if len(body.Params.Arguments.Formats) != 1 || body.Params.Arguments.Formats[0] != "markdown" {
			t.Errorf("formats = %v", body.Params.Arguments.Formats)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fakeMCPBody("# Hi\n\nhello world", "Hi", "http://example.com/page", "2026-11-10"))
	}))
	defer srv.Close()

	r := NewFirecrawlRenderer(RemoteRenderConfig{Enabled: true, Endpoint: srv.URL}, srv.Client())
	doc, err := r.Render(context.Background(), "http://example.com/page")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if doc.Via != remoteRenderVia {
		t.Errorf("via = %q, want %q", doc.Via, remoteRenderVia)
	}
	if doc.Markdown != "# Hi\n\nhello world" {
		t.Errorf("markdown = %q", doc.Markdown)
	}
	if doc.Title != "Hi" || doc.URL != "http://example.com/page" {
		t.Errorf("title/url = %q/%q", doc.Title, doc.URL)
	}
	if doc.Published.Format("2006-01-02") != "2026-11-10" {
		t.Errorf("published = %v", doc.Published)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d", hits.Load())
	}
}

// TestFirecrawlRendererSendsAPIKey 钉住配了 FIRECRAWL_API_KEY 时升级为
// Bearer 认证档。
func TestFirecrawlRendererSendsAPIKey(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("authorization = %q", auth)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fakeMCPBody("body text", "", "", ""))
	}))
	defer srv.Close()

	r := NewFirecrawlRenderer(RemoteRenderConfig{Enabled: true, Endpoint: srv.URL, APIKey: "test-key"}, srv.Client())
	doc, err := r.Render(context.Background(), "http://example.com/page")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// metadata.url 为空时回退到请求 URL。
	if doc.URL != "http://example.com/page" {
		t.Errorf("url fallback = %q", doc.URL)
	}
}

// TestFirecrawlRendererHTTPError 钉住非 2xx 按 statusKind 归类 (429 → 限流)。
func TestFirecrawlRendererHTTPError(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "over quota", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	r := NewFirecrawlRenderer(RemoteRenderConfig{Enabled: true, Endpoint: srv.URL}, srv.Client())
	_, err := r.Render(context.Background(), "http://example.com/page")
	if KindOf(err) != ErrRateLimited {
		t.Fatalf("kind = %s, want rate_limited (%v)", KindOf(err), err)
	}
}

// TestReadStreamableMessageJoinsMultilineData 钉住 SSE 单行 JSON 超长拆多行
// data: 时按行拼接 (段间换行是合法 JSON 空白), 而不是按行独立解码。
func TestReadStreamableMessageJoinsMultilineData(t *testing.T) {
	full := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`
	splitAt := strings.Index(full, `"result":`)
	body := "data: " + full[:splitAt] + "\ndata: " + full[splitAt:] + "\n\n"
	msg, err := readStreamableMessage(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readStreamableMessage: %v", err)
	}
	var decoded struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(msg, &decoded); err != nil {
		t.Fatalf("joined payload not valid JSON: %v", err)
	}
	if decoded.Result.Content[0].Text != "{}" {
		t.Errorf("text = %q", decoded.Result.Content[0].Text)
	}
}

// TestReadStreamableMessageNoData 钉住响应里没有 SSE data 帧时报错。
func TestReadStreamableMessageNoData(t *testing.T) {
	if _, err := readStreamableMessage(strings.NewReader("event: ping\n\n")); err == nil {
		t.Fatal("no data frame must error")
	}
}

// TestFirecrawlDocumentLayers 钉住两层 JSON 解码、空 markdown 拒绝与
// MCP error 字段透传 (都是 ErrUnavailable, 与本地渲染"无正文"同口径)。
func TestFirecrawlDocumentLayers(t *testing.T) {
	inner, _ := json.Marshal(map[string]any{
		"markdown": "real body",
		"metadata": map[string]any{"title": "T", "url": "http://x/1"},
	})
	outer, _ := json.Marshal(map[string]any{
		"result": map[string]any{"content": []map[string]any{{"text": string(inner)}}},
	})
	doc, err := firecrawlDocument(outer, "http://x/1")
	if err != nil {
		t.Fatalf("firecrawlDocument: %v", err)
	}
	if doc.Markdown != "real body" || doc.Via != remoteRenderVia {
		t.Errorf("doc = %+v", doc)
	}

	emptyInner, _ := json.Marshal(map[string]any{"markdown": "   "})
	emptyOuter, _ := json.Marshal(map[string]any{
		"result": map[string]any{"content": []map[string]any{{"text": string(emptyInner)}}},
	})
	if _, err := firecrawlDocument(emptyOuter, "http://x/1"); KindOf(err) != ErrUnavailable {
		t.Errorf("empty markdown kind = %s", KindOf(err))
	}

	errOuter, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": "url blocked"},
	})
	if _, err := firecrawlDocument(errOuter, "http://x/1"); err == nil {
		t.Error("MCP error field must surface")
	}
}

// TestRemoteRenderEligible 钉住兜底准入: ErrInvalid (URL 被 urlGuard 拒)
// 不兜底, 其余本地失败 (浏览器缺失/超时/挑战/代理形态) 都值得远程一试。
func TestRemoteRenderEligible(t *testing.T) {
	if remoteRenderEligible(nil) {
		t.Error("nil error must not be eligible")
	}
	if remoteRenderEligible(Wrap("fetch", ErrInvalid, errRemoteRenderNoMarkdown)) {
		t.Error("ErrInvalid must not be eligible")
	}
	if !remoteRenderEligible(Wrap("chromedp", ErrUnsupported, errRemoteRenderNoMarkdown)) {
		t.Error("ErrUnsupported (authed proxy) must be eligible")
	}
	if !remoteRenderEligible(Wrap("chromedp", ErrTimeout, errRemoteRenderNoMarkdown)) {
		t.Error("ErrTimeout must be eligible")
	}
}

// fakeJSRenderer 是注入 Manager.renderer 的桩渲染器。
type fakeJSRenderer struct {
	doc *Document
	err error
}

func (f fakeJSRenderer) Render(context.Context, string) (*Document, error) {
	return f.doc, f.err
}

// authedProxyEgress 构造一个带凭据代理的 Egress: browserOptions 对该形态
// 确定性返回 ErrUnsupported, 与测试机是否装了 Chrome 无关。sink 是一个真
// 正向代理 (透传 http 绝对 URI 请求) —— egress 对代理初始乐观健康, 远程
// 渲染走 ModeAuto 时会真的经过它, 必须能透传到 httptest 目标。
func authedProxyEgress(t *testing.T) *Egress {
	t.Helper()
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RequestURI = "" // RoundTrip 要求置空; 代理形态下 r.URL 是绝对地址
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(sink.Close)
	eg, err := NewEgress(EgressConfig{
		Mode:     ModeProxy,
		ProxyURL: "http://user:pass@" + strings.TrimPrefix(sink.URL, "http://"),
	})
	if err != nil {
		t.Fatalf("NewEgress: %v", err)
	}
	t.Cleanup(func() { _ = eg.Close() })
	return eg
}

// TestRenderJSInjectedRendererWins 钉住注入渲染器的优先级: 存在即全权接管,
// 不触发本地 Chromium 也不触发远程兜底。
func TestRenderJSInjectedRendererWins(t *testing.T) {
	allowAnyFetchURL(t)
	var remoteHits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteHits.Add(1)
	}))
	defer remote.Close()

	cfg := DefaultConfig()
	cfg.RemoteRender = RemoteRenderConfig{Enabled: true, Endpoint: remote.URL}
	m := NewManager(cfg, authedProxyEgress(t), nil)
	defer m.Close()
	m.renderer = fakeJSRenderer{doc: &Document{URL: "http://x/", Via: "fake"}}

	doc, err := m.renderJS(context.Background(), nil, "http://example.com/page")
	if err != nil {
		t.Fatalf("renderJS: %v", err)
	}
	if doc.Via != "fake" {
		t.Errorf("via = %q", doc.Via)
	}
	if remoteHits.Load() != 0 {
		t.Errorf("remote must not be called when renderer injected, hits = %d", remoteHits.Load())
	}
}

// TestRenderJSRemoteFallback 钉住兜底链: 本地渲染确定性失败 (带凭据代理
// 形态不支持) → 远程渲染接管并产出 firecrawl 标记的文档。
func TestRenderJSRemoteFallback(t *testing.T) {
	allowAnyFetchURL(t)
	var remoteHits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fakeMCPBody("rendered body", "RT", "http://example.com/page", ""))
	}))
	defer remote.Close()

	cfg := DefaultConfig()
	cfg.RemoteRender = RemoteRenderConfig{Enabled: true, Endpoint: remote.URL}
	eg := authedProxyEgress(t)
	m := NewManager(cfg, eg, nil)
	defer m.Close()

	doc, err := m.renderJS(context.Background(), eg.proxy, "http://example.com/page")
	if err != nil {
		t.Fatalf("renderJS: %v", err)
	}
	if doc.Via != remoteRenderVia {
		t.Errorf("via = %q, want %q", doc.Via, remoteRenderVia)
	}
	if remoteHits.Load() != 1 {
		t.Errorf("remote hits = %d, want 1", remoteHits.Load())
	}
}

// TestRenderJSNoFallbackOnInvalidURL 钉住 urlGuard 拒绝的 URL 不触发远程
// 兜底 (远程侧同样会拒, 白打一趟), 也钉住远程渲染关闭时本地失败直接返回。
func TestRenderJSNoFallbackOnInvalidURL(t *testing.T) {
	old := urlGuard
	urlGuard = func(raw string) (*url.URL, error) {
		return nil, Wrap("fetch", ErrInvalid, fmt.Errorf("blocked %s", raw))
	}
	t.Cleanup(func() { urlGuard = old })

	var remoteHits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteHits.Add(1)
	}))
	defer remote.Close()

	cfg := DefaultConfig()
	cfg.RemoteRender = RemoteRenderConfig{Enabled: true, Endpoint: remote.URL}
	eg := authedProxyEgress(t)
	m := NewManager(cfg, eg, nil)
	defer m.Close()

	_, err := m.renderJS(context.Background(), eg.proxy, "http://example.com/page")
	if KindOf(err) != ErrInvalid {
		t.Fatalf("kind = %s, want invalid (%v)", KindOf(err), err)
	}
	if remoteHits.Load() != 0 {
		t.Errorf("remote must not be called for guard-rejected URL, hits = %d", remoteHits.Load())
	}

	// 远程渲染关闭: 本地失败原样返回, 不偷偷连远程。
	cfg2 := DefaultConfig()
	m2 := NewManager(cfg2, authedProxyEgress(t), nil)
	defer m2.Close()
	if _, err := m2.renderJS(context.Background(), m2.egress.proxy, "http://example.com/page"); KindOf(err) != ErrUnsupported {
		t.Fatalf("disabled fallback kind = %s", KindOf(err))
	}
	if remoteHits.Load() != 0 {
		t.Errorf("remote must not be called when disabled, hits = %d", remoteHits.Load())
	}
}
