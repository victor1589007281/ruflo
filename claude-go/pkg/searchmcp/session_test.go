package searchmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// sessionStubTool 只观察协议层是否调用工具，不访问网络。calls 由 WebSearch
// 和 WebFetch 共享，用于证明 max_uses 是两者的合计而不是各自一份额度。
type sessionStubTool struct {
	name       string
	calls      *atomic.Int32
	sessionIDs chan<- string
}

func (t sessionStubTool) Name() string                         { return t.name }
func (sessionStubTool) Description() string                    { return "session budget test" }
func (sessionStubTool) InputSchema() json.RawMessage           { return json.RawMessage(`{"type":"object"}`) }
func (sessionStubTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (sessionStubTool) IsReadOnly(json.RawMessage) bool        { return true }
func (sessionStubTool) CheckPermissions(json.RawMessage, *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (t sessionStubTool) Call(_ context.Context, input json.RawMessage, tc *tool.ToolContext) (*tool.ToolResult, error) {
	t.calls.Add(1)
	if t.sessionIDs != nil {
		t.sessionIDs <- tc.SessionID
	}
	if string(input) == `{"fail":true}` {
		return &tool.ToolResult{Content: "tool failed", IsError: true}, nil
	}
	return &tool.ToolResult{Content: "tool ok"}, nil
}

func newBudgetTestServer(maxUses int) (*Server, *atomic.Int32, chan string) {
	reg := tool.NewRegistry()
	calls := &atomic.Int32{}
	sessionIDs := make(chan string, 64)
	names := []string{"WebSearch", "WebFetch"}
	for _, name := range names {
		reg.Register(sessionStubTool{name: name, calls: calls, sessionIDs: sessionIDs})
	}
	return &Server{reg: reg, names: names, sessions: newSessionBudget(maxUses)}, calls, sessionIDs
}

type testHTTPResponse struct {
	status int
	header http.Header
	body   []byte
}

func postRPC(rawURL, sessionID string, body []byte) (testHTTPResponse, error) {
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return testHTTPResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set(SessionHeader, sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return testHTTPResponse{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return testHTTPResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: data}, err
}

func mustPostRPC(t *testing.T, rawURL, sessionID, body string) testHTTPResponse {
	t.Helper()
	resp, err := postRPC(rawURL, sessionID, []byte(body))
	if err != nil {
		t.Fatalf("POST %s: %v", body, err)
	}
	return resp
}

func rpcResultIsError(t *testing.T, body []byte) (bool, string) {
	t.Helper()
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("invalid RPC response: %v: %s", err, body)
	}
	text := ""
	if len(envelope.Result.Content) > 0 {
		text = envelope.Result.Content[0].Text
	}
	return envelope.Result.IsError, text
}

func initializeHTTP(t *testing.T, rawURL string) string {
	t.Helper()
	resp := mustPostRPC(t, rawURL, "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if resp.status != http.StatusOK || len(resp.header.Get(SessionHeader)) != 64 {
		t.Fatalf("initialize status=%d session=%q body=%s", resp.status, resp.header.Get(SessionHeader), resp.body)
	}
	return resp.header.Get(SessionHeader)
}

func TestHTTPMaxUsesRequiresSessionAndCountsToolErrors(t *testing.T) {
	s, calls, sessionIDs := newBudgetTestServer(2)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if resp := mustPostRPC(t, ts.URL, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); resp.status != http.StatusBadRequest {
		t.Fatalf("missing session status = %d, body=%s", resp.status, resp.body)
	}
	if resp := mustPostRPC(t, ts.URL, "forged", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); resp.status != http.StatusNotFound {
		t.Fatalf("forged session status = %d, body=%s", resp.status, resp.body)
	}
	firstSession := initializeHTTP(t, ts.URL)

	// 工具已经执行并返回错误，也必须消耗一次；否则模型可用无效请求绕过预算。
	resp := mustPostRPC(t, ts.URL, firstSession, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"WebSearch","arguments":{"fail":true}}}`)
	if isError, _ := rpcResultIsError(t, resp.body); !isError || calls.Load() != 1 {
		t.Fatalf("failed tool call was not counted: isError=%v calls=%d", isError, calls.Load())
	}
	if got := <-sessionIDs; got != firstSession {
		t.Fatalf("ToolContext session = %q, want issued session", got)
	}
	resp = mustPostRPC(t, ts.URL, firstSession, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"WebFetch","arguments":{}}}`)
	if isError, _ := rpcResultIsError(t, resp.body); isError || calls.Load() != 2 {
		t.Fatalf("second combined call failed: isError=%v calls=%d body=%s", isError, calls.Load(), resp.body)
	}
	resp = mustPostRPC(t, ts.URL, firstSession, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"WebSearch","arguments":{}}}`)
	if isError, text := rpcResultIsError(t, resp.body); !isError || !strings.Contains(text, "max_uses exhausted") || calls.Load() != 2 {
		t.Fatalf("over-limit call reached tool: isError=%v text=%q calls=%d", isError, text, calls.Load())
	}

	secondSession := initializeHTTP(t, ts.URL)
	if secondSession == firstSession {
		t.Fatal("initialize reused another client's session")
	}
	resp = mustPostRPC(t, ts.URL, secondSession, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"WebSearch","arguments":{}}}`)
	if isError, _ := rpcResultIsError(t, resp.body); isError || calls.Load() != 3 {
		t.Fatalf("independent session did not receive its own budget: %s", resp.body)
	}
}

