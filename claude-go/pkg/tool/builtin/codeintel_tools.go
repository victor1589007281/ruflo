// codeintel_tools.go — 代码智能内置 Tool 套件（GitNexus + Graphify CLI 包装）。
//
// Go 仅做编排：所有智能工作交给外部工具。
//   - GitNexus (Node.js): 结构查询、影响分析、符号导航
//   - Graphify (Python): 语义查询、社区检测、路径分析
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/anthropic/claude-go/pkg/codeintel"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// CodeIntelInitTool — 仓库初始化构建
// ============================================================================

type CodeIntelInitTool struct{}

func NewCodeIntelInitTool() *CodeIntelInitTool { return &CodeIntelInitTool{} }

func (t *CodeIntelInitTool) Name() string        { return "code_intel_init" }
func (t *CodeIntelInitTool) IsReadOnly(_ json.RawMessage) bool         { return false }
func (t *CodeIntelInitTool) IsConcurrencySafe(_ json.RawMessage) bool  { return false }
func (t *CodeIntelInitTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult { return nil }

func (t *CodeIntelInitTool) Description() string {
	return `Initialize code intelligence for a repository by running GitNexus analyze and Graphify update.
Requires GitNexus (npm install -g gitnexus) and Graphify (pip3 install graphifyy) to be installed locally.
This tool delegates all work to external CLI tools — zero LLM tokens consumed during indexing.`
}

func (t *CodeIntelInitTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the git repository to index."}
		},
		"required": ["repo_path"]
	}`)
}

func (t *CodeIntelInitTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		RepoPath string `json:"repo_path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("parse error: %v", err), IsError: true}, nil
	}

	// 统一索引操作(gitnexus analyze + graphify update, 内部并行)
	gn := codeintel.NewGitNexus(in.RepoPath)
	gf := codeintel.NewGraphify(in.RepoPath)
	o := codeintel.RunIndex(gn, gf)

	result := map[string]interface{}{
		"status":         o.StatusOr("initialized"),
		"repo_path":      in.RepoPath,
		"gitnexus":       safeMap(o.GitNexus),
		"gitnexus_error": errString(o.GNErr),
		"graphify":       safeMap(o.Graphify),
		"graphify_error": errString(o.GFErr),
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	return &tool.ToolResult{Content: string(out)}, nil
}

// ============================================================================
// CodeIntelUpdateTool — 增量更新
// ============================================================================

type CodeIntelUpdateTool struct{}

func NewCodeIntelUpdateTool() *CodeIntelUpdateTool { return &CodeIntelUpdateTool{} }

func (t *CodeIntelUpdateTool) Name() string        { return "code_intel_update" }
func (t *CodeIntelUpdateTool) IsReadOnly(_ json.RawMessage) bool         { return false }
func (t *CodeIntelUpdateTool) IsConcurrencySafe(_ json.RawMessage) bool  { return false }
func (t *CodeIntelUpdateTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult { return nil }

func (t *CodeIntelUpdateTool) Description() string {
	return `Incrementally update the code intelligence index by re-running GitNexus analyze and Graphify update.
Zero LLM tokens consumed.`
}

func (t *CodeIntelUpdateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the indexed repository."}
		},
		"required": ["repo_path"]
	}`)
}

func (t *CodeIntelUpdateTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		RepoPath string `json:"repo_path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("parse error: %v", err), IsError: true}, nil
	}

	// 统一索引操作(gitnexus analyze + graphify update, 内部并行)
	gn := codeintel.NewGitNexus(in.RepoPath)
	gf := codeintel.NewGraphify(in.RepoPath)
	o := codeintel.RunIndex(gn, gf)

	result := map[string]interface{}{
		"status":         o.StatusOr("updated"),
		"repo_path":      in.RepoPath,
		"gitnexus":       safeMap(o.GitNexus),
		"gitnexus_error": errString(o.GNErr),
		"graphify":       safeMap(o.Graphify),
		"graphify_error": errString(o.GFErr),
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	return &tool.ToolResult{Content: string(out)}, nil
}

// ============================================================================
// CodeIntelStatusTool — 状态查询
// ============================================================================

