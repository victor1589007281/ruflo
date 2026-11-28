// 软链技能目录装载测试。
//
// 背景: os.ReadDir 的 DirEntry.IsDir 走 lstat, 指向目录的软链一律报 false。
// 用户级技能目录里用软链挂技能很常见 (~/.claude/skills/x -> ~/.agents/skills/x),
// 原来的 `if !entry.IsDir() { continue }` 会把它静默丢掉: 不报错、不告警,
// 只是 Skill 工具对它不存在、模型清单里也没有它 —— 表现为"路由基准必错"。
package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSymlinkTestSkill(t *testing.T, root, name string) {
	t.Helper()
	body := "---\nname: " + name + "\ndescription: " + name + " 的说明\n---\n\nbody\n"
	writeSkill(t, root, name, body) // 复用 watcher_test.go 的同包 helper
}

func TestLoadFromDirsFollowsSymlinkedSkillDir(t *testing.T) {
	real := t.TempDir()   // 真实技能库 (对应 ~/.agents/skills)
	skills := t.TempDir() // 入口技能目录 (对应 ~/.claude/skills)
	writeSymlinkTestSkill(t, real, "linked-skill")
	writeSymlinkTestSkill(t, skills, "plain-skill")

	if err := os.Symlink(filepath.Join(real, "linked-skill"), filepath.Join(skills, "linked-skill")); err != nil {
		t.Skipf("本平台不支持软链: %v", err)
	}

	reg := NewRegistry()
	if n := reg.LoadFromDirs([]string{skills}, "user"); n != 2 {
		t.Fatalf("软链技能也应被装载, 应加载 2 个, got %d", n)
	}
	if _, ok := reg.Get("linked-skill"); !ok {
		t.Fatal("软链技能 linked-skill 未进注册表 —— 它会对模型完全不可见")
	}
	// 清单渲染是最终的可见面: 名字必须出现在那里, 否则 Skill 工具无从选起。
	listing := reg.FormatShortListingForDir(0, skills)
	if listing == "" || !strings.Contains(listing, "linked-skill") {
		t.Fatalf("软链技能未出现在清单里:\n%s", listing)
	}
}

// 指向**目录之外**/悬空的软链不该让装载报错或凭空造技能。
func TestLoadFromDirsSkipsDanglingSymlink(t *testing.T) {
	skills := t.TempDir()
	writeSymlinkTestSkill(t, skills, "plain-skill")
	if err := os.Symlink(filepath.Join(skills, "does-not-exist"), filepath.Join(skills, "broken")); err != nil {
		t.Skipf("本平台不支持软链: %v", err)
	}
	reg := NewRegistry()
	if n := reg.LoadFromDirs([]string{skills}, "user"); n != 1 {
		t.Fatalf("悬空软链应被跳过, 应加载 1 个, got %d", n)
	}
}
