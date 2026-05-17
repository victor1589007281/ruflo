// mcpserver.go — MCP Server 实现（stdio 传输）。
//
// 将 codeintel 能力通过 MCP 协议暴露给外部消费者（Claude Desktop / Cursor / Cline）。
// 传输层：JSON-RPC 2.0 over stdio。
// 复用内置 tool 的核心逻辑，仅包装为 MCP 工具格式。
//
// 启动方式:
//   claude-go codeintel-mcp-server [--repo <path>]
//   或独立二进制: codeintel-mcp-server
//
// Claude Desktop 配置:
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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// MCPServer MCP 服务器。
type MCPServer struct {
	RepoPath   string
	Tools      []mcpToolDef
	Engine     *Engine
	Builder    *Builder
	BranchMgr  *BranchManager
	stdin      *bufio.Scanner
	stdout     io.Writer
	nextID     int64
}

type mcpToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// NewMCPServer 创建 MCP 服务器。
func NewMCPServer(repoPath string) *MCPServer {
	return &MCPServer{
		RepoPath:  repoPath,
		Engine:    NewEngine(repoPath),
		BranchMgr: NewBranchManager(repoPath),
		stdout:    os.Stdout,
	}
}

// SetBuilder 设置构建器（需要先有配置才能创建）。
func (s *MCPServer) SetBuilder(b *Builder) {
	s.Builder = b
}

// Run 启动 MCP 服务器事件循环（stdio）。
func (s *MCPServer) Run() error {
	s.stdin = bufio.NewScanner(os.Stdin)
	s.stdin.Buffer(make([]byte, 1024*1024), 1024*1024)

	for s.stdin.Scan() {
		line := s.stdin.Bytes()
		if len(line) == 0 {
			continue
		}
		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.sendError(0, -32700, "Parse error")
			continue
		}
		s.handleRequest(req)
	}
	return s.stdin.Err()
}

// ============================================================================
// JSON-RPC 请求处理
// ============================================================================

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *jsonrpcErr `json:"error,omitempty"`
}

type jsonrpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *MCPServer) handleRequest(req jsonrpcRequest) {
	switch req.Method {
	case "initialize":
		s.sendResult(req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"serverInfo": map[string]string{
				"name":    "codeintel-mcp",
				"version": "1.0.0",
			},
		})
	case "initialized", "notifications/initialized":
		// no-op
	case "tools/list":
		s.sendResult(req.ID, map[string]interface{}{"tools": s.listTools()})
	case "tools/call":
		s.handleToolCall(req.ID, req.Params)
	case "resources/list":
		s.sendResult(req.ID, map[string]interface{}{"resources": []interface{}{}})
	default:
		s.sendError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func (s *MCPServer) handleToolCall(id interface{}, params json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		s.sendError(id, -32602, "Invalid params")
		return
	}

	result, err := s.executeTool(p.Name, p.Arguments)
	if err != nil {
		s.sendResult(id, map[string]interface{}{
			"content": []map[string]string{{"type": "text", "text": err.Error()}},
			"isError": true,
		})
		return
	}
	s.sendResult(id, map[string]interface{}{
		"content": []map[string]string{{"type": "text", "text": result}},
	})
}

