// evolution_loop.go —— 统一学习调度循环 (design/03 §4.3「五个学习器、一个循环」)。
//
// # 为什么需要它
//
// 此前学习是**散点触发**: 团队完成路径上直接 `go func(){ LearnFromTeam; Consolidate }`,
// pipeline 与 swarm 各写一遍 (teams.go 两处)。设计明确要求用一个循环取代散点, 原因有三:
//
//  1. **并发不受控**: 每个团队完成都新起一个 goroutine, 五个学习器共享
//     experiences.json / 记忆库这些进程内状态。多团队同时完成时它们互相覆盖,
//     而每处散点都看不到别处在跑。
//  2. **没有预算**: 学习本身要花 LLM 调用 (蒸馏/做梦), 散点触发下无从统计更无从限流,
//     长跑实例的学习成本完全不可见 —— 设计 §4.5 要求"学习成本占比"指标, 前提是有个
//     地方能记账。
//  3. **没有空闲期整理**: 设计要求空闲时做更深的整合 (Consolidate 之外的深度整理),
//     散点触发天然做不到 —— 它只在"有事发生"时被叫醒。
//
// # 设计取舍
//
//   - **绝不阻塞交付**: 提交是非阻塞的, 队列满则丢弃并计数 (fail-open)。学习是增强项,
//     它的背压绝不能传导到团队执行。这与仓内 tracestore 的 writeErr 处理同一风格。
//   - **串行执行学习器**: 队列消费是单 goroutine。五个学习器共享状态, 并行跑等于自找
//     数据竞争; 学习不在关键路径上, 串行的延迟代价可以接受。
//   - **可完全不启用**: Loop 为 nil 时调用方回落到原来的直调路径 (见 teams.go 的
//     submitLearn)。这是向后兼容的硬要求 —— 6 个下游平台的行为不能因为本文件而变。
//   - **幂等去重**: 同一 team 在窗口内重复提交只学一次。团队 refine/重跑会多次走到
//     完成路径, 重复蒸馏同一批轨迹只会污染经验库 (同样的经验被记多次, UCB 计数虚高)。
package agent

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// LearnKind 学习触发的来源。不同来源对应不同的学习器组合。
type LearnKind string

const (
	// LearnTeamDone 团队执行完成: 跑经验学习器 (DISTILL) + 记忆整合。
	LearnTeamDone LearnKind = "team_done"
	// LearnIdle 空闲期深度整理: 只在队列静默一段时间后由循环自己发起。
	LearnIdle LearnKind = "idle"
)

// LearnRequest 一次学习请求。
type LearnRequest struct {
	Kind LearnKind
	Team string // LearnTeamDone 必填
	// RunID 供奖励聚合按 run 归因 (design/03 §4.2); 空则学习器退化为 team 粒度。
	RunID string
}

// LearnStats 循环的运行计数, 供 evo 操作台与指标读取。
// 用 atomic 而非加锁: 只增不减的计数器, 读侧是观测不需要一致性快照。
type LearnStats struct {
	Submitted atomic.Int64 // 提交次数
	Dropped   atomic.Int64 // 队列满被丢弃 (说明学习跟不上产出速度)
	Deduped   atomic.Int64 // 窗口内重复提交被合并
	Executed  atomic.Int64 // 真正执行的学习轮次
	IdleRuns  atomic.Int64 // 空闲期深度整理次数
	Failed    atomic.Int64 // 执行中 panic 被兜住的次数
}

// EvolutionLoopConfig 循环参数。零值可用 (走下面的默认值)。
type EvolutionLoopConfig struct {
	// QueueSize 请求队列长度, 默认 64。满了丢弃而不是阻塞。
	QueueSize int
	// DedupWindow 同 team 去重窗口, 默认 2 分钟。
	DedupWindow time.Duration
	// IdleAfter 队列静默多久后触发空闲期深度整理, 默认 15 分钟; <=0 关闭。
	IdleAfter time.Duration
	// MaxRoundsPerHour 每小时最多执行多少轮学习 (预算闸), 默认 60; <=0 不限。
	MaxRoundsPerHour int
}

