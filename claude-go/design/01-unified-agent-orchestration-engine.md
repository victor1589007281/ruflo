# 01 · 统一抽象 Agent 编排引擎（AgentGraph）设计方案

> 状态：设计稿 v1（2026-07-23）· 基于 HEAD `6bd8da22f` 全量源码梳理
> 关联：[00-overview.md](00-overview.md) · [02 分层架构](02-cloud-native-layered-architecture.md)（部署边界）· [03 RL 进化引擎](03-rl-evolution-engine.md)（轨迹/注入挂载点）

---

> ## ⚠️ 实现状态核查（2026-07-25）
>
> 本文件是**设计稿**，不是状态报告。下方每个小节已按当前代码（HEAD `9697f13c0`）标注实测状态。
> 独立核查方法：不采信 `PROGRESS.md` 的自评，逐条到代码追调用链；每条论断带 `file:line`。
>
> **图例**：✅ 已实现且生产接线 · 🟠 部分实现 · 🟡 已建成未通电（代码完整但生产零调用）· ❌ 未实现 · 📄 文档失真（实现成了别的样子或描述已过期）
>
> **关键前提**：区分「写了」「通电」「部署」三层。本目录全部实现落在 2026-07-24/25，而线上二进制构建于 07-17 ——
> **截至核查时一行都未上生产**（305 个团队目录中 0 个有 `graph-journal`，`~/.claude-go/statestore` 目录不存在）。
>
> **总体判定：≈20% 实现**（分母 = §4 的 12 个核心抽象 + §5 的 15 个 mode 模板 + §6 的 26 行覆盖矩阵 = 53 条，加权 10.5）。
> 分段：§4 ≈15%（只有 4.2 的 DAG 调度与 4.3 的 Journal 重放真正生产接线）· §5 = **0/15**（图模板库目录都不存在）· §6 ≈17%（26 行里真达成 1 行）。
> 按里程碑：M0 ≈95% · M1 ≈70% · M2 ≈12% · M3 ≈4% · M4 = 0%。
>
> **一句话**：已完成的绝大部分是 M0 的「止血 + 清理」与 M1 的「内核骨架」，而不是本文的目标架构。
> `pkg/graph` 内核质量不错且真实 LLM E2E 通过，但**生产零流量**：无内置工作流声明 `Mode:"graph"`，37 个线上动态工作流无一使用，灰度开关 `CLAUDE_GO_GRAPH_ENGINE` 未设。
>
> 逐条依据与改判清单见 [PROGRESS.md](PROGRESS.md) 的「三方独立核查」小节。

## 一、背景与问题诊断

claude-go 当前的编排能力分散在**三套各自独立的引擎**里，外加两份黑板实现、四处进度状态、三套 hook 机制。功能是全的（30+ 工作流、15 种执行 mode、检查点恢复、watchdog、门禁、refine），但每加一种新 mode 要改多处、每套引擎各写一遍调度/恢复/背压，且已产生真实缺陷。

### 1.1 三套编排引擎并存

| 引擎 | 位置 | 职责 | 问题 |
|---|---|---|---|
| Coordinator pipeline 恢复 | `pkg/agent/coordinator.go:365`（`runPipelineWithRecovery`） | 检查点恢复 + 依赖调度 + 重试 | 只覆盖 pipeline 形态 |
| WBS DAG 编排器 | `pkg/agent/orchestrator.go`（263KB 巨类，`Execute` 在 `:3997`） | planner 计划→TaskNode DAG→执行 | 停滞恢复/孤儿节点各写一套（`:4016-4030`、`:4055-4083`） |
| 通用 DAG 引擎 | `pkg/orchestrator/engine.go`（`Run` 在 `:229`） | K8s 风格 Filter→Score→Dispatch | 与前两者职责重叠，`workflow_orchestrated.go:1-24` 注释自认"两套调度系统" |

### 1.2 已知缺陷清单（重构靶点，均有 file:line）　　**[✅ 已作为靶点修复（部分）]**

> **实测**：双 mode switch 已收敛为 `dedicatedExecutorModes` 单一真源（`pkg/agent/workflow.go:72-89` + `pkg/agent/coordinator.go:200`，含守护测试）；嵌套重试已加硬上限。其余缺陷见各节。

1. **双 mode switch 不同步**：`Coordinator.RunWithRecovery`（`coordinator.go:195-221`）与 `WorkflowExecutor.Execute`（`workflow.go:347-380`）各维护一份模式表。`app_composite` / `game_composite` / `fanout` 只在后者有专用执行器（`workflow_app_composite.go:84`、`workflow_game_composite.go:78`），在前者落 default → `runPipelineWithRecovery`（`coordinator.go:218`），**专用跨团队编排被静默降级为普通 pipeline**。历史上加 plot-simulate/plot-predict 也踩过同一坑（两处都要加）。
2. **黑板双实现**：`pkg/agent/blackboard.go` 与 `pkg/orchestrator/blackboard.go`，orchestrated 模式要跨两者手工同步（`workflow_orchestrated.go:365-373`）。
3. **进度状态四源**：`checkpoints.json`（Coordinator）、`goals.json`（GoalTree）、V2 `TaskStore`（tasks.json）、`pkg/orchestrator` CheckpointStore。恢复逻辑互相打补丁（`coordinator.go:380-398` 处理阶段改名残留）。
4. **嵌套重试放大**：Coordinator `executeStageWithRetry`（`coordinator.go:516`，maxRetries=6，`teams.go:627`）包裹 `WorkflowExecutor.executeStageWithRetry`（`workflow.go:885`，内部**无界 `for attempt++`**）；且默认值不一致（`coordinator.go:134` 默认 2 vs `teams.go:627` 设 6）。
5. **角色→工具靠脆弱子串匹配**：`profileForTeamRole`（`feishu/session.go:376-398`）用 `strings.Contains(role,"build")` 判 Bash 授权（`world-builder` 误命中 Coding、任何含 `review` 的角色被强制只读）。授权语义分散三处：此处、`toolExposed`（`engine.go:169-178`）、CLI `DisableTools`（`main.go:2663-2674`）。
6. **三套 hook 并存**：`pkg/hooks`（外部 shell/gRPC/OPA hook）、`pkg/engine/internal_hook`（引擎内 Go HookChain）、`pkg/orchestrator` LifecycleHook。事件模型互不相通，编排层事件（stage 级）与引擎层事件（turn/tool 级）无法在一条链上组合。
7. **`executeWorkflow` 巨函数**（`teams.go:599-841`）：swarm 特判、Coordinator 构建、门禁、内容质量环、指标、Evolution、Dreaming、报告、记忆、通知、媒体推送十几件事塞一个函数——这正是"缺全局注入机制"的病征：横切关注点只能硬编码在主流程里。
8. **空壳与遗留**：`executeFanOut`/`executeAdversarial` 直接转调 pipeline（`workflow.go:148-156`）；`workflow.go.backup` 498KB；`orchestrator.go`(263KB)/`workflow.go`(81KB)/`roles.go`(78KB)/`teams.go`(68KB) 均为 God File。
9. **resume 判定脆弱**：`isResume` 仅凭 `Status==failed && Objective 相同`（`teams.go:551`），objective 文本任何差异即丢弃检查点全量重跑。
10. **subagent 无一等抽象**：subagent = factory 创建的隔离 QueryEngine（`teams.go:158`、`feishu/session.go:807`），无法被编排层看见、约束、计费。

### 1.3 现有能力盘点（新引擎必须全覆盖）　　**[🟠 见 §6]**

> **实测**：§6 覆盖矩阵 26 行中，真被新架构覆盖的只有 1 行（AllowedTools 双路径收敛，`pkg/engine/runner.go:278`）。

