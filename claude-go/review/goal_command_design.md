# /goal 命令设计与实现方案

> **对应源码**: `github.com/anthropic/claude-go`  
> **调研日期**: 2026-05-21  
> **核心结论**: claude-go 已具备 80% 的 `/goal` 基础设施，缺口集中在**显式 Plan Mode 状态机**、**用户可 Review 的计划生成**和**动态 Replanning 触发器**三个层面。

---

## 1. 业界调研：Goal-Oriented Agent Command

### 1.1 概念定义

`/goal` 是一类让 Agent **从"被动响应"转向"主动规划"**的交互范式。用户用自然语言描述一个高层次目标，Agent 自动完成：

1. **目标理解**（Goal Understanding）— 澄清歧义、识别约束
2. **任务分解**（Task Decomposition）— 将目标拆解为可执行子任务
3. **依赖分析**（Dependency Analysis）— 识别子任务间的顺序/并行关系
4. **计划生成**（Plan Generation）— 输出人类可读的执行计划
5. **计划审查**（Plan Review）— 用户确认、修改或取消
6. **自主执行**（Autonomous Execution）— 按 DAG 调度、监控、重试
7. **动态重规划**（Dynamic Replanning）— 环境变化时调整计划

### 1.2 业界主要实现对比

| 产品/框架 | 命令形态 | 规划模式 | 执行模式 | 重规划 | 关键设计 |
|---|---|---|---|---|---|
| **Claude Code Plan Mode** | `EnterPlanMode` / `ExitPlanMode` | 原生内置，只读工具分析 | Sub-agent 并行执行 | 手动触发 | 共享 Task List，三阶段合并（Diff→Conflict→Verify） |
| **GitHub Copilot Plan Mode** | Agent 下拉选择 "Plan" | 两阶段：Planner（只读）→ Executor（全编辑） | 单 Agent 顺序执行 | 用户点击 "Refine Plan" | Markdown Plan 文件，用户 Review 后 "Start Implementation" |
| **Devin** | Slack/Teams 自然语言 | 自适应规划循环 | 多 Agent 并行（子任务隔离） | 自动（Plan→Do→Check→Act） | 信心等级、上下文保持、学习反馈 |
| **OpenHands** | `get_planning_agent()` | Planning Agent（只读）+ Execution Agent（全编辑） | 顺序+并行混合 | 每 10 步自动 Replan | Planning Interval τ=10，Browsing Condensation k=1 |
| **MetaGPT** | `role.run(project)` | SOP 标准化流程（Product/Architect/Engineer） | 多 Role 顺序+并行 | 按 SOP 阶段重试 | 类软件开发团队的结构化分工 |
| **CrewAI** | `crew.kickoff()` | Task + Agent + Process 组合 | 按 Process 类型（sequential/hierarchical） | 手动定义新 Task | Role-based 多 Agent 协作 |
| **AutoGPT** | `ai_name.goals[]` | 目标列表 + 自主决策 | ReAct 循环 | 每步自主决策是否继续 | 长期记忆 + 自主决策链 |

### 1.3 关键论文与技术路线

| 论文 | 年份 | 核心贡献 | 与 /goal 的关联 |
|---|---|---|---|
| **PlanGenLLMs: A Modern Survey of LLM Planning Capabilities** (arXiv:2502.11221) | 2025 | 系统分类 LLM 规划范式：顺序、并行、异步、递归分解 | 递归分解是 /goal 的核心算法基础 |
| **A Roadmap to Guide the Integration of LLMs in Hierarchical Planning** (arXiv:2501.08068) | 2025 | LLM 与 HTN（Hierarchical Task Network）集成分类学 | HTN 分解策略可直接用于 /goal 的任务拆分 |
| **Hierarchical Task Network Planning with LLM-Generated Heuristics** (arXiv:2605.07707) | 2025 | LLM 生成 HTN 搜索启发式，减少 83% 搜索量 | 可用于 /goal 的依赖优化和关键路径识别 |
| **DRIP: Backward Reasoning via Hierarchical Decomposition** (OpenReview) | 2024-25 | 逆向目标分解，从终态反推中间子任务 | 适合模糊目标的分解，比正向 ReAct 更稳定 |
| **Enhancing LLM-Based Agents via Global Planning and Hierarchical Execution** (arXiv:2504.16563) | 2025 | 全局规划 + 层级执行，减少上下文漂移 | 直接对应 /goal 的 Plan-then-Execute 架构 |
| **Dynamic Decomposition and Re-planning for Complex Web Tasks** (arXiv:2510.06587) | 2025 | 动态分解与重规划框架 | 提供 Replanning 触发条件和策略 |

