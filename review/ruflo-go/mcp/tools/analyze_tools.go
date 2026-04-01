package tools

// 本文件实现基于 git diff 与路径启发式的「代码分析」MCP 工具（analyze_*）。
//
// 设计思路：
//   - 通过 exec 调用本地 git，获取 diff 文本或 --stat，供 IDE/Agent 侧做变更概览。
//   - 风险分数、分类标签、评审角色建议均为轻量启发式（关键词、路径片段、行数），非正式 SAST。
//   - 需在含 git 仓库的工作目录下使用，否则命令会失败并返回错误信息。

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ruflo/ruflo-go/mcp"
)

// analyzeTools 构造 analyze_* 系列 MCP 工具（diff、风险、分类、评审建议、单文件风险、统计）。
func analyzeTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{Name: "analyze_diff", Description: "Run git diff and return output", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"ref": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}}}, Handler: toolHandler(handleAnalyzeDiff)},
		{Name: "analyze_diff-risk", Description: "Heuristic risk score from diff text", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"ref": map[string]any{"type": "string"}}}, Handler: toolHandler(handleAnalyzeDiffRisk)},
		{Name: "analyze_diff-classify", Description: "Classify diff by touched paths", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"ref": map[string]any{"type": "string"}}}, Handler: toolHandler(handleAnalyzeDiffClassify)},
		{Name: "analyze_diff-reviewers", Description: "Suggest reviewer labels from diff", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"ref": map[string]any{"type": "string"}}}, Handler: toolHandler(handleAnalyzeDiffReviewers)},
		{Name: "analyze_file-risk", Description: "Static heuristics for a file path", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}, Handler: toolHandler(handleAnalyzeFileRisk)},
		{Name: "analyze_diff-stats", Description: "git diff --stat summary", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"ref": map[string]any{"type": "string"}}}, Handler: toolHandler(handleAnalyzeDiffStats)},
	}
}

// RegisterAnalyzeTools 向注册表登记基于 git diff 的分析类 MCP 工具。
func RegisterAnalyzeTools(reg *mcp.ToolRegistry) error {
	for _, t := range analyzeTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// gitDiffText 执行 git diff；ref 非空时作为对比引用（如分支或提交），path 非空时限定路径（git diff ref -- path）。
func gitDiffText(ctx context.Context, ref, path string) (string, error) {
	args := []string{"diff"}
	if ref != "" {
		args = append(args, ref)
	}
	if path != "" {
		args = append(args, "--", path)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// handleAnalyzeDiff 处理 analyze_diff：可选 ref、path；返回 diff 全文与字节长度。
func handleAnalyzeDiff(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	text, err := gitDiffText(ctx, strArg(m, "ref"), strArg(m, "path"))
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error(), Data: map[string]any{"diff": text}}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"diff": text, "bytes": len(text)}}
}

// riskFromDiff 根据 diff 文本中的敏感关键词密度与行数累加风险分，上限截断为 1.0。
func riskFromDiff(text string) float64 {
	score := 0.0
	lower := strings.ToLower(text)
	for _, kw := range []string{"password", "secret", "token", "auth", "crypto", "exec(", "eval("} {
		if strings.Contains(lower, kw) {
			score += 0.15
		}
	}
	if strings.Count(text, "\n") > 200 {
		score += 0.1
	}
	if score > 1 {
		score = 1
	}
	return score
}

// handleAnalyzeDiffRisk 处理 analyze_diff-risk：参数 ref；对整仓 diff 计算 risk 与行数。
func handleAnalyzeDiffRisk(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	text, err := gitDiffText(ctx, strArg(m, "ref"), "")
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"risk": riskFromDiff(text), "lines": strings.Count(text, "\n")}}
}

// handleAnalyzeDiffClassify 处理 analyze_diff-classify：根据 diff 中出现的路径片段打标签（go/tests/mcp/library）。
func handleAnalyzeDiffClassify(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	text, err := gitDiffText(ctx, strArg(m, "ref"), "")
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	tags := []string{}
	for _, p := range []struct{ needle, tag string }{
		{".go", "go"}, {"test", "tests"}, {"mcp/", "mcp"}, {"pkg/", "library"},
	} {
		if strings.Contains(text, p.needle) {
			tags = append(tags, p.tag)
		}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"tags": tags}}
}

// handleAnalyzeDiffReviewers 处理 analyze_diff-reviewers：据是否含测试文件与风险分建议 reviewer 角色列表。
func handleAnalyzeDiffReviewers(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	text, err := gitDiffText(ctx, strArg(m, "ref"), "")
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	revs := []string{"coder"}
	if strings.Contains(text, "_test.go") {
		revs = append(revs, "tester")
	}
	if riskFromDiff(text) > 0.4 {
		revs = append(revs, "security")
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"suggested": revs}}
}

// handleAnalyzeFileRisk 处理 analyze_file-risk：参数 path（必填）；按文件名与路径关键字估算静态风险。
func handleAnalyzeFileRisk(_ context.Context, m map[string]any) mcp.MCPToolResult {
	p := strArg(m, "path")
	risk := 0.1
	base := filepath.Base(p)
	if strings.Contains(strings.ToLower(base), "secret") || strings.Contains(strings.ToLower(base), "auth") {
		risk += 0.5
	}
	if strings.HasSuffix(p, ".go") && strings.Contains(strings.ToLower(p), "crypto") {
		risk += 0.2
	}
	if risk > 1 {
		risk = 1
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"path": p, "risk": risk}}
}

// handleAnalyzeDiffStats 处理 analyze_diff-stats：可选 ref；执行 git diff --stat 并返回统计输出。
func handleAnalyzeDiffStats(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	ref := strArg(m, "ref")
	args := []string{"diff", "--stat"}
	if ref != "" {
		args = append(args, ref)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error(), Data: map[string]any{"stat": string(out)}}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"stat": string(out)}}
}
