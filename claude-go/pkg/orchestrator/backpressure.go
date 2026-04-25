package orchestrator

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// TokenBucket 经典令牌桶限流器。
//
// 算法原理:
//   - 桶中持有若干令牌, 每次请求消耗 1 个
//   - 令牌以固定速率 (rate) 补充, 最多不超过容量 (capacity)
//   - 桶满时新令牌溢出, 实现突发 (burst) 容忍
//   - 令牌不足时阻塞等待, 或直接返回失败 (TryAcquire)
//
// 用途: L1 层背压 — 控制对外部 API (如 LLM) 的每秒请求数。
type TokenBucket struct {
	mu       sync.Mutex
	rate     float64   // 每秒补充的令牌数
	capacity float64   // 桶容量 (最大突发)
	tokens   float64   // 当前令牌数
	lastTime time.Time // 上次计算令牌的时间
	cond     *sync.Cond // 用于通知令牌补充完成, 避免 busy-loop
}

// NewTokenBucket 创建一个令牌桶。初始令牌数 = 桶容量 (允许启动时的突发)。
func NewTokenBucket(ratePerSec, capacity float64) *TokenBucket {
	tb := &TokenBucket{
		rate:     ratePerSec,
		capacity: capacity,
		tokens:   capacity,
		lastTime: time.Now(),
	}
	tb.cond = sync.NewCond(&tb.mu)
	return tb
}

/**
 * Acquire - 阻塞获取一个令牌 (如果令牌不足, 等待令牌补充)
 *
 * 演算示例 (rate=10/s, capacity=5):
 *   时刻 T0: tokens=5 (满桶)
 *   请求 1: tokens=4 → 立即返回
 *   请求 2: tokens=3 → 立即返回
 *   请求 3: tokens=2 → 立即返回
 *   请求 4: tokens=1 → 立即返回
 *   请求 5: tokens=0 → 立即返回 (桶空)
 *   请求 6: tokens=0 → 需要等待 (1-0)/10 = 0.1s → sleep 100ms → 醒来后 tokens>=1 → 返回 ✓
 *   请求 7: tokens=0.0 (被上一个请求消耗) → 等待 100ms → ...
 *
 * 突发能力: 桶满时可以连续处理 5 个请求 (capacity=5), 之后回到稳态每 100ms 处理 1 个。
 * 这就是 RPM 控制的本质: 长期平均速率 = rate, 短期允许突发 = capacity 个请求。
 *
 * 并发安全: 使用 sync.Cond 替代 busy-loop, 多个等待者共享同一个唤醒信号,
 * 避免每个等待者都创建独立的 time.After 导致的不精确问题。
 */
func (tb *TokenBucket) Acquire(ctx context.Context) error {
	for {
		tb.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(tb.lastTime).Seconds()
		tb.tokens = math.Min(tb.capacity, tb.tokens+elapsed*tb.rate)
		tb.lastTime = now
		if tb.tokens >= 1 {
			tb.tokens--
			tb.mu.Unlock()
			return nil
		}
		// 计算需要等待多久才能补充到 1 个令牌: wait = (缺额 / 速率)
		wait := time.Duration((1 - tb.tokens) / tb.rate * float64(time.Second))

		// 启动唤醒 goroutine: 超时或 ctx 取消时广播通知等待者重试
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				tb.cond.Broadcast()
			case <-time.After(wait):
				tb.cond.Broadcast()
			case <-done:
			}
		}()
		// Wait: 解锁 → 等待 Broadcast → 重新加锁 → 返回后进入下一轮循环重新计算令牌
		tb.cond.Wait()
		close(done)

		// 检查 ctx 是否已取消
		select {
		case <-ctx.Done():
			tb.mu.Unlock()
			return ctx.Err()
		default:
			tb.mu.Unlock()
		}
	}
}

// TryAcquire 非阻塞尝试获取一个令牌。
func (tb *TokenBucket) TryAcquire() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.lastTime).Seconds()
	tb.tokens = math.Min(tb.capacity, tb.tokens+elapsed*tb.rate)
	tb.lastTime = now
	if tb.tokens >= 1 {
		tb.tokens--
		return true
	}
	return false
}

