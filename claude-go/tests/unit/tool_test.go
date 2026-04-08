// 工具系统单元测试
// 测试: 工具注册、分区算法、工具执行
package unit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestToolRegistry 测试工具注册表基本操作
func TestToolRegistry(t *testing.T) {
	reg := tool.NewRegistry()

	// 注册内置工具
	todoTool := builtin.RegisterBaseTools(reg)
	_ = todoTool

	// 验证工具数量
	if reg.Count() < 7 {
		t.Errorf("期望至少 7 个内置工具, 实际 %d", reg.Count())
	}

	// 验证工具查找
	readTool, ok := reg.Get("Read")
	if !ok || readTool == nil {
		t.Fatal("Read 工具未找到")
	}
	if readTool.Name() != "Read" {
		t.Errorf("期望工具名 Read, 实际 %s", readTool.Name())
	}

	// 验证只读属性
	if !readTool.IsReadOnly(nil) {
		t.Error("Read 工具应该是只读的")
	}

	writeTool, _ := reg.Get("Write")
	if writeTool.IsReadOnly(nil) {
		t.Error("Write 工具不应该是只读的")
	}

	// 测试工具名列表
	names := reg.Names()
	if len(names) != reg.Count() {
		t.Errorf("Names() 长度 %d != Count() %d", len(names), reg.Count())
	}

	// 测试移除
	reg.Remove("TodoWrite")
	if _, ok := reg.Get("TodoWrite"); ok {
		t.Error("TodoWrite 应已被移除")
	}
}

// TestPartitionToolCalls 测试工具调用分区算法
// 对应 TS: toolOrchestration.ts 中的 partitionToolCalls
func TestPartitionToolCalls(t *testing.T) {
	reg := tool.NewRegistry()
	builtin.RegisterBaseTools(reg)

	// 构造混合工具调用: [Read, Grep, Write, Read, Read]
	blocks := []types.ContentBlock{
		{Type: types.ContentBlockToolUse, Name: "Read", ID: "1", Input: json.RawMessage(`{"path":"/tmp/a"}`)},
		{Type: types.ContentBlockToolUse, Name: "Grep", ID: "2", Input: json.RawMessage(`{"pattern":"foo"}`)},
		{Type: types.ContentBlockToolUse, Name: "Write", ID: "3", Input: json.RawMessage(`{"path":"/tmp/b","contents":"x"}`)},
		{Type: types.ContentBlockToolUse, Name: "Read", ID: "4", Input: json.RawMessage(`{"path":"/tmp/c"}`)},
		{Type: types.ContentBlockToolUse, Name: "Read", ID: "5", Input: json.RawMessage(`{"path":"/tmp/d"}`)},
	}

	batches := tool.PartitionToolCalls(blocks, reg)

	// 期望 3 个批次:
	// Batch 1: [Read, Grep] - concurrent safe
	// Batch 2: [Write] - serial
	// Batch 3: [Read, Read] - concurrent safe
	if len(batches) != 3 {
		t.Fatalf("期望 3 个批次, 实际 %d", len(batches))
	}

	if !batches[0].IsConcurrencySafe || len(batches[0].Blocks) != 2 {
		t.Errorf("Batch 0: 期望 concurrent 2 块, 实际 concurrent=%v len=%d",
			batches[0].IsConcurrencySafe, len(batches[0].Blocks))
	}

	if batches[1].IsConcurrencySafe || len(batches[1].Blocks) != 1 {
		t.Errorf("Batch 1: 期望 serial 1 块, 实际 concurrent=%v len=%d",
			batches[1].IsConcurrencySafe, len(batches[1].Blocks))
	}

	if !batches[2].IsConcurrencySafe || len(batches[2].Blocks) != 2 {
		t.Errorf("Batch 2: 期望 concurrent 2 块, 实际 concurrent=%v len=%d",
			batches[2].IsConcurrencySafe, len(batches[2].Blocks))
	}
}

// TestFileReadTool 测试文件读取工具
func TestFileReadTool(t *testing.T) {
	// 创建临时文件
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	readTool := builtin.NewFileReadTool()
	ctx := context.Background()
	tctx := &tool.ToolContext{Cwd: dir}

	// 测试完整读取
	input := json.RawMessage(`{"path":"` + testFile + `"}`)
	result, err := readTool.Call(ctx, input, tctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("不期望错误: %s", result.Content)
	}
	if !contains(result.Content, "line1") || !contains(result.Content, "line5") {
		t.Errorf("读取内容不完整: %s", result.Content)
	}

	// 测试 offset + limit
	input = json.RawMessage(`{"path":"` + testFile + `", "offset": 2, "limit": 2}`)
	result, err = readTool.Call(ctx, input, tctx)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(result.Content, "line2") || !contains(result.Content, "line3") {
		t.Errorf("offset/limit 读取不正确: %s", result.Content)
	}
	if contains(result.Content, "line1") || contains(result.Content, "line4") {
		t.Errorf("offset/limit 包含了不应有的内容")
	}

	// 测试文件不存在
	input = json.RawMessage(`{"path":"` + filepath.Join(dir, "nonexistent.txt") + `"}`)
	result, _ = readTool.Call(ctx, input, tctx)
	if !result.IsError {
		t.Error("期望文件不存在错误")
	}
}

