// queue_worker.go — 持久任务队列的运行时（Moqi R-H13）。
//
// # 为什么队列实现不够
//
// queue.go 只提供了 Claim/Ack/Nack/Renew 这些**原语**。R-H13 的验收写的是
// "harness 提供运行时"：至少一次投递、租约续期、优雅停止、并发控制、退避由
// 队列负责，而"谁来反复 Claim、拿到任务后怎么执行、停止时在途任务怎么办"
// 必须有一个载体。改造前没有任何生产代码调用 StoreQueue —— 契约测试全绿，
// 但部署里没有任何任务会被执行，这是"实现存在、能力不存在"的典型形态。
//
// # 三条运行时纪律
//
//  1. **租约活着**：handler 执行期间按 lease/3 续租。续租失败（任务已被别人
//     领走）立即取消 handler 的 ctx，且**不再 Ack/Nack** —— 此时我们已不是
//     持有者，任何写入都可能覆盖新持有者的结果。
//  2. **至少一次，不承诺恰好一次**：handler 成功才 Ack；失败/panic 一律 Nack，
//     由队列的退避与死信策略决定后续。重复执行是契约的一部分，handler 的
//     IdempotencyKey 才是去重的地方。
//  3. **优雅停止**：Stop 先停轮询，给在途 handler 一段宽限；宽限耗尽才取消
//     它们的 ctx。停轮询与杀在途分开，否则一次发布就会把正在跑的任务全变成
//     半成品（而且它们会被 Nack 后重跑，成本翻倍）。
package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TaskHandler 执行一个任务。
//
// 收到的是 ClaimedTask（含 Attempts / LeaseUntil），handler 可以据此做
// "第几次尝试"的分支；返回值决定 Ack（nil）还是 Nack（非 nil）。
//
// 幂等由 handler 自己保证：至少一次投递意味着同一个 IdempotencyKey 可能被执行
// 多次（worker 在写结果后、Ack 前崩溃是最常见的一种）。
type TaskHandler interface {
	HandleTask(ctx context.Context, task ClaimedTask) error
}

// TaskHandlerFunc 让函数满足 TaskHandler。
type TaskHandlerFunc func(ctx context.Context, task ClaimedTask) error

// HandleTask 实现 TaskHandler。
func (f TaskHandlerFunc) HandleTask(ctx context.Context, task ClaimedTask) error {
	return f(ctx, task)
}

// 队列 worker 默认值。
const (
	defaultWorkerConcurrency = 1
	defaultWorkerPoll        = 500 * time.Millisecond
	defaultWorkerStopGrace   = 30 * time.Second
	// workerAckTimeout 是 Ack/Nack 自身的超时。它不跟随任务 ctx：
	// 任务被取消后仍必须把"失败"写回队列，否则任务要等租约过期才会重派。
	workerAckTimeout = 10 * time.Second
)

// ErrQueueWorkerRunning 表示同一个 worker 被 Start 了两次。
//
// 不做静默幂等：两次 Start 会起两倍的轮询 goroutine，而调用方通常以为只有一个。
var ErrQueueWorkerRunning = errors.New("harness: 队列 worker 已在运行")

// QueueWorkerOptions 是 QueueWorker 的装配参数。
type QueueWorkerOptions struct {
	// Queue 是任务队列。必填。
	Queue TaskQueue
	// Handler 是任务处理器。必填。
	Handler TaskHandler
	// WorkerID 是租约持有者标识。空值自动生成（进程内唯一即可）。
	WorkerID string
	// Kinds 是只领取这些 kind；空 = 全部。
	Kinds []string
	// Caps 是本 worker 的能力标签。nil = 忽略 RequireCaps（兼容旧队列）；
	// 非 nil（含空切片）= 只领取 RequireCaps ⊆ Caps 的任务。
	Caps []string
	// Lease 是单次领取的租约时长；<=0 用队列默认。
	Lease time.Duration
	// Concurrency 是并发 handler 数；<=0 取 1。
	Concurrency int
	// PollInterval 是空队列时的轮询间隔；<=0 取 500ms。
	PollInterval time.Duration
	// StopGrace 是 Stop 给在途 handler 的宽限；<=0 取 30s。
	StopGrace time.Duration
	// OnError 接收运行时错误（Claim/Renew/Ack/Nack 失败）。可为 nil。
	//
	// 它**不**接收 handler 的业务错误：那是 Nack 的正常输入，不是运行时故障。
	OnError func(error)
}

// QueueWorkerStats 是 worker 的累计计数（观测用）。
type QueueWorkerStats struct {
	Claimed    int64
	Completed  int64
	Failed     int64
	LeaseLost  int64
	ClaimError int64
}

