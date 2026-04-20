package orchestrator

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Observer 可观测性接口, 用于指标收集和事件追踪。
type Observer interface {
	EmitMetric(name string, value float64, labels map[string]string)
	EmitEvent(event ObserverEvent)
}

// ObserverEvent 可观测事件。
type ObserverEvent struct {
	Time    time.Time
	Level   string // "info", "warn", "error"
	Message string
	Labels  map[string]string
}

// MetricsHook 是一个将引擎生命周期事件转换为可观测指标的 Hook 实现。
// 同时作为内置的日志收集器。
type MetricsHook struct {
	NoopHook
	mu       sync.Mutex
	observer Observer
	events   []ObserverEvent
}

// NewMetricsHook 创建指标 Hook。observer 为 nil 时仅在内存中收集事件。
func NewMetricsHook(observer Observer) *MetricsHook {
	return &MetricsHook{observer: observer}
}

func (h *MetricsHook) OnGraphStart(g *Graph) {
	h.emit("info", fmt.Sprintf("图 %s 开始执行, 共 %d 个任务", g.Name, g.TaskCount()),
		map[string]string{"graph_id": g.ID})
}

func (h *MetricsHook) OnTaskStart(t *Task) {
	if h.observer != nil {
		h.observer.EmitMetric("task.started", 1, map[string]string{
			"task_id": t.ID, "runner": t.Runner,
		})
	}
}

func (h *MetricsHook) OnTaskComplete(t *Task, _ any) {
	if h.observer != nil {
		t.mu.RLock()
		elapsed := t.completedAt.Sub(t.startedAt).Seconds()
		t.mu.RUnlock()
		h.observer.EmitMetric("task.completed", 1, map[string]string{"task_id": t.ID})
		h.observer.EmitMetric("task.duration_seconds", elapsed, map[string]string{"task_id": t.ID})
	}
}

func (h *MetricsHook) OnTaskFailed(t *Task, err error) {
	h.emit("error", fmt.Sprintf("任务 %s 失败: %s", t.ID, err),
		map[string]string{"task_id": t.ID})
	if h.observer != nil {
		h.observer.EmitMetric("task.failed", 1, map[string]string{"task_id": t.ID})
	}
}

func (h *MetricsHook) OnTaskRetry(t *Task, attempt int, _ interface{}) {
	h.emit("warn", fmt.Sprintf("任务 %s 重试第 %d 次", t.ID, attempt),
		map[string]string{"task_id": t.ID})
	if h.observer != nil {
		h.observer.EmitMetric("task.retries", float64(attempt), map[string]string{"task_id": t.ID})
	}
}

func (h *MetricsHook) OnStallDetected(g *Graph, stuck []*Task) {
	ids := make([]string, len(stuck))
	for i, t := range stuck {
		ids[i] = t.ID
	}
	h.emit("warn", fmt.Sprintf("检测到停滞, %d 个任务卡住: %s", len(stuck), strings.Join(ids, ",")),
		map[string]string{"graph_id": g.ID})
}

func (h *MetricsHook) OnGraphComplete(g *Graph, m ExecutionMetrics) {
	h.emit("info", fmt.Sprintf("图 %s 执行完毕: %d 完成, %d 失败, %d 取消, 耗时 %s",
		g.Name, m.CompletedTasks, m.FailedTasks, m.CancelledTasks, m.Elapsed),
		map[string]string{"graph_id": g.ID})
}

func (h *MetricsHook) emit(level, msg string, labels map[string]string) {
	evt := ObserverEvent{
		Time:    time.Now(),
		Level:   level,
		Message: msg,
		Labels:  labels,
	}
	h.mu.Lock()
	h.events = append(h.events, evt)
	h.mu.Unlock()
	if h.observer != nil {
		h.observer.EmitEvent(evt)
	}
}

// Events 返回收集的所有事件 (线程安全的副本)。
func (h *MetricsHook) Events() []ObserverEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]ObserverEvent, len(h.events))
	copy(cp, h.events)
	return cp
}

// ConflictDetector 并发冲突检测器。
//
// 当多个任务并行修改相同资源时, 用于声明和检测冲突。
// 引擎不强制使用此检测器, 由调用方按需注册为 FilterPlugin。
type ConflictDetector struct {
	mu      sync.RWMutex
	reads   map[string][]string // taskID → 读取的资源列表
	writes  map[string][]string // taskID → 写入的资源列表
}

// NewConflictDetector 创建冲突检测器。
func NewConflictDetector() *ConflictDetector {
	return &ConflictDetector{
		reads:  make(map[string][]string),
		writes: make(map[string][]string),
	}
}

// DeclareAccess 声明任务要访问的资源。
func (d *ConflictDetector) DeclareAccess(taskID string, reads, writes []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads[taskID] = reads
	d.writes[taskID] = writes
}

// CanParallel 检查两个任务是否可以安全地并行执行。
// 规则: 写-写冲突 或 读-写冲突 → 不可并行。
func (d *ConflictDetector) CanParallel(taskA, taskB string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()

	writesA := d.writes[taskA]
	writesB := d.writes[taskB]
	readsA := d.reads[taskA]
	readsB := d.reads[taskB]

	// 写-写冲突
	if hasOverlap(writesA, writesB) {
		return false
	}
	// 读-写冲突 (双向)
	if hasOverlap(readsA, writesB) || hasOverlap(readsB, writesA) {
		return false
	}
	return true
}

func hasOverlap(a, b []string) bool {
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if set[s] {
			return true
		}
	}
	return false
}

// ConflictFilter 将冲突检测器包装为调度器的 FilterPlugin。
// 当一个任务的写入资源与正在运行的任务有冲突时, 该任务不会被调度。
type ConflictFilter struct {
	detector *ConflictDetector
}

func NewConflictFilter(d *ConflictDetector) *ConflictFilter {
	return &ConflictFilter{detector: d}
}

func (f *ConflictFilter) Name() string { return "conflict" }
func (f *ConflictFilter) Filter(task *Task, ctx *SchedulerContext) bool {
	for runningID := range ctx.RunningIDs {
		if !f.detector.CanParallel(task.ID, runningID) {
			return false
		}
	}
	return true
}

// DynamicExpander 动态 DAG 扩展器接口。
//
// 当一个任务完成后, 扩展器可以根据其输出动态添加新任务和新边。
// 借鉴 DynTaskMAS 的运行时任务图扩展思路。
//
// 用途: LLM 规划出子任务后, 将子任务动态注入到图中。
type DynamicExpander interface {
	OnTaskComplete(task *Task, output any) (newTasks []*Task, newEdges []Edge)
}

// ExpanderHook 将 DynamicExpander 包装为 LifecycleHook。
type ExpanderHook struct {
	NoopHook
	expander DynamicExpander
	graph    *Graph
}

func NewExpanderHook(expander DynamicExpander, graph *Graph) *ExpanderHook {
	return &ExpanderHook{expander: expander, graph: graph}
}

func (h *ExpanderHook) OnTaskComplete(t *Task, output any) {
	newTasks, newEdges := h.expander.OnTaskComplete(t, output)
	for _, nt := range newTasks {
		_ = h.graph.AddTask(nt)
	}
	for _, ne := range newEdges {
		_ = h.graph.AddEdge(ne)
	}
	if len(newTasks) > 0 || len(newEdges) > 0 {
		_ = h.graph.Build()
	}
}
