// cost_period_test.go — R-H17 时段/日预算窗口的验收。
//
// 与 cost_test.go 一样，断言精确数值与精确时间点，而不是"非零"：只验非零的用例在
// 实现把两个窗口并成一个、或把未知成本当 0 时依然全绿。
//
// 每个"应当不成立"的结论都有对应的反向用例（未知 / 已知、旧窗口 / 新窗口、
// 有后台 goroutine / 没有），避免给恒真或恒假的实现发通行证。
package harness

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// unknownPrice 只认识 "cheap"；"mystery" 返回 0 —— CostLedger 必须把 0 解读为
// 未知模型，而不是免费。
func unknownPrice(model string, u types.Usage) float64 {
	if model != "cheap" {
		return 0
	}
	n := u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return float64(n) * 0.5
}

func dailyWindow(limit float64) CostPeriodWindow {
	return CostPeriodWindow{Window: 24 * time.Hour, Limit: limit, Label: "daily"}
}

// TestCostPeriodDayBoundary 钉住 UTC 日边界：23:59:59 与次日 00:00:00 必须落在
// 两个窗口，金额不串；窗口区间是 [00:00Z, 次日 00:00Z)。
func TestCostPeriodDayBoundary(t *testing.T) {
	l := NewCostLedger(testPrice)
	p := NewCostPeriodLedger(l, dailyWindow(100))

	day1 := time.Date(2026, 3, 1, 23, 59, 59, 0, time.UTC)
	day2 := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

	// 10 token cheap = 5.0；20 token cheap = 10.0。
	p.RecordAt(CostEntry{
		CostKey: CostKey{Role: RoleExtract, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
		Model:   "cheap", Usage: types.Usage{InputTokens: 10}, UsageKnown: true,
	}, day1)
	p.RecordAt(CostEntry{
		CostKey: CostKey{Role: RoleExtract, TurnID: "t2", SessionID: "s1", OwnerID: "alice"},
		Model:   "cheap", Usage: types.Usage{InputTokens: 20}, UsageKnown: true,
	}, day2)

	got1 := p.QueryAt(day1, "alice", RoleExtract)
	if len(got1) != 1 {
		t.Fatalf("窗口数=%d，want 1", len(got1))
	}
	a := got1[0]
	if a.Spent != 5.0 || a.Entries != 1 || !a.Known || a.Over {
		t.Fatalf("day1 视图 = %+v，want spent=5 entries=1 known=true over=false", a)
	}
	wantStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if !a.Start.Equal(wantStart) || !a.End.Equal(wantStart.Add(24*time.Hour)) {
		t.Fatalf("day1 窗口 = [%v, %v)，want [%v, %v)", a.Start, a.End, wantStart, wantStart.Add(24*time.Hour))
	}

	b := p.QueryAt(day2, "alice", RoleExtract)[0]
	if b.Spent != 10.0 || b.Entries != 1 {
		t.Fatalf("day2 视图 spent=%v entries=%d，want 10/1（不得把 day1 的 5 串进来）", b.Spent, b.Entries)
	}
	if !b.Start.Equal(day2) {
		t.Fatalf("day2 起点=%v，want %v", b.Start, day2)
	}

	// 同一窗口内的稍晚时刻必须命中同一个桶（不是按秒切窗）。
	c := p.QueryAt(day2.Add(12*time.Hour), "alice", RoleExtract)[0]
	if c.Spent != 10.0 || !c.Start.Equal(day2) {
		t.Fatalf("day2 中午视图 = %+v，want 命中 day2 起点/10.0", c)
	}
}

// TestCostPeriodOwnerRoleIsolation 钉住归集维度 (owner, role)：不同 owner、不同
// 角色互不污染，空 owner/role 按账本的归一规则落桶。
func TestCostPeriodOwnerRoleIsolation(t *testing.T) {
	p := NewCostPeriodLedger(NewCostLedger(testPrice), dailyWindow(100))
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

	record := func(owner, role, model string, tokens int) {
		p.RecordAt(CostEntry{
			CostKey: CostKey{Role: role, TurnID: "t", SessionID: "s", OwnerID: owner},
			Model:   model, Usage: types.Usage{InputTokens: tokens}, UsageKnown: true,
		}, now)
	}
	record("alice", RoleExtract, "cheap", 10)  // 5.0
	record("alice", RoleDialogue, "strong", 5) // 10.0
	record("bob", RoleExtract, "cheap", 2)     // 1.0
	record("", "", "cheap", 4)                 // local/dialogue = 2.0

	check := func(owner, role string, want float64, wantEntries int) {
		t.Helper()
		got := p.QueryAt(now, owner, role)
		if len(got) != 1 {
			t.Fatalf("%s/%s 窗口数=%d", owner, role, len(got))
		}
		if got[0].Spent != want || got[0].Entries != wantEntries {
			t.Fatalf("%s/%s spent=%v entries=%d，want %v/%d",
				owner, role, got[0].Spent, got[0].Entries, want, wantEntries)
		}
	}
	check("alice", RoleExtract, 5.0, 1)
	check("alice", RoleDialogue, 10.0, 1)
	check("bob", RoleExtract, 1.0, 1)
	// 空 owner 归一成 local；空 role 归一成 dialogue，因此与 alice/dialogue 分开，
	// 但与本条记录自己合并。
	check("", "", 2.0, 1)
	check(DefaultOwnerID, RoleDialogue, 2.0, 1)
}

// TestCostPeriodMultipleWindows 钉住"一个或多个窗口"：同一笔记录同时进每个窗口，
// 各自按自己的起点截断；短窗口先滚动，长窗口仍包含旧数据。
func TestCostPeriodMultipleWindows(t *testing.T) {
	p := NewCostPeriodLedger(NewCostLedger(testPrice),
		CostPeriodWindow{Window: time.Hour, Limit: 1, Label: "hourly"},
		dailyWindow(100),
	)
	now := time.Date(2026, 3, 2, 0, 30, 0, 0, time.UTC)
	p.RecordAt(CostEntry{
		CostKey: CostKey{Role: RoleExtract, OwnerID: "alice"},
		Model:   "cheap", Usage: types.Usage{InputTokens: 10}, UsageKnown: true, // 5.0
	}, now)

	got := p.QueryAt(now, "alice", RoleExtract)
	if len(got) != 2 {
		t.Fatalf("窗口数=%d，want 2", len(got))
	}
	if got[0].Label != "hourly" || got[1].Label != "daily" {
		t.Fatalf("窗口顺序 = %q,%q，want hourly,daily", got[0].Label, got[1].Label)
	}
	if got[0].Spent != 5.0 || !got[0].Over || !got[0].Known {
		t.Fatalf("hourly = %+v，want spent=5 over=true(limit=1) known=true", got[0])
	}
	if got[1].Spent != 5.0 || got[1].Over {
		t.Fatalf("daily = %+v，want spent=5 over=false(limit=100)", got[1])
	}
	if want := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC); !got[0].Start.Equal(want) {
		t.Fatalf("hourly 起点=%v，want %v", got[0].Start, want)
	}

	// 下一个小时：hourly 从 0 开始，daily 仍含 5.0。
	next := now.Add(time.Hour)
	p.RecordAt(CostEntry{
		CostKey: CostKey{Role: RoleExtract, OwnerID: "alice"},
		Model:   "cheap", Usage: types.Usage{InputTokens: 2}, UsageKnown: true, // 1.0
	}, next)

	got = p.QueryAt(next, "alice", RoleExtract)
	if got[0].Spent != 1.0 || got[0].Over { // 1.0 == limit，不是 ">"，不超限
		t.Fatalf("新 hourly = %+v，want spent=1 over=false", got[0])
	}
	if got[1].Spent != 6.0 {
		t.Fatalf("daily 跨小时 = %v，want 6.0（同一 UTC 日内累加）", got[1].Spent)
	}

	// 旧 hour 的桶仍在快照里，没有被滚动清掉。
	snap := p.Snapshot()
	if old, ok := snap.FindBucket("hourly", got[0].Start.Add(-time.Hour), "alice", RoleExtract); !ok || old.Spent != 5.0 || old.Entries != 1 {
		t.Fatalf("旧 hourly 桶 = (%+v, %v)，want 5.0/1 仍可查", old, ok)
	}
}

