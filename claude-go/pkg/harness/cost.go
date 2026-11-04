// cost.go — 按 角色 / turn / session / owner 归集成本与回退（Moqi R-H17）。
//
// # 为什么需要聚合器
//
// TurnTrace 已经带 Cost/CostKnown，但它回答的是"这一轮花了多少"。R-H17 要回答的是
// 跨轮的问题："extract 角色这周花了多少"、"owner alice 的 session 里哪一轮最贵"、
// "这个 turn 的模型从 A 换成 B 时，trace 里有没有回退理由"。逐轮求和能算出前两个，
// 却算不出第三个的一半 —— 回退事件不是某一轮的属性，而是轮内两次调用之间的关系。
// 所以这里把**记录**与**查询**放在同一个结构里，键统一是四维组合。
//
// # 三条口径纪律（每一条都对应一种"看起来正常"的静默错误）
//
//  1. **缺失不等于零**。价目函数为 nil、上游没回 usage、模型不在价目表里，三者都
//     让成本不可知。把它们记成 0 会让故障期间的账单看起来是免费的。于是每条记录
//     都带 Pricing 原因，聚合时只对"已知"的那部分求和，并保持 CostKnown=false 让
//     调用方知道这个和不是全额。
//  2. **未知模型不是免费**。价目函数返回 <=0 一律按"未知"处理（与 budget.go 对
//     c<=0 的口径一致）。宁可上报"不知道"，也不要上报一个恒 0 的已知成本 ——
//     本仓在 usage 上踩过"恒 0 与未采集长得一样"的坑，成本不能重犯。
//  3. **换模型必须有回退事件**。同一 turn、同一角色内出现两个不同模型，而回退集合
//     里没有对应的 (from,to)，就判定为"静默换模型"。检测只在**同角色**内进行：
//     同一轮 dialogue 走强模型、extract 走便宜模型是角色路由的正常结果，把它报成
//     静默换模型会让告警永远在响，最后被当成噪音关掉。
//
// # owner 归一，session / turn 不归一
//
// 空 owner 按 types.NormalizeOwner 归一成 DefaultOwnerID（"local"）：owner 是身份
// 作用域，全仓的写入口（ctx、StateStore、trace）都已经归一，这里若再保留一个 ""
// 桶，同一个用户就会分裂成 "local" 与 "" 两份小计，而两份都"看起来合理"。
// 反过来，session / turn 是调用方生成的**不透明标识**，空值的含义是"调用方没给"，
// 不是"本地默认会话"。给它们编造默认值（比如也塞 "local"）会把互不相干的轮次并进
// 同一个桶，成本被悄悄摊到别人头上。因此空 session 就记空 session，空 turn 就记
// 空 turn；四维键是**结构体**而不是拼接字符串，空值之间不会互相串桶。
//
// # 并发与快照
//
// 所有写路径共用一把互斥锁；快照在锁内复制 entries / 聚合表 / 回退集合，返回的
// 切片与 map 都是新的。若不复制，调用方在 range 快照的同时另一个 goroutine 追加
// entries，轻则看到半写状态（日志里"本轮成本"与"累计成本"对不上），重则 map 并发
// 读写直接 panic。价目函数在锁外调用：它是调用方代码，放锁内既会把计价延迟放大到
// 所有 turn 上，也可能在它回头读账本时死锁 —— 代价是它必须自己保证并发安全。
package harness