type CodeIntelStatusTool struct{}

func NewCodeIntelStatusTool() *CodeIntelStatusTool { return &CodeIntelStatusTool{} }

func (t *CodeIntelStatusTool) Name() string        { return "code_intel_status" }
func (t *CodeIntelStatusTool) IsReadOnly(_ json.RawMessage) bool         { return true }
func (t *CodeIntelStatusTool) IsConcurrencySafe(_ json.RawMessage) bool  { return true }
func (t *CodeIntelStatusTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult { return nil }

func (t *CodeIntelStatusTool) Description() string {
	return `Get the status of code intelligence index from GitNexus and Graphify.`
}

func (t *CodeIntelStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the indexed repository."}
		},
		"required": ["repo_path"]
	}`)
}

func (t *CodeIntelStatusTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		RepoPath string `json:"repo_path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("parse error: %v", err), IsError: true}, nil
	}

	engine := codeintel.NewEngine(in.RepoPath)
	qr, err := engine.Status("main")
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("status error: %v", err), IsError: true}, nil
	}
	out, _ := json.MarshalIndent(qr.Results, "", "  ")
	return &tool.ToolResult{Content: string(out)}, nil
}

// ============================================================================
// CodeIntelQueryTool — 查询
// ============================================================================

type CodeIntelQueryTool struct{}

func NewCodeIntelQueryTool() *CodeIntelQueryTool { return &CodeIntelQueryTool{} }

func (t *CodeIntelQueryTool) Name() string        { return "code_intel_query" }
func (t *CodeIntelQueryTool) IsReadOnly(_ json.RawMessage) bool         { return true }
func (t *CodeIntelQueryTool) IsConcurrencySafe(_ json.RawMessage) bool  { return true }
func (t *CodeIntelQueryTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult { return nil }

func (t *CodeIntelQueryTool) Description() string {
	return `Query the code intelligence graph via GitNexus or Graphify CLI.
Query types:
- navigate / find_refs / cross_shard / impact → GitNexus structural queries
- path / explain / communities / god_nodes / surprises → Graphify semantic queries
- query (generic) → routed to both engines`
}

func (t *CodeIntelQueryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the indexed repository."},
			"query_type": {"type": "string", "enum": ["navigate", "impact", "find_refs", "communities", "god_nodes", "path", "surprises", "cross_shard", "query", "explain"], "description": "Query type."},
			"shard": {"type": "string"},
			"symbol": {"type": "string"},
			"file_path": {"type": "string"},
			"depth": {"type": "integer"},
			"top_n": {"type": "integer"},
			"target_shard": {"type": "string"},
			"target_symbol": {"type": "string"}
		},
		"required": ["repo_path", "query_type"]
	}`)
}

func (t *CodeIntelQueryTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
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
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("parse error: %v", err), IsError: true}, nil
	}

	engine := codeintel.NewEngine(in.RepoPath)
	var qr *codeintel.QueryResult
	var err error

	switch in.QueryType {
	case "navigate":
		qr, err = engine.Navigate("main", codeintel.NavigateQuery{Symbol: in.Symbol, Depth: in.Depth, Shard: in.Shard})
	case "impact":
		qr, err = engine.Impact("main", codeintel.ImpactQuery{FilePath: in.FilePath, Depth: in.Depth})
	case "find_refs":
		qr, err = engine.FindRefs("main", codeintel.NavigateQuery{Symbol: in.Symbol, Shard: in.Shard})
	case "communities":
		qr, err = engine.Communities("main", codeintel.CommunityQuery{Shard: in.Shard, TopN: in.TopN})
	case "god_nodes":
		qr, err = engine.GodNodes("main", in.Shard, in.TopN)
	case "path":
		qr, err = engine.Path("main", in.Symbol, in.TargetSymbol)
	case "surprises":
		qr, err = engine.Surprises("main", in.Shard, in.TopN)
	case "cross_shard":
		qr, err = engine.CrossShard("main", codeintel.CrossShardQuery{Symbol: in.Symbol, TargetShard: in.TargetShard})
	case "query":
		// 通用查询：并行发给 GitNexus query 和 Graphify query，合并结果
		gn := codeintel.NewGitNexus(in.RepoPath)
		gf := codeintel.NewGraphify(in.RepoPath)
		var gnQr, gfQr *codeintel.QueryResult
		var gnErr, gfErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			gnQr, gnErr = gn.Query(in.Symbol)
		}()
		go func() {
			defer wg.Done()
			gfQr, gfErr = gf.Query(in.Symbol)
		}()
		wg.Wait()
		result := map[string]interface{}{
			"gitnexus":       safeMap(gnQr),
			"gitnexus_error": errString(gnErr),
			"graphify":       safeMap(gfQr),
			"graphify_error": errString(gfErr),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		return &tool.ToolResult{Content: string(out)}, nil
	case "explain":
		gf := codeintel.NewGraphify(in.RepoPath)
		qr, err = gf.Explain(in.Symbol)
	default:
		return &tool.ToolResult{Content: fmt.Sprintf("unknown query_type: %s", in.QueryType), IsError: true}, nil
	}

	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("query error: %v", err), IsError: true}, nil
	}
	out, _ := json.MarshalIndent(qr.Results, "", "  ")
	return &tool.ToolResult{Content: string(out)}, nil
}

// ============================================================================
// CodeIntelBranchTool — 分支管理（简化版）
// ============================================================================

type CodeIntelBranchTool struct{}

func NewCodeIntelBranchTool() *CodeIntelBranchTool { return &CodeIntelBranchTool{} }

func (t *CodeIntelBranchTool) Name() string        { return "code_intel_branch" }
func (t *CodeIntelBranchTool) IsReadOnly(_ json.RawMessage) bool         { return false }
func (t *CodeIntelBranchTool) IsConcurrencySafe(_ json.RawMessage) bool  { return false }
func (t *CodeIntelBranchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult { return nil }

func (t *CodeIntelBranchTool) Description() string {
	return `Branch management for code intelligence.