### 1.4 设计模式总结：ReAct vs Plan-and-Execute vs Hybrid

```
ReAct（探索型）          Plan-and-Execute（结构型）      Hybrid（推荐）
┌─────────┐             ┌─────────────┐                ┌─────────────┐
│ Thought │             │  Planning   │                │  Planning   │
│  → Act  │             │   Phase     │                │   Phase     │
│  → Obs  │             │  (1 call)   │                │  (1 call)   │
│  → ...  │             └──────┬──────┘                └──────┬──────┘
│ 循环N次 │                    ▼                           ▼
└─────────┘             ┌─────────────┐                ┌─────────────┐
                        │  Execution  │                │  Execution  │
高适应性、高成本           │   Phase     │                │   Phase     │
适合短程/探索任务          │ (1~N calls) │                │ (ReAct风格) │
                        └─────────────┘                └──────┬──────┘
低适应性、低成本              ▲                              │
适合长程/结构化任务           │                              ▼
                        Replan Trigger                  ┌─────────────┐
                        (错误/环境变化)                  │ Replanning  │
                                                        │  (按需触发)  │
                                                        └─────────────┘
```

**现代生产系统趋向 Hybrid 架构**：先用 Plan-and-Execute 建立全局结构，在执行阶段用 ReAct 处理局部不确定性，在异常时触发 Replanning。

### 1.5 /goal 的 7 个设计要点（Best Practices）

1. **Planning 与 Execution 权限隔离**：Planner 阶段只给只读工具（read/search/analyze），不给编辑工具。防止"边想边改"导致的混乱。
2. **计划必须人类可读**：生成 Markdown/JSON 格式的计划，用户可 Review、修改、取消。
3. **显式依赖声明**：子任务间的依赖关系必须显式声明，支持 DAG 调度器的并行优化。
4. **关键路径优先**：调度时优先执行关键路径上的任务，缩短总耗时。
5. **动态 Replanning 而非静态计划**：环境变化（文件被修改、测试失败、需求变更）时自动或半自动调整计划。
6. **冲突检测与串行化**：并行任务修改同一文件时，通过冲突检测自动添加依赖边。
7. **可观测性**：计划的每个阶段（planning/executing/completed/failed）都必须可追踪、可上报。

---

## 2. claude-go 源码能力分析

### 2.1 现有基础设施（可复用度 > 80%）

#### 2.1.1 三层编排架构（Planner → Orchestrator → V2 DAG）

`pkg/agent/orchestrator.go` 中已明确划分了三层职责：

```go
// 职责分工:
//   Planner:      输出 WBS (任务分解 + 依赖图)
//   Orchestrator:  消费 WBS → 写入 V2 DAG → 调度 → micro-test → 重试 → E2E
//   V2 TaskStore:  提供 DAG 存储 + 就绪队列 + 依赖解除 (单一数据源)
```

**可复用点**：`/goal` 可直接复用这套三层架构，只需在 Planner 前增加一个 **Goal 解析层**。

#### 2.1.2 WBS 多策略解析器

`ParsePlanToDAG` 和 `ParsePlanToDAGWithRepair` 实现了工业级的计划解析：

- **策略 1**：JSON 解析（结构化 WBS）
- **策略 2**：Markdown 表格解析（宽松格式）
- **策略 3**：编号列表解析（自由文本）
- **Repair Loop**：1 轮 LLM 修复格式错误
- **Fallback**：从自由文本提取最小 DAG
- **验证**：`validateParsedWBSForObjective` + `validateFinalWBSForObjective`

**可复用点**：`/goal` 的 Planner 输出直接喂给这套解析器即可。

#### 2.1.3 丰富的任务元数据（TaskNode）

