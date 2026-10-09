// resolve.go —— L1 (TieredStore) 侧的记忆冲突结构化消解 (H2-4)。
//
// # 缺口在哪
//
// 冲突消解在 FactStore 里已经实现了 (store.go 的 AddWithGate, 三条守门语义 + 裁决
// 记录), 但 L1 的 TieredStore **一条都没有**: 它是个纯粹的追加型 map, `Add` 进去的
// 东西永远都在, 直到 Retention 掉到 ForgetThreshold 之下被 Prune 清掉。
//
// 于是"决定不写 x.go 了"与三天前的"必须写 x.go"会**同时被 BM25 检索出来、同时进
// 提示词**。对长程任务这是最坏的一种污染: 模型不是在两个选项里挑, 而是同时看见两条
// 互相否定的指令, 而它们看起来同样权威、同样"来自过去的自己"。FactStore 的
// FormatFactsForPrompt 与 L1 的 FormatForPrompt 是两条独立的上行路径, 只修一条
// 不解决问题 —— 这正是本文件存在的理由。
//
// 另外 Ingestor.IngestText (自动摄入, pre_compact 与团队结果都走它) 当时调的是
// `factStore.Add` 而不是 `AddWithGate`, 等于把已经写好的守门整个绕过去了。那条在
// 同一批里一起修。
//
// # 为什么不上向量
//
// 方案 §21.4.7 明确排除。这里的判据与 FactStore 完全一致: **同主题 + 标记词命中**
// (isContradiction) —— "已修复/完成/正常" 对 "仍存在/未修复/broken"。它是机械的、
// 可解释的、零依赖的, 而且**能说清为什么**: 一条被取代的记忆上留着 SupersededBy,
// 顺着 ID 就能读到它的前后代。
//
// 结构性判据必然有覆盖不到的地方 (两条用词完全不同的矛盾陈述不会被抓住)。那个缺口
// 是**已知且被计数的** (见 ResolveStats.Unresolved), 不是被假装不存在的: 判据宁可漏,
// 不可错 —— 把两条其实不矛盾的记忆判成矛盾会直接抹掉一条真实记忆, 而漏判只是退回
// 今天的行为。
package memory

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

// ResolveStats 一次入库的消解读数。
//
// **每一条都要能被读出来**, 包括"什么都没发生"的那次: 只报"取代了几条"的组件,
// 无冲突入库与"守门根本没跑"逐字节相同 —— 而后者意味着两条矛盾记忆又同时进了
// 提示词, 只是没人看得见。
type ResolveStats struct {
	// Checked 候选被扫描的活跃记忆条数 (0 = 库里没东西可矛盾, 与"没扫"必须分得开)。
	Checked int
	// Contradicts 判定为矛盾、且**可以**被取代的条数。
	Contradicts int
	// Superseded 实际归档掉的条数 (>0 时才知道这次入库真的换掉了东西)。
	Superseded int
	// ContradictsEvergreen 矛盾对象是永久记忆因而**不可取代**的条数。
	//
	// 与 Superseded 分开报: 两者都是"发现了矛盾", 但一个换了、一个只能共存。
	// 合成一个数会让"永久记忆正在被反复顶撞"这件事完全不可见。
	ContradictsEvergreen int
}

// AddWithGate 带守门语义地写入一条 L1 记忆, 返回裁决。
//
// 与 FactStore.AddWithGate **同一套语义** (刻意如此 —— 两条上行路径的守门判据若
// 分叉, 同一条记忆在两条路径上会得到不同裁决):
//
//	GateAppended   无矛盾 → 直接追加
//	GateSuperseded 有矛盾 + 新记忆 Importance >= ReflectThreshold + 旧记忆非永久
//	               → 旧记忆归档 (SupersededBy 指向新记忆), 新记忆入库
//	GateKeptBoth   其余矛盾情况 (置信不足, 或矛盾对象是永久记忆)
//	               → 两条都留, 记 Contradicts 留证
//
// 返回裁决而不是只改状态: 调用方 (Ingestor / 引擎) 靠它决定要不要记一笔账, 而
// "这次入库换掉了什么"在只看库内容时是**反推不出来**的 (被取代的条目还在库里,
// 只是标了 SupersededBy —— 那是它对**自己**的交代, 不是这次操作对它的交代)。
func (s *TieredStore) AddWithGate(entry *MemoryEntry) GateDecision {
	return s.addWithOriginal(entry, "", true)
}

