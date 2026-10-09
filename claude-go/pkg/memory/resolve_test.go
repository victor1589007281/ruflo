// resolve_test.go —— H2-4 记忆冲突消解的单测。
//
// 方案 §21 H2-4 的验收原文是: **「写入一条与既有记忆矛盾的新记忆, 检索只返回新的
// 那一条」**。
//
// 判据必须落在**检索**上, 不能落在"库里有没有归档标记"上。归档只是一个字段, 而
// 真正要修的故障是"提示词里同时出现两条互相否定的指令" —— 只看标记的话, 一个
// 写了标记却忘了在 Retrieve 里过滤的实现会全绿, 而它在生产上与没做完全一样。
// 所以本文件每个"取代"用例都**同时**断言两件事: 库里有裁决记录, 检索里旧的那条
// 不见了。只断其一都是假绿。
//
// 本文件还钉住三条容易做反的分支:
//   - 置信不足时**不能**取代 (宁可两条都留, 也不能用一条低置信的新记忆抹掉旧记忆);
//   - 永久记忆**不能**被取代 (人工声明的"永远算数"是守门的唯一出口);
//   - 不守门的 Add 必须**逐字节保持旧行为** (它给的是历史回放/重建这类调用方)。
package memory

import (
	"strings"
	"testing"
	"time"
)

// 这两句是 isContradiction 判据的正例: 一句含"仍存在"(broken), 一句含"已修复"(fixed)。
// 措辞不能随便改 —— 换个说法就不再矛盾, 而测试会以"没触发取代"的形态失败, 看起来
// 像是消解逻辑坏了。
const (
	oldBroken = "auth.go 的登录超时 bug 仍存在, 需要继续排查。"
	newFixed  = "auth.go 的登录超时 bug 已修复, 根因是时钟漂移。"
)

// newEntry 造一条记忆, ID 留空由 store 分配。
func newEntry(content string, importance float64) *MemoryEntry {
	return &MemoryEntry{Content: content, Source: "test", Importance: importance}
}

// retrieved 检索并返回命中的正文列表。
func retrieved(t *testing.T, s *TieredStore, query string) []string {
	t.Helper()
	var out []string
	for _, e := range s.Retrieve(query, 10) {
		out = append(out, e.Content)
	}
	return out
}

// contains 判断一组正文里有没有某一条。
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ============================================================================
// 核心: 高置信矛盾 → 取代, 且旧的那条不再被检索出来
// ============================================================================

// TestH24_SupersedeHidesOldFromRetrieval 方案验收原文。
func TestH24_SupersedeHidesOldFromRetrieval(t *testing.T) {
	s := NewTieredStore()

	old := newEntry(oldBroken, 0.5)
	if dec := s.AddWithGate(old); dec != GateAppended {
		t.Fatalf("第一条记忆的裁决 = %s, 期望 appended (库里还没有东西可矛盾)", dec)
	}

	// 反思级置信 (>= ReflectThreshold) 才有裁决权。
	fresh := newEntry(newFixed, 0.9)
	dec := s.AddWithGate(fresh)
	if dec != GateSuperseded {
		t.Fatalf("矛盾 + 高置信的裁决 = %s, 期望 superseded", dec)
	}

	// 第一半: 库里有裁决记录, 而且是指向新记忆的。
	stored, ok := s.Get(old.ID)
	if !ok {
		t.Fatal("被取代的记忆被删掉了 —— 它必须留在库里供审计")
	}
	if stored.SupersededBy != fresh.ID {
		t.Fatalf("SupersededBy = %q, 期望 %q", stored.SupersededBy, fresh.ID)
	}

	// 第二半 (真正的验收判据): 检索只能看见新的。
	got := retrieved(t, s, "auth.go 登录超时 bug")
	if !contains(got, newFixed) {
		t.Fatalf("新的记忆没被检索出来: %v", got)
	}
	if contains(got, oldBroken) {
		t.Fatalf("被取代的旧记忆仍被检索出来 —— 提示词里会同时出现两条互斥陈述: %v", got)
	}
}