// TestCostPeriodUnknownCostNotZero 钉住未知成本口径：
//   - 未知条目不进 Spent 的确定值，但必须让 Known=false；
//   - 已知部分仍可查；
//   - 未知成本不得把"已经确定超限"翻成"未超"。
func TestCostPeriodUnknownCostNotZero(t *testing.T) {
	p := NewCostPeriodLedger(NewCostLedger(unknownPrice), dailyWindow(100))
	now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

	// 未知模型：金额不可知。单独一条时绝不能表现为"已知花了 0"。
	p.RecordAt(CostEntry{
		CostKey: CostKey{Role: RoleExtract, TurnID: "t1", OwnerID: "alice"},
		Model:   "mystery", Usage: types.Usage{InputTokens: 1000}, UsageKnown: true,
	}, now)
	got := p.QueryAt(now, "alice", RoleExtract)[0]
	if got.Spent != 0 || got.UnknownEntries != 1 || got.Known {
		t.Fatalf("未知模型单独一条 = %+v，want spent=0 unknown=1 known=false（不得是已知的 0）", got)
	}
	if got.Over {
		t.Fatal("只有未知成本时不能判 over")
	}

	// 再加入一条已知金额：Spent 只给已知部分（下界），Known 仍为 false。
	p.RecordAt(CostEntry{
		CostKey: CostKey{Role: RoleExtract, TurnID: "t2", OwnerID: "alice"},
		Model:   "cheap", Usage: types.Usage{InputTokens: 10}, UsageKnown: true, // 5.0
	}, now)
	got = p.QueryAt(now, "alice", RoleExtract)[0]
	if got.Spent != 5.0 || got.Entries != 2 || got.UnknownEntries != 1 || got.Known {
		t.Fatalf("已知+未知 = %+v，want spent=5 entries=2 unknown=1 known=false", got)
	}

	// 没有未知成本的桶：Known=true。空桶（确实没发生花费）也是 Known=true。
	if s := p.QueryAt(now, "alice", RoleDialogue)[0]; !s.Known || s.Entries != 0 || s.UnknownEntries != 0 || s.Spent != 0 {
		t.Fatalf("无记录的 dialogue = %+v，want known=true entries=0 spent=0", s)
	}

	// 未知成本不能掩盖已经确定的超限：已知下界 5.0 > limit 1。
	p2 := NewCostPeriodLedger(NewCostLedger(unknownPrice), dailyWindow(1))
	p2.RecordAt(CostEntry{
		CostKey: CostKey{Role: RoleExtract, OwnerID: "alice"},
		Model:   "mystery", Usage: types.Usage{InputTokens: 1000}, UsageKnown: true,
	}, now)
	p2.RecordAt(CostEntry{
		CostKey: CostKey{Role: RoleExtract, OwnerID: "alice"},
		Model:   "cheap", Usage: types.Usage{InputTokens: 10}, UsageKnown: true, // 5.0
	}, now)
	if s := p2.QueryAt(now, "alice", RoleExtract)[0]; !s.Over || s.Known {
		t.Fatalf("已知下界超限 = %+v，want over=true known=false（未知不得把 over 翻成 false）", s)
	}
}

