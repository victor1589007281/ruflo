# 通用 Agent 编排与 Workflow 引擎设计方案

> **版本**: 2.0 | **日期**: 2026-04-20
> **定位**: 独立通用引擎, 可覆盖现有所有团队工作流, 未来替换现有编排层

---

## 一、调研总结与设计原则

### 1.1 调研来源

| 领域 | 参考系统 | 核心启发 |
|:----:|:--------:|:--------:|
| **Agent 编排** | LangGraph, CrewAI, AutoGen, MetaGPT, Temporal, Airflow | 图执行+检查点, 角色委派, 回合制对话, SOP 流水线 |
| **LLM 论文** | DynTaskMAS, AgentOrchestra, LATS, GoT, D3 Debate | 动态DAG, 分层TEA, 树搜索, 对抗验证 |
| **OS 调度** | Linux CFS/EEVDF, GMP work-stealing, io_uring | 公平调度, 工作窃取, 异步批处理 |
| **基础设施** | K8s scheduler, Nomad, Temporal, Asynq | Filter→Score→Bind, 抢占, 持久队列 |
| **设计模式** | Saga, Event Sourcing, Actor, CSP, Petri Net | 补偿事务, 事件溯源, 通道通信 |

### 1.2 设计原则

```
┌─────────────────────────────────────────────────────────────────┐
│  P1: 编排与执行分离 (Temporal: Workflow vs Activity)            │
│  P2: 统一调度内核 (K8s: 一个调度器服务所有工作负载)              │
│  P3: 事件驱动+检查点 (Event Sourcing + Checkpoint)             │
│  P4: 插件式策略 (K8s: Filter/Score 可插拔)                     │
│  P5: 背压传播 (io_uring: 队列深度控制)                          │
│  P6: 瞬态/永久故障分治 (区分限流与代码缺陷)                     │
│  P7: 零共享状态 (CSP: channel 通信, 非共享内存)                 │
└─────────────────────────────────────────────────────────────────┘
```

---

## 二、总体架构

```
┌──────────────────── Workflow Engine ────────────────────────┐
│                                                             │
│  ┌─────────────┐   ┌──────────────┐   ┌────────────────┐   │
│  │  Workflow    │   │   Scheduler  │   │   Blackboard   │   │
│  │  Definition  │──▶│   (核心调度)  │◀─▶│  (共享状态)     │   │
│  │  (YAML/Code) │   │              │   │                │   │
│  └─────────────┘   └──────┬───────┘   └────────────────┘   │
│                           │                                  │
│              ┌────────────┼────────────┐                    │
│              ▼            ▼            ▼                    │
│  ┌──────────────┐ ┌───────────┐ ┌──────────────┐          │
│  │  TaskRunner   │ │ Evaluator │ │  Checkpoint  │          │
│  │  (执行单元)   │ │ (质量门禁) │ │  Store       │          │
│  └──────────────┘ └───────────┘ └──────────────┘          │
│                                                             │
│  ┌──────────────────────────────────────────────────────┐  │
│  │              Extension Layer (Hooks/Plugins)           │  │
│  │  ┌─────────┐ ┌──────────┐ ┌──────────┐ ┌─────────┐  │  │
│  │  │LifeCycle│ │ Strategy │ │ Observer │ │ Backpres│  │  │
│  │  │ Hooks   │ │ Plugins  │ │ Metrics  │ │ sure    │  │  │
│  │  └─────────┘ └──────────┘ └──────────┘ └─────────┘  │  │
│  └──────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

---

## 三、核心抽象

### 3.1 Task (任务 — 最小调度单元)

```go
type TaskState int
const (
    TaskPending    TaskState = iota // 等待调度
    TaskBlocked                     // 依赖未满足
    TaskReady                       // 依赖满足, 待调度
    TaskRunning                     // 执行中
    TaskCompleted                   // 成功
    TaskFailed                      // 永久失败
    TaskCancelled                   // 取消
    TaskSuspended                   // 挂起 (瞬态失败, 等待恢复)
)

