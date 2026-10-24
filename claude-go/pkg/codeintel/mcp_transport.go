// mcp_transport.go — MCP Server 多传输层实现（stdio / HTTP / SSE）。
//
// 职责: 对外暴露 codeintel 工具集，支持三种传输方式。
//   - stdio:  JSON-RPC 2.0 over stdin/stdout（Claude Desktop 默认）
//   - http:   RESTful JSON-RPC 端点
//   - sse:    Server-Sent Events 流式推送
//
// 启动方式:
//   claude-go codeintel-mcp-server --repo /path/to/repo
//
// Claude Desktop 配置 (stdio):
//   {
//     "mcpServers": {
//       "code-intel": {
//         "command": "claude-go",
//         "args": ["codeintel-mcp-server", "--repo", "/path/to/repo"]
//       }
//     }
//   }
package codeintel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/mcp"
)

// ============================================================================
// MCPTransport 接口
// ============================================================================

// MCPTransport 定义 MCP Server 传输层抽象。
type MCPTransport interface {
	// Name 返回传输层名称（stdio / http / sse）。
	Name() string

	// Run 启动传输层（阻塞直到停止或出错）。
	Run(srv *MCPServerV2) error

	// Stop 优雅停止传输层。
	Stop(ctx context.Context) error
}

// ============================================================================
// MCPServerV2 — 支持多传输的 MCP Server
// ============================================================================

// GitNexusConfig GitNexus 运行时调优配置
type GitNexusConfig struct {
	// DefaultHeapMB V8 默认堆内存上限 (MB)。0 表示使用 GitNexus 内置默认值 8192。
	DefaultHeapMB int `json:"defaultHeapMB,omitempty"`
	// MaxHeapMB V8 最大堆内存上限 (MB)。0 表示 32768。
	MaxHeapMB int `json:"maxHeapMB,omitempty"`
	// PerThousandFilesMB 每 1000 个可解析文件额外增加的堆内存 (MB)。0 表示 256。
	PerThousandFilesMB int `json:"perThousandFilesMB,omitempty"`
	// PerHundredMBSourceMB 每 100MB 源码额外增加的堆内存 (MB)。0 表示 512。
	PerHundredMBSourceMB int `json:"perHundredMBSourceMB,omitempty"`
	// IndexTimeoutMin gitnexus analyze 单次执行超时 (分钟)。0 表示内置默认 120。
	IndexTimeoutMin int `json:"indexTimeoutMin,omitempty"`
}

// indexTimeout 从配置换算索引超时；未配置时返回 0（由 GitNexus 用内置默认）。
func indexTimeout(cfg *GitNexusConfig) time.Duration {
	if cfg != nil && cfg.IndexTimeoutMin > 0 {
		return time.Duration(cfg.IndexTimeoutMin) * time.Minute
	}
	return 0
}

// MCPServerV2 Code Intelligence MCP 服务器（支持多传输）。
type MCPServerV2 struct {
	RepoPath     string
	IndexBaseDir string
	Engine       *Engine
	GitNexusCfg  *GitNexusConfig
}

// NewMCPServerV2 创建 MCP 服务器（V2）。
// repoPath 为空时服务器仍可按多仓库模式工作（工具通过参数传入 repo_path）。
func NewMCPServerV2(repoPath string) *MCPServerV2 {
	srv := &MCPServerV2{RepoPath: repoPath}
	if repoPath != "" {
		srv.Engine = NewEngine(repoPath)
	}
	return srv
}

// ============================================================================
// stdioTransport 实现
// ============================================================================

// stdioTransport 通过标准输入输出传输 JSON-RPC 2.0 消息。
type stdioTransport struct{}

// NewStdioTransport 创建 stdio 传输层。
func NewStdioTransport() MCPTransport {
	return &stdioTransport{}
}

// Name 返回传输层名称 "stdio"。
func (t *stdioTransport) Name() string { return "stdio" }

// Run 以「一行一条 JSON-RPC 消息」的帧格式从 stdin 读消息并就地分发, 读到 EOF 视为正常退出。
//
// 算法解读：
//
//	原理：stdio 是 Claude Desktop 的默认传输 —— 请求与响应共用 stdin/stdout,
//	      没有长度前缀, 只能靠换行切帧, 每个非空行即一条完整消息。
//	关键不变量：一条消息必须恰好占一行。响应侧由 writeJSON 用 json.Marshal (紧凑)
//	      而不是 MarshalIndent 保证 —— 任何一处改成缩进输出, 客户端都会把一行拆成多条。
//	代价与边界：串行分发, 慢工具会阻塞后续请求 (无并发); 空行被跳过;
//	      EOF 返回 nil, 因为对端关闭 stdin 是正常结束而非错误。
//	设计权衡：选换行分帧而非 Content-Length 头, 是为了兼容最朴素的 stdio 客户端;
//	      代价是消息内不能出现裸换行 (JSON 字符串里的换行会被转义, 所以实际安全)。
func (t *stdioTransport) Run(srv *MCPServerV2) error {
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// 客户端关闭 stdin = 正常下线, 返回 nil 让上层干净退出而不是报错。
				return nil
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		srv.handleMessage(line)
	}
}

