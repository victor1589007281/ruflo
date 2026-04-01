package tools

// 本文件实现通过 GitHub CLI（gh）调用的 MCP 工具（github_*）。
//
// 设计思路：
//   - 不直接使用 REST API，而是 exec gh，复用用户本机登录态（gh auth）。
//   - 返回 stdout、exit_code 与 error 字符串，便于上层判断是否成功。
//   - 依赖 PATH 中存在 gh，否则 runGh 会失败。

import (
	"context"
	"os/exec"
	"strconv"
	"strings"

	"github.com/ruflo/ruflo-go/mcp"
)

// runGh 执行 gh 子命令；返回合并后的标准输出/错误输出、退出码及 Go 错误（含 ExitError 时解析 exit code）。
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

// githubTools 构造 github_* MCP 工具：仓库分析、PR、Issue、Workflow、可用性探测。
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

// RegisterGitHubTools 向注册表登记通过 shell 调用 gh 的 MCP 工具。
func RegisterGitHubTools(reg *mcp.ToolRegistry) error {
	for _, t := range githubTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handleGitHubRepoAnalyze 处理 github_repo_analyze：可选 repo（owner/name）；gh repo view --json 基础字段。
func handleGitHubRepoAnalyze(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	repo := strArg(m, "repo")
	args := []string{"repo", "view", "--json", "name,description,url,defaultBranchRef"}
	if repo != "" {
		args = append(args, repo)
	}
	out, code, err := runGh(ctx, args...)
	return mcp.MCPToolResult{OK: err == nil, Data: map[string]any{"stdout": out, "exit_code": code, "error": errString(err)}}
}

// handleGitHubPRManage 处理 github_pr_manage：action=view 时需 number；否则 gh pr list --limit 20。
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

// handleGitHubIssueTrack 处理 github_issue_track：可选 state 传给 gh issue list。
func handleGitHubIssueTrack(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	state := strArg(m, "state")
	args := []string{"issue", "list", "--limit", "20"}
	if state != "" {
		args = append(args, "--state", state)
	}
	out, code, err := runGh(ctx, args...)
	return mcp.MCPToolResult{OK: err == nil, Data: map[string]any{"stdout": out, "exit_code": code, "error": errString(err)}}
}

// handleGitHubWorkflow 处理 github_workflow：gh workflow list --limit 30。
func handleGitHubWorkflow(ctx context.Context, _ map[string]any) mcp.MCPToolResult {
	out, code, err := runGh(ctx, "workflow", "list", "--limit", "30")
	return mcp.MCPToolResult{OK: err == nil, Data: map[string]any{"stdout": out, "exit_code": code, "error": errString(err)}}
}

// handleGitHubMetrics 处理 github_metrics：运行 gh version，根据退出码设置 gh_available。
func handleGitHubMetrics(ctx context.Context, _ map[string]any) mcp.MCPToolResult {
	_, code, err := runGh(ctx, "version")
	ok := err == nil && code == 0
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"gh_available": ok, "exit_code": code, "error": errString(err)}}
}
