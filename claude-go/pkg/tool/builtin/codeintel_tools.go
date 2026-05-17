// codeintel_tools.go — 代码智能内置 Tool 套件。
//
// 将 pkg/codeintel 引擎能力包装为 5 个内置 tool，注册到 claude-go 工具列表。
// 与 MCP Server 共享同一核心逻辑（Engine/Builder/BranchManager）。
//
// 工具列表:
//   - code_intel_init     仓库初始化构建（非只读、非并发安全）
//   - code_intel_update   增量更新（非只读、非并发安全）
//   - code_intel_status   状态查询（只读、并发安全）
//   - code_intel_query    图谱查询（只读、并发安全）
//   - code_intel_branch   分支管理（非只读、非并发安全）
package builtin

import (
	"context"
	"encoding/json"
	"fmt"

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
	return `Initialize code intelligence index for a repository.
Supports auto-sharding (detect shards by top-level directories) or manual shard configuration.
Build pipeline is pure static analysis (zero LLM tokens): Tree-sitter AST + call graph + Leiden community detection.
This tool creates the index under ~/.claude-code-intel/<repo-hash>/.`
}

func (t *CodeIntelInitTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the git repository to index."},
			"auto_shard": {"type": "boolean", "description": "Auto-detect shards by top-level directories. Default true."},
			"shards": {"type": "array", "description": "Manual shard config (only if auto_shard=false).", "items": {"type": "object", "properties": {"name": {"type": "string"}, "root_dirs": {"type": "array", "items": {"type": "string"}}, "max_files": {"type": "integer"}}}}
		},
		"required": ["repo_path"]
	}`)
}

func (t *CodeIntelInitTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		RepoPath   string                `json:"repo_path"`
		AutoShard  bool                  `json:"auto_shard"`
		Shards     []codeintel.ShardConfig `json:"shards,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("parse error: %v", err), IsError: true}, nil
	}

	store := codeintel.NewStore(in.RepoPath)
	if err := store.EnsureDirs("main"); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("ensure dirs error: %v", err), IsError: true}, nil
	}

	var shards []codeintel.ShardConfig
	if in.AutoShard {
		auto, err := codeintel.DetectShards(in.RepoPath, nil)
		if err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("auto-shard error: %v", err), IsError: true}, nil
		}
		shards = auto
	} else if len(in.Shards) > 0 {
		shards = in.Shards
	} else {
		shards = []codeintel.ShardConfig{{Name: "default", RootDirs: []string{"."}}}
	}

	cfg := &codeintel.RepoConfig{
		RepoPath:  in.RepoPath,
		RepoHash:  codeintel.HashRepoPath(in.RepoPath),
		Shards:    shards,
		AutoShard: in.AutoShard,
	}
	if err := store.SaveConfig(cfg); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("save config error: %v", err), IsError: true}, nil
	}

	builder, err := codeintel.NewBuilder(in.RepoPath)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("builder init error: %v", err), IsError: true}, nil
	}
	idx, err := builder.BuildAll("main", nil)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("build error: %v", err), IsError: true}, nil
	}

	result := map[string]interface{}{
		"status":        "initialized",
		"repo_path":     in.RepoPath,
		"shard_count":   len(cfg.Shards),
		"branch":        "main",
		"files_indexed": 0,
	}
	for _, si := range idx.Shards {
		result["files_indexed"] = result["files_indexed"].(int) + si.FileCount
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
	return `Incrementally update the code intelligence index based on git changes.
Only affected shards are rebuilt. Zero LLM tokens consumed.`
}