- **工作流**：~30 个静态注册（`workflow.go:104-133`）+ 动态注册 `RegisterWorkflow`（`dynamic_workflow.go:34-42`，拒绝与内置同名）+ dashboard `POST /api/workflows` 与 `workflows/*.json` 目录加载（`feishu/bot.go:494`）。
- **15 种 mode**：pipeline / fanout / adversarial / adversarial_dev / trading_debate / creative_media / novel_writing / swarm_novel / plot_simulate / plot_predict / ensemble_extract / review_panel / orchestrated / app_composite / game_composite。
- **团队模型**：`ProductionTeam`（`teams.go:346-379`）状态机 created/running/completed/delivered_with_remediation/failed/stopped/refining；Mailbox；RefineHistory/PendingFeedback；`WaitDone`；`loadPersistedTeams` 重启恢复（`teams.go:1785-1808`）。
- **黑板**：5 类条目（context/decision/artifact/result/progress，`blackboard.go:150`）、`SnapshotForRole` 预算截断（`:176`）、`HandoffContext` 阶段交接（`:311`）、500ms debounce 落盘（`:62-87`）。
- **控制**：角色级差异化超时 `computeStageTimeout`（`workflow.go:1338`）、指数退避+full jitter+限流慢退（`coordinator.go:551-576`）、双层 watchdog（activity 5min / 进展 10min，`coordinator.go:155-161,264`）、检查点恢复与 `InvalidateCheckpoints`（refine 增量重跑，`coordinator.go:782`）。
- **门禁**：产码工作流 compile/test/consistency fail-closed（`teams.go:707-722`）；`delivered_with_remediation` fail-open 交付（`teams.go:686-696`）；内容质量门 LLM 0-100 分阈值 75 触发重做（`content_gate.go:20`）；WBS 任务级 `BlockingPolicy`（`orchestrator.go:117,212-213`）。
- **目标分解三套**：GoalTree HTN（`goaldecomp.go:111,198`）、`ParsePlanToDAG` WBS（`orchestrator.go:312-329`）、swarm LLM 动态分解（`swarm.go:523`）。
- **产物约定**：`REPORT.md`（`teams.go:1684-1723`）、`NOVEL.md` 等域产物、`team.json` 快照。
- **任务机制**：`RunTeam` 异步 + `WaitDone` 可选同步（`teams.go:536-596`、`team_commands.go:57-79`）；:7777 只读 dashboard 把动作写队列目录 `.dashboard/actions/*.json` 由 :18080 主进程消费（`extra_handlers.go:1089-1094`、`server.go:382-395`）。
- **进化挂点**：经验注入 `workflow.go:759`、轨迹记录 `:825-838`、团队学习 `teams.go:786-792`（详见 design/03）。

---

## 二、业界方案与论文调研

| 来源 | 采纳的核心思想 |
|---|---|
| **LangGraph** | 图=节点+条件边；checkpointer 把每步状态持久化，支持中断/恢复/时间旅行；`interrupt` 人机协同。→ 我们的 GraphRun 事件溯源 + resume |
| **Temporal（durable execution）** | 控制面记录事件历史，worker 无状态重放恢复；活动（activity）幂等重试与 workflow 决定论分离。→ Journal 重放式恢复，取代四处检查点；2026 年业界共识是「为 LLM 工作负载定制的 durable execution 层」（token 预算一等公民、重放时上下文压缩） |
| **Anthropic《Building Effective Agents》** | prompt chaining / routing / parallelization / orchestrator-workers / evaluator-optimizer 五模式可组合。→ 15 种 mode 全部化归为图模板 |
| **OpenAI Agents SDK / Swarm** | handoff = agent 间控制权转移是一等原语。→ 边上的 `handoff` 语义（携带上下文裁剪策略） |
| **MetaGPT** | SOP 固化为流程、publish-subscribe 消息池。→ 黑板统一为发布订阅接口 |
| **AutoGen / AgentScope** | 会话式多 agent、分布式 actor 化 runtime。→ AgentRuntime 接口的远程实现 |
| **GPTSwarm（ICML'24）** | agent 系统=可优化的计算图，节点/边可被学习调整。→ 图模板作为 design/03 工作流进化器的搜索空间 |
| **AFlow（ICLR'25）/ ADAS** | 用 MCTS/元 agent 在代码空间搜索工作流。→ 图 DSL 必须是纯数据、可序列化、可变异 |
| **A2A / ACP 协议** | 远程 agent 的能力发现、任务委派、全双工流式。→ RemoteRuntime 的线协议参考（详见 design/02） |
| **GoalfyMax（arXiv:2507.09497）** | 协议驱动多 agent + 经验层分离。→ 编排层与进化层解耦、经验作为独立层 |
| **事件驱动企业编排（arXiv:2606.20058）** | 事件总线为中枢的 agent 编排可横向扩展。→ EventBus 抽象 |
| **本仓 docs/workflow-engine-design.md** | 既有内部设计已提出 Task/Graph/Scheduler/Blackboard/Checkpoint 五抽象与对抗循环、背压、动态扩展——本方案继承其概念框架，将其落到"收敛三引擎"的具体迁移路径上，并补齐它未覆盖的：节点=Agent 的统一模型、hook 总线、远程 runtime、全局注入 |

**设计原则**（从上述提炼）：

1. **图是纯数据**（可序列化 JSON/YAML），执行器是解释器——动态注册、LLM 生成、进化变异共用同一表示。
2. **一切皆 AgentNode**：LLM agent、确定性门禁、路由器、子图、人工确认，统一节点模型，只是策略（policy）不同。
3. **单一分发点**：mode 不再是 switch 分支，而是"模板展开成图"的编译过程；执行器只认图，不认 mode。
4. **恢复=重放**：append-only 事件日志是唯一进度真源。
5. **横切关注点走拦截器**：预算、限流、观测、进化采集不进主流程。

---

## 三、目标与非目标

**目标**
- G1 三引擎收敛为一个 AgentGraph 引擎；双 mode switch、双黑板、四状态源全部归一。
- G2 节点统一为 Agent，节点自带：hook、loop、rule/约束、skill 选择、subagent 派生能力。
- G3 通信机制（黑板/信箱/事件流）与任务机制（提交/排队/持久化/恢复/等待）抽象成接口。
- G4 远程 agent 管理：节点可声明放置约束，经 RuntimeRegistry 调度到本地或远程 runtime（接口在本文，网络化在 design/02）。
- G5 全局注入：Interceptor 链让预算管理、进化采集、观测等以插件注入，`executeWorkflow` 巨函数瘦身为图模板+拦截器。
- G6 现有全部功能行为等价迁移（第七节覆盖矩阵），外部接口（CLI 命令、飞书命令、dashboard API、文件产物约定）不变。

**非目标**
- 不重写 QueryEngine 内核 agent loop（`pkg/engine` 保留，作为节点的执行内核）。
- 不在本方案内定义跨机网络协议（design/02 负责）。
- 不改变 LLM 供应商接入方式（`pkg/api.Client` 保留）。

---

## 四、核心抽象

### 4.1 一切皆 AgentNode　　**[🟠 Kind 5/8 已实现 / router·subgraph·human 仍缺]**

> **实测**：`NodeKind` 只定义 5 个常量（`pkg/graph/spec.go:39-47`，router/reduce/loop-group 连常量都没有），`Validate` 只接受 `agent|gate`，其余 6 种**显式报错拒绝**（`pkg/graph/validate.go:32-39`）。✅ **map/reduce/loop-group 三种已实现（2026-07-25）**：新增 `pkg/graph/fanout.go`（map 扇出 + reduce 汇聚）与 `expand.go`，`Validate` 相应放开——但**未实现的 Kind 仍明确报错**，不是放开成静默接受。现为 5/8；router/subgraph/human 仍缺——但**它们不是 §五 的阻塞项**：15 个 mode 逐个核实后没有一个真的需要它们（设计文档写 composite 需要 `subgraph`，源码里 `app_composite`/`game_composite` 从不调 `RunTeam`/`TeamManager`，全部在一个 executor 内跑；条件边已能表达 router 的分支路由）。✅ **`AgentSpec` 三个字段已通电（2026-07-25）**：`Deterministic` 由 compile/test/build gate 派生并被 `runGate` 消费；`ToolProfile`/`MaxTurns` 经新增的 `NodeExecHints` ctx 载体下推——`MaxTurns` 通过包装 factory 改写 `ResolvedConfig` 真实生效（必须在 factory：`runAgent` 会在调用前覆盖该 ctx 值），`ToolProfile` 在飞书 runner 侧**优先于角色名子串推断**（退役了 `world-builder` 因含 "build" 被判 coding 档拿到 Bash 那个真实误判）。

