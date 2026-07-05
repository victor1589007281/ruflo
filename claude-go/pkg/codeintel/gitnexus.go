// gitnexus.go — GitNexus CLI 薄包装。
//
// 所有方法均为 exec 调用，零自定义逻辑。
package codeintel

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// GitNexus GitNexus CLI 包装器。
type GitNexus struct {
	RepoPath     string
	IndexBaseDir string
	MaxHeapMB    int           // Node.js V8 最大堆内存（MB），0 表示使用 GitNexus 默认值
	IndexTimeout time.Duration // analyze 执行超时，0 表示默认 2h
	paths        *ToolPaths
}

// NewGitNexus 创建 GitNexus 包装器。
func NewGitNexus(repoPath string) *GitNexus {
	return &GitNexus{RepoPath: repoPath}
}

func (g *GitNexus) ensurePaths() (*ToolPaths, error) {
	if g.paths != nil {
		return g.paths, nil
	}
	paths, err := DiscoverTools()
	if err != nil {
		return nil, err
	}
	g.paths = paths
	return paths, nil
}

func (g *GitNexus) setupIndex() error {
	if g.IndexBaseDir == "" {
		return nil
	}
	im := NewIndexManager(g.IndexBaseDir)
	if im == nil {
		return nil
	}
	_, _, err := im.SetupRepoIndex(g.RepoPath)
	return err
}

// Analyze 执行 gitnexus analyze（全量索引）。
func (g *GitNexus) Analyze() (*QueryResult, error) {
	if err := g.setupIndex(); err != nil {
		return nil, fmt.Errorf("setup index: %w", err)
	}
	paths, err := g.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()

	var env []string
	if g.MaxHeapMB > 0 {
		nodeOpts := os.Getenv("NODE_OPTIONS")
		if nodeOpts != "" {
			nodeOpts += " "
		}
		nodeOpts += fmt.Sprintf("--max-old-space-size=%d", g.MaxHeapMB)
		env = append(env, "NODE_OPTIONS="+nodeOpts)
	}
	env = append(env,
		"GITNEXUS_WORKER_POOL_SIZE=32",
		"GITNEXUS_CHUNK_BYTE_BUDGET=8388608",
		"GITNEXUS_PARSE_CHUNK_CONCURRENCY=4",
	)

	timeout := g.IndexTimeout
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	stdout, stderr, err := runWithTimeoutEnv(g.RepoPath, timeout, env, paths.NodePath, paths.GitNexusPath, "analyze", "--index-only", "--worker-timeout", "60", ".")
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("gitnexus_analyze", combined, execError("gitnexus", err, stderr), start), nil
	}
	return wrapResult("gitnexus_analyze", filterJSONOutput(combined), start), nil
}

// Status 执行 gitnexus status。
func (g *GitNexus) Status() (*QueryResult, error) {
	paths, err := g.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, err := runTool(g.RepoPath, paths.NodePath, paths.GitNexusPath, "status")
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("gitnexus_status", combined, execError("gitnexus", err, stderr), start), nil
	}
	return wrapResult("gitnexus_status", filterJSONOutput(combined), start), nil
}

// Query 执行 gitnexus query <search_query>。
func (g *GitNexus) Query(q string) (*QueryResult, error) {
	paths, err := g.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, err := runTool(g.RepoPath, paths.NodePath, paths.GitNexusPath, "query", q)
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("gitnexus_query", combined, execError("gitnexus", err, stderr), start), nil
	}
	return wrapResult("gitnexus_query", filterJSONOutput(combined), start), nil
}

// Context 执行 gitnexus context <name>。
func (g *GitNexus) Context(name string) (*QueryResult, error) {
	paths, err := g.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, err := runTool(g.RepoPath, paths.NodePath, paths.GitNexusPath, "context", name)
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("gitnexus_context", combined, execError("gitnexus", err, stderr), start), nil
	}
	return wrapResult("gitnexus_context", filterJSONOutput(combined), start), nil
}