```go
type TaskNode struct {
    Title          string    // 人类可读名称
    Role           string    // 执行角色（coder/tester/architect）
    AcceptCriteria string    // 验收标准
    SubGoals       []SubGoal // 子目标（可独立验证）
    RiskLevel      string    // "low"/"medium"/"high"
    WorkUnitType   string    // "contract"/"implementation"/"verification"
    ConflictKeys   []string  // 符号级冲突键
    ParallelGroup  string    // 并行分组
    BlockingPolicy string    // "fail_blocks_dependents"/"fail_open"
    EstimatedMin   int       // 预估分钟数
    // ... 共 30+ 个字段
}
```

**可复用点**：TaskNode 的元数据已经足够支撑 /goal 的精细化调度。

#### 2.1.4 K8s 风格三阶段调度器

`pkg/orchestrator/scheduler.go` 实现了：

- **Phase 1 Filter**：依赖检查、资源检查、冲突检测
- **Phase 2 Score**：优先级 × 100 + 关键路径 500 分 + 公平性惩罚
- **Phase 3 Dispatch**：取前 `maxBatch` 个任务并发执行

**可复用点**：`/goal` 的执行阶段直接复用 Engine + Scheduler，无需改动。

#### 2.1.5 DAG 状态机与事件驱动引擎

`pkg/orchestrator/graph.go` + `pkg/orchestrator/engine.go`：

- 7 种任务状态（Pending → Ready → Running → Completed/Failed/Suspended/Cancelled）
- 事件驱动解锁：任务完成时立即检查下游依赖
- 停滞检测：`StallTimeout` 无进展时自动恢复
- 检查点：`CheckpointEvery` 支持断点续跑

**可复用点**：Goal 的执行生命周期直接映射到 DAG 状态机。

#### 2.1.6 动态图扩展（DynamicExpander）

`pkg/orchestrator/observer.go`：

```go
type DynamicExpander interface {
    OnTaskComplete(task *Task, output any) (newTasks []*Task, newEdges []Edge)
}
```

**可复用点**：这就是 Replanning 的基础设施！任务完成后可动态注入新任务。

#### 2.1.7 并发冲突检测（ConflictDetector）

```go
type ConflictDetector struct {
    reads  map[string][]string  // taskID → 读取资源
    writes map[string][]string  // taskID → 写入资源
}
func (d *ConflictDetector) CanParallel(taskA, taskB string) bool
```

**可复用点**：/goal 的并行执行阶段自动使用 ConflictDetector，无需额外开发。

#### 2.1.8 认知负载评估与任务拆分

`cognitiveLoadScore` 函数综合评估：
- 文件耦合度、任务类型、逻辑密度关键词、风险等级、预估时间
- 负载 > 8 明确拆分，6~8 允许拆分，<6 倾向 provider stall

**可复用点**：/goal 的 Planner 可用此函数指导分解粒度。

#### 2.1.9 QueryEngine 的 Plan Mode 基础

`pkg/engine/engine.go` 中已有：

```go
// DynamicPlanCheck 动态计划模式检查。
// 当 LLM 调用 EnterPlanMode 时返回 true, 引擎自动切换到只读权限。
DynamicPlanCheck func() bool
```

**可复用点**：但现有实现只是权限切换，缺少完整的 Plan Mode 状态机。

### 2.2 能力缺口（需新增 < 20%）

| 缺口 | 优先级 | 说明 |
|---|---|---|
| **显式 `/goal` CLI/API 入口** | P0 | 没有命令行或 API 层面的 Goal 提交入口 |
| **Goal 生命周期状态机** | P0 | 缺少 `pending → planning → reviewing → executing → completed/failed` 状态管理 |
| **Plan Review 机制** | P0 | 生成计划后没有用户确认/修改/取消的交互环节 |
| **动态 Replanning 触发器** | P1 | DynamicExpander 已有接口，但缺少触发策略（错误率阈值、环境变化检测） |
| **Goal 级 Blackboard 命名空间** | P1 | 多个 Goal 并发时，Blackboard 需要隔离 |
| **Plan 版本管理** | P2 | 用户修改计划后需要版本追踪和回滚 |
| **Goal 模板库** | P2 | 常见目标（"重构 X 模块"、"添加 Y 功能"）的预设分解模板 |

### 2.3 集成点分析

