// cost_period.go — 成本账本的时段/日预算窗口（Moqi R-H17）。
//
// # 与 budget.go 的分工
//
// budget.go 是**本轮**闸：wall / tool_calls / run_tokens / cost，特意管住"这一轮
// 还能不能继续烧"。本文件是**跨轮记账**：把 CostLedger 已经归集好的金额按时间窗口
// 再切一层，回答"owner alice 的 extract 角色今天花了多少、还够不够"。两者不共享
// 状态，也不互相调用：本轮闸的判定在调用当时就要生效，而时段窗口是懒计算的，
// 它在 Record/Query 时按需确定"现在属于哪个窗口"。
//
// # 归集维度：按 (owner, role)
//
// R-H17 允许全局或 (owner, role) 二选一，这里选后者：日预算的常见诉求是
// "每个用户每个角色的日额度"，全局窗口会让一个吵闹的 owner 把所有人的额度吃光，
// 且事后无法从窗口数字里看出是谁吃的。owner 按 NormalizeOwner 归一、role 按
// normalizeCostKey 归一（空 role → dialogue），与 CostLedger 的键完全一致 —— 否则
// 同一批记录在账本与窗口里会落进两个不同的 role 桶，两边都"看起来合理"。
//
// # 窗口对齐：UTC + Unix 截断
//
// 窗口起点 = Unix 时间按 Window 向下取整，**一律 UTC**。这不是本地时区的
// "今天 00:00"：本地日会在 DST 切换日变成 23/25 小时，且同一份账本在不同时区的
// 机器上会切出不同的窗口 —— 分布式部署下就是同一个 owner 有多个"今天"。
// 对 24h 窗口，Unix epoch（1970-01-01T00:00:00Z）恰好是 UTC 零点，所以日窗口就是
// UTC 自然日；对 1h 窗口就是整点。其余 Window 按同一个公式截断。
//
// # 未知成本不是 0
//
// Spent 只累加 CostPricingKnown 的金额。未知成本的条目**不进 Spent**，而是进
// UnknownEntries，并把 Known 置 false。Known=false 的含义是"Spent 只是已知部分的
// 下界，不是这一窗的总花费"；此时 Over=false 只表示"尚不能证明超限"，绝不表示
// "已知未超"。调用方若把 Over 当唯一判据而忽略 Known，等于把未知成本当成了 0。
//
// 空窗口（Entries==0）的 Known 为 true、Spent 为 0：与 CostTotals 的"空组不可信"
// 不同，这里的桶完全由本账本的 Record 构建，没有记录就是确实没有发生花费，
// 预算闸必须能据此放行第一次调用；"没有数据"与"数据不完整"由 Entries 和
// UnknownEntries 分别回答，不再混用 Known 一个布尔。
//
// # 懒滚动、无后台 goroutine
//
// 每个桶的键里带自己的窗口起点（UnixNano），Record 时按当下时间写进对应起点，
// Query 时按当下时间算起点再去查。窗口之间是**键的隔离**，不是"到点清空"：
// 旧窗口的数据原样保留（Snapshot 可查），但新窗口的键不同，从 0 开始。
// 全文件没有任何 goroutine、timer 或 ticker。
package harness

import (
	"sort"
	"sync"
	"time"
)

// CostPeriodWindow 是一个时段预算窗口的配置。
//
// Window<=0 的配置在构造时被丢弃（没有合法截断方式）；Limit<=0 表示该窗口不限额，
// Over 恒为 false，但 Spent/Known 仍然照常统计。Label 只用于调用方区分多个窗口，
// 可为空，不参与任何计算。
type CostPeriodWindow struct {
	Window time.Duration
	Limit  float64
	Label  string
}

