package orchestrator

import (
	"container/heap"
	"sort"
	"sync"
)

// FilterPlugin 硬约束过滤器: 任意一个 Filter 拒绝, 任务就不会被调度。
// 类比 K8s 的 NodeAffinity / PodFitsResources 等过滤器。
type FilterPlugin interface {
	Name() string
	Filter(task *Task, ctx *SchedulerContext) bool
}

// ScorePlugin 软偏好打分器: 分数越高, 调度优先级越高。
// 类比 K8s 的 LeastRequestedPriority / BalancedResourceAllocation。
type ScorePlugin interface {
	Name() string
	Score(task *Task, ctx *SchedulerContext) int
}

// SchedulerContext 为 Filter/Score 插件提供决策所需的全局信息。
type SchedulerContext struct {
	Graph      *Graph             // 当前工作流图
	Blackboard ReadOnlyBlackboard // 黑板只读视图
	BP         *BackpressureCtrl  // 背压控制器
	RunningIDs map[string]bool    // 当前正在执行的任务 ID 集合
}

// Scheduler 实现了借鉴 K8s 调度器的三阶段调度管线。
//
// 调度流程:
//
//	┌──────────┐    ┌──────────┐    ┌──────────┐
//	│ Phase 1  │    │ Phase 2  │    │ Phase 3  │
//	│ Filter   │───▶│  Score   │───▶│ Dispatch │
//	│ (硬约束)  │    │ (软偏好)  │    │ (取前N个) │
//	└──────────┘    └──────────┘    └──────────┘
//	  依赖检查          优先级            maxBatch
//	  资源检查       关键路径加分          截断
//	  冷却期检查      公平性惩罚
//
// 所有插件都可以动态添加/替换, 实现调度策略的热插拔。
type Scheduler struct {
	mu      sync.RWMutex
	filters []FilterPlugin
	scorers []ScorePlugin
}

// NewScheduler 创建带内置插件的调度器。
func NewScheduler() *Scheduler {
	return &Scheduler{
		filters: []FilterPlugin{
			&DependencyFilter{},
			&ResourceFilter{},
		},
		scorers: []ScorePlugin{
			&PriorityScore{},
			&CriticalPathScore{},
		},
	}
}

// AddFilter 注册自定义过滤器插件。
func (s *Scheduler) AddFilter(f FilterPlugin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filters = append(s.filters, f)
}

// AddScorer 注册自定义打分器插件。
func (s *Scheduler) AddScorer(sc ScorePlugin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scorers = append(s.scorers, sc)
}