func (t *CodeIntelUpdateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the indexed repository."},
			"branch": {"type": "string", "description": "Branch to update. Default: main."}
		}
	}`)
}

func (t *CodeIntelUpdateTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		RepoPath string `json:"repo_path"`
		Branch   string `json:"branch,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("parse error: %v", err), IsError: true}, nil
	}
	if in.Branch == "" {
		in.Branch = "main"
	}
	builder, err := codeintel.NewBuilder(in.RepoPath)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("builder init error: %v", err), IsError: true}, nil
	}
	idx, err := builder.IncrementalUpdate(in.Branch, nil)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("update error: %v", err), IsError: true}, nil
	}
	result := map[string]interface{}{
		"status":      "updated",
		"branch":      in.Branch,
		"shard_count": len(idx.Shards),
		"updated_at":  idx.UpdatedAt,
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
	return `Get the status of code intelligence index: shards, branches, last update time, and indexed file counts.`
}

func (t *CodeIntelStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the indexed repository."},
			"branch": {"type": "string", "description": "Branch to check. Default: main."}
		}
	}`)
}

func (t *CodeIntelStatusTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		RepoPath string `json:"repo_path"`
		Branch   string `json:"branch,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("parse error: %v", err), IsError: true}, nil
	}
	if in.Branch == "" {
		in.Branch = "main"
	}
	engine := codeintel.NewEngine(in.RepoPath)
	qr, err := engine.Status(in.Branch)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("status error: %v", err), IsError: true}, nil
	}
	out, _ := json.MarshalIndent(qr.Results, "", "  ")
	return &tool.ToolResult{Content: string(out)}, nil
}

// ============================================================================
// CodeIntelQueryTool — 图谱查询
// ============================================================================

type CodeIntelQueryTool struct{}

func NewCodeIntelQueryTool() *CodeIntelQueryTool { return &CodeIntelQueryTool{} }

