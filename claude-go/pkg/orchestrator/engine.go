package orchestrator

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// EngineConfig 引擎全局配置。
type EngineConfig struct {
	MaxParallel     int           // 最大并发任务数 (0 = 自动取 DAG 宽度)
	DefaultTimeout  time.Duration // 单任务默认超时
	StallTimeout    time.Duration // 无进展多久后触发停滞恢复
	MaxStallRecover int           // 最大停滞恢复尝试次数
	CheckpointEvery int           // 每完成 N 个任务保存一次检查点 (0 = 禁用)
	RetryPolicy     RetryPolicy   // 重试策略
	RPM             float64       // 每分钟请求数限制 (0 = 无限制)
	RPMBurst        float64       // 突发容量
	QueueDepth      int           // 就绪队列最大深度 (0 = 无限制)
}

// DefaultEngineConfig 返回生产环境推荐默认配置。
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		MaxParallel:     8,
		DefaultTimeout:  10 * time.Minute,
		StallTimeout:    5 * time.Minute,
		MaxStallRecover: 3,
		CheckpointEvery: 1,
		RetryPolicy:     DefaultRetryPolicy(),
		RPM:             60,
		RPMBurst:        10,
		QueueDepth:      100,
	}
}

// Engine 是工作流编排引擎的核心, 负责驱动整个 DAG 从启动到完成。
//
// 调度算法设计 (融合 K8s + GMP + io_uring):
//
//	┌─────────────┐    ┌──────────────┐    ┌───────────┐
//	│ 事件驱动解锁 │───▶│ Filter→Score │───▶│  并发派发  │
//	│ (任务完成时  │    │ →Dispatch    │    │ (goroutine│
//	│  立即解锁   │    │ (K8s 三阶段)  │    │   池)     │
//	│  下游依赖)  │    └──────────────┘    └─────┬─────┘
//	└─────────────┘                              │
//	       ▲                                     │
//	       │         ┌──────────────┐            │
//	       └─────────│ 完成信号通道  │◀───────────┘
//	                 │ (doneCh)     │
//	                 └──────────────┘
//
// 关键机制:
//  1. 事件驱动解锁: 任务完成时立即检查并解锁下游, 无需轮询
//  2. Filter→Score→Dispatch: 借鉴 K8s 调度器的可插拔三阶段管线
//  3. 流式派发: 借鉴 io_uring 的完成队列, 任务完成即刻触发下一轮调度
//  4. 停滞检测+恢复: 定时检查是否有进展, 无进展时自动恢复挂起的任务
//  5. 检查点: 每完成 N 个任务持久化完整状态, 支持断点续跑
type Engine struct {
	config     EngineConfig
	scheduler  *Scheduler
	runners    *RunnerRegistry
	blackboard *Blackboard
	checkpoint CheckpointStore
	hook       LifecycleHook
	bp         *BackpressureCtrl

	// ---- 运行时状态 ----
	mu            sync.Mutex
	completed     int
	failed        int
	cancelled     int
	suspended     int
	totalRetries  int
	lastProgress  time.Time // 上次有进展的时间 (用于停滞检测)
	stallRecovers int       // 已执行的停滞恢复次数
	startTime     time.Time
	execID        string
	running       bool      // 引擎是否正在执行 (供外部监控查询)
}

// NewEngine 创建引擎实例。
// 内部自动初始化背压控制器、调度器、注册表等子系统。
func NewEngine(config EngineConfig) *Engine {
	bp := NewBackpressureCtrl(
		config.RPM/60, config.RPMBurst,
		config.MaxParallel, 1, config.MaxParallel*2,
		config.QueueDepth,
	)

	e := &Engine{
		config:     config,
		scheduler:  NewScheduler(),
		runners:    NewRunnerRegistry(),
		blackboard: NewBlackboard(),
		checkpoint: NewMemoryCheckpointStore(),
		hook:       NoopHook{},
		bp:         bp,
	}

	// 内置 noop 执行器, 用于同步点 / Join 节点
	e.runners.Register(&NoopRunner{})
	return e
}

