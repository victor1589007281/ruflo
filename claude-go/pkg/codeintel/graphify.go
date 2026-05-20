// graphify.go — Graphify CLI 薄包装。
//
// 所有方法均为 exec 调用，零自定义逻辑。
package codeintel

import (
	"fmt"
	"os"
	"time"
)

// Graphify Graphify CLI 包装器。
type Graphify struct {
	RepoPath string
	paths    *ToolPaths
}

// NewGraphify 创建 Graphify 包装器。
func NewGraphify(repoPath string) *Graphify {
	return &Graphify{RepoPath: repoPath}
}

func (gf *Graphify) ensurePaths() (*ToolPaths, error) {
	if gf.paths != nil {
		return gf.paths, nil
	}
	paths, err := DiscoverTools()
	if err != nil {
		return nil, err
	}
	gf.paths = paths
	return paths, nil
}

// Update 执行 graphify update（重新提取代码并更新图谱）。
func (gf *Graphify) Update(force bool) (*QueryResult, error) {
	paths, err := gf.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	args := []string{"update", "."}
	if force {
		args = append(args, "--force")
	}
	stdout, stderr, err := runWithTimeout(gf.RepoPath, 30*time.Minute, paths.GraphifyPath, args...)
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("graphify_update", combined, execError("graphify", err, stderr), start), nil
	}
	return wrapResult("graphify_update", combined, start), nil
}

// ClusterOnly 执行 graphify cluster-only（仅重新聚类）。
func (gf *Graphify) ClusterOnly() (*QueryResult, error) {
	paths, err := gf.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, err := runWithTimeout(gf.RepoPath, 30*time.Minute, paths.GraphifyPath, "cluster-only", ".")
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("graphify_cluster", combined, execError("graphify", err, stderr), start), nil
	}
	return wrapResult("graphify_cluster", combined, start), nil
}

// Query 执行 graphify query "<question>"。
// 默认 budget=2000 tokens，可通过环境变量 GRAPHIFY_BUDGET 覆盖。
func (gf *Graphify) Query(question string) (*QueryResult, error) {
	paths, err := gf.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	budget := "2000"
	if b := os.Getenv("GRAPHIFY_BUDGET"); b != "" {
		budget = b
	}
	stdout, stderr, err := runTool(gf.RepoPath, paths.GraphifyPath, "query", question, "--budget", budget)
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("graphify_query", combined, execError("graphify", err, stderr), start), nil
	}
	return wrapResult("graphify_query", combined, start), nil
}

// Path 执行 graphify path "A" "B"。
func (gf *Graphify) Path(src, dst string) (*QueryResult, error) {
	paths, err := gf.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, err := runTool(gf.RepoPath, paths.GraphifyPath, "path", src, dst)
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("graphify_path", combined, execError("graphify", err, stderr), start), nil
	}
	return wrapResult("graphify_path", combined, start), nil
}

// Explain 执行 graphify explain "X"。
func (gf *Graphify) Explain(node string) (*QueryResult, error) {
	paths, err := gf.ensurePaths()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	stdout, stderr, err := runTool(gf.RepoPath, paths.GraphifyPath, "explain", node)
	combined := append(stderr, '\n')
	combined = append(combined, stdout...)
	if err != nil {
		return wrapError("graphify_explain", combined, execError("graphify", err, stderr), start), nil
	}
	return wrapResult("graphify_explain", combined, start), nil
}

// IsIndexed 检查当前仓库是否已有 Graphify 图谱。
func (gf *Graphify) IsIndexed() bool {
	// 简单检查 graphify-out/graph.json 是否存在
	gfOut := fmt.Sprintf("%s/graphify-out/graph.json", gf.RepoPath)
	_, err := os.Stat(gfOut)
	return err == nil
}

// Communities 社区列表（alias to Explain 或返回提示）。
// Graphify 没有直接的 communities 子命令，用 explain 聚合节点替代。
func (gf *Graphify) Communities(branchName string, q CommunityQuery) (*QueryResult, error) {
	return gf.Query(fmt.Sprintf("communities in shard %s", q.Shard))
}

// GodNodes 高度数节点（alias to Query）。
func (gf *Graphify) GodNodes(branchName, shardName string, topN int) (*QueryResult, error) {
	return gf.Query(fmt.Sprintf("top %d god nodes in %s", topN, shardName))
}

// Surprises 异常边检测（alias to Query）。
func (gf *Graphify) Surprises(branchName, shardName string, topN int) (*QueryResult, error) {
	return gf.Query(fmt.Sprintf("anomalous edges in %s", shardName))
}
