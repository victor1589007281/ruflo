package market

import (
	"context"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProvider 是测试用数据源, 可按能力注入行为。
type fakeProvider struct {
	name    string
	markets []Market
	caps    CapSet
	healthy bool

	quoteFn   func(context.Context, Instrument) (*Quote, error)
	historyFn func(context.Context, Instrument, HistoryParams) ([]Bar, error)
	factsFn   func(context.Context, Instrument, string) ([]MarketFact, error)
	filingsFn func(context.Context, Instrument, time.Time) ([]Filing, error)

	quoteCalls   atomic.Int32
	historyCalls atomic.Int32
	factCalls    atomic.Int32
	filingCalls  atomic.Int32
}

// newFake 构造健康的测试 provider。
func newFake(name string, caps CapSet, markets ...Market) *fakeProvider {
	return &fakeProvider{name: name, caps: caps, markets: markets, healthy: true}
}

// Name 返回测试 provider 名称。
func (f *fakeProvider) Name() string { return f.name }

// Markets 返回支持市场。
func (f *fakeProvider) Markets() []Market { return f.markets }

// Capabilities 返回能力声明。
func (f *fakeProvider) Capabilities() CapSet { return f.caps }

// Healthy 返回健康状态。
func (f *fakeProvider) Healthy() bool { return f.healthy }

// Quote 调用注入的报价函数。
func (f *fakeProvider) Quote(ctx context.Context, inst Instrument) (*Quote, error) {
	f.quoteCalls.Add(1)
	if f.quoteFn == nil {
		return nil, Wrap(f.name, ErrUnsupported, nil)
	}
	return f.quoteFn(ctx, inst)
}

// History 调用注入的 K 线函数。
func (f *fakeProvider) History(ctx context.Context, inst Instrument, p HistoryParams) ([]Bar, error) {
	f.historyCalls.Add(1)
	if f.historyFn == nil {
		return nil, Wrap(f.name, ErrUnsupported, nil)
	}
	return f.historyFn(ctx, inst, p)
}

// Fundamentals 调用注入的财务函数。
func (f *fakeProvider) Fundamentals(ctx context.Context, inst Instrument, period string) ([]MarketFact, error) {
	f.factCalls.Add(1)
	if f.factsFn == nil {
		return nil, Wrap(f.name, ErrUnsupported, nil)
	}
	return f.factsFn(ctx, inst, period)
}

// Filings 调用注入的公告函数。
func (f *fakeProvider) Filings(ctx context.Context, inst Instrument, since time.Time) ([]Filing, error) {
	f.filingCalls.Add(1)
	if f.filingsFn == nil {
		return nil, Wrap(f.name, ErrUnsupported, nil)
	}
	return f.filingsFn(ctx, inst, since)
}

// TestSnapshotForwardFilterAndVerify 验证前视硬过滤、涨跌幅重算与跨源核价。
func TestSnapshotForwardFilterAndVerify(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	asOf := time.Date(2025, 6, 30, 15, 0, 0, 0, sh)

	p1 := newFake("p1", CapSet{
		CapQuote: true, CapHistory: true, CapFundamental: true, CapFiling: true,
	}, MarketCN)
	p1.quoteFn = func(context.Context, Instrument) (*Quote, error) {
		return &Quote{
			Last: 100, PrevClose: 90, Open: 91, High: 101, Low: 89,
			Volume: 123, ChangePct: 999, AsOf: asOf, Session: SessionRegular, Tradable: true,
		}, nil
	}
	p1.historyFn = func(context.Context, Instrument, HistoryParams) ([]Bar, error) {
		return []Bar{
			{Date: time.Date(2025, 6, 30, 0, 0, 0, 0, sh), Close: 10},
			{Date: time.Date(2025, 7, 1, 0, 0, 0, 0, sh), Close: 11},
		}, nil
	}
	p1.factsFn = func(context.Context, Instrument, string) ([]MarketFact, error) {
		return []MarketFact{
			{Field: "pe", Value: 20, AsOf: time.Date(2025, 6, 30, 0, 0, 0, 0, sh)},
			{Field: "pe", Value: 21, AsOf: time.Date(2025, 7, 1, 0, 0, 0, 0, sh)},
		}, nil
	}
	p1.filingsFn = func(context.Context, Instrument, time.Time) ([]Filing, error) {
		return []Filing{
			{ID: "a", Title: "半年报", Published: time.Date(2025, 6, 30, 9, 0, 0, 0, sh)},
			{ID: "b", Title: "未来公告", Published: time.Date(2025, 7, 1, 9, 0, 0, 0, sh)},
		}, nil
	}

	// 第二源稍慢 (30ms < 150ms 核价窗口), 价差 0.2% < 0.5% → verified。
	p2 := newFake("p2", CapSet{CapQuote: true}, MarketCN)
	p2.quoteFn = func(ctx context.Context, _ Instrument) (*Quote, error) {
		select {
		case <-time.After(30 * time.Millisecond):
		case <-ctx.Done():
			return nil, Wrap("p2", ErrTimeout, ctx.Err())
		}
		return &Quote{Last: 100.2, PrevClose: 90, AsOf: asOf}, nil
	}

	cfg := DefaultConfig()
	cfg.SnapshotDeadline = 2 * time.Second
	cfg.VerifyThreshold = 0.005
	reg := NewRegistry(cfg, p1, p2)

	snap, err := reg.Snapshot(context.Background(), "600519", SnapshotOptions{AsOf: asOf})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Quote == nil {
		t.Fatal("Quote 不应为空")
	}
	if snap.Quote.Last != 100 {
		t.Errorf("Quote.Last = %v, want 100 (主源值)", snap.Quote.Last)
	}
	wantPct := (100.0 - 90.0) / 90.0 * 100
	if math.Abs(snap.Quote.ChangePct-wantPct) > 1e-9 {
		t.Errorf("ChangePct = %v, want %v (必须由 Last/PrevClose 重算)", snap.Quote.ChangePct, wantPct)
	}
	if len(snap.Bars) != 1 || snap.Bars[0].Date.Day() != 30 {
		t.Errorf("前视过滤后 Bars = %+v, want 只保留 06-30", snap.Bars)
	}
	if len(snap.Facts) != 1 {
		t.Errorf("前视过滤后 Facts = %d, want 1", len(snap.Facts))
	}
	if len(snap.Filings) != 1 || snap.Filings[0].ID != "a" {
		t.Errorf("前视过滤后 Filings = %+v, want 只保留 a", snap.Filings)
	}
	warnings := strings.Join(snap.Warnings, "\n")
	if !strings.Contains(warnings, "前视过滤") {
		t.Errorf("Warnings 缺少前视过滤记录:\n%s", warnings)
	}
	if strings.Contains(warnings, "跨源不一致") {
		t.Errorf("0.2%% 价差不应判为不一致:\n%s", warnings)
	}
	if snap.Partial || snap.Missing != nil {
		t.Errorf("完整数据不应 Partial/Missing: partial=%v missing=%v", snap.Partial, snap.Missing)
	}
	if len(snap.Sources) != 2 || snap.Sources[0] != "p1" || snap.Sources[1] != "p2" {
		t.Errorf("Sources = %v, want [p1 p2]", snap.Sources)
	}
	if !snap.AsOf.Equal(asOf) {
		t.Errorf("AsOf = %s, want %s", snap.AsOf, asOf)
	}
}

// TestSnapshotVerifyMismatch 验证价差超阈值时保留主源值并列出两值。
func TestSnapshotVerifyMismatch(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	asOf := time.Date(2025, 6, 30, 15, 0, 0, 0, sh)

	p1 := newFake("fast", CapSet{CapQuote: true}, MarketCN)
	p1.quoteFn = func(context.Context, Instrument) (*Quote, error) {
		return &Quote{Last: 100, PrevClose: 99, AsOf: asOf, Volume: 1}, nil
	}
	p2 := newFake("slow", CapSet{CapQuote: true}, MarketCN)
	p2.quoteFn = func(ctx context.Context, _ Instrument) (*Quote, error) {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return nil, Wrap("slow", ErrTimeout, ctx.Err())
		}
		return &Quote{Last: 101, PrevClose: 99, AsOf: asOf, Volume: 1}, nil
	}

	cfg := DefaultConfig()
	cfg.SnapshotDeadline = 2 * time.Second
	cfg.VerifyThreshold = 0.005
	reg := NewRegistry(cfg, p1, p2)
	snap, err := reg.Snapshot(context.Background(), "600519",
		SnapshotOptions{AsOf: asOf, Fields: []Capability{CapQuote}})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Quote == nil || snap.Quote.Last != 100 {
		t.Fatalf("Quote = %+v, want 保留主源 last=100", snap.Quote)
	}
	warnings := strings.Join(snap.Warnings, "\n")
	if !strings.Contains(warnings, "跨源不一致") || !strings.Contains(warnings, "unverified") {
		t.Errorf("Warnings 缺少不一致记录:\n%s", warnings)
	}
	if !strings.Contains(warnings, "101") {
		t.Errorf("Warnings 应列出备源值 101:\n%s", warnings)
	}
}

