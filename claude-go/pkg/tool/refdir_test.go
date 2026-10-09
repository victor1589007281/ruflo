package tool

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func refJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 自指保护必须同时覆盖**两个**落盘目录。
//
// pkg/tool 的溢出目录 (执行时就超预算) 与 pkg/compact 的折叠引用目录 (执行时没超、
// 后来被压缩期折叠) 是两条独立的路, 但会互相拆台: 折叠给出引用 -> 模型 Read 回来
// -> 读回内容又超预算 -> 再落一份 artifact 并只给预览 -> 正文永远进不了上下文。
// 只盖第一个目录时, 这个循环照样成立, 只是绕了一层。
func TestReadsArtifact_CoversBothSpillDirs(t *testing.T) {
	cwd := t.TempDir()
	tctx := &ToolContext{Cwd: cwd}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"工具溢出目录 (绝对)", filepath.Join(cwd, ".claude-go", "artifacts", "tool-results", "Bash-abc.txt"), true},
		{"折叠引用目录 (绝对)", filepath.Join(cwd, ".claude-go", "tool-results", "abc123.txt"), true},
		{"工具溢出目录 (相对)", filepath.Join(".claude-go", "artifacts", "tool-results", "x.txt"), true},
		{"折叠引用目录 (相对)", filepath.Join(".claude-go", "tool-results", "x.txt"), true},
		{"普通工作区文件", filepath.Join(cwd, "src", "main.go"), false},
		{"artifacts 下但不是 tool-results", filepath.Join(cwd, ".claude-go", "artifacts", "other", "x.txt"), false},
		{"越过目录边界的兄弟路径", filepath.Join(cwd, ".claude-go", "tool-results-evil", "x.txt"), false},
	}
	for _, c := range cases {
		input := refJSON(t, map[string]string{"path": c.path})
		if got := readsArtifact(input, tctx); got != c.want {
			t.Errorf("%s: readsArtifact(%q) = %v, 期望 %v", c.name, c.path, got, c.want)
		}
	}
}

// nil tctx / 空 cwd / 无 path 字段都不该 panic, 也不该把任何路径判成 artifact。
func TestReadsArtifact_NilSafe(t *testing.T) {
	input := refJSON(t, map[string]string{"path": "/etc/passwd"})
	if readsArtifact(input, nil) {
		t.Error("nil tctx 不应判定为 artifact")
	}
	if readsArtifact(input, &ToolContext{Cwd: ""}) {
		t.Error("空 cwd 不应判定为 artifact")
	}
	// 没有 path 字段 (如 Bash 调用) 不应 panic。
	noPath := refJSON(t, map[string]string{"command": "ls"})
	if readsArtifact(noPath, &ToolContext{Cwd: t.TempDir()}) {
		t.Error("无 path 字段不应判定为 artifact")
	}
}
