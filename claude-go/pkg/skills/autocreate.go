// Package skills — 技能自动创建与自改进。
// 吸收 Hermes-agent 的核心优势: 复杂任务完成后 LLM 自动提炼技能文件。
package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LLMClient 用于技能提炼的 LLM 接口 (与 api.Client.SimpleComplete 对齐)。
type LLMClient interface {
	SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// AutoCreator 自动技能创建器。
// 任务成功后调用 MaybeCreate，LLM 判断是否值得提炼成可复用技能。
type AutoCreator struct {
	SkillDir string
	LLM      LLMClient
	Model    string
	Registry *Registry
}

// NewAutoCreator 创建技能自动创建器。
func NewAutoCreator(skillDir string, llm LLMClient, model string, reg *Registry) *AutoCreator {
	return &AutoCreator{
		SkillDir: skillDir,
		LLM:      llm,
		Model:    model,
		Registry: reg,
	}
}

// MaybeCreate 在复杂任务成功后调用，LLM 判断是否提炼技能。
// objective: 任务目标, approach: 使用的方法/步骤, outcome: 结果。
// 返回创建的技能名称（空字符串表示不值得创建）。
func (ac *AutoCreator) MaybeCreate(ctx context.Context, objective, approach, outcome string) (string, error) {
	if ac.LLM == nil || ac.SkillDir == "" {
		return "", nil
	}

	system := `你是一个技能提炼专家。根据完成的任务，判断是否值得创建一个可复用的技能文件。
只有满足以下条件才创建:
1. 任务涉及的方法/模式具有通用性，未来类似任务可直接复用
2. 方法步骤清晰，可以结构化描述
3. 不是一次性的简单操作

如果值得创建，输出 JSON:
{"create": true, "name": "技能名(英文小写+连字符)", "description": "一句话描述", "when_to_use": "何时使用", "content": "技能详细内容(Markdown)"}
如果不值得，输出: {"create": false}`

	userMsg := fmt.Sprintf("任务目标: %s\n\n使用方法:\n%s\n\n结果:\n%s",
		objective, approach, outcome)

	resp, err := ac.LLM.SimpleComplete(ctx, system, userMsg)
	if err != nil {
		return "", fmt.Errorf("LLM 调用失败: %w", err)
	}

	resp = strings.TrimSpace(resp)
	// 提取 JSON
	start := strings.Index(resp, "{")
	end := strings.LastIndex(resp, "}")
	if start < 0 || end < start {
		return "", nil
	}
	jsonStr := resp[start : end+1]

	var result struct {
		Create      bool   `json:"create"`
		Name        string `json:"name"`
		Description string `json:"description"`
		WhenToUse   string `json:"when_to_use"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil || !result.Create {
		return "", nil
	}
	if result.Name == "" || result.Content == "" {
		return "", nil
	}

	skillMD := fmt.Sprintf("---\nname: %s\ndescription: %s\nwhen_to_use: %s\ncreated_at: %s\nauto_generated: true\n---\n\n%s\n",
		result.Name, result.Description, result.WhenToUse, time.Now().Format(time.RFC3339), result.Content)

	dir := filepath.Join(ac.SkillDir, result.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建技能目录失败: %w", err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(skillMD), 0o644); err != nil {
		return "", fmt.Errorf("写入技能文件失败: %w", err)
	}

	if ac.Registry != nil {
		ac.Registry.Reload()
	}

	return result.Name, nil
}

// ImproveSkill 技能自改进: 根据使用反馈优化已有技能。
func (ac *AutoCreator) ImproveSkill(ctx context.Context, skillName, feedback string, success bool) error {
	if ac.LLM == nil || ac.Registry == nil {
		return nil
	}

	sk, ok := ac.Registry.Get(skillName)
	if !ok {
		return fmt.Errorf("技能 %s 不存在", skillName)
	}

	system := `你是技能优化专家。根据使用反馈改进已有技能的内容。
输出改进后的完整技能内容(Markdown格式，不包含 frontmatter)。
如果不需要改进，原样输出。`

	status := "成功"
	if !success {
		status = "失败"
	}
	userMsg := fmt.Sprintf("技能名: %s\n描述: %s\n\n当前内容:\n%s\n\n使用结果: %s\n反馈:\n%s",
		sk.Name, sk.Description, sk.Body, status, feedback)

	resp, err := ac.LLM.SimpleComplete(ctx, system, userMsg)
	if err != nil {
		return fmt.Errorf("LLM 调用失败: %w", err)
	}

	improved := strings.TrimSpace(resp)
	if improved == "" || improved == sk.Body {
		return nil
	}

	skillMD := fmt.Sprintf("---\nname: %s\ndescription: %s\nwhen_to_use: %s\nauto_generated: true\nimproved_at: %s\n---\n\n%s\n",
		sk.Name, sk.Description, sk.WhenToUse, time.Now().Format(time.RFC3339), improved)

	path := filepath.Join(ac.SkillDir, sk.Name, "SKILL.md")
	if err := os.WriteFile(path, []byte(skillMD), 0o644); err != nil {
		return fmt.Errorf("更新技能文件失败: %w", err)
	}

	ac.Registry.Reload()
	return nil
}