// Stop 无资源可释放 —— stdin/stdout 归进程所有, 停止即让 Run 的读循环随 EOF 退出, 故恒返回 nil。
func (t *stdioTransport) Stop(_ context.Context) error { return nil }

// ============================================================================
// httpTransport 实现
// ============================================================================

// httpTransport 通过 HTTP RESTful 端点暴露 MCP 方法。
type httpTransport struct {
	addr   string
	server *http.Server
	mu     sync.Mutex
}

// NewHTTPTransport 创建 HTTP 传输层。
// addr 格式如 "127.0.0.1:0"（自动分配端口）或 "127.0.0.1:8080"。
func NewHTTPTransport(addr string) MCPTransport {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	return &httpTransport{addr: addr}
}

// Name 返回传输层名称 "http"。
func (t *httpTransport) Name() string { return "http" }

// Run 注册 HTTP 路由并阻塞在 Serve 上, 直到 Stop 触发 http.ErrServerClosed (视为正常退出)。
//
// 注意：addr 配成 "127.0.0.1:0" 时端口由内核分配, 真实地址要等 Listen 之后才写回 t.addr,
// 因此调用方必须在 Run 起来之后经 Addr() 读取 —— 启动前拿到的是配置值而不是实际端口。
func (t *httpTransport) Run(srv *MCPServerV2) error {
	mux := http.NewServeMux()
	// 2026-07-28 无状态单端点: 路由经 Mcp-Method 头 / body method, 版本经
	// MCP-Protocol-Version 头 / _meta, 响应为 JSON-RPC 信封。
	mux.HandleFunc("/mcp", srv.handleHTTPMCP)
	// 兼容旧客户端 (2024-11-05 / 2025-11-25 裸端点)。
	mux.HandleFunc("/mcp/v1/initialize", srv.handleHTTPInitialize)
	mux.HandleFunc("/mcp/v1/tools/list", srv.handleHTTPToolsList)
	mux.HandleFunc("/mcp/v1/tools/call", srv.handleHTTPToolsCall)
	mux.HandleFunc("/mcp/v1/health", srv.handleHTTPHealth)

	t.server = &http.Server{
		Addr:              t.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", t.addr)
	if err != nil {
		return fmt.Errorf("http transport listen %s: %w", t.addr, err)
	}

	// 记录实际监听地址（自动分配端口时有用）
	t.mu.Lock()
	t.addr = ln.Addr().String()
	t.mu.Unlock()

	log.Printf("[mcp] http transport listening on http://%s", t.addr)
	if err := t.server.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop 优雅关闭 HTTP 服务 (等待在途请求跑完, 受 ctx 截止时间约束); 从未 Run 过时直接返回 nil。
func (t *httpTransport) Stop(ctx context.Context) error {
	if t.server == nil {
		return nil
	}
	return t.server.Shutdown(ctx)
}

// Addr 返回实际监听地址。
func (t *httpTransport) Addr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.addr
}

// ============================================================================
// sseTransport 实现
// ============================================================================

// sseClient 表示一个 SSE 连接客户端。
type sseClient struct {
	id      string
	ch      chan string
	created time.Time
}

// sseTransport 通过 Server-Sent Events 推送工具结果。
type sseTransport struct {
	addr    string
	server  *http.Server
	clients map[string]*sseClient
	mu      sync.RWMutex
	muAddr  sync.Mutex
}

// NewSSETransport 创建 SSE 传输层。
func NewSSETransport(addr string) MCPTransport {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	return &sseTransport{
		addr:    addr,
		clients: make(map[string]*sseClient),
	}
}

// Name 返回传输层名称 "sse"。
func (t *sseTransport) Name() string { return "sse" }

// Run 注册 SSE 路由 (/mcp/v1/events 长连接 + /mcp/v1/tools/call 投递入口) 并阻塞在 Serve 上。
//
// 与 httpTransport 同构: 真实监听地址同样在 Listen 之后才写回, 且由 muAddr 单独保护
// (不是 clients 的那把 mu —— 二者保护的数据不同, 混用会让地址读改写与客户端表互相阻塞)。
func (t *sseTransport) Run(srv *MCPServerV2) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/v1/events", t.handleSSEEvents)
	mux.HandleFunc("/mcp/v1/tools/call", srv.handleSSEToolsCall(t))
	mux.HandleFunc("/mcp/v1/health", srv.handleHTTPHealth)

	t.server = &http.Server{
		Addr:              t.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", t.addr)
	if err != nil {
		return fmt.Errorf("sse transport listen %s: %w", t.addr, err)
	}

	t.muAddr.Lock()
	t.addr = ln.Addr().String()
	t.muAddr.Unlock()

	log.Printf("[mcp] sse transport listening on http://%s", t.addr)
	if err := t.server.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop 优雅关闭 SSE 服务; 从未 Run 过时直接返回 nil。
func (t *sseTransport) Stop(ctx context.Context) error {
	if t.server == nil {
		return nil
	}
	return t.server.Shutdown(ctx)
}

// Addr 返回实际监听地址。
func (t *sseTransport) Addr() string {
	t.muAddr.Lock()
	defer t.muAddr.Unlock()
	return t.addr
}

// registerClient 注册 SSE 客户端。
func (t *sseTransport) registerClient(client *sseClient) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clients[client.id] = client
}

// unregisterClient 注销 SSE 客户端。
func (t *sseTransport) unregisterClient(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.clients[id]; ok {
		close(c.ch)
		delete(t.clients, id)
	}
}

// broadcast 向所有 SSE 客户端广播消息。
func (t *sseTransport) broadcast(eventType, data string) {
	t.mu.RLock()
	clients := make([]*sseClient, 0, len(t.clients))
	for _, c := range t.clients {
		clients = append(clients, c)
	}
	t.mu.RUnlock()

	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
	for _, c := range clients {
		select {
		case c.ch <- msg:
		default:
			// 客户端消费慢，跳过
		}
	}
}

// handleSSEEvents 处理 SSE 连接请求。
func (t *sseTransport) handleSSEEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	clientID := fmt.Sprintf("%s-%d", r.RemoteAddr, time.Now().UnixNano())
	client := &sseClient{
		id:      clientID,
		ch:      make(chan string, 64),
		created: time.Now(),
	}
	t.registerClient(client)
	defer t.unregisterClient(clientID)

	// 发送初始连接成功事件
	fmt.Fprintf(w, "event: connected\ndata: %s\n\n", clientID)
	flusher.Flush()

	// 心跳 ticker
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-client.ch:
			if !ok {
				return
			}
			_, _ = w.Write([]byte(msg))
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, "event: ping\ndata: %d\n\n", time.Now().Unix())
			flusher.Flush()
		}
	}
}

