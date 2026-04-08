// Package memory — 多层记忆架构。
// 对应 TS: memdir/ (auto-memory) + extractMemories + autoDream
//
// 三层记忆模型 (业界最佳实践 — MemGPT / Letta 架构):
//
//	┌──────────────────────────────────────────────────┐
//	│  Working Memory (工作记忆 / 短期)                 │
//	│  = QueryEngine.Messages (当前对话上下文)          │
//	│  容量: 模型 context window                        │
//	│  生命期: 单次对话                                 │
//	└──────────────────┬───────────────────────────────┘
//	                   │ 压缩时 extractBeforeCompact
//	┌──────────────────▼───────────────────────────────┐
//	│  Episodic Memory (情景记忆 / 中期)                │
//	│  = 每轮对话提取的关键事实                         │
//	│  容量: 内存 (per-session, 可持久化)               │
//	│  生命期: 跨多轮对话, 受遗忘曲线衰减              │
//	│  触发: 每轮 query 结束时自动提取                   │
//	└──────────────────┬───────────────────────────────┘
//	                   │ Dreaming 整理
//	┌──────────────────▼───────────────────────────────┐
//	│  Semantic Memory (语义记忆 / 长期)                │
//	│  = .claude/memory/*.md 文件                       │
//	│  容量: 磁盘 (索引 + 主题文件)                     │
//	│  生命期: 永久, 由 Dreaming 定期整理               │
//	│  召回: 每轮 query 开始时按相关性加载              │
//	└──────────────────────────────────────────────────┘
//
// 遗忘曲线 (Ebbinghaus + 间隔重复):
//
//	retention(t) = importance × e^(-λ×t / (1 + ln(accessCount+1)))
//
//	- importance: 记忆重要性 (0.0-1.0)
//	- λ: 衰减速率 (默认 0.1)
//	- t: 距上次访问的小时数
//	- accessCount: 被召回的次数 (间隔重复效应)
//
//	每次被召回时 accessCount++, 衰减变慢。
//	retention < threshold (默认 0.3) 时, 记忆在下次 Dreaming 时被清理。
package memory

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryEntry 单条记忆
type MemoryEntry struct {
	ID          string    `json:"id"`
	Content     string    `json:"content"`
	Topics      []string  `json:"topics,omitempty"`
	Source      string    `json:"source"` // "extraction", "consolidation", "manual", "pre_compact"
	Importance  float64   `json:"importance"`
	AccessCount int       `json:"accessCount"`
	CreatedAt   time.Time `json:"createdAt"`
	LastAccess  time.Time `json:"lastAccess"`
	ChatID      string    `json:"chatId,omitempty"`
}

// Retention 计算当前保留度 (Ebbinghaus 遗忘曲线 + 间隔重复)
func (e *MemoryEntry) Retention() float64 {
	hoursSinceAccess := time.Since(e.LastAccess).Hours()
	if hoursSinceAccess < 0 {
		hoursSinceAccess = 0
	}
	decay := 0.1
	spacedRepetition := 1.0 + math.Log(float64(e.AccessCount)+1)
	return e.Importance * math.Exp(-decay*hoursSinceAccess/spacedRepetition)
}

// Touch 标记记忆被访问 (增强间隔重复效应)
func (e *MemoryEntry) Touch() {
	e.AccessCount++
	e.LastAccess = time.Now()
}

// TieredStore 多层记忆存储。
// 进程级共享，所有 Session 共享同一个实例。
type TieredStore struct {
	mu       sync.RWMutex
	episodic map[string]*MemoryEntry // id → entry
	idSeq    int
	// ForgetThreshold 低于此保留度的记忆将被标记为可清理
	ForgetThreshold float64
}

// NewTieredStore 创建多层记忆存储
func NewTieredStore() *TieredStore {
	return &TieredStore{
		episodic:        make(map[string]*MemoryEntry),
		ForgetThreshold: 0.3,
	}
}

