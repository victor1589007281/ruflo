// Skills 系统测试
package unit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/tool"
)

func TestSkillParseFile(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "my-skill")
	os.MkdirAll(skillDir, 0755)

	content := `---
name: My Test Skill
description: A test skill for unit testing
when_to_use: When testing
version: 1.0
---
# My Test Skill

This is the skill body with instructions.

1. Step one
2. Step two
`
	skillPath := filepath.Join(skillDir, "SKILL.md")
	os.WriteFile(skillPath, []byte(content), 0644)

	skill, err := skills.ParseSkillFile(skillPath, "test")
	if err != nil {
		t.Fatal(err)
	}

	if skill.Name != "My Test Skill" {
		t.Errorf("Name: %s", skill.Name)
	}
	if skill.Description != "A test skill for unit testing" {
		t.Errorf("Description: %s", skill.Description)
	}
	if skill.WhenToUse != "When testing" {
		t.Errorf("WhenToUse: %s", skill.WhenToUse)
	}
	if skill.Version != "1.0" {
		t.Errorf("Version: %s", skill.Version)
	}
	if skill.LoadedFrom != "test" {
		t.Errorf("LoadedFrom: %s", skill.LoadedFrom)
	}
	if skill.Body == "" {
		t.Error("Body 不应为空")
	}
}

func TestSkillRegistry(t *testing.T) {
	reg := skills.NewRegistry()

	skill1 := &skills.Skill{Name: "skill-a", Description: "Skill A", Body: "Body A"}
	skill2 := &skills.Skill{Name: "skill-b", Description: "Skill B", Body: "Body B"}

	reg.Register(skill1)
	reg.Register(skill2)

	if reg.Count() != 2 {
		t.Errorf("期望 2, 实际 %d", reg.Count())
	}

	s, ok := reg.Get("skill-a")
	if !ok || s.Description != "Skill A" {
		t.Error("skill-a 查找失败")
	}

	// 卸载
	if !reg.Unregister("skill-a") {
		t.Error("卸载 skill-a 应成功")
	}
	if reg.Count() != 1 {
		t.Errorf("卸载后期望 1, 实际 %d", reg.Count())
	}
	if reg.Unregister("nonexistent") {
		t.Error("卸载不存在的技能应返回 false")
	}
}

func TestSkillLoadFromDirs(t *testing.T) {
	dir := t.TempDir()

	// 创建两个技能
	for _, name := range []string{"tool-a", "tool-b"} {
		skillDir := filepath.Join(dir, name)
		os.MkdirAll(skillDir, 0755)
		content := "---\nname: " + name + "\ndescription: " + name + " desc\n---\n# " + name + "\nBody"
		os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644)
	}

	// 创建非技能目录 (无 SKILL.md)
	os.MkdirAll(filepath.Join(dir, "not-a-skill"), 0755)

	reg := skills.NewRegistry()
	loaded := reg.LoadFromDirs([]string{dir}, "test")

	if loaded != 2 {
		t.Errorf("期望加载 2 个技能, 实际 %d", loaded)
	}
	if reg.Count() != 2 {
		t.Errorf("注册表期望 2 个, 实际 %d", reg.Count())
	}
}

func TestSkillTool(t *testing.T) {
	reg := skills.NewRegistry()
	reg.Register(&skills.Skill{
		Name:        "code-review",
		Description: "Review code for quality",
		Body:        "Please review the following code carefully.",
	})

	skillTool := skills.NewSkillTool(reg)

	if skillTool.Name() != "Skill" {
		t.Errorf("工具名: %s", skillTool.Name())
	}

	// 调用存在的技能
	input := json.RawMessage(`{"name": "code-review"}`)
	result, err := skillTool.Call(context.Background(), input, &tool.ToolContext{})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Errorf("不应报错: %s", result.Content)
	}
	if !containsSubstr(result.Content, "review the following code") {
		t.Errorf("结果应包含技能内容: %s", result.Content)
	}

	// 调用不存在的技能
	input = json.RawMessage(`{"name": "nonexistent"}`)
	result, _ = skillTool.Call(context.Background(), input, &tool.ToolContext{})
	if !result.IsError {
		t.Error("不存在的技能应报错")
	}
}

func TestSkillInstallUninstall(t *testing.T) {
	dir := t.TempDir()

	// 安装
	err := skills.InstallSkill(dir, "new-skill", "---\nname: new-skill\n---\nContent")
	if err != nil {
		t.Fatal(err)
	}

	// 验证文件存在
	path := filepath.Join(dir, "new-skill", "SKILL.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("SKILL.md 应存在: %v", err)
	}

	// 可以解析
	skill, err := skills.ParseSkillFile(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	if skill.Name != "new-skill" {
		t.Errorf("名称: %s", skill.Name)
	}

	// 卸载
	err = skills.UninstallSkill(dir, "new-skill")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("卸载后 SKILL.md 不应存在")
	}

	// 卸载不存在的
	err = skills.UninstallSkill(dir, "nonexistent")
	if err == nil {
		t.Error("卸载不存在的技能应报错")
	}
}

func TestSkillReload(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "test-skill")
	os.MkdirAll(skillDir, 0755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: test-skill\n---\nV1"), 0644)

	reg := skills.NewRegistry()
	reg.LoadFromDirs([]string{dir}, "test")

	initial := reg.Count()
	if initial != 1 {
		t.Fatalf("初始加载期望 1, 实际 %d", initial)
	}

	// 添加新技能
	newDir := filepath.Join(dir, "new-skill")
	os.MkdirAll(newDir, 0755)
	os.WriteFile(filepath.Join(newDir, "SKILL.md"), []byte("---\nname: new-skill\n---\nV1"), 0644)

	// Reload
	reloaded := reg.Reload()
	if reloaded <= initial+1 {
		t.Errorf("重载后数量应大于 %d, 实际 %d", initial+1, reloaded)
	}
	if _, ok := reg.Get("coding-standards"); !ok {
		t.Error("重载后应保留内置技能")
	}
	if _, ok := reg.Get("new-skill"); !ok {
		t.Error("重载后应包含新增技能")
	}
}

func TestSkillFormatListing(t *testing.T) {
	reg := skills.NewRegistry()
	reg.Register(&skills.Skill{Name: "a", Description: "Desc A", WhenToUse: "When A"})
	reg.Register(&skills.Skill{Name: "b", Description: "Desc B"})

	listing := reg.FormatListing()
	if !containsSubstr(listing, "available_skills") {
		t.Error("应包含 available_skills 标签")
	}
	if !containsSubstr(listing, "Desc A") || !containsSubstr(listing, "Desc B") {
		t.Error("应包含所有技能描述")
	}
}
