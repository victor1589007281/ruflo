// cost_test.go — Moqi R-H17 成本归集与"不允许静默换模型"的验收。
//
// 每个用例都刻意断言**精确数值**而不是"非零"：只验非零的测试在实现把两个桶合并、
// 或把某条记录重复计数时依然全绿，而这正是四维归集最容易出的错。
//
// 静默换模型的用例成对出现（有回退事件 / 无回退事件，结论必须相反）：只测前者
// 等于给一个恒 false 的实现发通行证。
package harness

import (
	"fmt"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// testPrice 按模型单价 × 全部 token 计价。未知模型返回 0 —— 与真实价目表"没这个
// 模型"的形状一致（账本须把 0 解读成未知，而不是免费）。
func testPrice(model string, u types.Usage) float64 {
	var unit float64
	switch model {
	case "cheap":
		unit = 0.5
	case "strong":
		unit = 2.0
	default:
		return 0
	}
	n := u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return float64(n) * unit
}

// TestCostLedgerDimensions 钉住四维分组：role / session / owner / turn 各自的小计
// 必须是精确值，且总账等于四份明细之和。
func TestCostLedgerDimensions(t *testing.T) {
	l := NewCostLedger(testPrice)

	// 12 token × 0.5 = 6.0
	l.Record(CostEntry{
		CostKey:    CostKey{Role: RoleExtract, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
		Model:      "cheap",
		Usage:      types.Usage{InputTokens: 10, OutputTokens: 2},
		UsageKnown: true,
	})
	// 20 token × 0.5 = 10.0
	l.Record(CostEntry{
		CostKey:    CostKey{Role: RoleExtract, TurnID: "t2", SessionID: "s1", OwnerID: "alice"},
		Model:      "cheap",
		Usage:      types.Usage{InputTokens: 20},
		UsageKnown: true,
	})
	// 10 token × 2.0 = 20.0
	l.Record(CostEntry{
		CostKey:    CostKey{Role: RoleDialogue, TurnID: "t1", SessionID: "s2", OwnerID: "bob"},
		Model:      "strong",
		Usage:      types.Usage{InputTokens: 5, OutputTokens: 5},
		UsageKnown: true,
	})
	// 1 token × 2.0 = 2.0
	l.Record(CostEntry{
		CostKey:    CostKey{Role: RoleDialogue, TurnID: "t3", SessionID: "s1", OwnerID: "alice"},
		Model:      "strong",
		Usage:      types.Usage{InputTokens: 1},
		UsageKnown: true,
	})

	snap := l.Snapshot()

	if snap.Totals.Entries != 4 || snap.Totals.Cost != 38.0 || snap.Totals.TotalTokens != 43 {
		t.Fatalf("总账 entries=%d cost=%v tokens=%d，want 4/38/43",
			snap.Totals.Entries, snap.Totals.Cost, snap.Totals.TotalTokens)
	}
	if !snap.Totals.CostKnown {
		t.Fatal("全部记录都计价成功，总账 CostKnown 应为 true")
	}

	type want struct {
		name    string
		entries int
		cost    float64
		tokens  int
	}
	checks := []want{
		{"role=extract", 2, 16.0, 32},
		{"role=dialogue", 2, 22.0, 11},
		{"session=s1", 3, 18.0, 33},
		{"session=s2", 1, 20.0, 10},
		{"owner=alice", 3, 18.0, 33},
		{"owner=bob", 1, 20.0, 10},
		{"turn=t1", 2, 26.0, 22},
		{"turn=t2", 1, 10.0, 20},
		{"turn=t3", 1, 2.0, 1},
	}
	got := []CostTotals{
		snap.RoleTotals(RoleExtract),
		snap.RoleTotals(RoleDialogue),
		snap.SessionTotals("s1"),
		snap.SessionTotals("s2"),
		snap.OwnerTotals("alice"),
		snap.OwnerTotals("bob"),
		snap.TurnTotals("t1"),
		snap.TurnTotals("t2"),
		snap.TurnTotals("t3"),
	}
	for i, w := range checks {
		g := got[i]
		if g.Entries != w.entries || g.Cost != w.cost || g.TotalTokens != w.tokens {
			t.Errorf("%s: entries=%d cost=%v tokens=%d，want %d/%v/%d",
				w.name, g.Entries, g.Cost, g.TotalTokens, w.entries, w.cost, w.tokens)
		}
		if !g.CostKnown {
			t.Errorf("%s: 全额计价的小计 CostKnown 应为 true", w.name)
		}
	}

	// 精确四维键单独可查，且不与同 role 的其他 turn 混。
	tot, ok := snap.Lookup(CostKey{Role: RoleExtract, TurnID: "t1", SessionID: "s1", OwnerID: "alice"})
	if !ok || tot.Entries != 1 || tot.Cost != 6.0 {
		t.Fatalf("精确键查询 = (%+v, %v)，want 1 条 / 6.0", tot, ok)
	}
	if _, ok := snap.Lookup(CostKey{Role: RoleExtract, TurnID: "t9", SessionID: "s1", OwnerID: "alice"}); ok {
		t.Fatal("不存在的键不应返回存在")
	}
}

// TestCostLedgerNoPriceKeepsTokens 钉住"没价目表不等于没用量"：
// 价目函数为 nil 时金额未知，但 usage 仍须进账本，且绝不能把成本记成已知的 0。
func TestCostLedgerNoPriceKeepsTokens(t *testing.T) {
	l := NewCostLedger(nil)
	rec := l.Record(CostEntry{
		CostKey:    CostKey{Role: RoleReflect, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
		Model:      "strong",
		Usage:      types.Usage{InputTokens: 100, OutputTokens: 50},
		UsageKnown: true,
	})
	if rec.CostKnown() || rec.Cost != 0 || rec.Pricing != CostPricingNoPriceFunc {
		t.Fatalf("nil 价目函数: cost=%v known=%v pricing=%v，want 0/false/%v",
			rec.Cost, rec.CostKnown(), rec.Pricing, CostPricingNoPriceFunc)
	}

	snap := l.Snapshot()
	if snap.Totals.Entries != 1 {
		t.Fatalf("entries=%d，want 1", snap.Totals.Entries)
	}
	if snap.Totals.Usage.InputTokens != 100 || snap.Totals.Usage.OutputTokens != 50 || snap.Totals.TotalTokens != 150 {
		t.Fatalf("token 未记到: %+v", snap.Totals.Usage)
	}
	if snap.Totals.CostKnown {
		t.Fatal("没有价目表时 CostKnown 必须为 false（0 不是已知成本）")
	}
	if snap.Totals.NoPriceFuncEntries != 1 || snap.Totals.PricedEntries != 0 {
		t.Fatalf("原因计数不对: %+v", snap.Totals)
	}
}

// TestCostLedgerUnknownModelNotFree 钉住未知模型不得被当成免费，并且已知部分的
// 金额仍可作为下界给出，但合计必须标为不完整。
func TestCostLedgerUnknownModelNotFree(t *testing.T) {
	l := NewCostLedger(testPrice)

	unknown := l.Record(CostEntry{
		CostKey:    CostKey{Role: RoleClassify, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
		Model:      "mystery",
		Usage:      types.Usage{InputTokens: 1000, OutputTokens: 1000},
		UsageKnown: true,
	})
	if unknown.CostKnown() || unknown.Pricing != CostPricingUnknownModel || unknown.Cost != 0 {
		t.Fatalf("未知模型: cost=%v known=%v pricing=%v，want 未知/0",
			unknown.Cost, unknown.CostKnown(), unknown.Pricing)
	}

	known := l.Record(CostEntry{
		CostKey:    CostKey{Role: RoleClassify, TurnID: "t2", SessionID: "s1", OwnerID: "alice"},
		Model:      "cheap",
		Usage:      types.Usage{InputTokens: 10},
		UsageKnown: true,
	})
	if !known.CostKnown() || known.Cost != 5.0 {
		t.Fatalf("已知模型: cost=%v known=%v，want 5.0/true", known.Cost, known.CostKnown())
	}

	snap := l.Snapshot()
	if snap.Totals.Cost != 5.0 {
		t.Fatalf("已知部分金额=%v，want 5.0（未知模型不得被算成 0 成本混入已知）", snap.Totals.Cost)
	}
	if snap.Totals.CostKnown {
		t.Fatal("存在未知模型时合计 CostKnown 必须为 false")
	}
	if snap.Totals.UnknownModelEntries != 1 {
		t.Fatalf("未知模型计数=%d，want 1", snap.Totals.UnknownModelEntries)
	}
	if snap.RoleTotals(RoleClassify).CostKnown {
		t.Fatal("按角色小计同样不能把未知模型当全额已知")
	}

	// 调用方显式声明价格未知时，即使价目函数能给正数也不得采信。
	explicit := l.Record(CostEntry{
		CostKey:      CostKey{Role: RoleClassify, TurnID: "t3", SessionID: "s1", OwnerID: "alice"},
		Model:        "cheap",
		Usage:        types.Usage{InputTokens: 10},
		UsageKnown:   true,
		PriceUnknown: true,
	})
	if explicit.CostKnown() || explicit.Pricing != CostPricingExplicitUnknown || explicit.Cost != 0 {
		t.Fatalf("显式未知: cost=%v pricing=%v，want 0/%v",
			explicit.Cost, explicit.Pricing, CostPricingExplicitUnknown)
	}

	// usage 未采集时也不能带全零 usage 去调价（否则会被误归因为未知模型）。
	// 这里故意填了非零 token 但 UsageKnown=false：账本必须把它们排除在合计之外，
	// 否则"没采到"会被当成真实用量计入分母。
	noUsage := l.Record(CostEntry{
		CostKey:    CostKey{Role: RoleClassify, TurnID: "t4", SessionID: "s1", OwnerID: "alice"},
		Model:      "cheap",
		Usage:      types.Usage{InputTokens: 7000},
		UsageKnown: false,
	})
	if noUsage.Pricing != CostPricingUnknownUsage {
		t.Fatalf("usage 未知时 pricing=%v，want %v", noUsage.Pricing, CostPricingUnknownUsage)
	}
	// 哪条 usage 未知只影响金额归因，不影响 token 口径：unknown 的 2000（usage 已测到）
	// + cheap 的 10 + 显式未知价的 10；noUsage 那条 0 token 不计入分母也不虚增。
	if snap := l.Snapshot(); snap.Totals.TotalTokens != 2020 {
		t.Fatalf("未知 usage 不得被当成真 0 而吞掉已知 token: total=%d，want 2020", snap.Totals.TotalTokens)
	}
}

// TestCostLedgerSilentSwitchDetection 是 R-H17 的核心验收。
//
// 成对构造同一组记录，唯一差别是有没有回退事件，结论必须相反：
//   - 有 A→B 的回退事件：判定正常（不是恒真的实现）；
//   - 没有：判定静默（不是恒假的实现）。
//
// 另外钉住两条不误报的边界：角色路由造成的模型差异、A→B→A 只解释一半。
func TestCostLedgerSilentSwitchDetection(t *testing.T) {
	base := CostKey{TurnID: "t1", SessionID: "s1", OwnerID: "alice"}

	t.Run("有回退事件不算静默", func(t *testing.T) {
		l := NewCostLedger(testPrice)
		l.Record(CostEntry{CostKey: base, Model: "cheap", Usage: types.Usage{InputTokens: 1}, UsageKnown: true})
		l.Record(CostEntry{CostKey: base, Model: "strong", Usage: types.Usage{InputTokens: 1}, UsageKnown: true})
		l.RecordFallback(FallbackEvent{CostKey: base, FromModel: "cheap", ToModel: "strong", Reason: "upstream_5xx"})

		snap := l.Snapshot()
		if snap.TurnHasSilentSwitch("t1") {
			t.Fatalf("有回退事件却被判静默: %+v", snap.SilentSwitches)
		}
		chain := snap.FallbacksForTurn("t1")
		if len(chain) != 1 || chain[0].FromModel != "cheap" || chain[0].ToModel != "strong" || chain[0].Reason != "upstream_5xx" {
			t.Fatalf("回退链 = %+v，want 1 条 cheap→strong（upstream_5xx）", chain)
		}
	})

	t.Run("无回退事件即静默", func(t *testing.T) {
		l := NewCostLedger(testPrice)
		l.Record(CostEntry{CostKey: base, Model: "cheap", Usage: types.Usage{InputTokens: 1}, UsageKnown: true})
		l.Record(CostEntry{CostKey: base, Model: "strong", Usage: types.Usage{InputTokens: 1}, UsageKnown: true})

		snap := l.Snapshot()
		if !snap.TurnHasSilentSwitch("t1") {
			t.Fatal("换了模型却没有回退事件，必须判为静默（恒 false 的实现会在这里变红）")
		}
		if len(snap.SilentSwitches) != 1 {
			t.Fatalf("静默转换数=%d，want 1", len(snap.SilentSwitches))
		}
		sw := snap.SilentSwitches[0]
		if sw.FromModel != "cheap" || sw.ToModel != "strong" || sw.TurnID != "t1" {
			t.Fatalf("静默转换 = %+v，want cheap→strong @t1", sw)
		}
		if !snap.HasSilentSwitch() {
			t.Fatal("HasSilentSwitch 应为 true")
		}
	})

	t.Run("角色路由不等于静默换模型", func(t *testing.T) {
		l := NewCostLedger(testPrice)
		l.Record(CostEntry{
			CostKey:    CostKey{Role: RoleDialogue, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
			Model:      "strong",
			Usage:      types.Usage{InputTokens: 1},
			UsageKnown: true,
		})
		l.Record(CostEntry{
			CostKey:    CostKey{Role: RoleExtract, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
			Model:      "cheap",
			Usage:      types.Usage{InputTokens: 1},
			UsageKnown: true,
		})
		if snap := l.Snapshot(); snap.TurnHasSilentSwitch("t1") {
			t.Fatalf("同 turn 不同角色的正常路由被误报: %+v", snap.SilentSwitches)
		}
	})

	t.Run("A→B→A 只解释一半", func(t *testing.T) {
		l := NewCostLedger(testPrice)
		for _, m := range []string{"cheap", "strong", "cheap"} {
			l.Record(CostEntry{CostKey: base, Model: m, Usage: types.Usage{InputTokens: 1}, UsageKnown: true})
		}
		l.RecordFallback(FallbackEvent{CostKey: base, FromModel: "cheap", ToModel: "strong", Reason: "retry"})

		snap := l.Snapshot()
		if !snap.TurnHasSilentSwitch("t1") {
			t.Fatal("B→A 没有回退事件，仍属静默，不能被 A→B 那一条掩盖")
		}
		if len(snap.SilentSwitches) != 1 || snap.SilentSwitches[0].FromModel != "strong" || snap.SilentSwitches[0].ToModel != "cheap" {
			t.Fatalf("静默转换 = %+v，want strong→cheap", snap.SilentSwitches)
		}
	})

	t.Run("不同 turn 互不牵连", func(t *testing.T) {
		l := NewCostLedger(testPrice)
		l.Record(CostEntry{CostKey: CostKey{TurnID: "t1", SessionID: "s1", OwnerID: "alice"}, Model: "cheap", Usage: types.Usage{InputTokens: 1}, UsageKnown: true})
		l.Record(CostEntry{CostKey: CostKey{TurnID: "t1", SessionID: "s1", OwnerID: "alice"}, Model: "strong", Usage: types.Usage{InputTokens: 1}, UsageKnown: true})
		l.RecordFallback(FallbackEvent{CostKey: CostKey{TurnID: "t1", SessionID: "s1", OwnerID: "alice"}, FromModel: "cheap", ToModel: "strong", Reason: "ok"})
		l.Record(CostEntry{CostKey: CostKey{TurnID: "t2", SessionID: "s1", OwnerID: "alice"}, Model: "cheap", Usage: types.Usage{InputTokens: 1}, UsageKnown: true})
		l.Record(CostEntry{CostKey: CostKey{TurnID: "t2", SessionID: "s1", OwnerID: "alice"}, Model: "strong", Usage: types.Usage{InputTokens: 1}, UsageKnown: true})

		snap := l.Snapshot()
		if snap.TurnHasSilentSwitch("t1") {
			t.Fatal("t1 有回退事件，不应被判静默")
		}
		if !snap.TurnHasSilentSwitch("t2") {
			t.Fatal("t2 无回退事件，必须被判静默")
		}
	})
}

// TestCostLedgerConcurrentRecord 用真并发压写入路径：8 个 goroutine 各写 50 条，
// 结束时总量必须**精确**相等（不是大于 0）。丢一条、重复记一条、非原子累加都会
// 让这份断言变红；配合 -race 还能抓出未加锁的共享 map。
func TestCostLedgerConcurrentRecord(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 50

	l := NewCostLedger(testPrice)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			owner := fmt.Sprintf("owner-%d", g)
			for i := 0; i < perGoroutine; i++ {
				l.Record(CostEntry{
					CostKey: CostKey{
						Role: RoleExtract, TurnID: fmt.Sprintf("t-%d-%d", g, i),
						SessionID: "s", OwnerID: owner,
					},
					Model: "strong",
					// 1 token × 2.0 = 2.0/条
					Usage:      types.Usage{InputTokens: 1},
					UsageKnown: true,
				})
			}
			l.RecordFallback(FallbackEvent{
				CostKey:   CostKey{Role: RoleExtract, TurnID: fmt.Sprintf("t-%d-0", g), SessionID: "s", OwnerID: owner},
				FromModel: "cheap", ToModel: "strong", Reason: "load",
			})
		}(g)
	}
	close(start)
	wg.Wait()

	snap := l.Snapshot()
	wantEntries := goroutines * perGoroutine
	if snap.Totals.Entries != wantEntries {
		t.Fatalf("entries=%d，want %d（丢写或重复记）", snap.Totals.Entries, wantEntries)
	}
	if snap.Totals.Cost != float64(wantEntries)*2.0 {
		t.Fatalf("cost=%v，want %v", snap.Totals.Cost, float64(wantEntries)*2.0)
	}
	if snap.Totals.TotalTokens != wantEntries {
		t.Fatalf("tokens=%d，want %d", snap.Totals.TotalTokens, wantEntries)
	}
	if snap.Totals.PricedEntries != wantEntries || !snap.Totals.CostKnown {
		t.Fatalf("计价计数=%d known=%v，want %d/true", snap.Totals.PricedEntries, snap.Totals.CostKnown, wantEntries)
	}
	if len(snap.Fallbacks) != goroutines {
		t.Fatalf("回退事件=%d，want %d", len(snap.Fallbacks), goroutines)
	}
	for g := 0; g < goroutines; g++ {
		owner := fmt.Sprintf("owner-%d", g)
		got := snap.OwnerTotals(owner)
		if got.Entries != perGoroutine || got.Cost != float64(perGoroutine)*2.0 {
			t.Fatalf("owner %s: entries=%d cost=%v，want %d/%v",
				owner, got.Entries, got.Cost, perGoroutine, float64(perGoroutine)*2.0)
		}
	}
}

// TestCostLedgerSnapshotIsolation 钉住快照是副本：取到快照后账本继续写入，
// 快照内容不得改变（否则日志/指标会看到半写状态）。
func TestCostLedgerSnapshotIsolation(t *testing.T) {
	l := NewCostLedger(testPrice)
	l.Record(CostEntry{
		CostKey:    CostKey{Role: RoleDialogue, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
		Model:      "cheap",
		Usage:      types.Usage{InputTokens: 10},
		UsageKnown: true,
	})
	l.RecordFallback(FallbackEvent{
		CostKey:   CostKey{Role: RoleDialogue, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
		FromModel: "cheap", ToModel: "cheap", Reason: "first",
	})

	snap := l.Snapshot()
	if snap.Totals.Entries != 1 || snap.Totals.Cost != 5.0 || len(snap.Fallbacks) != 1 {
		t.Fatalf("初始快照: %+v / fallbacks=%d", snap.Totals, len(snap.Fallbacks))
	}

	// 快照之后再写：金额、token、条目、回退链都不该出现在旧快照里。
	l.Record(CostEntry{
		CostKey:    CostKey{Role: RoleDialogue, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
		Model:      "strong",
		Usage:      types.Usage{InputTokens: 100},
		UsageKnown: true,
	})
	l.RecordFallback(FallbackEvent{
		CostKey:   CostKey{Role: RoleDialogue, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
		FromModel: "cheap", ToModel: "strong", Reason: "second",
	})

	if snap.Totals.Entries != 1 || snap.Totals.Cost != 5.0 || snap.Totals.TotalTokens != 10 {
		t.Fatalf("旧快照被后续写入污染: %+v", snap.Totals)
	}
	if len(snap.Entries) != 1 || len(snap.Fallbacks) != 1 {
		t.Fatalf("旧快照切片被后续写入污染: entries=%d fallbacks=%d", len(snap.Entries), len(snap.Fallbacks))
	}
	if got := snap.RoleTotals(RoleDialogue); got.Entries != 1 || got.Cost != 5.0 {
		t.Fatalf("旧快照分组被污染: %+v", got)
	}
	// ByKey 是最容易被漏掉的一层：ByRole/BySession/... 都是 Snapshot 里另建的
	// map，而 ByKey 是**直接从账本复制**过来的。若 Snapshot 忘了复制它，上面所有
	// 断言依然全绿（它们读的是另建的视图），只有 Lookup 会看到后续写入 —— 而且
	// 看到的形式不是"多一个键"，是"同一个键的合计从 1 条变成 2 条"，数值仍然
	// 合理，没有任何异常迹象。所以这里必须用 Lookup 断言旧快照。
	key := CostKey{Role: RoleDialogue, TurnID: "t1", SessionID: "s1", OwnerID: "alice"}
	if tot, ok := snap.Lookup(key); !ok || tot.Entries != 1 || tot.Cost != 5.0 {
		t.Fatalf("旧快照 ByKey 被后续写入污染 (Lookup): (%+v, %v)，want 1 条/5.0", tot, ok)
	}
	if len(snap.ByKey) != 1 {
		t.Fatalf("旧快照 ByKey 键数=%d，want 1", len(snap.ByKey))
	}

	// 新快照必须看到两次写入，证明写入确实发生了（不是写入被吞掉）。
	fresh := l.Snapshot()
	if fresh.Totals.Entries != 2 || fresh.Totals.Cost != 205.0 || len(fresh.Fallbacks) != 2 {
		t.Fatalf("新快照: entries=%d cost=%v fallbacks=%d，want 2/205/2",
			fresh.Totals.Entries, fresh.Totals.Cost, len(fresh.Fallbacks))
	}
}

// TestCostLedgerEmptyKeyIsolation 钉住边界：空 turn / 空 session / 空 owner 不能
// 因为"都是空"而被并进同一个桶互相污染。
//
// owner 按 types.NormalizeOwner 归一成 "local"（与 ctx/StateStore 一致），所以
// 空 owner 与显式 "local" 必须合并；turn / session 是不透明 ID，空值保持空值，
// 不同组合必须落在不同的四维键上。
func TestCostLedgerEmptyKeyIsolation(t *testing.T) {
	l := NewCostLedger(testPrice)

	// 完全空的键：role 归一成 dialogue，owner 归一成 local，turn/session 保持空。
	l.Record(CostEntry{CostKey: CostKey{}, Model: "cheap", Usage: types.Usage{InputTokens: 10}, UsageKnown: true})
	// 只有 turn 有值。
	l.Record(CostEntry{CostKey: CostKey{TurnID: "t1"}, Model: "cheap", Usage: types.Usage{InputTokens: 20}, UsageKnown: true})
	// 只有 session 有值。
	l.Record(CostEntry{CostKey: CostKey{SessionID: "s1"}, Model: "cheap", Usage: types.Usage{InputTokens: 30}, UsageKnown: true})
	// 显式 local owner + dialogue 角色 + 空 turn/session：必须与第一条合并。
	l.Record(CostEntry{CostKey: CostKey{Role: RoleDialogue, OwnerID: "local"}, Model: "cheap", Usage: types.Usage{InputTokens: 40}, UsageKnown: true})

	snap := l.Snapshot()
	if snap.Totals.Entries != 4 {
		t.Fatalf("entries=%d，want 4", snap.Totals.Entries)
	}

	emptyKey := CostKey{Role: RoleDialogue, OwnerID: DefaultOwnerID}
	tot, ok := snap.Lookup(emptyKey)
	if !ok {
		t.Fatalf("空键（归一后 role=dialogue owner=local）不存在")
	}
	if tot.Entries != 2 || tot.Cost != 25.0 {
		t.Fatalf("空键小计 entries=%d cost=%v，want 2/25（空 owner 与显式 local 合并，空 turn/session 不并）",
			tot.Entries, tot.Cost)
	}

	t1, ok := snap.Lookup(CostKey{TurnID: "t1", OwnerID: DefaultOwnerID})
	if !ok || t1.Entries != 1 || t1.Cost != 10.0 {
		t.Fatalf("turn=t1 小计 = (%+v, %v)，want 1/10", t1, ok)
	}
	s1, ok := snap.Lookup(CostKey{SessionID: "s1", OwnerID: DefaultOwnerID})
	if !ok || s1.Entries != 1 || s1.Cost != 15.0 {
		t.Fatalf("session=s1 小计 = (%+v, %v)，want 1/15", s1, ok)
	}

	// 单维视图：空 turn 桶只含真正没带 turn 的两条；空 session 桶同理。
	if got := snap.TurnTotals(""); got.Entries != 3 || got.Cost != 40.0 {
		t.Fatalf("空 turn 桶 entries=%d cost=%v，want 3/40", got.Entries, got.Cost)
	}
	if got := snap.SessionTotals(""); got.Entries != 3 || got.Cost != 35.0 {
		t.Fatalf("空 session 桶 entries=%d cost=%v，want 3/35", got.Entries, got.Cost)
	}
	// 两个真实 owner 桶（空 → local）必须只有 local 一个键。
	if len(snap.ByOwner) != 1 {
		t.Fatalf("owner 桶数=%d，want 1（归一后只剩 local）", len(snap.ByOwner))
	}
	if got := snap.OwnerTotals(""); got.Entries != 4 {
		t.Fatalf("空 owner 查询归一后应命中 local 全部记录: entries=%d，want 4", got.Entries)
	}
	if got := snap.RoleTotals(""); got.Entries != 4 {
		t.Fatalf("空 role 查询应命中 dialogue 全部记录: entries=%d，want 4", got.Entries)
	}
}