// SetRate 动态调整令牌生成速率。
func (tb *TokenBucket) SetRate(ratePerSec float64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.rate = ratePerSec
}

// AdaptiveSemaphore 自适应并发信号量, 实现 AIMD 算法。
//
// AIMD (Additive Increase, Multiplicative Decrease) 算法原理:
//   - 灵感来自 TCP 拥塞控制 (RFC 5681)
//   - 成功时: limit += 1 (加性增加, 缓慢扩容) — "探索更多带宽"
//   - 失败时: limit /= 2 (乘性降低, 快速收缩) — "检测到拥塞, 快速退避"
//   - 保持 limit 在 [minLimit, maxLimit] 范围内
//
// 与 TCP 拥塞控制的类比:
//   TCP: 收到 ACK → cwnd++ ; 丢包 → cwnd/=2
//   本系统: 任务成功 → limit++ ; 任务失败 → limit/=2
//
// 动态调整示例:
//   初始 limit=8
//   成功 → limit=9 → limit=10 → limit=11 → limit=12
//   失败 → limit=6 (快速减半, 保护系统不被错误请求淹没)
//   成功 → limit=7 → limit=8
//
// 用途: L2 层背压 — 根据执行成功率动态调整 Worker 并发度。
// 例如大量 429 错误时自动降低并发, 恢复正常后逐步回升。
type AdaptiveSemaphore struct {
	mu       sync.Mutex
	current  int32 // 当前活跃数
	limit    int32 // 当前并发上限
	minLimit int32 // 最小并发数
	maxLimit int32 // 最大并发数
	ch       chan struct{}
}

// NewAdaptiveSemaphore 创建自适应信号量。
// initial: 初始并发数, min/max: 允许的并发范围。
func NewAdaptiveSemaphore(initial, min, max int) *AdaptiveSemaphore {
	ch := make(chan struct{}, max)
	for i := 0; i < initial; i++ {
		ch <- struct{}{}
	}
	return &AdaptiveSemaphore{
		limit:    int32(initial),
		minLimit: int32(min),
		maxLimit: int32(max),
		ch:       ch,
	}
}