func (s *MCPServer) executeTool(name string, args json.RawMessage) (string, error) {
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
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// ============================================================================
// 工具实现（复用 Engine/Builder/BranchMgr）
// ============================================================================

func (s *MCPServer) toolInit(args json.RawMessage) (string, error) {
	var in struct {
		RepoPath string        `json:"repo_path"`
		Shards   []ShardConfig `json:"shards,omitempty"`
		AutoShard bool         `json:"auto_shard,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	repoPath := in.RepoPath
	if repoPath == "" {
		repoPath = s.RepoPath
	}
	if repoPath == "" {
		return "", fmt.Errorf("repo_path required")
	}

	store := NewStore(repoPath)
	if err := store.EnsureDirs("main"); err != nil {
		return "", err
	}

	var shards []ShardConfig
	if in.AutoShard {
		auto, err := DetectShards(repoPath, nil)
		if err != nil {
			return "", err
		}
		shards = auto
	} else if len(in.Shards) > 0 {
		shards = in.Shards
	} else {
		// 默认单分片
		shards = []ShardConfig{{Name: "default", RootDirs: []string{"."}}}
	}

	cfg := &RepoConfig{
		RepoPath:  repoPath,
		RepoHash:  HashRepoPath(repoPath),
		Shards:    shards,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		AutoShard: in.AutoShard,
	}
	if err := store.SaveConfig(cfg); err != nil {
		return "", err
	}

	builder, err := NewBuilder(repoPath)
	if err != nil {
		return "", err
	}
	idx, err := builder.BuildAll("main", nil)
	if err != nil {
		return "", err
	}

	result := map[string]interface{}{
		"status":       "initialized",
		"repo_path":    repoPath,
		"shard_count":  len(cfg.Shards),
		"branch":       "main",
		"files_indexed": 0,
	}
	for _, si := range idx.Shards {
		result["files_indexed"] = result["files_indexed"].(int) + si.FileCount
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

func (s *MCPServer) toolUpdate(args json.RawMessage) (string, error) {
	var in struct {
		Branch string `json:"branch,omitempty"`
	}
	_ = json.Unmarshal(args, &in)
	branch := in.Branch
	if branch == "" {
		branch = "main"
	}

	builder, err := NewBuilder(s.RepoPath)
	if err != nil {
		return "", err
	}
	idx, err := builder.IncrementalUpdate(branch, nil)
	if err != nil {
		return "", err
	}

	result := map[string]interface{}{
		"status":      "updated",
		"branch":      branch,
		"shard_count": len(idx.Shards),
		"updated_at":  idx.UpdatedAt,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

func (s *MCPServer) toolStatus(args json.RawMessage) (string, error) {
	var in struct {
		Branch string `json:"branch,omitempty"`
	}
	_ = json.Unmarshal(args, &in)
	branch := in.Branch
	if branch == "" {
		branch = "main"
	}

	qr, err := s.Engine.Status(branch)
	if err != nil {
		return "", err
	}
	out, _ := json.MarshalIndent(qr.Results, "", "  ")
	return string(out), nil
}

func (s *MCPServer) toolQuery(args json.RawMessage) (string, error) {
	var in struct {
		QueryType   string          `json:"query_type"`
		Shard       string          `json:"shard,omitempty"`
		Symbol      string          `json:"symbol,omitempty"`
		FilePath    string          `json:"file_path,omitempty"`
		Depth       int             `json:"depth,omitempty"`
		TopN        int             `json:"top_n,omitempty"`
		TargetShard  string          `json:"target_shard,omitempty"`
		TargetSymbol string          `json:"target_symbol,omitempty"`
		Branch       string          `json:"branch,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	branch := in.Branch
	if branch == "" {
		branch = "main"
	}

	var qr *QueryResult
	var err error
	switch in.QueryType {
	case "navigate":
		qr, err = s.Engine.Navigate(branch, NavigateQuery{Symbol: in.Symbol, Depth: in.Depth, Shard: in.Shard})
	case "impact":
		qr, err = s.Engine.Impact(branch, ImpactQuery{FilePath: in.FilePath, Depth: in.Depth})
	case "communities":
		qr, err = s.Engine.Communities(branch, CommunityQuery{Shard: in.Shard, TopN: in.TopN})
	case "god_nodes":
		qr, err = s.Engine.GodNodes(branch, in.Shard, in.TopN)
	case "cross_shard":
		qr, err = s.Engine.CrossShard(branch, CrossShardQuery{Symbol: in.Symbol, TargetShard: in.TargetShard})
	case "find_refs":
		qr, err = s.Engine.FindRefs(branch, NavigateQuery{Symbol: in.Symbol, Shard: in.Shard})
	case "path":
		qr, err = s.Engine.Path(branch, in.Symbol, in.TargetSymbol)
	case "surprises":
		qr, err = s.Engine.Surprises(branch, in.Shard, in.TopN)
	case "native_grep":
		qr, err = s.Engine.Native.Grep(in.Symbol, GrepOptions{Dir: in.FilePath, MaxResults: in.TopN})
	case "native_read":
		qr, err = s.Engine.Native.ReadFile(in.FilePath, ReadOptions{Offset: in.Depth, Limit: in.TopN})
	default:
		return "", fmt.Errorf("unknown query_type: %s", in.QueryType)
	}
	if err != nil {
		return "", err
	}
	out, _ := json.MarshalIndent(qr.Results, "", "  ")
	return string(out), nil
}

func (s *MCPServer) toolBranch(args json.RawMessage) (string, error) {
	var in struct {
		Action     string `json:"action"`       // switch, create, delete, list
		BranchName string `json:"branch_name,omitempty"`
		BaseBranch string `json:"base_branch,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}

	switch in.Action {
	case "switch":
		if err := s.BranchMgr.SwitchBranch(in.BranchName); err != nil {
			return "", err
		}
		return fmt.Sprintf("Switched to branch: %s", in.BranchName), nil
	case "create":
		base := in.BaseBranch
		if base == "" {
			base = "main"
		}
		if err := s.BranchMgr.CreateBranch(base, in.BranchName); err != nil {
			return "", err
		}
		return fmt.Sprintf("Created branch: %s (from %s)", in.BranchName, base), nil
	case "delete":
		if err := s.BranchMgr.DeleteBranch(in.BranchName); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted branch: %s", in.BranchName), nil
	case "list":
		branches, err := s.BranchMgr.ListBranches()
		if err != nil {
			return "", err
		}
		out, _ := json.MarshalIndent(map[string]interface{}{"branches": branches}, "", "  ")
		return string(out), nil
	default:
		return "", fmt.Errorf("unknown action: %s", in.Action)
	}
}

// ============================================================================
// 工具定义
// ============================================================================

func (s *MCPServer) listTools() []mcpToolDef {
	return []mcpToolDef{
		{
			Name:        "code_intel_init",
			Description: "Initialize code intelligence index for a repository. Supports auto-sharding or manual shard configuration.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string","description":"Absolute path to the git repository"},"auto_shard":{"type":"boolean","description":"Auto-detect shards by top-level directories"},"shards":{"type":"array","description":"Manual shard configuration","items":{"type":"object","properties":{"name":{"type":"string"},"root_dirs":{"type":"array","items":{"type":"string"}},"max_files":{"type":"integer"}}}},"required":["repo_path"]}`),
		},
		{
			Name:        "code_intel_update",
			Description: "Incrementally update the code intelligence index based on git changes.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"branch":{"type":"string","description":"Branch to update (default: main)"}}}`),
		},
		{
			Name:        "code_intel_status",
			Description: "Get the status of code intelligence index: shards, branches, last update.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"branch":{"type":"string","description":"Branch to check (default: main)"}}}`),
		},
		{
			Name:        "code_intel_query",
			Description: "Query the code intelligence graph. Supports: navigate, impact, find_refs, communities, god_nodes, path, surprises, cross_shard, native_grep, native_read.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query_type":{"type":"string","enum":["navigate","impact","find_refs","communities","god_nodes","path","surprises","cross_shard","native_grep","native_read"]},"shard":{"type":"string"},"symbol":{"type":"string"},"file_path":{"type":"string"},"depth":{"type":"integer"},"top_n":{"type":"integer"},"target_shard":{"type":"string"},"target_symbol":{"type":"string"},"branch":{"type":"string"}},"required":["query_type"]}`),
		},
		{
			Name:        "code_intel_branch",
			Description: "Manage code intelligence branches: switch, create, delete, list.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["switch","create","delete","list"]},"branch_name":{"type":"string"},"base_branch":{"type":"string"}},"required":["action"]}`),
		},
	}
}

// ============================================================================
// JSON-RPC IO
// ============================================================================

func (s *MCPServer) sendResult(id interface{}, result interface{}) {
	resp := jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	s.writeJSON(resp)
}

func (s *MCPServer) sendError(id interface{}, code int, message string) {
	resp := jsonrpcResponse{JSONRPC: "2.0", ID: id, Error: &jsonrpcErr{Code: code, Message: message}}
	s.writeJSON(resp)
}

func (s *MCPServer) writeJSON(v interface{}) {
	data, _ := json.Marshal(v)
	fmt.Fprintln(s.stdout, string(data))
}
