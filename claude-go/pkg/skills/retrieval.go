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

// NewSkillLibrary 构造技能库, root 为技能 markdown 文件的根目录。
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

// Find 按关键词命中数给技能打分, 返回得分最高的前 3 个 (无命中返回空)。
//
// Find matches skills against a query string.
//
// 算法解读：
//
//	原理：把查询按空白切成词, 对每个技能在「名字 + 关键词 + 正文」拼成的文本里
//	      统计命中的词数作为分数, 命中数 >0 的进入候选, 再按分数降序取前 3。
//	关键不变量：limit 必须夹到候选数以内 —— 候选不足 3 个时返回短切片而不是补零值;
//	      排序用的是朴素交换 (选择式冒泡), 交换只在严格大于时发生, 因此同分技能的
//	      相对顺序保持不变 (稳定)。
//	代价与边界：每个技能都要拼接并小写化整份正文, 每轮检索都是全量 O(技能数 × 正文长);
//	      子串匹配没有词边界, 短词会命中长词内部 (如 "go" 命中 "google")。
//	设计权衡：用「命中词数」这种无权重打分而不是 TF-IDF, 是为了零依赖与可解释;
//	      代价是长正文的技能天然更容易得分, 且没有任何排序之外的阈值过滤。
//
// 注意：本文件已标记 Deprecated (全仓零生产调用), 此函数仅供考古参考。
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

// Get 按技能名精确查找 (名字 = 文件名去掉 .md); 找不到返回错误。
//
// Get retrieves a skill by exact name.
//
// 注意：命中后返回的是循环变量的地址 —— 每个元素在遍历中被复制, 所以调用方拿到的是副本,
// 修改它不会影响技能库; 但每次调用都要重新遍历并读取全部技能文件。
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

// All 递归遍历 root 下所有 .md, 逐个读成 SkillTemplate (名字取文件名, 关键词由 extractKeywords 抽取)。
//
// All returns all available skills.
//
// 注意：遍历中任何一次读取失败 (含单个文件读不动、root 不存在) 都会让整个调用返回错误,
// 已读到的部分被丢弃 —— 没有「跳过坏文件继续」的容错; 且每次都重新读盘, 无缓存。
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

// extractKeywords 抽取技能关键词: 文件名按 "-" 拆词, 外加 "## 场景" 小节里的实义词。
//
// 注意：只认 `## 场景` 与 `## 场景 (When to use)` 两种小节标题, 且遇到下一个 `## ` 就停;
// 小节内的词按空白切分, 长度 >2 且不在停用词表才收录 —— 因此中文短语会被整段当作一个词,
// 英文短语则被拆成单词。文件名里带 "-" 的片段不做过短过滤, 会原样进关键词表。
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

// isStopWord 判断词是否在停用词表里 (少量中英文高频虚词), 命中则不作为关键词。
//
// 注意：表是写死的, 只覆盖这里列出的十几个词; 每次调用都重建一次 map, 未做包级缓存。
func isStopWord(w string) bool {
	stopWords := map[string]bool{
		"when": true, "use": true, "the": true, "and": true, "for": true,
		"with": true, "from": true, "that": true, "this": true, "are": true,
		"需要": true, "使用": true, "可以": true, "通过": true, "进行": true,
		"实现": true, "同时": true, "例如": true,
	}
	return stopWords[w]
}
