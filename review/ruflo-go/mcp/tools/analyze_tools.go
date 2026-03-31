package tools

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ruflo/ruflo-go/mcp"
)

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

// RegisterAnalyzeTools registers git-diff based analysis tools.
func RegisterAnalyzeTools(reg *mcp.ToolRegistry) error {
	for _, t := range analyzeTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

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

func handleAnalyzeDiff(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	text, err := gitDiffText(ctx, strArg(m, "ref"), strArg(m, "path"))
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error(), Data: map[string]any{"diff": text}}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"diff": text, "bytes": len(text)}}
}

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

func handleAnalyzeDiffRisk(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	text, err := gitDiffText(ctx, strArg(m, "ref"), "")
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"risk": riskFromDiff(text), "lines": strings.Count(text, "\n")}}
}

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
