// worktree.go 实现 Git 工作树切换占位逻辑与进程内定时任务（Cron）注册表。
//
// Worktree:
//   - EnterWorktree：在 Git 仓库下尝试创建 worktree（路径为 <cwd>/.claude-go-worktrees/<name>）。
//   - ExitWorktree：根据 action 移除 worktree 或仅返回说明；discard_changes 影响是否使用 git 的 force 语义。
//
// Cron:
//   - 全进程共享内存任务表，最多 50 条；不做真实调度，仅存储与列表展示。
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
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	EnterWorktreeToolName = "EnterWorktree"
	ExitWorktreeToolName  = "ExitWorktree"
	CronCreateToolName    = "CronCreate"
	CronDeleteToolName    = "CronDelete"
	CronListToolName      = "CronList"

	maxCronJobs = 50
)

var worktreeNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// --- Git worktree helpers ---

func denyIfPlan(tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式下不允许执行此写入类工具"}
	}
	return nil
}

// --- EnterWorktreeTool ---

type enterWorktreeInput struct {
	Name string `json:"name,omitempty"`
}

// EnterWorktreeTool 创建并切换到（逻辑上的）独立工作树目录。
type EnterWorktreeTool struct{}

// NewEnterWorktreeTool 构造 EnterWorktree 工具。
func NewEnterWorktreeTool() *EnterWorktreeTool {
	return &EnterWorktreeTool{}
}

func (t *EnterWorktreeTool) Name() string { return EnterWorktreeToolName }

func (t *EnterWorktreeTool) Description() string {
	return `在当前 Git 仓库下于 .claude-go-worktrees/<name> 创建 worktree（默认 name 为 default）。需系统已安装 git。`
}

func (t *EnterWorktreeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "可选，worktree 目录名，仅允许字母数字、._-"}
		},
		"additionalProperties": false
	}`)
}

func (t *EnterWorktreeTool) IsReadOnly(_ json.RawMessage) bool { return false }

func (t *EnterWorktreeTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *EnterWorktreeTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyIfPlan(tctx)
}

func (t *EnterWorktreeTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in enterWorktreeInput
	if len(input) > 0 && string(input) != "null" {
		if err := json.Unmarshal(input, &in); err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
		}
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "default"
	}
	if !worktreeNameRe.MatchString(name) {
		return &tool.ToolResult{Content: "name 仅允许 [a-zA-Z0-9._-]", IsError: true}, nil
	}
	cwd := "."
	if tctx != nil && tctx.Cwd != "" {
		cwd = tctx.Cwd
	}
	base := filepath.Join(cwd, ".claude-go-worktrees")
	path := filepath.Join(base, name)

	// 确认是 git 仓库
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--git-dir").CombinedOutput(); err != nil {
		return &tool.ToolResult{
			Content: fmt.Sprintf("当前目录不是 Git 仓库或 git 不可用: %v\n%s", err, strings.TrimSpace(string(out))),
			IsError: true,
		}, nil
	}

	if err := os.MkdirAll(base, 0o755); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("创建目录失败: %v", err), IsError: true}, nil
	}

	// git worktree add <path> 若已存在会失败，向调用方暴露 stderr
	cmd := exec.Command("git", "-C", cwd, "worktree", "add", path, "HEAD")
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return &tool.ToolResult{
			Content: fmt.Sprintf("git worktree add 失败: %v\n%s", err, text),
			IsError: true,
		}, nil
	}
	return &tool.ToolResult{
		Content: fmt.Sprintf("已创建 worktree：%s\n（建议后续工具将 working_directory 设为该路径。）\n%s", path, text),
	}, nil
}

// --- ExitWorktreeTool ---

type exitWorktreeInput struct {
	Action         string `json:"action"`
	DiscardChanges *bool  `json:"discard_changes,omitempty"`
}

// ExitWorktreeTool 移除 worktree 或执行其它退出动作。
type ExitWorktreeTool struct{}

// NewExitWorktreeTool 构造 ExitWorktree 工具。
func NewExitWorktreeTool() *ExitWorktreeTool {
	return &ExitWorktreeTool{}
}

func (t *ExitWorktreeTool) Name() string { return ExitWorktreeToolName }

func (t *ExitWorktreeTool) Description() string {
	return `退出 worktree：action 为 remove 时删除由 EnterWorktree 创建的目录（git worktree remove）；discard_changes 为 true 时附加 --force。`
}

func (t *ExitWorktreeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"description": "例如 remove（移除 worktree）、list（列出 worktree，只读查询）"
			},
			"discard_changes": {"type": "boolean", "description": "可选，remove 时是否强制丢弃本地修改"}
		},
		"required": ["action"]
	}`)
}

