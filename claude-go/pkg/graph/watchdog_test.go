package graph

// watchdog_test.go —— **图级**停滞检测 (design/01 §4.3 双层 watchdog 的图级那层) 的验收。
//
// ---------------------------------------------------------------------------
// 这组测试在钉什么
// ---------------------------------------------------------------------------
//
//	① **默认关是真的关**: Policies.Watchdog 为 nil 时 journal 里一条 graph.stalled
//	   都没有、hook 总线上一条 stalled 都没有、且 RunResult 与不带 watchdog 完全一致。
//	   这是"未开启时行为一字不变"的直接证据。
//	② **notify 档只观测**: 真的判定了停滞 (有 journal 事件 + hook 事件), 但节点照跑完、
//	   Status 仍是 completed。这一条是本项最重要的断言 —— 一个会自动 fail 团队的新
//	   watchdog 是生产事故。
//	③ **fail 档真能干预**, 且走的是引擎既有的取消路径 (Status=failed, 未跑节点不调度)。
//	④ **进展会重置计时**: 有节点在陆续完成 (journal 在长) 时绝不误判停滞。
//	   少了这一条, 上面的 ② 可以被一个"恒判停滞"的实现骗过。
//	⑤ Validate 拒绝未知 action (静默退档 = 让人以为策略生效了)。
//	⑥ 新增的 `graph.stalled` **不改变 Replay 结果**: journal 是 8+ 下游平台的恢复
//	   真源, 往里加事件类型必须证明重放等价, 否则表现为"开了 watchdog 的团队 resume
//	   会重跑已完成阶段"(重烧 token, 且没人会归因到一个"只观测"的开关)。
//	⑦ `Policies` 多的这个字段在未声明时**一个字节都不进序列化**: journal 的
//	   map.expanded / graph.expanded / subgraph.entered 三种事件都存序列化后的
//	   GraphSpec, 多一个字段会让旧 journal 与新代码的恢复结果不一致。
//
// ---------------------------------------------------------------------------
// 变异反证 (证明上面这些断言不是许愿式的)
// ---------------------------------------------------------------------------
//
//	M8 **同时**摘掉 notify 档的两道闸
//	   (engine.go: notify 也拿到 wdCtx+cancelRun; watchdog.go: 去掉 act==fail 判断)
//	   → ② 红: "notify 档绝不该让 Run 报错: context canceled"
//
//	   ⚠️ 诚实记一笔: **单独摘掉任何一道都不会红** —— 这是防御纵深, 不是测试无牙。
//	   两道闸各自独立充分: 引擎在 notify 档下根本不把取消句柄交给 watchdog
//	   (结构性), 而 watchdog 自己还再判一次 act。第一次做变异时只摘了 watchdog
//	   那道, 测试照绿, 于是把这条写在这里 —— 将来有人"清理冗余判断"删掉其中一道时,
//	   测试不会报警, 但另一道仍在守着。
//
//	M9 appendEv 不刷新进展时钟 (= 图级探针的输入恒定, 于是必然误判)
//	   → ④ 红: "有进展时不该被 watchdog 打断: context canceled"
//
//	   这条变异**第一版没抓到**: 当时 ④ 取 8 个 10ms 节点 (整图 80ms) 而阈值 200ms,
//	   跑完都没到第一次判定。量纲修正后 (9×30ms=270ms > 150ms 阈值) 才有牙。
//	   记这一笔是因为它正是"测试通过 ≠ 测试有效"的现场样本。
//
// ---------------------------------------------------------------------------
// 为什么不用更直觉的"注入假时钟"
// ---------------------------------------------------------------------------
//
// 直觉做法是给 watchdog 一个可控 now(), 于是不必真等。但那样测的是"我的假时钟
// 前进了", 而真正会出错的地方是**巡检 goroutine 与调度 goroutine 的交错**
// (谁先刷新时钟、取消到达时调度在哪一步) —— 假时钟会把这层交错整个抹掉。
// 所以这里用**极小的真实时长** (毫秒级 StallSec/CheckSec) 跑真的 goroutine:
// 阈值本身是可配的 int 秒, 但内部换算与判定逻辑不看单位, 用 checkEvery/stallAfter
// 的注入点 (下面的 msWatchdog 辅助函数) 把秒换成毫秒即可, 判定路径一字不改。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// wdRunner 一个可控节奏的节点执行体。
type wdRunner struct {
	delay time.Duration
	mu    sync.Mutex
	ran   []string
}