// SetCheckpointStore 替换默认的内存检查点存储为自定义实现 (如文件/数据库)。
func (e *Engine) SetCheckpointStore(cs CheckpointStore) { e.checkpoint = cs }

// SetHook 设置生命周期 Hook (可用 MultiHook 组合多个)。
func (e *Engine) SetHook(h LifecycleHook) { e.hook = h }

// SetBlackboard 替换默认黑板。
func (e *Engine) SetBlackboard(bb *Blackboard) { e.blackboard = bb }

// Scheduler 返回调度器实例, 用于添加自定义 Filter/Score 插件。
func (e *Engine) Scheduler() *Scheduler { return e.scheduler }

// Runners 返回执行器注册表, 用于注册自定义 TaskRunner。
func (e *Engine) Runners() *RunnerRegistry { return e.runners }

// BB 返回黑板实例, 用于外部读取共享状态。
func (e *Engine) BB() *Blackboard { return e.blackboard }

// IsRunning 返回引擎是否正在执行工作流 (供外部监控/心跳检测使用)。
func (e *Engine) IsRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// ExecutionResult 是引擎运行的最终结果。
type ExecutionResult struct {
	ExecID  string            // 执行 ID (graphID-timestamp)
	Success bool              // 是否全部成功 (无 failed/cancelled)
	Metrics ExecutionMetrics  // 执行指标
	Outputs map[string]any    // 各任务的输出
	Errors  map[string]string // 各任务的错误信息
}

// Run 执行工作流图直到所有任务完成或失败。
//
// 完整执行流程图 (以 parenting 工作流 8 阶段为例):
//
//	┌──────────────────────────────────────────────────────────────┐
//	│ Step 1: g.Build()                                           │
//	│   ┌─────────┐    ┌──────────────┐    ┌──────────────┐       │
//	│   │intake   │───▶│safety-screen │───▶│academic-tutor│       │
//	│   │(Ready)  │    │(Blocked)     │    │(Blocked)     │       │
//	│   └─────────┘    └──────────────┘    └──────────────┘       │
//	│                           │              ┌───────────────┐   │
//	│                           │              │psychology-coach│  │
//	│                           │              │(Blocked)      │   │
//	│                           │              └───────────────┘   │
//	│                           │              ┌───────────────┐   │
//	│                           │              │parenting-advisor│  │
//	│                           │              │(Blocked)      │   │
//	│                           │              └───────────────┘   │
//	│                           │              ┌───────────────┐   │
//	│                           │              │dev-assessor   │   │
//	│                           │              │(Blocked)      │   │
//	│                           └──────────────┴───────────────┘   │
//	│                                          │                    │
//	│                                    ┌─────▼──────┐            │
//	│                                    │action-plan │            │
//	│                                    │(Blocked)   │            │
//	│                                    └─────┬──────┘            │
//	│                                          │                    │
//	│                                    ┌─────▼──────────┐        │
//	│                                    │consultation-report│     │
//	│                                    │(Blocked)        │        │
//	│                                    └────────────────┘        │
//	└──────────────────────────────────────────────────────────────┘
//
//	┌──────────────────────────────────────────────────────────────┐
//	│ Step 2: 主循环 (调度 → 派发 → 等待 → 处理)                    │
//	│                                                              │
//	│ Round 1: 调度 intake (唯一 Ready)                            │
//	│   Filter(1) → Score(1) → Dispatch(1) → go executeTask()     │
//	│   等待 doneCh: intake 完成                                   │
//	│   handleDone: intake→Completed, 写黑板, unblock safety-screen│
//	│                                                              │
//	│ Round 2: 调度 safety-screen                                 │
//	│   Filter(1) → Score(1) → Dispatch(1) → go executeTask()     │
//	│   等待 doneCh: safety-screen 完成                            │
//	│   handleDone: safety-screen→Completed, 解锁 4 个并行阶段      │
//	│                                                              │
//	│ Round 3: 调度 4 个并行阶段 (MaxParallel=2, 本批取 2 个)       │
//	│   Filter(4) → Score(4) → Dispatch(2) → 2×go executeTask()   │
//	│   等待 doneCh: 第 1 个完成 → drain 排空第 2 个               │
//	│                                                              │
//	│ Round 4: 调度剩余 2 个并行阶段                               │
//	│   Filter(2) → Score(2) → Dispatch(2) → 2×go executeTask()   │
//	│   等待 doneCh: 2 个完成后解锁 action-plan                    │
//	│                                                              │
//	│ Round 5: 调度 action-plan → consultation-report              │
//	└──────────────────────────────────────────────────────────────┘
//
// 关键机制解读:
//  1. 事件驱动解锁: 任务完成后立即调用 unblockDownstream(),
//     检查下游依赖是否全部满足, 满足则 Blocked→Ready, 无需轮询
//  2. Filter→Score→Dispatch: 每轮调度都重新评估, 考虑背压/依赖/优先级
//  3. 流式派发: doneCh 是核心通道, 任务完成即刻触发下一轮调度
//  4. 停滞检测: 30s 定时器检查 lastProgress, 超阈值则尝试恢复
//  5. 检查点: 每完成 CheckpointEvery 个任务, 序列化全量状态到磁盘
/**
 * Run - 引擎主循环：调度 → 派发 → 等待 → 处理完成信号 → 重复直到所有任务终态
 *
 * 核心设计解读:
 * ┌─────────────────────────────────────────────────────┐
 * │  主循环是一个状态机，每次迭代做以下三件事之一：        │
 * │  1. 调度一批 Ready 任务 (Filter→Score→Dispatch)      │
 * │  2. 处理完成信号 (从 doneCh 读取)                    │
 * │  3. 停滞检测 (30s 定时器)                            │
 * │                                                     │
 * │  关键：任务完成时立即 drain 排空通道中剩余的完成信号    │
 * │  这减少了调度轮次，一次可以批量处理多个完成事件         │
 * └─────────────────────────────────────────────────────┘
 *
 * 并发控制示例 (假设 maxPar=2):
 *   Round 1: 调度 2 个任务 (avail=2) → 2 个 goroutine 执行 → doneCh 最多 2 个信号排队
 *   Round 2: 收到 1 个完成信号 → handleDone → drain 排空第 2 个 → 重新调度 (avail=2)
 *   Round 3: 收到 2 个完成信号 (同时完成) → handleDone → drain 不阻塞 → 重新调度 (avail=2)
 *
 * 时间复杂度: 每轮调度 O(V log V) [排序], 总轮数 = O(V / maxPar)
 * 总体时间复杂度: O(V² log V / maxPar)，但实际远小于此（因为 ReadyTasks 通常只返回少量任务）
 */
