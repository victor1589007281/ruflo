package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

// TestExitPlanModePersistsPlanFile 验证 ExitPlanMode 把计划落盘到 PlanFileDir,
// 返回结果里带路径, 且 plan flag 被清空 (实施阶段放行)。
func TestExitPlanModePersistsPlanFile(t *testing.T) {
	dir := t.TempDir()
	planText := "1. 调研\n2. 实现\n3. 验证"

	SetPlanModeForSession("planfile-test", true)
	defer SetPlanModeForSession("planfile-test", false)

	tctx := &tool.ToolContext{SessionID: "planfile-test", PlanFileDir: dir}
	input, _ := json.Marshal(exitPlanModeInput{Plan: planText})
	res, err := NewExitPlanModeTool().Call(nil, input, tctx)
	if err != nil {
		t.Fatalf("ExitPlanMode Call: %v", err)
	}
	if PlanModeActiveForSession("planfile-test") {
		t.Fatal("ExitPlanMode 后会话 plan flag 应已清空")
	}
	if !strings.Contains(res.Content, "已退出计划模式") {
		t.Fatalf("结果缺少退出提示: %s", res.Content)
	}
	// 返回内容应带落盘路径
	idx := strings.Index(res.Content, "计划已保存至: ")
	if idx < 0 {
		t.Fatalf("结果缺少保存路径: %s", res.Content)
	}
	path := strings.Fields(strings.TrimSpace(res.Content[idx+len("计划已保存至: "):]))[0]
	if !strings.HasPrefix(path, dir) {
		t.Fatalf("保存路径不在 PlanFileDir 下: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取计划文件: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, planText) {
		t.Fatalf("计划文件内容缺少计划文本:\n%s", content)
	}
	if !strings.Contains(content, "# 实施计划") {
		t.Fatalf("计划文件应含标题:\n%s", content)
	}
}

// TestExitPlanModeNoPlanFileDir 验证未配置 PlanFileDir 时不落盘、仅回显。
func TestExitPlanModeNoPlanFileDir(t *testing.T) {
	SetPlanModeForSession("planfile-no", true)
	defer SetPlanModeForSession("planfile-no", false)
	tctx := &tool.ToolContext{SessionID: "planfile-no"} // 无 PlanFileDir
	res, err := NewExitPlanModeTool().Call(nil, json.RawMessage(`{"plan":"plan text"}`), tctx)
	if err != nil {
		t.Fatalf("ExitPlanMode Call: %v", err)
	}
	if strings.Contains(res.Content, "计划已保存至") {
		t.Fatalf("未配置 PlanFileDir 不应落盘: %s", res.Content)
	}
	if !strings.Contains(res.Content, "plan text") {
		t.Fatalf("应回显计划文本: %s", res.Content)
	}
}

// TestPersistPlanFileCreatesDir 验证目录不存在时自动创建。
func TestPersistPlanFileCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "plans")
	path, err := persistPlanFile(dir, "abc-123", "计划内容")
	if err != nil {
		t.Fatalf("persistPlanFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("计划文件不存在: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("路径应在目录下: %s", path)
	}
}

// TestSanitizePlanName 验证会话名清理。
func TestSanitizePlanName(t *testing.T) {
	cases := map[string]string{
		"chat-abc_1": "chat-abc_1",
		"oc_xxx:abc": "oc_xxx_abc",
		"":           "global",
	}
	for in, want := range cases {
		if got := sanitizePlanName(in); got != want {
			t.Fatalf("sanitizePlanName(%q)=%q want %q", in, got, want)
		}
	}
}