// Schedule 执行三阶段调度, 返回按优先级排序的待执行任务批次。
//
// 具体打分计算示例 (parenting 工作流):
//
//	假设 4 个并行阶段都 Ready:
//	  academic-tutor (Priority=5, 在关键路径上)
//	  psychology-coach (Priority=5, 不在关键路径上)
//	  parenting-advisor (Priority=5, 不在关键路径上)
//	  development-assessor (Priority=5, 在关键路径上)
//
//	Phase 1 -- Filter:
//	  DependencyFilter: 4 个任务的上游 safety-screen 都 Completed → 全部通过
//	  ResourceFilter: 背压允许入队 → 全部通过
//	  feasible = [academic, psych, parent, dev]
//
//	Phase 2 -- Score:
//	  academic-tutor:
//	    PriorityScore    = 5 * 100 = 500
//	    CriticalPathScore = 500      (在关键路径上)
//	    FairnessScore     = -0 * 50 = 0    (无重试)
//	    Total = 1000
//
//	  psychology-coach:
//	    PriorityScore    = 5 * 100 = 500
//	    CriticalPathScore = 0         (不在关键路径上)
//	    FairnessScore     = -0 * 50 = 0
//	    Total = 500
//
//	  排序: academic(1000) > dev(1000) > psych(500) > parent(500)
//
//	Phase 3 -- Dispatch:
//	  如果 avail=2 (还有 2 个执行槽): 取 [academic, dev]
//	  如果 avail=4: 取全部 4 个
//
// 为什么关键路径任务优先?
//   关键路径是 DAG 中最长的依赖链, 决定了最短完成时间。
//   优先执行关键路径任务可以最小化该链的完成时间, 从而缩短整体耗时。
/**
 * Schedule - 三阶段调度管线 (借鉴 K8s 调度器设计)
 *
 * Phase 1: Filter (硬约束) - 不能妥协的条件, 任一不满足则排除该任务
 *   - DependencyFilter: 所有上游依赖必须已完成 (这是最基本的, 否则没有输入数据)
 *   - ResourceFilter: 背压队列还有空间 (否则塞满会 OOM)
 *   - 用户自定义 Filter: 比如冲突检测、标签过滤等
 *
 * Phase 2: Score (软偏好) - 可妥协的偏好, 分数越高越优先调度
 *   - PriorityScore = Priority × 100 (用户设定的优先级)
 *   - CriticalPathScore = 500 (如果在关键路径上) — 关键路径任务优先可缩短整体耗时
 *   - FairnessScore = -Retries × 50 (重试越多分越低, 防止饥饿)
 *   - 用户自定义 Score: 比如资源亲和性、反亲和性等
 *
 * Phase 3: Dispatch (截断) - 取前 maxBatch 个任务派发执行
 *
 * 打分示例 (4 个并行任务, maxBatch=2):
 *   academic-tutor (Priority=5, 关键路径) → 5×100 + 500 + 0 = 1000 ✓ 选中
 *   dev-assessor   (Priority=5, 关键路径) → 5×100 + 500 + 0 = 1000 ✓ 选中 (同分, 排序在前)
 *   psych-coach    (Priority=5, 非关键路径) → 5×100 + 0 + 0 = 500  ✗ 未选中 (超出批次)
 *   parent-advisor (Priority=5, 非关键路径) → 5×100 + 0 + 0 = 500  ✗ 未选中 (超出批次)
 *   下轮调度时再派 psych-coach 和 parent-advisor。
 *
 * 时间复杂度: O(V × F + V × S + V log V) = O(V log V)
 *   - Filter: V 个任务 × F 个过滤器 → O(V × F)
 *   - Score: V 个任务 × S 个打分器 → O(V × S)
 *   - Sort: V 个任务排序 → O(V log V)
 *   F 和 S 通常很小 (2-4), 所以实际为 O(V log V)
 */
func (s *Scheduler) Schedule(ctx *SchedulerContext, maxBatch int) []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ready := ctx.Graph.ReadyTasks()
	if len(ready) == 0 {
		return nil
	}

	// ---- Phase 1: 过滤 (硬约束) ----
	// 遍历所有就绪任务, 任意一个 Filter 返回 false 则排除该任务
	var feasible []*Task
	for _, t := range ready {
		pass := true
		for _, f := range s.filters {
			if !f.Filter(t, ctx) {
				pass = false
				break
			}
		}
		if pass {
			feasible = append(feasible, t)
		}
	}
	if len(feasible) == 0 {
		return nil
	}

	// ---- Phase 2: 打分 (软偏好) ----
	// 对每个可行任务, 累加所有 Scorer 的分数
	type scored struct {
		task  *Task
		score int
	}
	items := make([]scored, len(feasible))
	for i, t := range feasible {
		total := 0
		for _, sc := range s.scorers {
			total += sc.Score(t, ctx)
		}
		items[i] = scored{task: t, score: total}
	}
	// 按分数降序排序 (高分优先)
	sort.Slice(items, func(i, j int) bool {
		return items[i].score > items[j].score
	})

	// ---- Phase 3: 派发 (截断到 maxBatch) ----
	// 只取前 maxBatch 个, 其余留在 Ready 状态等下一轮调度
	n := len(items)
	if maxBatch > 0 && n > maxBatch {
		n = maxBatch
	}
	result := make([]*Task, n)
	for i := 0; i < n; i++ {
		result[i] = items[i].task
	}
	return result
}

// ---- 内置 Filter 插件 ----

// DependencyFilter 依赖过滤器: 确保所有上游任务已完成。
type DependencyFilter struct{}

func (f *DependencyFilter) Name() string { return "dependency" }
func (f *DependencyFilter) Filter(task *Task, ctx *SchedulerContext) bool {
	for _, depID := range task.DependsOn {
		dep, ok := ctx.Graph.Tasks[depID]
		if !ok || dep.State() != TaskCompleted {
			return false
		}
	}
	return true
}

