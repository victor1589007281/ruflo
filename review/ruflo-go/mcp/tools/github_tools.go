package tools

import (
	"context"
	"os/exec"
	"strconv"
	"strings"

	"github.com/ruflo/ruflo-go/mcp"
)

func runGh(ctx context.Context, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	exit := 0
	if err != nil {
		exit = 1
		if x, ok := err.(*exec.ExitError); ok {
			exit = x.ExitCode()
		}
		return string(out), exit, err
	}
	return string(out), exit, nil
}

func githubTools() []*mcp.MCPTool {
	obj := map[string]any{"type": "object", "properties": map[string]any{}}
	return []*mcp.MCPTool{
		{Name: "github_repo_analyze", Description: "Run gh repo view and related inspection", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"repo": map[string]any{"type": "string"}}}, Handler: toolHandler(handleGitHubRepoAnalyze)},
		{Name: "github_pr_manage", Description: "List or view PRs via gh pr", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string"}, "number": map[string]any{"type": "number"}}}, Handler: toolHandler(handleGitHubPRManage)},
		{Name: "github_issue_track", Description: "List issues via gh issue", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"state": map[string]any{"type": "string"}}}, Handler: toolHandler(handleGitHubIssueTrack)},
		{Name: "github_workflow", Description: "List workflows via gh workflow", InputSchema: obj, Handler: toolHandler(handleGitHubWorkflow)},
		{Name: "github_metrics", Description: "Aggregate gh CLI availability", InputSchema: obj, Handler: toolHandler(handleGitHubMetrics)},
	}
}

// RegisterGitHubTools registers tools that shell out to the gh CLI.
func RegisterGitHubTools(reg *mcp.ToolRegistry) error {
	for _, t := range githubTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleGitHubRepoAnalyze(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	repo := strArg(m, "repo")
	args := []string{"repo", "view", "--json", "name,description,url,defaultBranchRef"}
	if repo != "" {
		args = append(args, repo)
	}
	out, code, err := runGh(ctx, args...)
	return mcp.MCPToolResult{OK: err == nil, Data: map[string]any{"stdout": out, "exit_code": code, "error": errString(err)}}
}

func handleGitHubPRManage(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	action := strings.ToLower(strArg(m, "action"))
	if action == "view" {
		n := 0
		if v, ok := m["number"].(float64); ok {
			n = int(v)
		}
		if n <= 0 {
			return mcp.MCPToolResult{OK: false, Error: "number required for view"}
		}
		out, code, err := runGh(ctx, "pr", "view", strconv.Itoa(n), "--json", "title,state,url")
		return mcp.MCPToolResult{OK: err == nil, Data: map[string]any{"stdout": out, "exit_code": code, "error": errString(err)}}
	}
	out, code, err := runGh(ctx, "pr", "list", "--limit", "20")
	return mcp.MCPToolResult{OK: err == nil, Data: map[string]any{"stdout": out, "exit_code": code, "error": errString(err)}}
}

func handleGitHubIssueTrack(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	state := strArg(m, "state")
	args := []string{"issue", "list", "--limit", "20"}
	if state != "" {
		args = append(args, "--state", state)
	}
	out, code, err := runGh(ctx, args...)
	return mcp.MCPToolResult{OK: err == nil, Data: map[string]any{"stdout": out, "exit_code": code, "error": errString(err)}}
}

func handleGitHubWorkflow(ctx context.Context, _ map[string]any) mcp.MCPToolResult {
	out, code, err := runGh(ctx, "workflow", "list", "--limit", "30")
	return mcp.MCPToolResult{OK: err == nil, Data: map[string]any{"stdout": out, "exit_code": code, "error": errString(err)}}
}

func handleGitHubMetrics(ctx context.Context, _ map[string]any) mcp.MCPToolResult {
	_, code, err := runGh(ctx, "version")
	ok := err == nil && code == 0
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"gh_available": ok, "exit_code": code, "error": errString(err)}}
}