func (e *Engine) Run(ctx context.Context, g *Graph) (*ExecutionResult, error) {
	if err := g.Build(); err != nil {
		return nil, fmt.Errorf("图构建失败: %w", err)
	}

	e.execID = fmt.Sprintf("%s-%d", g.ID, time.Now().UnixMilli())
	e.startTime = time.Now()
	e.lastProgress = time.Now()
	e.completed = 0
	e.failed = 0
	e.cancelled = 0
	e.suspended = 0
	e.totalRetries = 0
	e.stallRecovers = 0
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.running = false; e.mu.Unlock() }()

	// 自动适配并发度: maxPar = min(config.MaxParallel, DAGWidth)
	// 举例: 配置 MaxParallel=8 但 DAG 最大宽度只有 4（即最多 4 个任务能并行）
	// 则 maxPar=4，避免创建多余的 goroutine 浪费资源
	maxPar := e.config.MaxParallel
	if maxPar <= 0 || maxPar > g.DAGWidth() {
		maxPar = g.DAGWidth()
	}
	if maxPar < 1 {
		maxPar = 1
	}

	e.hook.OnGraphStart(g)

	// 完成信号通道: 任务 goroutine 完成后向此通道发送结果
	doneCh := make(chan taskDone, maxPar*2)

	// 停滞检测定时器: 使用 StallTimeout / 10 作为检测间隔
	// 例如 StallTimeout=5min → 检测间隔=30s
	stallCheckInterval := e.config.StallTimeout / 10
	if stallCheckInterval < 5*time.Second {
		stallCheckInterval = 5 * time.Second // 最少 5 秒，避免过于频繁检测
	}
	stallTicker := time.NewTicker(stallCheckInterval)
	defer stallTicker.Stop()

	totalTasks := g.TaskCount()

	for {
		if e.isComplete(totalTasks) {
			break
		}

		select {
		case <-ctx.Done():
			e.cancelRemaining(g)
			return e.buildResult(g), ctx.Err()
		default:
		}

		// ---- 调度阶段: Filter → Score → Dispatch ----
		sCtx := &SchedulerContext{
			Graph:      g,
			Blackboard: e.blackboard,
			BP:         e.bp,
			RunningIDs: e.runningIDs(g),
		}
		avail := maxPar - e.runningCount(g)
		batch := e.scheduler.Schedule(sCtx, avail)
		if len(batch) > 0 {
			log.Printf("[engine] 调度: avail=%d, batch=%d", avail, len(batch))
		}

		for _, t := range batch {
			t.mu.Lock()
			t.setStateInternal(TaskRunning)
			t.startedAt = time.Now()
			t.mu.Unlock()

			e.bp.IncrQueue()
			e.hook.OnTaskStart(t)

			go e.executeTask(ctx, g, t, doneCh)
		}

		// 等待至少一个完成信号, 或停滞检测定时器
		select {
		case done := <-doneCh:
			e.handleDone(g, done)
		case <-stallTicker.C:
			e.checkStall(g)
		case <-ctx.Done():
			e.cancelRemaining(g)
			return e.buildResult(g), ctx.Err()
		}

		// 非阻塞排空已完成的信号 (批量处理, 减少调度轮次)
	drain:
		for {
			select {
			case done := <-doneCh:
				e.handleDone(g, done)
			default:
				break drain
			}
		}

		// 超时排空: 处理正在发送中但还未到达通道的信号 (最多 5ms)
		// 这减少了高并发下的任务状态短暂不一致窗口
		// 修复: 使用 time.NewTimer 替代 time.After, 避免主循环每轮泄漏 timer
		drainTimer := time.NewTimer(5 * time.Millisecond)
	drainTimeout:
		for {
			select {
			case done := <-doneCh:
				if !drainTimer.Stop() {
					<-drainTimer.C
				}
				e.handleDone(g, done)
			case <-drainTimer.C:
				break drainTimeout
			}
		}
		drainTimer.Stop()
	}

	metrics := e.buildMetrics(totalTasks)
	e.hook.OnGraphComplete(g, metrics)
	e.saveCheckpoint(g)

	return e.buildResult(g), nil
}