```
用户输入 /goal "重构认证模块，改用 JWT + Redis"
            │
            ▼
┌─────────────────────┐
│   GoalCommand       │  ← 新增：CLI/API 入口
│   (pkg/cmd/goal.go) │
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│   GoalSession       │  ← 新增：生命周期状态机
│   (pkg/goal/...)    │
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐     ┌─────────────────────┐
│   GoalPlanner       │────▶│   PlanReviewer      │  ← 新增：用户 Review
│   (增强现有 Planner) │     │   (pkg/goal/...)    │
└──────────┬──────────┘     └──────────┬──────────┘
           │ 用户确认后                   │
           ▼                             ▼
┌─────────────────────────────────────────────────────┐
│   Orchestrator.ParsePlanToDAG (已有)                │
│   → V2 DAG → Engine.Run (已有)                      │
│   → Scheduler.Filter→Score→Dispatch (已有)          │
│   → DynamicExpander / ReplanTrigger (新增触发器)     │
└─────────────────────────────────────────────────────┘
```

---

## 3. /goal 实现方案

### 3.1 总体架构：Goal-Oriented Execution Loop (GOEL)

```
┌──────────────────────────────────────────────────────────────────────┐
│                         Goal-Oriented Execution Loop                 │
│                                                                      │
│  ┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐          │
│  │  INPUT   │──▶│  PLAN    │──▶│  REVIEW  │──▶│ EXECUTE  │          │
│  │          │   │          │   │          │   │          │          │
│  │ /goal    │   │ Read-Only│   │ Markdown │   │ DAG      │          │
│  │ "..."    │   │ Analysis │   │ + Confirm│   │ Engine   │          │
│  └──────────┘   └──────────┘   └──────────┘   └────┬─────┘          │
│                                                     │                │
│                              ┌──────────────────────┘                │
│                              ▼                                       │
│                       ┌──────────────┐                               │
│                       │   MONITOR    │                               │
│                       │              │                               │
│                       │ • Progress   │                               │
│                       │ • Bottleneck │                               │
│                       │ • Replan?    │                               │
│                       └──────┬───────┘                               │
│                              │                                       │
│              ┌───────────────┼───────────────┐                      │
│              ▼               ▼               ▼                      │
│        ┌─────────┐    ┌──────────┐    ┌──────────┐                 │
│        │ COMPLETE│    │ REPLAN   │    │  HUMAN   │                 │
│        │         │    │          │    │  LOOP    │                 │
│        └─────────┘    └──────────┘    └──────────┘                 │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘
```

### 3.2 核心组件设计

#### 3.2.1 GoalCommand（CLI/API 入口）

位置：`pkg/cmd/goal.go`（CLI）或 `pkg/api/goal_handler.go`（API）

```go
type GoalCommand struct {
    SessionManager *goal.SessionManager
    Planner        *goal.GoalPlanner
    Reviewer       *goal.PlanReviewer
}

// Run 执行 /goal 命令的完整生命周期
func (c *GoalCommand) Run(ctx context.Context, objective string, opts GoalOptions) error {
    // 1. 创建 Goal Session
    session := c.SessionManager.Create(objective, opts)
    
    // 2. 进入 PLAN 阶段（只读权限）
    session.Transition(goal.StatePlanning)
    plan, err := c.Planner.Plan(ctx, session)
    if err != nil { return err }
    
    // 3. 进入 REVIEW 阶段（等待用户确认）
    session.Transition(goal.StateReviewing)
    approved, err := c.Reviewer.Review(ctx, session, plan)
    if err != nil || !approved {
        session.Transition(goal.StateCancelled)
        return err
    }
    
    // 4. 进入 EXECUTE 阶段（全权限）
    session.Transition(goal.StateExecuting)
    return c.executePlan(ctx, session, plan)
}
```

#### 3.2.2 GoalSession（目标生命周期状态机）

位置：`pkg/goal/session.go`

```go
type GoalState int

const (
    GoalPending    GoalState = iota // 已创建，未开始规划
    GoalPlanning                     // 正在分析代码库并生成计划
    GoalReviewing                    // 计划已生成，等待用户 Review
    GoalApproved                     // 用户已确认，准备执行
    GoalExecuting                    // 正在按 DAG 执行
    GoalReplanning                   // 执行中触发重规划
    GoalPaused                       // 用户手动暂停
    GoalCompleted                    // 全部成功
    GoalFailed                       // 有任务永久失败
    GoalCancelled                    // 用户取消
)

type GoalSession struct {
    ID          string
    Objective   string
    State       GoalState
    Plan        *GoalPlan
    GraphID     string        // 关联的 DAG Graph ID
    Blackboard  *Blackboard   // Goal 级隔离的 Blackboard
    Metrics     GoalMetrics
    CreatedAt   time.Time
    StartedAt   *time.Time
    CompletedAt *time.Time
    
    // 事件通道（供 UI 实时推送进度）
    EventCh chan GoalEvent
}
```