// CostPeriodStatus 是某个 (窗口, owner, role) 在当前窗口的预算视图。
//
// 字段口径：
//   - Start/End 是当前窗口的 [Start, End) 区间，UTC。
//   - Spent 只含**已计价**条目之和；存在未知成本时它是下界。
//   - Known 为 true 表示桶里没有未知成本条目（空桶也为 true，见文件头）。
//   - Over 为 Limit>0 && Spent>Limit。Known=false 时 Over=false 只代表"未证明超限"。
type CostPeriodStatus struct {
	Label          string
	Window         time.Duration
	Start          time.Time
	End            time.Time
	Limit          float64
	Spent          float64
	Entries        int
	UnknownEntries int
	Over           bool
	Known          bool
}

// CostPeriodBucket 是快照里一个具体窗口桶的副本（含历史窗口）。
type CostPeriodBucket struct {
	Label          string
	Window         time.Duration
	Start          time.Time
	End            time.Time
	Owner          string
	Role           string
	Entries        int
	UnknownEntries int
	Spent          float64
	Known          bool
}

// CostPeriodSnapshot 是一份隔离的窗口视图副本。
//
// Buckets 覆盖账本里出现过的所有窗口（不只是"当前"），排序固定，便于日志与测试
// 直接比对；取其之后账本继续写不会改变它。
type CostPeriodSnapshot struct {
	Windows []CostPeriodWindow
	Buckets []CostPeriodBucket
}

// periodBucketKey 是窗口桶的键：窗口配置序号 + 窗口起点 + (owner, role)。
//
// 用 UnixNano 而不是 time.Time 作为起点：time.Time 的 == 还会比较 monotonic
// 读数和 Location 指针，两个语义相同的时刻可能不等 —— 那会让同一个窗口分裂成
// 两个桶，且两边数值都合理。int64 只有数值语义。
type periodBucketKey struct {
	window int
	start  int64
	owner  string
	role   string
}

// periodBucket 是一个窗口内的累计值。
type periodBucket struct {
	entries int
	unknown int
	spent   float64
}

// CostPeriodLedger 在 CostLedger 之上叠加并发安全的时段窗口。
//
// 它**不自带记录能力**：Record 先交给底层 CostLedger 完成归一化与计价，再把结果
// 写进窗口。因此窗口里的金额与账本快照逐条一致，不会出现两套价目口径。
// 未传给构造函数的 CostLedger 时，Record 是空操作（与 CostLedger 的 nil 安全一致）。
type CostPeriodLedger struct {
	mu sync.Mutex
	// ledger 是金额的唯一来源；本结构只做时间切片。
	ledger *CostLedger
	// windows 是构造时复制的合法配置（Window>0），调用方之后改自己的切片不影响这里。
	windows []CostPeriodWindow
	// nowFn 可选；为零时回落到 ledger.Now()（再回落 time.Now）。
	nowFn   func() time.Time
	buckets map[periodBucketKey]periodBucket
}

// NewCostPeriodLedger 构造时段账本。
//
// 至少应传一个窗口；Window<=0 的配置被静默丢弃会掩盖配置错误，因此这里**跳过**
// 它们但保持其余窗口工作，调用方可用 Windows() 核对实际生效的配置（长度不一致
// 即说明有非法项）。Limit 不做校验：<=0 是"不限额"这一明确语义。
func NewCostPeriodLedger(ledger *CostLedger, windows ...CostPeriodWindow) *CostPeriodLedger {
	valid := make([]CostPeriodWindow, 0, len(windows))
	for _, w := range windows {
		if w.Window <= 0 {
			continue
		}
		valid = append(valid, w)
	}
	return &CostPeriodLedger{ledger: ledger, windows: valid}
}

// SetNow 注入时钟；nil 时回落到底层 CostLedger 的时钟（其自身再回落 time.Now）。
// 与 CostLedger.SetNow 一样，应在开始记录前配置。
func (p *CostPeriodLedger) SetNow(now func() time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.nowFn = now
	p.mu.Unlock()
}

// Windows 返回实际生效的窗口配置副本。
func (p *CostPeriodLedger) Windows() []CostPeriodWindow {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]CostPeriodWindow(nil), p.windows...)
}

