# JiuwenSwarm 深度解读与 claude-go 蜂群模式融合方案

> 本文基于对 [openJiuwen-ai/jiuwenswarm](https://github.com/openJiuwen-ai/jiuwenswarm) 源码与文档的全面分析，结合 claude-go `pkg/swarm_intel` 现有蜂群智能引擎，识别可吸收的设计模式与工程实践，输出可落地的融合改造方案。

---

## 目录

1. [JiuwenSwarm 架构总览](#1-jiuwenswarm-架构总览)
2. [核心子系统拆解](#2-核心子系统拆解)
   - 2.1 模式系统 (Modes)
   - 2.2 任务规划 (TodoToolkit)
   - 2.3 经验记忆 (Task Memory)
   - 2.4 分布式 Team (Distributed Team)
   - 2.5 技能自演进 (Skill Self-Evolution)
3. [claude-go 蜂群模式现状](#3-claude-go-蜂群模式现状)
4. [双向对比矩阵](#4-双向对比矩阵)
5. [可吸收模式与融合方案](#5-可吸收模式与融合方案)
   - M1: 模式注册表与动态 Rail 挂载
   - M2: 持久化 Todo 与动态任务干预
   - M3: 经验记忆引擎 (ACE / ReasoningBank / ReMe)
   - M4: 分布式团队运行时 (Control / Data Plane)
   - M5: 技能自演进流水线 (SkillDev Pipeline)
6. [实施优先级与风险](#6-实施优先级与风险)
7. [参考文件](#7-参考文件)

---

## 1. JiuwenSwarm 架构总览

JiuwenSwarm 是一个面向 Python 生态的 AI Agent 框架，强调**模式驱动、团队协同、经验沉淀**。其顶层架构可概括为：

```
┌─────────────────────────────────────────────────────────────┐
│                     Channels (Web / CLI / WS)                │
├─────────────────────────────────────────────────────────────┤
│  Gateway  →  AgentServer  →  Runtime (Runner + DeepAgent)   │
├─────────────────────────────────────────────────────────────┤
│  Harness Layer                                               │
│  ├── Modes (agent.plan / agent.fast / code.* / team)        │
│  ├── TodoToolkit (todo_create/insert/complete/remove/list)  │
│  ├── Memory (internal FTS + vector + external service)      │
│  ├── Task Memory (experience_retrieve/learn/clear)          │
│  └── Team (TeamManager + distributed runtime + A2X registry)│
├─────────────────────────────────────────────────────────────┤
│  Skill Layer                                                 │
│  ├── SkillManager (加载/安装/卸载/marketplace)               │
│  └── SkillDevService (无状态流水线 + checkpoint + 挂起点)    │
└─────────────────────────────────────────────────────────────┘
```

关键设计哲学：
- **配置即策略**：`config/config.yaml` 中 `modes` 段定义了不同模式下的工具白名单、Rails 约束、记忆策略。
- **会话级隔离**：Todo、Memory、Team workspace 均按 `session_id` 隔离，避免多会话污染。
- **控制面/数据面分离**：分布式 Team 的 bootstrap（控制面）与任务执行（数据面）解耦，通过 A2X 注册中心 + pyzmq 实现动态发现。
- **无状态服务**：SkillDevService 不持有 Pipeline 生命周期，每次请求从 StateStore 加载 → 执行 → checkpoint → 释放。

---

## 2. 核心子系统拆解

### 2.1 模式系统 (Modes)

JiuwenSwarm 支持 4 大类 5 子模式：

| 模式 | 代号 | 工具集 | 记忆策略 | Rails |
|------|------|--------|----------|-------|
| Agent (规划) | `agent.plan` | 全工具 | 主动记忆 (proactive) | 无额外 |
| Agent (快速) | `agent.fast` | 全工具 | 被动记忆 (passive) | 无额外 |
| Code (规划) | `code.plan` | 白名单 + 编码记忆 | 编码记忆 | FileSystemRail + SkillUseRail + LspRail |
| Code (常规) | `code.normal` | 白名单 + 编码记忆 | 编码记忆 | 同上 |
| Team | `team` | 全工具 | 按团队配置 | 按团队配置 |

切换命令：`/mode agent.plan`、`/switch plan`。

**工程亮点**：
- **动态工具白名单**：Code 模式下仅暴露 `web_free_search`、`web_fetch_webpage` 等白名单工具，避免 Agent 在编码场景中调用无关工具导致上下文膨胀。
- **动态 Rail 挂载**：不同模式下挂载不同的 Rails（如 `FileSystemRail` 限制文件访问范围），Rails 不是全局硬编码，而是模式配置驱动。
- **记忆策略差异**：`agent.plan` 在每次推理前自动检索记忆，`agent.fast` 仅在用户显式触发时检索，用记忆成本换响应速度。

### 2.2 任务规划 (TodoToolkit)

提供 5 个工具，以 Markdown 形式持久化到 `workspace/session/{session_id}/todo.md`：

| 工具 | 行为 |
|------|------|
| `todo_create` | 创建初始清单，若已存在则失败 |
| `todo_insert` | 在指定索引插入，已有任务后移；若清单不存在则自动创建 |
| `todo_complete` | 标记完成，可附带 `result` |
| `todo_remove` | 删除任务，剩余任务重新编号 |
| `todo_list` | 列出全部任务及状态 |

**工程亮点**：
- **按 session 的文件锁**：`TodoToolkit` 使用类变量 `_session_locks`（`Dict[str, threading.Lock]`）保证同一会话下的并发安全，避免 read-modify-write 丢失更新。
- **人机协作的动态干预**：用户可随时追加需求，Agent 调用 `todo_insert` 插入新任务；支持任务取消（`cancelled` 状态）。
- **Markdown 人可读**：持久化格式为 `- [ ] 1. task | status | result`，用户可直接查看/编辑。

### 2.3 经验记忆 (Task Memory)

内建任务经验系统，提供 3 个工具：

| 工具 | 用途 |
|------|------|
| `experience_retrieve` | 根据 query 检索相关历史经验 |
| `experience_learn` | 记录关键规律、规则、洞见 |
| `experience_clear` | 清除所有经验（需用户确认） |

**算法插件化**：
- **ACE**（默认）：基于嵌入相似度的经验检索。
- **ReasoningBank**：推理增强型检索，适合复杂推理链路。
- **ReMe**：反思型检索，侧重从错误中学习。

**工程亮点**：
- **降级可用**：外接经验服务不可用时，本地 `workspace/agent/task-data.json` 仍可提供 `persisted_only` 状态的数据。
- **结构化经验**：`experience_learn` 支持 `section`、`when_to_use`、`title`、`tools_used` 等元数据，便于后续分类检索。
- **双源合并**：服务正常时，检索结果合并本地持久化数据和服务端数据。

### 2.4 分布式 Team (Distributed Team)

JiuwenSwarm 的 Team 模式不仅支持单进程内的 `inprocess` 协作，还支持跨进程/跨机的 `distributed` 部署：

**角色定义**：
- **Leader**：负责任务分解、team 创建、成员调度、结果汇总。
- **Teammate**：接收任务、执行、回传结果。

**关键组件**：
- **A2X Registry**：外部注册中心，teammate 启动后注册为 blank/idle 节点；leader `spawn_member` 时从注册中心预约空闲节点。
- **pyzmq Transport**：leader 与 teammate 之间的直接通信（`direct_addr`）+ 发布订阅（`pubsub_publish_addr` / `pubsub_subscribe_addr`）。
- **PostgreSQL Shared Storage**：多进程共享 TeamDB，存储任务、成员状态、消息等业务状态。
- **TeamManager**：管理 team 生命周期、skill 同步、evolution watchers、rails。

**控制面 / 数据面分离**（重点）：
- **控制面**：teammate 注册 → leader 预约 → direct ZMQ bootstrap → teammate ACK → 状态同步。
- **数据面**：任务创建、认领、完成、团队消息仍走 team 业务链路（共享存储 + team runtime）。
- **兜底策略**：bootstrap 发送失败后不再 fallback 到 `team_message`；teammate 侧不再使用 DB 轮询兜底。

### 2.5 技能自演进 (Skill Self-Evolution)

SkillDevService 是一个**无状态请求处理器**，对外暴露 7 个 method：

| Method | 行为 |
|--------|------|
| `skilldev.start` | 发起新任务 |
| `skilldev.respond` | 统一确认（后端根据 `task_id` 当前阶段自动路由） |
| `skilldev.status` | 查状态 / 列任务 |
| `skilldev.download` | 下载产物 |
| `skilldev.cancel` | 取消任务 |
| `skilldev.file.list` | 获取文件树 |
| `skilldev.file.read` | 读取文件内容 |

**设计要点**：
- **无状态**：不持有 Pipeline 对象，不做生命周期管理。
- **每次请求**：StateStore 加载状态 → 创建 Pipeline → 执行 → checkpoint → 释放。
- **挂起点 (Suspension Points)**：Pipeline 执行到特定阶段会挂起，等待用户确认后再继续（`skilldev.respond` 自动路由到当前阶段）。
- **Method Dispatch**：用字典映射 `ReqMethod` → handler，避免 if/elif 链。

---

## 3. claude-go 蜂群模式现状

claude-go 的 `pkg/swarm_intel` 是一个基于**群体智能算法**的预测与模拟引擎，核心为 7 阶段流水线：

```
Decompose → Scout → Predict → Debate → Fuse → Calibrate → Learn
```

**核心组件**：

| 组件 | 学术来源 | 功能 |
|------|----------|------|
| BoidsCoordinator | Reynolds 1987 | 对齐/分离/聚合 → 信念空间多样性控制 |
| PheromoneMemory | ACO (Dorigo 1997) | 信素沉积/蒸发/强化，路径记忆 |
| Multi-Agent Debate | Du et al. 2023 | 多轮辩论提升推理准确性 |
| Bayesian Fuser | 对数意见池 | 融合多源概率估计 |
| ByzantineFuser | BFT | 容忍 f < n/3 的恶意/故障分析师 |
| BanditRouter | MAB | 动态选择最优分析师角色 |
| ConformalCalibrator | 共形预测 | 提供 95% 置信区间校准 |
| ContextCompressor | M10 | 上下文压缩，截断防膨胀 |
| ResilientCaller | M10 | 弹性调用（重试/熔断） |
| ReasoningBank | M6 | 历史推理路径检索与复用 |

**现状局限**：
1. **偏重预测/模拟**：`swarm_intel` 主要解决“预测问题”，而非“任务执行问题”。没有显式的任务拆解、待办追踪、动态干预机制。
2. **无模式系统**：没有按场景切换工具集、Rails、记忆策略的能力。
3. **团队执行层薄弱**：虽有 Boids + Debate 的“群体推理”，但缺乏 Leader/Teammate 的角色分工、任务调度、生命周期管理。
4. **经验沉淀偏底层**：`ReasoningBank` 和 `PheromoneStore` 存储的是算法中间状态（信素、预测历史），而非用户可直接调用的“经验工具”。
5. **技能系统静态**：没有 SkillDev 式的无状态流水线与挂起点设计。

---

## 4. 双向对比矩阵

| 维度 | JiuwenSwarm | claude-go swarm_intel | 差距 |
|------|-------------|----------------------|------|
| **核心目标** | 任务执行 + 团队协作 | 群体智能预测/模拟 | 互补 |
| **模式切换** | 4 模式 5 子模式，配置驱动 | 无显式模式系统 | J > C |
| **任务规划** | TodoToolkit，Markdown 持久化，动态干预 | GoalTree + WBS + DAG，无持久化 Todo | J ≈ C，互补 |
| **经验记忆** | 用户级工具 (retrieve/learn/clear)，ACE/ReMe/ReasoningBank | 算法级 ReasoningBank + PheromoneStore | J > C (用户可见性) |
| **团队架构** | Leader/Teammate + A2X + pyzmq + PostgreSQL | BoidsCoordinator + Multi-Agent Debate | J > C (执行层) |
| **技能演进** | SkillDevService 无状态流水线 + 挂起点 | 静态 skill 注册 | J > C |
| **群体智能算法** | 无专门算法层 | Boids + ACO + Bayesian + Bandit + Conformal | C > J |
| **可观测性** | Metrics + TeamMonitorHandler | MetricsCollector + NotifyFunc | 相当 |
| **容错设计** | 降级到 local、fallback 策略 | ResilientCaller + ByzantineFuser | C > J (算法层) |
| **上下文压缩** | 依赖外部模型 | ContextCompressor (M10) | C > J |

---

## 5. 可吸收模式与融合方案

### M1: 模式注册表与动态 Rail 挂载

**JiuwenSwarm 做法**：
- `config.yaml` 中 `modes.agent.plan`、`modes.code.rails` 等段定义不同模式的行为。
- 运行时根据当前模式动态挂载/卸载 Rails，调整工具白名单。
- `agent.fast` 用被动记忆，`agent.plan` 用主动记忆。

**claude-go 融合方案**：

在 claude-go 中引入 `ModeRegistry`，与现有 `Agent` 运行时集成：

```go
// 概念设计（详见 jiuwenswarm_code_examples.go）
type ModeRegistry struct {
    modes map[string]ModeConfig
}

type ModeConfig struct {
    ToolWhitelist   []string      // 空表示全工具
    Rails           []RailMount   // 动态挂载的 Rails
    MemoryStrategy  MemoryStrategy // proactive | passive
    EmbeddingConfig *EmbeddingConfig
    MaxIterations   int
}
```

**吸收价值**：
- 解决当前 claude-go “全工具全场景”导致的上下文膨胀问题。
- Code 模式下自动限制工具集，减少 Token 消耗（与推理优化方案协同）。
- 不同模式配置不同的 `MaxIterations`，快速模式降低迭代上限以提升响应速度。

**实施位置**：`pkg/agent/moderegistry.go`（新建）。

---

### M2: 持久化 Todo 与动态任务干预

**JiuwenSwarm 做法**：
- `TodoToolkit` 按 `session_id` 隔离，Markdown 持久化。
- 5 个工具覆盖任务全生命周期，支持中途插入/取消。
- 文件锁保证并发安全。

**claude-go 融合方案**：

claude-go 已有 `GoalTree`、`WBS Parser`、`DAG Engine`（见 `pkg/agent/goaldecomp.go`），但缺乏**用户可见的、可干预的待办清单**。融合思路：

1. **增强 GoalPlanner**：在目标分解后，将 WBS 节点同步到 `TodoManager`。
2. **TodoManager**：按 `session_id` 持久化到 `workspace/{session_id}/todo.md`，提供与 jiuwenswarm 等价的 5 个操作。
3. **动态干预钩子**：当用户插入新需求时，`DynamicExpander`（已有组件）检测变化 → 调用 `todo_insert` → 触发 DAG 局部重排。
4. **人机协作界面**：Todo Markdown 文件可供用户直接编辑，Agent 检测到文件变化后重新加载。

```go
// 概念设计
type TodoManager struct {
    sessionID string
    todoPath  string
    mu        sync.Mutex
}

func (tm *TodoManager) Insert(at int, tasks []string) error
func (tm *TodoManager) Complete(idx int, result string) error
```

**吸收价值**：
- 解决长周期任务中的“目标丢失与执行断层”问题。
- 为 `/goal` 命令（已有方案）提供可视化进度追踪。
- 人可读的 Markdown 格式降低用户认知负担。

**实施位置**：`pkg/agent/todomanager.go`（新建），与 `pkg/agent/goaldecomp.go` 联动。

---

### M3: 经验记忆引擎 (ACE / ReasoningBank / ReMe)

**JiuwenSwarm 做法**：
- `experience_retrieve/learn/clear` 三个用户级工具。
- 算法可插拔：ACE（嵌入相似度）、ReasoningBank（推理增强）、ReMe（反思型）。
- 本地 JSON 持久化 + 可选远程服务，降级可用。
- 经验带结构化元数据：`section`、`when_to_use`、`tools_used`、`label`。

**claude-go 融合方案**：

claude-go 已有 `ReasoningBank`（`pkg/swarm_intel/store.go` 附近），但它是**算法内部组件**，用户无法直接调用。融合思路：

1. **升级为 ExperienceEngine**：将 `ReasoningBank` 封装为 `ExperienceEngine`，对外暴露 `Retrieve`、`Learn`、`Clear` 接口。
2. **算法插件化**：定义 `RetrievalAlgo` 接口，实现 `ACE`、`ReasoningBank`、`ReMe` 三种策略。
3. **结构化经验记录**：在 `Learn` 时记录 `tools_used`（哪些工具成功/失败）、`when_to_use`（适用场景）、`brier_score`（预测校准度，从 `swarm_intel` 的 `ConformalCalibrator` 获取）。
4. **双源合并**：本地 SQLite/JSONL + 可选远程经验服务。
5. **与 swarm_intel 联动**：`Engine.Predict` 的 Phase 2 (Scout) 和 Phase 7 (Learn) 自动调用 `ExperienceEngine`。

```go
// 概念设计
type ExperienceEngine struct {
    localStore   *LocalExperienceStore
    remoteClient *ExperienceServiceClient // optional
    retriever    RetrievalAlgo
    summarizer   SummaryAlgo
}

type ExperienceRecord struct {
    ID          string
    Query       string
    Content     string
    Section     string
    WhenToUse   string
    ToolsUsed   []ToolUsage
    BrierScore  float64
    Source      string // "local" | "remote" | "merged"
}
```

**吸收价值**：
- 将算法内部的经验沉淀转化为用户可感知的能力。
- `tools_used` 记录失败工具调用，与 OpenClacky 的 ToolScopeManager 协同，实现工具黑名单动态更新。
- 预测场景下利用历史 Brier Score 筛选高可信度经验。

**实施位置**：`pkg/swarm_intel/experience.go`（新建），与现有 `ReasoningBank` 复用存储层。

---

### M4: 分布式团队运行时 (Control / Data Plane)

**JiuwenSwarm 做法**：
- **控制面**：A2X Registry + direct ZMQ bootstrap + ACK 确认。
- **数据面**：PostgreSQL 共享存储 + team runtime 业务链路。
- **TeamManager**：生命周期管理、skill 同步、evolution watchers。
- **运行时继承**：teammate 自动继承 leader 的部分配置（`RuntimeInfo`、`TeamWorkspaceInfo`）。

**claude-go 融合方案**：

claude-go 在 `CLAUDE.md` 中已有 **Agent Teams** 概念（`TeamCreate`、`TaskCreate`、`SendMessage`、`hierarchical` topology），但当前实现偏“协调层”而非“执行层”。融合思路：

1. **TeamRuntime 包**：新建 `pkg/teamruntime/`，实现 Leader/Teammate 角色定义。
2. **控制面**：
   - 引入 `RegistryClient` 接口（A2X 风格），支持多种注册后端（etcd / Consul / 自定义 HTTP）。
   - `Leader` 通过 `ReserveMembers` 从注册中心发现空闲 teammate。
   - `Teammate` 启动后注册自身 `bootstrap_addr`，监听 bootstrap 消息。
   - Bootstrap 成功后 teammate 更新状态为 `busy`，team 解散后重置为 `idle`。
3. **数据面**：
   - 任务状态、成员状态、消息通过共享存储同步（优先 PostgreSQL，降级到 SQLite + 文件锁）。
   - 直接通信使用 gRPC / QUIC（比 pyzmq 更符合 Go 生态）。
4. **运行时继承**：teammate 接收 leader 的 `RuntimeConfig`（模型配置、工具白名单、Rails），避免逐节点重复配置。
5. **与 swarm_intel 结合**：Leader 可用 `swarm_intel.Engine` 做任务分解和预测，teammate 执行具体子任务。

```go
// 概念设计
type TeamRuntime struct {
    role       Role // Leader | Teammate
    registry   RegistryClient
    transport  Transport // gRPC / QUIC
    storage    TeamStorage // PostgreSQL | SQLite
    leaderCfg  *RuntimeConfig // teammate 继承
}

func (tr *TeamRuntime) Bootstrap(ctx context.Context) error
func (tr *TeamRuntime) ReserveMembers(n int) ([]MemberEndpoint, error)
func (tr *TeamRuntime) DestroyTeam(ctx context.Context) error
```

**吸收价值**：
- 将 claude-go 的 Agent Teams 从“协调层”升级为“可跨机执行的运行时”。
- 控制面/数据面分离提升分布式场景下的可靠性。
- 运行时继承减少多节点配置漂移。

**实施位置**：`pkg/teamruntime/`（新建目录）。

---

### M5: 技能自演进流水线 (SkillDev Pipeline)

**JiuwenSwarm 做法**：
- **无状态服务**：`SkillDevService` 不持有 Pipeline 生命周期。
- **StateStore**：每次请求加载/保存状态，支持断点续传。
- **挂起点**：Pipeline 执行到特定阶段自动挂起，等待用户确认。
- **Method Dispatch**：字典路由，避免 if/elif 链。

**claude-go 融合方案**：

claude-go 的技能系统当前为静态注册（`skills/` 目录下的 markdown 定义）。融合思路：

1. **SkillDevPipeline 接口**：定义阶段化流水线接口。
   ```go
   type SkillDevPipeline interface {
       Run(ctx context.Context, state *SkillDevState) (SkillDevEventIterator, error)
       Stage() SkillDevStage
       SuspensionPoints() []SkillDevStage // 哪些阶段需要用户确认
   }
   ```
2. **StateStore**：基于 SQLite/JSONL 的持久化存储，支持 `Load(taskID)` / `Save(taskID, state)`。
3. **挂起点机制**：Pipeline 运行到 `SuspensionPoints` 中的阶段时，返回 `Suspended` 事件；用户通过 `skilldev.respond` 提供确认/修改后，从 StateStore 恢复状态继续执行。
4. **Method Dispatch**：在 CLI 或 API 层用 `map[ReqMethod]Handler` 替代 switch-case。
5. **与 Team 结合**：Skill 演进任务可分配给 teammate 执行，leader 仅做审批。

```go
// 概念设计
type SkillDevState struct {
    TaskID      string
    Stage       SkillDevStage
    Input       SkillDevInput
    Artifacts   []Artifact
    Suspended   bool
    History     []StageTransition
}

type SkillDevService struct {
    deps       SkillDevDeps
    dispatch   map[ReqMethod]HandlerFunc
    stateStore StateStore
}
```

**吸收价值**：
- 技能从“静态定义”演进为“可迭代开发、人机协作审查”的动态资产。
- 无状态设计便于水平扩展和故障恢复。
- 挂起点设计保证关键变更（如工具定义修改）必须经过人工确认。

**实施位置**：`pkg/skilldev/`（新建目录）。

---

## 6. 实施优先级与风险

### 优先级矩阵

| 模式 | 优先级 | 工作量 | 依赖 | 收益 |
|------|--------|--------|------|------|
| M2 TodoManager | P0 | 中 | 无 | 直接增强 /goal 体验，用户可见 |
| M1 ModeRegistry | P1 | 中 | M2 | 与推理优化方案（工具白名单）协同 |
| M3 ExperienceEngine | P1 | 大 | 无 | 复用现有 ReasoningBank，提升 swarm_intel 实用性 |
| M5 SkillDev Pipeline | P2 | 大 | M1（模式系统） | 长期资产，短期非阻塞 |
| M4 TeamRuntime | P2 | 很大 | M1, M3 | 需要分布式基础设施投入 |

### 风险与缓解

| 风险 | 缓解措施 |
|------|----------|
| ModeRegistry 与现有 Agent 运行时耦合深 | 从“可选配置”开始，默认全工具，逐步收窄 |
| TodoManager 与 GoalTree 数据模型冲突 | TodoManager 作为 GoalTree 的“视图层”，单向同步 |
| ExperienceEngine 远程服务不可用 | 强制本地降级，与 jiuwenswarm 的 `persisted_only` 语义一致 |
| TeamRuntime 引入 gRPC 依赖 | 先用 HTTP/JSON，性能验证后再切 gRPC |
| SkillDev 挂起点增加交互复杂度 | 仅对高风险阶段（如工具定义变更）启用挂起 |

---

## 7. 参考文件

### JiuwenSwarm 侧

| 文件 | 内容 |
|------|------|
| `docs/zh/模式系统.md` | 4 模式 5 子模式的定义与配置 |
| `docs/zh/任务规划.md` | TodoToolkit 设计与使用 |
| `docs/zh/经验记忆.md` | Task Memory 工具与算法 |
| `docs/zh/分布式Team.md` | 分布式 Team 控制面/数据面分离 |
| `agents/harness/common/tools/todo_toolkits.py` | TodoToolkit 源码（文件锁、Markdown 解析） |
| `agents/harness/common/tools/memory_tools.py` | Memory 工具源码 |
| `agents/harness/common/memory/manager.py` | MemoryIndexManager（SQLite + FTS + vector） |
| `agents/harness/team/team_manager.py` | TeamManager 生命周期管理 |
| `server/runtime/skill/skilldev/service.py` | SkillDevService 无状态设计 |
| `server/runtime/skill/skilldev/pipeline.py` | SkillDev Pipeline 阶段定义 |

### claude-go 侧

| 文件 | 内容 |
|------|------|
| `pkg/swarm_intel/engine.go` | 7 阶段群体智能引擎 |
| `pkg/swarm_intel/boids.go` | BoidsCoordinator 信念空间协调 |
| `pkg/swarm_intel/pheromone.go` | PheromoneMemory ACO 信素记忆 |
| `pkg/swarm_intel/types.go` | 核心类型定义 |
| `pkg/swarm_intel/store.go` | PheromoneStore + PredictionHistory + ReasoningBank 持久化 |
| `pkg/agent/goaldecomp.go` | GoalTree + WBS + DAG Engine（/goal 基础设施） |
| `review/goal_command_design.md` | /goal 命令设计方案 |
| `review/agent_inference_optimization_plan.md` | 推理优化方案（含工具白名单、Rails） |
