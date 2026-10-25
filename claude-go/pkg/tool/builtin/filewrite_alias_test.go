// filewrite_alias_test.go —— Write 入参键名的兼容与缺字段保护。
//
// 真实事故: 模型先按 schema 用 `contents` 写 evidence.md, 后续两次改写用了
// `content`。Go 的 Unmarshal 不报未知字段, Contents 留空, Write 把 7KB 的
// 证据台账静默截成 0 字节 —— 全程没有任何错误返回。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

func newWriteTctx(dir string) *tool.ToolContext {
	return &tool.ToolContext{Cwd: dir}
}

func TestWriteToolAcceptsContentAlias(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "evidence.md")
	res, err := NewFileWriteTool().Call(context.Background(),
		json.RawMessage(`{"path":"evidence.md","content":"别名写法的正文"}`), newWriteTctx(dir))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("content 别名应被接受: res=%+v err=%v", res, err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("文件没有写出来: %v", err)
	}
	if string(got) != "别名写法的正文" {
		t.Fatalf("写入内容 = %q", got)
	}

	// contents 优先级高于 content: 两者同时出现时以 schema 字段为准。
	res, err = NewFileWriteTool().Call(context.Background(),
		json.RawMessage(`{"path":"evidence.md","contents":"正式字段","content":"别名"}`), newWriteTctx(dir))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("contents 应正常写入: %+v %v", res, err)
	}
	got, _ = os.ReadFile(p)
	if string(got) != "正式字段" {
		t.Fatalf("contents 应优先, got %q", got)
	}
}

// TestWriteToolMissingContentKeyDoesNotTruncate 钉住事故本身: 缺字段时必须
// 报错, 且磁盘上的旧内容一个字节都不能动。
func TestWriteToolMissingContentKeyDoesNotTruncate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "evidence.md")
	old := "# 已有证据台账\n\n| E1 | ... |\n"
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewFileWriteTool().Call(context.Background(),
		json.RawMessage(`{"path":"evidence.md","wrongKey":"新正文"}`), newWriteTctx(dir))
	if err != nil {
		t.Fatalf("Call 返回错误: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("缺 contents 字段必须报错, got %+v", res)
	}
	if !strings.Contains(res.Content, "contents") {
		t.Fatalf("错误信息要指出字段名, got %q", res.Content)
	}
	got, _ := os.ReadFile(p)
	if string(got) != old {
		t.Fatalf("旧文件被改写: %q", got)
	}
}

// TestWriteToolExplicitEmptyStillAllowed 钉住另一侧: 显式 contents:"" 是
// 调用方明确要求清空, 不能被缺字段保护误伤。
func TestWriteToolExplicitEmptyStillAllowed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewFileWriteTool().Call(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"path":%q,"contents":""}`, "empty.txt")), newWriteTctx(dir))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("显式空内容应允许: %+v %v", res, err)
	}
	got, _ := os.ReadFile(p)
	if len(got) != 0 {
		t.Fatalf("文件未被清空: %q", got)
	}
}