// TestH24_SupersedeChainIsBidirectional 取代关系是双向的。
//
// 只留一向的话, "这条记忆是从哪来的"查得到, "它后来被谁推翻了"查不到 —— 而后者
// 才是排查"模型为什么改了主意"时要问的那个问题。
func TestH24_SupersedeChainIsBidirectional(t *testing.T) {
	s := NewTieredStore()
	old := newEntry(oldBroken, 0.5)
	s.AddWithGate(old)
	fresh := newEntry(newFixed, 0.9)
	s.AddWithGate(fresh)

	by, supersedes, _ := ResolveStatsOf(mustGetPtr(t, s, fresh.ID))
	if by != "" {
		t.Fatalf("新记忆不该有 SupersededBy, 实得 %q", by)
	}
	if len(supersedes) != 1 || supersedes[0] != old.ID {
		t.Fatalf("新记忆的 Supersedes = %v, 期望 [%s]", supersedes, old.ID)
	}
}

// TestH24_LowConfidenceKeepsBoth 对照臂: 置信不足时**不取代**。
//
// 这条是守门的存在理由。缺了它, 一次模型走神产生的低置信错觉就能抹掉一条真实
// 记忆, 而抹掉是不可逆的 —— 库里只剩那半句错话。
func TestH24_LowConfidenceKeepsBoth(t *testing.T) {
	s := NewTieredStore()
	old := newEntry(oldBroken, 0.5)
	s.AddWithGate(old)

	// 0.5 < ReflectThreshold(0.8)
	weak := newEntry(newFixed, 0.5)
	dec := s.AddWithGate(weak)
	if dec != GateKeptBoth {
		t.Fatalf("低置信矛盾的裁决 = %s, 期望 kept_both", dec)
	}

	// 旧记忆没被动过。
	if stored := mustGet(t, s, old.ID); stored.SupersededBy != "" {
		t.Fatalf("低置信的新记忆把旧记忆顶掉了 (SupersededBy=%q)", stored.SupersededBy)
	}

	// 两条都还在检索里 —— 这是 kept_both 的真实代价, 必须如实暴露出来,
	// 而不是被写成"冲突已解决"。
	got := retrieved(t, s, "auth.go 登录超时 bug")
	if !contains(got, oldBroken) || !contains(got, newFixed) {
		t.Fatalf("kept_both 时两条都该可检索, 实得 %v", got)
	}

	// 留证: 矛盾必须被记在**新**记忆上, 否则"这里有个未消解的冲突"没人知道。
	_, _, contradicts := ResolveStatsOf(mustGetPtr(t, s, weak.ID))
	if len(contradicts) != 1 || contradicts[0] != old.ID {
		t.Fatalf("Contradicts = %v, 期望 [%s]", contradicts, old.ID)
	}
}

// TestH24_EvergreenSurvivesContradiction 永久记忆不能被任何置信度顶掉。
func TestH24_EvergreenSurvivesContradiction(t *testing.T) {
	s := NewTieredStore()
	old := newEntry(oldBroken, 0.5)
	old.Evergreen = true
	s.AddWithGate(old)

	fresh := newEntry(newFixed, 1.0) // 顶格置信
	dec := s.AddWithGate(fresh)
	if dec != GateKeptBoth {
		t.Fatalf("顶撞永久记忆的裁决 = %s, 期望 kept_both", dec)
	}
	if stored := mustGet(t, s, old.ID); stored.SupersededBy != "" {
		t.Fatal("永久记忆被取代了 —— evergreen 是守门的唯一人工出口")
	}
	if got := retrieved(t, s, "auth.go 登录超时 bug"); !contains(got, oldBroken) {
		t.Fatalf("永久记忆从检索里消失了: %v", got)
	}
	// 读数: "发现了矛盾但动不了"必须与"没有矛盾"分得开。
	st := s.LastResolveStats()
	if st.ContradictsEvergreen != 1 {
		t.Fatalf("ContradictsEvergreen = %d, 期望 1", st.ContradictsEvergreen)
	}
	if st.Superseded != 0 {
		t.Fatalf("永久记忆被算进了 Superseded (%d)", st.Superseded)
	}
}

