// filewrite.go 实现 FileWrite 工具 (写入/非并发安全)。
// 对应 TS 源码: review/claude/src/tools/FileWriteTool/FileWriteTool.ts
//
// 功能:
//   - 将内容写入指定路径 (覆盖或创建新文件)
//   - 自动创建父目录
//   - 返回写入的字节数
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const FileWriteToolName = "Write"

type fileWriteInput struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

type FileWriteTool struct{}

func NewFileWriteTool() *FileWriteTool { return &FileWriteTool{} }

func (t *FileWriteTool) Name() string { return FileWriteToolName }

func (t *FileWriteTool) Description() string {
	return `Writes a file to the local filesystem. This tool will overwrite the existing file if there is one at the provided path.`
}

func (t *FileWriteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "The absolute path to the file to write."},
			"contents": {"type": "string", "description": "The contents to write to the file."}
		},
		"required": ["path", "contents"]
	}`)
}

func (t *FileWriteTool) IsReadOnly(_ json.RawMessage) bool { return false }
func (t *FileWriteTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *FileWriteTool) CheckPermissions(input json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx == nil || tctx.PermissionMode != types.PermissionModePlan {
		return nil
	}
	// 规划期唯一可写例外: 计划文件目录 (PlanFileDir)。模型可在此起草/更新计划,
	// 对齐 Claude 客户端 plan mode 只放行 plans 目录的语义; 其余路径一律拒写。
	if tctx.PlanFileDir != "" {
		var in fileWriteInput
		if json.Unmarshal(input, &in) == nil && in.Path != "" {
			if tctx.PathInPlanDir(expandPath(in.Path, tctx.Cwd)) {
				return nil
			}
		}
	}
	return &types.PermissionResult{
		Behavior: types.PermissionDeny,
		Reason:   "Plan mode: 写入操作不可用 (唯一例外: 计划文件目录)",
	}
}

// Call 执行文件写入。
// 对应 TS: FileWriteTool.ts 中的 call()
func (t *FileWriteTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in fileWriteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	filePath := expandPath(in.Path, tctx.Cwd)

	// 方案三 L2 护栏: 写入前语法校验 (弱模型模式), 坏内容拒写并回注错误
	if res := guardWriteSyntax(filePath, in.Contents, tctx); res != nil {
		return res, nil
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("创建目录失败: %v", err), IsError: true}, nil
	}

	if err := os.WriteFile(filePath, []byte(in.Contents), 0o644); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("写入文件失败: %v", err), IsError: true}, nil
	}

	return &tool.ToolResult{Content: fmt.Sprintf("Successfully wrote %d bytes to %s", len(in.Contents), filePath)}, nil
}