// taskDone 是任务 goroutine 完成后发送的结果信号。
type taskDone struct {
	taskID string
	output any
	err    error
}

// executeTask 在独立 goroutine 中执行单个任务。
//
// 执行流程:
//  1. 设置超时上下文
//  2. 获取背压许可 (RPM 令牌 + 并发信号量)
//  3. 查找并调用 TaskRunner
//  4. 释放并发许可 (AIMD: 成功+1, 失败/2)
//  5. 向 doneCh 发送结果
func (e *Engine) executeTask(ctx context.Context, g *Graph, t *Task, doneCh chan<- taskDone) {
	defer e.bp.DecrQueue()

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = e.config.DefaultTimeout
	}

	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 获取背压许可 (L1: RPM + L2: 并发)
	if err := e.bp.AcquireAll(taskCtx); err != nil {
		log.Printf("[engine] task %s: 背压获取失败: %v", t.ID, err)
		doneCh <- taskDone{taskID: t.ID, err: err}
		return
	}

	runner, err := e.runners.Get(t.Runner)
	if err != nil {
		log.Printf("[engine] task %s: runner 不存在: %v", t.ID, err)
		e.bp.ReleaseConc(false)
		doneCh <- taskDone{taskID: t.ID, err: err}
		return
	}

	log.Printf("[engine] task %s: 开始执行 runner=%s", t.ID, runner.Name())
	output, runErr := runner.Execute(taskCtx, t, e.blackboard)
	success := runErr == nil
	e.bp.ReleaseConc(success)

	if runErr != nil {
		log.Printf("[engine] task %s: 执行失败: %v", t.ID, runErr)
	} else {
		log.Printf("[engine] task %s: 执行成功", t.ID)
	}

	doneCh <- taskDone{taskID: t.ID, output: output, err: runErr}
}