```go
// pkg/graph/spec.go —— 图与节点是纯数据，可 JSON 序列化
type GraphSpec struct {
    Name     string            `json:"name"`
    Version  string            `json:"version"`
    Params   map[string]ParamDef `json:"params,omitempty"`   // {objective} 等占位参数声明
    Nodes    []NodeSpec        `json:"nodes"`
    Edges    []EdgeSpec        `json:"edges"`
    Policies GraphPolicies     `json:"policies,omitempty"`   // 图级默认：预算/超时/并发/门禁
    Meta     GraphMeta         `json:"meta,omitempty"`       // ProducesCode/QualityGate 等声明式元数据（继承 workflow.go:258-262 的教训：门禁凭元数据不凭名字白名单）
}

type NodeSpec struct {
    ID    string   `json:"id"`
    Kind  NodeKind `json:"kind"` // agent | gate | router | map | reduce | subgraph | human | loop-group
    // —— 节点即 Agent：所有 Kind 共享同一 Agent 载体，差异只在策略 ——
    Agent AgentSpec `json:"agent"`

    Loop        *LoopPolicy    `json:"loop,omitempty"`        // 节点级循环（见 4.4）
    Constraints *ConstraintSet `json:"constraints,omitempty"` // 规则/约束（见 4.6）
    Hooks       []HookBinding  `json:"hooks,omitempty"`       // 节点级 hook（见 4.5）
    Budget      *BudgetSpec    `json:"budget,omitempty"`      // token/时间/成本预算
    Retry       *RetryPolicy   `json:"retry,omitempty"`       // 唯一一层重试（消灭嵌套重试）
    Timeout     *TimeoutSpec   `json:"timeout,omitempty"`     // 吸收 computeStageTimeout 角色差异化
    Placement   *Placement     `json:"placement,omitempty"`   // 远程调度约束（见 4.9）
    Expand      *ExpandSpec    `json:"expand,omitempty"`      // 动态展开为子图（见 4.3）
}

type AgentSpec struct {
    Role         string        `json:"role"`                  // → RoleRegistry
    Prompt       string        `json:"prompt,omitempty"`      // 模板，支持 {objective}/{prev_result}/{user_feedback}/{blackboard.*}
    Model        ModelPolicy   `json:"model,omitempty"`       // 别名/tier/温度；沿用 modelconfig 优先级 role>plan>global
    ToolProfile  string        `json:"tool_profile,omitempty"`// 显式声明，替代 profileForTeamRole 子串匹配
    Skills       SkillSelector `json:"skills,omitempty"`      // 见 4.7
    MaxTurns     int           `json:"max_turns,omitempty"`
    Deterministic *DetSpec     `json:"deterministic,omitempty"` // gate/router 可以是纯代码策略（零 LLM）
}
```

**"节点都是 Agent"的统一语义**：`Kind` 只决定节点的**策略形态**，不引入第二套执行路径：

| Kind | 策略 | 覆盖现状 |
|---|---|---|
| `agent` | LLM agent（QueryEngine 隔离实例） | 所有 stage（`executeStage`） |
| `gate` | 确定性代码（编译/测试）或 LLM 评分（content gate），输出 pass/fail/score | `validation_gate.go`、`content_gate.go:20`、toolskill 质量门 |
| `router` | 条件边选择（确定性表达式或 LLM 意图判断） | mode 内的分支特判、IntentRecognizer |
| `map` / `reduce` | 对集合展开 N 个并行子节点 / 聚合 | fanout、ensemble_extract、review_panel 评审团、swarm 并行预测 |
| `subgraph` | 引用另一 GraphSpec（参数化实例化） | app_composite/game_composite 跨团队编排、novel-v3 章节子流程 |
| `human` | 挂起等待外部输入（飞书确认/refine 反馈） | PendingFeedback、refine 循环、goal 授权 |
| `loop-group` | 一组节点的循环容器 | adversarial 轮次、Rounds 字段 |

gate/router 设 `Deterministic` 时零 LLM 调用——但仍走同一节点生命周期（hook、预算、轨迹），所以进化引擎能看到全部决策点。

### 4.2 Graph：DAG + 条件边 + 动态展开　　**[🟠 DAG ✅ / 条件边可表达 / 动态展开 ✅]**

> **实测**：ready-set 并行调度真实且生产接线（`pkg/graph/engine.go:183-252`）。条件边实现为**自研极简条件**而非设计写的 CEL（`pkg/graph/condition.go:54-111`，仅 ok/fail/score/contains 四类）。✅ **可表达性已通电（2026-07-25）**：新增 per-workflow 覆盖表（`RegisterGraphOverride` / `LoadGraphOverridesJSON` / `CLAUDE_GO_GRAPH_OVERRIDES`，fail-open）让条件边/Loop/Retry/Timeout 可从生产输入表达，且 `stageDeclaredOverride`+`mergeStageOverride` 是预留接缝（将来给 `StageDef` 加字段只需改前者）。**条件绝不从阶段名推断**——那等于把设计明确否定的"名字白名单"搬进调度器，且猜错会静默跳过整个分支。✅ **`ExpandSpec` 已实现（2026-07-25）**：`pkg/graph/expand.go`。**边界是必需而非可选**——展开深度/单次节点数/总节点数三重上限，设计明确要求"有界展开"因为无界会被 LLM 产出打爆；展开节点继承或收窄父约束、**不能放宽**（§4.2 单调性）。事件溯源的关键取舍：`graph.expanded` 入 journal 且 **Replay 按事件重建已展开的图而不是重新问 runner**——上游产出来自 LLM，重问会得到与首跑不同的图，事件溯源就失效了；另有 `graph.expand_rejected` 单独记账，有它才能解释"模型给了子图但图没变大"。

```go
type EdgeSpec struct {
    From      string   `json:"from"`
    To        string   `json:"to"`
    Condition string   `json:"condition,omitempty"` // CEL 表达式：output.score >= 75 / verdict == "pass"
    Handoff   *Handoff `json:"handoff,omitempty"`   // 交接语义：上下文裁剪策略（摘要/全文/黑板键列表）
}
```

- **拓扑执行**：ready-set 驱动（依赖满足即调度），天然并行，替代手写 `executePipeline` 反复扫描（`workflow.go:464-524`）与 `filterParallel`。
- **条件边**：吸收 adversarial "评分不过重做"、门禁 fail 分支、trading_debate 多空裁决等 mode 内 if-else。
- **动态展开（ExpandSpec）**：节点执行产出 `[]NodeSpec+[]EdgeSpec` 追加进运行图（有界：深度/节点数上限）。统一收编三套目标分解：
  - GoalTree HTN 分解（`goaldecomp.go:111`）→ planner 节点展开子目标节点；
  - WBS `ParsePlanToDAG`（`orchestrator.go:312`）→ planner 节点展开 TaskNode 图（Provides/Requires/ConflictKeys 进 ConstraintSet）；
  - swarm 动态分解（`swarm.go:523`）→ decompose 节点展开分层并行子图。
- **子图**：`subgraph` 节点引用命名 GraphSpec，参数注入。composite 类 mode 从"专用执行器"降级为普通子图组合——**双 switch 不一致问题从根上消失**（不再存在第二张模式表）。

### 4.3 执行模型：GraphRun + 事件溯源 Journal　　**[🟠 事件 17/16 ✅ · InvalidateFrom ✅ / 四源归一属 M4]**

> **实测**：FileJournal + Replay 真实且有零重跑硬证据（`pkg/graph/engine_test.go:422-423`）。但**「取代四源」未达成**：checkpoints.json/goals.json/tasks.json/orchestrator CheckpointStore 全在，journal 是**第五源**。✅ **事件类型已补齐并超出设计（2026-07-25）**：设计列 16 种，现有 17 种——补上了 `map.expanded`/`graph.expanded`/`graph.expand_rejected`/`loop.group.iteration`/`subgraph.spawned`/`subgraph.rejected`/`budget.consumed`/`budget.exceeded`/`node.invalidated`。
>
> ✅ **`InvalidateFrom` 已实现**：`pkg/graph/journal.go`，refine 改为追加 `node.invalidated` 事件而不是删快照文件（5 个测试）。**顺带修掉一个静默失效的真 bug**：灰度开关（`CLAUDE_GO_GRAPH_ENGINE`）开着时 `wf.Mode` 仍是 `"pipeline"`，于是「非 pipeline 转整体重跑」那道闸放行了按阶段精修，但它只失效 `checkpoints.json` 而 `graph-journal` 原样保留 → Resume 重放全部 `node.completed` → 零节点执行、直接返回旧产出，**用户的反馈静默消失**。失效必须连带摘掉该节点派生出的运行图形态（分片集/组轮次/展开产物/子节点缓存），否则是"失效了一半"——重跑的节点会继承上一轮的扇出与轮次进度。`InvalidateFrom` 在 runID 为空时**直接报错**而不是写一条注定被 Replay 过滤的事件（静默失败正是这个 bug 原本的形态）。
>
> ⚠️ **RunStatus 仍 3 态**（completed/partial/failed）而非设计的 8 态：`paused`/`stopped`/`refining` 在团队层已有对应状态，图层再加一套需要两层状态机对齐，属 M4。
> ⚠️ **四源归一未达成**：checkpoints.json / goals.json / tasks.json / orchestrator CheckpointStore 仍在，journal 是第五源。删除它们要动 8+ 下游平台共用的主路径，属 M4。✅ **重试单层化已落地（2026-07-25）**：`TranslateWorkflow` 现在总给出 `Policies.DefaultRetry={6,5s}`，`stageNodeRunner` 调 `ExecuteSingleStage` 时打 `WithOuterRetryDriven` 标记使内层退化——**图层负责重试、内层让位**，次数刻意与 pipeline 外层（`Coordinator.MaxRetries=6`）对齐，故总尝试数同量级而非新的乘法放大。另修正一处前提：角色差异化的**单次**超时在图模式下本来就生效，缺的是节点级天花板，现按 `computeStageTimeout×(retries+1)×2` 给预算（复用同一张角色→超时表而非再抄一份）。

