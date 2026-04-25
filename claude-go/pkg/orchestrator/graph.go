package orchestrator

import (
	"fmt"
	"sync"
	"time"
)

// TaskState 表示任务的生命周期状态。
//
// 状态转换图:
//
//	              ┌──────────────────────────────────┐
//	              │                                   │
//	  ┌───────┐  │  ┌─────────┐   ┌─────────┐       │
//	  │Pending│──┴─▶│ Ready   │──▶│Running │───┐     │
//	  └───┬───┘     └─────────┘   └────┬────┘   │     │
//	      │                             │        │     │
//	      │  ┌─────────┐               │   ┌────▼───┐ │
//	      └─▶│Blocked  │               │   │Completed│ │
//	         └────┬────┘               │   └─────────┘ │
//	              │                    │                │
//	              │               ┌────▼────┐          │
//	              │               │  Failed │          │
//	              │               └────┬────┘          │
//	              │                    │                │
//	              │               ┌────▼─────┐         │
//	              └───────────────│Suspended │─────────┘
//	                              │(瞬态恢复) │
//	                              └──────────┘
type TaskState int

const (
	TaskPending   TaskState = iota // 等待依赖解析
	TaskBlocked                    // 上游依赖未满足
	TaskReady                      // 依赖已满足, 等待调度
	TaskRunning                    // 正在执行
	TaskCompleted                  // 执行成功
	TaskFailed                     // 永久失败 (重试耗尽)
	TaskCancelled                  // 被取消 (用户取消 或 上游级联失败)
	TaskSuspended                  // 瞬态失败挂起, 等待 stall recovery 恢复
)

func (s TaskState) String() string {
	switch s {
	case TaskPending:
		return "pending"
	case TaskBlocked:
		return "blocked"
	case TaskReady:
		return "ready"
	case TaskRunning:
		return "running"
	case TaskCompleted:
		return "completed"
	case TaskFailed:
		return "failed"
	case TaskCancelled:
		return "cancelled"
	case TaskSuspended:
		return "suspended"
	default:
		return "unknown"
	}
}

// IsTerminal 判断状态是否为终态 (completed/failed/cancelled)。
// 终态的任务不会再被调度器处理。
func (s TaskState) IsTerminal() bool {
	return s == TaskCompleted || s == TaskFailed || s == TaskCancelled
}

// EdgeKind 定义图边的语义类型。
type EdgeKind int

const (
	EdgeDependency  EdgeKind = iota // 标准数据/顺序依赖
	EdgeConditional                 // 条件边: 仅当条件满足时才激活
)

// Edge 表示 DAG 中两个任务之间的有向连接。
type Edge struct {
	From      string                          // 源任务 ID
	To        string                          // 目标任务 ID
	Kind      EdgeKind                        // 边的语义类型
	Condition func(ctx *ExecutionContext) bool // 条件边的判断函数; 无条件时为 nil
}

// Task 是引擎中最小的可调度单元。
//
// 设计理念: 借鉴 K8s Pod 的资源声明式设计,
// 每个 Task 声明自己的约束 (超时/重试/标签), 由调度器统一裁决。
type Task struct {
	ID           string            // 全局唯一标识
	Name         string            // 人类可读名称
	DependsOn    []string          // 上游依赖任务 ID 列表
	Priority     int               // 优先级 (越高越先调度)
	Timeout      time.Duration     // 单次执行超时; 0 表示使用引擎默认值
	MaxRetries   int               // 永久错误最大重试次数
	MaxTransient int               // 瞬态错误 (429/网络) 额外重试次数
	Labels       map[string]string // 元数据标签 (供 Filter/Score 插件使用)
	Runner       string            // 执行器名称 (在 RunnerRegistry 中注册)
	Input        any               // 不透明输入数据
	Config       map[string]any    // 执行器专用配置

	// ---- 以下为运行时状态, 由调度器独占管理 ----
	mu           sync.RWMutex
	state        TaskState
	retries      int   // 已使用的重试次数
	transRetries int   // 已使用的瞬态重试次数
	err          string
	output       any
	startedAt    time.Time
	completedAt  time.Time
	version      int64 // 单调递增版本号, 用于乐观并发控制 (CAS)
}

// State 线程安全地获取任务当前状态。
func (t *Task) State() TaskState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state
}

// Output 线程安全地获取任务输出。
func (t *Task) Output() any {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.output
}

