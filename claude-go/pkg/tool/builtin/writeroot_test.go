// writeroot_test.go —— 写入根限制 (opt-in) 的验收。
//
// 真实故障: 调研 agent 在 bypass 模式下把 evidence.md 写成了
// /home/victor/evidence.md —— 既污染宿主家目录, 又让并发跑的四个任务互相覆盖
// 同一份文件。所以判据必须落在磁盘上: 被拒的那次**不能创建文件**。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

func TestWriteTool_WriteRootRejectsOutside(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "evidence.md")
	tctx := &tool.ToolContext{Cwd: root, WriteRoot: root}

	// 根内相对路径: 放行。
	res, err := (&FileWriteTool{}).Call(context.Background(),
		json.RawMessage(`{"path":"evidence.md","contents":"ok"}`), tctx)
	if err != nil || res.IsError {
		t.Fatalf("根内写入被误拒: err=%v res=%+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(root, "evidence.md")); err != nil {
		t.Fatalf("根内文件没有落盘: %v", err)
	}

	// 根外绝对路径: 拒绝, 且磁盘上不能出现该文件。
	res, err = (&FileWriteTool{}).Call(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"path":%q,"contents":"leak"}`, outside)), tctx)
	if err != nil {
		t.Fatalf("Write 调用出错: %v", err)
	}
	if !res.IsError {
		t.Fatalf("根外写入应被拒, got %+v", res)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("被拒的写入仍然创建了文件: %v", err)
	}

	// `../` 逃逸: 同样拒绝。
	res, err = (&FileWriteTool{}).Call(context.Background(),
		json.RawMessage(`{"path":"../escape.md","contents":"leak"}`), tctx)
	if err != nil {
		t.Fatalf("Write 调用出错: %v", err)
	}
	if !res.IsError {
		t.Fatalf("../ 逃逸应被拒, got %+v", res)
	}
}

func TestEditTool_WriteRootRejectsOutside(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(outside, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := (&FileEditTool{}).Call(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"hello","new_string":"pwned"}`, outside)),
		&tool.ToolContext{Cwd: root, WriteRoot: root})
	if err != nil {
		t.Fatalf("Edit 调用出错: %v", err)
	}
	if !res.IsError {
		t.Fatalf("根外编辑应被拒, got %+v", res)
	}
	if b, _ := os.ReadFile(outside); string(b) != "hello" {
		t.Fatalf("被拒的编辑仍然改了文件: %q", string(b))
	}
}

// TestWriteTool_NoWriteRootKeepsLegacyBehavior 钉住默认关闭: 未声明
// RestrictWritesToCwd 的角色 (coder 等) 行为必须逐字节不变。
func TestWriteTool_NoWriteRootKeepsLegacyBehavior(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "free.txt")
	res, err := (&FileWriteTool{}).Call(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"path":%q,"contents":"ok"}`, outside)),
		&tool.ToolContext{Cwd: t.TempDir()})
	if err != nil || res.IsError {
		t.Fatalf("未声明 WriteRoot 时不应限制: err=%v res=%+v", err, res)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("文件没有落盘: %v", err)
	}
}