// ============================================================================
// JSON-RPC 2.0 消息结构（共享）
// ============================================================================

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *mcpError   `json:"error,omitempty"`
}

type mcpError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ============================================================================
// MCPServerV2 — 消息处理（兼容 stdio + HTTP + SSE）
// ============================================================================

// handleMessage 解析一条 JSON-RPC 文本并分发到对应方法; 这是 stdio 与 HTTP 共用的唯一入口。
//
// 算法解读：
//
//	原理：MCP 的 JSON-RPC 信封里, id 缺失即「通知」(notification), 按规范通知不得有响应。
//	      分发前先做协议版本闸门: 客户端可在 params._meta 里声明版本, 不在支持列表就报错。
//	关键不变量：只有 id 非 nil 的请求才允许写响应 —— 给通知回包会让客户端的
//	      请求-响应配对错位 (它按顺序认领响应), 后面的调用全部串位。
//	代价与边界：版本不匹配时直接拒绝, 不做降级协商; 未知方法按 id 有无决定
//	      是回 -32601 还是静默丢弃。
//	设计权衡：把版本校验放在分发之前而不是各 handler 内部, 避免每个方法各写一遍;
//	      代价是 server/discover 这类「免握手取版本」的 RPC 也要先过闸。
func (s *MCPServerV2) handleMessage(raw string) {
	var req mcpRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		// -32700: 连 JSON 都不是, 没有可回的 id (协议要求此时 id 为 null)。
		s.writeError(nil, -32700, "Parse error", err.Error())
		return
	}

	// id 缺失 = 通知, 后续所有错误分支都靠这个标志决定要不要回包。
	isNotification := req.ID == nil

	// 2026-07-28: 协议版本经 _meta 每次请求携带; 不在支持列表 → 报错并带 supported。
	if v := metaVersionFromParams(req.Params); v != "" && !mcp.IsSupported(v) {
		s.writeError(req.ID, mcp.ErrorUnsupportedProtocolVersion, "Unsupported protocol version",
			map[string]interface{}{
				"supported": mcp.SupportedProtocolVersions,
				"requested": v,
			})
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "notifications/initialized":
		// 无需响应
	case "server/discover":
		s.handleDiscover(req)
	case "ping":
		s.writeResult(req.ID, map[string]interface{}{})
	case "tools/list":
		s.handleToolsList(req)
	case "tools/call":
		s.handleToolsCall(req)
	default:
		if !isNotification {
			// 通知不回错包: 回了会给客户端送出一个它无法配对的响应, 后面的调用全部串位。
			s.writeError(req.ID, -32601, "Method not found", req.Method)
		}
	}
}

// metaVersionFromParams 从 JSON-RPC params 的 _meta 提取协议版本。
func metaVersionFromParams(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var p map[string]interface{}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	return metaVersionFromMap(p)
}

// metaVersionFromMap 从已反序列化的 params 里取 _meta 下的协议版本; 任一层缺失或类型不符都返回空串。
func metaVersionFromMap(p map[string]interface{}) string {
	raw, ok := p["_meta"]
	if !ok {
		return ""
	}
	meta, ok := raw.(map[string]interface{})
	if !ok {
		return ""
	}
	v, _ := meta[mcp.MetaKeyProtocolVersion].(string)
	return v
}

// handleInitialize 响应 initialize: 回报协议版本、tools 能力与服务器身份 (serverInfo)。
func (s *MCPServerV2) handleInitialize(req mcpRequest) {
	result := map[string]interface{}{
		"protocolVersion": mcp.ProtocolVersion,
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": s.serverInfo(),
	}
	s.writeResult(req.ID, result)
}