// QueueWorker 是 TaskQueue 的进程内运行时。
//
// 一个实例可以被 Start 一次；Stop 之后不重启（重启需要新的实例，避免把上一轮
// 的 stats 与 ctx 混进来）。
type QueueWorker struct {
	opts QueueWorkerOptions

	mu       sync.Mutex
	started  bool
	stopping bool
	// pollCancel 只停轮询；taskCancel 停所有在途 handler。
	pollCancel context.CancelFunc
	taskCancel context.CancelFunc
	stats      QueueWorkerStats

	loops sync.WaitGroup
	tasks sync.WaitGroup
}

// NewQueueWorker 校验并构造 worker。
func NewQueueWorker(opts QueueWorkerOptions) (*QueueWorker, error) {
	if opts.Queue == nil {
		return nil, errors.New("harness: QueueWorker 缺少 Queue")
	}
	if opts.Handler == nil {
		return nil, errors.New("harness: QueueWorker 缺少 Handler")
	}
	if opts.WorkerID == "" {
		opts.WorkerID = "harness-worker-" + string(newTaskID())
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultWorkerConcurrency
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultWorkerPoll
	}
	if opts.StopGrace <= 0 {
		opts.StopGrace = defaultWorkerStopGrace
	}
	return &QueueWorker{opts: opts}, nil
}

// Start 起轮询 goroutine。ctx 结束会同时停止轮询与在途 handler。
func (w *QueueWorker) Start(ctx context.Context) error {
	if w == nil {
		return errors.New("harness: nil QueueWorker")
	}
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return ErrQueueWorkerRunning
	}
	w.started = true
	pollCtx, pollCancel := context.WithCancel(ctx)
	// taskCtx 与 pollCtx 分开：Stop 先只取消 pollCtx，给在途任务收尾的机会。
	taskCtx, taskCancel := context.WithCancel(ctx)
	w.pollCancel, w.taskCancel = pollCancel, taskCancel
	n := w.opts.Concurrency
	w.mu.Unlock()

	for i := 0; i < n; i++ {
		w.loops.Add(1)
		go w.loop(pollCtx, taskCtx)
	}
	return nil
}

// Stop 优雅停止：先停轮询，等待在途任务，宽限耗尽才取消它们。
//
// ctx 控制调用方愿意等多久；w.opts.StopGrace 是给 handler 的宽限（两者取更早
// 到的那个）。返回错误表示有任务没能在宽限内结束 —— 它们会被取消并 Nack，
// 由队列重派，不是数据丢失。
func (w *QueueWorker) Stop(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	if !w.started || w.stopping {
		w.mu.Unlock()
		return nil
	}
	w.stopping = true
	pollCancel, taskCancel := w.pollCancel, w.taskCancel
	grace := w.opts.StopGrace
	w.mu.Unlock()

	// 1) 不再领新任务。
	pollCancel()

	// 2) 等在途 handler。
	//
	// 每个 loop goroutine 是**同步**执行 runTask 的，所以 loops.Wait 已经覆盖
	// "所有已领取的任务处理完"。改造前这里先无超时地 loops.Wait()、再单独等
	// tasks：一个不响应 ctx 的 handler 会让 Stop 永远挂住，StopGrace 形同虚设。
	finished := make(chan struct{})
	go func() {
		w.loops.Wait()
		close(finished)
	}()

	graceCtx, cancelGrace := context.WithTimeout(ctx, grace)
	defer cancelGrace()
	select {
	case <-finished:
		taskCancel()
		return nil
	case <-graceCtx.Done():
		// 3) 宽限耗尽：取消在途，再给 5s 让它们退出（handler 应尊重 ctx）。
		// 仍不退出就只能返回错误 —— 无法从外部杀死一个不检查 ctx 的 goroutine，
		// 把它伪装成"已停止"会让调用方以为进程可以安全退出。
		taskCancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
		}
		return fmt.Errorf("harness: 队列 worker 停止超时 (%s): %w", grace, graceCtx.Err())
	}
}