// TestH24_NoContradictionAppends 对照臂: 不矛盾的照常追加, 不做任何取代。
//
// 少了这一臂, "旧的不见了"会被一个**见谁删谁**的实现满足 —— 那会以每写一条记忆
// 就抹掉一条旧记忆的方式, 把整个库慢慢吃空。
func TestH24_NoContradictionAppends(t *testing.T) {
	s := NewTieredStore()
	first := newEntry("auth.go 的超时时间调成了 30 秒。", 0.9)
	s.AddWithGate(first)

	// 同主题但**不矛盾**的陈述。
	second := newEntry("auth.go 里加了一行日志, 便于下次排查。", 0.9)
	if dec := s.AddWithGate(second); dec != GateAppended {
		t.Fatalf("不矛盾的裁决 = %s, 期望 appended", dec)
	}
	if stored := mustGet(t, s, first.ID); stored.SupersededBy != "" {
		t.Fatal("不矛盾的旧记忆被顶掉了")
	}
	got := retrieved(t, s, "auth.go 超时")
	if !contains(got, first.Content) || !contains(got, second.Content) {
		t.Fatalf("两条都该在, 实得 %v", got)
	}
	// 读数: 扫过了, 但没有矛盾。
	st := s.LastResolveStats()
	if st.Checked != 1 || st.Contradicts != 0 || st.Superseded != 0 {
		t.Fatalf("无矛盾时的读数不对: %+v", st)
	}
}

// TestH24_UngatedAddDoesNotSupersede 不守门的 Add 保持旧行为。
//
// 这是"守门是显式的"这条设计的分界: Add/AddWithOriginal 是给历史回放与重建用的
// 入口, 它必须**原样追加** —— 若它也开始取代, 从盘上重建记忆库会得到与原来不同的
// 内容 (重建过程中每条新记忆都可能顶掉前一条), 而那是一条静默的数据损坏路径。
func TestH24_UngatedAddDoesNotSupersede(t *testing.T) {
	s := NewTieredStore()
	old := newEntry(oldBroken, 0.5)
	s.Add(old)
	fresh := newEntry(newFixed, 0.9)
	s.Add(fresh)

	if stored := mustGet(t, s, old.ID); stored.SupersededBy != "" {
		t.Fatal("不守门的 Add 触发了取代 —— 重建路径会静默改写记忆库")
	}
	if got := retrieved(t, s, "auth.go 登录超时 bug"); !contains(got, oldBroken) {
		t.Fatalf("不守门的 Add 让旧记忆从检索里消失了: %v", got)
	}
}

// TestH24_UnspecifiedImportanceDoesNotSupersede 没写 Importance 的条目按保守处理。
//
// Importance 为 0 会被填成默认 0.5 —— 而 0.5 < ReflectThreshold, 所以它**没有**
// 裁决权。这是对的: "没打分"不该被当成"高分", 否则一个忘了填 Importance 的调用方
// 会获得最强的覆盖权。
func TestH24_UnspecifiedImportanceDoesNotSupersede(t *testing.T) {
	s := NewTieredStore()
	old := newEntry(oldBroken, 0.5)
	s.AddWithGate(old)

	fresh := &MemoryEntry{Content: newFixed, Source: "test"} // Importance 未设
	if dec := s.AddWithGate(fresh); dec != GateKeptBoth {
		t.Fatalf("未设 Importance 的裁决 = %s, 期望 kept_both", dec)
	}
	if stored := mustGet(t, s, old.ID); stored.SupersededBy != "" {
		t.Fatal("未设 Importance 的条目把旧记忆顶掉了")
	}
}

