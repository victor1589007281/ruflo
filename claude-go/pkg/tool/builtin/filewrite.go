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

// NewFileWriteTool 创建 FileWrite 工具实例 (无状态, 每次调用都返回新对象)。
func NewFileWriteTool() *FileWriteTool { return &FileWriteTool{} }

// Name 返回工具名 "Write" (与权限规则、acceptEdits 的文件编辑工具名单对齐)。
func (t *FileWriteTool) Name() string { return FileWriteToolName }

// Description 返回给模型看的工具说明; 强调**覆盖语义** —— 路径上已有文件会被直接覆盖。
func (t *FileWriteTool) Description() string {
	return `Writes a file to the local filesystem. This tool will overwrite the existing file if there is one at the provided path.`
}

// InputSchema 返回入参 JSON Schema (path 与 contents 均必填)。
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

// IsReadOnly 恒为 false —— 本工具会写文件。
func (t *FileWriteTool) IsReadOnly(_ json.RawMessage) bool        { return false }

// IsConcurrencySafe 恒为 false —— 并行写同一路径会互相覆盖 (并发保护另见 Call 里的 ClaimWrite)。
func (t *FileWriteTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// CheckPermissions 只在 Plan 模式下表态: 除计划文件目录外一律拒绝写入, 其余模式返回
// nil 交给全局权限链裁决。
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

	// H3-4 并发写闸: 同一个文件同时被两个并行子代理写 ⇒ 显式拒绝, 不静默覆盖。
	// 位置在所有别的守卫**之前**: 这是一条关于"谁有权动这个文件"的判据, 与内容是否
	// 合法无关 —— 一份语法完全正确的覆盖同样是数据丢失, 先做语法校验会让坏内容被拒、
	// 好内容被放行, 而覆盖这件事本身才是要挡的。
	if err := tctx.ClaimWrite(filePath); err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	// 方案三 L2 护栏: 写入前语法校验 (弱模型模式), 坏内容拒写并回注错误
	if res := guardWriteSyntax(filePath, in.Contents, tctx); res != nil {
		return res, nil
	}

	// 文件级检查点: 写入前记下当前修订, 写入后再记新修订 ——
	// 于是最高版本恒等于磁盘内容, "回退一步"即恢复上一个修订。
	// 位置在语法护栏等全部拒写分支之后: 被拒的写入没有产生新修订。
	snapshotRef := checkpointBeforeWrite(filePath, tctx)

	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("创建目录失败: %v", err), IsError: true}, nil
	}

	if err := os.WriteFile(filePath, []byte(in.Contents), 0o644); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("写入文件失败: %v", err), IsError: true}, nil
	}
	checkpointAfterWrite(filePath, tctx)

	msg := fmt.Sprintf("Successfully wrote %d bytes to %s", len(in.Contents), filePath)
	if snapshotRef != "" {
		msg += fmt.Sprintf("\n[file-history] 写前版本: %s", snapshotRef)
	}
	return &tool.ToolResult{Content: msg}, nil
}