type Task struct {
    ID           string
    Name         string
    DependsOn    []string       // 依赖的任务 ID
    Priority     int            // 优先级 (越高越先)
    Timeout      time.Duration  // 单任务超时
    MaxRetries   int            // 最大重试次数 (永久错误)
    MaxTransient int            // 瞬态错误额外重试次数
    Labels       map[string]string // 标签 (用于 Filter/Score)
    
    // 执行策略
    Runner       string         // TaskRunner 名称
    Input        any            // 输入数据
    Config       map[string]any // Runner 配置
    
    // 运行时状态 (由调度器管理)
    State        TaskState
    Retries      int
    Error        string
    Output       any
    StartedAt    time.Time
    CompletedAt  time.Time
}
```

**状态机**:
```
              ┌──────────────────────────────────┐
              │                                   │
  ┌───────┐  │  ┌─────────┐   ┌─────────┐       │
  │Pending│──┴─▶│ Ready   │──▶│Running │───┐     │
  └───┬───┘     └─────────┘   └────┬────┘   │     │
      │                             │        │     │
      │  ┌─────────┐               │   ┌────▼───┐ │
      └─▶│Blocked  │               │   │Completed│ │
         └────┬────┘               │   └─────────┘ │
              │                    │                │
              │               ┌────▼────┐          │
              │               │  Failed │          │
              │               └────┬────┘          │
              │                    │                │
              │               ┌────▼─────┐         │
              └───────────────│Suspended │─────────┘
                              │(瞬态恢复) │
                              └──────────┘
```

### 3.2 Graph (工作流图 — DAG + 条件边)

```go
type EdgeKind int
const (
    EdgeDependency EdgeKind = iota  // 数据/顺序依赖
    EdgeConditional                  // 条件分支 (满足条件才激活)
    EdgeDynamic                      // 运行时动态添加
)

type Edge struct {
    From      string
    To        string
    Kind      EdgeKind
    Condition func(ctx *ExecutionContext) bool // 条件边的判断函数
}

type Graph struct {
    ID    string
    Tasks map[string]*Task
    Edges []Edge
}
```

### 3.3 Scheduler (调度器 — 三阶段: Filter → Score → Dispatch)

借鉴 K8s 调度器的可插拔设计:

```go
// FilterPlugin 硬约束过滤 (不满足 → 不调度)
type FilterPlugin interface {
    Name() string
    Filter(task *Task, ctx *SchedulerContext) bool
}

// ScorePlugin 软约束打分 (分高 → 优先调度)
type ScorePlugin interface {
    Name() string
    Score(task *Task, ctx *SchedulerContext) int
}

