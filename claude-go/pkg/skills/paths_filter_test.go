package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFrontmatterPaths(t *testing.T) {
	// YAML 列表形态
	fm := "name: x\ndescription: d\npaths:\n  - \"**/*.go\"\n  - \"cmd/**\"\nstatus: active\n"
	sk := &Skill{}
	parseFrontmatter(fm, sk)
	if len(sk.Paths) != 2 || sk.Paths[0] != "**/*.go" || sk.Paths[1] != "cmd/**" {
		t.Errorf("YAML 列表 paths 解析失败: %+v", sk.Paths)
	}
	// 行内形态
	sk2 := &Skill{}
	parseFrontmatter(`name: y
paths: ["**/*.py", "scripts/**"]
`, sk2)
	if len(sk2.Paths) != 2 || sk2.Paths[0] != "**/*.py" {
		t.Errorf("行内 paths 解析失败: %+v", sk2.Paths)
	}
	// 无 paths → 空
	sk3 := &Skill{}
	parseFrontmatter("name: z\ndescription: d\n", sk3)
	if len(sk3.Paths) != 0 {
		t.Errorf("无 paths 应为空, 得 %+v", sk3.Paths)
	}
}

func TestMatchPathGlob(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.go", "pkg/engine/engine.go", true},
		{"**/*.go", "main.go", true},
		{"**/*.go", "README.md", false},
		{"cmd/**", "cmd/claude-go/main.go", true},
		{"cmd/**", "pkg/x.go", false},
		{"*.py", "scripts/a.py", true}, // basename 命中
		{"pkg/*/main.go", "pkg/app/main.go", true},
		{"pkg/*/main.go", "pkg/app/sub/main.go", false},
	}
	for _, c := range cases {
		if got := matchPathGlob(c.pattern, c.path); got != c.want {
			t.Errorf("match(%q,%q)=%v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestVisibleInDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "x.go"), []byte("package x"), 0o644)

	withGo := &Skill{Name: "go-skill", Paths: []string{"**/*.go"}}
	withPy := &Skill{Name: "py-skill", Paths: []string{"**/*.py"}}
	noPaths := &Skill{Name: "plain"}

	if !withGo.VisibleInDir(dir) {
		t.Error("存在 .go 文件应可见")
	}
	if withPy.VisibleInDir(dir) {
		t.Error("无 .py 文件应不可见")
	}
	if !noPaths.VisibleInDir(dir) {
		t.Error("无 paths 声明应恒可见")
	}
	if !withPy.VisibleInDir("") {
		t.Error("空目录参数应恒可见 (向后兼容)")
	}
}

func TestActiveInDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a"), 0o644)
	r := NewRegistry()
	r.Register(&Skill{Name: "go1", Paths: []string{"**/*.go"}})
	r.Register(&Skill{Name: "py1", Paths: []string{"**/*.py"}})
	r.Register(&Skill{Name: "any"})
	got := r.ActiveInDir(dir)
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if !names["go1"] || !names["any"] || names["py1"] {
		t.Errorf("ActiveInDir 过滤错误: %v", names)
	}
}