```go
// pkg/graph/run.go
type GraphRun struct {
    RunID   string      // 全局唯一，即 design/03 的 trace-id 根
    Graph   GraphSpec   // 冻结快照（含展开后的动态节点）
    Status  RunStatus   // created|running|paused|completed|delivered_with_remediation|failed|stopped|refining
    Journal Journal     // append-only 事件日志（唯一进度真源）
}

// 事件类型（journal.jsonl，每团队一份）
// run.created / node.scheduled / node.started / node.output / node.completed / node.failed
// / node.retried / graph.expanded / gate.verdict / budget.consumed / hook.decision
// / human.requested / human.responded / run.paused / run.resumed / run.finished
```

- **恢复=重放**：进程重启后 `Replay(journal)` 重建 ready-set，已完成节点直接取缓存输出。取代 `checkpoints.json` + `goals.json` + `tasks.json` + CheckpointStore 四源，也修复 `coordinator.go:380-398`（阶段改名残留）与 `teams.go:551`（objective 文本比对判 resume）两类脆弱恢复——resume 凭 RunID，refine 凭 `InvalidateFrom(nodeID)` 事件。
- **refine**：追加 `human.responded` 事件 + 失效下游节点，增量重跑（行为等价 `InvalidateCheckpoints`，`coordinator.go:782`）。
- **重试单层化**：重试只存在于节点 RetryPolicy（引擎执行），瞬态错误判定与限流慢退（base 15s/cap 120s，`coordinator.go:551-576`）收编为内置 RetryClassifier。QueryEngine 内层不再自带无界重试环（`workflow.go:885` 废除）。
- **watchdog**：图级（进展检测：Journal 尾部 N 分钟无事件即停滞）+ 节点级（activity 心跳）两层，参数沿用 `coordinator.go:155-161`；停滞动作=发 hook 事件+按策略 retry/fail/notify，收编 `orchestrator.go:4055-4083` 的独立实现。

### 4.4 Loop：节点级与组级循环　　**[✅ 节点级已通电 / 组级 loop-group 已实现]**

> **实测**：节点级 LoopPolicy 实现完整且有测试（`pkg/graph/engine.go:360-382`），但 `TranslateWorkflow` 从不设 Loop、`StageDef` 也无对应字段 ⇒ **生产零产生方**。✅ **`loop-group` 已实现（2026-07-25）**：组内子图整体循环，`loop.group.iteration` 入 journal 且 **resume 按已完成轮次续跑**（幂等）。

```go
type LoopPolicy struct {
    MaxIterations int    `json:"max_iterations"`          // 硬上限，必填（无界循环违法）
    Until         string `json:"until,omitempty"`          // CEL 退出条件：gate.score >= 75
    DrainRounds   int    `json:"drain_rounds,omitempty"`   // 连续 N 轮无新产出即退出（loop-until-dry）
    Feedback      string `json:"feedback,omitempty"`       // 每轮回灌上一轮产出的模板
}
```

覆盖：adversarial `Rounds`、content gate 重做环（`teams.go` 内容质量环）、novel-v3 章节循环、refine 多轮、evaluator-optimizer 模式。`loop-group` 容器把"生成→评审"两节点整体循环，即对抗模式的标准化表达。

### 4.5 Hook 总线：三套合一　　**[🟠 已通电为观测桥 · 外部 hook 已覆盖图模式 / 归一属 M4]**

> **实测**：`HookBus` 骨架存在（`pkg/graph/hooks.go:16-51`，仅 graph/node scope、仅 deny 被解释），✅ **HookBus 已通电（2026-07-25）**：`teamGraphHooks` 把节点 pre → `TaskRunning` 占位 + `team.Stages` 增量刷盘 + 心跳，post/failure → 终态 StageResult + 刷盘 + 阶段指标（4 个 label 与 `recordStageMetrics` 完全一致，既有 dashboard 不受影响）。**绝不返回 deny**——灰度期观测桥不该新增阻塞路径。这同时补上了此前"图路径无 stage 级增量刷盘与指标、灰度打开后 dashboard 看不到进度"的缺口。三套原物全在：`pkg/hooks/`、`pkg/engine/internal_hook/`（22 文件）、`pkg/orchestrator/hooks.go:20`。
>
> ⚠️ **一处此前标注偏保守，已核实更正**：读者容易从"三套并存"推断出「外部 hook 在图模式下不生效」——**不是这样**。逐跳核实过调用链：图路径 `stageNodeRunner.RunNode`（`graph_adapter.go:748`）→ `ExecuteSingleStage` → `we.factory`（`workflow.go:1591`），而生产 factory 就是 `SessionManager.CreateAgentRunner`（`feishu/session.go:1030`，见 `feishu/worker_runtime.go:17`），它造出的 `sessionAgentRunner.Execute` 会触发 `ExecuteSubagentStartHooks`/`ExecuteSubagentStopHooks`（`session.go:1137`/`:1263`）。**所以用户配置的 shell/HTTP/gRPC/OPA hook 在图模式下照样触发**，因为它们挂在 runner 内部而图引擎调的就是这个 runner。
>
> 于是 §4.5 的真实剩余工作是**架构归一而非能力缺失**：三套实现的合并卡在删除 `pkg/orchestrator`（它那套 LifecycleHook 要被 `graph|node` 作用域取代），属 M4。设计文档里 `HookEvent` 的完整形态（turn/tool/session 作用域、mutate/approve 决策、Mutation 载荷）在 v1 骨架里仍是子集。

```go
// pkg/graph/hooks.go —— 统一事件模型，双维度：作用域 × 相位
type HookEvent struct {
    Scope   HookScope // graph | node | turn | tool | session
    Phase   string    // pre | post | failure | idle | stalled | budget-exceeded ...
    RunID   string; NodeID string; TurnID string        // trace 链
    Payload map[string]any
}
type HookDecision struct { Action string /* allow|deny|block|approve|mutate */; Reason string; Mutation map[string]any }

type Hook interface { Match(HookEvent) bool; Execute(context.Context, HookEvent) (HookDecision, error) }
```

**三套现存机制变成同一总线上的三类 Hook 实现**：

| 现状 | 归宿 |
|---|---|
| `pkg/hooks` 外部 shell/HTTP/gRPC/OPA hook，事件 PreToolUse/Stop/SubagentStart/TeammateIdle/TaskCompleted…（`hooks.go:12-19`） | `ExternalHook` 适配器：配置格式不变，事件名映射到 Scope×Phase；决策语义 deny/block/approve 保留（`hooks.go:100-110,206-222`） |
| `pkg/engine/internal_hook` HookChain（AutoCompact/MemoryInject/ToolGate/LoopDetector/CircuitBreaker…，`engine.go:275-348`） | 保留在引擎内（性能敏感、每 turn 触发），但注册进同一总线的 `turn|tool` 作用域，事件对图层可见 |
| `pkg/orchestrator` LifecycleHook（ExpanderHook/MetricsHook/心跳，`workflow_orchestrated.go:119-131`） | 直接被 `graph|node` 作用域取代 |

- **每个节点集成 hook**：NodeSpec.Hooks 绑定节点级 hook；图级 Policies.Hooks 对全部节点生效；hook 本身也可以是 agent（`HookBinding{NodeRef}` 指向一个 gate 节点）——审批型 hook 即 human 节点的语法糖。
- **fail-open/fail-closed 显式化**：每个 HookBinding 声明 `on_error: ignore|block`，取代当前 PreToolUse 隐式 fail-open（`hooks.go:96-98`）。