func (c EvolutionLoopConfig) withDefaults() EvolutionLoopConfig {
	if c.QueueSize <= 0 {
		c.QueueSize = 64
	}
	if c.DedupWindow <= 0 {
		c.DedupWindow = 2 * time.Minute
	}
	if c.IdleAfter == 0 {
		c.IdleAfter = 15 * time.Minute
	}
	if c.MaxRoundsPerHour == 0 {
		c.MaxRoundsPerHour = 60
	}
	return c
}

// MetricsSink 循环上报指标所需的最小能力 (与 team.metrics() 返回值兼容)。
type MetricsSink interface {
	Record(module, name string, value float64)
}

// EvolutionLoop 统一学习调度循环。
type EvolutionLoop struct {
	cfg     EvolutionLoopConfig
	engine  *EvolutionEngine
	metrics MetricsSink

	reqs  chan LearnRequest
	Stats LearnStats

	// structure 学习器 d/e (工作流/Prompt 进化 + 权重导出), 只在空闲相位跑。
	// nil = 未装配, 空闲期只做 Consolidate (与本文件最初的行为一致)。
	// 见 evolution_structure.go。
	structure *structureLearners

	mu       sync.Mutex
	lastSeen map[string]time.Time // team → 上次提交时刻 (去重)
	// 预算窗口: 简单的滑动小时计数, 不引入额外依赖。
	windowStart time.Time
	windowCount int

	startOnce sync.Once
	stopOnce  sync.Once
	done      chan struct{}
}

// NewEvolutionLoop 构造循环。engine 为 nil 时返回 nil —— 调用方据此回落到直调路径。
func NewEvolutionLoop(engine *EvolutionEngine, metrics MetricsSink, cfg EvolutionLoopConfig) *EvolutionLoop {
	if engine == nil {
		return nil
	}
	cfg = cfg.withDefaults()
	return &EvolutionLoop{
		cfg:      cfg,
		engine:   engine,
		metrics:  metrics,
		reqs:     make(chan LearnRequest, cfg.QueueSize),
		lastSeen: make(map[string]time.Time),
		done:     make(chan struct{}),
	}
}

// Start 启动消费协程 (幂等, 多次调用只起一个)。
func (l *EvolutionLoop) Start(ctx context.Context) {
	if l == nil {
		return
	}
	l.startOnce.Do(func() { go l.run(ctx) })
}

// Stop 停止循环 (幂等)。已入队但未执行的请求被丢弃 —— 学习不是必须完成的工作。
func (l *EvolutionLoop) Stop() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() { close(l.done) })
}

// Submit 非阻塞提交一次学习请求。
//
// 返回值只用于测试与可观测: false 表示被丢弃或去重, **调用方不应据此重试** ——
// 学习跟不上产出速度时正确的行为是丢弃, 而不是把背压传导回交付路径。
func (l *EvolutionLoop) Submit(req LearnRequest) bool {
	if l == nil {
		return false
	}
	l.Stats.Submitted.Add(1)

	if req.Kind == LearnTeamDone && req.Team != "" {
		l.mu.Lock()
		last, ok := l.lastSeen[req.Team]
		if ok && time.Since(last) < l.cfg.DedupWindow {
			l.mu.Unlock()
			l.Stats.Deduped.Add(1)
			return false
		}
		l.lastSeen[req.Team] = time.Now()
		l.mu.Unlock()
	}

	select {
	case l.reqs <- req:
		return true
	default:
		// 队列满: 丢弃并计数。Dropped 持续增长说明学习速度跟不上团队产出,
		// 该调 QueueSize 或看学习器是否卡在某次 LLM 调用上。
		l.Stats.Dropped.Add(1)
		log.Printf("[evolution-loop] 学习队列已满 (size=%d), 丢弃 %s/%s 的学习请求 (累计丢弃 %d)",
			l.cfg.QueueSize, req.Kind, req.Team, l.Stats.Dropped.Load())
		return false
	}
}

