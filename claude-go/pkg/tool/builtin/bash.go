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

// NewBashTool 构造 Bash 工具实例 (无状态, 可安全共享)。
func NewBashTool() *BashTool { return &BashTool{} }

// Name 返回工具对外名 (常量 BashToolName, 见其注释里的改名由来)。
func (t *BashTool) Name() string { return BashToolName }

// Aliases 保留 2026-09-13 之前的旧名, 供治理配置与历史会话状态继续按 "Shell" 引用它。
func (t *BashTool) Aliases() []string { return []string{"Shell"} }

// Description 返回给模型看的工具说明: 优先用专用工具、禁止交互式命令、输出上限 30000 字符。
func (t *BashTool) Description() string {
	// 描述里不用反引号 —— 整个字符串是 raw literal, 反引号会把它截断。
	return `Executes a given command in a shell session.
- Pass the command as the "command" field; use "working_directory" to run it somewhere other than the default cwd (it does not persist between calls).
- Prefer the dedicated tools when one applies (Read / Write / StrReplace / Glob / Grep) — they are permission-checked and render better than cat/sed/echo/find/grep.
- Never start commands that wait for interactive input: they hang until the timeout.
- The tool blocks up to "block_until_ms" (default 30000) and returns what has been collected so far; output beyond 30000 characters is truncated with a note.`
}

// InputSchema 返回入参 JSON Schema (command 必填, 其余可选)。
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

// parseBashCommand 从原始入参里抠出 command 字段; 空入参或解析失败一律返回空串。
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

// IsReadOnly 只读判定的对外形状: 命令整条被判为只读时才报只读。
func (t *BashTool) IsReadOnly(input json.RawMessage) bool {
	return isReadOnlyCommand(parseBashCommand(input))
}

// IsConcurrencySafe 与 IsReadOnly 同判据 —— 只有被证明只读的命令才允许并发执行。
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

// isReadOnlySingleCommand 判断单条命令 (无管道、无连接符) 是否只读: 取 basename 比对
// 白名单, 并额外放行 git status/log/diff 三个只读子命令。
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

// truncateToMaxRunes 按**字符**数 (非字节) 截断, 超限时追加截断提示; maxRunes<=0 表示不截断。
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
//
// 算法解读：
//
//	原理：把命令整条交给 sandbox.RunProcessGuarded 在独立进程组里跑, 由它统一施加
//	      超时、输出上限与整组回收; 本函数只做入参解析、失败分类与回给模型的文案拼装。
//	关键不变量：失败分类必须**先于** exitCode 判断 —— FailureSetup (logdir/pipe/Start
//	      失败) 时 ExitCode 保持零值 0, 若先判 exitCode<0 会把它漏进成功路径, 报出
//	      "Exit code: 0 (no output)" 这种假成功 (见下方注释)。
//	代价与边界：回给模型的文本按 rune 截断到 30000 字符; 截断只作用于文案, 不影响
//	      退出码判定, 也不影响 sandbox 侧的完整日志 (LogDir)。
//	设计权衡：超时同时挂在 ctx 与 ResourceLimits 两处 —— ctx 让本函数及时返回并走
//	      超时文案分支, ResourceLimits.Timeout 才是真正 SIGKILL 整个进程组的那道闸,
//	      只挂 ctx 会留下孤儿进程。
func (t *BashTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in bashInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	// 工作目录: 默认用调用方给的 cwd, 显式指定时才相对它展开 (不跨调用持久化)。
	cwd := tctx.Cwd
	if in.WorkingDirectory != "" {
		cwd = expandPath(in.WorkingDirectory, tctx.Cwd)
	}

	// 目标合并闸: 同一份 PDF/URL 被拆成多条命令重复访问时直接拒绝, 把"合并成
	// 一条命令"作为 tool_result 回给模型。位置在**执行之前** —— 拒绝的意义就是
	// 不产生任何 I/O 与新的工具输出。
	if tctx != nil && tctx.BashMerge != nil {
		if reason := tctx.BashMerge.CheckAndRecord(cwd, in.Command); reason != "" {
			return &tool.ToolResult{Content: reason, IsError: true}, nil
		}
	}

	// block_until_ms 只放宽等待上限, 不给"无上限"留口子 —— 不传就是默认 30s。
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
	// exitCode 预置 -1 而不是 0: 用来区分"进程没起来"(result 为 nil / 无退出码) 与"退出了但码非零"。
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

	// 只在成功路径补 "(no output)": 失败路径的文案里已经带了退出码/错误, 再补反而淹没原因。
	if text == "" {
		text = "(no output)"
	}

	return &tool.ToolResult{Content: fmt.Sprintf("Exit code: 0\n\n%s", text)}, nil
}
