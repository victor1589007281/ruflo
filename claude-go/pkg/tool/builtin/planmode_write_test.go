package builtin

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestPlanModePlanDirWriteException 规划期唯一可写例外: 计划文件目录 (PlanFileDir)。
//
// 用户指出 "plan mode 不能写" 不准确 —— 对齐 Claude 客户端 plan mode 语义:
// 只读强制, 但**计划文件目录内允许 Write/Edit** (模型在此起草/更新计划);
// 目录外一律拒写, `../` 逃逸与未配置 PlanFileDir 时全拒。非 plan 模式行为不变。
func TestPlanModePlanDirWriteException(t *testing.T) {
	planDir := filepath.Join(t.TempDir(), "plans")
	outside := filepath.Join(t.TempDir(), "x.txt")

	write := NewFileWriteTool()
	edit := NewFileEditTool()

	planTctx := &tool.ToolContext{
		Cwd:            t.TempDir(),
		PermissionMode: types.PermissionModePlan,
		PlanFileDir:    planDir,
	}

	// 计划文件目录内 → 放行
	inside := filepath.Join(planDir, "draft.md")
	if r := write.CheckPermissions(json.RawMessage(`{"path":"`+inside+`","contents":"x"}`), planTctx); r != nil {
		t.Fatalf("plan 目录内 Write 应放行, got %+v", r)
	}
	if r := edit.CheckPermissions(json.RawMessage(`{"path":"`+inside+`","old_string":"a","new_string":"b"}`), planTctx); r != nil {
		t.Fatalf("plan 目录内 Edit 应放行, got %+v", r)
	}

	// 目录外 → 拒
	if r := write.CheckPermissions(json.RawMessage(`{"path":"`+outside+`","contents":"x"}`), planTctx); r == nil {
		t.Fatal("plan 目录外 Write 应被拒")
	}
	if r := edit.CheckPermissions(json.RawMessage(`{"path":"`+outside+`","old_string":"a","new_string":"b"}`), planTctx); r == nil {
		t.Fatal("plan 目录外 Edit 应被拒")
	}

	// ../ 逃逸 (目标在 plan 目录父级) → 拒
	escape := filepath.Join(planDir, "..", "escape.md")
	if r := write.CheckPermissions(json.RawMessage(`{"path":"`+escape+`","contents":"x"}`), planTctx); r == nil {
		t.Fatal(".. 逃逸 Write 应被拒")
	}

	// PlanFileDir 未配置 → 无例外, 全拒
	noPlanDir := &tool.ToolContext{Cwd: t.TempDir(), PermissionMode: types.PermissionModePlan}
	if r := write.CheckPermissions(json.RawMessage(`{"path":"`+inside+`","contents":"x"}`), noPlanDir); r == nil {
		t.Fatal("PlanFileDir 未配置时 Write 应被拒")
	}

	// 非 plan 模式 → 行为不变 (放行)
	normal := &tool.ToolContext{Cwd: t.TempDir(), PermissionMode: types.PermissionModeAcceptEdits}
	if r := write.CheckPermissions(json.RawMessage(`{"path":"`+outside+`","contents":"x"}`), normal); r != nil {
		t.Fatalf("非 plan 模式 Write 应放行, got %+v", r)
	}
}
