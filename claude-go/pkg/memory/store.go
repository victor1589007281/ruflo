package memory

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FactStore L2 结构化记忆存储 (JSON 持久化)
// 借鉴 Google Always-On Memory Agent 的 SQLite 方案，
// 但使用 JSON 文件保持与现有代码库一致。
type FactStore struct {
	mu          sync.RWMutex
	facts       map[string]*MemoryFact // id → fact
	connections []FactConnection       // 事实间关联
	idSeq       int
	persistDir  string
	dirty       bool

	// 整合日志
	consolidationLog []ConsolidationEntry
}

// FactConnection 事实间关联
type FactConnection struct {
	FactIDA  string    `json:"factIdA"`
	FactIDB  string    `json:"factIdB"`
	Relation string    `json:"relation"` // "related" / "contradicts" / "causes" / "part_of"
	Strength float64   `json:"strength"`
	Created  time.Time `json:"created"`
}

// ConsolidationEntry 整合日志条目
type ConsolidationEntry struct {
	ID               string    `json:"id"`
	CycleDate        time.Time `json:"cycleDate"`
	FactsInput       int       `json:"factsInput"`
	FactsMerged      int       `json:"factsMerged"`
	PatternsFound    int       `json:"patternsFound"`
	Contradictions   int       `json:"contradictions"`
	DurationMs       int64     `json:"durationMs"`
}

// NewFactStore 创建 L2 事实存储
func NewFactStore(persistDir string) *FactStore {
	fs := &FactStore{
		facts:      make(map[string]*MemoryFact),
		persistDir: persistDir,
	}
	if persistDir != "" {
		_ = os.MkdirAll(persistDir, 0755)
		fs.loadFromDisk()
	}
	return fs
}

// Add 添加一条记忆事实
func (fs *FactStore) Add(fact *MemoryFact) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if fact.ID == "" {
		fs.idSeq++
		fact.ID = fmt.Sprintf("fact-%s-%04d", time.Now().Format("20060102-150405"), fs.idSeq)
	}
	if fact.CreatedAt.IsZero() {
		fact.CreatedAt = time.Now()
	}
	if fact.LastAccess.IsZero() {
		fact.LastAccess = fact.CreatedAt
	}
	if fact.Strength == 0 {
		fact.Strength = 1.0
	}
	if fact.DecayRate == 0 {
		fact.DecayRate = DefaultDecayRate(fact.Category)
	}

	fs.facts[fact.ID] = fact
	fs.dirty = true

	if fact.Importance >= 0.8 {
		go fs.PersistToDisk()
	}
}

// AddConnection 添加事实间关联
func (fs *FactStore) AddConnection(factIDA, factIDB, relation string, strength float64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.connections = append(fs.connections, FactConnection{
		FactIDA:  factIDA,
		FactIDB:  factIDB,
		Relation: relation,
		Strength: strength,
		Created:  time.Now(),
	})
	fs.dirty = true
}

// Retrieve 混合检索: BM25 + 分类衰减 + 近因 + 频次
func (fs *FactStore) Retrieve(query string, topK int) []*MemoryFact {
	if topK <= 0 {
		topK = 10
	}
	queryTerms := tokenize(query)
	if len(queryTerms) == 0 {
		return nil
	}

	fs.mu.RLock()
	totalDocs := len(fs.facts)
	if totalDocs == 0 {
		fs.mu.RUnlock()
		return nil
	}

	type docInfo struct {
		fact  *MemoryFact
		terms []string
	}
	docs := make([]docInfo, 0, totalDocs)
	docFreq := make(map[string]int)
	totalTerms := 0

	for _, fact := range fs.facts {
		if fact.Archived {
			continue
		}
		if fact.Retention() < 0.05 {
			continue
		}
		contentTerms := tokenize(fact.Content)
		topicTerms := tokenize(strings.Join(fact.Topics, " "))
		allTerms := append(contentTerms, topicTerms...)
		docs = append(docs, docInfo{fact: fact, terms: allTerms})
		totalTerms += len(allTerms)
		seen := make(map[string]bool)
		for _, t := range allTerms {
			if !seen[t] {
				seen[t] = true
				docFreq[t]++
			}
		}
	}

	if len(docs) == 0 {
		fs.mu.RUnlock()
		return nil
	}
	avgDocLen := float64(totalTerms) / float64(len(docs))

	const k1 = 1.5
	const b = 0.75
	N := float64(len(docs))

	type scored struct {
		fact  *MemoryFact
		score float64
	}
	var candidates []scored

	for _, doc := range docs {
		tf := make(map[string]int)
		for _, t := range doc.terms {
			tf[t]++
		}
		docLen := float64(len(doc.terms))

		bm25 := 0.0
		matchedTerms := 0
		for _, qt := range queryTerms {
			if tf[qt] == 0 {
				continue
			}
			matchedTerms++
			n := float64(docFreq[qt])
			idf := math.Log((N-n+0.5)/(n+0.5) + 1)
			if idf < 0.01 {
				idf = 0.01
			}
			tfNorm := (float64(tf[qt]) * (k1 + 1)) / (float64(tf[qt]) + k1*(1-b+b*docLen/avgDocLen))
			bm25 += idf * tfNorm
		}

		if matchedTerms == 0 {
			continue
		}

		retention := doc.fact.Retention()
		recencyHours := time.Since(doc.fact.LastAccess).Hours()
		recencyBoost := 1.0 / (1.0 + recencyHours/168.0)
		frequencyBoost := 1.0 + math.Log(float64(doc.fact.AccessCount)+1)*0.1

		// 综合评分 = BM25 × retention × (1 + recency) × frequency
		score := bm25 * retention * (1.0 + recencyBoost) * frequencyBoost
		candidates = append(candidates, scored{doc.fact, score})
	}
	fs.mu.RUnlock()

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > topK {
		candidates = candidates[:topK]
	}

	fs.mu.Lock()
	result := make([]*MemoryFact, len(candidates))
	for i, c := range candidates {
		c.fact.Touch()
		result[i] = c.fact
	}
	fs.dirty = true
	fs.mu.Unlock()

	return result
}

