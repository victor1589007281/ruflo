// Package mcp 实现 MCP (Model Context Protocol) 客户端。
// 对应 TS 源码: review/claude/src/services/mcp/client.ts
//
// Claude Code 作为 MCP 客户端连接到外部 MCP 服务器:
//   - stdio 传输: 启动子进程，通过 stdin/stdout 通信
//   - http 传输: 通过 HTTP/SSE 连接远程服务器
//
// MCP 协议基于 JSON-RPC 2.0:
//   - initialize: 握手 + 能力协商
//   - tools/list: 发现服务器提供的工具
//   - tools/call: 调用远程工具
//   - resources/list: 列出可用资源
//   - resources/read: 读取资源
//
// 发现的 MCP 工具会被包装成 MCPTool 注册到工具池中，
// 与内置工具一起参与 queryLoop 的工具编排。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// ServerConfig MCP 服务器配置
// 对应 TS: services/mcp/types.ts 中的 McpStdioServerConfig 等
type ServerConfig struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"` // stdio, http
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// Connection 与单个 MCP 服务器的连接
type Connection struct {
	Config     ServerConfig
	Status     string // connected, pending, error, disconnected
	Tools      []ToolInfo
	Process    *exec.Cmd
	stdin      io.WriteCloser
	stdout     *bufio.Scanner
	httpClient *http.Client
	mu         sync.Mutex
	nextID     atomic.Int64
}

// ToolInfo MCP 服务器声明的工具信息
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// jsonrpcRequest JSON-RPC 2.0 请求
type jsonrpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// jsonrpcResponse JSON-RPC 2.0 响应
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Client MCP 客户端管理器
// 管理多个 MCP 服务器连接
type Client struct {
	connections []*Connection
	mu          sync.Mutex
}

// NewClient 创建 MCP 客户端
func NewClient() *Client {
	return &Client{}
}

// LoadServerConfigsFromFile reads a Claude-style MCP JSON config (top-level "mcpServers" map)
// and returns ServerConfig entries with Name set from each map key.
func LoadServerConfigsFromFile(path string) ([]ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse mcp config: %w", err)
	}
	if len(root.MCPServers) == 0 {
		return nil, nil
	}
	out := make([]ServerConfig, 0, len(root.MCPServers))
	for name, raw := range root.MCPServers {
		var sc ServerConfig
		if err := json.Unmarshal(raw, &sc); err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", name, err)
		}
		sc.Name = name
		if sc.Transport == "" {
			sc.Transport = "stdio"
		}
		out = append(out, sc)
	}
	return out, nil
}

// Connect 连接到 MCP 服务器。
// 对应 TS: services/mcp/client.ts 中的 connectToServer()
//
// stdio 传输流程:
//  1. spawn 子进程 (command + args)
//  2. 发送 initialize 请求
//  3. 收到 initialize 响应 (获取 server capabilities)
//  4. 发送 initialized 通知
//  5. 发送 tools/list 获取工具列表
func (c *Client) Connect(ctx context.Context, config ServerConfig) (*Connection, error) {
	conn := &Connection{
		Config: config,
		Status: "pending",
	}

	switch config.Transport {
	case "stdio", "":
		if err := c.connectStdio(ctx, conn); err != nil {
			conn.Status = "error"
			return conn, fmt.Errorf("stdio 连接失败: %w", err)
		}
	case "http":
		if err := c.connectHTTP(ctx, conn); err != nil {
			conn.Status = "error"
			return conn, fmt.Errorf("http 连接失败: %w", err)
		}
	default:
		return nil, fmt.Errorf("不支持的传输方式: %s", config.Transport)
	}

	c.mu.Lock()
	c.connections = append(c.connections, conn)
	c.mu.Unlock()

	return conn, nil
}

// connectStdio 通过 stdio 传输连接 MCP 服务器
func (c *Client) connectStdio(ctx context.Context, conn *Connection) error {
	args := conn.Config.Args
	cmd := exec.CommandContext(ctx, conn.Config.Command, args...)

	cmd.Env = os.Environ()
	if len(conn.Config.Env) > 0 {
		for k, v := range conn.Config.Env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("获取 stdin 管道失败: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("获取 stdout 管道失败: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动进程失败: %w", err)
	}

	conn.Process = cmd
	conn.stdin = stdin
	conn.stdout = bufio.NewScanner(stdout)
	conn.stdout.Buffer(make([]byte, 1024*1024), 1024*1024)

	// 发送 initialize。启动阶段必须继承调用方 ctx, 避免异常 MCP 进程卡死 Bot 初始化。
	initResult, err := conn.sendRequestCtx(ctx, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]string{
			"name":    "claude-go",
			"version": "1.0.0",
		},
	})
	if err != nil {
		return fmt.Errorf("initialize 失败: %w", err)
	}
	_ = initResult

	// 发送 initialized 通知
	_ = conn.sendNotification("notifications/initialized", nil)

	// 获取工具列表
	toolsResult, err := conn.sendRequestCtx(ctx, "tools/list", nil)
	if err != nil {
		conn.Status = "connected"
		return nil // 工具列表失败不影响连接
	}

	var toolsList struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(toolsResult, &toolsList); err == nil {
		conn.Tools = toolsList.Tools
	}

	conn.Status = "connected"
	return nil
}

