package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/search"
)

// mcpTestServer 是用于测试的极简 MCP Streamable HTTP 服务端。
type mcpTestServer struct {
	t                *testing.T
	mode             string // json | sse
	tools            []string
	text             string
	rejectNumResults bool
	initStatus       int

	mu          sync.Mutex
	initCalls   int
	callArgs    []map[string]any
	badSession  bool
	lastTool    string
	toolQueries int
}

// handler 按 JSON-RPC method 分发测试响应。
func (m *mcpTestServer) handler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		m.t.Errorf("decode request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	switch req.Method {
	case "initialize":
		m.mu.Lock()
		m.initCalls++
		m.mu.Unlock()
		if m.initStatus != 0 {
			http.Error(w, "rate limited: too many requests", m.initStatus)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-123")
		m.respond(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "test", "version": "1"},
		})
	case "tools/list":
		m.checkSession(w, r)
		tools := make([]map[string]any, 0, len(m.tools))
		for _, name := range m.tools {
			tools = append(tools, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}})
		}
		m.respond(w, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		m.checkSession(w, r)
		m.mu.Lock()
		m.callArgs = append(m.callArgs, req.Params.Arguments)
		m.lastTool = req.Params.Name
		m.mu.Unlock()
		if m.rejectNumResults {
			if _, ok := req.Params.Arguments["numResults"]; ok {
				m.respondError(w, req.ID, -32602, "invalid params: unknown argument numResults")
				return
			}
		}
		m.respond(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": m.text}},
		})
	default:
		m.respondError(w, req.ID, -32601, "method not found")
	}
}

// checkSession 断言后续请求透传了 Mcp-Session-Id。
func (m *mcpTestServer) checkSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Mcp-Session-Id") != "sess-123" {
		m.mu.Lock()
		m.badSession = true
		m.mu.Unlock()
		m.t.Errorf("missing Mcp-Session-Id on %s", r.URL.Path)
	}
}