// GetAll 获取所有活跃事实
func (fs *FactStore) GetAll() []*MemoryFact {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	result := make([]*MemoryFact, 0, len(fs.facts))
	for _, f := range fs.facts {
		if !f.Archived {
			result = append(result, f)
		}
	}
	return result
}

// GetByCategory 按分类检索
func (fs *FactStore) GetByCategory(cat MemoryCategory) []*MemoryFact {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	var result []*MemoryFact
	for _, f := range fs.facts {
		if f.Category == cat && !f.Archived {
			result = append(result, f)
		}
	}
	return result
}

// Count 返回活跃事实数
func (fs *FactStore) Count() int {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	count := 0
	for _, f := range fs.facts {
		if !f.Archived {
			count++
		}
	}
	return count
}

// RunDecayCycle 执行衰减周期: 遍历所有事实，归档低保留率的
func (fs *FactStore) RunDecayCycle(archiveThreshold float64) (archived int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	for _, fact := range fs.facts {
		if fact.ShouldArchive(archiveThreshold) {
			fact.Archived = true
			archived++
		}
	}
	if archived > 0 {
		fs.dirty = true
	}
	return
}

// DetectContradictions 检测同主题的矛盾事实
func (fs *FactStore) DetectContradictions() []FactConnection {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	topicFacts := make(map[string][]*MemoryFact)
	for _, f := range fs.facts {
		if f.Archived {
			continue
		}
		for _, t := range f.Topics {
			topicFacts[t] = append(topicFacts[t], f)
		}
	}

	var contradictions []FactConnection
	for _, facts := range topicFacts {
		if len(facts) < 2 {
			continue
		}
		for i := 0; i < len(facts)-1; i++ {
			for j := i + 1; j < len(facts); j++ {
				if isContradiction(facts[i].Content, facts[j].Content) {
					contradictions = append(contradictions, FactConnection{
						FactIDA:  facts[i].ID,
						FactIDB:  facts[j].ID,
						Relation: "contradicts",
						Strength: 0.8,
						Created:  time.Now(),
					})
				}
			}
		}
	}
	return contradictions
}

func isContradiction(a, b string) bool {
	aLower := strings.ToLower(a)
	bLower := strings.ToLower(b)

	fixedMarkers := []string{"已修复", "fixed", "resolved", "完成", "正常"}
	brokenMarkers := []string{"仍存在", "still", "broken", "未修复", "仍有问题"}

	aFixed, bFixed := false, false
	aBroken, bBroken := false, false

	for _, m := range fixedMarkers {
		if strings.Contains(aLower, m) {
			aFixed = true
		}
		if strings.Contains(bLower, m) {
			bFixed = true
		}
	}
	for _, m := range brokenMarkers {
		if strings.Contains(aLower, m) {
			aBroken = true
		}
		if strings.Contains(bLower, m) {
			bBroken = true
		}
	}
	return (aFixed && bBroken) || (bFixed && aBroken)
}