// handleDone 处理任务完成信号。
// 成功: 更新状态 → 写黑板 → 解锁下游 → 保存检查点
// 失败: 交给 handleFailure 处理重试/级联/挂起
func (e *Engine) handleDone(g *Graph, done taskDone) {
	t := g.Tasks[done.taskID]
	if t == nil {
		return
	}

	if done.err == nil {
		// ---- 成功路径 ----
		t.mu.Lock()
		t.setStateInternal(TaskCompleted)
		t.output = done.output
		t.completedAt = time.Now()
		t.mu.Unlock()

		e.mu.Lock()
		e.completed++
		e.lastProgress = time.Now()
		e.mu.Unlock()

		// 将输出写入黑板, 供下游任务通过 bb.Read("taskID/output") 获取
		e.blackboard.Write(t.ID+"/output", done.output, WriteMeta{Author: t.Runner, Category: "output"})

		e.hook.OnTaskComplete(t, done.output)
		e.unblockDownstream(g, t.ID)
		e.maybeCheckpoint(g)
	} else {
		e.handleFailure(g, t, done.err)
	}
}

// handleFailure 统一处理任务失败。
//
// 三级错误分治决策树:
//
//	task 失败 → ClassifyError(err)
//	  │
//	  ├─ ErrorTransient (429/网络超时/连接重置)
//	  │   重试策略: MaxRetries + MaxTransient 额度
//	  │   ├── 有额度 → retries++ → 状态=Ready → 异步退避延迟 → 下一轮调度拾取
//	  │   │        退避: Full Jitter Exponential Backoff
//	  │   │          delay = base * 2^attempt, jitter = random(0, min(delay, maxDelay))
//	  │   │        瞬态错误的 base = TransientBase (通常更大, 15s vs 2s)
//	  │   │
//	  │   └── 额度耗尽 → 状态=Suspended → 不级联下游
//	  │              Suspended 是"暂停"而非"终止", 等 stallRecovery 唤醒
//	  │              设计原因: LLM 限流通常是暂时的, 挂起比直接失败更合理
//	  │
//	  ├─ ErrorPermanent (验证失败/代码质量不达标/业务逻辑错误)
//	  │   重试策略: 仅 MaxRetries 额度 (无额外瞬态重试)
//	  │   ├── 有额度 → retries++ → 状态=Ready → 立即重试 (无延迟)
//	  │   │        注意: Permanent 错误重试无退避, 因为重试不会改变结果
//	  │   │        依赖上层逻辑 (如 AdversarialRunner) 做反馈迭代
//	  │   │
//	  │   └── 额度耗尽 → 状态=Failed → cascadeFailure(所有下游 Cancelled)
//	  │              设计原因: 上游数据无效, 下游不可能产出正确结果
//	  │
//	  └─ ErrorFatal (API Key 无效/配额耗尽/账户停用)
//	      重试策略: 永不重试
//	      └── 状态=Failed → cascadeFailure(所有下游 Cancelled)
//	                 设计原因: 基础设施级问题, 重试只会浪费时间和资源
//
// 与 Coordinator 重试的区别:
//  - Coordinator: 团队级, 阶段间重试, 带指数退避 + 全抖动
//  - Engine: DAG 级, 任务内重试, 按错误类型分治
//  两者互补: Coordinator 处理宏观流程, Engine 处理微观执行
func (e *Engine) handleFailure(g *Graph, t *Task, err error) {
	// 修复 TOCTOU 竞态: 先读取重试次数, 再做决策, 然后原子递增
	t.mu.Lock()
	attempt := t.retries
	decision := e.config.RetryPolicy.ShouldRetry(err.Error(), attempt)

	if decision.ShouldRetry {
		t.retries++
		t.setStateInternal(TaskReady)
		t.err = err.Error()
		t.mu.Unlock()

		e.mu.Lock()
		e.totalRetries++
		e.mu.Unlock()

		e.hook.OnTaskRetry(t, attempt+1, decision.Delay)

		// 退避延迟: 通过 goroutine 异步等待, 不阻塞调度主循环
		if decision.Delay > 0 {
			go func() {
				time.Sleep(decision.Delay)
				t.mu.Lock()
				if t.state == TaskReady {
					// 任务仍在 Ready 状态, 将由下一次调度循环拾取
				}
				t.mu.Unlock()
			}()
		}
		return
	}

	// 重试耗尽, 按错误类型做终态处理
	kind := decision.Kind
	t.mu.Unlock()

	switch kind {
	case ErrorTransient:
		// 瞬态: 挂起但不级联, stallRecovery 可在限流解除后恢复
		t.mu.Lock()
		t.setStateInternal(TaskSuspended)
		t.err = err.Error()
		t.mu.Unlock()

		e.mu.Lock()
		e.suspended++
		e.mu.Unlock()

		e.hook.OnTaskSuspended(t, err)

	case ErrorFatal:
		// 致命: 立即失败并级联取消所有下游
		t.mu.Lock()
		t.setStateInternal(TaskFailed)
		t.err = err.Error()
		t.mu.Unlock()

		e.mu.Lock()
		e.failed++
		e.mu.Unlock()

		e.hook.OnTaskFailed(t, err)
		e.cascadeFailure(g, t.ID)

	default: // ErrorPermanent
		// 永久: 失败并级联
		t.mu.Lock()
		t.setStateInternal(TaskFailed)
		t.err = err.Error()
		t.mu.Unlock()

		e.mu.Lock()
		e.failed++
		e.lastProgress = time.Now()
		e.mu.Unlock()

		e.hook.OnTaskFailed(t, err)
		e.cascadeFailure(g, t.ID)
	}
}