#### 3.2.3 GoalPlanner（目标分解器）

位置：`pkg/goal/planner.go`（复用并增强现有 `pkg/agent/` 的 Planner）

**增强点**：
1. **两阶段规划**：Phase 1 只读分析 → Phase 2 计划生成
2. **目标澄清**：Planner 可主动向用户提问（如 "JWT 用 HS256 还是 RS256？"）
3. **认知负载感知**：利用 `cognitiveLoadScore` 控制分解粒度
4. **模板匹配**：常见目标匹配预设模板，减少 LLM 调用

```go
type GoalPlanner struct {
    LLM            LLMClient
    ToolScope      string              // "read-only" during planning
    TemplateDB     *PlanTemplateDB     // 预设模板库
    MaxDepth       int                 // 最大递归分解深度
    LoadThreshold  float64             // 认知负载阈值（>8 强制拆分）
}

// Plan 执行目标分解，返回结构化计划
func (p *GoalPlanner) Plan(ctx context.Context, session *GoalSession) (*GoalPlan, error) {
    // Phase 1: 只读分析（代码库扫描、依赖识别）
    analysis, err := p.analyze(ctx, session.Objective)
    if err != nil { return nil, err }
    
    // Phase 2: 生成 WBS（利用模板或 LLM）
    plan, err := p.generateWBS(ctx, session.Objective, analysis)
    if err != nil { return nil, err }
    
    // Phase 3: 认知负载评估与自动拆分
    plan = p.splitByCognitiveLoad(plan)
    
    return plan, nil
}
```

#### 3.2.4 PlanReviewer（计划审查）

位置：`pkg/goal/reviewer.go`

**设计原则**：计划必须**阻塞等待用户确认**，不允许"默认执行"。

```go
type PlanReviewer struct {
    Renderer PlanRenderer  // Markdown / JSON / TUI 渲染器
    Timeout  time.Duration // Review 超时（默认 30 分钟）
}

// Review 渲染计划并等待用户确认
func (r *PlanReviewer) Review(ctx context.Context, session *GoalSession, plan *GoalPlan) (bool, error) {
    // 1. 渲染为人类可读的 Markdown
    markdown := r.Renderer.Render(plan)
    
    // 2. 推送 Review 请求（CLI 打印 / API 推送 / WebSocket）
    session.Emit(GoalEvent{
        Type:    EventPlanReady,
        Payload: markdown,
    })
    
    // 3. 阻塞等待用户响应（approve / modify / cancel）
    resp, err := r.waitForResponse(ctx, session.ID)
    if err != nil { return false, err }
    
    switch resp.Action {
    case "approve":
        return true, nil
    case "modify":
        plan.ApplyModifications(resp.Modifications)
        return r.Review(ctx, session, plan) // 递归 Review
    case "cancel":
        return false, ErrPlanCancelled
    default:
        return false, fmt.Errorf("unknown action: %s", resp.Action)
    }
}
```

**Review 界面示例（Markdown）**：

```markdown
## Goal: 重构认证模块，改用 JWT + Redis

### 计划概览
- 预估时间: 45 分钟
- 任务数: 8（并行度: 3）
- 风险等级: Medium

### 任务分解

| # | 任务 | 角色 | 依赖 | 预估 | 风险 |
|---|------|------|------|------|------|
| 1 | 分析现有 auth 模块接口 | architect | - | 5min | Low |
| 2 | 设计 JWT + Redis 新接口 | architect | #1 | 8min | Medium |
| 3 | 实现 JWT token 生成/验证 | coder | #2 | 10min | Medium |
| 4 | 实现 Redis session 存储 | coder | #2 | 10min | Medium |
| 5 | 修改登录/登出 handler | coder | #3,#4 | 8min | High |
| 6 | 编写单元测试 | tester | #3,#4,#5 | 10min | Low |
| 7 | 运行集成测试 | tester | #6 | 5min | Low |
| 8 | 清理旧 session 代码 | coder | #7 | 5min | Low |

### 关键路径
1 → 2 → 3 → 5 → 6 → 7 → 8（预计 43 分钟）

### 冲突检测
- 任务 #3 和 #4 可并行（无文件冲突）
- 任务 #5 依赖 #3,#4，将自动串行

[确认执行] [修改计划] [取消]
```

