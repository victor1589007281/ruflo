// gitinspect.go 实现 GitInspect 工具 (只读/并发安全)。
//
// 它存在的理由不是"再包一层 git status": 只读分析档位没有 Bash, 而 Bash 的
// 只读判定本身也不是安全边界 (只看首个 token, `cat x > y` 仍会被判只读)。
// 给 agent 一个固定动词、固定参数的 git 读取面, 才能既让它看见未提交内容,
// 又不把任意 shell 能力塞回去。
//
// 安全边界由**调用方写入的 scope 文件**决定, 不由模型输入决定:
//
//		CLAUDE_GO_GIT_INSPECT_SCOPE=/tmp/.../scope.json
//		{"root":"/path/to/repo","paths":["src/a.go","README.md"]}
//
//	  - root 唯一, 模型不能换仓库;
//	  - diff 只能针对 paths 里的文件, 不能借 `-- path` 探其它文件;
//	  - status 输出也按 paths 过滤, 被后端判为敏感而未列入的文件连名字都不出现;
//	  - --no-ext-diff / --no-textconv / --no-optional-locks / 无 pager, 避免仓库
//	    config 把一次"读取"变成执行外部程序或写 index。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const GitInspectToolName = "GitInspect"

// GitInspectScopeEnv 指向后端生成的只读 scope JSON。
const GitInspectScopeEnv = "CLAUDE_GO_GIT_INSPECT_SCOPE"

const (
	gitInspectMaxOutput = 160 * 1024
	gitInspectTimeout   = 30 * time.Second
)

type gitInspectScope struct {
	Root  string   `json:"root"`
	Paths []string `json:"paths"`
}

type gitInspectInput struct {
	Operation string `json:"operation"`
	// Path 仅 diff 使用; 必须在 scope.paths 内。
	Path string `json:"path,omitempty"`
	// Staged=true 看 index 与 HEAD 的差异; 默认看工作区与 index 的差异。
	Staged bool `json:"staged,omitempty"`
	Stat   bool `json:"stat,omitempty"`
	Limit  int  `json:"limit,omitempty"`
}

type GitInspectTool struct{}

func NewGitInspectTool() *GitInspectTool { return &GitInspectTool{} }

func (t *GitInspectTool) Name() string { return GitInspectToolName }

func (t *GitInspectTool) Description() string {
	return `Read-only git inspection for the current commit-review scope.
Operations:
- status: changed files inside the authorized scope (porcelain)
- diff: worktree-vs-index diff (or staged diff), optionally for one authorized path; stat=true returns a diffstat
- log: recent commit subjects (no patches)

The repository root and authorized paths are fixed by the host. You cannot inspect other repositories or files outside the scope. This tool never writes, stages, commits, or pushes.`
}

