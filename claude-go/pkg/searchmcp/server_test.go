package searchmcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// stubTool 是最小的 Tool 实现, 用于在不联网的前提下验证协议层。
type stubTool struct{}

func (stubTool) Name() string                 { return "echo" }
func (stubTool) Description() string          { return "echo input" }
func (stubTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (stubTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: "echo:" + string(input)}, nil
}
func (stubTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (stubTool) CheckPermissions(json.RawMessage, *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (stubTool) IsReadOnly(json.RawMessage) bool { return true }

func newTestServer() *Server {
	reg := tool.NewRegistry()
	reg.Register(stubTool{})
	return &Server{reg: reg, names: []string{"echo"}}
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, raw)
	}
	return out
}

func TestHandleRPCInitializeAndList(t *testing.T) {
	s := newTestServer()

	init := decode(t, s.HandleRPC(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)))
	result := init["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}

	list := decode(t, s.HandleRPC(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)))
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools/list = %v", tools)
	}
}

// TestHandleRPCDiscover 断言 2026-07-28 客户端的第一步 server/discover 有应答,
// 且支持的版本列表只包含本服务真正实现的版本。
func TestHandleRPCDiscover(t *testing.T) {
	s := newTestServer()
	got := decode(t, s.HandleRPC(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":7,"method":"server/discover"}`)))
	result := got["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Fatalf("discover protocolVersion = %v", result["protocolVersion"])
	}
	versions := result["supportedVersions"].([]any)
	if len(versions) != 1 || versions[0] != ProtocolVersion {
		t.Fatalf("supportedVersions = %v", versions)
	}
}

// TestMCPClientConnectsOverHTTP 用 claude-go 自己的 MCP 客户端连一遍。
//
// 这是对"能不能在能力中心里显示"最接近的离线判据: 客户端在 connectHTTP 里先发
// server/discover, 该方法缺失时连接直接失败、服务器不会出现在 ListServers 里。
func TestMCPClientConnectsOverHTTP(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()

	conn, err := mcp.NewClient().Connect(context.Background(), mcp.ServerConfig{
		Name: "claude-go-search", Transport: "http", URL: ts.URL,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	if conn.Status != "connected" {
		t.Fatalf("status = %q", conn.Status)
	}
	if len(conn.Tools) != 1 || conn.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", conn.Tools)
	}
}

func TestHandleRPCNotificationReturnsNil(t *testing.T) {
	s := newTestServer()
	if got := s.HandleRPC(context.Background(),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); got != nil {
		t.Fatalf("通知不应回包, got %s", got)
	}
}

func TestHandleRPCErrors(t *testing.T) {
	s := newTestServer()

	parse := decode(t, s.HandleRPC(context.Background(), []byte(`not json`)))
	if parse["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Fatalf("parse error code = %v", parse["error"])
	}

	unknown := decode(t, s.HandleRPC(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":3,"method":"nope"}`)))
	if unknown["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("method not found code = %v", unknown["error"])
	}

	badParams := decode(t, s.HandleRPC(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{}}`)))
	if badParams["error"].(map[string]any)["code"].(float64) != -32602 {
		t.Fatalf("invalid params code = %v", badParams["error"])
	}
}

func TestHandleRPCCallTool(t *testing.T) {
	s := newTestServer()

	ok := decode(t, s.HandleRPC(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{"q":"hi"}}}`)))
	result := ok["result"].(map[string]any)
	if result["isError"].(bool) {
		t.Fatalf("unexpected isError: %v", result)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"q":"hi"`) {
		t.Fatalf("content = %q", text)
	}

	missing := decode(t, s.HandleRPC(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope"}}`)))
	if !missing["result"].(map[string]any)["isError"].(bool) {
		t.Fatalf("unknown tool 应返回 isError: %v", missing)
	}
}

func TestServeStdio(t *testing.T) {
	s := newTestServer()
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	var out strings.Builder
	if err := s.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	if !strings.Contains(out.String(), `"result":{}`) {
		t.Fatalf("stdout = %q", out.String())
	}
}
