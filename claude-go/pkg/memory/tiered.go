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
	"context"
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

	"github.com/anthropic/claude-go/pkg/textutil"
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
	// Embedding 可选向量 (embed.go 可选后端写入; 为空时检索退化为纯 BM25)。
	Embedding []float32 `json:"embedding,omitempty"`
	// Ref 指向本条记忆**未截断原文**的句柄 (AddWithOriginal 存下后回填, 见 blob.go)。
	// 空表示原文没有超出 Content, 没有额外可取回的东西。
	//
	// 为什么要有它: Content 是**面向提示词**的有界摘要 —— 团队产出的每个阶段在
	// 写入前就被 truncateResult 砍到几百字符, 再被 FormatForPrompt 按 4096 截一刀。
	// 摘要够让模型判断"这条记忆讲的是什么", 却不够让它照着做。原文留在快照里,
	// 模型先用摘要认出相关性, 再按 Ref 取回全文 —— 渐进披露: 检索阶段只付摘要的钱,
	// 真要用了才付全文的钱。
	Ref string `json:"ref,omitempty"`

	// —— H2-4 冲突消解 (见 resolve.go) ——
	//
	// SupersededBy 取代了本条的记忆 ID。**非空 = 本条已归档, 检索不再返回它**,
	// 但条目本身留在库里 (与 Prune 的删除不同): 一条记忆为什么消失, 必须查得到。
	SupersededBy string `json:"supersededBy,omitempty"`
	// SupersededAt 被取代的时刻 (审计用; SupersededBy 为空时无意义)。
	SupersededAt time.Time `json:"supersededAt,omitempty"`
	// Supersedes 本条取代掉的记忆 ID 列表 (与 SupersededBy 构成双向链)。
	Supersedes []string `json:"supersedes,omitempty"`
	// Contradicts 与本条矛盾、但**没有被取代**的记忆 ID (置信不足 / 对方是永久记忆)。
	// 非空意味着提示词里可能同时出现两条互斥陈述 —— 这是如实留证, 不是正常状态。
	Contradicts []string `json:"contradicts,omitempty"`
	// Evergreen 永久豁免遗忘与取代 (与 MemoryFact.Evergreen 同义)。
	//
	// 一条被标为永久的记忆**不会**被新记忆顶掉 —— 这是人工声明"这条永远算数"的
	// 唯一出口, 缺了它, 守门就只能靠 Importance, 而 Importance 是启发式打的分。
	Evergreen bool `json:"evergreen,omitempty"`
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
	if e.Embedding != nil {
		c.Embedding = make([]float32, len(e.Embedding))
		copy(c.Embedding, e.Embedding)
	}
	return c
}