// Stats 返回累计计数快照。
func (w *QueueWorker) Stats() QueueWorkerStats {
	if w == nil {
		return QueueWorkerStats{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// loop 是单个轮询 goroutine。
func (w *QueueWorker) loop(pollCtx, taskCtx context.Context) {
	defer w.loops.Done()
	for {
		if pollCtx.Err() != nil {
			return
		}
		task, err := w.opts.Queue.Claim(pollCtx, w.opts.WorkerID, w.opts.Kinds, w.opts.Lease)
		if err != nil {
			if pollCtx.Err() != nil {
				return
			}
			w.addClaimError()
			w.report(fmt.Errorf("harness: claim 失败: %w", err))
			if !sleepCtx(pollCtx, w.opts.PollInterval) {
				return
			}
			continue
		}
		if task == nil {
			if !sleepCtx(pollCtx, w.opts.PollInterval) {
				return
			}
			continue
		}
		w.runTask(taskCtx, task)
	}
}

// runTask 执行一个已领取的任务，并按结果 Ack/Nack。
func (w *QueueWorker) runTask(taskCtx context.Context, task *ClaimedTask) {
	w.tasks.Add(1)
	defer w.tasks.Done()
	w.addClaimed()

	hctx, hcancel := context.WithCancel(taskCtx)
	defer hcancel()

	// 租约续期：失败说明任务已经不属于我们，立即取消 handler。
	hbDone := make(chan struct{})
	var leaseMu sync.Mutex
	leaseLost := false
	lease := w.opts.Lease
	if lease <= 0 {
		lease = defaultLease
	}
	renewEvery := lease / 3
	if renewEvery <= 0 {
		renewEvery = time.Second
	}
	go func() {
		// hbDone 由 runTask 关闭：续租 goroutine 可能先于 handler 返回
		// （续租失败时它主动退出），两边都 close 会 panic。
		t := time.NewTicker(renewEvery)
		defer t.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
				if err := w.renew(hctx, TaskID(task.ID), lease); err != nil {
					leaseMu.Lock()
					leaseLost = true
					leaseMu.Unlock()
					w.report(fmt.Errorf("harness: 任务 %s 续租失败, 放弃持有: %w", task.ID, err))
					hcancel()
					return
				}
			}
		}
	}()

	// panic 也必须走 Nack：否则任务停在 leased 状态直到租约过期，
	// 而租约过期是分钟级；一次可恢复的 panic 会因此让任务"卡住几分钟"。
	var handleErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				handleErr = fmt.Errorf("harness: 任务 %s handler panic: %v", task.ID, r)
			}
		}()
		handleErr = w.opts.Handler.HandleTask(hctx, *task)
	}()

	close(hbDone)
	<-hbDone

	leaseMu.Lock()
	lost := leaseLost
	leaseMu.Unlock()
	if lost {
		// 已不是持有者：任何 Ack/Nack 都可能与新持有者竞争。
		w.addLeaseLost()
		return
	}

	// Ack/Nack 自身不能跟随已取消的任务 ctx：任务失败后仍要把结果写回队列。
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(taskCtx), workerAckTimeout)
	defer cancel()
	if handleErr == nil {
		if err := w.opts.Queue.Ack(ackCtx, TaskID(task.ID)); err != nil {
			w.report(fmt.Errorf("harness: 任务 %s Ack 失败: %w", task.ID, err))
			return
		}
		w.addCompleted()
		return
	}
	if err := w.opts.Queue.Nack(ackCtx, TaskID(task.ID), handleErr); err != nil {
		w.report(fmt.Errorf("harness: 任务 %s Nack 失败: %w", task.ID, err))
		return
	}
	w.addFailed()
}

// renew 续租。优先用 worker 作用域的 RenewFor（能发现"租约已被别人抢走"）；
// 旧实现只实现 Renew 时退回它。
func (w *QueueWorker) renew(ctx context.Context, id TaskID, lease time.Duration) error {
	renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerAckTimeout)
	defer cancel()
	if r, ok := w.opts.Queue.(interface {
		RenewFor(context.Context, TaskID, string, time.Duration) error
	}); ok {
		return r.RenewFor(renewCtx, id, w.opts.WorkerID, lease)
	}
	return w.opts.Queue.Renew(renewCtx, id, lease)
}

func (w *QueueWorker) report(err error) {
	if err != nil && w.opts.OnError != nil {
		w.opts.OnError(err)
	}
}

func (w *QueueWorker) addClaimed() {
	w.mu.Lock()
	w.stats.Claimed++
	w.mu.Unlock()
}

func (w *QueueWorker) addCompleted() {
	w.mu.Lock()
	w.stats.Completed++
	w.mu.Unlock()
}

func (w *QueueWorker) addFailed() {
	w.mu.Lock()
	w.stats.Failed++
	w.mu.Unlock()
}

func (w *QueueWorker) addLeaseLost() {
	w.mu.Lock()
	w.stats.LeaseLost++
	w.mu.Unlock()
}

func (w *QueueWorker) addClaimError() {
	w.mu.Lock()
	w.stats.ClaimError++
	w.mu.Unlock()
}

// sleepCtx 等待 d 或 ctx 结束；ctx 结束返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