func (t *GitInspectTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"operation": {"type": "string", "enum": ["status", "diff", "log"]},
			"path": {"type": "string", "description": "For diff: one path from the authorized scope. Omit to diff all authorized paths."},
			"staged": {"type": "boolean", "description": "For diff: compare the index against HEAD instead of the worktree."},
			"stat": {"type": "boolean", "description": "For diff: return a diffstat instead of the full patch."},
			"limit": {"type": "integer", "description": "For log: number of commits, 1-30 (default 10)."}
		},
		"required": ["operation"]
	}`)
}

func (t *GitInspectTool) IsReadOnly(_ json.RawMessage) bool        { return true }
func (t *GitInspectTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *GitInspectTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *GitInspectTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in gitInspectInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	scope, err := loadGitInspectScope()
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	allowed := make(map[string]bool, len(scope.Paths))
	for _, p := range scope.Paths {
		allowed[p] = true
	}

	switch in.Operation {
	case "status":
		out, err := gitInspectStatus(ctx, scope, allowed)
		if err != nil {
			return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		return &tool.ToolResult{Content: capGitOutput(redactGitSecrets(out))}, nil
	case "diff":
		paths := scope.Paths
		if in.Path != "" {
			p, ok := normalizeScopePath(in.Path)
			if !ok || !allowed[p] {
				return &tool.ToolResult{Content: fmt.Sprintf("拒绝: 路径不在授权范围内: %s", in.Path), IsError: true}, nil
			}
			paths = []string{p}
		}
		out, err := gitInspectDiff(ctx, scope.Root, paths, in.Staged, in.Stat)
		if err != nil {
			return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		if strings.TrimSpace(out) == "" {
			out = "(no diff)"
		}
		return &tool.ToolResult{Content: capGitOutput(redactGitSecrets(out))}, nil
	case "log":
		limit := in.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 30 {
			limit = 30
		}
		out, err := gitInspectLog(ctx, scope.Root, limit)
		if err != nil {
			return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		return &tool.ToolResult{Content: capGitOutput(redactGitSecrets(out))}, nil
	default:
		return &tool.ToolResult{Content: "operation 只能是 status / diff / log", IsError: true}, nil
	}
}

func loadGitInspectScope() (*gitInspectScope, error) {
	file := strings.TrimSpace(os.Getenv(GitInspectScopeEnv))
	if file == "" {
		return nil, fmt.Errorf("GitInspect 未启用: 缺少 %s", GitInspectScopeEnv)
	}
	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("GitInspect scope 不可用: %v", err)
	}
	if info.Size() > 4<<20 {
		return nil, fmt.Errorf("GitInspect scope 过大")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("GitInspect scope 读取失败: %v", err)
	}
	var scope gitInspectScope
	if err := json.Unmarshal(raw, &scope); err != nil {
		return nil, fmt.Errorf("GitInspect scope 解析失败: %v", err)
	}
	root, err := filepath.Abs(strings.TrimSpace(scope.Root))
	if err != nil || strings.TrimSpace(scope.Root) == "" {
		return nil, fmt.Errorf("GitInspect scope.root 无效")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("GitInspect 仓库根不可用: %v", err)
	}
	out := &gitInspectScope{Root: root}
	seen := map[string]bool{}
	for _, p := range scope.Paths {
		n, ok := normalizeScopePath(p)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		out.Paths = append(out.Paths, n)
		if len(out.Paths) >= 1000 {
			break
		}
	}
	if len(out.Paths) == 0 {
		return nil, fmt.Errorf("GitInspect scope.paths 为空")
	}
	return out, nil
}

// normalizeScopePath 把 scope 里的路径统一成 git 使用的 slash 相对路径。
// 绝对路径、`..` 逃逸与空路径一律拒绝 —— 它们会让 `git diff -- <path>`
// 指向 scope 之外。
func normalizeScopePath(p string) (string, bool) {
	p = strings.TrimSpace(filepath.ToSlash(p))
	if p == "" || p == "." || strings.HasPrefix(p, "/") {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", false
	}
	return clean, true
}

func gitInspectEnv() []string {
	return append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"LC_ALL=C",
	)
}

func gitInspectRun(ctx context.Context, root string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gitInspectTimeout)
	defer cancel()
	base := []string{"--no-pager", "--no-optional-locks", "-C", root}
	cmd := exec.CommandContext(cctx, "git", append(base, args...)...)
	cmd.Env = gitInspectEnv()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git 超时")
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git 失败: %s", msg)
	}
	return stdout.String(), nil
}

func gitInspectStatus(ctx context.Context, scope *gitInspectScope, allowed map[string]bool) (string, error) {
	// 先确认 root 真的是一个仓库的顶层。不是的话 git status 也会失败, 但这里
	// 的报错能说清"scope.root 指错了", 而不是给模型一段看不懂的 git 错误。
	top, err := gitInspectRun(ctx, scope.Root, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("GitInspect 仓库不可用: %v", err)
	}
	if real, err := filepath.EvalSymlinks(strings.TrimSpace(top)); err == nil && real != scope.Root {
		return "", fmt.Errorf("GitInspect scope.root 不是仓库顶层")
	}

	raw, err := gitInspectRun(ctx, scope.Root,
		"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=no")
	if err != nil {
		return "", err
	}
	parts := strings.Split(raw, "\x00")
	var lines []string
	for i := 0; i < len(parts); i++ {
		entry := parts[i]
		if len(entry) < 4 {
			continue
		}
		status := entry[:2]
		path := entry[3:]
		old := ""
		if strings.ContainsAny(status, "RC") && i+1 < len(parts) {
			old = parts[i+1]
			i++
		}
		norm, ok := normalizeScopePath(path)
		if !ok || !allowed[norm] {
			continue
		}
		line := status + " " + norm
		if old != "" {
			if oldNorm, ok := normalizeScopePath(old); ok && allowed[oldNorm] {
				line += " (from " + oldNorm + ")"
			} else {
				line += " (renamed from an unscoped path)"
			}
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "(no changes in scoped paths)", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func gitInspectDiff(ctx context.Context, root string, paths []string, staged, stat bool) (string, error) {
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color"}
	if staged {
		args = append(args, "--cached")
	}
	if stat {
		args = append(args, "--stat")
	}
	args = append(args, "--")
	args = append(args, paths...)
	return gitInspectRun(ctx, root, args...)
}

func gitInspectLog(ctx context.Context, root string, limit int) (string, error) {
	args := []string{
		"log", "--no-color", "--no-decorate", "--no-show-signature",
		"--date=short", "--format=%h%x09%ad%x09%s",
		"-n", fmt.Sprintf("%d", limit),
	}
	return gitInspectRun(ctx, root, args...)
}

// gitSecretPatterns 只放**高置信度**的凭据形态, 不追求覆盖所有"看起来像密钥"的
// 字符串。这里的目的是防止 diff 的**已删除行**把旧密钥重新送进模型上下文:
// 工作区文件内容已经由后端扫描过, 但一条被删掉的 secret 行只存在于 diff 里。
var gitSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{15,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
}

func redactGitSecrets(s string) string {
	for _, re := range gitSecretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED:SECRET]")
	}
	return s
}

// capGitOutput 把工具输出截断到固定上限。截断是必要的: 一个 10 万行的 diff
// 直接进上下文既烧 token, 也会把真正有用的文件挤出模型窗口。
func capGitOutput(s string) string {
	if len(s) <= gitInspectMaxOutput {
		return s
	}
	cut := gitInspectMaxOutput
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // 不切断 UTF-8
		cut--
	}
	return s[:cut] + "\n... (GitInspect 输出已截断)"
}