import (
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// CostPriceFunc 把一个模型的 usage 折算成金额。
//
// 与 RunnerOptions.Price 同签名、同口径：返回 <=0 表示"不知道该模型的价格"，
// 而不是"免费"。若确有零价模型（自建、离线），请让价目函数返回一个正数并在
// 业务侧按 cents 换算；把 0 当已知零价会让"漏配价目表"与"真的不要钱"再也分不开。
//
// 同一个函数会被多个 goroutine 并发调用（Record 不持锁调价），实现必须无状态
// 或自行同步 —— 否则 -race 下会报竞态，而生产上是随机的错价。
type CostPriceFunc func(model string, u types.Usage) float64

// CostKey 是四维归集键。
//
// 用结构体而不是 "role|turn|session|owner" 这类拼接串：标识符里允许出现任意字符，
// turn="a:b" + session="" 与 turn="a" + session=":b" 在朴素拼接下会得到同一个键，
// 两个不相干 turn 的成本被静默合并。结构体键逐字段比较，不存在编码歧义。
type CostKey struct {
	Role      string
	TurnID    string
	SessionID string
	OwnerID   string
}

// normalizeCostKey 归一化键里**有全局规则**的维度。
//
//   - owner：见 types.NormalizeOwner。空 owner 在契约里等价于 DefaultOwnerID。
//   - role：TurnRequest.Role 的空值语义是 RoleDialogue（resolveModel 也这样处理）。
//     若这里保留 ""，旧调用路径（不填 Role）的成本会落在 "" 桶，而新路径落在
//     "dialogue" 桶，按角色查询时两边都少一截，且两边的数都不为 0，很难发现。
//
// turn / session 刻意不归一：它们是调用方生成的不透明 ID，空表示"没提供"；
// 编造默认值会合并无关轮次（见文件头）。
func normalizeCostKey(k CostKey) CostKey {
	k.OwnerID = NormalizeOwner(k.OwnerID)
	if k.Role == "" {
		k.Role = RoleDialogue
	}
	return k
}

// CostPricing 说明一条记录的成本为什么可用或不可用。
//
// 只留一个 CostKnown 布尔是不够的：聚合之后 CostKnown=false 会同时代表"整个部署
// 没装价目表"、"这个模型不在价目表里"、"上游没回 usage"三种情况，而它们的处置
// 完全不同（重配 / 补表或告警 / 查上游）。所以每条记录保存原因，快照里再给出
// 分类计数 —— 这比仅仅把 CostKnown 置 false 多一份可运维性。
type CostPricing uint8

const (
	// CostPricingKnown 表示 Cost 是该条的真实金额。
	CostPricingKnown CostPricing = iota
	// CostPricingNoPriceFunc 表示账本没有价目函数（整条链路不计算成本）。
	CostPricingNoPriceFunc
	// CostPricingUnknownModel 表示价目函数对这个模型返回了 <=0。
	CostPricingUnknownModel
	// CostPricingUnknownUsage 表示调用方声明 usage 未采集，因而算不出钱。
	CostPricingUnknownUsage
	// CostPricingExplicitUnknown 表示调用方通过 PriceUnknown 显式声明本条价格未知。
	CostPricingExplicitUnknown
)

// String 返回稳定的原因名，便于日志与验收输出直接比对。
func (p CostPricing) String() string {
	switch p {
	case CostPricingKnown:
		return "known"
	case CostPricingNoPriceFunc:
		return "no_price_func"
	case CostPricingUnknownModel:
		return "unknown_model"
	case CostPricingUnknownUsage:
		return "unknown_usage"
	case CostPricingExplicitUnknown:
		return "explicit_unknown"
	default:
		return "unknown"
	}
}

// CostEntry 是一次成本记录的输入。
//
// UsageKnown=false 时 token 不会被累加：未知的 usage 不是 0 token，混进去会让
// token 小计偏小，且没有任何字段能看出偏小。
type CostEntry struct {
	CostKey
	Model string
	Usage types.Usage
	// UsageKnown 由调用方声明"这份 usage 确实是测到的"。零值 false 表示没采到。
	UsageKnown bool
	// PriceUnknown 让调用方显式声明"本条价格未知"，即使账本有价目函数且能返回
	// 正数。用于模型别名、临时促销价、供应商改了计费口径但本地表还没更新等场景：
	// 此时猜一个价格比标未知更危险。
	PriceUnknown bool
}

// CostRecord 是一条已归集的记录。快照里的 Entry 切片元素就是它。
type CostRecord struct {
	CostKey
	Model      string
	Usage      types.Usage
	UsageKnown bool
	// Cost 只在 Pricing==CostPricingKnown 时有意义；其余情况下恒为 0，调用方
	// 必须看 Pricing，不能把 0 读成免费。
	Cost    float64
	Pricing CostPricing
}

// CostKnown 报告本条金额是否可用。
func (r CostRecord) CostKnown() bool { return r.Pricing == CostPricingKnown }

// CostTotals 是一组记录的合计。
//
// CostKnown 的语义是"这份合计是全额"：Entries>0 且每一条都计价成功。部分计价时
// Cost 仍给出已知部分的和，但 CostKnown=false —— 不能因为"有数"就把它当总额。
// 空组（Entries==0）同样是 CostKnown=false：没有数据不等于成本为 0，这与 trace
// 里 UsageKnown/CostKnown 的纪律一致。
type CostTotals struct {
	Entries     int
	Cost        float64
	CostKnown   bool
	Usage       types.Usage
	TotalTokens int

	PricedEntries          int
	NoPriceFuncEntries     int
	UnknownModelEntries    int
	UnknownUsageEntries    int
	ExplicitUnknownEntries int
}

// add 合并一条记录。只累加已知 usage 的 token 与已知价格的钱。
func (t *CostTotals) add(r CostRecord) {
	t.Entries++
	if r.UsageKnown {
		t.Usage.InputTokens += r.Usage.InputTokens
		t.Usage.OutputTokens += r.Usage.OutputTokens
		t.Usage.CacheReadInputTokens += r.Usage.CacheReadInputTokens
		t.Usage.CacheCreationInputTokens += r.Usage.CacheCreationInputTokens
		t.TotalTokens += r.Usage.InputTokens + r.Usage.OutputTokens +
			r.Usage.CacheReadInputTokens + r.Usage.CacheCreationInputTokens
	}
	switch r.Pricing {
	case CostPricingKnown:
		t.PricedEntries++
		t.Cost += r.Cost
	case CostPricingNoPriceFunc:
		t.NoPriceFuncEntries++
	case CostPricingUnknownModel:
		t.UnknownModelEntries++
	case CostPricingUnknownUsage:
		t.UnknownUsageEntries++
	case CostPricingExplicitUnknown:
		t.ExplicitUnknownEntries++
	}
	// 每条都计价成功才算"全额已知"。部分成功时 Cost 只是下界，不可当总额。
	t.CostKnown = t.Entries > 0 && t.PricedEntries == t.Entries
}

// FallbackEvent 是一次模型回退记录。
//
// 与 TurnTrace 的关系：trace 记录"实际用了哪个模型"，回退事件记录"为什么从
// from 换到 to"。两者分开存，因为一次回退可能横跨两次调用，而 trace 是一轮一条。
// 账本按四维键保存它们，快照里的 Fallbacks 即为回退链（按记录顺序）。
type FallbackEvent struct {
	CostKey
	FromModel string
	ToModel   string
	Reason    string
}

// fallbackKey 是回退集合的内部索引：同一个键内 (from,to) 唯一。
type fallbackKey struct {
	key  CostKey
	from string
	to   string
}

// ModelSwitch 是一次"有模型变化但缺回退事件解释"的转换。
type ModelSwitch struct {
	CostKey
	FromModel string
	ToModel   string
}

// CostLedger 是并发安全的四维成本与回退聚合器。
//
// 零值可用（等价于没有价目函数）；要让金额被计算，用 NewCostLedger 传入价目函数。
type CostLedger struct {
	mu    sync.Mutex
	price CostPriceFunc

	// nowFn 是可选的时钟注入（见 SetNow）。它受 mu 保护，但**在锁外调用**：
	// 调用方代码可能回头读账本，持锁调用会死锁。为零时用 time.Now。
	nowFn func() time.Time

	entries     []CostRecord
	byKey       map[CostKey]CostTotals
	fallbacks   []FallbackEvent
	fallbackSet map[fallbackKey]struct{}
}

// NewCostLedger 构造账本。price 为 nil 表示整条链路不计算金额：
// 成本仍以 CostKnown=false 记录，token 照常累加。
func NewCostLedger(price CostPriceFunc) *CostLedger {
	return &CostLedger{price: price}
}

// SetNow 注入时钟；now 为 nil 时恢复 time.Now。可在构造后、开始使用前调用。
//
// 只影响需要"现在"的派生视图（目前是 CostPeriodLedger 的窗口归属），
// 不改变任何已有记录的成本或键。CostLedger 自身不在 Record 时打时间戳 ——
// 账本记的是金额与用量，时间由调用方在自己需要时给出，避免同一轮被重放时
// 因为两次 time.Now 而落进不同窗口。
func (l *CostLedger) SetNow(now func() time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.nowFn = now
	l.mu.Unlock()
}

// Now 返回账本当前时间（未注入时钟时为 time.Now）。始终为 UTC。
//
// 调用方代码在锁外执行：now 可能很慢甚至阻塞，不能因此卡住所有 Record 的
// 价目计算与追加。
func (l *CostLedger) Now() time.Time {
	if l == nil {
		return time.Now().UTC()
	}
	l.mu.Lock()
	fn := l.nowFn
	l.mu.Unlock()
	if fn == nil {
		return time.Now().UTC()
	}
	return fn().UTC()
}

// priceOf 决定一条记录的成本与原因。**必须在锁外调用**（原因见文件头）。
//
// 判定顺序有意如此：
//   - 显式未知优先：调用方比价目函数更了解这一条的特殊性。
//   - 没价目函数其次：这是部署级配置缺失，比单条模型缺失更该先被看到。
//   - usage 未知再次：不要带着全零 usage 去调价目函数 —— 它多半会返回 0，
//     于是记录被归因成"模型不在表里"，真正的问题（上游没回 usage）反而被藏起来。
//   - 最后才信价目函数的返回值；<=0 一律按未知，绝不按免费。
func (l *CostLedger) priceOf(e CostEntry) (float64, CostPricing) {
	switch {
	case e.PriceUnknown:
		return 0, CostPricingExplicitUnknown
	case l.price == nil:
		return 0, CostPricingNoPriceFunc
	case !e.UsageKnown:
		return 0, CostPricingUnknownUsage
	}
	c := l.price(e.Model, e.Usage)
	if c <= 0 {
		return 0, CostPricingUnknownModel
	}
	return c, CostPricingKnown
}

// Record 记录一次调用的成本，返回落账后的记录（含归一化后的键与计价结果）。
//
// token 与金额分开记：价目函数为 nil 时金额不可知，但 usage 仍然是事实。把两者
// 绑在一起（"没价就不记 token"）会让一轮的最大开销来源从 token 统计里消失。
func (l *CostLedger) Record(e CostEntry) CostRecord {
	if l == nil {
		return CostRecord{}
	}
	cost, pricing := l.priceOf(e)
	rec := CostRecord{
		CostKey:    normalizeCostKey(e.CostKey),
		Model:      e.Model,
		Usage:      e.Usage,
		UsageKnown: e.UsageKnown,
		Cost:       cost,
		Pricing:    pricing,
	}

	l.mu.Lock()
	if l.byKey == nil {
		l.byKey = make(map[CostKey]CostTotals)
	}
	t := l.byKey[rec.CostKey]
	t.add(rec)
	l.byKey[rec.CostKey] = t
	l.entries = append(l.entries, rec)
	l.mu.Unlock()
	return rec
}

// RecordFallback 记录一次回退事件。reason 原样保存（脱敏由调用方负责）。
//
// from==to 也照记不误：它不构成"换模型"，因此不会覆盖任何静默检测；保留它是因为
// 日志里"回退到同一个模型"往往说明重试逻辑有 bug，删掉会丢掉证据。
func (l *CostLedger) RecordFallback(ev FallbackEvent) {
	if l == nil {
		return
	}
	ev.CostKey = normalizeCostKey(ev.CostKey)
	l.mu.Lock()
	if l.fallbackSet == nil {
		l.fallbackSet = make(map[fallbackKey]struct{})
	}
	l.fallbackSet[fallbackKey{key: ev.CostKey, from: ev.FromModel, to: ev.ToModel}] = struct{}{}
	l.fallbacks = append(l.fallbacks, ev)
	l.mu.Unlock()
}

// CostSnapshot 是某一时刻的只读副本。
//
// 所有切片与 map 都是新的：快照取到后，账本继续写入不会改变它（见文件头"并发与
// 快照"）。调用方可以安全地把快照交给日志/指标 goroutine，无需与写入方协调。
type CostSnapshot struct {
	Entries []CostRecord
	Totals  CostTotals
	// ByKey 是精确四维小计；其他 By* 是单维便利视图。
	ByKey     map[CostKey]CostTotals
	ByRole    map[string]CostTotals
	BySession map[string]CostTotals
	ByOwner   map[string]CostTotals
	ByTurn    map[string]CostTotals
	// Fallbacks 是按记录顺序排列的回退链，供 trace/审计直接使用。
	Fallbacks []FallbackEvent
	// SilentSwitches 是"模型变了但没有对应回退事件"的转换列表。
	SilentSwitches []ModelSwitch
}

// Snapshot 在锁内复制全部状态，再在锁外组装（组装是纯读，不碰账本）。
func (l *CostLedger) Snapshot() CostSnapshot {
	if l == nil {
		return emptyCostSnapshot()
	}
	l.mu.Lock()
	entries := append([]CostRecord(nil), l.entries...)
	byKey := make(map[CostKey]CostTotals, len(l.byKey))
	for k, v := range l.byKey {
		byKey[k] = v
	}
	fallbacks := append([]FallbackEvent(nil), l.fallbacks...)
	fset := make(map[fallbackKey]struct{}, len(l.fallbackSet))
	for k := range l.fallbackSet {
		fset[k] = struct{}{}
	}
	l.mu.Unlock()

	snap := emptyCostSnapshot()
	snap.Entries = entries
	snap.ByKey = byKey
	snap.Fallbacks = fallbacks
	for _, r := range entries {
		snap.Totals.add(r)
	}
	for k, v := range byKey {
		mergeTotals(snap.ByRole, k.Role, v)
		mergeTotals(snap.BySession, k.SessionID, v)
		mergeTotals(snap.ByOwner, k.OwnerID, v)
		mergeTotals(snap.ByTurn, k.TurnID, v)
	}
	snap.SilentSwitches = detectSilentSwitches(entries, fset)
	return snap
}

func emptyCostSnapshot() CostSnapshot {
	return CostSnapshot{
		ByKey:     make(map[CostKey]CostTotals),
		ByRole:    make(map[string]CostTotals),
		BySession: make(map[string]CostTotals),
		ByOwner:   make(map[string]CostTotals),
		ByTurn:    make(map[string]CostTotals),
	}
}

// mergeTotals 把一份小计并入单维视图。
//
// 不能直接赋值（后写覆盖先写）：同一个 role 下可能有多个四维键，覆盖只会留下最后
// 一个键的数据，而且数值仍然"看起来合理"。必须逐字段累加；CostKnown 用同样的
// "每条都计价成功"口径重算，不能沿用某一组的布尔。
func mergeTotals(dst map[string]CostTotals, key string, v CostTotals) {
	t := dst[key]
	t.Entries += v.Entries
	t.Cost += v.Cost
	t.Usage.InputTokens += v.Usage.InputTokens
	t.Usage.OutputTokens += v.Usage.OutputTokens
	t.Usage.CacheReadInputTokens += v.Usage.CacheReadInputTokens
	t.Usage.CacheCreationInputTokens += v.Usage.CacheCreationInputTokens
	t.TotalTokens += v.TotalTokens
	t.PricedEntries += v.PricedEntries
	t.NoPriceFuncEntries += v.NoPriceFuncEntries
	t.UnknownModelEntries += v.UnknownModelEntries
	t.UnknownUsageEntries += v.UnknownUsageEntries
	t.ExplicitUnknownEntries += v.ExplicitUnknownEntries
	t.CostKnown = t.Entries > 0 && t.PricedEntries == t.Entries
	dst[key] = t
}

// detectSilentSwitches 找出"模型变了却没有回退事件"的相邻转换。
//
// 检测按 (role, turn, session, owner) 分组、按记录顺序比较相邻模型：
//   - 只看同角色：同一 turn 内 dialogue 与 extract 用不同模型是路由的正常结果。
//   - 每次相邻变化都要一条事件：A→B→A 只记了 A→B 时，B→A 仍是静默的。
//     只检查"本 turn 有没有任意一条回退事件"会把这种半解释状态放过。
func detectSilentSwitches(entries []CostRecord, recorded map[fallbackKey]struct{}) []ModelSwitch {
	if len(entries) == 0 {
		return nil
	}
	prev := make(map[CostKey]string, len(entries))
	var out []ModelSwitch
	for _, r := range entries {
		last, seen := prev[r.CostKey]
		prev[r.CostKey] = r.Model
		if !seen || last == r.Model {
			continue
		}
		fk := fallbackKey{key: r.CostKey, from: last, to: r.Model}
		if _, ok := recorded[fk]; ok {
			continue
		}
		out = append(out, ModelSwitch{CostKey: r.CostKey, FromModel: last, ToModel: r.Model})
	}
	return out
}

// Lookup 返回精确四维键的小计。第二个返回值表示该键是否存在；不存在的键返回
// 零值 CostTotals（CostKnown=false），而不是一个"成本 0 的已知值"。
func (s CostSnapshot) Lookup(key CostKey) (CostTotals, bool) {
	key = normalizeCostKey(key)
	t, ok := s.ByKey[key]
	return t, ok
}

// RoleTotals 返回某个角色的小计（空角色按 dialogue）。
func (s CostSnapshot) RoleTotals(role string) CostTotals {
	if role == "" {
		role = RoleDialogue
	}
	return s.ByRole[role]
}

// SessionTotals 返回某个 session 的小计。空 session 只匹配真正没带 session 的记录。
func (s CostSnapshot) SessionTotals(sessionID string) CostTotals {
	return s.BySession[sessionID]
}

// OwnerTotals 返回某个 owner 的小计（入参按 NormalizeOwner 归一）。
func (s CostSnapshot) OwnerTotals(ownerID string) CostTotals {
	return s.ByOwner[NormalizeOwner(ownerID)]
}

// TurnTotals 返回某个 turn 的小计。
//
// TurnID 按 TurnRequest 的契约是全局幂等键，所以这里用裸 ID 作单维视图的 key；
// 需要连同 owner/session 精确定位时用 Lookup。
func (s CostSnapshot) TurnTotals(turnID string) CostTotals {
	return s.ByTurn[turnID]
}

// HasSilentSwitch 报告快照里是否存在任何静默换模型。
func (s CostSnapshot) HasSilentSwitch() bool { return len(s.SilentSwitches) > 0 }

// TurnHasSilentSwitch 报告指定 turn 是否发生过静默换模型（R-H17 核心验收）。
//
// 用"存在任一未解释转换"而不是"模型数量>1"：角色路由同样会让一个 turn 出现多个
// 模型，但那是声明过的路由，不该算静默。
func (s CostSnapshot) TurnHasSilentSwitch(turnID string) bool {
	for _, sw := range s.SilentSwitches {
		if sw.TurnID == turnID {
			return true
		}
	}
	return false
}

// FallbacksForTurn 返回指定 turn 的回退链（按记录顺序）。
//
// 返回新切片：调用方排序/截断不会改动快照里的 Fallbacks。
func (s CostSnapshot) FallbacksForTurn(turnID string) []FallbackEvent {
	var out []FallbackEvent
	for _, ev := range s.Fallbacks {
		if ev.TurnID == turnID {
			out = append(out, ev)
		}
	}
	return out
}