// Add 添加一条记忆
func (s *TieredStore) Add(entry *MemoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.ID == "" {
		s.idSeq++
		entry.ID = time.Now().Format("20060102-150405") + "-" + strings.Repeat("0", 4-len(itoa(s.idSeq))) + itoa(s.idSeq)
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.LastAccess.IsZero() {
		entry.LastAccess = entry.CreatedAt
	}
	if entry.Importance == 0 {
		entry.Importance = 0.5
	}
	s.episodic[entry.ID] = entry
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// Retrieve 按相关性检索记忆 (BM25 Okapi + 遗忘曲线 + 近因加权)。
// 业界标准: BM25 是信息检索领域最成熟的算法, 比 Jaccard 更准确。
//
// 评分公式: score = BM25(query, doc) × retention × (1 + recencyBoost)
//
// BM25 Okapi:
//
//	score(D,Q) = Σ IDF(qi) × f(qi,D)×(k1+1) / (f(qi,D) + k1×(1 - b + b×|D|/avgdl))
//	IDF(qi) = ln((N - n(qi) + 0.5) / (n(qi) + 0.5) + 1)
func (s *TieredStore) Retrieve(query string, topK int) []*MemoryEntry {
	if topK <= 0 {
		topK = 5
	}
	queryTerms := tokenize(query)
	if len(queryTerms) == 0 {
		return nil
	}

	s.mu.RLock()

	// Pass 1: 构建 BM25 所需的全局统计
	totalDocs := len(s.episodic)
	if totalDocs == 0 {
		s.mu.RUnlock()
		return nil
	}

	type docInfo struct {
		entry *MemoryEntry
		terms []string
	}
	docs := make([]docInfo, 0, totalDocs)
	docFreq := make(map[string]int) // term → 含该词的文档数
	totalTerms := 0

	for _, entry := range s.episodic {
		if entry.Retention() < s.ForgetThreshold*0.5 {
			continue
		}
		contentTerms := tokenize(entry.Content)
		topicTerms := tokenize(strings.Join(entry.Topics, " "))
		allTerms := append(contentTerms, topicTerms...)
		docs = append(docs, docInfo{entry: entry, terms: allTerms})
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
		s.mu.RUnlock()
		return nil
	}
	avgDocLen := float64(totalTerms) / float64(len(docs))

	// Pass 2: BM25 评分
	const k1 = 1.5
	const b = 0.75
	N := float64(len(docs))

	type scored struct {
		entry *MemoryEntry
		score float64
	}
	var candidates []scored

	for _, doc := range docs {
		// 计算词频
		tf := make(map[string]int)
		for _, t := range doc.terms {
			tf[t]++
		}
		docLen := float64(len(doc.terms))

		bm25 := 0.0
		for _, qt := range queryTerms {
			if tf[qt] == 0 {
				continue
			}
			n := float64(docFreq[qt])
			idf := math.Log((N-n+0.5)/(n+0.5) + 1)
			tfNorm := (float64(tf[qt]) * (k1 + 1)) / (float64(tf[qt]) + k1*(1-b+b*docLen/avgDocLen))
			bm25 += idf * tfNorm
		}

		if bm25 < 0.01 {
			continue
		}

		retention := doc.entry.Retention()
		recencyHours := time.Since(doc.entry.LastAccess).Hours()
		recencyBoost := 1.0 / (1.0 + recencyHours/168.0) // 一周半衰期

		score := bm25 * retention * (1.0 + recencyBoost)
		candidates = append(candidates, scored{doc.entry, score})
	}
	s.mu.RUnlock()

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	if len(candidates) > topK {
		candidates = candidates[:topK]
	}

	s.mu.Lock()
	result := make([]*MemoryEntry, len(candidates))
	for i, c := range candidates {
		c.entry.Touch()
		result[i] = c.entry
	}
	s.mu.Unlock()

	return result
}

// GetAll 获取所有记忆 (用于 Dreaming 整理)
func (s *TieredStore) GetAll() []*MemoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*MemoryEntry, 0, len(s.episodic))
	for _, e := range s.episodic {
		result = append(result, e)
	}
	return result
}

// Prune 清理遗忘的记忆 (retention < ForgetThreshold)
func (s *TieredStore) Prune() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	pruned := 0
	for id, entry := range s.episodic {
		if entry.Retention() < s.ForgetThreshold {
			delete(s.episodic, id)
			pruned++
		}
	}
	return pruned
}

// Count 返回记忆条数
func (s *TieredStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.episodic)
}

// FormatForPrompt 格式化检索到的记忆为系统提示词片段。
// 对应 TS: readMemoriesForSurfacing + 4096 bytes/file cap
func FormatForPrompt(entries []*MemoryEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<relevant_memories>\n")
	sb.WriteString("The following memories from past sessions may be relevant:\n\n")
	totalBytes := 0
	maxBytes := 60 * 1024 // 60KB session budget (对应 TS: RELEVANT_MEMORIES_CONFIG.MAX_SESSION_BYTES)
	for _, e := range entries {
		content := e.Content
		if len(content) > 4096 {
			content = content[:4096] + "...(truncated)"
		}
		entry := "- " + content + "\n"
		if totalBytes+len(entry) > maxBytes {
			break
		}
		sb.WriteString(entry)
		totalBytes += len(entry)
	}
	sb.WriteString("</relevant_memories>\n\n")
	return sb.String()
}

// tokenize 简单分词 (小写 + 按空格/标点分割)
func tokenize(text string) []string {
	text = strings.ToLower(text)
	var tokens []string
	var current strings.Builder
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 0x4e00 && r <= 0x9fff {
			current.WriteRune(r)
		} else {
			if current.Len() > 1 {
				tokens = append(tokens, current.String())
			}
			current.Reset()
		}
	}
	if current.Len() > 1 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