#### 3.2.5 ReplanTrigger（动态重规划触发器）

位置：`pkg/goal/replan.go`（实现 DynamicExpander 接口）

**触发条件**（可配置）：

| 条件 | 阈值 | 动作 |
|---|---|---|
| 任务失败率 | > 30% | Replan（可能计划有系统性错误） |
| 停滞检测 | StallTimeout 触发 | Replan 或请求人类介入 |
| 文件被外部修改 | 与 plan 时的快照不一致 | Replan（环境已变化） |
| 用户主动请求 | `/replan` 命令 | 立即 Replan |
| 新需求发现 | 执行中发现 plan 未覆盖的需求 | 增量 Replan |

```go
type ReplanTrigger struct {
    FailureThreshold   float64       // 失败率阈值（默认 0.3）
    StallTimeout       time.Duration // 停滞超时（默认 5min）
    FileChangeCheck    bool          // 是否检查文件变化
    Planner            *GoalPlanner  // 用于重新生成计划
}

func (t *ReplanTrigger) OnTaskComplete(task *Task, output any) ([]*Task, []Edge) {
    // 1. 检查是否需要 Replan
    if !t.shouldReplan(task) {
        return nil, nil
    }
    
    // 2. 收集当前执行上下文（已完成任务的输出、失败原因）
    context := t.gatherContext(task)
    
    // 3. 调用 Planner 生成增量计划
    newPlan := t.Planner.Replan(context)
    
    // 4. 转换为 Task + Edge
    return t.planToTasks(newPlan)
}

func (t *ReplanTrigger) shouldReplan(task *Task) bool {
    // 失败率检查
    if t.failureRate() > t.FailureThreshold {
        return true
    }
    // 停滞检查
    if t.isStalled() {
        return true
    }
    // 文件变化检查
    if t.FileChangeCheck && t.hasFileChanged(task) {
        return true
    }
    return false
}
```

### 3.3 执行流程（完整时序）

```
用户: /goal "重构认证模块，改用 JWT + Redis"
  │
  ▼
[GoalSession: pending → planning]
  │
  ├──▶ [Planner Phase 1: 只读分析]
  │      ├── read_file(auth/*.go)
  │      ├── grep_search("session", "auth")
  │      └── analyze_dependencies()
  │
  ├──▶ [Planner Phase 2: WBS 生成]
  │      ├── LLM: "分解为 8 个子任务"
  │      ├── cognitiveLoadScore() 校验粒度
  │      └── normalizeAndSplitRawTasks()
  │
  ├──▶ [Planner Phase 3: 冲突检测]
  │      └── smartConflictKeys() 生成依赖边
  │
  ▼
[GoalSession: planning → reviewing]
  │
  ├──▶ [PlanReviewer 渲染 Markdown]
  │      └── 推送给用户（CLI / API / WebSocket）
  │
  ├──▶ [用户 Review]
  │      ├── 确认 → [approved]
  │      ├── 修改 → [重新生成 → Review]
  │      └── 取消 → [cancelled]
  │
  ▼
[GoalSession: reviewing → executing]
  │
  ├──▶ [Orchestrator.ParsePlanToDAG()]
  │      └── 写入 V2 TaskStore DAG
  │
  ├──▶ [Engine.Run()]
  │      ├── Scheduler.Filter→Score→Dispatch
  │      ├── 并行执行（ConflictDetector 自动串行化冲突）
  │      └── 事件驱动解锁下游任务
  │
  ├──▶ [ReplanTrigger 监控]
  │      ├── 正常 → 继续执行
  │      ├── 失败率高 → [replanning]
  │      └── 停滞 → [replanning / human-in-the-loop]
  │
  ▼
[GoalSession: executing → completed/failed]
  │
  ├──▶ [生成执行报告]
  │      ├── 成功任务列表
  │      ├── 失败任务及原因
  │      ├── 修改的文件清单
  │      └── 建议的后续操作
  │
  ▼
用户: 收到报告
```

