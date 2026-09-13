// bash.go 实现 Bash/Shell 工具 (非并发安全)。
// 对应 TS 源码: review/claude/src/tools/BashTool/BashTool.ts
//
// 功能:
//   - 在 shell 中执行命令
//   - 支持工作目录指定
//   - 支持超时控制
//   - 捕获 stdout + stderr
//   - 返回退出码
//   - 大输出截断保护
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/sandbox"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// BashToolName 是命令执行工具对外暴露的名字。
//
// 2026-09-13 之前叫 "Shell"。改成 "Bash" 是为了跟 Claude Code 的工具面对齐:
// 模型 (尤其按 Claude 语料训练的) 会直接写 tool_use name="Bash", 旧名只能靠
// xml_toolcall 的别名表兜底, 而直连原生 tool_use 的模型没有这层兜底。
// 旧名不作废: Aliases() 仍报 "Shell", 治理配置 (AllowedTools/DisabledTools)、
// 持久化会话状态里残留的 "Shell" 继续生效。
const BashToolName = "Bash"

const maxResultSizeChars = 30000

// 默认超时 30 秒
const (
	defaultBashTimeout = 30 * time.Second
)

type bashInput struct {
	Command          string `json:"command"`
	WorkingDirectory string `json:"working_directory,omitempty"`
	Description      string `json:"description,omitempty"`
	BlockUntilMs     int    `json:"block_until_ms,omitempty"`
}

type BashTool struct{}

func NewBashTool() *BashTool { return &BashTool{} }

func (t *BashTool) Name() string { return BashToolName }

// Aliases 保留 2026-09-13 之前的旧名, 供治理配置与历史会话状态继续按 "Shell" 引用它。
func (t *BashTool) Aliases() []string { return []string{"Shell"} }

func (t *BashTool) Description() string {
	// 描述里不用反引号 —— 整个字符串是 raw literal, 反引号会把它截断。
	return `Executes a given command in a shell session.
- Pass the command as the "command" field; use "working_directory" to run it somewhere other than the default cwd (it does not persist between calls).
- Prefer the dedicated tools when one applies (Read / Write / StrReplace / Glob / Grep) — they are permission-checked and render better than cat/sed/echo/find/grep.
- Never start commands that wait for interactive input: they hang until the timeout.
- The tool blocks up to "block_until_ms" (default 30000) and returns what has been collected so far; output beyond 30000 characters is truncated with a note.`
}

func (t *BashTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "The command to execute."},
			"working_directory": {"type": "string", "description": "The working directory to execute the command in."},
			"description": {"type": "string", "description": "Brief description of what this command does."},
			"block_until_ms": {"type": "number", "description": "How long to block and wait (ms). Default 30000."}
		},
		"required": ["command"]
	}`)
}

func parseBashCommand(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var in bashInput
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	return in.Command
}

func (t *BashTool) IsReadOnly(input json.RawMessage) bool {
	return isReadOnlyCommand(parseBashCommand(input))
}

func (t *BashTool) IsConcurrencySafe(input json.RawMessage) bool {
	return isReadOnlyCommand(parseBashCommand(input))
}

// CheckPermissions Bash 工具始终需要权限确认 (除非 bypass 模式)。
// 对应 TS: BashTool.ts 中的 checkPermissions + bashPermissions.ts
func (t *BashTool) CheckPermissions(input json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "Plan mode: 命令执行工具不可用"}
	}
	return nil
}

var readOnlyCommands = map[string]struct{}{
	"ls":       {},
	"cat":      {},
	"echo":     {},
	"pwd":      {},
	"find":     {},
	"grep":     {},
	"head":     {},
	"tail":     {},
	"wc":       {},
	"which":    {},
	"env":      {},
	"printenv": {},
}

// isReadOnlyCommand 将命令归为只读（用于并发/只读分类）。
// 顺序组合（; && ||）一律视为非只读；管道要求每一段均为只读单命令。
func isReadOnlyCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	if strings.Contains(cmd, "&&") || strings.Contains(cmd, "||") || strings.Contains(cmd, ";") {
		return false
	}
	if strings.Contains(cmd, "|") {
		for _, part := range strings.Split(cmd, "|") {
			if !isReadOnlySingleCommand(strings.TrimSpace(part)) {
				return false
			}
		}
		return true
	}
	return isReadOnlySingleCommand(cmd)
}

func isReadOnlySingleCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	base := filepath.Base(fields[0])
	if _, ok := readOnlyCommands[base]; ok {
		return true
	}
	if base == "git" && len(fields) >= 2 {
		switch fields[1] {
		case "status", "log", "diff":
			return true
		}
	}
	return false
}

func truncateToMaxRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "\n... (output truncated)"
}

// Call 执行 shell 命令。
// 对应 TS: BashTool.ts 中的 call()
func (t *BashTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in bashInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	cwd := tctx.Cwd
	if in.WorkingDirectory != "" {
		cwd = expandPath(in.WorkingDirectory, tctx.Cwd)
	}

	timeout := defaultBashTimeout
	if in.BlockUntilMs > 0 {
		timeout = time.Duration(in.BlockUntilMs) * time.Millisecond
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := sandbox.RunProcessGuarded(cmdCtx, cwd, []string{"bash", "-c", in.Command}, sandbox.ResourceLimits{
		Timeout:         timeout,
		OutputMaxBytes:  4 * 1024 * 1024,
		PreviewMaxBytes: 192 * 1024,
		LogMaxBytes:     16 * 1024 * 1024,
	})
	text := ""
	exitCode := -1
	if result != nil {
		text = truncateToMaxRunes(result.CombinedPreview, maxResultSizeChars)
		exitCode = result.ExitCode
	}
	if err != nil {
		if result != nil && result.FailureKind == sandbox.FailureOutputLimit {
			return &tool.ToolResult{
				Content: fmt.Sprintf("Command stopped: %s\nLogs: %s\n%s", result.FailureKind, result.LogDir, text),
				IsError: true,
			}, nil
		}
		if cmdCtx.Err() == context.DeadlineExceeded || (result != nil && result.FailureKind == sandbox.FailureTimeout) {
			return &tool.ToolResult{
				Content: fmt.Sprintf("Command timed out after %v\n%s", timeout, text),
				IsError: true,
			}, nil
		}
		// FailureSetup (logdir MkdirAll / pipe / Start 失败) 时 ExitCode 保持零值 0,
		// 若先判 exitCode<0 会漏掉它并穿进成功路径 → "Exit code: 0 (no output)"。
		// 必须在 exitCode<0 之前按 err 优先拦截。
		if err != nil && exitCode < 0 || (result != nil && result.FailureKind != sandbox.FailureNone && result.FailureKind != sandbox.FailureOutputLimit && result.FailureKind != sandbox.FailureTimeout) {
			return &tool.ToolResult{
				Content: fmt.Sprintf("命令执行失败: %v\n%s", err, text),
				IsError: true,
			}, nil
		}
	}

	if exitCode != 0 {
		return &tool.ToolResult{
			Content: fmt.Sprintf("Exit code: %d\n\n%s", exitCode, text),
			IsError: true,
		}, nil
	}

	if text == "" {
		text = "(no output)"
	}

	return &tool.ToolResult{Content: fmt.Sprintf("Exit code: 0\n\n%s", text)}, nil
}