// ResourceFilter 资源过滤器: 检查背压是否允许继续调度。
type ResourceFilter struct{}

func (f *ResourceFilter) Name() string { return "resource" }
func (f *ResourceFilter) Filter(_ *Task, ctx *SchedulerContext) bool {
	if ctx.BP == nil {
		return true
	}
	return ctx.BP.CanEnqueue()
}

// CooldownFilter 冷却期过滤器: 阻止近期瞬态失败的任务被立即重新调度。
type CooldownFilter struct {
	Cooldown map[string]int64 // taskID → 最早可调度的 Unix 时间戳
}

func (f *CooldownFilter) Name() string { return "cooldown" }
func (f *CooldownFilter) Filter(task *Task, ctx *SchedulerContext) bool {
	if f.Cooldown == nil {
		return true
	}
	_, has := f.Cooldown[task.ID]
	return !has
}

// ---- 内置 Score 插件 ----

// PriorityScore 优先级打分: 按 Task.Priority 加权, 值越高越优先调度。
type PriorityScore struct{}

func (s *PriorityScore) Name() string { return "priority" }
func (s *PriorityScore) Score(task *Task, _ *SchedulerContext) int {
	return task.Priority * 100
}

// CriticalPathScore 关键路径加分: DAG 关键路径上的任务额外 +500 分。
//
// 原理: 关键路径是 DAG 中最长的依赖链, 决定了整体执行时间的下限。
// 优先调度关键路径上的任务可以最大化缩短总执行时间。
//
// sync.Once 懒加载: 第一次 Score 调用时计算一次关键路径,
// 之后直接查 criticalSet map, O(1) 查找。
// 注意: 如果图被 DynamicExpander 修改过, 此处的 criticalSet 不会重新计算,
// 因为 sync.Once 只执行一次。对于 DAG 扩展场景, 建议传入新的 CriticalPathScore 实例。
type CriticalPathScore struct {
	criticalSet map[string]bool
	once        sync.Once
}

func (s *CriticalPathScore) Name() string { return "critical_path" }
func (s *CriticalPathScore) Score(task *Task, ctx *SchedulerContext) int {
	s.once.Do(func() {
		cp := ctx.Graph.CriticalPath()
		s.criticalSet = make(map[string]bool, len(cp))
		for _, id := range cp {
			s.criticalSet[id] = true
		}
	})
	if s.criticalSet[task.ID] {
		return 500
	}
	return 0
}

// FairnessScore 公平性打分: 借鉴 Linux CFS 的 vruntime 思想。
// 已消耗更多重试的任务被惩罚 (降低优先级), 防止饥饿。
type FairnessScore struct{}

func (s *FairnessScore) Name() string { return "fairness" }
func (s *FairnessScore) Score(task *Task, _ *SchedulerContext) int {
	return -task.Retries() * 50
}

// ---- 优先级队列 (用于未来的流式调度器) ----

type pqItem struct {
	task  *Task
	score int
	index int
}

type priorityQueue []*pqItem

func (pq priorityQueue) Len() int           { return len(pq) }
func (pq priorityQueue) Less(i, j int) bool { return pq[i].score > pq[j].score }
func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}
func (pq *priorityQueue) Push(x any) {
	n := len(*pq)
	item := x.(*pqItem)
	item.index = n
	*pq = append(*pq, item)
}
func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*pq = old[:n-1]
	return item
}

// PriorityTaskQueue 线程安全的优先级队列, 用于就绪任务的有序管理。
type PriorityTaskQueue struct {
	mu sync.Mutex
	pq priorityQueue
}

func NewPriorityTaskQueue() *PriorityTaskQueue {
	ptq := &PriorityTaskQueue{}
	heap.Init(&ptq.pq)
	return ptq
}

func (q *PriorityTaskQueue) Push(t *Task, score int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	heap.Push(&q.pq, &pqItem{task: t, score: score})
}

func (q *PriorityTaskQueue) Pop() *Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pq.Len() == 0 {
		return nil
	}
	item := heap.Pop(&q.pq).(*pqItem)
	return item.task
}

func (q *PriorityTaskQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pq.Len()
}