### 3.4 与现有系统的最小侵入集成

| 现有组件 | 改动方式 | 说明 |
|---|---|---|
| `pkg/agent/orchestrator.go` | **无侵入，直接复用** | ParsePlanToDAG、rawTasksToDAG、WBS 解析全部复用 |
| `pkg/orchestrator/engine.go` | **无侵入，直接复用** | Engine.Run、Scheduler、Stall 检测、Checkpoint 全部复用 |
| `pkg/orchestrator/observer.go` | **实现接口** | ReplanTrigger 实现 DynamicExpander 接口 |
| `pkg/engine/engine.go` | **增强** | DynamicPlanCheck 扩展为完整的 Plan Mode 状态机 |
| `pkg/orchestrator/tool.go` | **增强** | ToolScopeManager 增加 "planning" 场景（只读工具集） |
| `pkg/api/client.go` | **无改动** | 无需修改 |
| CLI 入口 | **新增** | `pkg/cmd/goal.go` 新增 `/goal` 命令 |

---

## 4. 代码实现示例

详见同目录下的 `goal_command_code_examples.go`，包含以下组件的参考实现：

1. `GoalSession` — 生命周期状态机
2. `GoalPlanner` — 两阶段目标分解器
3. `PlanReviewer` — Markdown 渲染 + 用户确认
4. `ReplanTrigger` — 动态重规划触发器（实现 DynamicExpander）
5. `GoalCommand` — CLI 入口编排

---

## 5. 风险与演进路线

### 5.1 风险

| 风险 | 概率 | 影响 | 缓解措施 |
|---|---|---|---|
| Plan 质量差导致执行失败 | 中 | 高 | Planner 增加验证层 + V1 Fallback DAG + 用户 Review |
| Replanning 过于频繁 | 中 | 中 | 设置冷却期（5 分钟内不重复 Replan）、最大 Replan 次数（3 次） |
| 用户 Review 阻塞流程 | 高 | 低 | 支持异步 Review（Web/API），支持 "信任模式" 跳过 Review |
| 多 Goal 并发 Blackboard 污染 | 低 | 高 | Goal 级 Blackboard 命名空间隔离 |
| Plan 过于乐观/悲观 | 中 | 低 | 利用历史执行数据校准预估时间 |

### 5.2 演进路线

**Phase 1（MVP，2 周）**：
- GoalCommand CLI 入口
- GoalSession 状态机
- PlanReviewer Markdown 渲染
- 复用现有 Planner + Orchestrator + Engine

**Phase 2（增强，2 周）**：
- ReplanTrigger 实现（失败率 + 停滞触发）
- Goal 级 Blackboard 隔离
- Plan Template DB（常见目标模板）

**Phase 3（完善，2 周）**：
- 计划版本管理与回滚
- 历史数据驱动的预估校准
- WebSocket 实时进度推送
- `/replan` 主动重规划命令

---

## 6. 参考来源

- [GitHub Blog: Agent mode 101](https://github.blog/ai-and-ml/github-copilot/agent-mode-101-all-about-github-copilots-powerful-mode/)
- [Microsoft DevBlogs: Planning in Visual Studio](https://devblogs.microsoft.com/visualstudio/introducing-planning-in-visual-studio-public-preview/)
- [GitHub Blog: Copilot CLI Plan Mode](https://github.blog/changelog/2026-01-21-github-copilot-cli-plan-before-you-build-steer-as-you-go/)
- [VS Code Agent TODOs Extension](https://github.com/digitarald/vscode-agent-todos)
- [Claude Code Sub-agent Patterns](https://claudelab.net/en/articles/claude-code/claude-code-subagent-pattern-autonomous-parallel-task)
- [PlanGenLLMs Survey](https://arxiv.org/abs/2502.11221)
- [Roadmap to LLMs in Hierarchical Planning](https://arxiv.org/abs/2501.08068)
- [Hierarchical Task Network Planning with LLM Heuristics](https://arxiv.org/abs/2605.07707)
- [ReAct vs Plan-and-Execute Architecture Comparison](https://cheesecat.net/blog/react-plan-execute-architecture-comparison-2026-zh-tw/)
