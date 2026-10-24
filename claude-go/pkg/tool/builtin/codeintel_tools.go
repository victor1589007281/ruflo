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

// NewCodeIntelInitTool 构造仓库索引初始化工具。
func NewCodeIntelInitTool() *CodeIntelInitTool { return &CodeIntelInitTool{} }

// Name 返回工具名 "code_intel_init"。
func (t *CodeIntelInitTool) Name() string                             { return "code_intel_init" }

// IsReadOnly 建索引会写 .gitnexus / graphify 产物, 故非只读。
func (t *CodeIntelInitTool) IsReadOnly(_ json.RawMessage) bool        { return false }

// IsConcurrencySafe 全量建索引会写同一份索引目录, 并发调用不安全。
func (t *CodeIntelInitTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// CheckPermissions 工具自身不设门槛, 权限交由上层策略裁决。
func (t *CodeIntelInitTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Description 返回给模型看的英文说明 (索引全程走外部 CLI, 零 LLM token)。
func (t *CodeIntelInitTool) Description() string {
	return `Initialize code intelligence for a repository by running GitNexus analyze and Graphify update.
Requires GitNexus (npm install -g gitnexus) and Graphify (pip3 install graphifyy) to be installed locally.
This tool delegates all work to external CLI tools — zero LLM tokens consumed during indexing.`
}

// InputSchema 声明入参: 仅一个必填的 repo_path (绝对路径)。
func (t *CodeIntelInitTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the git repository to index."}
		},
		"required": ["repo_path"]
	}`)
}

// Call 解析 repo_path 后执行一次全量索引 (GitNexus analyze + Graphify update), 回显两侧结果与错误。
//
// 两个引擎的失败**不互相阻断**: 各自的结果与 error 分别落在 gitnexus_error / graphify_error,
// 所以单边可用时工具仍然返回成功 —— 索引是"尽力而为"的增强, 不是硬依赖。
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

// NewCodeIntelUpdateTool 构造增量更新工具。
func NewCodeIntelUpdateTool() *CodeIntelUpdateTool { return &CodeIntelUpdateTool{} }

// Name 返回工具名 "code_intel_update"。
func (t *CodeIntelUpdateTool) Name() string                             { return "code_intel_update" }

// IsReadOnly 更新会重写索引产物, 故非只读。
func (t *CodeIntelUpdateTool) IsReadOnly(_ json.RawMessage) bool        { return false }

// IsConcurrencySafe 与 init 共用索引目录, 并发不安全。
func (t *CodeIntelUpdateTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// CheckPermissions 工具自身不设门槛, 权限交由上层策略裁决。
func (t *CodeIntelUpdateTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Description 返回给模型看的英文说明 (增量重建, 零 LLM token)。
func (t *CodeIntelUpdateTool) Description() string {
	return `Incrementally update the code intelligence index by re-running GitNexus analyze and Graphify update.
Zero LLM tokens consumed.`
}

// InputSchema 声明入参: 仅一个必填的 repo_path。
func (t *CodeIntelUpdateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the indexed repository."}
		},
		"required": ["repo_path"]
	}`)
}

// Call 解析 repo_path 后重跑一次索引, 语义与 init 相同 (区别只在返回的默认 status 文案)。
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

// NewCodeIntelStatusTool 构造索引状态查询工具。
func NewCodeIntelStatusTool() *CodeIntelStatusTool { return &CodeIntelStatusTool{} }

// Name 返回工具名 "code_intel_status"。
func (t *CodeIntelStatusTool) Name() string                             { return "code_intel_status" }

// IsReadOnly 只读索引状态, 不落盘。
func (t *CodeIntelStatusTool) IsReadOnly(_ json.RawMessage) bool        { return true }