// TestSnapshotParallelFallback 验证主源超时、备源成功时 2s 内返回且 Sources 只有备源。
func TestSnapshotParallelFallback(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	asOf := time.Date(2025, 6, 30, 15, 0, 0, 0, sh)

	slow := newFake("slow-primary", CapSet{CapQuote: true}, MarketCN)
	slow.quoteFn = func(ctx context.Context, _ Instrument) (*Quote, error) {
		<-ctx.Done() // 模拟东财 000/超时
		return nil, Wrap("slow-primary", ErrTimeout, ctx.Err())
	}
	fast := newFake("fast-backup", CapSet{CapQuote: true}, MarketCN)
	fast.quoteFn = func(ctx context.Context, _ Instrument) (*Quote, error) {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return nil, Wrap("fast-backup", ErrTimeout, ctx.Err())
		}
		return &Quote{Last: 42, PrevClose: 40, AsOf: asOf, Volume: 10}, nil
	}

	cfg := DefaultConfig()
	cfg.SnapshotDeadline = 3 * time.Second
	reg := NewRegistry(cfg, slow, fast)

	start := time.Now()
	snap, err := reg.Snapshot(context.Background(), "600519",
		SnapshotOptions{AsOf: asOf, Fields: []Capability{CapQuote}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("Snapshot 耗时 %s, 必须在 2s 内返回", elapsed)
	}
	if snap.Quote == nil || snap.Quote.Last != 42 {
		t.Fatalf("Quote = %+v, want 备源 last=42", snap.Quote)
	}
	if len(snap.Sources) != 1 || snap.Sources[0] != "fast-backup" {
		t.Errorf("Sources = %v, want [fast-backup]", snap.Sources)
	}
}