// TestCostPeriodObserveDoesNotDoubleCount 钉住 runner 接线最容易踩的坑：
// 同一笔 usage 已经写进 CostLedger 后，入窗只能用 Observe，不能再走 Record ——
// 否则底层账本被记两份，总花费翻倍而两份数值都"看起来合理"。
func TestCostPeriodObserveDoesNotDoubleCount(t *testing.T) {
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	e := CostEntry{
		CostKey: CostKey{Role: RoleExtract, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
		Model:   "cheap", Usage: types.Usage{InputTokens: 10}, UsageKnown: true, // 5.0
	}

	l := NewCostLedger(testPrice)
	p := NewCostPeriodLedger(l, dailyWindow(100))
	rec := l.Record(e)
	p.ObserveAt(rec, now)

	if got := l.Snapshot().Totals; got.Entries != 1 || got.Cost != 5.0 {
		t.Fatalf("Observe 后底层账本 = %+v，want 1 条/5.0（不得重复记账）", got)
	}
	if got := p.QueryAt(now, "alice", RoleExtract)[0]; got.Spent != 5.0 || got.Entries != 1 {
		t.Fatalf("Observe 后窗口 = %+v，want 5.0/1", got)
	}

	// 对照组：Record 自己负责记账 + 入窗，两边各应恰好一条。
	l2 := NewCostLedger(testPrice)
	p2 := NewCostPeriodLedger(l2, dailyWindow(100))
	p2.RecordAt(e, now)
	if got := l2.Snapshot().Totals; got.Entries != 1 || got.Cost != 5.0 {
		t.Fatalf("Record 后底层账本 = %+v，want 1 条/5.0", got)
	}
	if got := p2.QueryAt(now, "alice", RoleExtract)[0]; got.Spent != 5.0 || got.Entries != 1 {
		t.Fatalf("Record 后窗口 = %+v，want 5.0/1", got)
	}
}

// TestCostPeriodRollingKeepsHistory 钉住懒滚动：新窗口从 0 开始，旧窗口及快照
// 不被改写；快照取到后继续写入不改变它。
func TestCostPeriodRollingKeepsHistory(t *testing.T) {
	p := NewCostPeriodLedger(NewCostLedger(testPrice), dailyWindow(100))
	day1 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	day3 := time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC)

	e := func(tokens int) CostEntry {
		return CostEntry{
			CostKey: CostKey{Role: RoleExtract, OwnerID: "alice"},
			Model:   "cheap", Usage: types.Usage{InputTokens: tokens}, UsageKnown: true,
		}
	}
	p.RecordAt(e(10), day1) // 5.0
	p.RecordAt(e(20), day2) // 10.0

	if s := p.QueryAt(day2, "alice", RoleExtract)[0]; s.Spent != 10.0 || s.Entries != 1 {
		t.Fatalf("day2 = %+v，want 从 0 开始只含 day2 的 10.0", s)
	}
	// 回到 day1 查询：旧窗口数据必须原样保留。
	if s := p.QueryAt(day1, "alice", RoleExtract)[0]; s.Spent != 5.0 || s.Entries != 1 {
		t.Fatalf("day1 = %+v，want 5.0/1", s)
	}

	snap := p.Snapshot()
	start1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	start2 := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	if b, ok := snap.FindBucket("daily", start1, "alice", RoleExtract); !ok || b.Spent != 5.0 {
		t.Fatalf("快照 day1 桶 = (%+v, %v)，want 5.0", b, ok)
	}
	if b, ok := snap.FindBucket("daily", start2, "alice", RoleExtract); !ok || b.Spent != 10.0 {
		t.Fatalf("快照 day2 桶 = (%+v, %v)，want 10.0", b, ok)
	}
	if len(snap.Buckets) != 2 {
		t.Fatalf("桶数=%d，want 2（懒滚动不得凭空造桶）", len(snap.Buckets))
	}

	// 快照隔离：day3 写入后旧快照不变，新快照看到第三个桶。
	p.RecordAt(e(2), day3) // 1.0
	if len(snap.Buckets) != 2 || snap.Buckets[0].Spent != 5.0 {
		t.Fatalf("旧快照被后续写入污染: %+v", snap.Buckets)
	}
	fresh := p.Snapshot()
	if len(fresh.Buckets) != 3 {
		t.Fatalf("新快照桶数=%d，want 3", len(fresh.Buckets))
	}
	if b, ok := fresh.FindBucket("daily", time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC), "alice", RoleExtract); !ok || b.Spent != 1.0 {
		t.Fatalf("快照 day3 桶 = (%+v, %v)，want 1.0", b, ok)
	}
}

