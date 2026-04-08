// 记忆系统单元测试
// 测试: CLAUDE.md 加载、@include 处理、优先级排序
package unit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/memory"
)

// TestMemoryLoaderBasic 测试基本的记忆文件加载
func TestMemoryLoaderBasic(t *testing.T) {
	dir := t.TempDir()

	// 创建 CLAUDE.md
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Project Rules\n- Rule 1\n- Rule 2"), 0644)

	loader := memory.NewLoader(dir)
	files := loader.LoadAll()

	found := false
	for _, f := range files {
		if filepath.Base(f.Path) == "CLAUDE.md" {
			found = true
			if !containsSubstr(f.Content, "Rule 1") {
				t.Errorf("CLAUDE.md 内容不正确: %s", f.Content)
			}
		}
	}
	if !found {
		t.Error("未找到 CLAUDE.md")
	}
}

// TestMemoryLoaderInclude 测试 @include 指令
func TestMemoryLoaderInclude(t *testing.T) {
	dir := t.TempDir()

	// 创建被引用的文件
	os.WriteFile(filepath.Join(dir, "extra.md"), []byte("Extra content here"), 0644)

	// 创建包含 @include 的 CLAUDE.md
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Rules\n@./extra.md\n- Rule after include"), 0644)

	loader := memory.NewLoader(dir)
	files := loader.LoadAll()

	for _, f := range files {
		if filepath.Base(f.Path) == "CLAUDE.md" {
			if !containsSubstr(f.Content, "Extra content here") {
				t.Errorf("@include 未被处理: %s", f.Content)
			}
		}
	}
}

// TestMemoryLoaderRulesDir 测试 .claude/rules/ 目录加载
func TestMemoryLoaderRulesDir(t *testing.T) {
	dir := t.TempDir()

	rulesDir := filepath.Join(dir, ".claude", "rules")
	os.MkdirAll(rulesDir, 0755)

	os.WriteFile(filepath.Join(rulesDir, "coding.md"), []byte("Use Go 1.22+"), 0644)
	os.WriteFile(filepath.Join(rulesDir, "testing.md"), []byte("Write unit tests"), 0644)

	loader := memory.NewLoader(dir)
	files := loader.LoadAll()

	ruleCount := 0
	for _, f := range files {
		if containsSubstr(f.Path, ".claude/rules/") {
			ruleCount++
		}
	}
	if ruleCount < 2 {
		t.Errorf("期望至少 2 个 rules 文件, 实际 %d", ruleCount)
	}
}

// TestMemoryPromptBuild 测试提示词构建
func TestMemoryPromptBuild(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Always use Go"), 0644)

	loader := memory.NewLoader(dir)
	files := loader.LoadAll()
	prompt := memory.BuildMemoryPrompt(files)

	if prompt == "" {
		t.Error("提示词不应为空")
	}
	if !containsSubstr(prompt, "Always use Go") {
		t.Error("提示词应包含记忆内容")
	}
	if !containsSubstr(prompt, "instructions OVERRIDE") {
		t.Error("提示词应包含 OVERRIDE 指示")
	}
}
