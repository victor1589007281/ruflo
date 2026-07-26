// Package memory — 多层记忆架构 (v2: 持久化 + 衰减召回)。
//
// 设计参考:
//   - MemGPT / Letta: 三层记忆模型 (Working → Episodic → Semantic)
//   - Ebbinghaus 遗忘曲线 + 间隔重复: 自然衰减，复习增强
//   - Generative Agents (Park et al. 2023): 重要性 × 近因 × 相关性 三因子召回
//   - RAPTOR (Sarthi et al. 2024): 递归抽象 + 树状检索
//
// v2 改进 (解决"失忆"根因):
//
//  1. 移除 2h CreatedAt 硬截断 → 完全依赖 Ebbinghaus Retention() 做软衰减
//
//  2. 新增磁盘持久化 (JSON) → 进程重启后恢复记忆
//
//  3. 团队产出写入高权重 (Importance=0.9) 记忆 → 团队名可被检索
//
//  4. 多路检索: BM25 + 实体匹配 bonus → 提高召回质量
//
//  5. 重要性分级: team_result(0.9) > pre_compact(0.7) > extraction(0.6) > agent(0.5)
//
//     ┌──────────────────────────────────────────────────┐
//     │  Working Memory (工作记忆 / 短期)                 │
//     │  = QueryEngine.Messages (当前对话上下文)          │
//     └──────────────────┬───────────────────────────────┘
//     │ compact → extractKeyFacts
//     ┌──────────────────▼───────────────────────────────┐
//     │  Episodic Memory (情景记忆 / 中期)  ← 本文件     │
//     │  = TieredStore (BM25 + Ebbinghaus)               │
//     │  持久化: episodic_memory.json                     │
//     └──────────────────┬───────────────────────────────┘
//     │ Dreaming 整理
//     ┌──────────────────▼───────────────────────────────┐
//     │  Semantic Memory (语义记忆 / 长期)                │
//     │  = .claude/memory/*.md                            │
//     └──────────────────────────────────────────────────┘
package memory

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
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

// Touch 标记记忆被访问 (增强间隔重复效应)。
//
// ⚠️ 调用方必须持 TieredStore 的**写锁**: 它改的两个字段会被 PersistToDisk 读到
// (见那里的注释)。`Retrieve` 是这么做的。
func (e *MemoryEntry) Touch() {
	e.AccessCount++
	e.LastAccess = time.Now()
}

// snapshot 返回一份与原对象**无共享**的值拷贝, 供锁外序列化使用。
//
// Topics 单独拷一份: 浅拷贝会让快照与原对象共享同一个底层数组, 于是"按值快照"这件事
// 在唯一的引用字段上恰好不成立 —— 这类半个深拷贝比不拷更难查, 因为大部分字段看着是安全的。
func (e *MemoryEntry) snapshot() MemoryEntry {
	c := *e
	if e.Topics != nil {
		c.Topics = make([]string, len(e.Topics))
		copy(c.Topics, e.Topics)
	}
	return c
}

// TieredStore 多层记忆存储 (v2: 含磁盘持久化)。
// 进程级共享，所有 Session 共享同一个实例。
type TieredStore struct {
	mu       sync.RWMutex
	episodic map[string]*MemoryEntry // id → entry
	idSeq    int
	// ForgetThreshold 低于此保留度的记忆将被标记为可清理
	ForgetThreshold float64
	// persistPath 记忆持久化文件路径 (空则不持久化)
	persistPath string
	// dirty 标记是否有未持久化的变更
	dirty bool
	// bg 跟踪 Add 内部起的**后台落盘** goroutine, 使本存储可 join (见 WaitPersist)。
	//
	// 为什么需要它: `Add` 对高权重记忆 fire-and-forget 一次 PersistToDisk。生产上无所谓
	// (进程退出即止), 但测试里 `t.TempDir()` 的清理会与那次写盘竞态 —— 表现是
	// "RemoveAll: directory not empty" 的**间歇性**失败, 且看起来与被测逻辑毫无关系。
	// 这与 pkg/dreaming 的 bg 是同一条理由 (那边已因此吃过一次亏)。
	bg sync.WaitGroup
}

// WaitPersist 等待 Add 触发的后台落盘全部结束。
//
// 面向测试与优雅关闭: 调用它的时候**必须已经没有并发的 Add** —— `sync.WaitGroup` 规定
// "计数器为 0 时开始的 Add 必须发生在 Wait 之前", 违反即数据竞争 (pkg/dreaming 的
// WaitBackground 正是踩了这一条: 外层 goroutine 没登记, Wait 先看到 0 就早退, 随后
// 内层 Add 与它并发)。
func (s *TieredStore) WaitPersist() { s.bg.Wait() }

// NewTieredStore 创建多层记忆存储
func NewTieredStore() *TieredStore {
	return &TieredStore{
		episodic:        make(map[string]*MemoryEntry),
		ForgetThreshold: 0.3,
	}
}

// NewTieredStoreWithPersist 创建带磁盘持久化的记忆存储。
// 启动时自动从文件加载历史记忆。
func NewTieredStoreWithPersist(dir string) *TieredStore {
	s := &TieredStore{
		episodic:        make(map[string]*MemoryEntry),
		ForgetThreshold: 0.3,
	}
	if dir != "" {
		_ = os.MkdirAll(dir, 0755)
		s.persistPath = filepath.Join(dir, "episodic_memory.json")
		s.loadFromDisk()
	}
	return s
}