func (t *ExitWorktreeTool) IsReadOnly(_ json.RawMessage) bool { return false }

func (t *ExitWorktreeTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *ExitWorktreeTool) CheckPermissions(input json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	var in exitWorktreeInput
	_ = json.Unmarshal(input, &in)
	if strings.EqualFold(strings.TrimSpace(in.Action), "list") {
		return nil
	}
	return denyIfPlan(tctx)
}

func (t *ExitWorktreeTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in exitWorktreeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if action == "" {
		return &tool.ToolResult{Content: "action 不能为空", IsError: true}, nil
	}
	cwd := "."
	if tctx != nil && tctx.Cwd != "" {
		cwd = tctx.Cwd
	}

	switch action {
	case "list":
		out, err := exec.Command("git", "-C", cwd, "worktree", "list").CombinedOutput()
		if err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("git worktree list 失败: %v\n%s", err, string(out)), IsError: true}, nil
		}
		return &tool.ToolResult{Content: string(out)}, nil

	case "remove":
		// 移除 .claude-go-worktrees 下全部 worktree（简化语义），或文档说明需配合 name——此处移除所有子项
		base := filepath.Join(cwd, ".claude-go-worktrees")
		entries, err := filepath.Glob(filepath.Join(base, "*"))
		if err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("扫描 worktree 目录失败: %v", err), IsError: true}, nil
		}
		if len(entries) == 0 {
			return &tool.ToolResult{Content: "未找到 .claude-go-worktrees 下的 worktree。"}, nil
		}
		force := in.DiscardChanges != nil && *in.DiscardChanges
		var b strings.Builder
		for _, p := range entries {
			args := []string{"-C", cwd, "worktree", "remove"}
			if force {
				args = append(args, "--force")
			}
			args = append(args, p)
			out, err := exec.Command("git", args...).CombinedOutput()
			if err != nil {
				fmt.Fprintf(&b, "移除 %s 失败: %v %s\n", p, err, strings.TrimSpace(string(out)))
				continue
			}
			fmt.Fprintf(&b, "已移除 %s\n", p)
		}
		return &tool.ToolResult{Content: strings.TrimSpace(b.String())}, nil

	default:
		return &tool.ToolResult{Content: fmt.Sprintf("未知 action: %q（支持 list、remove）", in.Action), IsError: true}, nil
	}
}

// --- Cron 共享存储 ---

type cronJob struct {
	ID        string    `json:"id"`
	Cron      string    `json:"cron"`
	Prompt    string    `json:"prompt"`
	Recurring bool      `json:"recurring"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	cronMu   sync.Mutex
	cronJobs = make(map[string]cronJob)
)

// --- CronCreateTool ---

type cronCreateInput struct {
	Cron      string `json:"cron"`
	Prompt    string `json:"prompt"`
	Recurring *bool  `json:"recurring,omitempty"`
}

// CronCreateTool 在内存中注册一条 cron 任务（不做真实调度）。
type CronCreateTool struct{}

// NewCronCreateTool 构造 CronCreate 工具。
func NewCronCreateTool() *CronCreateTool {
	return &CronCreateTool{}
}

func (t *CronCreateTool) Name() string { return CronCreateToolName }

func (t *CronCreateTool) Description() string {
	return `创建定时任务记录：cron 表达式、提示词 prompt、是否 recurring；全进程最多 50 条，存储于内存。`
}

func (t *CronCreateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"cron": {"type": "string", "description": "cron 表达式"},
			"prompt": {"type": "string", "description": "触发时使用的提示词/任务描述"},
			"recurring": {"type": "boolean", "description": "可选，是否重复执行，默认 true"}
		},
		"required": ["cron", "prompt"]
	}`)
}