// now 返回当前时刻（UTC）。先用自己的注入，再用底层账本的注入。
func (p *CostPeriodLedger) now() time.Time {
	if p == nil {
		return time.Now().UTC()
	}
	p.mu.Lock()
	fn := p.nowFn
	p.mu.Unlock()
	if fn != nil {
		return fn().UTC()
	}
	if p.ledger != nil {
		return p.ledger.Now().UTC()
	}
	return time.Now().UTC()
}

// costWindowStart 返回 t 所在窗口的起点（UTC）。
//
// 用向下取整而不是 Go 的整数除法：后者对负的 UnixNano 向零截断，会把 1969-12-31
// 的一小段时间并进 1970-01-01 的窗口。预算不会跑在负时间上，但窗口函数一旦写错
// 就是静默错桶，修它比写对它贵。
func costWindowStart(t time.Time, window time.Duration) time.Time {
	width := int64(window)
	ns := t.UTC().UnixNano()
	rem := ns % width
	if rem < 0 {
		rem += width
	}
	return time.Unix(0, ns-rem).UTC()
}

// Record 记录一次成本，并按账本当前时间落入窗口。返回底层账本落账后的记录。
func (p *CostPeriodLedger) Record(e CostEntry) CostRecord {
	if p == nil {
		return CostRecord{}
	}
	return p.RecordAt(e, p.now())
}

// RecordAt 用调用方给出的时间落账，便于重放/测试精确定位窗口。
//
// now 只影响**窗口归属**，不参与计价；同一份 CostEntry 无论从哪个时刻记入，
// CostLedger 里的金额都是一样的。窗口归属按 now.UTC() 计算。
func (p *CostPeriodLedger) RecordAt(e CostEntry, now time.Time) CostRecord {
	if p == nil || p.ledger == nil {
		return CostRecord{}
	}
	rec := p.ledger.Record(e)
	p.add(rec, now)
	return rec
}

// Observe 把一个**已经计价**的记录写入窗口，不再调用底层 CostLedger。
//
// 用于 runner 这类"先写 CostLedger 拿到 CostRecord、同一笔还要进时段账"的调用方：
// 若这里再走一次 Record，底层账本会被记两份 —— 用户看到的总花费翻倍，而两份记录
// 的数值都合理，事后极难发现。Record 是"记账 + 入窗"，Observe 只入窗。
func (p *CostPeriodLedger) Observe(rec CostRecord) {
	if p == nil {
		return
	}
	p.ObserveAt(rec, p.now())
}

// ObserveAt 是带显式时间的 Observe（窗口归属只看 now，不看记录里的任何时间）。
func (p *CostPeriodLedger) ObserveAt(rec CostRecord, now time.Time) {
	if p == nil {
		return
	}
	p.add(rec, now)
}

// add 把一条已计价记录并入每个生效窗口。rec.CostKey 再归一一次是幂等的，
// 让手工构造的 CostRecord 也不会因为空 owner/role 落到与账本不同的桶里。
func (p *CostPeriodLedger) add(rec CostRecord, now time.Time) {
	rec.CostKey = normalizeCostKey(rec.CostKey)
	at := now.UTC()

	p.mu.Lock()
	if p.buckets == nil {
		p.buckets = make(map[periodBucketKey]periodBucket)
	}
	for i := range p.windows {
		w := p.windows[i]
		k := periodBucketKey{
			window: i,
			start:  costWindowStart(at, w.Window).UnixNano(),
			owner:  rec.OwnerID,
			role:   rec.Role,
		}
		b := p.buckets[k]
		b.entries++
		if rec.CostKnown() {
			b.spent += rec.Cost
		} else {
			b.unknown++
		}
		p.buckets[k] = b
	}
	p.mu.Unlock()
}

// Query 用账本当前时间查询 owner/role 在各窗口的预算视图。
func (p *CostPeriodLedger) Query(owner, role string) []CostPeriodStatus {
	if p == nil {
		return nil
	}
	return p.QueryAt(p.now(), owner, role)
}