// TestCostPeriodInjectedClock 钉住 clock 复用：CostLedger.SetNow 的时钟被时段账本
// 继承；CostPeriodLedger.SetNow 可以覆盖它。Record/Query 都不依赖真实 wall clock。
func TestCostPeriodInjectedClock(t *testing.T) {
	l := NewCostLedger(testPrice)
	now := time.Date(2026, 3, 2, 0, 30, 0, 0, time.UTC)
	l.SetNow(func() time.Time { return now })

	p := NewCostPeriodLedger(l, CostPeriodWindow{Window: time.Hour, Limit: 100, Label: "hourly"})
	e := func(tokens int) CostEntry {
		return CostEntry{
			CostKey: CostKey{Role: RoleExtract, OwnerID: "alice"},
			Model:   "cheap", Usage: types.Usage{InputTokens: tokens}, UsageKnown: true,
		}
	}
	p.Record(e(10)) // 5.0，落在 00:00-01:00
	got := p.Query("alice", RoleExtract)
	if len(got) != 1 || got[0].Spent != 5.0 {
		t.Fatalf("注入时钟后 Query = %+v，want 5.0", got)
	}
	if want := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC); !got[0].Start.Equal(want) {
		t.Fatalf("窗口起点=%v，want %v", got[0].Start, want)
	}

	// 时间前进两小时：同一时钟下新记录进新桶，Query 也只读新桶。
	now = now.Add(2 * time.Hour)
	p.Record(e(2)) // 1.0
	got = p.Query("alice", RoleExtract)
	if got[0].Spent != 1.0 || !got[0].Start.Equal(time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("前进两小时后 = %+v，want 新桶 1.0", got[0])
	}

	// 覆盖时钟：即使底层账本时钟不动，时段账本按自己的时钟落桶。
	p.SetNow(func() time.Time { return time.Date(2026, 3, 2, 5, 15, 0, 0, time.UTC) })
	p.Record(e(4)) // 2.0
	got = p.Query("alice", RoleExtract)
	if got[0].Spent != 2.0 || !got[0].Start.Equal(time.Date(2026, 3, 2, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("覆盖时钟后 = %+v，want 05:00 桶 2.0", got[0])
	}
}