### 4.6 规则与约束：ConstraintSet 单一真源　　**[✅ 已实现并接线]**

> **实测（改造前）**：`ConstraintSet` 全仓零命中，约束散在三处：`pkg/feishu/session.go:421-443` 子串匹配（`world-builder` 命中 Coding 的老坑）、`pkg/engine/engine.go:185`、`cmd/claude-go/main.go` DisableTools。
>
> ✅ **已实现并接线（2026-07-25）**：`pkg/agent/constraints.go`（46 个测试）。核心是 **`Narrow` 单调收窄——放宽必须被拒绝**，这是安全属性而非便利功能。三处决策点现从同一真源读；`NarrowedPermissionMode` 让权限档位也只能收紧（阶梯 `bypass<acceptEdits<auto<default<dontAsk<plan`，**空串按 default 计**，否则 `bypass` 声明会被当成收窄接受 = fail-open）。
>
> 关键设计取舍（与设计文档的偏离，已核实必要）：
> - **档位是偏序不是全序**。六档工具集真的互不包含（`research` 有 WebFetch/WebSearch 而 `coding` 没有；`analysis` 有网络而 `team` 没有），所以不能用 int 等级比"谁更严"。改用特权位子集判定；两档不可比时 `Narrow` **拒绝而不猜**（fail-closed）。特权位表用真实注册表双向反查，`profile.go` 加工具忘同步就红。
> - **`pkg/engine` 与 `pkg/agent` 零互相 import**：engine 只定义最小接口 `ToolConstraintSource`，`*agent.ConstraintSet` 结构性满足，编译期断言放在 `pkg/feishu`（全仓唯一同时 import 两者的包）。engine 是热路径底层包，反向依赖 agent 那棵重依赖树是倒挂。
> - **`Allow` 的 nil ≠ 空集，且故意不加 `omitempty`**：空集经 JSON 往返若被省掉，"全部拒绝"会静默变"不限制"。有专门往返测试。
> - **向后兼容**：`profileForTeamRole` 子串匹配一行没搬没抄，降级为 `ResolveToolProfile` 的 fallback（6 个下游平台依赖它，不能一刀切删），可被显式声明覆盖且用回退时打 deprecation 日志（按 role+profile 去重、**上限 512 条**——`nested-agent:<SubagentType>` 来源是模型可控字符串，无上限就是慢性内存泄漏）。20 组 (role, workflow) 逐项等价有测试。
> - **嵌套 agent 只采纳"更窄的"节点声明**：直接覆盖会放宽——节点声明 `coding` + `opts.ReadOnly=true`（本该 research）会让只读子代理拿到 Shell。
>
> ⚠️ **仍未接的最后一跳**：`cmd/claude-go/main.go:2877` 的 CLI DisableTools 尚未改从 ConstraintSet 读；且 `main.go:715` 的 slash 直通防护目前靠嗅探 `eng.Config.AllowedTools == nil` 判断"是否受限会话"——拿 map 的 nil 性当权限标志，应改读 `ConstraintOrigin`。另 `Paths`/`ModelTier`/`Conflicts` 有类型有单调性有校验但**运行期还无消费者**（`Provides/Requires` 仍活在待删的 `pkg/agent/orchestrator.go` WBS 里）。

```go
type ConstraintSet struct {
    Tools       *ToolConstraint  `json:"tools,omitempty"`       // allow/deny 列表；nil=继承 profile，空集=全拒（沿用 engine.go:125-131 语义）
    Paths       *PathConstraint  `json:"paths,omitempty"`       // 读写路径沙箱
    Permission  string           `json:"permission,omitempty"`  // bypass|plan|acceptEdits|...
    Conflicts   []string         `json:"conflict_keys,omitempty"` // 互斥资源键（收编 WBS ConflictKeys）
    Provides    []string         `json:"provides,omitempty"`    // 契约（收编 WBS Provides/Requires）
    Requires    []string         `json:"requires,omitempty"`
    Blocking    string           `json:"blocking,omitempty"`    // fail_open|fail_blocks_dependents（orchestrator.go:117）
    ModelTier   *ModelBound      `json:"model_tier,omitempty"`  // 允许的模型档位上下限
}
```

- **编译期校验**：`GraphSpec.Validate()` 检查死锁（不可达节点/循环依赖）、约束冲突、占位符未绑定——吸收 docs/dynamic-workflow-design.md 的"拆地雷"结论。
- **运行期强制单点**：约束编译为 QueryEngine 的 `AllowedTools`/`PermissionMode`/hook 配置下发。`toolExposed`（`engine.go:169-178`）保持唯一执行点；`RunIsolated` 的 `gateToolUses` 复刻（`runner.go:247`）改为调用同一函数。**角色→工具不再子串匹配**：RoleDef 增加显式 `tool_profile` 字段，`profileForTeamRole` 仅作为缺省回退并打 deprecation 日志。
- **slash 直通防护**保留：受限会话不进 slash 分发（`main.go:700-703`），在图层同样成立——受限 GraphRun 无法展开携带更高权限的子图（子图约束只能收窄不能放宽，**约束单调性**）。

### 4.7 Skill 选择与编排　　**[✅ 已实现并接线]**

> **实测（2026-07-25 实现）**：`pkg/agent/skill_selector.go` 落地 `SkillSelector`，并已接进 `roles.go` 的注入点（`SetSkillSelector`/`NewSkillSelectorForRoles`，**opt-in，默认回落静态逻辑**——否则又是一个"已建成未通电"）。修掉原逻辑的两个问题：①`RecommendedSkills` 只按角色名索引、**完全不看本次 objective**，同一角色不论做什么拿到的技能永远一样；②预算是"条数×单条上限"而非总量，各处上限彼此独立、实际注入量无处可见。现在是**显式声明 > 角色推荐 > objective 关键词相关性补选**，用总量预算取代条数上限，被挤掉的记进 `Dropped`（此前截断是静默的）。取舍：纯确定性打分不引入 LLM（选技能若要调 LLM 就与它要解决的省 token 自相矛盾）；同分按名字升序保证 prompt 可复现否则缓存失效；中文按双字滑窗切分（本仓无 embedding 端点）。11 个测试。

```go
type SkillSelector struct {
    Static    []string `json:"static,omitempty"`     // 显式指定（RoleDef.Skills 等价）
    Recommend bool     `json:"recommend,omitempty"`  // 按项目 profile 推荐（roles.go:338,367 收编）
    Query     string   `json:"query,omitempty"`      // 运行期按任务语义检索（对接 design/03 技能库检索）
    MaxCount  int      `json:"max_count,omitempty"`; MaxChars int `json:"max_chars,omitempty"` // 注入预算（roles.go:105-159 截断语义）
}
```

- 注入路径不变：合并进 `<role_skills>` 块（`MergedPrompt`，`roles.go:105-159`）；Skill 工具按需取全文。
- **编排能力**：skill 可声明 `graph:` 段——技能不仅是提示词，还能携带一个子图模板（如 mr-chain 类多阶段技能），Skill 选择即子图注入。这为 design/03 的"skill 进化=图模板进化"铺路。

### 4.8 Subagent 派生　　**[🟠 图内派生 ✅ / 收编飞书裸 QueryEngine 属 M4]**

