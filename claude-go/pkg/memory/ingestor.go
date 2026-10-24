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

	// lastDecisions 最近一次摄入里两条存储通道各自的裁决 (H2-4)。
	//
	// 上报的理由: 自动摄入是**没有人在旁边看**的写入路径 —— 它每次跑完都会往记忆
	// 里加东西, 而"这次加的东西顶掉了一条旧记忆"与"两条矛盾记忆现在并存"这两种
	// 结局, 事后从库内容里反推不出来 (前者被归档的条目还在, 后者恰恰是两条都在)。
	lastDecisions []IngestDecision
}

// IngestDecision 一次摄入对**一条通道**的裁决。
type IngestDecision struct {
	// Store 通道名: "fact" (FactStore) 或 "tiered" (L1 TieredStore)。
	Store string
	// FactID 本次写入的 ID。
	FactID string
	// Decision 守门裁决 (GateAppended / GateSuperseded / GateKeptBoth)。
	Decision GateDecision
	// Stats 该通道那次守门的扫描读数 (TieredStore 侧才有; FactStore 侧为零值)。
	Stats ResolveStats
}

// LastDecisions 返回最近一次摄入的裁决 (只读检视用)。
func (ig *Ingestor) LastDecisions() []IngestDecision {
	if ig == nil {
		return nil
	}
	return append([]IngestDecision(nil), ig.lastDecisions...)
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

	// 收口点的历史: 这里原来直接调 Add / AddWithGate 之前的 factStore.Add —— 两条
	// 通道**都绕过了守门**。FactStore 侧的守门 (AddWithGate) 早就写好了, 自动摄入
	// 却走的是不带门的 Add, 于是"已修复"与"仍存在"这类矛盾在自动摄入这条路上一路
	// 绿灯。守门写在库里但没人走它, 等于不存在 —— 这类"守卫没通电"是本批里反复
	// 出现的一个形态 (见验收方法论里的"冗余守卫没通电就该删")。
	ig.lastDecisions = ig.lastDecisions[:0]

	if ig.factStore != nil {
		dec := ig.factStore.AddWithGate(fact)
		ig.lastDecisions = append(ig.lastDecisions, IngestDecision{
			Store: "fact", FactID: fact.ID, Decision: dec,
		})
	}

	if ig.tieredStore != nil {
		dec := ig.tieredStore.AddWithGate(&MemoryEntry{
			ID:         id,
			Content:    content,
			Topics:     topics,
			Source:     source,
			Importance: importance,
			CreatedAt:  time.Now(),
			LastAccess: time.Now(),
		})
		ig.lastDecisions = append(ig.lastDecisions, IngestDecision{
			Store: "tiered", FactID: id, Decision: dec,
			Stats: ig.tieredStore.LastResolveStats(),
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

// maxF 返回两浮点数的较大者。
func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
