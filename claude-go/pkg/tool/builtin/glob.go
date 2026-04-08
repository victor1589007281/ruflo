// glob.go 实现 Glob 工具 (只读/并发安全)。
// 对应 TS 源码: review/claude/src/tools/GlobTool/GlobTool.ts
//
// 功能:
//   - 按 glob 模式搜索文件 (含 ** 递归语义)
//   - 若模式不以 **/ 开头则自动添加 (与 TS 行为一致)
//   - 按修改时间排序结果
//   - 返回匹配的文件路径列表
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const GlobToolName = "Glob"

const defaultGlobMaxResults = 100

type globInput struct {
	Pattern         string `json:"glob_pattern"`
	TargetDirectory string `json:"target_directory,omitempty"`
	Limit           int    `json:"limit,omitempty"`
}

type GlobTool struct{}

func NewGlobTool() *GlobTool { return &GlobTool{} }

func (t *GlobTool) Name() string { return GlobToolName }

func (t *GlobTool) Description() string {
	return `Tool to search for files matching a glob pattern. Returns matching file paths sorted by modification time.`
}

func (t *GlobTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"glob_pattern": {"type": "string", "description": "The glob pattern to match files against (supports **)."},
			"target_directory": {"type": "string", "description": "Absolute path to directory to search in."},
			"limit": {"type": "integer", "description": "Maximum number of paths to return. Defaults to 100."}
		},
		"required": ["glob_pattern"]
	}`)
}

func (t *GlobTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (t *GlobTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *GlobTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 执行 glob 搜索。
// 对应 TS: GlobTool.ts 中的 call()
//
// 使用 doublestar 匹配相对路径以支持 ** 语义。
func (t *GlobTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in globInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	rootDir := tctx.Cwd
	if in.TargetDirectory != "" {
		rootDir = expandPath(in.TargetDirectory, tctx.Cwd)
	}

	maxResults := in.Limit
	if maxResults <= 0 {
		maxResults = defaultGlobMaxResults
	}

	pattern := in.Pattern
	if !strings.HasPrefix(pattern, "**/") {
		pattern = "**/" + pattern
	}
	pattern = filepath.ToSlash(pattern)

	type fileEntry struct {
		path    string
		modTime int64
	}

	var matches []fileEntry

	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fs.SkipAll
		default:
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(rootDir, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		ok, err := doublestar.PathMatch(pattern, relSlash)
		if err != nil || !ok {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		matches = append(matches, fileEntry{
			path:    path,
			modTime: info.ModTime().UnixNano(),
		})
		return nil
	})

	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("搜索失败: %v", err), IsError: true}, nil
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].modTime > matches[j].modTime
	})

	truncated := len(matches) > maxResults
	if truncated {
		matches = matches[:maxResults]
	}

	if len(matches) == 0 {
		return &tool.ToolResult{Content: fmt.Sprintf("No files found matching pattern: %s", in.Pattern)}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d file(s)", len(matches)))
	if truncated {
		sb.WriteString(" (results capped at limit; use a narrower pattern or raise limit)")
	}
	sb.WriteString(":\n")
	for _, m := range matches {
		sb.WriteString(m.path + "\n")
	}
	return &tool.ToolResult{Content: sb.String()}, nil
}