// unblockDownstream 事件驱动的下游解锁。
//
// 与轮询方案的对比:
//
//  方案 A (轮询 -- 不推荐):
//    for {
//      for each Blocked task {
//        if all upstreams Completed { unblock }
//      }
//      sleep(1s)  // 浪费 CPU, 延迟 1s 才感知
//    }
//
//  方案 B (事件驱动 -- 本方案):
//    task Completed → 只检查该 task 的下游邻居
//    仅 O(downstream_count * upstream_count) 的局部检查
//    零延迟, 任务完成的同一 goroutine 中立即触发
//
//  为什么不用 Channel 通知?
//    每个任务建一个 done channel 会增加复杂度, 且下游可能依赖多个上游,
//    需要 select 多个 channel。直接遍历下游更简单高效。
//
//  示例: safety-screen 完成后:
//    downstream[safety-screen] = [academic-tutor, psychology-coach,
//                                 parenting-advisor, development-assessor]
//    对这 4 个任务逐个检查: 它们的上游只有 [safety-screen], 已全部完成
//    → 4 个任务同时从 Blocked 转为 Ready
func (e *Engine) unblockDownstream(g *Graph, completedID string) {
	g.mu.RLock()
	downstream := g.downstream[completedID]
	g.mu.RUnlock()

	for _, downID := range downstream {
		dt := g.Tasks[downID]
		if dt == nil {
			continue
		}
		dt.mu.Lock()
		if dt.state != TaskBlocked {
			dt.mu.Unlock()
			continue
		}
		// 检查所有上游是否都已完成
		allDone := true
		for _, upID := range dt.DependsOn {
			up := g.Tasks[upID]
			if up == nil || up.State() != TaskCompleted {
				allDone = false
				break
			}
		}
		if allDone {
			dt.setStateInternal(TaskReady)
			dt.mu.Unlock()
			e.hook.OnTaskReady(dt)
		} else {
			dt.mu.Unlock()
		}
	}
}

