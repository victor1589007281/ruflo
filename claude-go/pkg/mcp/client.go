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
	"errors"
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
//
// 多实例部署（2026-07-28 无状态协议）时用 URLs 配置后端列表：
//   - 会话粘性: 同一会话键（StickyKey 或每请求传入）经一致性哈希恒命中同一后端，
//     保留该实例的进程内查询缓存热态;
//   - 均衡: 不同会话键哈希散开，请求均匀分布到各后端。
type ServerConfig struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"` // stdio, http
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	URLs      []string          `json:"urls,omitempty"` // 多后端列表（HTTP，优先于 URL）
	StickyKey string            `json:"stickyKey,omitempty"` // 会话粘性键；空则按连接生成
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

	// 2026-07-28 无状态协议 + 多后端粘性负载均衡。
	protocolVersion string            // 协商后的协议版本
	backends        []string          // HTTP 后端基址列表
	ring            *ConsistentHash   // 一致性哈希环（会话粘性）
	stickyKey       string            // 连接级默认粘性键

	// 多后端诊断: 记录最近一次 HTTP 请求实际落到的后端基址与服务端 Pod host
	// （X-Claude-Go-Backend 响应头）。用于多实例部署下验证会话粘性/负载分布。
	lastBackend     string
	lastBackendHost string
}

// LastBackend 返回最近一次 HTTP 请求实际落到的后端基址。
func (conn *Connection) LastBackend() string {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.lastBackend
}

// LastBackendHost 返回最近一次 HTTP 响应中服务端上报的 Pod host
// （X-Claude-Go-Backend），空串表示后端未上报。
func (conn *Connection) LastBackendHost() string {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.lastBackendHost
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
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// envelopeResponse 2026-07-28 统一 /mcp 端点的 JSON-RPC 信封响应。
type envelopeResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// ProtocolError 带协议语义的 MCP 错误（含 data.supported 等协商信息）。
type ProtocolError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message)
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

	conn.protocolVersion = ProtocolVersion

	// 发送 initialize。启动阶段必须继承调用方 ctx, 避免异常 MCP 进程卡死 Bot 初始化。
	// stdio 下 2026-07-28 为建议性变更: 仍走握手, 但从响应协商实际协议版本。
	initResult, err := conn.sendRequestCtx(ctx, "initialize", map[string]interface{}{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]string{
			"name":    "claude-go",
			"version": "1.0.0",
		},
	})
	if err != nil {
		return fmt.Errorf("initialize 失败: %w", err)
	}
	if len(initResult) > 0 {
		var ir struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(initResult, &ir); err == nil && ir.ProtocolVersion != "" {
			conn.protocolVersion = ir.ProtocolVersion
		}
	}

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

// connectHTTP 通过 HTTP 传输连接 MCP 服务器。
//
// 2026-07-28 无状态流程: server/discover 协商协议版本 → tools/list 取工具。
// 服务器不支持 stateless（返回 UnsupportedProtocolVersionError）时回退旧
// initialize 流程（/mcp/v1/initialize），保证对旧版 MCP 服务器向后兼容。
func (c *Client) connectHTTP(ctx context.Context, conn *Connection) error {
	if conn.Config.URL == "" && len(conn.Config.URLs) == 0 {
		return fmt.Errorf("HTTP 传输需要配置 url 或 urls")
	}
	conn.httpClient = &http.Client{Timeout: 60 * time.Second}

	// 多后端: URLs 优先，其次单 URL；一致性哈希环实现会话粘性。
	conn.backends = append([]string{}, conn.Config.URLs...)
	if len(conn.backends) == 0 {
		conn.backends = []string{conn.Config.URL}
	}
	conn.ring = NewConsistentHash(conn.backends, 0)
	if conn.Config.StickyKey != "" {
		conn.stickyKey = conn.Config.StickyKey
	} else {
		conn.stickyKey = fmt.Sprintf("conn-%d", conn.nextID.Add(1))
	}
	conn.protocolVersion = ProtocolVersion

	// 无状态协商。
	if err := conn.discover(ctx); err != nil {
		if v := conn.downgradeFromErr(err); v != "" {
			conn.protocolVersion = v
			if lerr := conn.legacyConnect(ctx, v); lerr != nil {
				conn.Status = "error"
				return lerr
			}
			conn.Status = "connected"
			return nil
		}
		conn.Status = "error"
		return fmt.Errorf("server/discover 失败: %w", err)
	}
	if err := conn.fetchTools(ctx); err != nil {
		conn.Status = "connected" // 工具列表失败不影响连接
		return nil
	}
	conn.Status = "connected"
	return nil
}

// discover 调用 server/discover（2026-07-28）协商协议版本。
func (conn *Connection) discover(ctx context.Context) error {
	res, err := conn.sendRequestCtx(ctx, "server/discover", nil)
	if err != nil {
		return err
	}
	var d struct {
		ProtocolVersion    string   `json:"protocolVersion"`
		SupportedVersions  []string `json:"supportedVersions"`
	}
	if err := json.Unmarshal(res, &d); err == nil && d.ProtocolVersion != "" {
		conn.protocolVersion = d.ProtocolVersion
	}
	return nil
}

// fetchTools 拉取工具列表。
func (conn *Connection) fetchTools(ctx context.Context) error {
	res, err := conn.sendRequestCtx(ctx, "tools/list", nil)
	if err != nil {
		return err
	}
	var tl struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(res, &tl); err != nil {
		return err
	}
	conn.Tools = tl.Tools
	return nil
}