// respond 按 mode 返回 JSON 或 SSE 响应。
func (m *mcpTestServer) respond(w http.ResponseWriter, id json.RawMessage, result any) {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if m.mode == "sse" {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}

// respondError 返回 JSON-RPC 错误。
func (m *mcpTestServer) respondError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
	if m.mode == "sse" {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}

// newTestMCP 构造指向测试服务端的 Parallel provider。
func newTestMCP(t *testing.T, srv *httptest.Server) search.Provider {
	t.Helper()
	cfg := search.DefaultConfig()
	p, err := newParallel(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("newParallel: %v", err)
	}
	return p
}

// TestMCPJSONSearch 覆盖 JSON 响应、工具发现与结构化结果解析。
func TestMCPJSONSearch(t *testing.T) {
	m := &mcpTestServer{
		t:     t,
		mode:  "json",
		tools: []string{"fetch_url", "web_search"},
		text:  `[{"title":"Alpha","url":"https://alpha.example/a","snippet":"first"},{"title":"Beta","link":"https://beta.example/b","description":"second"}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	defer srv.Close()

	p := newTestMCP(t, srv)
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "golang", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if res[0].URL != "https://alpha.example/a" || res[0].Title != "Alpha" || res[0].Snippet != "first" {
		t.Errorf("unexpected first result: %+v", res[0])
	}
	if res[0].Provider != "parallel" || res[0].Rank != 1 || res[1].Rank != 2 {
		t.Errorf("provider/rank not filled: %+v", res)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastTool != "web_search" {
		t.Errorf("should pick tool containing search, got %q", m.lastTool)
	}
	if m.initCalls != 1 {
		t.Errorf("initialize should run once, got %d", m.initCalls)
	}
	if m.badSession {
		t.Error("session id was not propagated")
	}
}

// TestMCPSSEAndArgumentFallback 覆盖 SSE 响应与 numResults 被拒后的参数回退。
func TestMCPSSEAndArgumentFallback(t *testing.T) {
	m := &mcpTestServer{
		t:                t,
		mode:             "sse",
		tools:            []string{"web_search"},
		text:             `{"web":{"results":[{"title":"W","url":"https://w.example","text":"wt"}]}}`,
		rejectNumResults: true,
	}
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	defer srv.Close()

	p := newTestMCP(t, srv)
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "q", Limit: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].URL != "https://w.example" {
		t.Fatalf("unexpected results: %+v", res)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.callArgs) != 2 {
		t.Fatalf("want 2 tool calls (fallback), got %d", len(m.callArgs))
	}
	if _, ok := m.callArgs[1]["numResults"]; ok {
		t.Errorf("fallback call should not carry numResults: %+v", m.callArgs[1])
	}
	if m.callArgs[1]["query"] != "q" {
		t.Errorf("fallback call lost query: %+v", m.callArgs[1])
	}
}

// TestMCPLineFallback 覆盖非 JSON 输出的逐行标题/URL 解析。
func TestMCPLineFallback(t *testing.T) {
	m := &mcpTestServer{
		t:     t,
		mode:  "json",
		tools: []string{"search"},
		text:  "1. Alpha - https://alpha.example/x\n2. Beta https://beta.example/y\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	defer srv.Close()

	res, err := newTestMCP(t, srv).Search(context.Background(), search.SearchRequest{Query: "q"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if res[0].Title != "Alpha" || res[0].URL != "https://alpha.example/x" {
		t.Errorf("unexpected result 0: %+v", res[0])
	}
	if res[1].Title != "Beta" || res[1].URL != "https://beta.example/y" {
		t.Errorf("unexpected result 1: %+v", res[1])
	}
}

// TestMCPMissingSearchTool 覆盖工具清单里没有 search 工具时的 ErrUnsupported。
func TestMCPMissingSearchTool(t *testing.T) {
	m := &mcpTestServer{t: t, mode: "json", tools: []string{"fetch_url"}, text: "[]"}
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	defer srv.Close()

	_, err := newTestMCP(t, srv).Search(context.Background(), search.SearchRequest{Query: "q"})
	if err == nil {
		t.Fatal("expected error")
	}
	if search.KindOf(err) != search.ErrUnsupported {
		t.Fatalf("want ErrUnsupported, got %v (%v)", search.KindOf(err), err)
	}
}

// TestMCPUnparsableOutput 覆盖工具输出无法解析时不编造结果。
func TestMCPUnparsableOutput(t *testing.T) {
	m := &mcpTestServer{t: t, mode: "json", tools: []string{"search"}, text: "no links here at all"}
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	defer srv.Close()

	_, err := newTestMCP(t, srv).Search(context.Background(), search.SearchRequest{Query: "q"})
	if err == nil || search.KindOf(err) != search.ErrUnsupported {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

// TestMCPRateLimited 覆盖 429 分类与本地退避。
func TestMCPRateLimited(t *testing.T) {
	m := &mcpTestServer{t: t, mode: "json", tools: []string{"search"}, initStatus: http.StatusTooManyRequests}
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	defer srv.Close()

	p := newTestMCP(t, srv)
	_, err := p.Search(context.Background(), search.SearchRequest{Query: "q"})
	if err == nil || search.KindOf(err) != search.ErrRateLimited {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if p.Healthy() {
		t.Error("provider should be unhealthy after rate-limited transport failure")
	}
}

// TestMCPSessionAndProtocolHeaders 覆盖会话头透传之外的协议版本头。
func TestMCPSessionAndProtocolHeaders(t *testing.T) {
	var sawProtocol string
	var mu sync.Mutex
	inner := &mcpTestServer{t: t, mode: "json", tools: []string{"search"}, text: `[]`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Session-Id") != "" {
			mu.Lock()
			sawProtocol = r.Header.Get("MCP-Protocol-Version")
			mu.Unlock()
		}
		inner.handler(w, r)
	}))
	defer srv.Close()

	if _, err := newTestMCP(t, srv).Search(context.Background(), search.SearchRequest{Query: "q"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sawProtocol != "2025-03-26" {
		t.Errorf("want MCP-Protocol-Version 2025-03-26, got %q", sawProtocol)
	}
}

// TestParseMCPResultShapes 覆盖三种结构化形状与零结果。
func TestParseMCPResultShapes(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"array", `[{"title":"a","url":"https://a"}]`, 1},
		{"results", `{"results":[{"title":"b","url":"https://b"},{"title":"c","url":"https://c"}]}`, 2},
		{"web", `{"web":{"results":[{"title":"d","url":"https://d"}]}}`, 1},
		{"empty array", `[]`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMCPResults("p", tc.text)
			if err != nil {
				t.Fatalf("parseMCPResults: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("want %d, got %d", tc.want, len(got))
			}
		})
	}
}

// TestProviderDeclaresOnlySearch 覆盖能力声明与 Fetch 拒绝。
func TestProviderDeclaresOnlySearch(t *testing.T) {
	m := &mcpTestServer{t: t, mode: "json", tools: []string{"search"}, text: `[]`}
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	defer srv.Close()

	p := newTestMCP(t, srv)
	if got := p.Kinds(); got != search.KindSet(search.KindSearch) {
		t.Fatalf("want only KindSearch, got %v", got)
	}
	_, ferr := p.Fetch(context.Background(), search.FetchRequest{URL: "https://x"})
	var se *search.Error
	if !errors.As(ferr, &se) || se.Kind != search.ErrUnsupported {
		t.Fatalf("Fetch must return ErrUnsupported, got %v", ferr)
	}
}

func TestParseMCPBlocksExtractsTitleURLAndSnippet(t *testing.T) {
	text := "Title: Tutorial | Tokio\nURL: https://tokio.rs/tokio/tutorial\nPublished: 2026-04-03T00:00:00.000Z\nAuthor: N/A\nHighlights:\nTokio is an asynchronous runtime.\nIt provides building blocks.\n\nTitle: Hello Tokio\nURL: https://tokio.rs/tokio/tutorial/hello-tokio\nHighlights:\nWe will get started."
	got, err := parseMCPResults("exa_anon", text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(got), got)
	}
	if got[0].Title != "Tutorial | Tokio" || got[0].URL != "https://tokio.rs/tokio/tutorial" {
		t.Fatalf("unexpected first result: %+v", got[0])
	}
	if got[0].Snippet == "" || got[1].Snippet == "" {
		t.Fatalf("highlights should become snippets: %+v", got)
	}
}

func TestArgsForToolSchemaUsesDeclaredProperties(t *testing.T) {
	parallel := json.RawMessage(`{"type":"object","required":["objective","search_queries"],"properties":{"objective":{"type":"string"},"search_queries":{"type":"array","items":{"type":"string"}},"session_id":{"type":"string"}}}`)
	args := argsForToolSchema(parallel, "rust tokio", 5)
	if args["objective"] != "rust tokio" {
		t.Fatalf("objective missing: %+v", args)
	}
	if q, ok := args["search_queries"].([]string); !ok || len(q) != 1 || q[0] != "rust tokio" {
		t.Fatalf("search_queries wrong: %+v", args)
	}
	if _, ok := args["numResults"]; ok {
		t.Fatalf("must not send undeclared numResults: %+v", args)
	}

	exa := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"numResults":{"type":"number"}},"required":["query"]}`)
	args = argsForToolSchema(exa, "q", 3)
	if args["query"] != "q" || args["numResults"] != 3 {
		t.Fatalf("exa args wrong: %+v", args)
	}
}

// TestParseMCPResultsKeepsPublishDate 验证 Parallel 的 publish_date 不被丢弃。
//
// 丢掉时间会让新闻类结果在质量门里全部算"不新鲜", gate 恒为 fresh 失败。
func TestParseMCPResultsKeepsPublishDate(t *testing.T) {
	text := `{"search_id":"x","results":[{"url":"https://a.com/1","title":"t","publish_date":"2026-10-23T10:00:00Z","excerpts":["e"]}]}`
	got, err := parseMCPResults("parallel", text)
	if err != nil {
		t.Fatalf("parseMCPResults: %v", err)
	}
	if len(got) != 1 || got[0].Published.IsZero() {
		t.Fatalf("Published 丢失: %+v", got)
	}
}