// AddWithGateAndOriginal 同 AddWithGate, 并额外把未截断原文存为快照。
//
// 两个入口 (带不带原文) 都保留门: 自动摄入 (Ingestor) 走的是不带原文那条, 而
// 团队结果/压缩前摘要走带原文那条 —— 后者恰恰是最需要守门的 (它们的 Content 是
// 被截过的摘要, 冲突往往只剩关键词还认得出来)。
func (s *TieredStore) AddWithGateAndOriginal(entry *MemoryEntry, original string) GateDecision {
	return s.addWithOriginal(entry, original, true)
}

// LastResolveStats 返回最近一次**带守门**入库的读数 (nil 安全, 未守门过时 Checked=0)。
//
// 存在的理由与 H2-3 的 LastSweep 相同: 日志只在真取代了东西时打, 所以"守门跑了但
// 没有矛盾"与"守门从没跑过"必须有一个地方能分开问。
func (s *TieredStore) LastResolveStats() ResolveStats {
	if s == nil {
		return ResolveStats{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastResolve
}

// addWithOriginal 是三个公开入口的实现。
//
// gate 为 false 时**一次矛盾扫描都不做** (而不是扫完忽略结果): 不守门的语义是
// "原样追加", 扫描只为时报数就是在给一条没人读的路径付 CPU。
func (s *TieredStore) addWithOriginal(entry *MemoryEntry, original string, gate bool) GateDecision {
	if entry == nil {
		return GateAppended
	}
	// 向量化与原文快照都在**锁外** (10s 级网络调用 / 文件 I/O 不能持锁) —— 与
	// 改造前一致, 这段顺序不能动。
	if s.embedder != nil && len(entry.Embedding) == 0 && strings.TrimSpace(entry.Content) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		if vec, err := s.embedder.Embed(ctx, entry.Content); err == nil {
			entry.Embedding = vec
		}
		cancel()
	}
	// 原文快照也在锁外: 写文件是 I/O, 不能持锁 (与上面同理)。这是唯一的收口点,
	// 所以"存不存快照"的判断只写一遍, 各调用方不必各自记得。
	if utf8.RuneCountInString(original) > utf8.RuneCountInString(entry.Content)+blobMinRunes {
		entry.Ref = s.putOriginal(original)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.fillDefaultsLocked(entry)

	if !gate {
		s.episodic[entry.ID] = entry
		s.dirty = true
		s.persistIfHeavyLocked(entry)
		return GateAppended
	}

	// —— 守门: 扫描与裁决必须在同一把锁里完成 ——
	//
	// 与 FactStore.AddWithGate 同一条理由: 拆成"调用方先查、再写"会在并发整合下
	// 出现 check-then-act 竞态 (两条互相矛盾的记忆各自扫到"没有矛盾", 双双入库)。
	decision, st := s.resolveConflictsLocked(entry)
	s.lastResolve = st
	s.episodic[entry.ID] = entry
	s.dirty = true
	s.persistIfHeavyLocked(entry)
	return decision
}

// resolveConflictsLocked 扫描活跃记忆、裁决并归档。调用方必须持**写锁**。
//
// 扫描范围是全部活跃记忆, 不按主题预分桶 (FactStore 那条路径按 topic 分桶)。
// 不分桶是**刻意**的: L1 的条目经常没有 Topics (Ingestor 自动摄入传的就是 nil),
// 按主题分桶会让这些条目永远扫不到矛盾 —— 而它们恰恰是自动摄入的主力。整表扫描的
// 代价是 O(n) 字符串匹配, 相对一次入库的其它开销可以忽略。
func (s *TieredStore) resolveConflictsLocked(entry *MemoryEntry) (GateDecision, ResolveStats) {
	var st ResolveStats
	var supersedeIDs []string
	var evergreenIDs []string

	for id, old := range s.episodic {
		if id == entry.ID || old.SupersededBy != "" {
			continue // 已被取代的不参与裁决: 它们对提示词已经不可见
		}
		st.Checked++
		if !isContradiction(entry.Content, old.Content) {
			continue
		}
		if old.Evergreen {
			evergreenIDs = append(evergreenIDs, id)
			continue
		}
		supersedeIDs = append(supersedeIDs, id)
	}
	st.Contradicts = len(supersedeIDs)
	st.ContradictsEvergreen = len(evergreenIDs)

	switch {
	case len(supersedeIDs) == 0 && len(evergreenIDs) == 0:
		return GateAppended, st

	case len(supersedeIDs) > 0 && len(evergreenIDs) == 0 && entry.Importance >= ReflectThreshold:
		for _, id := range supersedeIDs {
			old := s.episodic[id]
			old.SupersededBy = entry.ID
			old.SupersededAt = time.Now()
		}
		// 新记忆记下自己取代了谁 —— 与旧记忆的 SupersededBy 是**双向**的。
		// 只留一向的话, "这条记忆是从哪来的" (顺着 SupersededBy 往前) 查得到,
		// "它后来被谁推翻了" 查不到, 而后者才是排查"模型为什么改了主意"时要问的。
		entry.Supersedes = append(entry.Supersedes, supersedeIDs...)
		st.Superseded = len(supersedeIDs)
		return GateSuperseded, st

	default:
		// 低置信矛盾, 或矛盾对象里有永久记忆 —— **一条都不归档**, 只留证。
		//
		// 这里曾经写成"归档 supersedeIDs, 只是不归档 evergreen" —— 那是个真 bug:
		// kept_both 的意思就是"两条都留着", 顺带归档一半等于用一个布尔把两件不同
		// 的事混为一谈。它被 TestH24_LowConfidenceKeepsBoth 抓住 (低置信的新记忆
		// 把旧记忆顶掉了)。FactStore 那条同分支的路径也不归档, 语义现在对齐了。
		//
		// Supersedes 同样不记: 这条新记忆**没有**取代任何人, 在它身上写 Supersedes
		// 会让双向链把"有一次冲突"读成"发生了一次取代"。
		entry.Contradicts = append(entry.Contradicts, append(supersedeIDs, evergreenIDs...)...)
		st.Superseded = 0
		return GateKeptBoth, st
	}
}

// fillDefaultsLocked 补齐 ID 与各项默认值。调用方必须持写锁 (idSeq 不是原子的)。
func (s *TieredStore) fillDefaultsLocked(entry *MemoryEntry) {
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
}

// persistIfHeavyLocked 高权重记忆立即持久化。调用方必须持写锁 (登记 bg 必须发生在
// 任何 WaitPersist 之前, 见 bg 字段的注释)。
func (s *TieredStore) persistIfHeavyLocked(entry *MemoryEntry) {
	if entry.Importance < 0.8 {
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		s.PersistToDisk()
	}()
}

// ResolveStatsOf 从一条记忆上读出它自己的来龙去脉 (只读检视用)。
//
// 返回的是**双向**的: SupersededBy 指向取代它的那条, Supersedes 指向它取代过的
// 那些。被取代的记忆仍在库里 (归档而非删除), 所以整条链随时可以重建 —— 这是
// "结构化"在"消解"之外的另一半: 不只是做了什么决定, 还包括凭什么做的。
func ResolveStatsOf(e *MemoryEntry) (supersededBy string, supersedes, contradicts []string) {
	if e == nil {
		return "", nil, nil
	}
	return e.SupersededBy, append([]string(nil), e.Supersedes...), append([]string(nil), e.Contradicts...)
}
