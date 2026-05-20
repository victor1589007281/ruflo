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
	"strings"
	"sync"
	"time"
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

// MCPServerV2 Code Intelligence MCP 服务器（支持多传输）。
type MCPServerV2 struct {
	RepoPath string
	Engine   *Engine
}

// NewMCPServerV2 创建 MCP 服务器（V2）。
func NewMCPServerV2(repoPath string) *MCPServerV2 {
	return &MCPServerV2{
		RepoPath: repoPath,
		Engine:   NewEngine(repoPath),
	}
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

func (t *stdioTransport) Name() string { return "stdio" }

func (t *stdioTransport) Run(srv *MCPServerV2) error {
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
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

func (t *httpTransport) Name() string { return "http" }

func (t *httpTransport) Run(srv *MCPServerV2) error {
	mux := http.NewServeMux()
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

func (t *sseTransport) Name() string { return "sse" }

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

func (s *MCPServerV2) handleMessage(raw string) {
	var req mcpRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		s.writeError(nil, -32700, "Parse error", err.Error())
		return
	}

	isNotification := req.ID == nil

	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "notifications/initialized":
		// 无需响应
	case "tools/list":
		s.handleToolsList(req)
	case "tools/call":
		s.handleToolsCall(req)
	default:
		if !isNotification {
			s.writeError(req.ID, -32601, "Method not found", req.Method)
		}
	}
}

func (s *MCPServerV2) handleInitialize(req mcpRequest) {
	result := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]string{
			"name":    "claude-go-codeintel",
			"version": "1.0.0",
		},
	}
	s.writeResult(req.ID, result)
}

func (s *MCPServerV2) handleToolsList(req mcpRequest) {
	tools := []mcpTool{
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
	s.writeResult(req.ID, map[string]interface{}{"tools": tools})
}

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

func (s *MCPServerV2) toolInit(args json.RawMessage) (string, bool) {
	var in struct{ RepoPath string `json:"repo_path"` }
	if err := json.Unmarshal(args, &in); err != nil {
		return fmt.Sprintf("parse error: %v", err), true
	}
	gn := NewGitNexus(in.RepoPath)
	gf := NewGraphify(in.RepoPath)

	gnResult, gnErr := gn.Analyze()
	gfResult, gfErr := gf.Update(true)

	result := map[string]interface{}{
		"status":         "initialized",
		"repo_path":      in.RepoPath,
		"gitnexus":       safeMCPResult(gnResult),
		"gitnexus_error": errMCPString(gnErr),
		"graphify":       safeMCPResult(gfResult),
		"graphify_error": errMCPString(gfErr),
	}
	if gnErr != nil && gfErr != nil {
		result["status"] = "failed"
	} else if gnErr != nil || gfErr != nil {
		result["status"] = "partial"
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), false
}

func (s *MCPServerV2) toolUpdate(args json.RawMessage) (string, bool) {
	var in struct{ RepoPath string `json:"repo_path"` }
	if err := json.Unmarshal(args, &in); err != nil {
		return fmt.Sprintf("parse error: %v", err), true
	}
	gn := NewGitNexus(in.RepoPath)
	gf := NewGraphify(in.RepoPath)

	gnResult, gnErr := gn.Analyze()
	gfResult, gfErr := gf.Update(true)

	result := map[string]interface{}{
		"status":         "updated",
		"repo_path":      in.RepoPath,
		"gitnexus":       safeMCPResult(gnResult),
		"gitnexus_error": errMCPString(gnErr),
		"graphify":       safeMCPResult(gfResult),
		"graphify_error": errMCPString(gfErr),
	}
	if gnErr != nil && gfErr != nil {
		result["status"] = "failed"
	} else if gnErr != nil || gfErr != nil {
		result["status"] = "partial"
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), false
}

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
	gf := NewGraphify(in.RepoPath)
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
			"graphify_indexed": gf.IsIndexed(),
		}
	case "reindex":
		gnQr, gnErr := gn.Analyze()
		gfQr, gfErr := gf.Update(true)
		result = map[string]interface{}{
			"action":         "reindex",
			"gitnexus":       safeMCPResult(gnQr),
			"gitnexus_error": errMCPString(gnErr),
			"graphify":       safeMCPResult(gfQr),
			"graphify_error": errMCPString(gfErr),
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

func (s *MCPServerV2) writeResult(id interface{}, result interface{}) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	s.writeJSON(resp)
}

func (s *MCPServerV2) writeError(id interface{}, code int, message string, data interface{}) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mcpError{Code: code, Message: message, Data: data},
	}
	s.writeJSON(resp)
}

func (s *MCPServerV2) writeJSON(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Println(string(data))
}

func safeMCPResult(qr *QueryResult) interface{} {
	if qr == nil || qr.Results == nil {
		return nil
	}
	return qr.Results
}

func errMCPString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ============================================================================
// HTTP Handlers
// ============================================================================

func (s *MCPServerV2) handleHTTPInitialize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	result := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]string{
			"name":    "claude-go-codeintel",
			"version": "1.0.0",
		},
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *MCPServerV2) handleHTTPToolsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	tools := []mcpTool{
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
	writeJSON(w, http.StatusOK, map[string]interface{}{"tools": tools})
}

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

func (s *MCPServerV2) handleHTTPHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"repoPath": s.RepoPath,
		"version":  "1.0.0",
		"time":     time.Now(),
	})
}

// ============================================================================
// SSE Handlers
// ============================================================================

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

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