func (r *wdRunner) RunNode(ctx context.Context, node NodeSpec, _ NodeInput) NodeResult {
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			// 取消时报失败: 与生产 runner 同形 (它们把 ctx 取消当执行失败上报)。
			return NodeResult{Status: NodeStatusFailed, Err: "cancelled"}
		}
	}
	r.mu.Lock()
	r.ran = append(r.ran, node.ID)
	r.mu.Unlock()
	return NodeResult{Status: NodeStatusCompleted, Output: "ok:" + node.ID}
}

func (r *wdRunner) ranIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.ran))
	copy(out, r.ran)
	return out
}

// wdBus 记录 stalled 事件的总线。
type wdBus struct {
	mu      sync.Mutex
	stalled []HookEvent
}

func (b *wdBus) Emit(_ context.Context, ev HookEvent) HookDecision {
	if ev.Scope == ScopeGraph && ev.Phase == "stalled" {
		b.mu.Lock()
		b.stalled = append(b.stalled, ev)
		b.mu.Unlock()
	}
	return HookDecision{}
}

func (b *wdBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.stalled)
}

// wdChain 一条 N 节点的线性图 (串行 ⇒ 总耗时可预期)。
func wdChain(n int, wd *WatchdogSpec) GraphSpec {
	spec := GraphSpec{Name: "wd", Policies: GraphPolicies{MaxParallel: 1, Watchdog: wd}}
	for i := 0; i < n; i++ {
		id := "n" + string(rune('0'+i))
		spec.Nodes = append(spec.Nodes, NodeSpec{ID: id, Kind: NodeKindAgent,
			Agent: AgentSpec{Role: "worker"}})
		if i > 0 {
			spec.Edges = append(spec.Edges, EdgeSpec{From: "n" + string(rune('0'+i-1)), To: id})
		}
	}
	return spec
}

// msWatchdog 一个以**毫秒**为单位的 watchdog 声明。
//
// WatchdogSpec 的字段单位是秒 (JSON 声明面向运维), 而测试要在毫秒级验证交错。
// 直接构造内部 duration 而不是等 10 分钟: stallAfter/checkEvery 是唯一的单位换算处,
// 判定循环本身只吃 duration —— 所以这里改的是"单位", 判定路径一字未变。
func msWatchdog(stall, check time.Duration, action string, maxNotices int) *WatchdogSpec {
	return &WatchdogSpec{
		StallSec:   0, // 交给下面的 override
		CheckSec:   0,
		Action:     action,
		MaxNotices: maxNotices,
		stallOvr:   stall,
		checkOvr:   check,
	}
}

// countStalled 数 journal 里的 graph.stalled 事件。
func countStalled(t *testing.T, j *MemoryJournal) int {
	t.Helper()
	evs, err := j.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Type == EvGraphStalled {
			n++
		}
	}
	return n
}

