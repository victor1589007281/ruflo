// grep.go 实现 Grep 工具 (只读/并发安全)。
// 对应 TS 源码: review/claude/src/tools/GrepTool/GrepTool.ts
//
// 功能:
//   - 正则表达式搜索文件内容
//   - 基于 ripgrep (rg) 实现
//   - 支持文件类型过滤
//   - 支持 glob 过滤
//   - 支持上下文行 (before_context / after_context / context)
//   - 多种输出模式 (content, files_with_matches, count)
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const GrepToolName = "Grep"

const defaultGrepHeadLimit = 250

type grepInput struct {
	Pattern           string `json:"pattern"`
	Path              string `json:"path,omitempty"`
	Glob              string `json:"glob,omitempty"`
	Type              string `json:"type,omitempty"`
	OutputMode        string `json:"output_mode,omitempty"` // content, files_with_matches, count; default files_with_matches
	CaseInsensitive   bool   `json:"case_insensitive,omitempty"`
	BeforeContext     int    `json:"before_context,omitempty"`
	AfterContext      int    `json:"after_context,omitempty"`
	Context           int    `json:"context,omitempty"`
	Multiline         bool   `json:"multiline,omitempty"`
	HeadLimit         *int   `json:"head_limit,omitempty"` // nil → default 250; 0 → unlimited
	Offset            *int   `json:"offset,omitempty"`
}

type GrepTool struct{}

func NewGrepTool() *GrepTool { return &GrepTool{} }

func (t *GrepTool) Name() string { return GrepToolName }

func (t *GrepTool) Description() string {
	return `A powerful search tool built on ripgrep. Supports full regex syntax.`
}

func (t *GrepTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "The regular expression pattern to search for."},
			"path": {"type": "string", "description": "File or directory to search in."},
			"glob": {"type": "string", "description": "Glob pattern to filter files."},
			"type": {"type": "string", "description": "File type to search (e.g. js, py, go)."},
			"output_mode": {"type": "string", "enum": ["content", "files_with_matches", "count"], "description": "content, files_with_matches, or count. Defaults to files_with_matches."},
			"case_insensitive": {"type": "boolean", "description": "Case insensitive search (rg -i)."},
			"before_context": {"type": "integer", "description": "Lines to show before each match (rg -B). Content mode only."},
			"after_context": {"type": "integer", "description": "Lines to show after each match (rg -A). Content mode only."},
			"context": {"type": "integer", "description": "Lines before and after each match (rg -C). Content mode only."},
			"multiline": {"type": "boolean", "description": "Enable multiline mode."},
			"head_limit": {"type": "integer", "description": "Max output lines/entries. Omit for default 250; 0 means unlimited."},
			"offset": {"type": "integer", "description": "Skip first N lines/entries before applying head_limit."}
		},
		"required": ["pattern"]
	}`)
}

func (t *GrepTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (t *GrepTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *GrepTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func grepEffectiveOffset(in *int) int {
	if in == nil || *in < 0 {
		return 0
	}
	return *in
}

func grepApplyLineLimit(s string, head *int, offset int) (out string, truncated bool) {
	if s == "" {
		return "", false
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	off := offset
	if off > len(lines) {
		off = len(lines)
	}
	rest := lines[off:]

	var limit int
	unlimited := false
	if head == nil {
		limit = defaultGrepHeadLimit
	} else if *head == 0 {
		unlimited = true
	} else {
		limit = *head
	}

	if unlimited {
		return strings.Join(rest, "\n"), false
	}
	if len(rest) > limit {
		truncated = true
		rest = rest[:limit]
	}
	out = strings.Join(rest, "\n")
	if out != "" {
		out += "\n"
	}
	return out, truncated
}

// Call 执行 grep 搜索。
// 对应 TS: GrepTool.ts 中的 call()
//
// 底层调用 ripgrep (rg) 命令，构造参数列表后执行。
// 如果 rg 不可用则回退到 grep。
func (t *GrepTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in grepInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	searchPath := tctx.Cwd
	if in.Path != "" {
		searchPath = expandPath(in.Path, tctx.Cwd)
	}

	mode := in.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}

	// 构建 rg 参数
	args := []string{"--color=never", "--no-heading"}

	if in.CaseInsensitive {
		args = append(args, "-i")
	}
	if in.Multiline {
		args = append(args, "-U", "--multiline-dotall")
	}

	switch mode {
	case "files_with_matches":
		args = append(args, "-l")
	case "count":
		args = append(args, "-c")
	default:
		args = append(args, "-n") // 行号
	}

	if mode == "content" {
		if in.Context > 0 {
			args = append(args, fmt.Sprintf("-C%d", in.Context))
		} else {
			if in.BeforeContext > 0 {
				args = append(args, fmt.Sprintf("-B%d", in.BeforeContext))
			}
			if in.AfterContext > 0 {
				args = append(args, fmt.Sprintf("-A%d", in.AfterContext))
			}
		}
	}

	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	if in.Type != "" {
		args = append(args, "--type", in.Type)
	}

	if strings.HasPrefix(in.Pattern, "-") {
		args = append(args, "-e", in.Pattern)
	} else {
		args = append(args, in.Pattern)
	}
	args = append(args, "--", searchPath)

	cmd := exec.CommandContext(ctx, "rg", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	output := stdout.String()
	if output == "" && err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return &tool.ToolResult{Content: "No matches found."}, nil
		}
		errMsg := stderr.String()
		if errMsg == "" {
			errMsg = err.Error()
		}
		return &tool.ToolResult{Content: fmt.Sprintf("搜索错误: %s", errMsg), IsError: true}, nil
	}

	off := grepEffectiveOffset(in.Offset)
	output, _ = grepApplyLineLimit(output, in.HeadLimit, off)

	const maxOutputLen = 50000
	if len(output) > maxOutputLen {
		output = output[:maxOutputLen] + "\n... (results truncated)"
	}

	return &tool.ToolResult{Content: output}, nil
}