// cascadeFailure 递归级联失败: 将所有下游非终态任务标记为 Cancelled。
//
// 级联语义:
//   上游永久失败 → 下游任务缺少必要输入数据 → 不可能成功产出 → 直接取消
//
// 递归 vs 非递归:
//   A→B→C, 如果 A 失败:
//   递归: cancel(B), 然后从 B 继续 cancel(C) — 整条链都取消
//   非递归: 只 cancel(B), C 仍保持 Blocked — C 永远不会被解锁但会占用资源
//
// 为什么不用 ctx.Cancel() ?
//   ctx.Cancel 会取消所有正在执行的任务, 包括其他并行分支中不相关的任务。
//   cascadeFailure 是精细的拓扑级联, 只影响失败任务的直接和间接下游。
//
// 示例: parenting 工作流中, 如果 safety-screen 失败 (Fatal 错误):
//   cascadeFailure(safety-screen) →
//     cancel(academic-tutor) → cascade(academic-tutor) → cancel(action-plan) → cascade(action-plan) → cancel(consultation-report)
//     cancel(psychology-coach) → cascade(...) → 同上
//     cancel(parenting-advisor) → cascade(...) → 同上
//     cancel(development-assessor) → cascade(...) → 同上
//   注意: action-plan 会被多次尝试取消, 但 IsTerminal 检查避免重复计数
func (e *Engine) cascadeFailure(g *Graph, failedID string) {
	g.mu.RLock()
	downstream := g.downstream[failedID]
	g.mu.RUnlock()

	for _, downID := range downstream {
		dt := g.Tasks[downID]
		if dt == nil {
			continue
		}
		dt.mu.Lock()
		if dt.state.IsTerminal() {
			dt.mu.Unlock()
			continue
		}
		dt.setStateInternal(TaskCancelled)
		dt.err = fmt.Sprintf("上游任务 %s 失败, 级联取消", failedID)
		dt.mu.Unlock()

		e.mu.Lock()
		e.cancelled++
		e.mu.Unlock()

		e.cascadeFailure(g, downID)
	}
}

// checkStall 停滞检测: 定期检查是否有任务卡住。
//
// 触发条件: 距上次有进展 > StallTimeout
// 恢复策略: 重置 Suspended 任务为 Ready, 强制重置超时的 Running 任务
func (e *Engine) checkStall(g *Graph) {
	e.mu.Lock()
	elapsed := time.Since(e.lastProgress)
	recovers := e.stallRecovers
	e.mu.Unlock()

	if elapsed < e.config.StallTimeout {
		return
	}
	if recovers >= e.config.MaxStallRecover {
		return
	}

	// 收集卡住的任务
	var stuck []*Task
	for _, t := range g.Tasks {
		t.mu.RLock()
		s := t.state
		t.mu.RUnlock()
		if s == TaskRunning || s == TaskSuspended {
			stuck = append(stuck, t)
		}
	}

	if len(stuck) == 0 {
		return
	}

	e.hook.OnStallDetected(g, stuck)
	recovered := e.attemptStallRecovery(g)

	e.mu.Lock()
	e.stallRecovers++
	if recovered > 0 {
		e.lastProgress = time.Now()
	}
	e.mu.Unlock()
}

// attemptStallRecovery 执行停滞恢复。
//
// 恢复逻辑:
//   - Suspended (瞬态失败挂起): 重置为 Ready, 清空重试计数器, 给予全新的重试额度
//   - Running (卡在执行中): 如果超过 2 倍默认超时, 强制重置为 Ready
func (e *Engine) attemptStallRecovery(g *Graph) int {
	recovered := 0
	for _, t := range g.Tasks {
		t.mu.Lock()
		switch t.state {
		case TaskSuspended:
			t.setStateInternal(TaskReady)
			t.retries = 0
			t.transRetries = 0
			t.err = ""
			recovered++

			e.mu.Lock()
			e.suspended--
			e.mu.Unlock()

		case TaskRunning:
			if time.Since(t.startedAt) > e.config.DefaultTimeout*2 {
				t.setStateInternal(TaskReady)
				t.retries++
				t.err = "停滞恢复: 从卡死的 Running 状态重置"
				recovered++
			}
		}
		t.mu.Unlock()
	}
	return recovered
}

