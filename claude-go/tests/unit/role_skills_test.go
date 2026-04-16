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