// allowRound 预算闸: 每小时最多 MaxRoundsPerHour 轮。
func (l *EvolutionLoop) allowRound() bool {
	if l.cfg.MaxRoundsPerHour <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.windowStart.IsZero() || now.Sub(l.windowStart) >= time.Hour {
		l.windowStart, l.windowCount = now, 0
	}
	if l.windowCount >= l.cfg.MaxRoundsPerHour {
		return false
	}
	l.windowCount++
	return true
}

func (l *EvolutionLoop) run(ctx context.Context) {
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	if l.cfg.IdleAfter > 0 {
		idleTimer = time.NewTimer(l.cfg.IdleAfter)
		idleC = idleTimer.C
		defer idleTimer.Stop()
	}
	resetIdle := func() {
		if idleTimer == nil {
			return
		}
		// Go 1.23+ 起 Stop 后 channel 不再送值, 直接 Reset 即可;
		// 绝不写 `if !Stop() { <-C }` 那个老 drain 惯用法 —— 本仓的
		// pkg/orchestrator 死锁就是它造成的。
		idleTimer.Stop()
		idleTimer.Reset(l.cfg.IdleAfter)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-l.done:
			return
		case req := <-l.reqs:
			resetIdle()
			l.execute(ctx, req)
		case <-idleC:
			resetIdle()
			l.execute(ctx, LearnRequest{Kind: LearnIdle})
		}
	}
}

// execute 跑一轮学习。panic 一律兜住 —— 学习出错绝不能带走进程。
func (l *EvolutionLoop) execute(ctx context.Context, req LearnRequest) {
	if !l.allowRound() {
		log.Printf("[evolution-loop] 本小时学习轮次已达上限 %d, 跳过 %s/%s",
			l.cfg.MaxRoundsPerHour, req.Kind, req.Team)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			l.Stats.Failed.Add(1)
			log.Printf("[evolution-loop] 学习轮次 panic 已兜住: %v", r)
		}
	}()

	start := time.Now()
	switch req.Kind {
	case LearnTeamDone:
		// 经验学习器 (DISTILL) → 记忆整合。顺序有意义: 先从轨迹提炼, 再整合去重。
		l.engine.LearnFromTeam(ctx, req.Team)
		l.engine.Consolidate()
	case LearnIdle:
		// 空闲期只做整合, 不重复蒸馏 (没有新轨迹可蒸馏)。
		l.engine.Consolidate()
		// 学习器 d/e 的相位 (design/03 §4.3d/e): 工作流归纳 + prompt 反思 + 语料导出。
		// 它们的节奏是设计里最慢的两档, 跨多个 run 才有统计意义, 所以只在这里跑。
		l.runStructureLearners(ctx)
		l.Stats.IdleRuns.Add(1)
	}
	if l.metrics != nil {
		l.engine.CollectMetrics(l.metrics)
		// 学习成本占比的前置指标 (design/03 §4.5): 先把耗时与轮次记上,
		// token 口径待 §4.2 的 run 级回溯反馈落地后再补。
		l.metrics.Record("evolution", "learn_round_duration_sec", time.Since(start).Seconds())
		l.metrics.Record("evolution", "learn_round_count", 1)
	}
	l.Stats.Executed.Add(1)
}

// StatsSnapshot 供 evo 操作台/测试读取的一致性快照。
type StatsSnapshot struct {
	Submitted, Dropped, Deduped, Executed, IdleRuns, Failed int64
	QueueLen, QueueCap                                      int
}

// Snapshot 返回当前计数。
func (l *EvolutionLoop) Snapshot() StatsSnapshot {
	if l == nil {
		return StatsSnapshot{}
	}
	return StatsSnapshot{
		Submitted: l.Stats.Submitted.Load(),
		Dropped:   l.Stats.Dropped.Load(),
		Deduped:   l.Stats.Deduped.Load(),
		Executed:  l.Stats.Executed.Load(),
		IdleRuns:  l.Stats.IdleRuns.Load(),
		Failed:    l.Stats.Failed.Load(),
		QueueLen:  len(l.reqs),
		QueueCap:  cap(l.reqs),
	}
}