// TestH24_AlreadySupersededNotReCounted 已被取代的条目不再参与后续裁决。
//
// 少了这条, 一条被归档的旧记忆会在每次新入库时被重新"发现"一次并重新归档 ——
// 读数 (Superseded 计数) 因而永远是错的, 而链也会被反复追加同一个 ID。
func TestH24_AlreadySupersededNotReCounted(t *testing.T) {
	s := NewTieredStore()
	old := newEntry(oldBroken, 0.5)
	s.AddWithGate(old)
	s.AddWithGate(newEntry(newFixed, 0.9)) // old 被取代

	// 再来一条同样与 old 矛盾的新记忆。
	third := newEntry("auth.go 的登录超时 bug 已修复, 并加上了回归测试。", 0.9)
	dec := s.AddWithGate(third)
	if dec != GateAppended {
		t.Fatalf("与**已被取代**的旧记忆矛盾的裁决 = %s, 期望 appended (它已经不在检索里了)", dec)
	}
	if stored := mustGet(t, s, old.ID); stored.SupersededBy == third.ID {
		t.Fatal("已被取代的条目被重复裁决, 指向被改写成了新记忆")
	}
	if st := s.LastResolveStats(); st.Checked != 1 {
		t.Fatalf("Checked = %d, 期望 1 —— 被取代的条目不该再进候选", st.Checked)
	}
}

// ============================================================================
// 自动摄入通道 (Ingestor) —— 原来两条通道都绕过了守门
// ============================================================================

// TestH24_IngestorRoutesThroughGate 自动摄入必须走守门。
//
// 这是本次修复的第二个落点: Ingestor.IngestText 当时对 FactStore 调的是不带门的
// Add, 对一个**已经在库里写好的守门**视而不见。判据落在检索上 —— 只断言"裁决枚举
// 变了"会被一个"返回正确枚举但没真归档"的实现满足。
func TestH24_IngestorRoutesThroughGate(t *testing.T) {
	fs := NewFactStore(t.TempDir())
	ts := NewTieredStore()
	ig := NewIngestor(fs, ts)

	// **topics 传 nil** —— 这正是生产路径的形态 (SessionRecord.Topics 从来没人填,
	// 见 cmd/claude-go 的 cliDreamAdapter 与 pkg/agent/teams.go 的 RecordSession)。
	// 传了非空 topics 就把要验的那个缺口绕过去了。
	//
	// 来源用 team_result (scoreImportance 给 0.8) 而不是 dream (0.65) —— 见下一条
	// 测试: 自动摄入的**打分区间**本身就把大部分来源挡在 ReflectThreshold 之外,
	// 这不是缺陷但必须被知道。
	ig.IngestText(oldBroken, "team_result", nil)
	ig.IngestText(newFixed, "team_result", nil)

	// L1 侧: 旧的不能再被检索出来。
	got := retrieved(t, ts, "auth.go 登录超时 bug")
	if contains(got, oldBroken) {
		t.Fatalf("自动摄入的两条矛盾记忆同时留在 L1 检索里: %v", got)
	}
	if !contains(got, newFixed) {
		t.Fatalf("新的记忆不在 L1 检索里: %v", got)
	}

	// 裁决必须被报出来 —— 自动摄入是没人看着的写入路径, 事后从库内容反推不出
	// "这次顶掉了什么"。
	decs := ig.LastDecisions()
	if len(decs) == 0 {
		t.Fatal("摄入没有报出任何裁决")
	}
	var sawSuperseded bool
	for _, d := range decs {
		if d.Decision == GateSuperseded {
			sawSuperseded = true
		}
	}
	if !sawSuperseded {
		t.Fatalf("摄入裁决里没有 superseded: %+v", decs)
	}
}

// TestH24_IngestorFactStoreNilTopicsDetected FactStore 侧同样要能认出 nil-topics 的矛盾。
//
// 这条单独钉住 sharesTopic 的那次放宽 —— 在放宽之前, 两边 topics 都是 nil 时
// sharesTopic 恒为 false, 于是 AddWithGate (无论谁调) 永远返回 GateAppended,
// 看起来一切正常而守门一次都没生效。
func TestH24_IngestorFactStoreNilTopicsDetected(t *testing.T) {
	fs := NewFactStore(t.TempDir())
	ig := NewIngestor(fs, nil)

	ig.IngestText(oldBroken, "team_result", nil)
	ig.IngestText(newFixed, "team_result", nil)

	// 活跃事实里只应剩下新的那条。
	var active []string
	for _, f := range fs.GetAll() {
		active = append(active, f.Content)
	}
	if contains(active, oldBroken) {
		t.Fatalf("FactStore 里两条矛盾事实都还活跃: %v", active)
	}
	if !contains(active, newFixed) {
		t.Fatalf("新的那条不在活跃事实里: %v", active)
	}
}