// cancelRemaining 上下文取消时, 将所有非终态任务标记为 Cancelled。
// 修复: 使用 g.mu.RLock() 保护 map 遍历, 防止并发修改导致 panic
func (e *Engine) cancelRemaining(g *Graph) {
	g.mu.RLock()
	taskIDs := make([]string, 0, len(g.Tasks))
	for id := range g.Tasks {
		taskIDs = append(taskIDs, id)
	}
	g.mu.RUnlock()

	for _, id := range taskIDs {
		t := g.Tasks[id]
		if t == nil {
			continue
		}
		t.mu.Lock()
		if !t.state.IsTerminal() && t.state != TaskCompleted {
			t.setStateInternal(TaskCancelled)
			t.err = "上下文已取消"
			e.mu.Lock()
			e.cancelled++
			e.mu.Unlock()
		}
		t.mu.Unlock()
	}
}

// isComplete 判断所有任务是否都已到达终态。
func (e *Engine) isComplete(total int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.completed+e.failed+e.cancelled >= total
}

// runningCount 统计当前正在执行的任务数。
func (e *Engine) runningCount(g *Graph) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	count := 0
	for _, t := range g.Tasks {
		if t.State() == TaskRunning {
			count++
		}
	}
	return count
}

// runningIDs 返回正在执行的任务 ID 集合 (供调度器上下文使用)。
func (e *Engine) runningIDs(g *Graph) map[string]bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	ids := make(map[string]bool)
	for _, t := range g.Tasks {
		if t.State() == TaskRunning {
			ids[t.ID] = true
		}
	}
	return ids
}

// maybeCheckpoint 在每完成 N 个任务时触发检查点保存。
func (e *Engine) maybeCheckpoint(g *Graph) {
	if e.config.CheckpointEvery <= 0 {
		return
	}
	e.mu.Lock()
	c := e.completed
	e.mu.Unlock()
	if c%e.config.CheckpointEvery == 0 {
		e.saveCheckpoint(g)
	}
}

// saveCheckpoint 保存完整的执行状态快照。
// 包含: 所有任务状态 + 执行指标。
// 黑板数据通过黑板自身的持久化机制单独保存。
func (e *Engine) saveCheckpoint(g *Graph) {
	state := &ExecutionState{
		ExecID:    e.execID,
		GraphID:   g.ID,
		GraphName: g.Name,
		Tasks:     make(map[string]TaskSnapshot),
		Metrics:   e.buildMetrics(g.TaskCount()),
	}
	for id, t := range g.Tasks {
		t.mu.RLock()
		state.Tasks[id] = TaskSnapshot{
			ID:          t.ID,
			State:       t.state,
			Retries:     t.retries,
			Error:       t.err,
			Output:      t.output,
			StartedAt:   t.startedAt,
			CompletedAt: t.completedAt,
		}
		t.mu.RUnlock()
	}

	if err := e.checkpoint.Save(e.execID, state); err == nil {
		e.hook.OnCheckpoint(e.execID, state)
	}
}

// buildMetrics 构建执行指标摘要。
func (e *Engine) buildMetrics(total int) ExecutionMetrics {
	e.mu.Lock()
	defer e.mu.Unlock()
	return ExecutionMetrics{
		TotalTasks:     total,
		CompletedTasks: e.completed,
		FailedTasks:    e.failed,
		CancelledTasks: e.cancelled,
		SuspendedTasks: e.suspended,
		TotalRetries:   e.totalRetries,
		Elapsed:        time.Since(e.startTime),
	}
}

// buildResult 构建最终执行结果。
func (e *Engine) buildResult(g *Graph) *ExecutionResult {
	outputs := make(map[string]any)
	errors := make(map[string]string)
	for id, t := range g.Tasks {
		t.mu.RLock()
		if t.output != nil {
			outputs[id] = t.output
		}
		if t.err != "" {
			errors[id] = t.err
		}
		t.mu.RUnlock()
	}
	return &ExecutionResult{
		ExecID:  e.execID,
		Success: e.failed == 0 && e.cancelled == 0,
		Metrics: e.buildMetrics(g.TaskCount()),
		Outputs: outputs,
		Errors:  errors,
	}
}

// ExecutionContext 为条件边和动态扩展器提供执行上下文。
type ExecutionContext struct {
	Graph      *Graph
	Blackboard ReadOnlyBlackboard
	TaskID     string
}