func (t *CodeIntelQueryTool) Name() string        { return "code_intel_query" }
func (t *CodeIntelQueryTool) IsReadOnly(_ json.RawMessage) bool         { return true }
func (t *CodeIntelQueryTool) IsConcurrencySafe(_ json.RawMessage) bool  { return true }
func (t *CodeIntelQueryTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult { return nil }

func (t *CodeIntelQueryTool) Description() string {
	return `Query the code intelligence graph. Supports structural and semantic queries:
- navigate: symbol definition + callers + callees
- impact: dependency radius of a file
- communities: Leiden communities + god nodes in a shard
- god_nodes: highest-degree nodes in a shard
- cross_shard: cross-shard references of a symbol
	- find_refs: all reference locations of a symbol
	- path: shortest path between two symbols
	- surprises: anomalous edges (cross-community / high-weight)
	- native_grep: real-time grep fallback
	- native_read: real-time file read fallback`
}

func (t *CodeIntelQueryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the indexed repository."},
			"query_type": {"type": "string", "enum": ["navigate", "impact", "find_refs", "communities", "god_nodes", "path", "surprises", "cross_shard", "native_grep", "native_read"]},
			"shard": {"type": "string"},
			"symbol": {"type": "string"},
			"file_path": {"type": "string"},
			"depth": {"type": "integer"},
			"top_n": {"type": "integer"},
			"target_shard": {"type": "string"},
			"branch": {"type": "string", "description": "Default: main."}
		},
		"required": ["repo_path", "query_type"]
	}`)
}

func (t *CodeIntelQueryTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		RepoPath    string `json:"repo_path"`
		QueryType   string `json:"query_type"`
		Shard       string `json:"shard,omitempty"`
		Symbol      string `json:"symbol,omitempty"`
		FilePath    string `json:"file_path,omitempty"`
		Depth       int    `json:"depth,omitempty"`
		TopN        int    `json:"top_n,omitempty"`
		TargetShard  string `json:"target_shard,omitempty"`
		TargetSymbol string `json:"target_symbol,omitempty"`
		Branch       string `json:"branch,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("parse error: %v", err), IsError: true}, nil
	}
	if in.Branch == "" {
		in.Branch = "main"
	}

	engine := codeintel.NewEngine(in.RepoPath)
	var qr *codeintel.QueryResult
	var err error

	switch in.QueryType {
	case "navigate":
		qr, err = engine.Navigate(in.Branch, codeintel.NavigateQuery{Symbol: in.Symbol, Depth: in.Depth, Shard: in.Shard})
	case "impact":
		qr, err = engine.Impact(in.Branch, codeintel.ImpactQuery{FilePath: in.FilePath, Depth: in.Depth})
	case "communities":
		qr, err = engine.Communities(in.Branch, codeintel.CommunityQuery{Shard: in.Shard, TopN: in.TopN})
	case "god_nodes":
		qr, err = engine.GodNodes(in.Branch, in.Shard, in.TopN)
	case "cross_shard":
		qr, err = engine.CrossShard(in.Branch, codeintel.CrossShardQuery{Symbol: in.Symbol, TargetShard: in.TargetShard})
	case "find_refs":
		qr, err = engine.FindRefs(in.Branch, codeintel.NavigateQuery{Symbol: in.Symbol, Shard: in.Shard})
	case "path":
		qr, err = engine.Path(in.Branch, in.Symbol, in.TargetSymbol)
	case "surprises":
		qr, err = engine.Surprises(in.Branch, in.Shard, in.TopN)
	case "native_grep":
		qr, err = engine.Native.Grep(in.Symbol, codeintel.GrepOptions{Dir: in.FilePath, MaxResults: in.TopN})
	case "native_read":
		qr, err = engine.Native.ReadFile(in.FilePath, codeintel.ReadOptions{Offset: in.Depth, Limit: in.TopN})
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
// CodeIntelBranchTool — 分支管理
// ============================================================================

type CodeIntelBranchTool struct{}

func NewCodeIntelBranchTool() *CodeIntelBranchTool { return &CodeIntelBranchTool{} }

func (t *CodeIntelBranchTool) Name() string        { return "code_intel_branch" }
func (t *CodeIntelBranchTool) IsReadOnly(_ json.RawMessage) bool         { return false }
func (t *CodeIntelBranchTool) IsConcurrencySafe(_ json.RawMessage) bool  { return false }
func (t *CodeIntelBranchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult { return nil }

func (t *CodeIntelBranchTool) Description() string {
	return `Manage code intelligence branches (index snapshots per git branch).
Actions: switch (activate), create (CoW from base), delete, list.
Branch isolation uses Copy-on-Write metadata with shared read-only index data.`
}

func (t *CodeIntelBranchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the indexed repository."},
			"action": {"type": "string", "enum": ["switch", "create", "delete", "list"]},
			"branch_name": {"type": "string"},
			"base_branch": {"type": "string", "description": "Base branch for create action. Default: main."}
		},
		"required": ["repo_path", "action"]
	}`)
}

func (t *CodeIntelBranchTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		RepoPath   string `json:"repo_path"`
		Action     string `json:"action"`
		BranchName string `json:"branch_name,omitempty"`
		BaseBranch string `json:"base_branch,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("parse error: %v", err), IsError: true}, nil
	}

	bm := codeintel.NewBranchManager(in.RepoPath)
	var result string
	var err error

	switch in.Action {
	case "switch":
		err = bm.SwitchBranch(in.BranchName)
		result = fmt.Sprintf("Switched to branch: %s", in.BranchName)
	case "create":
		base := in.BaseBranch
		if base == "" {
			base = "main"
		}
		err = bm.CreateBranch(base, in.BranchName)
		result = fmt.Sprintf("Created branch: %s (from %s)", in.BranchName, base)
	case "delete":
		err = bm.DeleteBranch(in.BranchName)
		result = fmt.Sprintf("Deleted branch: %s", in.BranchName)
	case "list":
		var branches []string
		branches, err = bm.ListBranches()
		if err == nil {
			out, _ := json.MarshalIndent(map[string]interface{}{"branches": branches}, "", "  ")
			result = string(out)
		}
	default:
		return &tool.ToolResult{Content: fmt.Sprintf("unknown action: %s", in.Action), IsError: true}, nil
	}

	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("branch error: %v", err), IsError: true}, nil
	}
	return &tool.ToolResult{Content: result}, nil
}