> **实测（改造前）**：`SpawnSubgraph` 零命中；subagent 是节点内 agent 自己造的裸 QueryEngine（`pkg/feishu/session.go` `runNestedAgent`、`cmd/claude-go/main.go`），**对编排层完全不可见**——没有 NodeID、不进 Journal、不受 hook/预算/轨迹覆盖，一个节点可以在里面烧掉任意多 token 而图这一层什么都看不到。
>
> ✅ **`SpawnSubgraph` 已实现（2026-07-25）**：`pkg/graph/spawn.go`（19 个测试）。设计要的三样「对编排层可见」不是额外写的，是**复用同一个 `rc`** 白拿的：子图节点经同一个 `rc.nodeExec` 执行 ⇒ 自动过同一条拦截器链（预算把子图开销记在父运行头上）、自动进同一份 journal（NodeID 形如 `<父>~sp<指纹>/<子>`）、自动受同一套 hook 覆盖。有测试直接断言「父 1 次 + 子图 2 次 = 预算台账 3 次」与「预算满了能掐住子图」。
>
> 与 §4.2 动态展开的分工：展开是 planner 节点**跑完后**把分解结果并入当前 run（异步、改图）；spawn 是节点**执行期间**由 agent 工具调用派生、同步等结果（不改父图）。边界闸的理由相同（内容来自 LLM 产出），故 spawn 复用 expand 的 `narrowToParent` 做单调收窄，不另写一套。
>
> **命名空间用请求内容指纹而非序号**——这是个隐蔽坑：用序号时父节点重跑序号从 0 重来，而 agent 这次可能请求了**不同**的子图，同一个 `~sp0/` 下 resume 缓存会把上次产出错配给这次。指纹方案下请求相同则命中缓存（省真金白银的 LLM 调用）、不同则命名空间不同不可能误命中。指纹对节点声明顺序不敏感，否则会白丢缓存。
>
> 四道闸：授权（未声明 `Spawn` 则 `NodeInput.Spawn` 为 nil，授权缺失表现为"没这个能力"而非调了才报错）、深度、单次条数、派生次数（与条数是两道不同的闸——后者挡"派生 500 次每次 1 个节点"），外加运行图总量闸。拒绝一律记 `subgraph.rejected`，否则"agent 说它派生了但图里什么都没有"事后不可解释。`SpawnSpec` 三个上限允许 0（取缺省）但**负值在 Validate 阶段就拒**：负数会被 `<=0 取缺省` 静默当成"没设"，一个写错的 `-1` 就绕过了边界。
>
> ⚠️ **被自己的测试抓出一个真缺陷**：预算原本"执行后记账"，嵌套派生时父节点在飞行中一直没被计数，子图便能借这个空档多跑几个节点，**透支量正好等于派生深度**。改为**准入即记账**（判定与记账在同一把锁里，否则 N 个并发节点会同时通过"还差 1 个配额"的判定，并发度就是透支量）。
>
> ⚠️ **仍缺**：收编 `pkg/feishu` 的 `runNestedAgent` 与 `cmd/claude-go` 那两条裸 QueryEngine 路径——它们是全部会话共用的 Agent 工具主路径，替换属 M4。现状是「图内派生已可见，图外派生仍不可见」。

- 节点内 agent 通过 `SpawnSubgraph(spec, params)` 工具派生子图（受 ConstraintSet 单调性约束、计入父节点预算）。取代"factory 创建裸 QueryEngine"（`teams.go:158`、`feishu/session.go:807-830`、`main.go:2639`）——**subagent 从此对编排层可见**：有 NodeID、进 Journal、受 hook/预算/轨迹覆盖。
- 现有 `cliAgentRunner`/`sessionAgentRunner` 改为 AgentRuntime 的两个实现（见 4.9），行为不变。

### 4.9 远程 Agent 管理：AgentRuntime 接口　　**[🟠 接口与放置已落地 / 远程实现待 design/02 R3]**

> **实测（2026-07-25 实现）**：`pkg/agent/runtime.go` 落地 `AgentRuntime`/`RuntimeRegistry`/`RuntimeCaps`/`Placement`（硬约束过滤 + 软偏好打分 + 团队亲和 + 租约过期剔除），并用 `NewLocalRuntime` 把既有 `CreateAgentFunc` 收编为本地 runtime（**不改动 cliAgentRunner/sessionAgentRunner 两个既有实现**）。⚠️ **一处对设计稿的偏离**：接口定在 `pkg/agent` 而非 `pkg/graph/runtime.go`——要被收编的三个执行器都在 pkg/agent 及其上层，而 pkg/graph 是纯调度内核不认识 agent 语义，放进去会让内核反向依赖 RunMetadata/ToolProfile/团队 cwd。折中是图侧继续用 `NodeRunner`，`stageNodeRunner` 作桥，**pkg/graph 零改动**。⚠️ 澄清名字撞车：`pkg/cluster` 的 `RequireCaps` 是队列标签过滤（布尔匹配无打分），本文的 `Placement` 才是放置策略；前者是后者求解后用于跨机路由的投影。**仍缺**：k8s-job runtime（`pkg/sandbox/k8s_runner.go` 尚未接为 AgentRuntime）与远程 runtime（design/02 R3）。关键语义：团队亲和权重高于任何 Prefer（产码门禁在 `<cwd>/go.mod` 上跑，节点散落会让上一阶段的代码消失）；同分按名字升序保证放置确定性。11 个测试。

```go
// pkg/graph/runtime.go —— 本文只定义接口与调度语义；网络化实现见 design/02
type AgentRuntime interface {
    Capabilities() RuntimeCaps                 // 工具集/沙箱/浏览器/GPU/最大并发
    Execute(ctx context.Context, task NodeTask) (<-chan NodeEvent, error) // 流式事件
    Cancel(runID, nodeID string) error
}
type RuntimeRegistry interface {
    Register(name string, rt AgentRuntime, lease time.Duration)
    Heartbeat(name string) // 见 design/02 心跳系统
    Pick(p *Placement, caps RuntimeCaps) (AgentRuntime, error) // 放置：约束过滤+打分
}
type Placement struct {
    Require []string `json:"require,omitempty"` // "bash","browser","k8s-sandbox","gpu"
    Prefer  string   `json:"prefer,omitempty"`  // "local"|"remote:<name>"|"any"
    Affinity string  `json:"affinity,omitempty"`// "team"（同团队节点尽量同 runtime，共享 cwd）
}
```

- v1 内置三个 runtime：`local-cli`（收编 `main.go:2639` cliAgentRunner）、`local-session`（收编 `feishu/session.go:807`）、`k8s-job`（收编 `sandbox/k8s_runner.go` 的 K8s Job 下发，共享 PVC 工作区——现状唯一的跨机执行雏形）。
- 远程 runtime（gRPC/A2A worker 拉取模型）在 design/02 R3 落地，接口在此冻结。
- 团队 cwd 亲和：`Affinity: team` 保证产码工作流的节点落同一工作区（否则经共享存储，design/02）。

### 4.10 全局注入：Interceptor 链　　**[🟠 切面链 ✅ 已通电 / 6 个内置拦截器落地 1 个]**

> **实测（改造前）**：`NodeInterceptor`/`CallInterceptor` 两接口零命中；六个内置拦截器全无。
>
> ✅ **切面链已实现并通电（2026-07-25）**：`pkg/graph/interceptor.go` + `pkg/agent/graph_interceptors.go`（29 个测试）。
>
> **挂载点只有一处**：`engine.callRunner`——它自称"真正调 runner 的唯一出口"，实测确实是（agent/gate 的重试环与 loop 环、map 分片、loop-group 组内节点全部经它）。挂这一处就自动覆盖全部 Kind 与全部重试/循环轮次，有测试专门证明「重试每一轮都过链」与「map 分片也过链」——嵌套调度路径若绕过链，扇出就是预算黑洞。
>
> **链语义定得比设计稿严**，因为拦截器是第三方注入点（设计文档明说 aiops 的权限桥要从此注入）：
> - `next` 必须**恰好调一次**。不调 = 节点被悄悄吞掉；调两次 = 绕过引擎的重试计数。链本身检测这两种误用并转为 failed 且**点名是哪个拦截器**，不静默容忍。
> - 但**明示拒绝**（不调 next 而返回 failed/skipped 终态）是正当用法，与"忘了调 next"可区分——预算拦截器正是走这条路径。
> - 拦截器 panic 不穿透（不带走整个图运行）但也**不当成放行**——转为节点 failed。
> - 重名/空名/nil 在开跑前就拒绝：`Name` 是 journal 归因的键，重名会让"谁拒了这个节点"永久不可考。
>
> **BudgetManager 已落地**（设计表中第一项、用户点名的例子）：节点执行次数 / 墙钟 / token / 单节点次数四道闸，`budget.consumed` 与 `budget.exceeded` 双事件入 journal——有后者才能区分"节点失败"与"没让它跑"。两个关键取舍：**判定在执行前**（执行后判定意味着预算总会被超出至少一个节点的开销，有测试断言"实际执行恰好等于上限，多一次就是透支"）；**token 报 0 = 未回报而非没花**（否则"用量回报尚未实现"会被当成"这次免费"，闸形同虚设）。
>
> **默认值的设计**：默认不限制等于没通电，默认拍一个数又会改变现状行为（8+ 下游平台在用 `:18080`）。折中是从 GraphSpec **算出**一次合法运行的执行次数真上界（节点数 × 重试 × 循环轮次，声明了 map/展开时按 `MaxTotalNodes` 算）再乘 2 倍宽裕——合法运行撞不到、失控运行会撞上。有测试逐场景验证「算出的上界 ≥ 合法运行上限」，也验证它没宽到失去意义。环境变量 `CLAUDE_GO_GRAPH_BUDGET_*` 可设紧，`CLAUDE_GO_GRAPH_INTERCEPTORS=off` 可一键退回。
>
> 一处 API 自我修正：`NewBudgetManager` 原本收一个 `evAppender` 参数，但那是**未导出类型**——外部包（装配链的 `pkg/agent`）根本造不出来，靠构造参数传等于生产上一条事件都不落。改为引擎经 `journalAware` 接口注入。
>
> ⚠️ **仍缺**：`CallInterceptor`（LLM 调用切面）零实现；6 个内置拦截器只落地 BudgetManager，EvolutionRecorder / GateEnforcer / Notifier / MetricsEmitter / RateLimiter 仍在主流程硬编码（其中 GateEnforcer 的"按 Meta 声明而非工作流名白名单"已单独落地）。`executeWorkflow` **仍是 282 行巨函数**（`pkg/agent/teams.go:646+`）——把它拆成"编译图模板 + 装配拦截器"属 M4，风险在于它是 8+ 平台共用的主路径。