// connectHTTP 通过 HTTP 传输连接 MCP 服务器
func (c *Client) connectHTTP(ctx context.Context, conn *Connection) error {
	if conn.Config.URL == "" {
		return fmt.Errorf("HTTP 传输需要配置 url")
	}
	conn.httpClient = &http.Client{Timeout: 30 * time.Second}

	// 发送 initialize
	initBody, _ := json.Marshal(map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]string{
			"name":    "claude-go",
			"version": "1.0.0",
		},
	})
	resp, err := conn.httpClient.Post(conn.Config.URL+"/mcp/v1/initialize", "application/json", bytes.NewReader(initBody))
	if err != nil {
		return fmt.Errorf("initialize 请求失败: %w", err)
	}
	_ = resp.Body.Close()

	// 获取工具列表
	toolsResp, err := conn.httpClient.Post(conn.Config.URL+"/mcp/v1/tools/list", "application/json", nil)
	if err != nil {
		conn.Status = "connected"
		return nil
	}
	var toolsList struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.NewDecoder(toolsResp.Body).Decode(&toolsList); err == nil {
		conn.Tools = toolsList.Tools
	}
	_ = toolsResp.Body.Close()

	conn.Status = "connected"
	return nil
}

// sendRequest 发送 JSON-RPC 请求并等待响应
func (conn *Connection) sendRequest(method string, params interface{}) (json.RawMessage, error) {
	return conn.sendRequestCtx(context.Background(), method, params)
}

// sendRequestCtx 发送 JSON-RPC 请求 (支持 context 取消/超时)。
// 内部通过 goroutine 包装阻塞 I/O，使 ctx 能够中断挂起的读写。
func (conn *Connection) sendRequestCtx(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	// HTTP 传输: 直接发送 RESTful POST
	if conn.Config.Transport == "http" {
		var body []byte
		if params != nil {
			var err error
			body, err = json.Marshal(params)
			if err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, conn.Config.URL+"/mcp/v1/"+method, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := conn.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		result, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(result))
		}
		return result, nil
	}

	id := conn.nextID.Add(1)
	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if _, err := conn.stdin.Write(append(data, '\n')); err != nil {
		return nil, err
	}

	type scanResult struct {
		data []byte
		ok   bool
	}
	ch := make(chan scanResult, 1)
	go func() {
		ok := conn.stdout.Scan()
		if ok {
			raw := conn.stdout.Bytes()
			cp := make([]byte, len(raw))
			copy(cp, raw)
			ch <- scanResult{data: cp, ok: true}
		} else {
			ch <- scanResult{ok: false}
		}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("MCP 请求超时或被取消: %w", ctx.Err())
	case sr := <-ch:
		if !sr.ok {
			return nil, fmt.Errorf("读取响应失败")
		}
		var resp jsonrpcResponse
		if err := json.Unmarshal(sr.data, &resp); err != nil {
			return nil, err
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("RPC 错误 %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// sendNotification 发送 JSON-RPC 通知 (无 ID, 不期望响应)
func (conn *Connection) sendNotification(method string, params interface{}) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}

	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = conn.stdin.Write(append(data, '\n'))
	return err
}

// CallTool 调用 MCP 工具。
// 对应 TS: services/mcp/client.ts 中的 callMCPToolWithUrlElicitationRetry()
func (conn *Connection) CallTool(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	result, err := conn.sendRequestCtx(ctx, "tools/call", map[string]interface{}{
		"name":      name,
		"arguments": json.RawMessage(arguments),
	})
	if err != nil {
		return "", err
	}

	var callResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &callResult); err != nil {
		return string(result), nil
	}

	var texts []string
	for _, c := range callResult.Content {
		if c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// Close 关闭连接
func (conn *Connection) Close() error {
	if conn.Config.Transport == "http" {
		conn.httpClient = nil
		return nil
	}
	if conn.stdin != nil {
		conn.stdin.Close()
	}
	if conn.Process != nil && conn.Process.Process != nil {
		return conn.Process.Process.Kill()
	}
	return nil
}

// GetAllConnections 获取所有连接
func (c *Client) GetAllConnections() []*Connection {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]*Connection, len(c.connections))
	copy(result, c.connections)
	return result
}

// MCPTool 将 MCP 远程工具包装为本地 Tool 接口。
// 对应 TS: tools/MCPTool/MCPTool.ts
type MCPTool struct {
	conn       *Connection
	info       ToolInfo
	serverName string
}

// NewMCPTool 创建 MCP 工具包装
func NewMCPTool(conn *Connection, info ToolInfo) *MCPTool {
	return &MCPTool{
		conn:       conn,
		info:       info,
		serverName: conn.Config.Name,
	}
}

func (t *MCPTool) Name() string {
	return fmt.Sprintf("mcp_%s_%s", t.serverName, t.info.Name)
}

func (t *MCPTool) Description() string          { return t.info.Description }
func (t *MCPTool) InputSchema() json.RawMessage { return t.info.InputSchema }
func (t *MCPTool) IsReadOnly(_ json.RawMessage) bool {
	lower := strings.ToLower(t.info.Name)
	for _, prefix := range []string{"read", "get", "list", "search", "query", "fetch", "describe", "show", "status", "info", "check", "view"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	for _, suffix := range []string{"_list", "_get", "_read", "_search", "_status", "_info"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}
func (t *MCPTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *MCPTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *MCPTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	result, err := t.conn.CallTool(ctx, t.info.Name, input)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("MCP 工具调用失败: %v", err), IsError: true}, nil
	}
	return &tool.ToolResult{Content: result}, nil
}

// RegisterMCPTools 将 MCP 连接中发现的工具注册到工具注册表。
// 对应 TS: services/mcp/client.ts 中的 getMcpToolsCommandsAndResources()
func RegisterMCPTools(reg *tool.Registry, connections []*Connection) {
	for _, conn := range connections {
		for _, info := range conn.Tools {
			reg.Register(NewMCPTool(conn, info))
		}
	}
}