GitNexus and Graphify do not maintain per-branch isolated indexes by default.
This tool provides: switch (git checkout), detect-changes, and status.`
}

func (t *CodeIntelBranchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the indexed repository."},
			"action": {"type": "string", "enum": ["switch", "detect_changes", "status", "reindex"]},
			"branch_name": {"type": "string"}
		},
		"required": ["repo_path", "action"]
	}`)
}

func (t *CodeIntelBranchTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		RepoPath   string `json:"repo_path"`
		Action     string `json:"action"`
		BranchName string `json:"branch_name,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("parse error: %v", err), IsError: true}, nil
	}

	gn := codeintel.NewGitNexus(in.RepoPath)
	gf := codeintel.NewGraphify(in.RepoPath)
	var result map[string]interface{}
	var err error

	switch in.Action {
	case "switch":
		if in.BranchName == "" {
			return &tool.ToolResult{Content: "branch_name required for switch", IsError: true}, nil
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
			"action":           "detect_changes",
			"gitnexus":         safeMap(qr),
			"gitnexus_error":   errString(e),
		}
	case "status":
		gnQr, gnErr := gn.Status()
		result = map[string]interface{}{
			"action":           "status",
			"gitnexus":         safeMap(gnQr),
			"gitnexus_error":   errString(gnErr),
			"gitnexus_indexed": gn.IsIndexed(),
			"graphify_indexed": gf.IsIndexed(),
		}
	case "reindex":
		o := codeintel.RunIndex(gn, gf)
		result = map[string]interface{}{
			"action":         "reindex",
			"gitnexus":       safeMap(o.GitNexus),
			"gitnexus_error": errString(o.GNErr),
			"graphify":       safeMap(o.Graphify),
			"graphify_error": errString(o.GFErr),
		}
	default:
		return &tool.ToolResult{Content: fmt.Sprintf("unknown action: %s", in.Action), IsError: true}, nil
	}

	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("branch error: %v", err), IsError: true}, nil
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return &tool.ToolResult{Content: string(out)}, nil
}

// ============================================================================
// 辅助函数
// ============================================================================

func safeMap(qr *codeintel.QueryResult) interface{} {
	if qr == nil {
		return nil
	}
	return qr.Results
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