// TestCostPeriodConcurrentRecord 用真并发压写入与查询路径：100 个 goroutine 写同一
// (owner, role) 桶，总量必须精确相等；同时有读者并发 Query/Snapshot。配合 -race
// 抓未加锁的 map 访问。
func TestCostPeriodConcurrentRecord(t *testing.T) {
	const goroutines = 100
	const perGoroutine = 10
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

	p := NewCostPeriodLedger(NewCostLedger(testPrice), dailyWindow(1_000_000))

	var wg sync.WaitGroup
	start := make(chan struct{})
	done := make(chan struct{})

	// 读者：与写者并发，验证 Query/Snapshot 不会在 map 上竞态。
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = p.QueryAt(now, "alice", RoleExtract)
				_ = p.Snapshot()
			}
		}
	}()

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perGoroutine; i++ {
				// 1 token strong = 2.0/条，全部落同一个 (owner, role) 桶。
				p.RecordAt(CostEntry{
					CostKey: CostKey{
						Role: RoleExtract, TurnID: fmt.Sprintf("t-%d", i),
						SessionID: "s", OwnerID: "alice",
					},
					Model:      "strong",
					Usage:      types.Usage{InputTokens: 1},
					UsageKnown: true,
				}, now)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(done)
	readers.Wait()

	wantEntries := goroutines * perGoroutine
	wantSpent := float64(wantEntries) * 2.0
	got := p.QueryAt(now, "alice", RoleExtract)[0]
	if got.Entries != wantEntries || got.Spent != wantSpent {
		t.Fatalf("entries=%d spent=%v，want %d/%v（丢写、重复记或非原子累加）",
			got.Entries, got.Spent, wantEntries, wantSpent)
	}
	if !got.Known || got.UnknownEntries != 0 {
		t.Fatalf("全部已知成本应 known=true: %+v", got)
	}
}

// TestCostPeriodNoBackgroundGoroutine 钉住"懒滚动"：构造、写入、查询、快照都不应
// 留下后台 goroutine / timer。
func TestCostPeriodNoBackgroundGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()

	p := NewCostPeriodLedger(NewCostLedger(testPrice), dailyWindow(100))
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		p.RecordAt(CostEntry{
			CostKey: CostKey{Role: RoleExtract, OwnerID: "alice"},
			Model:   "cheap", Usage: types.Usage{InputTokens: 1}, UsageKnown: true,
		}, now.Add(time.Duration(i)*time.Minute))
		_ = p.QueryAt(now, "alice", RoleExtract)
		_ = p.Snapshot()
	}

	// 给运行时一点时间回收测试框架自身的临时 goroutine；留 1 个容差避免噪音。
	deadline := time.Now().Add(2 * time.Second)
	for {
		after := runtime.NumGoroutine()
		if after <= before+1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine 数从 %d 增长到 %d，疑有后台 goroutine", before, after)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
