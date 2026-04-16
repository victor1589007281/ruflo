package unit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/skills"
)

func TestBuiltinSkillsLoadedByDefault(t *testing.T) {
	reg := skills.NewRegistry()
	loaded := reg.LoadDefaults(t.TempDir())
	if loaded < 10 {
		t.Fatalf("expected embedded skills to load, got %d", loaded)
	}

	for _, name := range []string{"coding-standards", "golang-patterns", "python-testing"} {
		skill, ok := reg.Get(name)
		if !ok {
			t.Fatalf("expected builtin skill %q to be registered", name)
		}
		if skill.LoadedFrom != "builtin" {
			t.Fatalf("expected %q to be loaded from builtin, got %q", name, skill.LoadedFrom)
		}
	}
}

func TestLoadDefaultsIncludesClaudeGoStateDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".claude-go", "skills", "state-skill", "SKILL.md"), "---\nname: state-skill\ndescription: state dir skill\n---\nState body\n")

	reg := skills.NewRegistry()
	reg.LoadDefaults(dir)
	skill, ok := reg.Get("state-skill")
	if !ok {
		t.Fatal("expected .claude-go/skills skill to be loaded")
	}
	if skill.LoadedFrom != "state" {
		t.Fatalf("expected state source, got %q", skill.LoadedFrom)
	}
}

func TestReloadPreservesBuiltins(t *testing.T) {
	reg := skills.NewRegistry()
	reg.LoadDefaults(t.TempDir())
	if _, ok := reg.Get("coding-standards"); !ok {
		t.Fatal("expected builtin skill before reload")
	}
	reg.Reload()
	if _, ok := reg.Get("coding-standards"); !ok {
		t.Fatal("expected builtin skill after reload")
	}
}

func TestRecommendedSkillsForRole_GoProject(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/test\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")

	coderSkills := skills.RecommendedSkillsForRole(dir, "coder")
	testerSkills := skills.RecommendedSkillsForRole(dir, "tester")

	if !containsString(coderSkills, "golang-patterns") {
		t.Fatalf("coder should receive golang-patterns, got %v", coderSkills)
	}
	if !containsString(testerSkills, "golang-testing") {
		t.Fatalf("tester should receive golang-testing, got %v", testerSkills)
	}
}

func TestRoleRegistryInjectsRecommendedSkills(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/test\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "service.go"), "package service\n\nfunc Run() error { return nil }\n")

	reg := agent.NewRoleRegistry(dir)
	if !containsString(reg.RecommendedSkills("coder"), "golang-patterns") {
		t.Fatalf("expected coder recommended skills to include golang-patterns, got %v", reg.RecommendedSkills("coder"))
	}

	coderPrompt := reg.MergedPrompt("coder", "实现 Go 服务", "前置结果")
	if !strings.Contains(coderPrompt, "Go Development Patterns") {
		t.Fatalf("expected coder prompt to include builtin Go skill, got: %s", coderPrompt)
	}

	testerPrompt := reg.MergedPrompt("tester", "验证 Go 服务", "前置结果")
	if !strings.Contains(testerPrompt, "Go Testing Patterns") {
		t.Fatalf("expected tester prompt to include builtin Go testing skill, got: %s", testerPrompt)
	}
}

func TestRoleRegistryResolvesSpecializedGoRoles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/test\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")

	reg := agent.NewRoleRegistry(dir)
	if got := reg.ResolveRoleName("coder"); got != "go-coder" {
		t.Fatalf("expected coder to resolve to go-coder, got %q", got)
	}
	info := reg.DescribeRole("coder")
	if info == nil || info.Resolved != "go-coder" {
		t.Fatalf("expected described role to resolve to go-coder, got %+v", info)
	}
	if !containsString(reg.RoleSkills("coder"), "golang-patterns") {
		t.Fatalf("expected effective role skills to include golang-patterns, got %v", reg.RoleSkills("coder"))
	}
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