// handleDiscover 处理 server/discover（2026-07-28 新增 RPC，可选）。
// 让客户端免握手即可取回 supportedVersions / capabilities / serverInfo。
func (s *MCPServerV2) handleDiscover(req mcpRequest) {
	result := map[string]interface{}{
		"protocolVersion":   mcp.ProtocolVersion,
		"supportedVersions": mcp.SupportedProtocolVersions,
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": s.serverInfo(),
	}
	s.writeResult(req.ID, result)
}

// serverInfo 返回服务器身份（含 host，便于多实例部署时确认请求落到哪个 Pod）。
func (s *MCPServerV2) serverInfo() map[string]string {
	info := map[string]string{
		"name":    "claude-go-codeintel",
		"version": "1.0.0",
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		info["host"] = host
	}
	return info
}

// handleToolsList 响应 tools/list, 把 toolsList() 的静态声明原样回给客户端。
func (s *MCPServerV2) handleToolsList(req mcpRequest) {
	s.writeResult(req.ID, map[string]interface{}{"tools": s.toolsList()})
}

// toolsList 返回全部 codeintel 工具声明（stdio / HTTP / stateless 三入口共用）。
func (s *MCPServerV2) toolsList() []mcpTool {
	return []mcpTool{
		{
			Name:        "code_intel_init",
			Description: "Initialize code intelligence for a repository by running GitNexus analyze and Graphify update. Zero LLM tokens consumed during indexing.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string","description":"Absolute path to the git repository to index."}},"required":["repo_path"]}`),
		},
		{
			Name:        "code_intel_update",
			Description: "Incrementally update the code intelligence index by re-running GitNexus analyze and Graphify update. Zero LLM tokens consumed.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string","description":"Absolute path to the indexed repository."}},"required":["repo_path"]}`),
		},
		{
			Name:        "code_intel_status",
			Description: "Get the status of code intelligence index from GitNexus and Graphify.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string","description":"Absolute path to the indexed repository."}},"required":["repo_path"]}`),
		},
		{
			Name:        "code_intel_query",
			Description: "Query the code intelligence graph via GitNexus or Graphify CLI. Query types: navigate, impact, find_refs, path, explain, communities, god_nodes, surprises, cross_shard.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string","description":"Absolute path to the indexed repository."},"query_type":{"type":"string","enum":["navigate","impact","find_refs","communities","god_nodes","path","surprises","cross_shard","query","explain"],"description":"Query type."},"shard":{"type":"string"},"symbol":{"type":"string"},"file_path":{"type":"string"},"depth":{"type":"integer"},"top_n":{"type":"integer"},"target_shard":{"type":"string"},"target_symbol":{"type":"string"}},"required":["repo_path","query_type"]}`),
		},
		{
			Name:        "code_intel_branch",
			Description: "Branch management for code intelligence: detect_changes, status, reindex.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string","description":"Absolute path to the indexed repository."},"action":{"type":"string","enum":["switch","detect_changes","status","reindex"],"description":"Action to perform."},"branch_name":{"type":"string"}},"required":["repo_path","action"]}`),
		},
	}
}

// handleToolsCall 响应 tools/call: 解出工具名与参数后执行, 并把结果包成 MCP 的 content 数组。
//
// 注意：executeTool 用 ("", true) 同时表示「工具未找到」和「工具本身失败」, 这里靠
// 「isError 且文本为空」把前者升级为 JSON-RPC 层错误 (-32601), 后者则留在 result 里
// 以 IsError=true 返回 —— 两种失败对客户端的意义不同, 不能合并。
func (s *MCPServerV2) handleToolsCall(req mcpRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(req.ID, -32602, "Invalid params", err.Error())
		return
	}

	resultText, isError := s.executeTool(params.Name, params.Arguments)
	if isError && resultText == "" {
		// 工具未找到
		s.writeError(req.ID, -32601, "Tool not found", params.Name)
		return
	}

	result := mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: resultText}},
		IsError: isError,
	}
	s.writeResult(req.ID, result)
}

// executeTool 执行工具调用并返回结果文本和错误标志。
// 返回 ("", true) 表示工具未找到。
func (s *MCPServerV2) executeTool(name string, args json.RawMessage) (string, bool) {
	switch name {
	case "code_intel_init":
		return s.toolInit(args)
	case "code_intel_update":
		return s.toolUpdate(args)
	case "code_intel_status":
		return s.toolStatus(args)
	case "code_intel_query":
		return s.toolQuery(args)
	case "code_intel_branch":
		return s.toolBranch(args)
	default:
		return "", true
	}
}

// ============================================================================
// Tool 实现（复用 Engine 逻辑）
// ============================================================================

// calcHeapMB 根据仓库规模与配置计算 GitNexus V8 堆内存上限。
//
// 公式: heap = default + (fileCount/1000)*per1K + (sourceMB/100)*per100MB
// 其中 fileCount 为 GitNexus 支持语言的可解析文件数，sourceMB 为对应源码总大小。
// 结果在 [default, max] 区间内。返回 0 表示让 GitNexus 使用其内置默认值。
func calcHeapMB(repoPath string, cfg *GitNexusConfig) int {
	if cfg == nil {
		cfg = &GitNexusConfig{}
	}
	base := cfg.DefaultHeapMB
	if base <= 0 {
		base = 8192
	}
	max := cfg.MaxHeapMB
	if max <= 0 {
		max = 32768
	}
	per1K := cfg.PerThousandFilesMB
	if per1K <= 0 {
		per1K = 256
	}
	per100MB := cfg.PerHundredMBSourceMB
	if per100MB <= 0 {
		per100MB = 512
	}

	fileCount, sourceMB := estimateRepoSize(repoPath)

	heap := base
	heap += (fileCount / 1000) * per1K
	heap += (sourceMB / 100) * per100MB

	if heap > max {
		heap = max
	}
	if heap < base {
		heap = base
	}
	return heap
}

// estimateRepoSize 统计仓库中 GitNexus 支持语言的可解析文件数及源码总大小 (MB)。
func estimateRepoSize(repoPath string) (fileCount int, sourceMB int) {
	exts := map[string]bool{
		".c": true, ".h": true, ".cpp": true, ".cc": true, ".cxx": true, ".hpp": true,
		".go": true, ".rs": true, ".java": true, ".py": true,
		".js": true, ".jsx": true, ".ts": true, ".tsx": true,
		".php": true, ".swift": true, ".kt": true, ".dart": true,
		".cs": true, ".rb": true, ".vue": true, ".cbl": true, ".cob": true,
	}
	var totalBytes int64
	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !exts[ext] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		fileCount++
		totalBytes += info.Size()
		return nil
	})
	return fileCount, int(totalBytes / (1024 * 1024))
}

// withRepoLock 为"同一仓库分支"获取跨进程排他锁（多实例共享 PV 场景）。
// 返回的释放函数供 defer；未启用集中索引（IndexBaseDir 为空）时为 no-op。
func (s *MCPServerV2) withRepoLock(repoPath string) (func(), error) {
	if s.IndexBaseDir == "" {
		return func() {}, nil
	}
	im := NewIndexManager(s.IndexBaseDir)
	if im == nil {
		return func() {}, nil
	}
	lk, err := im.LockRepo(repoPath)
	if err != nil {
		return func() {}, fmt.Errorf("acquire repo lock: %w", err)
	}
	return func() { _ = lk.Release() }, nil
}

// toolInit 全量建索引 (GitNexus analyze + Graphify update), 返回两套索引各自的产出或错误。
//
// 注意：整段执行被 withRepoLock 串行化, 且锁的粒度是「同一仓库」而不是全局 ——
// 多实例共享 PV 时, 不同仓库可并行, 同仓库只能有一个索引进程。
func (s *MCPServerV2) toolInit(args json.RawMessage) (string, bool) {
	var in struct{ RepoPath string `json:"repo_path"` }
	if err := json.Unmarshal(args, &in); err != nil {
		return fmt.Sprintf("parse error: %v", err), true
	}
	release, err := s.withRepoLock(in.RepoPath)
	if err != nil {
		return err.Error(), true
	}
	defer release()

	gn := NewGitNexus(in.RepoPath)
	gn.IndexBaseDir = s.IndexBaseDir
	gn.MaxHeapMB = calcHeapMB(in.RepoPath, s.GitNexusCfg)
	gn.IndexTimeout = indexTimeout(s.GitNexusCfg)
	gf := NewGraphify(in.RepoPath)
	gf.IndexBaseDir = s.IndexBaseDir

	o := RunIndex(gn, gf)
	result := map[string]interface{}{
		"status":         o.StatusOr("initialized"),
		"repo_path":      in.RepoPath,
		"gitnexus":       safeMCPResult(o.GitNexus),
		"gitnexus_error": errMCPString(o.GNErr),
		"graphify":       safeMCPResult(o.Graphify),
		"graphify_error": errMCPString(o.GFErr),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), false
}

// toolUpdate 增量更新索引 —— 与 toolInit 走同一条 RunIndex 路径, 只是 status 缺省值不同。
//
// 注意：GitNexus/Graphify 的 analyze 本身是增量语义, 所以这里没有单独的「增量」实现,
// 差异仅在返回给模型的 status 文案 (initialized / updated)。
func (s *MCPServerV2) toolUpdate(args json.RawMessage) (string, bool) {
	var in struct{ RepoPath string `json:"repo_path"` }
	if err := json.Unmarshal(args, &in); err != nil {
		return fmt.Sprintf("parse error: %v", err), true
	}
	release, err := s.withRepoLock(in.RepoPath)
	if err != nil {
		return err.Error(), true
	}
	defer release()

	gn := NewGitNexus(in.RepoPath)
	gn.IndexBaseDir = s.IndexBaseDir
	gn.MaxHeapMB = calcHeapMB(in.RepoPath, s.GitNexusCfg)
	gn.IndexTimeout = indexTimeout(s.GitNexusCfg)
	gf := NewGraphify(in.RepoPath)
	gf.IndexBaseDir = s.IndexBaseDir

	o := RunIndex(gn, gf)
	result := map[string]interface{}{
		"status":         o.StatusOr("updated"),
		"repo_path":      in.RepoPath,
		"gitnexus":       safeMCPResult(o.GitNexus),
		"gitnexus_error": errMCPString(o.GNErr),
		"graphify":       safeMCPResult(o.Graphify),
		"graphify_error": errMCPString(o.GFErr),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), false
}

// toolStatus 查询索引状态; 固定查 "main" 分支 (GitNexus/Graphify 的索引不做分支隔离)。
func (s *MCPServerV2) toolStatus(args json.RawMessage) (string, bool) {
	var in struct{ RepoPath string `json:"repo_path"` }
	if err := json.Unmarshal(args, &in); err != nil {
		return fmt.Sprintf("parse error: %v", err), true
	}
	engine := NewEngine(in.RepoPath)
	qr, err := engine.Status("main")
	if err != nil {
		return fmt.Sprintf("status error: %v", err), true
	}
	out, _ := json.MarshalIndent(qr.Results, "", "  ")
	return string(out), false
}

// toolQuery 按 query_type 分派到 Engine 的图查询方法, 统一返回 Results 的缩进 JSON。
//
// 注意：query 与 explain 两条分支是例外 —— 它们绕开 Engine 直接打 GitNexus/Graphify CLI,
// 并且自行序列化返回 (query 甚至同时返回两套引擎的结果与错误), 因此不会走到末尾的
// qr.Results 统一出口; 新增 query_type 时别默认「一定返回 QueryResult」。
func (s *MCPServerV2) toolQuery(args json.RawMessage) (string, bool) {
	var in struct {
		RepoPath     string `json:"repo_path"`
		QueryType    string `json:"query_type"`
		Shard        string `json:"shard,omitempty"`
		Symbol       string `json:"symbol,omitempty"`
		FilePath     string `json:"file_path,omitempty"`
		Depth        int    `json:"depth,omitempty"`
		TopN         int    `json:"top_n,omitempty"`
		TargetShard  string `json:"target_shard,omitempty"`
		TargetSymbol string `json:"target_symbol,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return fmt.Sprintf("parse error: %v", err), true
	}

	engine := NewEngine(in.RepoPath)
	var qr *QueryResult
	var err error

	switch in.QueryType {
	case "navigate":
		qr, err = engine.Navigate("main", NavigateQuery{Symbol: in.Symbol, Depth: in.Depth, Shard: in.Shard})
	case "impact":
		qr, err = engine.Impact("main", ImpactQuery{FilePath: in.FilePath, Depth: in.Depth})
	case "find_refs":
		qr, err = engine.FindRefs("main", NavigateQuery{Symbol: in.Symbol, Shard: in.Shard})
	case "communities":
		qr, err = engine.Communities("main", CommunityQuery{Shard: in.Shard, TopN: in.TopN})
	case "god_nodes":
		qr, err = engine.GodNodes("main", in.Shard, in.TopN)
	case "path":
		qr, err = engine.Path("main", in.Symbol, in.TargetSymbol)
	case "surprises":
		qr, err = engine.Surprises("main", in.Shard, in.TopN)
	case "cross_shard":
		qr, err = engine.CrossShard("main", CrossShardQuery{Symbol: in.Symbol, TargetShard: in.TargetShard})
	case "query":
		gn := NewGitNexus(in.RepoPath)
		gf := NewGraphify(in.RepoPath)
		gnQr, gnErr := gn.Query(in.Symbol)
		gfQr, gfErr := gf.Query(in.Symbol)
		result := map[string]interface{}{
			"gitnexus":       safeMCPResult(gnQr),
			"gitnexus_error": errMCPString(gnErr),
			"graphify":       safeMCPResult(gfQr),
			"graphify_error": errMCPString(gfErr),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), false
	case "explain":
		gf := NewGraphify(in.RepoPath)
		qr, err = gf.Explain(in.Symbol)
	default:
		return fmt.Sprintf("unknown query_type: %s", in.QueryType), true
	}

	if err != nil {
		return fmt.Sprintf("query error: %v", err), true
	}
	out, _ := json.MarshalIndent(qr.Results, "", "  ")
	return string(out), false
}

