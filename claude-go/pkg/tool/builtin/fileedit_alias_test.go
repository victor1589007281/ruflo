// fileedit_alias_test.go —— StrReplace 入参缺失键的保护。
//
// 缺 new_string 时 Go 零值把匹配段替换成空串 (静默删除); 缺 old_string 且
// replace_all=true 时 strings.Count(content,"") = len+1, 新串会被插到每个
// 字符之间。两种都不报错, 所以必须在改动磁盘前拒绝。
package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

func TestEditToolMissingNewStringDoesNotDelete(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "evidence.md")
	old := "结论 A\n结论 B\n"
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := (&FileEditTool{}).Call(context.Background(),
		json.RawMessage(`{"path":"evidence.md","old_string":"结论 A"}`), &tool.ToolContext{Cwd: dir})
	if err != nil {
		t.Fatalf("Call 返回错误: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("缺 new_string 必须报错, got %+v", res)
	}
	if !strings.Contains(res.Content, "new_string") {
		t.Fatalf("错误信息要指出字段名, got %q", res.Content)
	}
	got, _ := os.ReadFile(p)
	if string(got) != old {
		t.Fatalf("文件被改写: %q", got)
	}
}

func TestEditToolAcceptsOldStrAlias(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("alpha beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := (&FileEditTool{}).Call(context.Background(),
		json.RawMessage(`{"path":"a.txt","old_str":"alpha","new_str":"gamma"}`), &tool.ToolContext{Cwd: dir})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("old_str/new_str 别名应被接受: %+v %v", res, err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "gamma beta" {
		t.Fatalf("替换结果 = %q", got)
	}
}