// Impact 执行 gitnexus impact <target>。
func (g *GitNexus) Impact(target string) (*QueryResult, error) {
	paths, err := g.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, err := runTool(g.RepoPath, paths.NodePath, paths.GitNexusPath, "impact", target)
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("gitnexus_impact", combined, execError("gitnexus", err, stderr), start), nil
	}
	return wrapResult("gitnexus_impact", filterJSONOutput(combined), start), nil
}

// DetectChanges 执行 gitnexus detect-changes。
func (g *GitNexus) DetectChanges() (*QueryResult, error) {
	paths, err := g.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, err := runTool(g.RepoPath, paths.NodePath, paths.GitNexusPath, "detect-changes")
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("gitnexus_detect_changes", combined, execError("gitnexus", err, stderr), start), nil
	}
	return wrapResult("gitnexus_detect_changes", filterJSONOutput(combined), start), nil
}

// List 执行 gitnexus list（列出所有索引仓库）。
func (g *GitNexus) List() (*QueryResult, error) {
	paths, err := g.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, err := runTool(g.RepoPath, paths.NodePath, paths.GitNexusPath, "list")
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("gitnexus_list", combined, execError("gitnexus", err, stderr), start), nil
	}
	return wrapResult("gitnexus_list", filterJSONOutput(combined), start), nil
}

// Cypher 执行原始 Cypher 查询（gitnexus cypher）。
func (g *GitNexus) Cypher(query string) (*QueryResult, error) {
	paths, err := g.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, err := runTool(g.RepoPath, paths.NodePath, paths.GitNexusPath, "cypher", query)
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("gitnexus_cypher", combined, execError("gitnexus", err, stderr), start), nil
	}
	return wrapResult("gitnexus_cypher", filterJSONOutput(combined), start), nil
}

// gitnexusIndexedFrom 从一次 GitNexus.Status() 结果推断是否已索引。
// GitNexus.Status() 经 wrapResult/tryParseJSON 后结果形如
// {"output": "...仓库: X\n索引提交: abc\n状态: ✅ 已是最新..."}（CLI 报错时经
// wrapError 含 "error" 键）。老逻辑读取并不存在的 "Status" 键 → 恒 false，
// 只因此前 Status 路径从不调用它才未暴露。这里改为解析权威的 CLI 文本。
// 抽为独立函数，使各状态入口共用同一判定，避免重复解析。
func gitnexusIndexedFrom(qr *QueryResult, err error) bool {
	if err != nil || qr == nil {
		return false
	}
	m, ok := qr.Results.(map[string]interface{})
	if !ok {
		return false
	}
	if _, isErr := m["error"]; isErr { // wrapError：CLI 报错 → 视为未索引
		return false
	}
	out, _ := m["output"].(string)
	if out == "" {
		return false
	}
	for _, neg := range []string{"未索引", "尚未索引", "未建立索引", "not indexed"} {
		if strings.Contains(out, neg) {
			return false
		}
	}
	// 已索引判据：状态行含索引提交或"已是最新/需要更新"（后者=已索引但需增量更新）。
	return strings.Contains(out, "索引提交") ||
		strings.Contains(out, "已是最新") ||
		strings.Contains(out, "需要更新")
}

// IsIndexed 检查当前仓库是否已被 GitNexus 索引。
func (g *GitNexus) IsIndexed() bool {
	return gitnexusIndexedFrom(g.Status())
}

// Navigate 符号导航（alias to Context）。
func (g *GitNexus) Navigate(branchName string, q NavigateQuery) (*QueryResult, error) {
	return g.Context(q.Symbol)
}

// FindRefs 符号引用（alias to Context）。
func (g *GitNexus) FindRefs(branchName string, q NavigateQuery) (*QueryResult, error) {
	return g.Context(q.Symbol)
}

// CrossShard 跨片查询（alias to Query）。
func (g *GitNexus) CrossShard(branchName string, q CrossShardQuery) (*QueryResult, error) {
	return g.Query(q.Symbol)
}

// ImpactQuery 影响分析（包装 Impact）。
func (g *GitNexus) ImpactQuery(branchName string, q ImpactQuery) (*QueryResult, error) {
	return g.Impact(q.FilePath)
}