// legacyConnect 旧协议（≤2025-11-25）回退: initialize 握手 + tools/list。
func (conn *Connection) legacyConnect(ctx context.Context, version string) error {
	initBody, _ := json.Marshal(map[string]interface{}{
		"protocolVersion": version,
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]string{
			"name":    "claude-go",
			"version": "1.0.0",
		},
	})
	backend := conn.backends[0]
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(backend, "/")+"/mcp/v1/initialize", bytes.NewReader(initBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := conn.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("initialize 请求失败: %w", err)
	}
	_ = resp.Body.Close()

	res, err := conn.httpRequest(ctx, "tools/list", nil, backend)
	if err != nil {
		return err
	}
	var tl struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(res, &tl); err == nil {
		conn.Tools = tl.Tools
	}
	return nil
}

// downgradeFromErr 从 UnsupportedProtocolVersionError 的 data.supported 解析
// 可用的最高兼容版本；非版本类错误返回空串。
func (conn *Connection) downgradeFromErr(err error) string {
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		return ""
	}
	if pe.Code != ErrorUnsupportedProtocolVersion {
		return ""
	}
	var data struct {
		Supported []string `json:"supported"`
	}
	if len(pe.Data) > 0 {
		_ = json.Unmarshal(pe.Data, &data)
	}
	for _, v := range SupportedProtocolVersions {
		for _, s := range data.Supported {
			if s == v {
				return v
			}
		}
	}
	return ""
}

// clientCaps 声明客户端能力（2026-07-28 每次请求经 _meta 携带）。
func (conn *Connection) clientCaps() map[string]interface{} {
	return map[string]interface{}{
		"tools": map[string]interface{}{"listChanged": false},
	}
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

	// HTTP 传输: 会话粘性一致性哈希选后端 → 2026-07-28 无状态端点。
	if conn.Config.Transport == "http" {
		sticky := StickyKeyFromContext(ctx)
		if sticky == "" {
			sticky = conn.stickyKey
		}
		backend := conn.ring.Get(sticky)
		if backend == "" {
			backend = conn.backends[0]
		}
		return conn.httpRequest(ctx, method, params, backend)
	}

	id := conn.nextID.Add(1)
	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
	}
	if pm, ok := params.(map[string]interface{}); ok {
		req.Params = WithMetaParams(pm, conn.protocolVersion, "claude-go", "1.0.0", conn.clientCaps())
	} else {
		req.Params = params
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

// httpRequest 向指定后端发送 2026-07-28 无状态 JSON-RPC 请求。
// body 为 JSON-RPC 信封 + _meta（协议版本/客户端身份/能力）; 请求头带
// MCP-Protocol-Version 与 Mcp-Method（头级路由）。响应为 JSON-RPC 信封，
// 返回 result 原始 JSON。
func (conn *Connection) httpRequest(ctx context.Context, method string, params interface{}, backend string) (json.RawMessage, error) {
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      conn.nextID.Add(1),
		"method":  method,
	}
	if paramsMap, ok := params.(map[string]interface{}); ok {
		body["params"] = WithMetaParams(paramsMap, conn.protocolVersion, "claude-go", "1.0.0", conn.clientCaps())
	} else if params != nil {
		body["params"] = params
	} else {
		body["params"] = WithMetaParams(nil, conn.protocolVersion, "claude-go", "1.0.0", conn.clientCaps())
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(backend, "/")+"/mcp", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(HeaderProtocolVersion, conn.protocolVersion)
	req.Header.Set(HeaderMethod, method)

	resp, err := conn.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 多后端诊断: 记录实际落到的后端与 Pod host（供粘性/分布验证）。
	conn.lastBackend = backend
	conn.lastBackendHost = resp.Header.Get(HeaderBackend)

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusBadRequest {
		return nil, parseEnvelopeError(respBytes)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var env envelopeResponse
	if err := json.Unmarshal(respBytes, &env); err != nil {
		return nil, fmt.Errorf("解析 MCP 响应失败: %w", err)
	}
	if env.Error != nil {
		return nil, &ProtocolError{Code: env.Error.Code, Message: env.Error.Message, Data: env.Error.Data}
	}
	return env.Result, nil
}

// parseEnvelopeError 从错误响应体解析 ProtocolError（含协商 data.supported）。
func parseEnvelopeError(body []byte) error {
	var env envelopeResponse
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		return fmt.Errorf("HTTP 400: %s", string(body))
	}
	return &ProtocolError{Code: env.Error.Code, Message: env.Error.Message, Data: env.Error.Data}
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

func (t *MCPTool) Call(ctx context.Context, input json.RawMessage, toolCtx *tool.ToolContext) (*tool.ToolResult, error) {
	// 会话粘性: 从工具上下文推导粘性键写入 ctx，多后端时经一致性哈希命中同一实例。
	if key := stickyKeyFromToolCtx(toolCtx); key != "" {
		ctx = WithStickyKey(ctx, key)
	}
	result, err := t.conn.CallTool(ctx, t.info.Name, input)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("MCP 工具调用失败: %v", err), IsError: true}, nil
	}
	return &tool.ToolResult{Content: result}, nil
}

// stickyKeyFromToolCtx 从工具上下文推导会话粘性键：
// 优先 AgentID（子代理隔离），其次第一条消息 UUID（会话隔离）。
func stickyKeyFromToolCtx(tctx *tool.ToolContext) string {
	if tctx == nil {
		return ""
	}
	if tctx.AgentID != "" {
		return "agent:" + string(tctx.AgentID)
	}
	for i := range tctx.Messages {
		if m := tctx.Messages[i]; m.UUID != "" {
			return "session:" + m.UUID
		}
	}
	return ""
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