func TestHTTPMaxUsesConcurrentCallsNeverOverspend(t *testing.T) {
	const limit, workers = 8, 32
	s, calls, _ := newBudgetTestServer(limit)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	sessionID := initializeHTTP(t, ts.URL)

	var wg sync.WaitGroup
	results := make(chan string, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"WebSearch","arguments":{}}}`, i+10)
			resp, err := postRPC(ts.URL, sessionID, []byte(body))
			if err != nil {
				errs <- err
				return
			}
			if resp.status != http.StatusOK {
				errs <- fmt.Errorf("status %d: %s", resp.status, resp.body)
				return
			}
			results <- string(resp.body)
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	succeeded, rejected := 0, 0
	for body := range results {
		var envelope struct {
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("invalid concurrent response: %v", err)
		}
		if envelope.Result.IsError {
			rejected++
		} else {
			succeeded++
		}
	}
	if calls.Load() != limit || succeeded != limit || rejected != workers-limit {
		t.Fatalf("calls=%d succeeded=%d rejected=%d, want %d/%d/%d", calls.Load(), succeeded, rejected, limit, limit, workers-limit)
	}
}

func TestStdioMaxUsesUsesOneProcessSession(t *testing.T) {
	s, calls, sessionIDs := newBudgetTestServer(1)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"WebSearch","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"WebFetch","arguments":{}}}`,
	}, "\n") + "\n"
	var out strings.Builder
	if err := s.ServeStdio(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || strings.Count(out.String(), "max_uses exhausted") != 1 {
		t.Fatalf("stdio budget calls=%d output=%s", calls.Load(), out.String())
	}
	if got := <-sessionIDs; len(got) != 64 {
		t.Fatalf("stdio ToolContext session = %q", got)
	}
}

func TestInternalMCPClientReturnsSessionHeader(t *testing.T) {
	s, calls, _ := newBudgetTestServer(1)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	conn, err := mcp.NewClient().Connect(context.Background(), mcp.ServerConfig{
		Name: "budget-test", Transport: "http", URL: ts.URL,
	})
	if err != nil {
		t.Fatalf("Connect must return session on discover and tools/list: %v", err)
	}
	defer conn.Close()
	if len(conn.Tools) != 2 {
		t.Fatalf("tools/list did not survive session enforcement: %+v", conn.Tools)
	}
	if _, err := conn.CallTool(context.Background(), "WebSearch", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
	text, err := conn.CallTool(context.Background(), "WebFetch", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(text, "max_uses exhausted") || calls.Load() != 1 {
		t.Fatalf("second CallTool text=%q err=%v calls=%d", text, err, calls.Load())
	}
	wrapped := mcp.NewMCPTool(conn, mcp.ToolInfo{Name: "WebFetch", Description: "budget test"})
	result, err := wrapped.Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Content, "max_uses exhausted") {
		t.Fatalf("MCPTool dropped remote isError: result=%+v err=%v", result, err)
	}
}

func TestMaxUsesFromEnvRejectsInvalidConfiguration(t *testing.T) {
	for _, tt := range []struct {
		value   string
		want    int
		wantErr bool
	}{{"", 0, false}, {"0", 0, false}, {"7", 7, false}, {"-1", 0, true}, {"many", 0, true}} {
		t.Run(fmt.Sprintf("value_%q", tt.value), func(t *testing.T) {
			t.Setenv(MaxUsesEnv, tt.value)
			got, err := MaxUsesFromEnv()
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("MaxUsesFromEnv() = %d, %v", got, err)
			}
		})
	}
}
