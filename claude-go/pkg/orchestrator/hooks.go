package orchestrator

// LifecycleHook 允许外部代码观察和响应引擎事件。
//
// 所有方法同步调用, 实现者不应阻塞。
// 用途: 监控、通知 (飞书/Slack)、指标上报、自定义恢复策略等。
//
// Hook 触发时机:
//
//	OnGraphStart     — 图开始执行
//	OnTaskReady      — 任务依赖满足, 变为可调度
//	OnTaskStart      — 任务开始执行
//	OnTaskComplete   — 任务成功完成
//	OnTaskFailed     — 任务最终失败 (重试耗尽)
//	OnTaskRetry      — 任务即将重试
//	OnTaskSuspended  — 任务因瞬态错误挂起
//	OnGraphComplete  — 图执行结束 (无论成败)
//	OnStallDetected  — 检测到执行停滞
//	OnCheckpoint     — 检查点已保存
type LifecycleHook interface {
	OnGraphStart(g *Graph)
	OnTaskReady(t *Task)
	OnTaskStart(t *Task)
	OnTaskComplete(t *Task, output any)
	OnTaskFailed(t *Task, err error)
	OnTaskRetry(t *Task, attempt int, delay interface{})
	OnTaskSuspended(t *Task, err error)
	OnGraphComplete(g *Graph, metrics ExecutionMetrics)
	OnStallDetected(g *Graph, stuckTasks []*Task)
	OnCheckpoint(execID string, state *ExecutionState)
}

// NoopHook 默认空实现, 嵌入后可选择性覆盖需要的方法。
type NoopHook struct{}

func (NoopHook) OnGraphStart(*Graph)                      {}
func (NoopHook) OnTaskReady(*Task)                        {}
func (NoopHook) OnTaskStart(*Task)                        {}
func (NoopHook) OnTaskComplete(*Task, any)                {}
func (NoopHook) OnTaskFailed(*Task, error)                {}
func (NoopHook) OnTaskRetry(*Task, int, interface{})      {}
func (NoopHook) OnTaskSuspended(*Task, error)             {}
func (NoopHook) OnGraphComplete(*Graph, ExecutionMetrics) {}
func (NoopHook) OnStallDetected(*Graph, []*Task)          {}
func (NoopHook) OnCheckpoint(string, *ExecutionState)     {}

// MultiHook 扇出 Hook: 将事件分发给多个 Hook 实现。
type MultiHook struct {
	hooks []LifecycleHook
}

func NewMultiHook(hooks ...LifecycleHook) *MultiHook {
	return &MultiHook{hooks: hooks}
}

func (m *MultiHook) OnGraphStart(g *Graph) {
	for _, h := range m.hooks {
		h.OnGraphStart(g)
	}
}

func (m *MultiHook) OnTaskReady(t *Task) {
	for _, h := range m.hooks {
		h.OnTaskReady(t)
	}
}

func (m *MultiHook) OnTaskStart(t *Task) {
	for _, h := range m.hooks {
		h.OnTaskStart(t)
	}
}

func (m *MultiHook) OnTaskComplete(t *Task, output any) {
	for _, h := range m.hooks {
		h.OnTaskComplete(t, output)
	}
}

func (m *MultiHook) OnTaskFailed(t *Task, err error) {
	for _, h := range m.hooks {
		h.OnTaskFailed(t, err)
	}
}

func (m *MultiHook) OnTaskRetry(t *Task, attempt int, delay interface{}) {
	for _, h := range m.hooks {
		h.OnTaskRetry(t, attempt, delay)
	}
}

func (m *MultiHook) OnTaskSuspended(t *Task, err error) {
	for _, h := range m.hooks {
		h.OnTaskSuspended(t, err)
	}
}

func (m *MultiHook) OnGraphComplete(g *Graph, metrics ExecutionMetrics) {
	for _, h := range m.hooks {
		h.OnGraphComplete(g, metrics)
	}
}

func (m *MultiHook) OnStallDetected(g *Graph, stuckTasks []*Task) {
	for _, h := range m.hooks {
		h.OnStallDetected(g, stuckTasks)
	}
}

func (m *MultiHook) OnCheckpoint(execID string, state *ExecutionState) {
	for _, h := range m.hooks {
		h.OnCheckpoint(execID, state)
	}
}
