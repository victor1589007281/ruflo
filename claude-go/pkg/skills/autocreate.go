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

	// status: shadow —— 自动提炼的技能默认处 shadow 态, 晋升 active 由进化门禁
	// (配对轨迹审计, design/03 §4.3c) 裁决; 谱系字段供审计与回滚。
	skillMD := fmt.Sprintf("---\nname: %s\ndescription: %s\nwhen_to_use: %s\ncreated_at: %s\nauto_generated: true\nstatus: shadow\n---\n\n%s\n",
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

// preserveStatus 读回 SKILL.md 现有 status 值 (无则视为 active)。
// 用于改进技能时不丢治理状态 —— 否则 shadow 技能被改进后会变成"无 status
// 字段 = 视为 active", 静默绕过进化门禁 (design/03 §4.6 必过闸)。
func preserveStatus(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "active"
	}
	head := string(data)
	if len(head) > 800 {
		head = head[:800]
	}
	for _, ln := range strings.Split(head, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "status:") {
			if v := strings.TrimSpace(strings.TrimPrefix(ln, "status:")); v != "" {
				return v
			}
		}
	}
	return "active"
}

// ImproveSkill 技能自改进: 根据使用反馈优化已有技能。
func (ac *AutoCreator) ImproveSkill(ctx context.Context, skillName, feedback string, success bool) error {
	if ac.LLM == nil || ac.Registry == nil {
		return nil
	}

	// GetAny 而非 Get: 自改进必须能改 shadow 技能 (Get 是运行期视图, 已排除 shadow)。
	// 配合下面的 preserveStatus, 改进后仍留在 shadow 态, 不静默绕过进化门禁。
	sk, ok := ac.Registry.GetAny(skillName)
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

	// 保留原 status/audit 行: 否则改进一个 shadow 技能会因"无 status 字段 = 视为
	// active"而静默绕过进化门禁 (design/03 §4.6 必过闸)。
	statusLine := "status: " + preserveStatus(filepath.Join(ac.SkillDir, sk.Name, "SKILL.md"))
	skillMD := fmt.Sprintf("---\nname: %s\ndescription: %s\nwhen_to_use: %s\nauto_generated: true\n%s\nimproved_at: %s\n---\n\n%s\n",
		sk.Name, sk.Description, sk.WhenToUse, statusLine, time.Now().Format(time.RFC3339), improved)

	path := filepath.Join(ac.SkillDir, sk.Name, "SKILL.md")
	if err := os.WriteFile(path, []byte(skillMD), 0o644); err != nil {
		return fmt.Errorf("更新技能文件失败: %w", err)
	}

	ac.Registry.Reload()
	return nil
}