// Error 线程安全地获取任务错误信息。
func (t *Task) Error() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.err
}

// Retries 线程安全地获取已重试次数。
func (t *Task) Retries() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.retries
}

// setStateInternal 内部状态转换 (调用者必须已持有 t.mu 写锁)。
// 每次转换递增版本号, 用于 checkpoint 的一致性校验。
func (t *Task) setStateInternal(s TaskState) {
	t.state = s
	t.version++
}

// Graph 是一个带类型边的有向无环图 (DAG)。
//
// 核心数据结构设计:
//   - Tasks: map 存储, O(1) 按 ID 查找
//   - downstream/upstream: 预计算邻接表, Build() 时一次构建
//   - 使用 Kahn 算法检测环路, 同时确定拓扑序
type Graph struct {
	ID    string
	Name  string
	Tasks map[string]*Task
	Edges []Edge

	// 预计算的邻接表, Build() 后生效, O(1) 查询
	mu         sync.RWMutex
	downstream map[string][]string // taskID → 下游任务 ID 列表
	upstream   map[string][]string // taskID → 上游任务 ID 列表
	built      bool
}

// NewGraph 创建一个新的空图。
func NewGraph(id, name string) *Graph {
	return &Graph{
		ID:         id,
		Name:       name,
		Tasks:      make(map[string]*Task),
		downstream: make(map[string][]string),
		upstream:   make(map[string][]string),
	}
}

// AddTask 注册一个任务到图中。任务 ID 必须唯一。
func (g *Graph) AddTask(t *Task) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.Tasks[t.ID]; exists {
		return fmt.Errorf("重复的任务 ID: %s", t.ID)
	}
	g.Tasks[t.ID] = t
	g.built = false
	return nil
}

// AddEdge 添加一条有向边。所有边添加完毕后需调用 Build() 生效。
func (g *Graph) AddEdge(e Edge) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.Tasks[e.From]; !ok {
		return fmt.Errorf("边的源任务不存在: %s", e.From)
	}
	if _, ok := g.Tasks[e.To]; !ok {
		return fmt.Errorf("边的目标任务不存在: %s", e.To)
	}
	g.Edges = append(g.Edges, e)
	g.built = false
	return nil
}

// Build 预计算邻接表并验证 DAG 合法性。
//
// 算法: Kahn 拓扑排序 (BFS 版本)
//
// 逐步推演示例 (parenting 工作流简化版):
//
//	图结构:
//	  intake → safety-screen → academic-tutor
//	                           → psychology-coach
//	                           → parenting-advisor
//	                           → development-assessor
//	                                    ↓
//	                             action-plan → consultation-report
//
//	Step 1: 统计入度
//	  intake: 0        ← 无依赖
//	  safety-screen: 1  ← [intake]
//	  academic-tutor: 1 ← [safety-screen]
//	  psychology-coach: 1
//	  parenting-advisor: 1
//	  development-assessor: 1
//	  action-plan: 4    ← [academic-tutor, psychology-coach, parenting-advisor, development-assessor]
//	  consultation-report: 1 ← [action-plan]
//
//	Step 2: 入度为 0 的入队 → queue = [intake]
//
//	Step 3: 处理队列
//	  出队 intake, visited=1
//	  intake 的下游: safety-screen, inDeg[safety-screen]-- → 0 → 入队
//	  出队 safety-screen, visited=2
//	  下游 4 个各减 1, 都不为 0 (都是 0? 等等, safety-screen 的下游入度都是 1, 减到 0)
//	  → queue = [academic-tutor, psychology-coach, parenting-advisor, development-assessor]
//	  依次出队 4 个, visited=6
//	  每个的下游都是 action-plan, action-plan 入度从 4 减到 0 → 入队
//	  出队 action-plan, visited=7, consultation-report 入度从 1 减到 0 → 入队
//	  出队 consultation-report, visited=8
//
//	Step 4: visited(8) == TaskCount(8) → 无环路 ✓
//
//	Step 5: 初始化状态
//	  intake → Ready (无上游)
//	  其余 → Blocked (有上游依赖)
//	  同时填充每个 Task 的 DependsOn 字段
//
// 环路检测: 如果 visited != total, 说明有环
//   例: A→B→A, queue=[A], 出队A→B入度减到0入队, 出队B→A但A已被处理, 队列空
//   visited=2, 但总任务=2... 等等, 这种情况需要仔细处理:
//   实际上 A 被处理后 B 入队, B 处理后 A 的入度再减, 但 A 已经 visited 了
//   问题在于有环时环内节点的入度永远不会减到 0 (除了第一个)
//   所以 visited 必然 < total
//
// 时间复杂度: O(V + E), 每个节点和边各访问常数次
/**
 * Build - 预计算邻接表 + 环路检测 + 初始化任务状态 (Build 后 DAG 才能被 Engine.Run 使用)
 *
 * Kahn 算法原理:
 *   1. 计算每个节点的入度（有多少条边指向它）
 *   2. 入度为 0 的节点入队（没有依赖，可以直接执行）
 *   3. 出队一个节点，将其所有下游节点的入度减 1；如果下游节点入度变为 0，入队
 *   4. 重复步骤 3 直到队列为空
 *   5. 如果访问的节点数 != 总节点数 → 存在环路（环内节点的入度永远不会减到 0）
 *
 * 为什么要建邻接表 (downstream/upstream)?
 *   如果不建邻接表，每次检查下游依赖需要遍历所有边: O(E)
 *   建了邻接表后: O(1) 查找某个任务的直接下游/上游 → 大幅提升 unblockDownstream 和 cascadeFailure 的性能
 *
 * 时间复杂度: O(V + E)
 *   - 构建邻接表: O(E) 遍历所有边
 *   - 计算入度: O(E)
 *   - Kahn 排序: 每个节点出队一次，每条边访问一次 → O(V + E)
 *   - 初始化状态: O(V) 遍历所有任务 + O(V + E) 填充 DependsOn
 * 总: O(V + E)
 */