// TestSnapshotMissingCapability 验证缺失能力写 Missing 且 Partial=true。
func TestSnapshotMissingCapability(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	asOf := time.Date(2025, 6, 30, 15, 0, 0, 0, sh)
	p := newFake("only-quote", CapSet{CapQuote: true}, MarketCN)
	p.quoteFn = func(context.Context, Instrument) (*Quote, error) {
		return &Quote{Last: 1, PrevClose: 1, AsOf: asOf, Volume: 1}, nil
	}
	reg := NewRegistry(DefaultConfig(), p)
	snap, err := reg.Snapshot(context.Background(), "600519",
		SnapshotOptions{AsOf: asOf, Fields: []Capability{CapQuote, CapMacro}})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !snap.Partial {
		t.Error("Partial 应为 true")
	}
	if len(snap.Missing) != 1 || snap.Missing[0] != string(CapMacro) {
		t.Errorf("Missing = %v, want [macro]", snap.Missing)
	}
}

// TestSnapshotUndeclaredCapabilityNotRouted 验证未声明能力的 provider 不会被调用。
func TestSnapshotUndeclaredCapabilityNotRouted(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	asOf := time.Date(2025, 6, 30, 15, 0, 0, 0, sh)
	// 只声明 Quote, 却实现了 History 方法; 注册表不得路由。
	p := newFake("quote-only", CapSet{CapQuote: true}, MarketCN)
	p.historyFn = func(context.Context, Instrument, HistoryParams) ([]Bar, error) {
		return []Bar{{Date: asOf, Close: 1}}, nil
	}
	reg := NewRegistry(DefaultConfig(), p)
	snap, err := reg.Snapshot(context.Background(), "600519",
		SnapshotOptions{AsOf: asOf, Fields: []Capability{CapHistory}})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if p.historyCalls.Load() != 0 {
		t.Errorf("history 被调用 %d 次, 未声明能力不得路由", p.historyCalls.Load())
	}
	if len(snap.Missing) != 1 || snap.Missing[0] != string(CapHistory) {
		t.Errorf("Missing = %v, want [history]", snap.Missing)
	}
}

