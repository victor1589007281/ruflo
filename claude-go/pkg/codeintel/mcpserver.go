// mcpserver.go — MCP Server 实现（stdio JSON-RPC 2.0）。
//
// 职责: 对外暴露 codeintel 工具集，供 Claude Desktop / Cursor / Cline 使用。
// 协议: MCP (Model Context Protocol) stdio 传输
// 方法: initialize, tools/list, tools/call, notifications/initialized
//
// 启动方式:
//   claude-go codeintel-mcp-server --repo /path/to/repo
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
	"strings"
)

// ============================================================================
// MCP Server 结构
// ============================================================================

// MCPServer Code Intelligence MCP 服务器。
type MCPServer struct {
	RepoPath string
	Engine   *Engine
}

// NewMCPServer 创建 MCP 服务器。
func NewMCPServer(repoPath string) *MCPServer {
	return &MCPServer{
		RepoPath: repoPath,
		Engine:   NewEngine(repoPath),
	}
}

// Run 启动 stdio MCP 服务器（阻塞直到 stdin 关闭）。
func (s *MCPServer) Run() error {
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
		s.handleMessage(line)
	}
}

// ============================================================================
// JSON-RPC 2.0 消息处理
// ============================================================================


func (s *MCPServer) handleMessage(raw string) {
	var req mcpRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		s.writeError(nil, -32700, "Parse error", err.Error())
		return
	}

	// 通知（无 ID）不发送响应
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

func (s *MCPServer) handleInitialize(req mcpRequest) {
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

func (s *MCPServer) handleToolsList(req mcpRequest) {
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

func (s *MCPServer) handleToolsCall(req mcpRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(req.ID, -32602, "Invalid params", err.Error())
		return
	}

	var resultText string
	var isError bool

	switch params.Name {
	case "code_intel_init":
		resultText, isError = s.toolInit(params.Arguments)
	case "code_intel_update":
		resultText, isError = s.toolUpdate(params.Arguments)
	case "code_intel_status":
		resultText, isError = s.toolStatus(params.Arguments)
	case "code_intel_query":
		resultText, isError = s.toolQuery(params.Arguments)
	case "code_intel_branch":
		resultText, isError = s.toolBranch(params.Arguments)
	default:
		s.writeError(req.ID, -32601, "Tool not found", params.Name)
		return
	}

	result := mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: resultText}},
		IsError: isError,
	}
	s.writeResult(req.ID, result)
}

// ============================================================================
// Tool 实现（复用 Engine 逻辑）
// ============================================================================

func (s *MCPServer) toolInit(args json.RawMessage) (string, bool) {
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

func (s *MCPServer) toolUpdate(args json.RawMessage) (string, bool) {
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

func (s *MCPServer) toolStatus(args json.RawMessage) (string, bool) {
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

func (s *MCPServer) toolQuery(args json.RawMessage) (string, bool) {
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

func (s *MCPServer) toolBranch(args json.RawMessage) (string, bool) {
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

func (s *MCPServer) writeResult(id interface{}, result interface{}) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	s.writeJSON(resp)
}

func (s *MCPServer) writeError(id interface{}, code int, message string, data interface{}) {
	resp := mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mcpError{Code: code, Message: message, Data: data},
	}
	s.writeJSON(resp)
}

func (s *MCPServer) writeJSON(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Println(string(data))
}