```go
// 两个切面：节点执行 与 LLM 调用
type NodeInterceptor interface { Around(ctx context.Context, t NodeTask, next NodeExec) (NodeResult, error) }
type CallInterceptor interface { Around(ctx context.Context, c LLMCall, next CallExec) (LLMResult, error) }
```

标准内置拦截器（`executeWorkflow` 巨函数由此瘦身为"编译图模板 + 装配拦截器"）：

| 拦截器 | 收编现状 |
|---|---|
| BudgetManager | 图级/节点级 token+时间+成本台账；超预算→降级模型/暂停/终止（新能力，用户点名的全局注入示例）。事件 `budget.consumed` 进 Journal |
| EvolutionRecorder | 经验注入（`workflow.go:759`）+ 轨迹记录（`:825-838`）+ 团队学习触发（`teams.go:786-792`）→ 全部改为拦截器挂载，**headless 与飞书两条路径自然同构**（修复 design/03 诊断的开环） |
| GateEnforcer | 产码门禁 fail-closed（`teams.go:707-722`）与 `delivered_with_remediation` fail-open（`:686-696`）：读 GraphSpec.Meta 声明（不再按工作流名白名单） |
| MetricsEmitter | 团队/阶段指标、LLMCallRecord 关联 trace-id |
| Notifier | 飞书通知、媒体推送（从 executeWorkflow 剥离） |
| RateLimiter / CircuitBreaker | 沿用 api.Client 熔断，前移到 CallInterceptor 统一观测 |

拦截器配置在图级 Policies 或全局 settings，顺序确定、可开关——第三方横切逻辑（如 aiops 平台的权限桥）也从此注入而非改主流程。

### 4.11 通信机制抽象　　**[🟠 接口与 Watch 已落地 / 双黑板待 M4 归一]**

> **实测**：`pkg/graph/blackboard.go` 不存在。双黑板仍并存（`pkg/agent/blackboard.go:29` + `pkg/orchestrator/blackboard.go:69`），跨黑板手工同步仍在 `pkg/agent/workflow_orchestrated.go:133-136`。**设计要「新增」的 `Watch` 只存在于那份要被删的实现里**（`pkg/orchestrator/blackboard.go:210`）。Mailbox 仍是裸 slice。

```go
type Blackboard interface { // 单一接口，收编两份实现
    Put(e BoardEntry) error                       // 5 类条目沿用 blackboard.go:150
    Snapshot(role string, budget int) string      // SnapshotForRole 等价
    Handoff(from, to string) string               // HandoffContext 等价（blackboard.go:311）
    Watch(prefix string) <-chan BoardEntry        // 新增：发布订阅（MetaGPT 消息池风格）
}
type Mailbox interface { Send(MailMessage) error; Inbox(agent string) []MailMessage } // teams.go:1638 收编
```

- 默认实现=现有文件黑板（debounce 落盘，`blackboard.go:62-87`），分布式实现见 design/02。
- `pkg/orchestrator/blackboard.go` 删除，orchestrated 跨黑板手工同步（`workflow_orchestrated.go:365-373`）消失。
- 事件流：GraphRun 的 Journal 本身即对外事件流（dashboard SSE、飞书进度播报订阅之，取代 `updateHeartbeat` 回填 team.json 的轮询观测，`teams.go:1763`）。

### 4.12 任务机制抽象　　**[✅ 已实现，含动作队列消费方]**

> ✅ **已实现（2026-07-25）**：`pkg/agent/taskservice.go` 的 `FileQueueTaskService`——Submit（**幂等键**取代 `tryStartTeam` 的状态判断去重，后者有竞态）/ Wait / List / **ConsumeActions**。最有价值的一点是给一个只写不读的队列接上了消费方：:7777 动作队列此前两处写入（`pkg/dashboard/extra_handlers.go`、`v13_handlers.go`）而**全仓无消费方**，那句"等待 claude-go 主进程消费"是**假承诺**、写进去的动作烂在盘上。后端复用 `pkg/statestore`（其 `validateBucket` 禁止 `/`，桶名已扁平化）。向后兼容：`RunTeam`/`WaitDone` 签名未变，新接口与它们并存。

```go
type TaskService interface {
    Submit(spec GraphSpec, params map[string]any, opts SubmitOpts) (RunID, error) // 异步，等价 RunTeam
    Wait(runID string) (RunResult, error)          // WaitDone 等价
    Pause/Resume/Stop(runID string) error
    Refine(runID string, feedback string, fromNode string) error
    List(filter TaskFilter) []RunSummary
}
```

- 队列语义标准化：:7777 的文件队列目录（`.dashboard/actions/*.json`）升级为 TaskService 的 `file-queue` 后端（行为不变），分布式后端见 design/02。
- `team.json` 保留为**投影**（从 Journal 物化，供 dashboard/`team status` 兼容读取），不再是真源。

---

## 五、15 种 mode → 图模板映射　　**[🟠 4/15 等价且有等价性测试，默认关；余 11 逐条记账能力缺口]**

> **实测**：`pkg/graph/templates/` **目录不存在**，图模板库零落地；`TranslateWorkflow` 是 WorkflowDef 直译器不是模板库。只有 pipeline/fanout 可经灰度开关切图，而 `CLAUDE_GO_GRAPH_ENGINE` 全仓/全部署清单无处设置。13 个专用 mode 全部仍走各自执行器（`pkg/agent/workflow.go:409-434`）。§5 承诺的「fanout 首次真正实现 map→reduce」未发生——`executeFanOut` 仍原封不动转调 pipeline（`pkg/agent/workflow.go:201-203`）。

mode 消失，成为**内置图模板库**（`pkg/graph/templates/`，纯 JSON 数据 + 少量展开函数）。单一分发点=模板实例化。

| 现 mode | 图模板 | 说明 |
|---|---|---|
| pipeline / default | 线性 DAG | stages 按 DependsOn 直译；Parallel 组=同层并行节点 |
| fanout | map→reduce | 修复现状空壳（`workflow.go:148-156` 转调 pipeline）——首次真正实现 |
| adversarial | loop-group[generate→critique(gate)] | Rounds→MaxIterations；Until: score 达标。修复现状空壳 |
| adversarial_dev | loop-group[coder→reviewer(gate)→fixer] + 编译/测试 gate 节点 | 门禁从工作流名白名单改元数据声明 |
| trading_debate | map[多空 agent]→debate(loop)→judge(gate) | |
| creative_media | pipeline + 媒体产物 artifact 节点 | chromedp 渲染=deterministic gate 节点 |
| novel_writing (novel-v2) | planner(expand)→map[章节 subgraph]→assembly→review(gate) | NOVEL.md 产物约定保留 |
| swarm_novel (novel-v3) | decompose(expand)→分层并行 subgraph→assembly→editorial(gate) | 末尾编审短产出 fail 的教训→assembly 节点 Blocking: fail_open |
| plot_simulate / plot_predict | swarm 模板：Decompose→Scout→map[Predict]→Debate(loop)→Fuse(reduce) | swarm_intel 五阶段直译；信素记忆挂 design/03 |
| ensemble_extract | map[N 抽取]→reduce(投票合并) | |
| review_panel | map[评审员]→reduce(评分矩阵) | |
| orchestrated | planner(expand: ParsePlanToDAG)→动态 TaskNode 图 | WBS 契约字段→ConstraintSet；263KB Orchestrator 逐步只剩 ParsePlanToDAG 解析器 |
| app_composite / game_composite | subgraph 组合（前端图+后端图+联调 gate） | **修复静默降级缺陷**（coordinator.go:218 default 分支问题从根上消失） |