// TestSnapshotNonTradingDayFallback 验证周末回退并在 Warnings 标注实际日期。
func TestSnapshotNonTradingDayFallback(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	asOf := time.Date(2025, 6, 28, 10, 0, 0, 0, sh) // 周六
	p := newFake("quote", CapSet{CapQuote: true}, MarketCN)
	p.quoteFn = func(context.Context, Instrument) (*Quote, error) {
		return &Quote{
			Last: 10, PrevClose: 9, Volume: 100,
			AsOf: time.Date(2025, 6, 27, 15, 0, 0, 0, sh),
		}, nil
	}
	reg := NewRegistry(DefaultConfig(), p)
	snap, err := reg.Snapshot(context.Background(), "600519",
		SnapshotOptions{AsOf: asOf, Fields: []Capability{CapQuote}})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	want := time.Date(2025, 6, 27, 0, 0, 0, 0, sh)
	if !snap.AsOf.Equal(want) {
		t.Errorf("AsOf = %s, want 回退到 %s", snap.AsOf, want)
	}
	if !snap.Stale {
		t.Error("非交易日回退应标记 Stale")
	}
	if !strings.Contains(strings.Join(snap.Warnings, "\n"), "非交易日") {
		t.Errorf("Warnings 缺少非交易日说明: %v", snap.Warnings)
	}
	if snap.Quote == nil {
		t.Error("回退后的前一交易日报价应保留")
	}
}

// TestSnapshotDeadlinePartial 验证预算耗尽返回部分数据并标记 Partial。
func TestSnapshotDeadlinePartial(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	asOf := time.Date(2025, 6, 30, 15, 0, 0, 0, sh)
	p := newFake("hang", CapSet{CapQuote: true}, MarketCN)
	p.quoteFn = func(ctx context.Context, _ Instrument) (*Quote, error) {
		<-ctx.Done()
		return nil, Wrap("hang", ErrTimeout, ctx.Err())
	}
	cfg := DefaultConfig()
	cfg.SnapshotDeadline = 80 * time.Millisecond
	reg := NewRegistry(cfg, p)

	start := time.Now()
	snap, err := reg.Snapshot(context.Background(), "600519",
		SnapshotOptions{AsOf: asOf, Fields: []Capability{CapQuote}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("预算 80ms, 实际耗时 %s", elapsed)
	}
	if !snap.Partial {
		t.Error("Partial 应为 true")
	}
	if snap.Quote != nil {
		t.Error("超时不应拿到报价")
	}
	if !strings.Contains(strings.Join(snap.Warnings, "\n"), "预算") {
		t.Errorf("Warnings 缺少预算耗尽说明: %v", snap.Warnings)
	}
}

// TestRegistryQuoteRecomputesChange 验证 Quote 方法同样重算涨跌幅。
func TestRegistryQuoteRecomputesChange(t *testing.T) {
	p := newFake("q", CapSet{CapQuote: true}, MarketCN)
	p.quoteFn = func(context.Context, Instrument) (*Quote, error) {
		return &Quote{Last: 110, PrevClose: 100, ChangePct: 999}, nil
	}
	reg := NewRegistry(DefaultConfig(), p)
	q, err := reg.Quote(context.Background(), "600519", time.Time{})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if math.Abs(q.ChangePct-10) > 1e-9 {
		t.Errorf("ChangePct = %v, want 10", q.ChangePct)
	}
	if q.Inst.Canonical != "SH:600519" {
		t.Errorf("Inst.Canonical = %s, want SH:600519", q.Inst.Canonical)
	}
}

// TestRegistryProvidersAndDisable 验证 Providers() 摘要与 Enabled=false 过滤。
func TestRegistryProvidersAndDisable(t *testing.T) {
	p := newFake("fake", CapSet{CapQuote: true}, MarketCN)
	reg := NewRegistry(DefaultConfig(), p)
	infos := reg.Providers()
	if len(infos) != 1 || infos[0].Name != "fake" || !infos[0].Healthy {
		t.Fatalf("Providers() = %+v", infos)
	}

	cfg := DefaultConfig()
	cfg.Providers["fake"] = ProviderConfig{Enabled: false}
	reg = NewRegistry(cfg, p)
	if len(reg.Providers()) != 0 {
		t.Error("Enabled=false 的 provider 不应被注册")
	}
}