// FormatFactsForPrompt 格式化事实为系统提示词
func FormatFactsForPrompt(facts []*MemoryFact) string {
	if len(facts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<structured_memories>\n")
	sb.WriteString("Key facts from past sessions (organized by type):\n\n")

	catGroups := make(map[MemoryCategory][]*MemoryFact)
	for _, f := range facts {
		catGroups[f.Category] = append(catGroups[f.Category], f)
	}

	catNames := map[MemoryCategory]string{
		CategoryFact:       "Facts & Decisions",
		CategoryPreference: "Preferences & Style",
		CategoryGoal:       "Goals & Milestones",
		CategoryEvent:      "Events & Changes",
		CategoryContext:    "Recent Context",
	}

	totalBytes := 0
	maxBytes := 30 * 1024
	for _, cat := range []MemoryCategory{CategoryFact, CategoryPreference, CategoryGoal, CategoryEvent, CategoryContext} {
		group := catGroups[cat]
		if len(group) == 0 {
			continue
		}
		header := fmt.Sprintf("## %s\n", catNames[cat])
		sb.WriteString(header)
		totalBytes += len(header)
		for _, f := range group {
			content := f.Content
			if len(content) > 2048 {
				content = content[:2048] + "..."
			}
			entry := fmt.Sprintf("- %s\n", content)
			if totalBytes+len(entry) > maxBytes {
				break
			}
			sb.WriteString(entry)
			totalBytes += len(entry)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("</structured_memories>\n\n")
	return sb.String()
}

// --- 持久化 ---

func (fs *FactStore) PersistToDisk() {
	if fs.persistDir == "" {
		return
	}
	fs.mu.RLock()
	if !fs.dirty {
		fs.mu.RUnlock()
		return
	}
	facts := make([]*MemoryFact, 0, len(fs.facts))
	for _, f := range fs.facts {
		facts = append(facts, f)
	}
	conns := make([]FactConnection, len(fs.connections))
	copy(conns, fs.connections)
	fs.mu.RUnlock()

	data, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		log.Printf("[FactStore] 序列化失败: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(fs.persistDir, "memory_facts.json"), data, 0644); err != nil {
		log.Printf("[FactStore] 持久化失败: %v", err)
		return
	}

	if len(conns) > 0 {
		connData, _ := json.MarshalIndent(conns, "", "  ")
		_ = os.WriteFile(filepath.Join(fs.persistDir, "memory_connections.json"), connData, 0644)
	}

	fs.mu.Lock()
	fs.dirty = false
	fs.mu.Unlock()
}

func (fs *FactStore) loadFromDisk() {
	factsPath := filepath.Join(fs.persistDir, "memory_facts.json")
	data, err := os.ReadFile(factsPath)
	if err != nil {
		return
	}
	var facts []*MemoryFact
	if err := json.Unmarshal(data, &facts); err != nil {
		log.Printf("[FactStore] 加载失败: %v", err)
		return
	}
	loaded := 0
	for _, f := range facts {
		if !f.Archived && f.Retention() >= 0.02 {
			fs.facts[f.ID] = f
			loaded++
		}
	}
	log.Printf("[FactStore] 从磁盘恢复 %d 条事实 (跳过 %d 条)", loaded, len(facts)-loaded)

	connPath := filepath.Join(fs.persistDir, "memory_connections.json")
	if connData, err := os.ReadFile(connPath); err == nil {
		_ = json.Unmarshal(connData, &fs.connections)
	}
}

// LogConsolidation 记录整合日志
func (fs *FactStore) LogConsolidation(entry ConsolidationEntry) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.consolidationLog = append(fs.consolidationLog, entry)
	if len(fs.consolidationLog) > 100 {
		fs.consolidationLog = fs.consolidationLog[len(fs.consolidationLog)-100:]
	}
}

// Stats 统计信息
func (fs *FactStore) Stats() FactStoreStats {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	stats := FactStoreStats{
		TotalFacts:       len(fs.facts),
		TotalConnections: len(fs.connections),
		CategoriesCount:  make(map[MemoryCategory]int),
	}
	for _, f := range fs.facts {
		if f.Archived {
			stats.ArchivedFacts++
		} else {
			stats.ActiveFacts++
		}
		if f.Evergreen {
			stats.EvergreenFacts++
		}
		stats.CategoriesCount[f.Category]++
	}
	return stats
}

// FactStoreStats 统计
type FactStoreStats struct {
	TotalFacts       int                        `json:"totalFacts"`
	ActiveFacts      int                        `json:"activeFacts"`
	ArchivedFacts    int                        `json:"archivedFacts"`
	EvergreenFacts   int                        `json:"evergreenFacts"`
	TotalConnections int                        `json:"totalConnections"`
	CategoriesCount  map[MemoryCategory]int     `json:"categoriesCount"`
}