动态注册工作流（`RegisterWorkflow`/`POST /api/workflows`/`workflows/*.json`）：`WorkflowDef→GraphSpec` 有损耗为零的直译器（Stages/DependsOn/Parallel/Prompt 占位符一一对应），注册表与模板库共用命名空间、维持"拒绝与内置同名"（`dynamic_workflow.go:42`）。

---

## 六、现有功能覆盖矩阵（编排域）　　**[🟠 26 行真覆盖 1 行]**

> **实测**：⚠️ **本矩阵是计划表，不是状态表**。逐行核实后：真覆盖 1 行（#17 AllowedTools 收敛）+ 5 个半行 + 4 行只有字段无消费 ⇒ ≈17%。特别注意 #5「重启恢复」仍是强制置 failed（`pkg/agent/teams.go:1902-1904`）、#9 门禁仍按工作流名白名单（`pkg/agent/teams.go:757`）。

| 现有功能 | 位置 | 新架构归属 | 状态 |
|---|---|---|---|
| 30+ 静态工作流 | `workflow.go:104-133` | 模板库直译 | 等价 |
| 动态注册（API/目录/LLM 生成） | `dynamic_workflow.go:34`、`server.go:236-237` | WorkflowDef→GraphSpec 直译器 | 等价 |
| 团队状态机 7 态 | `teams.go:172-185` | RunStatus 同名 7 态 | 等价 |
| 检查点恢复/refine 增量重跑 | `coordinator.go:365-462,782` | Journal 重放 + InvalidateFrom | 增强（修 resume 脆弱判定） |
| 重启恢复 | `teams.go:1785-1808` | Journal 重放（running→按最后事件续跑而非强制 failed） | 增强 |
| 双层 watchdog | `coordinator.go:155-161,264` | 图级+节点级 watchdog，参数沿用 | 等价 |
| 重试退避+限流慢退 | `coordinator.go:516-598` | 节点 RetryPolicy 单层 | 增强（消灭嵌套放大） |
| 角色差异化超时 | `workflow.go:1338` | TimeoutSpec 按 role 缺省表 | 等价 |
| 编译/测试/一致性门禁 | `teams.go:707-722` | gate 节点 + GateEnforcer 拦截器 | 等价（元数据驱动） |
| delivered_with_remediation | `teams.go:686-696` | 图级 Blocking: fail_open 语义 | 等价 |
| 内容质量门 0-100/75 | `content_gate.go:20` | LLM gate 节点（分数进 Journal→design/03 奖励源） | 增强 |
| 黑板 5 类/预算快照/交接 | `blackboard.go:139-311` | Blackboard 接口默认实现 | 等价+Watch 新增 |
| Mailbox | `teams.go:359,1638` | Mailbox 接口 | 等价 |
| REPORT.md/NOVEL.md/team.json | `teams.go:1684-1757` | artifact 约定不变；team.json 变投影 | 等价 |
| GoalTree/WBS/swarm 三套分解 | `goaldecomp.go`、`orchestrator.go:312`、`swarm.go:523` | ExpandSpec 三种展开器 | 等价 |
| 三套 hook | `hooks.go`、`internal_hook`、LifecycleHook | Hook 总线（外部 hook 配置格式不变） | 等价+组合能力新增 |
| AllowedTools 双路径强制 | `engine.go:169`、`runner.go:247` | ConstraintSet→单一 toolExposed | 等价（收敛实现） |
| slash 直通防护 | `main.go:700-703` | 约束单调性 | 等价 |
| 工具 profile 4 档 | `builtin/profile.go:70-87`、`session.go:376-398` | AgentSpec.ToolProfile 显式声明 | 增强（弃子串匹配） |
| 思考型角色 maxTurns=1/DisableTools | `main.go:2663-2674` | AgentSpec.MaxTurns + Deterministic | 等价 |
| subagent 隔离执行 | `teams.go:158`、`session.go:807`、`main.go:2639` | AgentRuntime 三实现 + SpawnSubgraph | 增强（编排层可见） |
| 异步 run/WaitDone/:7777 队列 | `teams.go:536-596`、`extra_handlers.go:1089` | TaskService | 等价 |
| 并发启动去重 | `teams.go:328` | TaskService Submit 幂等键 | 等价 |
| 进化/记忆/dreaming 挂点 | `workflow.go:758-840`、`teams.go:786-792` | EvolutionRecorder 拦截器 | 增强（headless 同构，见 design/03） |
| K8s Job 执行 | `sandbox/k8s_runner.go` | `k8s-job` runtime | 等价 |
| swarm_intel 五阶段/信素/校准 | `pkg/swarm_intel` | swarm 图模板 + design/03 收编信素记忆 | 等价 |

---

## 七、代码组织

```
pkg/graph/
  spec.go        // GraphSpec/NodeSpec/EdgeSpec/ConstraintSet —— 纯数据+Validate
  templates/     // 15 mode 的模板 JSON + 展开器（goaltree.go/wbs.go/swarm.go）
  engine.go      // 调度器：ready-set + 条件边 + 动态展开（唯一执行器）
  journal.go     // 事件溯源 + 重放
  hooks.go       // Hook 总线 + ExternalHook 适配（pkg/hooks 配置兼容）
  runtime.go     // AgentRuntime/RuntimeRegistry + local-cli/local-session/k8s-job
  interceptor.go // NodeInterceptor/CallInterceptor + 内置六拦截器
  blackboard.go  // Blackboard/Mailbox 接口 + 文件实现（收编两份旧实现）
  taskservice.go // TaskService + file-queue 后端
  compat.go      // WorkflowDef→GraphSpec 直译器；team.json 投影
```

淘汰路线：`pkg/agent/coordinator.go`、`pkg/orchestrator/*`、`workflow.go` 各 mode 执行器 → M4 后删除；`pkg/agent/orchestrator.go` 缩为 ParsePlanToDAG 解析器；`workflow.go.backup`（498KB）、根目录散落 `test_*.go` 立即清理。

---

## 八、迁移路线　　**[🟠 M0 95% · M1 70% · M2 12% · M3 4% · M4 0%]**

> **实测**：M1 的验收项「kill -9 恢复重放正确」**无对应测试**（`journal_test.go` 只模拟尾部截断行，不是进程 kill）。M2 记 ✅ 的 loop/条件边应为 🟡（生产零产生方）。M4 = 0%：`pkg/orchestrator` 仍生产可达（4 个注册工作流），四大 God File 全在且 `workflow.go` 比设计稿时**更大**。

| 里程碑 | 内容 | 验收 |
|---|---|---|
| M0 止血（1 周） | ① 双 switch 一致性：coordinator 增加 app_composite/game_composite/fanout 分支（临时）；② 嵌套重试收敛：`workflow.go:885` 无界循环加上限并在恢复路径禁用内层；③ 清理 backup/散落文件 | 现有 e2e 全绿；composite 类工作流真正走专用执行器 |
| M1 图内核（2 周） | GraphSpec/engine/journal/直译器；pipeline 与 fanout 两模板切到新引擎（feature flag `graph_engine: true`） | 同一工作流新旧引擎产物 diff 等价；kill -9 恢复重放正确 |
| M2 全模板（3 周） | 其余 13 mode 模板化；gate/loop-group/expand 三能力；Hook 总线接通外部 hook 配置 | 全部内置工作流过既有 e2e；`tests/eval` 增图引擎回放用例 |
| M3 注入与约束（2 周） | 六拦截器装配；ConstraintSet 单一真源；ToolProfile 显式化 | 预算超限降级 e2e；headless 进化采集打通（配合 design/03 E0） |
| M4 运行时与收尾（2 周） | AgentRuntime 三实现；SpawnSubgraph；旧引擎删除，team.json 投影化 | 全量回归 + 双周生产陪跑无回退 |

**风险**：① 15 mode 中 novel-v3/swarm 类行为微妙（kimi compaction、短产出 fail-open 教训），模板化必须逐个对照产物验收；② Journal 写放大——沿用黑板 500ms debounce 合并；③ 外部 hook 生态兼容——配置格式冻结，仅内部事件映射。

**开放问题**：① CEL 表达式引擎选型（cel-go vs 自研极简求值器）；② 图版本升级时在跑 run 的兼容策略（冻结快照已缓解，需定义 refine 跨版本语义）；③ swarm_intel 的信素/贝叶斯融合是否下沉为通用 reduce 策略库（倾向 M4 后再议）。