// toolBranch 处理分支相关动作: switch / detect_changes / status / reindex。
//
// 注意：只有 reindex 会取仓库锁并真正跑索引 —— 且它用 release() 显式释放而不是 defer,
// 因为 switch/detect_changes/status 这些无锁分支共用同一个函数, defer 会让无锁路径也持锁。
// switch 分支不执行 git checkout, 只回一条提示命令: 索引不做分支隔离, 由调用方自己决定何时切。
func (s *MCPServerV2) toolBranch(args json.RawMessage) (string, bool) {
	var in struct {
		RepoPath   string `json:"repo_path"`
		Action     string `json:"action"`
		BranchName string `json:"branch_name,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return fmt.Sprintf("parse error: %v", err), true
	}

	gn := NewGitNexus(in.RepoPath)
	gn.IndexBaseDir = s.IndexBaseDir
	gn.MaxHeapMB = calcHeapMB(in.RepoPath, s.GitNexusCfg)
	gn.IndexTimeout = indexTimeout(s.GitNexusCfg)
	gf := NewGraphify(in.RepoPath)
	gf.IndexBaseDir = s.IndexBaseDir
	var result map[string]interface{}

	switch in.Action {
	case "switch":
		if in.BranchName == "" {
			return "branch_name required for switch", true
		}
		result = map[string]interface{}{
			"action":       "switch",
			"branch":       in.BranchName,
			"note":         "GitNexus/Graphify indexes are not branch-isolated. After switching, run 'reindex' to refresh.",
			"git_checkout": fmt.Sprintf("git -C %s checkout %s", in.RepoPath, in.BranchName),
		}
	case "detect_changes":
		qr, e := gn.DetectChanges()
		result = map[string]interface{}{
			"action":         "detect_changes",
			"gitnexus":       safeMCPResult(qr),
			"gitnexus_error": errMCPString(e),
		}
	case "status":
		gnQr, gnErr := gn.Status()
		result = map[string]interface{}{
			"action":           "status",
			"gitnexus":         safeMCPResult(gnQr),
			"gitnexus_error":   errMCPString(gnErr),
			"gitnexus_indexed": gitnexusIndexedFrom(gnQr, gnErr),
			"graphify_indexed": gf.IsIndexed(),
		}
	case "reindex":
		release, lockErr := s.withRepoLock(in.RepoPath)
		if lockErr != nil {
			return lockErr.Error(), true
		}
		o := RunIndex(gn, gf)
		release()
		result = map[string]interface{}{
			"action":         "reindex",
			"gitnexus":       safeMCPResult(o.GitNexus),
			"gitnexus_error": errMCPString(o.GNErr),
			"graphify":       safeMCPResult(o.Graphify),
			"graphify_error": errMCPString(o.GFErr),
		}
	default:
		return fmt.Sprintf("unknown action: %s", in.Action), true
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), false
}

// ============================================================================
// 响应输出辅助
// ============================================================================

// writeResult 把 result 包成 JSON-RPC 2.0 成功信封并写出。
func (s *MCPServerV2) writeResult(id interface{}, result interface{}) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	s.writeJSON(resp)
}

// writeError 把错误包成 JSON-RPC 2.0 错误信封并写出; id 传 nil 表示连请求 id 都无法确定。
func (s *MCPServerV2) writeError(id interface{}, code int, message string, data interface{}) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mcpError{Code: code, Message: message, Data: data},
	}
	s.writeJSON(resp)
}

// writeJSON 用紧凑 JSON + 换行把信封写到 stdout; 必须保持单行, 否则会破坏 stdio 的换行分帧。
func (s *MCPServerV2) writeJSON(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Println(string(data))
}

// safeMCPResult 取查询结果的 Results 供序列化; 空结果与 nil 结果都归一成 JSON null。
func safeMCPResult(qr *QueryResult) interface{} {
	if qr == nil || qr.Results == nil {
		return nil
	}
	return qr.Results
}

// errMCPString 把 error 转成字符串; nil 转空串, 便于直接塞进结果 JSON。
func errMCPString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ============================================================================
// HTTP Handlers
// ============================================================================

// handleHTTPInitialize 处理旧客户端 (2024-11-05 / 2025-11-25) 的裸端点 POST /mcp/v1/initialize。
func (s *MCPServerV2) handleHTTPInitialize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	result := map[string]interface{}{
		"protocolVersion": mcp.ProtocolVersion,
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": s.serverInfo(),
	}
	writeJSON(w, http.StatusOK, result)
}

// handleHTTPToolsList 处理旧客户端的裸端点 POST /mcp/v1/tools/list。
func (s *MCPServerV2) handleHTTPToolsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"tools": s.toolsList()})
}

// handleHTTPToolsCall 处理旧客户端的裸端点 POST /mcp/v1/tools/call, 把失败直接映射成 HTTP 状态码。
//
// 注意：与 stdio 的 handleToolsCall 不同, 这里没有 JSON-RPC 信封 —— 工具执行失败用
// 200 + isError, 工具不存在用 404, 参数解不动用 400, 三条错误路径的状态码是接口契约的一部分。
func (s *MCPServerV2) handleHTTPToolsCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	resultText, isError := s.executeTool(params.Name, params.Arguments)
	if isError && resultText == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tool not found: " + params.Name})
		return
	}

	result := mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: resultText}},
		IsError: isError,
	}
	writeJSON(w, http.StatusOK, result)
}

// handleHTTPHealth 探活端点 (GET/POST 皆可): 回报协议版本、时间与 Pod hostname, 便于确认请求落到哪个实例。
func (s *MCPServerV2) handleHTTPHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	resp := map[string]interface{}{
		"ok":      true,
		"version": mcp.ProtocolVersion,
		"time":    time.Now(),
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		resp["host"] = host
	}
	if s.RepoPath != "" {
		resp["repoPath"] = s.RepoPath
	}
	writeJSON(w, http.StatusOK, resp)
}

// rpcError 统一 /mcp 端点内部错误结构。
type rpcError struct {
	code    int
	message string
	data    interface{}
}

// handleHTTPMCP 处理 2026-07-28 无状态统一端点 POST /mcp。
//
// 请求: MCP-Protocol-Version 头（或 body _meta）声明协议版本; Mcp-Method 头
// 声明要调用的方法（tools/list / tools/call / server/discover / ping）;
// body 为 JSON-RPC 信封 {jsonrpc,id,method,params}。
// 响应: JSON-RPC 信封 {jsonrpc,id,result|error}, 附 MCP-Protocol-Version 与
// X-Claude-Go-Backend（Pod hostname, 用于多实例部署下验证粘性/均衡）。
func (s *MCPServerV2) handleHTTPMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	// 解析 JSON-RPC 信封（空 body 也允许，路由全靠头）。
	var body mcpRequest
	if bodyBytes, err := io.ReadAll(r.Body); err == nil && len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &body)
	}

	// 版本协商: MCP-Protocol-Version 头优先, 否则 body _meta。
	declared := r.Header.Get(mcp.HeaderProtocolVersion)
	if declared == "" {
		declared = metaVersionFromParams(body.Params)
	}
	w.Header().Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion)
	if host, err := os.Hostname(); err == nil && host != "" {
		w.Header().Set(mcp.HeaderBackend, host)
	}
	if declared != "" && !mcp.IsSupported(declared) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      body.ID,
			"error": map[string]interface{}{
				"code":    mcp.ErrorUnsupportedProtocolVersion,
				"message": "Unsupported protocol version",
				"data": map[string]interface{}{
					"supported": mcp.SupportedProtocolVersions,
					"requested": declared,
				},
			},
		})
		return
	}

	// 路由: Mcp-Method 头（2026-07-28）优先, 回退 body method。
	method := r.Header.Get(mcp.HeaderMethod)
	if method == "" {
		method = body.Method
	}

	var result interface{}
	var rpcErr *rpcError
	switch method {
	case "server/discover":
		result = map[string]interface{}{
			"protocolVersion":   mcp.ProtocolVersion,
			"supportedVersions": mcp.SupportedProtocolVersions,
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":        s.serverInfo(),
		}
	case "tools/list":
		result = map[string]interface{}{"tools": s.toolsList()}
	case "tools/call":
		name, args, err := decodeToolCall(body.Params)
		if err != nil {
			rpcErr = &rpcError{code: mcp.ErrorInvalidParams, message: "Invalid params"}
			break
		}
		text, isErr := s.executeTool(name, args)
		if isErr && text == "" {
			rpcErr = &rpcError{code: mcp.ErrorUnknownTool, message: "Tool not found: " + name}
			break
		}
		result = mcpToolResult{Content: []mcpContent{{Type: "text", Text: text}}, IsError: isErr}
	case "ping":
		result = map[string]interface{}{}
	default:
		rpcErr = &rpcError{code: mcp.ErrorMethodNotFound, message: "Method not found: " + method}
	}

	if rpcErr != nil {
		errObj := map[string]interface{}{"code": rpcErr.code, "message": rpcErr.message}
		if rpcErr.data != nil {
			errObj["data"] = rpcErr.data
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"jsonrpc": "2.0", "id": body.ID, "error": errObj,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"jsonrpc": "2.0", "id": body.ID, "result": result,
	})
}

// decodeToolCall 从 tools/call 的 params 解出工具名与参数。
func decodeToolCall(params json.RawMessage) (name string, args json.RawMessage, err error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", nil, err
	}
	return p.Name, p.Arguments, nil
}

// ============================================================================
// SSE Handlers
// ============================================================================

// handleSSEToolsCall 返回 SSE 版 tools/call 的 handler: 立即回 202, 结果异步经 SSE 广播。
//
// 算法解读：
//
//	原理：SSE 只有服务端到客户端的单向通道, 所以请求-响应被拆成两段 ——
//	      POST 只负责投递 (202 Accepted), 真正的结果由 broadcast 推给所有在线连接。
//	关键不变量：投递必须立刻返回, 不能等工具跑完 —— 索引类工具动辄分钟级,
//	      同步等待会顶穿客户端的 HTTP 超时; 因此执行被放进 goroutine。
//	代价与边界：payload 里只带 tool 名, 没有请求 id, 多客户端或并发调用时
//	      无法把结果与请求配对 (只能按工具名和到达顺序自行关联)。
//	设计权衡：用「广播」而不是「按连接回推」, 实现简单但牺牲了定向性;
//	      单客户端场景够用, 多客户端共享一个 server 时会互相看到对方的工具结果。
func (s *MCPServerV2) handleSSEToolsCall(t *sseTransport) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}

		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		// 异步执行工具，结果通过 SSE 广播
		go func() {
			resultText, isError := s.executeTool(params.Name, params.Arguments)

			var payload map[string]interface{}
			if isError && resultText == "" {
				payload = map[string]interface{}{
					"tool":   params.Name,
					"error":  "tool not found: " + params.Name,
					"status": "error",
				}
			} else {
				payload = map[string]interface{}{
					"tool":    params.Name,
					"result":  resultText,
					"isError": isError,
					"status":  "ok",
				}
			}
			data, _ := json.Marshal(payload)
			t.broadcast("tool_result", string(data))
		}()

		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"accepted": true,
			"tool":     params.Name,
			"message":  "result will be pushed via SSE",
		})
	}
}

// ============================================================================
// HTTP 辅助函数
// ============================================================================

// writeJSON 写出一个 JSON 响应: 统一 UTF-8 + no-store, 并在编码前先落状态码。
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
