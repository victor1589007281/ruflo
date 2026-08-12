// Package skills — retrieval.go 孤儿代码裁定 (2026-08-13, 手册 7.0.6)。
//
// Deprecated: SkillLibrary/SkillTemplate 源自一次性大批次提交 (a283dede7),
// 全仓零生产调用点、无文档、无加载器 (runtime/go 模板库同)。技能检索的现行
// 路径是 Registry.Get/Active + FormatShortListing(ForDir) —— 本文件保留仅为
// 考古参考, 新代码请勿使用; 待下一轮清理批次删除。
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SkillLibrary provides retrieval of runtime skills by keyword matching.
type SkillLibrary struct {
	root string
}

func NewSkillLibrary(root string) *SkillLibrary {
	return &SkillLibrary{root: root}
}

// SkillTemplate represents a loaded skill template.
type SkillTemplate struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Content  string   `json:"content"`
	Keywords []string `json:"keywords"`
}

// Find matches skills against a query string.
func (sl *SkillLibrary) Find(query string) ([]SkillTemplate, error) {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil, nil
	}

	all, err := sl.All()
	if err != nil {
		return nil, err
	}

	type scored struct {
		skill SkillTemplate
		score int
	}

	var scoredSkills []scored
	for _, skill := range all {
		score := 0
		searchText := strings.ToLower(skill.Name + " " + strings.Join(skill.Keywords, " ") + " " + skill.Content)
		for _, word := range words {
			if strings.Contains(searchText, word) {
				score++
			}
		}
		if score > 0 {
			scoredSkills = append(scoredSkills, scored{skill: skill, score: score})
		}
	}

	// Sort by score descending (simple bubble sort for stability)
	for i := 0; i < len(scoredSkills); i++ {
		for j := i + 1; j < len(scoredSkills); j++ {
			if scoredSkills[j].score > scoredSkills[i].score {
				scoredSkills[i], scoredSkills[j] = scoredSkills[j], scoredSkills[i]
			}
		}
	}

	limit := 3
	if len(scoredSkills) < limit {
		limit = len(scoredSkills)
	}

	result := make([]SkillTemplate, limit)
	for i := 0; i < limit; i++ {
		result[i] = scoredSkills[i].skill
	}
	return result, nil
}

// Get retrieves a skill by exact name.
func (sl *SkillLibrary) Get(name string) (*SkillTemplate, error) {
	all, err := sl.All()
	if err != nil {
		return nil, err
	}
	for _, skill := range all {
		if skill.Name == name {
			return &skill, nil
		}
	}
	return nil, fmt.Errorf("skill not found: %s", name)
}

// All returns all available skills.
func (sl *SkillLibrary) All() ([]SkillTemplate, error) {
	var skills []SkillTemplate

	err := filepath.Walk(sl.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		name := strings.TrimSuffix(filepath.Base(path), ".md")
		keywords := extractKeywords(name, string(content))

		skills = append(skills, SkillTemplate{
			Name:     name,
			Path:     path,
			Content:  string(content),
			Keywords: keywords,
		})
		return nil
	})

	if err != nil {
		return nil, err
	}
	return skills, nil
}

func extractKeywords(name, content string) []string {
	var keywords []string
	// Add words from filename
	keywords = append(keywords, strings.Split(name, "-")...)

	// Extract keywords from "场景" section
	lines := strings.Split(content, "\n")
	inScene := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## 场景") || strings.HasPrefix(trimmed, "## 场景 (When to use)") {
			inScene = true
			continue
		}
		if inScene {
			if strings.HasPrefix(trimmed, "## ") {
				break
			}
			if trimmed != "" {
				// Add meaningful words from scene description
				words := strings.Fields(trimmed)
				for _, w := range words {
					w = strings.ToLower(strings.Trim(w, "，。、；：？！\"'"))
					if len(w) > 2 && !isStopWord(w) {
						keywords = append(keywords, w)
					}
				}
			}
		}
	}
	return keywords
}

func isStopWord(w string) bool {
	stopWords := map[string]bool{
		"when": true, "use": true, "the": true, "and": true, "for": true,
		"with": true, "from": true, "that": true, "this": true, "are": true,
		"需要": true, "使用": true, "可以": true, "通过": true, "进行": true,
		"实现": true, "同时": true, "例如": true,
	}
	return stopWords[w]
}