// TestFileWriteTool 测试文件写入工具
func TestFileWriteTool(t *testing.T) {
	dir := t.TempDir()
	writeTool := builtin.NewFileWriteTool()
	ctx := context.Background()
	tctx := &tool.ToolContext{Cwd: dir}

	testFile := filepath.Join(dir, "output.txt")
	input := json.RawMessage(`{"path":"` + testFile + `", "contents": "hello world"}`)

	result, err := writeTool.Call(ctx, input, tctx)
	if err != nil || result.IsError {
		t.Fatalf("写入失败: %v %s", err, result.Content)
	}

	data, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Errorf("文件内容不匹配: %q", string(data))
	}

	// 测试 Plan 模式拒绝写入
	tctx.PermissionMode = types.PermissionModePlan
	perm := writeTool.CheckPermissions(input, tctx)
	if perm == nil || perm.Behavior != types.PermissionDeny {
		t.Error("Plan 模式应该拒绝写入")
	}
}

// TestFileEditTool 测试文件编辑工具
func TestFileEditTool(t *testing.T) {
	dir := t.TempDir()
	editTool := builtin.NewFileEditTool()
	ctx := context.Background()
	tctx := &tool.ToolContext{Cwd: dir}

	testFile := filepath.Join(dir, "edit.txt")
	os.WriteFile(testFile, []byte("hello world\nfoo bar\n"), 0644)

	// 正常替换
	input := json.RawMessage(`{"path":"` + testFile + `", "old_string":"foo bar", "new_string":"baz qux"}`)
	result, _ := editTool.Call(ctx, input, tctx)
	if result.IsError {
		t.Fatalf("编辑失败: %s", result.Content)
	}

	data, _ := os.ReadFile(testFile)
	if !contains(string(data), "baz qux") {
		t.Errorf("替换未生效: %s", string(data))
	}

	// 不唯一匹配测试
	os.WriteFile(testFile, []byte("aaa\naaa\naaa\n"), 0644)
	input = json.RawMessage(`{"path":"` + testFile + `", "old_string":"aaa", "new_string":"bbb"}`)
	result, _ = editTool.Call(ctx, input, tctx)
	if !result.IsError {
		t.Error("多次匹配应该报错")
	}

	// replace_all 模式
	input = json.RawMessage(`{"path":"` + testFile + `", "old_string":"aaa", "new_string":"bbb", "replace_all": true}`)
	result, _ = editTool.Call(ctx, input, tctx)
	if result.IsError {
		t.Fatalf("replace_all 失败: %s", result.Content)
	}
	data, _ = os.ReadFile(testFile)
	if contains(string(data), "aaa") {
		t.Error("replace_all 未替换所有出现")
	}
}

// TestBashTool 测试 Shell 工具
func TestBashTool(t *testing.T) {
	bashTool := builtin.NewBashTool()
	ctx := context.Background()
	tctx := &tool.ToolContext{Cwd: t.TempDir()}

	// 基本命令
	input := json.RawMessage(`{"command":"echo hello"}`)
	result, _ := bashTool.Call(ctx, input, tctx)
	if result.IsError {
		t.Fatalf("命令失败: %s", result.Content)
	}
	if !contains(result.Content, "hello") {
		t.Errorf("输出不包含 hello: %s", result.Content)
	}

	// 失败命令
	input = json.RawMessage(`{"command":"exit 1"}`)
	result, _ = bashTool.Call(ctx, input, tctx)
	if !result.IsError {
		t.Error("exit 1 应该返回错误")
	}
}

// TestTodoWriteTool 测试 TodoWrite 工具
func TestTodoWriteTool(t *testing.T) {
	todoTool := builtin.NewTodoWriteTool()
	ctx := context.Background()
	tctx := &tool.ToolContext{Cwd: t.TempDir()}

	// 创建待办
	input := json.RawMessage(`{
		"todos": [
			{"id": "1", "content": "Task A", "status": "pending"},
			{"id": "2", "content": "Task B", "status": "in_progress"}
		]
	}`)
	result, _ := todoTool.Call(ctx, input, tctx)
	if result.IsError {
		t.Fatalf("创建失败: %s", result.Content)
	}

	todos := todoTool.GetTodos()
	if len(todos) != 2 {
		t.Errorf("期望 2 个 todo, 实际 %d", len(todos))
	}

	// Merge 更新
	input = json.RawMessage(`{
		"todos": [
			{"id": "1", "content": "Task A Updated", "status": "completed"},
			{"id": "3", "content": "Task C", "status": "pending"}
		],
		"merge": true
	}`)
	result, _ = todoTool.Call(ctx, input, tctx)
	if result.IsError {
		t.Fatalf("merge 失败: %s", result.Content)
	}

	todos = todoTool.GetTodos()
	if len(todos) != 3 {
		t.Errorf("merge 后期望 3 个 todo, 实际 %d", len(todos))
	}
	if todos["1"].Status != "completed" {
		t.Errorf("todo 1 状态应为 completed, 实际 %s", todos["1"].Status)
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (s == substr || len(s) > len(substr) && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