// loadFromDisk 从磁盘加载记忆 (启动时调用)。
func (s *TieredStore) loadFromDisk() {
	if s.persistPath == "" {
		return
	}
	data, err := os.ReadFile(s.persistPath)
	if err != nil {
		return // 文件不存在是正常情况
	}
	var entries []*MemoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("[Memory] 加载记忆文件失败: %v", err)
		return
	}
	for _, e := range entries {
		if e.Retention() >= s.ForgetThreshold*0.5 {
			s.episodic[e.ID] = e
			s.idSeq++
		}
	}
	log.Printf("[Memory] 从磁盘恢复 %d 条记忆 (跳过 %d 条已遗忘)", len(s.episodic), len(entries)-len(s.episodic))
}

// PersistToDisk 将记忆持久化到磁盘。定期调用或在写入高权重记忆后调用。
//
// ⚠️ **快照必须按值拷贝, 不能只拷指针** —— 这里曾是一个真实的数据竞争 (由 `-race`
// 在 tests/eval 里抓到, 三条 WARNING: DATA RACE):
//
//	Add() 对高权重记忆 `go s.PersistToDisk()`  ← 后台 goroutine 序列化 entries
//	Retrieve() 在写锁内 `c.entry.Touch()`      ← 同时改 AccessCount / LastAccess
//
// 改造前只在 RLock 里拷**指针切片**, 然后**在锁外**对指针指向的对象做 JSON 序列化,
// 于是序列化正在读的 entry 可以被 Touch 并发改写。后果不只是 race 检测器报警:
// `time.Time` 是多字长值, 撕裂读能写出一个**无意义的时间戳**落到磁盘, 而下次
// LoadFromDisk 会拿它去算 Retention() —— 一条记忆可能因此被判成早该遗忘并**丢弃**。
//
// 为什么不是"把序列化搬进 RLock" (更短的改动): 那会把整个记忆库的 JSON 编码 + 写盘
// 时间都攥在读锁里, 而 Retrieve 要拿写锁 —— 记忆检索在交付主路径上, 会被写盘卡住。
// 按值快照的代价只是一次浅拷贝 (MemoryEntry 全是值字段, Topics 单独拷一份)。
func (s *TieredStore) PersistToDisk() {
	if s.persistPath == "" {
		return
	}
	s.mu.RLock()
	if !s.dirty {
		s.mu.RUnlock()
		return
	}
	entries := make([]MemoryEntry, 0, len(s.episodic))
	for _, e := range s.episodic {
		entries = append(entries, e.snapshot())
	}
	s.mu.RUnlock()

	// 序列化的是快照, 与 s.episodic 里的对象再无共享 ⇒ 锁外编码是安全的。
	// (JSON 形态与改造前逐字节一致: []MemoryEntry 与 []*MemoryEntry 编码结果相同。)
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		log.Printf("[Memory] 序列化失败: %v", err)
		return
	}
	if err := os.WriteFile(s.persistPath, data, 0644); err != nil {
		log.Printf("[Memory] 持久化失败: %v", err)
		return
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
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
	s.dirty = true

	// 高权重记忆立即持久化 (team_result, manual)。
	// 登记进 bg 使其可 join —— 理由见 bg 字段的注释 (Add 在 s.mu 内, 计数器递增发生在
	// 任何 WaitPersist 之前, 契约成立)。
	if entry.Importance >= 0.8 {
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			s.PersistToDisk()
		}()
	}
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
				idf = 0.01 // IDF 下限: 防止所有文档包含该词时分数归零
			}
			tfNorm := (float64(tf[qt]) * (k1 + 1)) / (float64(tf[qt]) + k1*(1-b+b*docLen/avgDocLen))
			bm25 += idf * tfNorm
		}

		if matchedTerms == 0 {
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

// tokenize 中英文混合分词。
// 英文: 按空格/标点分割。中文: unigram + bigram (无分词器也可有效检索)。
// 关键: CJK 和 Latin 必须分开，否则会形成跨语言巨型 token 导致 BM25 失效。
func tokenize(text string) []string {
	text = strings.ToLower(text)
	var tokens []string
	var latin strings.Builder
	var cjkChars []rune

	flushLatin := func() {
		if latin.Len() > 1 {
			tokens = append(tokens, latin.String())
		}
		latin.Reset()
	}
	flushCJK := func() {
		for _, c := range cjkChars {
			tokens = append(tokens, string(c))
		}
		for i := 0; i+1 < len(cjkChars); i++ {
			tokens = append(tokens, string(cjkChars[i])+string(cjkChars[i+1]))
		}
		cjkChars = cjkChars[:0]
	}

	for _, r := range text {
		if r >= 0x4e00 && r <= 0x9fff {
			flushLatin()
			cjkChars = append(cjkChars, r)
		} else if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			flushCJK()
			latin.WriteRune(r)
		} else {
			flushLatin()
			flushCJK()
		}
	}
	flushLatin()
	flushCJK()
	return tokens
}