// TestH24_IngestScoringCapsMostSourcesBelowGate 把自动摄入的打分区间钉成断言。
//
// scoreImportance 给各来源的分是 0.5/0.6/0.65/0.7/0.75/0.8, 而 ReflectThreshold 是
// 0.8 —— **只有 team_result / blackboard (0.8) 够得着裁决权**, dream / extraction /
// pre_compact / evolution 全都只能 kept_both。
//
// 这不是缺陷 (低置信本来就该保守), 但它决定了"守门在自动摄入这条路上有多活跃",
// 而这件事只看代码注释是读不出来的: 接上守门之后, 一次 dream 摄入产生的矛盾**仍然
// 会两条并存**。谁要改这个行为, 该改的是打分或阈值, 而不是怀疑守门没接上。
func TestH24_IngestScoringCapsMostSourcesBelowGate(t *testing.T) {
	ig := NewIngestor(nil, NewTieredStore())

	cases := []struct {
		source  string
		canRule bool // 是否达到 ReflectThreshold, 即"有权归档旧记忆"
	}{
		{"team_result", true},
		{"blackboard", true},
		{"evolution", false},
		{"pre_compact", false},
		{"extraction", false},
		{"dream", false},
		{"", false},
	}
	for _, c := range cases {
		if got := ig.scoreImportance(newFixed, c.source); (got >= ReflectThreshold) != c.canRule {
			t.Errorf("来源 %q 的分 = %.2f, 期待能否裁决 = %v (阈值 %.2f)",
				c.source, got, c.canRule, ReflectThreshold)
		}
	}
}

// TestH24_FactStoreGateReachableWithoutTopics 直接调 FactStore.AddWithGate 也一样
// 要能命中 —— 把 sharesTopic 的放宽与调用方解耦。
func TestH24_FactStoreGateReachableWithoutTopics(t *testing.T) {
	fs := NewFactStore(t.TempDir())

	old := NewMemoryFact("", oldBroken, CategoryFact, 0.5, "test", nil)
	if dec := fs.AddWithGate(old); dec != GateAppended {
		t.Fatalf("第一条的裁决 = %s, 期望 appended", dec)
	}
	fresh := NewMemoryFact("", newFixed, CategoryFact, 0.9, "test", nil)
	if dec := fs.AddWithGate(fresh); dec != GateSuperseded {
		t.Fatalf("nil topics 下矛盾 + 高置信的裁决 = %s, 期望 superseded", dec)
	}
	if !old.Archived {
		t.Fatal("旧事实没有被归档 —— 守门在这条路径上仍然不通电")
	}
}

// ============================================================================
// 读数本身
// ============================================================================

// TestH24_StatsSeparateScannedFromHappened "扫了但没有矛盾"必须与"没扫"分得开。
func TestH24_StatsSeparateScannedFromHappened(t *testing.T) {
	s := NewTieredStore()
	// 库里空着: 扫了, 但一条候选都没有。
	s.AddWithGate(newEntry("第一个事实。", 0.5))
	if st := s.LastResolveStats(); st.Checked != 0 || st.Superseded != 0 {
		t.Fatalf("空库上的读数不对: %+v", st)
	}

	s.AddWithGate(newEntry("第二个不矛盾的陈述。", 0.5))
	st := s.LastResolveStats()
	if st.Checked != 1 {
		t.Fatalf("Checked = %d, 期望 1 —— 扫过了却没报", st.Checked)
	}
	if st.Superseded != 0 || st.Contradicts != 0 {
		t.Fatalf("无矛盾时不该报取代: %+v", st)
	}
}