// Acquire 阻塞获取一个并发许可。
func (s *AdaptiveSemaphore) Acquire(ctx context.Context) error {
	select {
	case <-s.ch:
		atomic.AddInt32(&s.current, 1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

/**
 * Release - 归还并发许可 + AIMD 自适应调整 (核心拥塞控制逻辑)
 *
 * AIMD 演算 (Additive Increase Multiplicative Decrease):
 *   初始 limit=8, min=1, max=16
 *
 *   成功路径 (缓慢增长, 探索系统能力):
 *     Release(true)  → limit = 8+1 = 9
 *     Release(true)  → limit = 9+1 = 10
 *     Release(true)  → limit = 10+1 = 11
 *     ... 逐步增长到 maxLimit=16
 *
 *   失败路径 (快速收缩, 保护系统):
 *     Release(false) → limit = 11/2 = 5 (瞬间减半!)
 *     Release(true)  → limit = 5+1 = 6
 *     Release(false) → limit = 6/2 = 3
 *     Release(false) → limit = 3/2 = 1 (降到 minLimit, 不再降)
 *
 *   为什么成功只+1，失败却/2?
 *     - 成功说明系统还有余力, 但不知道余量有多少, 所以缓慢试探 (+1)
 *     - 失败说明系统过载, 必须立即释放资源 (/2)
 *     - 这正是 TCP 拥塞控制的精髓: "乐观时保守, 悲观时激进"
 *
 *   在 LLM 编排场景中的表现:
 *     当 LLM API 开始 429 时, 多个任务并发失败 → limit 快速降低 → 减少并发请求 → API 恢复 → 逐步回升。
 *     整个过程自动调节, 无需人工干预。
 */
func (s *AdaptiveSemaphore) Release(success bool) {
	atomic.AddInt32(&s.current, -1)
	s.mu.Lock()
	if success {
		// 加性增加: 成功时 +1, 不超过最大值 (缓慢探索)
		if s.limit < s.maxLimit {
			s.limit++
		}
	} else {
		// 乘性降低: 失败时减半, 不低于最小值 (快速退避)
		newLimit := s.limit / 2
		if newLimit < s.minLimit {
			newLimit = s.minLimit
		}
		s.limit = newLimit
	}
	s.mu.Unlock()

	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// Limit 返回当前并发上限。
func (s *AdaptiveSemaphore) Limit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.limit)
}

// Active 返回当前活跃并发数。
func (s *AdaptiveSemaphore) Active() int {
	return int(atomic.LoadInt32(&s.current))
}

// BackpressureCtrl 三层背压控制器。
//
// 架构设计:
//
//	┌──────────────────────────────────────────────┐
//	│ L1: RPM 令牌桶        — API 速率限制         │
//	│ (全局每秒请求数上限, 防止被 API 限流)          │
//	├──────────────────────────────────────────────┤
//	│ L2: AIMD 自适应并发    — Worker 并发控制      │
//	│ (根据成功率动态调整, 失败多时自动降速)          │
//	├──────────────────────────────────────────────┤
//	│ L3: 就绪队列深度       — 调度器过载保护        │
//	│ (限制 Ready 队列长度, 防止 OOM)              │
//	└──────────────────────────────────────────────┘
type BackpressureCtrl struct {
	RPM         *TokenBucket       // L1: 全局速率限制
	Concurrency *AdaptiveSemaphore // L2: 自适应并发控制
	QueueDepth  int32              // L3: 就绪队列最大深度; 0 = 无限
	queueSize   int32              // 当前就绪队列大小
}

// NewBackpressureCtrl 创建三层背压控制器。
func NewBackpressureCtrl(rpm float64, rpmBurst float64, concurrency, minConc, maxConc int, queueDepth int) *BackpressureCtrl {
	return &BackpressureCtrl{
		RPM:         NewTokenBucket(rpm, rpmBurst),
		Concurrency: NewAdaptiveSemaphore(concurrency, minConc, maxConc),
		QueueDepth:  int32(queueDepth),
	}
}

// CanEnqueue 检查就绪队列是否还能接受更多任务。
func (bp *BackpressureCtrl) CanEnqueue() bool {
	if bp.QueueDepth <= 0 {
		return true
	}
	return atomic.LoadInt32(&bp.queueSize) < bp.QueueDepth
}

// IncrQueue 就绪队列计数 +1。
func (bp *BackpressureCtrl) IncrQueue() {
	atomic.AddInt32(&bp.queueSize, 1)
}

// DecrQueue 就绪队列计数 -1。
func (bp *BackpressureCtrl) DecrQueue() {
	atomic.AddInt32(&bp.queueSize, -1)
}

// AcquireAll 同时获取 RPM 令牌和并发许可。
// 先获取 RPM 令牌 (可能阻塞等待速率恢复), 再获取并发许可。
func (bp *BackpressureCtrl) AcquireAll(ctx context.Context) error {
	if err := bp.RPM.Acquire(ctx); err != nil {
		return err
	}
	return bp.Concurrency.Acquire(ctx)
}

// ReleaseConc 释放并发许可, 同时反馈执行成败给 AIMD 控制器。
func (bp *BackpressureCtrl) ReleaseConc(success bool) {
	bp.Concurrency.Release(success)
}

// BPSnapshot 背压状态诊断快照。
type BPSnapshot struct {
	ConcLimit  int // 当前并发上限
	ConcActive int // 当前活跃并发数
	QueueSize  int // 当前就绪队列大小
	QueueDepth int // 就绪队列最大深度
}

// Snapshot 获取背压状态快照, 用于监控和诊断。
func (bp *BackpressureCtrl) Snapshot() BPSnapshot {
	return BPSnapshot{
		ConcLimit:  bp.Concurrency.Limit(),
		ConcActive: bp.Concurrency.Active(),
		QueueSize:  int(atomic.LoadInt32(&bp.queueSize)),
		QueueDepth: int(bp.QueueDepth),
	}
}