func (t *CronCreateTool) IsReadOnly(_ json.RawMessage) bool { return false }

func (t *CronCreateTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *CronCreateTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyIfPlan(tctx)
}

func (t *CronCreateTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in cronCreateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(in.Cron) == "" || strings.TrimSpace(in.Prompt) == "" {
		return &tool.ToolResult{Content: "cron 与 prompt 均不能为空", IsError: true}, nil
	}
	cronMu.Lock()
	defer cronMu.Unlock()
	if len(cronJobs) >= maxCronJobs {
		return &tool.ToolResult{Content: fmt.Sprintf("已达 cron 任务上限（%d 条）", maxCronJobs), IsError: true}, nil
	}
	id := fmt.Sprintf("cron-%d", time.Now().UnixNano())
	rec := true
	if in.Recurring != nil {
		rec = *in.Recurring
	}
	job := cronJob{ID: id, Cron: in.Cron, Prompt: in.Prompt, Recurring: rec, CreatedAt: time.Now().UTC()}
	cronJobs[id] = job
	raw, _ := json.Marshal(job)
	return &tool.ToolResult{Content: string(raw)}, nil
}

// --- CronDeleteTool ---

type cronDeleteInput struct {
	ID string `json:"id"`
}

// CronDeleteTool 按 id 删除内存中的 cron 记录。
type CronDeleteTool struct{}

// NewCronDeleteTool 构造 CronDelete 工具。
func NewCronDeleteTool() *CronDeleteTool {
	return &CronDeleteTool{}
}

func (t *CronDeleteTool) Name() string { return CronDeleteToolName }

func (t *CronDeleteTool) Description() string {
	return `根据 id 删除已注册的内存 cron 任务。`
}

func (t *CronDeleteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "CronCreate 返回的任务 id"}
		},
		"required": ["id"]
	}`)
}

func (t *CronDeleteTool) IsReadOnly(_ json.RawMessage) bool { return false }

func (t *CronDeleteTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *CronDeleteTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	return denyIfPlan(tctx)
}

func (t *CronDeleteTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in cronDeleteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(in.ID) == "" {
		return &tool.ToolResult{Content: "id 不能为空", IsError: true}, nil
	}
	cronMu.Lock()
	defer cronMu.Unlock()
	if _, ok := cronJobs[in.ID]; !ok {
		return &tool.ToolResult{Content: fmt.Sprintf("未找到 id=%q", in.ID), IsError: true}, nil
	}
	delete(cronJobs, in.ID)
	return &tool.ToolResult{Content: fmt.Sprintf("已删除 %s", in.ID)}, nil
}

// --- CronListTool ---

// CronListTool 列出当前内存中全部 cron 任务（只读）。
type CronListTool struct{}

// NewCronListTool 构造 CronList 工具。
func NewCronListTool() *CronListTool {
	return &CronListTool{}
}

func (t *CronListTool) Name() string { return CronListToolName }

func (t *CronListTool) Description() string {
	return `列出内存中已注册的全部 cron 任务（JSON 数组）。`
}

func (t *CronListTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {},
		"additionalProperties": false
	}`)
}

func (t *CronListTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *CronListTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *CronListTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *CronListTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	if len(input) > 0 && string(input) != "{}" && string(input) != "null" {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(input, &m); err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
		}
		if len(m) > 0 {
			return &tool.ToolResult{Content: "CronList 不需要任何字段，请传入空对象 {}。", IsError: true}, nil
		}
	}
	cronMu.Lock()
	list := make([]cronJob, 0, len(cronJobs))
	for _, j := range cronJobs {
		list = append(list, j)
	}
	cronMu.Unlock()
	raw, err := json.Marshal(list)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("序列化失败: %v", err), IsError: true}, nil
	}
	return &tool.ToolResult{Content: string(raw)}, nil
}