// Scheduler 核心调度器
type Scheduler struct {
    filters  []FilterPlugin
    scorers  []ScorePlugin
    queue    *PriorityQueue     // 就绪队列 (借鉴 CFS vruntime)
    workers  *WorkerPool        // 执行器池 (借鉴 GMP)
    limiter  *BackpressureCtrl  // 背压控制 (借鉴 io_uring 队列深度)
}
```

**内置 Filter 插件**:
- `DependencyFilter`: 所有依赖已 completed
- `ResourceFilter`: Worker 池有空闲
- `CooldownFilter`: 瞬态失败后的冷却期未到

**内置 Score 插件**:
- `PriorityScore`: 按 Task.Priority 排序
- `FairnessScore`: 借鉴 CFS vruntime, 防止饥饿
- `LocalityScore`: 优先调度到有缓存的 Worker (GMP 本地队列)
- `CriticalPathScore`: DAG 关键路径上的任务优先

### 3.4 TaskRunner (执行器 — 可插拔)

```go
type TaskRunner interface {
    Name() string
    Execute(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (output any, err error)
}
```

**内置 Runner**:
- `LLMRunner`: 调用 LLM API
- `CodeRunner`: 编译+执行代码
- `EvalRunner`: 质量评估 (SkepticalReviewer)
- `TestRunner`: micro-test 验证
- `CompositeRunner`: 组合多个 Runner (对抗循环)
- `NoopRunner`: 空操作 (用于同步点)

### 3.5 Blackboard (黑板 — 结构化共享状态)

```go
type Blackboard interface {
    // 写入 (带版本, 防冲突)
    Write(key string, value any, meta WriteMeta) (version int64, err error)
    // 读取 (指定版本或最新)
    Read(key string, version int64) (value any, meta ReadMeta, err error)
    // 范围查询
    Query(prefix string, filter QueryFilter) []Entry
    // 监听变更 (事件驱动)
    Watch(prefix string) <-chan ChangeEvent
    // 快照
    Snapshot() map[string]Entry
}
```

**vs 现有实现的改进**:
- **版本控制**: 每次写入递增版本号, 支持 CAS 防冲突
- **事件驱动**: Watch 替代轮询, 任务完成立即通知下游
- **结构化 schema**: 预定义 key 格式 `{taskID}/output`, `{taskID}/eval`
- **容量控制**: 单 key 最大值, 自动截断策略

### 3.6 Checkpoint (检查点 — 持久化+恢复)

```go
type CheckpointStore interface {
    Save(execID string, state *ExecutionState) error
    Load(execID string) (*ExecutionState, error)
    List() ([]string, error)
}

type ExecutionState struct {
    GraphID     string
    Tasks       map[string]TaskSnapshot
    Blackboard  map[string]Entry
    Metrics     ExecutionMetrics
    SavedAt     time.Time
}
```

---

## 四、高级特性

### 4.1 对抗循环 (Adversarial Loop)

作为 `CompositeRunner` 的一种实现, 而非硬编码在调度器中:

```go
type AdversarialRunner struct {
    Generator   TaskRunner        // coder
    Evaluator   TaskRunner        // reviewer (SkepticalReviewer)
    Verifier    TaskRunner        // tester (micro-test)
    Terminator  TerminationPolicy // AdaptiveTerminator
    MaxRounds   int
}
```

### 4.2 背压控制 (Backpressure)

三层背压, 借鉴 io_uring 队列深度 + AIMD:

```go
type BackpressureCtrl struct {
    // L1: 全局 RPM 令牌桶 (API 层)
    RPMBucket    *TokenBucket
    // L2: 并发信号量 (Worker 层)
    Concurrency  *AdaptiveSemaphore  // AIMD 自适应
    // L3: 就绪队列深度 (调度层)
    QueueDepth   int                 // 超过 → 拒绝新任务入队
}
```

### 4.3 故障分治

```
错误分类器 (ErrorClassifier)
  │
  ├─ Transient (429/网络/超时)
  │   ├─ 指数退避 + jitter
  │   ├─ 额外重试配额
  │   ├─ 任务 → Suspended (不级联)
  │   └─ StallRecovery 可恢复
  │
  ├─ Permanent (验证/代码质量)
  │   ├─ 标准重试
  │   ├─ 任务 → Failed
  │   └─ 级联下游 Failed
  │
  └─ Fatal (API Key 无效/配额耗尽)
      ├─ 立即停止
      └─ 通知用户
```

### 4.4 并发冲突检测

当多个任务并行修改同一资源时:

```go
type ConflictDetector interface {
    // 声明任务要读写的资源
    DeclareAccess(taskID string, reads, writes []string)
    // 检查是否可并行 (无写-写冲突)
    CanParallel(taskA, taskB string) bool
    // 检测并解决冲突
    Resolve(conflicts []Conflict) Resolution
}
```

### 4.5 动态 DAG 扩展

借鉴 DynTaskMAS — 运行时根据 LLM 规划扩展图:

```go
type DynamicExpander interface {
    // 任务完成后, 可能动态添加新任务/边
    OnTaskComplete(task *Task, output any) (newTasks []*Task, newEdges []Edge)
}
```

---

## 五、扩展层

### 5.1 生命周期 Hook

```go
type LifecycleHook interface {
    OnGraphStart(g *Graph)
    OnTaskReady(t *Task)
    OnTaskStart(t *Task)
    OnTaskComplete(t *Task, result any)
    OnTaskFailed(t *Task, err error)
    OnTaskRetry(t *Task, attempt int)
    OnGraphComplete(g *Graph, results map[string]any)
    OnStallDetected(g *Graph, stuckTasks []*Task)
}
```

### 5.2 调度策略插件

| 策略 | 说明 | 来源 |
|:----:|:----:|:----:|
| `FIFOStrategy` | 先进先出 | 基础 |
| `PriorityStrategy` | 优先级队列 | K8s PriorityClass |
| `FairShareStrategy` | 公平调度 (vruntime) | Linux CFS |
| `DeadlineStrategy` | 截止期优先 | Linux EEVDF |
| `WorkStealingStrategy` | 空闲 Worker 窃取 | Go GMP |
| `CriticalPathStrategy` | DAG 关键路径优先 | 项目管理 CPM |
| `GangStrategy` | 齐套调度 | Volcano.sh |

### 5.3 可观测性

```go
type Observer interface {
    EmitMetric(name string, value float64, labels map[string]string)
    EmitEvent(event Event)
    EmitSpan(span Span)
}
```

---

## 六、与现有系统的映射

| 现有概念 | 新引擎映射 | 说明 |
|:--------:|:----------:|:----:|
| `WorkflowDef` + `StageDef` | `Graph` + `Task` | 统一 DAG 与 Stage 两套模型 |
| `WorkflowExecutor.Execute` | `Engine.Run(graph)` | 统一入口 |
| `Orchestrator.executeTaskNode` | `CompositeRunner` (对抗) | 对抗循环变为 Runner 插件 |
| `filterParallel` | `Scheduler.Filter+Score` | 可插拔并行判断 |
| `Blackboard.Write/Read` | `Blackboard` (带版本) | 增加版本+Watch |
| `CheckpointStore` | `CheckpointStore` | 增强: 全图快照 |
| `AdaptiveTerminator` | `TerminationPolicy` 接口 | 可插拔终止策略 |
| `RateLimitGuard` | `BackpressureCtrl.RPMBucket` | 集成到背压层 |
| `AgentPool` + `factory` | `WorkerPool` + `TaskRunner` | Runner 可插拔 |
| `handleTaskFailure` | `ErrorClassifier` + `RetryPolicy` | 瞬态/永久分治 |
| `teamWatchdog` | `StallDetector` (Hook) | 可配置阈值 |
| `activityCallback` | `LifecycleHook.OnTaskStart` | 统一 Hook |

---

## 七、目录结构

```
claude-go/pkg/engine/
├── engine.go           // Engine 主入口
├── graph.go            // Graph, Task, Edge 定义
├── scheduler.go        // Scheduler 核心 (Filter→Score→Dispatch)
├── runner.go           // TaskRunner 接口 + 内置 Runner
├── blackboard.go       // Blackboard 实现
├── checkpoint.go       // CheckpointStore 实现
├── backpressure.go     // 背压控制
├── errors.go           // 错误分类 + 重试策略
├── hooks.go            // 生命周期 Hook
├── strategy/           // 调度策略插件
│   ├── fifo.go
│   ├── priority.go
│   ├── fairshare.go
│   └── critical_path.go
└── adversarial.go      // 对抗循环 Runner
```

---

## 八、性能目标

| 指标 | 目标 | 参考 |
|:----:|:----:|:----:|
| 调度延迟 | < 1ms (10 任务) | Go channel + priority queue |
| 就绪检查 | O(1) 摊销 | 事件驱动 unblock |
| 检查点写入 | < 10ms | JSON + debounce |
| 背压响应 | < 100ms | AIMD + 令牌桶 |
| 最大并发任务 | 1000+ | goroutine pool |
| 图规模 | 10000+ 节点 | 邻接表 + 拓扑排序 |
