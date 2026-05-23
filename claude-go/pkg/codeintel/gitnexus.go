// gitnexus.go — GitNexus CLI 薄包装。
//
// 所有方法均为 exec 调用，零自定义逻辑。
package codeintel

import (
	"fmt"
	"time"
)

// GitNexus GitNexus CLI 包装器。
type GitNexus struct {
	RepoPath     string
	IndexBaseDir string
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
	stdout, stderr, err := runWithTimeout(g.RepoPath, 2*time.Hour, paths.NodePath, paths.GitNexusPath, "analyze", "--index-only", "--worker-timeout", "60", ".")
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

// IsIndexed 检查当前仓库是否已被 GitNexus 索引。
func (g *GitNexus) IsIndexed() bool {
	qr, err := g.Status()
	if err != nil {
		return false
	}
	m, ok := qr.Results.(map[string]interface{})
	if !ok {
		return false
	}
	status, _ := m["Status"].(string)
	return status != "" && status != "not indexed"
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