// TieredStore 多层记忆存储 (v2: 含磁盘持久化)。
// 进程级共享，所有 Session 共享同一个实例。
type TieredStore struct {
	mu       sync.RWMutex
	episodic map[string]*MemoryEntry // id → entry
	// embedder 可选向量化后端 (默认 nil = 纯 BM25, 手册 7.0.6 缺口的可选修复)。
	embedder Embedder
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

	// lastResolve 最近一次带守门入库的消解读数 (见 resolve.go 的 LastResolveStats)。
	// 由 mu 保护。
	lastResolve ResolveStats
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
// SetEmbedder 装配可选向量化后端 (nil 即纯 BM25)。在首批 Add 前调用。
func (s *TieredStore) SetEmbedder(e Embedder) { s.embedder = e }

// Add 添加一条记忆 (不带原文快照)。等价于 AddWithOriginal(entry, "")。
func (s *TieredStore) Add(entry *MemoryEntry) { s.AddWithOriginal(entry, "") }

// AddWithOriginal 添加一条记忆, 并把**未截断原文**另存为快照、把句柄写进 entry.Ref。
//
// original 传空, 或没有比 Content 长出足够多时, 一个字节都不落 (Ref 留空)。
//
// 为什么原文要单独传进来而不是从 Content 里推断: Content 是**已经截过的摘要**,
// 原文在截断那一刻就丢了 —— Add 无论如何都还原不出它。所以只有调用方能提供,
// 而调用方恰好是唯一同时握着两份文本的地方 (见 runPhaseMemory)。
// 实现体已挪到 resolve.go 的 addWithOriginal (守门与非守门两条路共用同一段入库逻辑,
// 只有"扫不扫矛盾"这一个 bit 不同 —— 抄成两份的话, 向量化/快照/落盘这三件事早晚会
// 分叉, 而分叉的表现是"某一条路径上的记忆不持久化", 极难归因)。
func (s *TieredStore) AddWithOriginal(entry *MemoryEntry, original string) {
	s.addWithOriginal(entry, original, false)
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
		// 已被取代的记忆**不进检索** (H2-4)。
		//
		// 这一条是整条链上唯一会改变模型所见的地方 —— 归档只是把标记写进库里,
		// 真正"消解"掉冲突的动作在这里: 不排除的话, 被推翻的旧陈述照样会被 BM25
		// 捞出来进提示词, 与取代它的新陈述并排出现, 问题一点没解决。
		//
		// 删掉条目 (而不是只过滤) 会让"这条记忆为什么不见了"永久无人可查, 所以
		// 条目留在库里, 只在检索这一处让路 —— 审计路径 (GetAll / Get) 照常看得见。
		if entry.SupersededBy != "" {
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

	// 可选向量化混合 (手册 7.0.6): embedder 装配且查询向量化成功时,
	// final = 0.6×BM25(归一) + 0.4×cosine; 任何一步失败都静默退回纯 BM25。
	if s.embedder != nil && len(candidates) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		qv, err := s.embedder.Embed(ctx, query)
		cancel()
		if err == nil {
			maxBM := 0.0
			for _, c := range candidates {
				if c.score > maxBM {
					maxBM = c.score
				}
			}
			if maxBM > 0 {
				for i, c := range candidates {
					cos := 0.0
					if len(c.entry.Embedding) > 0 {
						cos = Cosine(qv, c.entry.Embedding)
						if cos < 0 {
							cos = 0
						}
					}
					candidates[i].score = 0.6*(c.score/maxBM) + 0.4*cos
				}
			}
		}
	}

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

// Get 按 ID 取一条记忆的**值拷贝** (不存在时 ok=false)。
//
// 返回值拷贝而不是 `*MemoryEntry`: 调用方 (memory_read 工具) 拿到指针后会在锁外
// 读它, 而 `Retrieve` / `Touch` 会在写锁内改同一条对象的 AccessCount/LastAccess
// —— memory 包刚因为这类"锁外读活对象"吃过一次 -race (见 snapshot 的注释)。
// 拷贝一次比每次都去论证"我只读了不会变的字段"便宜得多。
func (s *TieredStore) Get(id string) (MemoryEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.episodic[id]
	if !ok {
		return MemoryEntry{}, false
	}
	return e.snapshot(), true
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
		// 单条上限 (字符数, 不是字节数): 中文一个字 3 字节, 按字节切会把字劈成
		// 半个非法 UTF-8 序列 —— 出站序列化时被替换成 U+FFFD, 同一段记忆在不同
		// 轮次可能渲染出不同字节, 记忆块之后的前缀缓存随之失效。
		content := textutil.Truncate(e.Content, 4096, "...(truncated)")
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

// IndexPreviewRunes 是索引里每条记忆保留的正文预览长度 (字符数)。
// 定在 120: 够看出"这条讲的是什么", 又不至于让索引本身变成第二份正文。
const IndexPreviewRunes = 120

// FormatIndexForPrompt 把检索结果格式化为**索引**而不是正文。
//
// 与 FormatForPrompt 的关系: 两者回答的是同一个问题 ("把哪些记忆端给模型"),
// 但付的钱差一个数量级。实测这套部署里 top-7 的正文合计 ~19K 字符 —— 占掉系统
// 提示的 2/3 —— 而其中 5 条是按表面词重叠捞上来的别的业务的记忆。索引版本把这
// 同一批结果压到 ~1K 字符, 模型认出哪条相关之后再按 id 取全文。
//
// 为什么要带 id 而不是只给标题: 模型必须有个**稳定的句柄**去调 memory_read。
// 标题会重复、会变, id 是落盘时定下的。
//
// 空结果返回空串 (与 FormatForPrompt 同约定): 调用方据此决定要不要注入这个块。
func FormatIndexForPrompt(entries []*MemoryEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<memory_index>\n")
	sb.WriteString(fmt.Sprintf("%d memories from past sessions may be relevant (titles only).\n", len(entries)))
	sb.WriteString("Call memory_read with an id to get the full text, or memory_search to re-query with other keywords.\n\n")
	for _, e := range entries {
		preview := textutil.Truncate(oneLine(e.Content), IndexPreviewRunes, "…")
		topics := ""
		if len(e.Topics) > 0 {
			topics = " [" + strings.Join(e.Topics, ", ") + "]"
		}
		// 有 Ref 的标注可得全文 —— 模型据此知道这条**值得**取; 没有的说明
		// 正文即全部, 取了也只是同一段文字。
		full := ""
		if e.Ref != "" {
			full = " full=yes"
		}
		sb.WriteString(fmt.Sprintf("- id=%s (%s)%s%s: %s\n",
			e.ID, e.CreatedAt.Format("2006-01-02"), topics, full, preview))
	}
	sb.WriteString("</memory_index>\n\n")
	return sb.String()
}

// oneLine 把多行正文压成一行: 索引按条占一行, 换行会把"一条"读成"好几条"。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
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
