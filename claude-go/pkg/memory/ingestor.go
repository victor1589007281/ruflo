package memory

import (
	"fmt"
	"strings"
	"time"
)

// Ingestor 记忆自动摄入器
// 每次 Agent 交互后自动分类、评分、存储记忆
type Ingestor struct {
	factStore   *FactStore
	tieredStore *TieredStore
	idSeq       int
}

// NewIngestor 创建记忆摄入器
func NewIngestor(factStore *FactStore, tieredStore *TieredStore) *Ingestor {
	return &Ingestor{
		factStore:   factStore,
		tieredStore: tieredStore,
	}
}

// IngestText 摄入一段文本 (自动分类 + 评分)
func (ig *Ingestor) IngestText(content, source string, topics []string) *MemoryFact {
	if strings.TrimSpace(content) == "" || len(content) < 10 {
		return nil
	}
	cat := ig.classify(content)
	importance := ig.scoreImportance(content, source)
	ig.idSeq++
	id := fmt.Sprintf("ing-%s-%04d", time.Now().Format("20060102-150405"), ig.idSeq)

	fact := NewMemoryFact(id, content, cat, importance, source, topics)

	if ig.factStore != nil {
		ig.factStore.Add(fact)
	}

	if ig.tieredStore != nil {
		ig.tieredStore.Add(&MemoryEntry{
			ID:         id,
			Content:    content,
			Topics:     topics,
			Source:     source,
			Importance: importance,
			CreatedAt:  time.Now(),
			LastAccess: time.Now(),
		})
	}

	return fact
}

// IngestFacts 批量摄入关键事实 (来自 SmartExtractKeyFacts)
func (ig *Ingestor) IngestFacts(facts []string, source string) []*MemoryFact {
	var results []*MemoryFact
	for _, f := range facts {
		if result := ig.IngestText(f, source, nil); result != nil {
			results = append(results, result)
		}
	}
	return results
}

// classify 分类记忆 (基于内容启发式)
func (ig *Ingestor) classify(content string) MemoryCategory {
	lower := strings.ToLower(content)

	// Fact: API/接口/类型/架构决策
	factMarkers := []string{
		"func ", "type ", "interface ", "struct ", "api", "endpoint",
		"架构", "决策", "选择", "采用", "schema", "protocol",
		"package", "import", "module", "config",
	}
	for _, m := range factMarkers {
		if strings.Contains(lower, m) {
			return CategoryFact
		}
	}

	// Preference: 用户偏好
	prefMarkers := []string{
		"我喜欢", "我习惯", "我偏好", "我倾向", "prefer", "style",
		"convention", "命名", "格式", "总是用", "always use",
	}
	for _, m := range prefMarkers {
		if strings.Contains(lower, m) {
			return CategoryPreference
		}
	}

	// Goal: 目标/计划
	goalMarkers := []string{
		"todo", "目标", "计划", "plan", "milestone", "sprint",
		"需要完成", "接下来", "next step", "deadline",
	}
	for _, m := range goalMarkers {
		if strings.Contains(lower, m) {
			return CategoryGoal
		}
	}

	// Event: 时间戳/部署/发布
	eventMarkers := []string{
		"部署", "发布", "deploy", "release", "merge", "commit",
		"修复了", "fixed", "resolved", "updated", "已完成",
	}
	for _, m := range eventMarkers {
		if strings.Contains(lower, m) {
			return CategoryEvent
		}
	}

	// Context: 默认分类 (临时讨论等)
	return CategoryContext
}

// scoreImportance 评估重要性 (0-1)
func (ig *Ingestor) scoreImportance(content, source string) float64 {
	score := 0.5
	lower := strings.ToLower(content)

	// 来源加权
	switch source {
	case "team_result", "blackboard":
		score = 0.8
	case "pre_compact":
		score = 0.7
	case "evolution":
		score = 0.75
	case "dream":
		score = 0.65
	case "extraction":
		score = 0.6
	}

	// 内容信号
	if strings.Contains(lower, "error") || strings.Contains(lower, "错误") ||
		strings.Contains(lower, "bug") || strings.Contains(lower, "修复") {
		score = maxF(score, 0.7)
	}
	if strings.Contains(lower, "决策") || strings.Contains(lower, "决定") ||
		strings.Contains(lower, "decision") || strings.Contains(lower, "concluded") {
		score = maxF(score, 0.75)
	}
	if strings.Contains(lower, "重要") || strings.Contains(lower, "important") ||
		strings.Contains(lower, "critical") || strings.Contains(lower, "关键") {
		score = maxF(score, 0.8)
	}

	// 长度加权 (信息密度指标)
	if len(content) > 500 {
		score = maxF(score, 0.6)
	}
	if len(content) > 1000 {
		score = maxF(score, 0.65)
	}

	if score > 1.0 {
		score = 1.0
	}
	return score
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
