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
// 主循环逻辑:
//  1. 构建图 (环路检测 + 初始化任务状态)
//  2. 进入主循环:
//     a. 调度器选择就绪任务 (Filter → Score → Dispatch)
//     b. 派发 goroutine 执行 (受背压控制)
//     c. select 等待: 任务完成信号 / 停滞检测 / 上下文取消
//     d. 处理完成: 成功→解锁下游 / 失败→重试或级联
//  3. 所有任务到达终态后退出, 返回执行结果
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

	// 自动适配并发度: 不超过 DAG 最大宽度
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

	// 停滞检测定时器
	stallTicker := time.NewTicker(30 * time.Second)
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

	log.Printf("[engine] task %s: 获取背压许可...", t.ID)

	// 获取背压许可 (L1: RPM + L2: 并发)
	if err := e.bp.AcquireAll(taskCtx); err != nil {
		log.Printf("[engine] task %s: 背压获取失败: %v", t.ID, err)
		doneCh <- taskDone{taskID: t.ID, err: err}
		return
	}

	log.Printf("[engine] task %s: 背压获取成功, 查找 runner=%s", t.ID, t.Runner)

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
// 三级错误分治策略 (借鉴 TCP 拥塞控制思路):
//
//	错误 → 分类器
//	  ├─ 瞬态 (429/网络/超时)
//	  │   ├─ 还有重试额度? → 设为 Ready + 指数退避延迟 → 下一轮重新调度
//	  │   └─ 重试耗尽? → 设为 Suspended (不级联, 等 stallRecovery 恢复)
//	  │
//	  ├─ 永久 (验证/代码质量)
//	  │   ├─ 还有重试额度? → 设为 Ready → 立即重试
//	  │   └─ 重试耗尽? → 设为 Failed + 级联下游 Cancelled
//	  │
//	  └─ 致命 (API Key 无效/配额耗尽)
//	      └─ 设为 Failed + 级联所有下游
func (e *Engine) handleFailure(g *Graph, t *Task, err error) {
	t.mu.Lock()
	attempt := t.retries
	t.mu.Unlock()

	decision := e.config.RetryPolicy.ShouldRetry(err.Error(), attempt)

	if decision.ShouldRetry {
		t.mu.Lock()
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
	switch decision.Kind {
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
// 当任务完成时, 遍历其所有下游任务:
//   - 如果下游处于 Blocked 状态, 且其所有上游依赖都已 Completed
//   - 则将下游状态转为 Ready, 使其可被下一轮调度拾取
//
// 这是引擎的核心性能优化: 避免轮询检查依赖状态。
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
// 设计原因: 上游永久失败后, 下游任务不可能成功 (依赖数据缺失),
// 继续调度只会浪费 LLM 调用。直接级联标记避免无意义消耗。
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
func (e *Engine) cancelRemaining(g *Graph) {
	for _, t := range g.Tasks {
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
