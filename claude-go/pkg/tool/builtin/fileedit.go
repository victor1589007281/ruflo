// fileedit.go 实现 FileEdit (StrReplace) 工具。
// 对应 TS 源码: review/claude/src/tools/FileEditTool/FileEditTool.ts
//
// 功能:
//   - 精确字符串替换 (old_string → new_string)
//   - old_string 必须在文件中唯一匹配 (防止误改)
//   - 支持 replace_all 模式 (替换所有出现)
//   - 保留原始缩进和空白
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const FileEditToolName = "StrReplace"

type fileEditInput struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type FileEditTool struct{}

func NewFileEditTool() *FileEditTool { return &FileEditTool{} }

func (t *FileEditTool) Name() string { return FileEditToolName }

func (t *FileEditTool) Description() string {
	return `Performs exact string replacements in files.
The edit will FAIL if old_string is not unique in the file.
Use replace_all for replacing all occurrences of old_string.`
}

func (t *FileEditTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "The absolute path to the file to modify."},
			"old_string": {"type": "string", "description": "The text to replace (must be different from new_string)."},
			"new_string": {"type": "string", "description": "The text to replace it with."},
			"replace_all": {"type": "boolean", "description": "Replace all occurrences (default false)."}
		},
		"required": ["path", "old_string", "new_string"]
	}`)
}

func (t *FileEditTool) IsReadOnly(_ json.RawMessage) bool { return false }
func (t *FileEditTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *FileEditTool) CheckPermissions(input json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx == nil || tctx.PermissionMode != types.PermissionModePlan {
		return nil
	}
	// 规划期唯一可写例外: 计划文件目录 (PlanFileDir), 对齐 Claude 客户端 plans 目录语义。
	if tctx.PlanFileDir != "" {
		var in fileEditInput
		if json.Unmarshal(input, &in) == nil && in.Path != "" {
			if tctx.PathInPlanDir(expandPath(in.Path, tctx.Cwd)) {
				return nil
			}
		}
	}
	return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "Plan mode: 编辑操作不可用 (唯一例外: 计划文件目录)"}
}

// Call 执行字符串替换。
// 对应 TS: FileEditTool.ts 中的 call()
//
// 算法:
//   1. 读取文件全文
//   2. 计算 old_string 出现次数
//   3. 非 replace_all 模式下要求恰好 1 次匹配
//   4. 执行替换
//   5. 原子写回文件
func (t *FileEditTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in fileEditInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	filePath := expandPath(in.Path, tctx.Cwd)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("读取文件失败: %v", err), IsError: true}, nil
	}

	content := string(data)
	count := strings.Count(content, in.OldString)

	if count == 0 {
		return &tool.ToolResult{
			Content: fmt.Sprintf("old_string 在文件中未找到。请确保 old_string 与文件内容完全匹配（包括空白和缩进）。"),
			IsError: true,
		}, nil
	}

	if !in.ReplaceAll && count > 1 {
		return &tool.ToolResult{
			Content: fmt.Sprintf("old_string 在文件中出现了 %d 次，请提供更多上下文使其唯一，或使用 replace_all=true。", count),
			IsError: true,
		}, nil
	}

	var newContent string
	if in.ReplaceAll {
		newContent = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		newContent = strings.Replace(content, in.OldString, in.NewString, 1)
	}

	// 方案三 L2 护栏: 写入前语法校验 (弱模型模式), 坏 diff 拒写并回注错误
	if res := guardWriteSyntax(filePath, newContent, tctx); res != nil {
		return res, nil
	}

	if err := os.WriteFile(filePath, []byte(newContent), 0o644); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("写入文件失败: %v", err), IsError: true}, nil
	}

	replacements := count
	if !in.ReplaceAll {
		replacements = 1
	}
	return &tool.ToolResult{Content: fmt.Sprintf("Successfully replaced %d occurrence(s) in %s", replacements, filePath)}, nil
}