// Test图级watchdog未声明时完全不生效 —— ① 段。
func Test图级watchdog未声明时完全不生效(t *testing.T) {
	// 节点 delay 远大于任何可能的巡检周期: 若 watchdog 在"未声明"时也起,
	// 这里必然会判定停滞。
	runner := &wdRunner{delay: 120 * time.Millisecond}
	j := NewMemoryJournal()
	bus := &wdBus{}
	eng := &Engine{Runner: runner, Journal: j, Hooks: bus}

	rr, err := eng.Run(context.Background(), wdChain(2, nil), RunOpts{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rr.Status != RunStatusCompleted {
		t.Fatalf("Status 应为 completed, got %s", rr.Status)
	}
	if n := countStalled(t, j); n != 0 {
		t.Errorf("未声明 watchdog 时 journal 不该有 graph.stalled, got %d 条", n)
	}
	if n := bus.count(); n != 0 {
		t.Errorf("未声明 watchdog 时总线不该有 stalled 事件, got %d 条", n)
	}
}

// Test图级watchdog的notify档只观测不干预 —— ② 段 (本项最重要的断言)。
func Test图级watchdog的notify档只观测不干预(t *testing.T) {
	// 一个 250ms 的节点 + 40ms 阈值 ⇒ 必然被判定停滞至少一次。
	runner := &wdRunner{delay: 250 * time.Millisecond}
	j := NewMemoryJournal()
	bus := &wdBus{}
	eng := &Engine{Runner: runner, Journal: j, Hooks: bus}

	spec := wdChain(2, msWatchdog(40*time.Millisecond, 10*time.Millisecond, WatchdogNotify, 0))
	rr, err := eng.Run(context.Background(), spec, RunOpts{})
	if err != nil {
		t.Fatalf("notify 档绝不该让 Run 报错: %v", err)
	}
	// 干预面: 一字不变。
	if rr.Status != RunStatusCompleted {
		t.Errorf("notify 档必须让运行照常收尾: Status got %s want completed", rr.Status)
	}
	if got := len(runner.ranIDs()); got != 2 {
		t.Errorf("notify 档下全部节点都该跑完: got %d want 2", got)
	}
	// 观测面: 真的判定了。
	if n := countStalled(t, j); n == 0 {
		t.Error("notify 档应把停滞记进 journal, 实际 0 条 —— 观测本身失效了")
	}
	if n := bus.count(); n == 0 {
		t.Error("notify 档应向总线发 stalled 事件, 实际 0 条")
	}
	// 载荷必须自解释: 少了阈值就无法判断"这次告警到底按什么标准算的"。
	bus.mu.Lock()
	p := bus.stalled[0].Payload
	bus.mu.Unlock()
	for _, k := range []string{"graph", "stall_ms", "threshold_ms", "action", "repeat"} {
		if _, ok := p[k]; !ok {
			t.Errorf("stalled 载荷缺 %q: %v", k, p)
		}
	}
	if p["action"] != WatchdogNotify {
		t.Errorf("载荷 action 应为 notify, got %v", p["action"])
	}
}

// Test图级watchdog的fail档走既有取消路径 —— ③ 段。
func Test图级watchdog的fail档走既有取消路径(t *testing.T) {
	runner := &wdRunner{delay: 400 * time.Millisecond}
	j := NewMemoryJournal()
	bus := &wdBus{}
	eng := &Engine{Runner: runner, Journal: j, Hooks: bus}

	spec := wdChain(3, msWatchdog(40*time.Millisecond, 10*time.Millisecond, WatchdogFail, 0))
	start := time.Now()
	rr, err := eng.Run(context.Background(), spec, RunOpts{})
	elapsed := time.Since(start)

	// **不新造终态**: 走引擎既有的取消路径 ⇒ failed + error 含 ctx.Err()。
	if rr.Status != RunStatusFailed {
		t.Errorf("fail 档停滞应让运行 failed (取消路径), got %s", rr.Status)
	}
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error 应含 ctx.Err(), got %v", err)
	}
	// 真的提前中止了: 3 个 400ms 串行节点跑完要 1.2s, 停滞后应远早于此返回。
	if elapsed > 900*time.Millisecond {
		t.Errorf("fail 档应提前中止, 实际耗时 %s (接近全跑完)", elapsed)
	}
	if got := len(runner.ranIDs()); got >= 3 {
		t.Errorf("fail 档下不该把全部节点跑完: got %d", got)
	}
	if n := countStalled(t, j); n != 1 {
		t.Errorf("fail 档判定一次就该收手 (cancelRun 后 return), got %d 条", n)
	}
}

// Test图级watchdog有进展时不误判 —— ④ 段。
//
// 这一条是 ② 的反面对照: 没有它, 一个"恒判停滞"的实现也能让 ② 绿。
func Test图级watchdog有进展时不误判(t *testing.T) {
	// 关键的量纲设计 (不是随手取的数):
	//   单节点耗时 30ms  <  阈值 150ms   ⇒ 每个节点都在阈值内产出 journal 事件
	//   整图耗时 9×30=270ms  >  阈值 150ms ⇒ **整图跨过了不止一个阈值窗口**
	//
	// 后者是这条测试有牙的全部原因: 若整图总耗时也短于阈值, 那么"时钟根本不刷新"
	// 的实现也照样绿 (跑完了都还没到第一次判定)。第一版这里取 8×10ms=80ms < 200ms,
	// 于是变异 (appendEv 不刷新时钟) 没被抓到 —— 这个坑记在这里免得有人再调小。
	runner := &wdRunner{delay: 30 * time.Millisecond}
	j := NewMemoryJournal()
	bus := &wdBus{}
	eng := &Engine{Runner: runner, Journal: j, Hooks: bus}

	spec := wdChain(9, msWatchdog(150*time.Millisecond, 15*time.Millisecond, WatchdogFail, 0))
	rr, err := eng.Run(context.Background(), spec, RunOpts{})
	if err != nil {
		t.Fatalf("有进展时不该被 watchdog 打断: %v", err)
	}
	if rr.Status != RunStatusCompleted {
		t.Errorf("Status got %s want completed", rr.Status)
	}
	if n := countStalled(t, j); n != 0 {
		t.Errorf("journal 一直在长, 不该判停滞, got %d 条", n)
	}
}

// Test图级watchdog的MaxNotices限制重复告警 —— notify 档下停滞会周期性重复判定。
func Test图级watchdog的MaxNotices限制重复告警(t *testing.T) {
	runner := &wdRunner{delay: 400 * time.Millisecond}
	j := NewMemoryJournal()
	eng := &Engine{Runner: runner, Journal: j, Hooks: &wdBus{}}

	spec := wdChain(1, msWatchdog(20*time.Millisecond, 5*time.Millisecond, WatchdogNotify, 1))
	if _, err := eng.Run(context.Background(), spec, RunOpts{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := countStalled(t, j); n != 1 {
		t.Errorf("MaxNotices=1 应只判定一次, got %d 条", n)
	}
}

// Test图级watchdog声明校验 —— ⑤ 段。
func Test图级watchdog声明校验(t *testing.T) {
	// 未知 action 必须**拒**, 不是静默退到 notify: 静默退档会让写了 action:"retry"
	// 的人以为运行会被干预, 而实际只发了条日志 (fail-open 且完全不可考)。
	spec := wdChain(1, &WatchdogSpec{Action: "retry"})
	err := spec.Validate()
	if err == nil {
		t.Fatal("action=retry 应被 Validate 拒绝")
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Errorf("报错应点出是哪个 action: %v", err)
	}
	// 负数同样拒。
	if err := wdChain(1, &WatchdogSpec{StallSec: -1}).Validate(); err == nil {
		t.Error("负的 stall_sec 应被拒绝")
	}
	// 合法声明与 nil 都放行。
	for _, wd := range []*WatchdogSpec{nil, {}, {Action: WatchdogNotify}, {Action: WatchdogFail, StallSec: 60}} {
		if err := wdChain(1, wd).Validate(); err != nil {
			t.Errorf("合法 watchdog 声明被拒: %+v -> %v", wd, err)
		}
	}
}

// Test图级watchdog事件不影响Replay —— journal 是恢复真源, 往里加一种新事件类型
// **必须**证明它不改变重放结果。
//
// 为什么这条不能省: graph.stalled 是本轮新增的第 20+ 种 journal 事件, 而 Replay 是
// 8+ 下游平台的恢复语义所在。若它被误解释 (哪怕只是让某个节点从 Completed 里掉出去),
// 表现就是"开了 watchdog 的团队 resume 会重跑已完成阶段" —— 重烧 token 且很难归因到
// 一个"只观测"的开关上。
func Test图级watchdog事件不影响Replay(t *testing.T) {
	base := []Event{
		{Seq: 1, Type: EvRunCreated, RunID: "r1"},
		{Seq: 2, Type: EvNodeCompleted, RunID: "r1", NodeID: "n0", Data: map[string]any{"output": "A"}},
		{Seq: 3, Type: EvNodeCompleted, RunID: "r1", NodeID: "n1", Data: map[string]any{"output": "B"}},
	}
	// 同一序列, 中间穿插两条 graph.stalled (真实形态: 停滞判定发生在节点之间)。
	withStall := []Event{
		base[0],
		base[1],
		{Seq: 3, Type: EvGraphStalled, RunID: "r1", Data: map[string]any{"stall_ms": 600000, "action": "notify"}},
		{Seq: 4, Type: EvGraphStalled, RunID: "r1", Data: map[string]any{"stall_ms": 1200000, "action": "notify"}},
		{Seq: 5, Type: EvNodeCompleted, RunID: "r1", NodeID: "n1", Data: map[string]any{"output": "B"}},
	}
	a, b := Replay(base), Replay(withStall)
	if len(a.Completed) != len(b.Completed) {
		t.Fatalf("Completed 条数不等价: 无停滞事件 %d, 有停滞事件 %d", len(a.Completed), len(b.Completed))
	}
	for id, ra := range a.Completed {
		rb, ok := b.Completed[id]
		if !ok {
			t.Errorf("节点 %s 在有停滞事件的重放里丢了", id)
			continue
		}
		if ra.Output != rb.Output || ra.Status != rb.Status {
			t.Errorf("节点 %s 重放结果不等价: %+v vs %+v", id, ra, rb)
		}
	}
	if a.Finished != b.Finished || a.Status != b.Status {
		t.Errorf("终态不等价: %v/%q vs %v/%q", a.Finished, a.Status, b.Finished, b.Status)
	}
}

// Test图级watchdog未声明时不改变图的序列化形态 —— Policies 多一个字段就可能改
// journal 里的子图载荷 (map.expanded / graph.expanded / subgraph.entered 都存
// 序列化后的 GraphSpec), 那会让**旧 journal 与新代码的恢复结果不一致**。
// 指针 + omitempty 保证 nil 时一个字节都不多, 这里把它钉住。
func Test图级watchdog未声明时不改变图的序列化形态(t *testing.T) {
	b, err := json.Marshal(GraphPolicies{MaxParallel: 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "watchdog") {
		t.Errorf("未声明 watchdog 时序列化不该出现该字段: %s", b)
	}
	// 声明了才出现 (否则上面那条断言可能只是因为 tag 写错、字段永远不序列化)。
	b2, err := json.Marshal(GraphPolicies{Watchdog: &WatchdogSpec{StallSec: 60}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b2), "watchdog") {
		t.Errorf("声明了 watchdog 却没序列化出来: %s", b2)
	}
}

// Test图级watchdog巡检间隔被夹到阈值以内 —— 巡检比阈值稀疏会让告警系统性迟到,
// 而那种偏差没人会往巡检间隔上想。
func Test图级watchdog巡检间隔被夹到阈值以内(t *testing.T) {
	wd := &WatchdogSpec{StallSec: 10, CheckSec: 600}
	if got := wd.checkEvery(); got != 10*time.Second {
		t.Errorf("巡检间隔应被夹到阈值 10s, got %s", got)
	}
	// 缺省沿用 coordinator.go 的既有数字 (10min / 60s)。
	def := &WatchdogSpec{}
	if got := def.stallAfter(); got != DefaultWatchdogStallSec*time.Second {
		t.Errorf("缺省阈值应为 %ds, got %s", DefaultWatchdogStallSec, got)
	}
	if got := def.checkEvery(); got != DefaultWatchdogCheckSec*time.Second {
		t.Errorf("缺省巡检间隔应为 %ds, got %s", DefaultWatchdogCheckSec, got)
	}
}
