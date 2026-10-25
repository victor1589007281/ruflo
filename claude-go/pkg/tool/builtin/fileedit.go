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
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
	// old_str/new_str 是常见的别名写法。缺字段时 OldString 为空, 而
	// strings.Count(content, "") = len+1 —— replace_all 会变成"在每个字符间
	// 插入新串"; new_string 缺失则会把匹配到的原文删掉。两者都必须先校验。
	OldStr     string `json:"old_str"`
	NewStr     string `json:"new_str"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type FileEditTool struct{}

// NewFileEditTool 构造 FileEdit 工具实例 (无状态, 可直接复用)。
func NewFileEditTool() *FileEditTool { return &FileEditTool{} }

// Name 返回工具名 "StrReplace"。
func (t *FileEditTool) Name() string { return FileEditToolName }

// Description 返回给模型看的工具说明; 强调 old_string 必须唯一, 否则调用失败。
func (t *FileEditTool) Description() string {
	return `Performs exact string replacements in files.
The edit will FAIL if old_string is not unique in the file.
Use replace_all for replacing all occurrences of old_string.`
}

// InputSchema 返回 JSON Schema; required 里不含 replace_all, 默认按唯一匹配处理。
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

// IsReadOnly 恒为 false —— 本工具会写文件。
func (t *FileEditTool) IsReadOnly(_ json.RawMessage) bool { return false }

// IsConcurrencySafe 恒为 false —— 并发编辑同一文件会互相覆盖, 由 tctx.ClaimWrite 在调用期兜底。
func (t *FileEditTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// CheckPermissions 规划模式 (Plan) 下默认拒绝写操作, 唯一例外是计划文件目录内的路径。
// 返回 nil 表示不干预, 交给后续权限链继续判定。
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
//  1. 读取文件全文
//  2. 计算 old_string 出现次数
//  3. 非 replace_all 模式下要求恰好 1 次匹配
//  4. 执行替换
//  5. 原子写回文件
func (t *FileEditTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in fileEditInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if in.OldString == "" && in.OldStr != "" {
		in.OldString = in.OldStr
	}
	if in.NewString == "" && in.NewStr != "" {
		in.NewString = in.NewStr
	}
	// 缺字段与"显式空串"必须区分: 缺 new_string 会把匹配段删掉, 缺 old_string
	// 在 replace_all 下会变成逐字符插入。两者都是静默数据破坏。
	var raw map[string]json.RawMessage
	if json.Unmarshal(input, &raw) == nil {
		_, hasOld := raw["old_string"]
		_, hasOldAlias := raw["old_str"]
		_, hasNew := raw["new_string"]
		_, hasNewAlias := raw["new_str"]
		if !hasOld && !hasOldAlias {
			return &tool.ToolResult{Content: "编辑被拒绝: 缺少 old_string 字段 (别名为 old_str)。", IsError: true}, nil
		}
		if !hasNew && !hasNewAlias {
			return &tool.ToolResult{Content: "编辑被拒绝: 缺少 new_string 字段 (别名为 new_str); 缺省会把匹配段删除。", IsError: true}, nil
		}
	}

	filePath := expandPath(in.Path, tctx.Cwd)

	// 写入根限制 (opt-in): 与 FileWrite 同一条判据, 拦在入口。
	if !tctx.PathInWriteRoot(filePath) {
		return &tool.ToolResult{
			Content: fmt.Sprintf("写入被拒绝: 本次运行的写入范围限定在 %s, 而目标 %s 不在其中。请改用相对路径写到当前工作目录。", tctx.WriteRoot, filePath),
			IsError: true,
		}, nil
	}

	// H3-4 并发写闸。位置在**读文件之前**: 两个并行子代理各自读到旧内容、各自基于
	// 那一份算新内容, 正是覆盖的成因 —— 读到一半才拦, 另一方已经按过期内容构造好了
	// 它的写入。拦在入口, 冲突的一侧连读都不必发生, 报错也就干净。
	if err := tctx.ClaimWrite(filePath); err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

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

	// 文件级检查点: 留存写前内容。位置在唯一性校验与语法护栏之后 ——
	// 那些分支不改动文件, 不该产生快照。
	snapshotRef := checkpointBeforeWrite(filePath, tctx)

	if err := os.WriteFile(filePath, []byte(newContent), 0o644); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("写入文件失败: %v", err), IsError: true}, nil
	}
	checkpointAfterWrite(filePath, tctx)

	replacements := count
	if !in.ReplaceAll {
		replacements = 1
	}
	msg := fmt.Sprintf("Successfully replaced %d occurrence(s) in %s", replacements, filePath)
	if snapshotRef != "" {
		msg += fmt.Sprintf("\n[file-history] 写前版本: %s", snapshotRef)
	}
	return &tool.ToolResult{Content: msg}, nil
}
