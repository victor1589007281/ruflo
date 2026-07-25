package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPreserveStatus 守护 design/03 §4.6「必过闸」: 改进技能不得丢 status,
// 否则 shadow 技能被 ImproveSkill 重写后会因"无 status = 视为 active"绕过门禁。
func TestPreserveStatus(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	shadow := write("shadow.md", "---\nname: s\nstatus: shadow\n---\n正文")
	if got := preserveStatus(shadow); got != "shadow" {
		t.Errorf("应保留 shadow, got %q", got)
	}

	archived := write("arch.md", "---\nname: a\nstatus: archived\ndescription: x\n---\n正文")
	if got := preserveStatus(archived); got != "archived" {
		t.Errorf("应保留 archived, got %q", got)
	}

	// 无 status 字段 → active（既有手写技能的默认语义）
	plain := write("plain.md", "---\nname: p\ndescription: x\n---\n正文")
	if got := preserveStatus(plain); got != "active" {
		t.Errorf("无 status 应默认 active, got %q", got)
	}

	// 文件不存在 → active（不因读失败而误判为受管状态）
	if got := preserveStatus(filepath.Join(dir, "nope.md")); got != "active" {
		t.Errorf("读失败应默认 active, got %q", got)
	}
}