// IsConcurrencySafe 纯查询, 无共享写入。
func (t *CodeIntelStatusTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 工具自身不设门槛, 权限交由上层策略裁决。
func (t *CodeIntelStatusTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Description 返回给模型看的英文说明。
func (t *CodeIntelStatusTool) Description() string {
	return `Get the status of code intelligence index from GitNexus and Graphify.`
}

// InputSchema 声明入参: 仅一个必填的 repo_path。
func (t *CodeIntelStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_path": {"type": "string", "description": "Absolute path to the indexed repository."}
		},
		"required": ["repo_path"]
	}`)
}

// Call 经 Engine.Status 取索引状态; 硬编码分支名 "main"。
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

// NewCodeIntelQueryTool 构造图谱查询工具 (本套件的主力)。
func NewCodeIntelQueryTool() *CodeIntelQueryTool { return &CodeIntelQueryTool{} }

// Name 返回工具名 "code_intel_query"。
func (t *CodeIntelQueryTool) Name() string                             { return "code_intel_query" }

// IsReadOnly 查询只读。
func (t *CodeIntelQueryTool) IsReadOnly(_ json.RawMessage) bool        { return true }

// IsConcurrencySafe 多个查询可并行发起 (引擎侧各自独立进程)。
func (t *CodeIntelQueryTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 工具自身不设门槛, 权限交由上层策略裁决。
func (t *CodeIntelQueryTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Description 返回给模型看的英文说明, 并列出 query_type 到两个引擎的分派关系。
func (t *CodeIntelQueryTool) Description() string {
	return `Query the code intelligence graph via GitNexus or Graphify CLI.
Query types:
- navigate / find_refs / cross_shard / impact → GitNexus structural queries
- path / explain / communities / god_nodes / surprises → Graphify semantic queries
- query (generic) → routed to both engines`
}

// InputSchema 声明入参: repo_path + query_type 必填, 其余字段按查询类型可选。
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

// Call 按 query_type 分派到 GitNexus (结构) 或 Graphify (语义), 并把结果 JSON 回给模型。
//
// 算法解读：
//
//	原理：一张 switch 表把 10 种 query_type 映射到两个外部引擎的方法; 只有 "query" 与
//		"explain" 走特例分支 —— 前者要同时问两边再合并, 后者只属于 Graphify。
//	关键不变量：分支名 "main" 是写死的。两个引擎都不做分支隔离索引, 所以查询永远打在
//		主分支的索引上; 换了分支不重新索引, 这里返回的就是过期结果。
//	代价与边界：每次调用都 NewEngine 起一遍外部 CLI 子进程, 无进程内缓存, 单次开销在
//		百毫秒量级; "query" 分支是两个 goroutine 并行发, 总耗时取两者较慢的一个。
//	设计权衡：不做 query_type → 引擎的注册表, 而是硬编码 switch —— 换来的是新增查询类型
//		时必须改代码 (编译器会提醒补分支), 而不是运行期静默路由到错误引擎。
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

// NewCodeIntelBranchTool 构造分支管理工具。
func NewCodeIntelBranchTool() *CodeIntelBranchTool { return &CodeIntelBranchTool{} }

// Name 返回工具名 "code_intel_branch"。
func (t *CodeIntelBranchTool) Name() string                             { return "code_intel_branch" }

// IsReadOnly switch / reindex 会改变仓库或索引状态, 故非只读。
func (t *CodeIntelBranchTool) IsReadOnly(_ json.RawMessage) bool        { return false }

// IsConcurrencySafe 含切分支与重建索引等写动作, 并发不安全。
func (t *CodeIntelBranchTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// CheckPermissions 工具自身不设门槛, 权限交由上层策略裁决。
func (t *CodeIntelBranchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Description 返回给模型看的英文说明, 并点明两个引擎默认不做分支隔离索引。
func (t *CodeIntelBranchTool) Description() string {
	return `Branch management for code intelligence.
GitNexus and Graphify do not maintain per-branch isolated indexes by default.
This tool provides: switch (git checkout), detect-changes, and status.`
}

// InputSchema 声明入参: repo_path + action 必填, branch_name 仅 switch 用。
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

// Call 按 action 分派 switch / detect_changes / status / reindex 四类分支操作。
//
// 注意：switch 分支**只拼出 git checkout 命令行文本, 自己并不执行** —— 返回的是给调用方
// 照着敲的字符串 (见 "git_checkout" 字段)。真正切分支要靠 bash 工具另跑一次, 这是刻意的
// 分工: 代码智能工具不改工作区状态。
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
			"action":         "detect_changes",
			"gitnexus":       safeMap(qr),
			"gitnexus_error": errString(e),
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

// safeMap 把可能为 nil 的查询结果转成可直接进 JSON 的值; nil 保持 nil 而不是空对象。
func safeMap(qr *codeintel.QueryResult) interface{} {
	if qr == nil {
		return nil
	}
	return qr.Results
}

// errString 把 error 转成字符串; nil 返回空串 (便于直接塞进结果 JSON)。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// RecallAliases 声明"这句话问的是我"的检索通路 (tool.RecallAliasProvider)。
//
// ── 为什么这里必须有这么一段 ────────────────────────────────────────────
//
// 描述是**英文 + 引擎术语** (navigate / find_refs / impact), 而问的人说的是
// "谁调用了这个函数"。这两边一个共同字符都没有, 词面检索**结构上**过不去 ——
// 不是排序没排好, 是没有可排的东西。
//
// 别名是那条绕过去的通路, 且是**可审计**的: 一条别名错了, 你看得见是谁写的。
// 对照 embedder 那条路 (pkg/memory/embed.go): 它召回错了你只知道"距离近"。
//
// ⚠️ 这里匹配的是**整串小写子串** (见 recall.go 的 textHits / substringHits),
// 不是分词后的词。所以条目要写成"用户可能原样打出来的整句话", 而不是关键词
// 拼盘 —— 写成 "调用方" 这种两字词, 只有当查询里**连着出现**这三个字时才命中,
// 它长什么样本身不影响分词 (别名与描述走同一条 textHits)。
//
// 也正因为它只对子串生效, 这一档**永远只能修一部分**: 覆盖的是"有人预见到了
// 这种说法", 不是"任意说法都能召回"。保留集里 ④ 跨语言那一档如实记着这个天花板。
func (t *CodeIntelQueryTool) RecallAliases() []string {
	return []string{
		// 英文侧: 描述里是 find_refs / impact 这种引擎术语, 不写这句就问不到
		"who calls this function", "who calls", "find callers", "call hierarchy",
		"find references", "impact analysis", "what breaks if",
		// 中文侧: 描述全是英文, 一个字都对不上
		"调用方", "谁调用", "引用关系", "影响面", "改了会怎样", "谁会受影响",
	}
}