// QueryAt 查询 now 所在窗口的预算视图：每个生效窗口返回一项，顺序与 Windows 一致。
//
// 只读当前窗口；历史窗口请用 Snapshot。查询不写任何状态，可以并发调用。
func (p *CostPeriodLedger) QueryAt(now time.Time, owner, role string) []CostPeriodStatus {
	if p == nil {
		return nil
	}
	key := normalizeCostKey(CostKey{OwnerID: owner, Role: role})
	at := now.UTC()

	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]CostPeriodStatus, 0, len(p.windows))
	for i := range p.windows {
		w := p.windows[i]
		start := costWindowStart(at, w.Window)
		b := p.buckets[periodBucketKey{
			window: i,
			start:  start.UnixNano(),
			owner:  key.OwnerID,
			role:   key.Role,
		}]
		out = append(out, CostPeriodStatus{
			Label:          w.Label,
			Window:         w.Window,
			Start:          start,
			End:            start.Add(w.Window),
			Limit:          w.Limit,
			Spent:          b.spent,
			Entries:        b.entries,
			UnknownEntries: b.unknown,
			Over:           w.Limit > 0 && b.spent > w.Limit,
			// 空桶也是 Known=true（见文件头）：没有记录 = 确实没有花费。
			Known: b.unknown == 0,
		})
	}
	return out
}

// Snapshot 复制全部窗口桶（含历史窗口）与配置。
//
// 与 CostLedger.Snapshot 同一纪律：锁内复制、锁外组装；返回的切片都是新的，
// 之后继续 Record 不会改变已取到的快照。
func (p *CostPeriodLedger) Snapshot() CostPeriodSnapshot {
	if p == nil {
		return CostPeriodSnapshot{}
	}
	p.mu.Lock()
	windows := append([]CostPeriodWindow(nil), p.windows...)
	type kv struct {
		k periodBucketKey
		b periodBucket
	}
	raw := make([]kv, 0, len(p.buckets))
	for k, b := range p.buckets {
		raw = append(raw, kv{k: k, b: b})
	}
	p.mu.Unlock()

	snap := CostPeriodSnapshot{Windows: windows}
	for _, item := range raw {
		// 桶键里的 window 序号只在构造后稳定；配置复制后仍按同一个序号取。
		w := windows[item.k.window]
		start := time.Unix(0, item.k.start).UTC()
		snap.Buckets = append(snap.Buckets, CostPeriodBucket{
			Label:          w.Label,
			Window:         w.Window,
			Start:          start,
			End:            start.Add(w.Window),
			Owner:          item.k.owner,
			Role:           item.k.role,
			Entries:        item.b.entries,
			UnknownEntries: item.b.unknown,
			Spent:          item.b.spent,
			Known:          item.b.unknown == 0,
		})
	}
	sort.Slice(snap.Buckets, func(i, j int) bool {
		a, b := snap.Buckets[i], snap.Buckets[j]
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		return a.Role < b.Role
	})
	return snap
}

// FindBucket 在快照里查一个精确的 (窗口 Label, 窗口起点, owner, role) 桶。
//
// Label 是必需的判别维度：1h 与 24h 窗口的起点可能相同（例如某日 00:00），
// 只按起点+owner+role 查会命中先遍历到的那个窗口 —— 两个桶的数值各自都合理，
// 错得很安静。
//
// 第二返回值表示是否存在；不存在时返回零值（不要把它读成"成本 0 的已知桶"）。
func (s CostPeriodSnapshot) FindBucket(label string, start time.Time, owner, role string) (CostPeriodBucket, bool) {
	key := normalizeCostKey(CostKey{OwnerID: owner, Role: role})
	start = start.UTC()
	for _, b := range s.Buckets {
		if b.Label == label && b.Start.Equal(start) && b.Owner == key.OwnerID && b.Role == key.Role {
			return b, true
		}
	}
	return CostPeriodBucket{}, false
}