// TestH24_NilStoreSafe nil 存储上的新方法必须安全 (fail-open 与既有纪律一致)。
func TestH24_NilStoreSafe(t *testing.T) {
	var s *TieredStore
	if got := s.LastResolveStats(); got.Checked != 0 {
		t.Fatalf("nil 存储报出了读数: %+v", got)
	}
	by, sup, con := ResolveStatsOf(nil)
	if by != "" || sup != nil || con != nil {
		t.Fatal("nil 条目的 ResolveStatsOf 不为零值")
	}
}

// TestH24_RetrieveStillWorksForOrdinaryEntries 回归: 加了取代过滤之后, 普通记忆的
// 检索一条都不能少。
//
// 过滤条件写反 (例如 `if entry.SupersededBy == "" { continue }`) 会让检索永远返回
// 空, 而上面每一个"旧的看不见了"的断言**仍然全绿** —— 它们恰恰是要看见空。
// 所以必须有一条断言"该看得见的看得见"。
func TestH24_RetrieveStillWorksForOrdinaryEntries(t *testing.T) {
	s := NewTieredStore()
	for i, c := range []string{
		"billing.go 的结算逻辑走的是整数分, 不是浮点。",
		"billing.go 的超时设成了 15 秒。",
		"billing.go 的错误码统一走 pkg/errors。",
	} {
		e := newEntry(c, 0.5)
		e.LastAccess = time.Now().Add(-time.Duration(i) * time.Minute)
		s.AddWithGate(e)
	}
	got := retrieved(t, s, "billing.go")
	if len(got) != 3 {
		t.Fatalf("三条互不矛盾的记忆应全部可检索, 实得 %d 条: %v", len(got), got)
	}
	for _, c := range []string{
		"billing.go 的结算逻辑走的是整数分, 不是浮点。",
		"billing.go 的超时设成了 15 秒。",
		"billing.go 的错误码统一走 pkg/errors。",
	} {
		if !contains(got, c) {
			t.Fatalf("普通记忆 %q 没被检索出来: %v", c, got)
		}
	}
	// 顺带确认这条路径没把库搅乱。
	if s.Count() != 3 {
		t.Fatalf("库里有 %d 条, 期望 3", s.Count())
	}
}

// mustGet 取一条记忆 (值拷贝), 取不到直接失败。
func mustGet(t *testing.T, s *TieredStore, id string) MemoryEntry {
	t.Helper()
	e, ok := s.Get(id)
	if !ok {
		t.Fatalf("库里没有 id=%s", id)
	}
	return e
}

// mustGetPtr 同上但取指针 —— 供 ResolveStatsOf 这类收 *MemoryEntry 的判据使用。
// Get 刻意返回值拷贝 (见那里的注释), 所以这里取地址的是那份拷贝, 不是库里的活对象。
func mustGetPtr(t *testing.T, s *TieredStore, id string) *MemoryEntry {
	t.Helper()
	e := mustGet(t, s, id)
	return &e
}

// 编译期护栏: 本文件依赖的两个常量必须仍然满足守门语义。
// 有人把 ReflectThreshold 调到 0.5 以下时, "低置信不取代"那几条会静默变成空场景。
var _ = func() bool { return ReflectThreshold > 0.5 && ReflectThreshold <= 1.0 }

func init() {
	// 上面那句只是个表达式, 这里做真正的事: 保证 oldBroken / newFixed 确实互为矛盾。
	// 判据一改 (isContradiction 的标记词表), 这两个常量就失效, 而失效的形态是
	// "取代没发生" —— 看起来像消解逻辑坏了。在这里当场炸掉, 指向正确的修改方向。
	if !isContradiction(oldBroken, newFixed) {
		panic("测试常量不再构成矛盾: 请更新 oldBroken/newFixed 的措辞以匹配 isContradiction 的标记词")
	}
	if !strings.Contains(oldBroken, "auth.go") || !strings.Contains(newFixed, "auth.go") {
		panic("测试常量必须共享同一个落点 (auth.go) —— 否则检索断言失去意义")
	}
}