func (g *Graph) Build() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.downstream = make(map[string][]string)
	g.upstream = make(map[string][]string)

	for _, e := range g.Edges {
		g.downstream[e.From] = append(g.downstream[e.From], e.To)
		g.upstream[e.To] = append(g.upstream[e.To], e.From)
	}

	// ---- Kahn 拓扑排序: 环路检测 ----
	inDeg := make(map[string]int)
	for id := range g.Tasks {
		inDeg[id] = 0
	}
	for _, e := range g.Edges {
		if e.Kind == EdgeDependency {
			inDeg[e.To]++
		}
	}

	queue := make([]string, 0)
	for id, deg := range inDeg {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	visited := 0
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range g.downstream[node] {
			inDeg[next]--
			if inDeg[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if visited != len(g.Tasks) {
		return fmt.Errorf("图中存在环路: 已访问 %d / 共 %d 个任务", visited, len(g.Tasks))
	}

	// 初始化任务状态: 无上游 → Ready, 有上游 → Blocked (依赖满足前不能执行)
	for id, t := range g.Tasks {
		deps := g.upstream[id]
		if len(deps) == 0 {
			t.state = TaskReady
		} else {
			t.state = TaskBlocked
			t.DependsOn = deps
		}
	}

	g.built = true
	return nil
}

// Downstream 获取指定任务的直接下游任务 ID 列表。
func (g *Graph) Downstream(taskID string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.downstream[taskID]
}

// Upstream 获取指定任务所依赖的上游任务 ID 列表。
func (g *Graph) Upstream(taskID string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.upstream[taskID]
}

// ReadyTasks 返回所有处于 Ready 状态的任务。
func (g *Graph) ReadyTasks() []*Task {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var ready []*Task
	for _, t := range g.Tasks {
		t.mu.RLock()
		if t.state == TaskReady {
			ready = append(ready, t)
		}
		t.mu.RUnlock()
	}
	return ready
}

// CriticalPath 使用 DAG 最长路径算法计算关键路径。
//
// 算法: 基于拓扑序的动态规划 (最长路径 = 关键路径)
//
// 核心思想:
//   在 DAG 上, 最长路径 (关键路径) 决定了整体完成时间的下限。
//   因为无论怎么并行, 关键路径上的任务必须依次执行。
//
// 逐步推演示例:
//
//	图: A→B→C→D (链) 和 A→E→D (短路)
//
//	Step 1: Kahn 拓扑序: A, B, E, C, D
//
//	Step 2: 松弛操作 (relaxation)
//	  dist[A] = 1 (入度为 0, 初始化为 1)
//	  处理 A: 下游 B → dist[B] = max(0, 1+1) = 2, pred[B] = A
//	          下游 E → dist[E] = max(0, 1+1) = 2, pred[E] = A
//	  处理 B: 下游 C → dist[C] = max(0, 2+1) = 3, pred[C] = B
//	  处理 E: 下游 D → dist[D] = max(0, 2+1) = 3, pred[D] = E
//	  处理 C: 下游 D → dist[D] = max(3, 3+1) = 4, pred[D] = C  ← 更长, 更新前驱
//
//	Step 3: 找最长: dist[D] = 4 最大, 从 D 回溯:
//	  D ← pred[D]=C ← pred[C]=B ← pred[B]=A
//	  关键路径: [A, B, C, D] (长度 4)
//
//	为什么不是 A→E→D? 因为 A→E→D 长度只有 3, 不是最长。
//
// 调度优化: CriticalPathScore 给关键路径上的任务 +500 分,
// 使其优先被调度, 从而最小化整体完成时间。
func (g *Graph) CriticalPath() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	dist := make(map[string]int)
	pred := make(map[string]string)

	// Kahn 拓扑排序
	inDeg := make(map[string]int)
	for id := range g.Tasks {
		inDeg[id] = 0
	}
	for _, e := range g.Edges {
		if e.Kind == EdgeDependency {
			inDeg[e.To]++
		}
	}
	queue := make([]string, 0)
	for id, d := range inDeg {
		if d == 0 {
			queue = append(queue, id)
			dist[id] = 1
		}
	}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range g.downstream[node] {
			// 松弛操作: 如果通过 node 到 next 的路径更长, 则更新
			if dist[node]+1 > dist[next] {
				dist[next] = dist[node] + 1
				pred[next] = node
			}
			inDeg[next]--
			if inDeg[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	// 找最长路径终点, 沿前驱回溯
	maxDist, maxNode := 0, ""
	for id, d := range dist {
		if d > maxDist {
			maxDist = d
			maxNode = id
		}
	}

	var path []string
	for n := maxNode; n != ""; n = pred[n] {
		path = append([]string{n}, path...)
	}
	return path
}

// DAGWidth 使用 Kahn 分层法计算 DAG 最大并行宽度。
//
// 算法: BFS 逐层推进, 每层的节点数 = 该层的理论并行度。
//
// 逐步推演示例:
//
//	图: intake → safety-screen → [academic, psych, parent, dev]
//	                                        ↓
//	                                 action-plan → report
//
//	Layer 0: [intake]               → 宽度 1
//	Layer 1: [safety-screen]        → 宽度 1
//	Layer 2: [academic, psych, parent, dev] → 宽度 4 ← 最大!
//	Layer 3: [action-plan]          → 宽度 1
//	Layer 4: [report]               → 宽度 1
//
//	返回: 4
//
// 为什么需要分层 (level-order BFS)?
//   标准 Kahn 的队列在某一时刻可能包含不同层的节点。
//   分层法: 每次处理完当前层的所有节点后, 再把下一层节点统一入队,
//   这样才能准确统计每层的并行度。
//
// 用途: Engine.Run() 用此值自动设置 maxParallel:
//   maxPar = min(config.MaxParallel, g.DAGWidth())
//   避免配置 MaxParallel=8 但 DAG 最大宽度只有 4 的浪费。
func (g *Graph) DAGWidth() int {
	g.mu.RLock()
	defer g.mu.RUnlock()

	inDeg := make(map[string]int)
	for id := range g.Tasks {
		inDeg[id] = 0
	}
	for _, e := range g.Edges {
		if e.Kind == EdgeDependency {
			inDeg[e.To]++
		}
	}

	queue := make([]string, 0)
	for id, d := range inDeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}

	maxWidth := 0
	for len(queue) > 0 {
		if len(queue) > maxWidth {
			maxWidth = len(queue)
		}
		nextQueue := make([]string, 0)
		for _, node := range queue {
			for _, next := range g.downstream[node] {
				inDeg[next]--
				if inDeg[next] == 0 {
					nextQueue = append(nextQueue, next)
				}
			}
		}
		queue = nextQueue
	}
	return maxWidth
}

// TaskCount 返回图中任务总数。
func (g *Graph) TaskCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.Tasks)
}

// Stats 返回各状态的任务计数摘要。
func (g *Graph) Stats() map[TaskState]int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	stats := make(map[TaskState]int)
	for _, t := range g.Tasks {
		t.mu.RLock()
		stats[t.state]++
		t.mu.RUnlock()
	}
	return stats
}
