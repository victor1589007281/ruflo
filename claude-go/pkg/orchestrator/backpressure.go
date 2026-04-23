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
}

// NewTokenBucket 创建一个令牌桶。初始令牌数 = 桶容量 (允许启动时的突发)。
func NewTokenBucket(ratePerSec, capacity float64) *TokenBucket {
	return &TokenBucket{
		rate:     ratePerSec,
		capacity: capacity,
		tokens:   capacity,
		lastTime: time.Now(),
	}
}

// Acquire 阻塞获取一个令牌。如果令牌不足, 计算需要等待的时间并 sleep。
// 支持通过 ctx 取消等待。
func (tb *TokenBucket) Acquire(ctx context.Context) error {
	for {
		tb.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(tb.lastTime).Seconds()
		// 按经过时间补充令牌, 不超过容量
		tb.tokens = math.Min(tb.capacity, tb.tokens+elapsed*tb.rate)
		tb.lastTime = now
		if tb.tokens >= 1 {
			tb.tokens--
			tb.mu.Unlock()
			return nil
		}
		// 计算需要等待多久才能补充到 1 个令牌
		wait := time.Duration((1 - tb.tokens) / tb.rate * float64(time.Second))
		tb.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
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

// Release 归还并发许可, 并根据成败调整上限 (AIMD)。
func (s *AdaptiveSemaphore) Release(success bool) {
	atomic.AddInt32(&s.current, -1)
	s.mu.Lock()
	if success {
		// 加性增加: 成功时 +1, 不超过最大值
		if s.limit < s.maxLimit {
			s.limit++
		}
	} else {
		// 乘性降低: 失败时减半, 不低于最小值
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
