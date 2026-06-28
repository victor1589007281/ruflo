# 数据库运维智能体集群（DB-Ops Fleet）改造方案

> 目标：claude-go 内部跑一群"数据库管理运维智能体"（每类 DB 一个），自动监控 + 运维；
> **决策推理走远程模型（kimi），简单/执行类大部分操作走本地 Gemma**。
>
> 状态：设计稿（2026-06-18）。两个承重假设已分别经代码核实与本地实测确认（见 §3）。

---

## 1. 结论（TL;DR）

**当前 claude-go 部分满足：底座有，但"DB 运维集群 + 双模型接线 + 运维安全"这三块没有。**

- ✅ **够用的底座已就位**：按角色路由模型（`modelconfig`）、gemma 已是可调工具的合法 agent 主模型（ollama 暴露 Anthropic `/v1/messages`）、cron 周期触发、动态工作流、飞书告警、目标/对抗循环、swarm 编排。
- ❌ **缺的三件事**：(1) 数据库运维角色与工具，(2) "推理=kimi / 执行=gemma" 的具体接线与编排，(3) 让 agent 真在生产库上动手所必需的**安全护栏**。
- 🔑 **关键判断**："推理 vs 执行"的双模型**不是**靠"同一个 agent 中途换模型"（引擎不支持，且没必要），而是落成**双角色工作流**：`决策角色→kimi` 出方案、`执行角色→gemma` 干活。这个能力**配置层就能表达**，无需改引擎。

工作量预估：在现有底座上 **加法为主**，核心改造集中在 M1–M3（角色 + 工具 + 安全），约中等规模；M0 是已基本完成的可行性验证。

---

## 2. 需求拆解

把"一群 DB 运维智能体自动监控运维、推理用 kimi、执行用 gemma"拆成能力清单：

| # | 能力 | 含义 |
|---|------|------|
| C1 | 多 agent / 每类 DB 一个 | 每种数据库（MySQL/Redis/PG/Mongo…）有自己的运维智能体 |
| C2 | 自动监控 | 周期性采集指标/状态，发现异常 |
| C3 | 自动运维 | 异常时诊断 + 执行修复动作 |
| C4 | 推理→kimi | 根因分析、风险评估、方案生成等"动脑"步骤用远程大模型 |
| C5 | 执行→gemma | 采集、只读检查、执行命令等"动手"步骤用本地小模型（占大多数） |
| C6 | 闭环/告警 | 修复后验收，关键事件推送告警，沉淀经验 |

---

## 3. 现状盘点：已具备的底座（含证据）

> 路径均相对 `/home/victor/base/git/temp/ruflo/claude-go`。

### 3.1 按角色路由模型 —— C4/C5 的核心机制（已存在）
- `pkg/agent/modelconfig/resolver.go:44` `Resolve(planName, role)`，优先级 **role alias > plan modelAlias > global default**。
- `resolver.go:104` `resolveAlias`：把 `ollama:gemma4:26b-a4b-it-qat` 这种别名解析出 `BaseURL/APIKey/ProviderName`，**`resolver.go:127` `cfg.BaseURL = entry.Provider.BaseURL`**。
- `pkg/feishu/session.go:759` 每个 stage 的角色独立解析模型，`ConfiguredCloneFull(mcfg.BaseURL, mcfg.APIKey, …)` 建一个**指向该 provider 端点**的 client 克隆（`pkg/api/client.go:201`）。
- 配置里 `ai.plans.<plan>.roles` 已是"角色→模型别名"映射（当前 `development` plan 已给 researcher/architect/planner 指定 kimi）。

> **承重假设①（已代码核实）**：角色解析到 `ollama:gemma4` 时，克隆体 `BaseURL=http://localhost:11434/v1`，**指向 ollama 而非 kimi**。所以"同一团队里 decider 走 kimi、operator 走 gemma"开箱即可表达。
> ⚠️ 小坑：`ConfiguredCloneFull` 没有把 `ResolvedConfig.FirstTokenTimeoutSec/CallTimeoutSec` 透传给克隆体（`client.go:201` 只拷固定字段）。本地 CPU gemma 慢（首 token 可达数百秒），克隆会沿用父 client（kimi）的较短超时 → **需要补一行把慢模型超时透传到克隆**（M0 修）。

### 3.2 gemma 是合法的"会调工具"的 agent 主模型（已实测确认）
- `cmd/claude-go/main.go:2190` `IsLocalEndpoint` → `api.NewOllamaClient`；`pkg/api/client.go:543` 注释明确 "Ollama ≥0.30 在 /v1/messages 上提供完整 Anthropic Messages API（含 tool_use/thinking/图片）"。
- **本地实测**（直接 curl `localhost:11434/v1/messages`，带一个 `run_sql` 工具，问"查 MySQL 当前连接数"）：gemma4 返回合法 `tool_use`（`SHOW STATUS LIKE 'Threads_connected'`，`stop_reason:"tool_use"`），并自行推断出"当前连接数=Threads_connected"。

> **承重假设②（已实测确认）**：gemma 能在 Anthropic 协议上当 executor 发工具调用。
> ⚠️ 调优点：gemma 默认 `think=true`，本次实测一个工具调用花了 ~492 输出 token 在思考。executor 角色应配 `think:false` 求快省钱（见 gemma 部署记忆）；这条要确认 ollama client 请求体能下发 `think:false`。

### 3.2b gemma 能用技能（已实测确认）——但只能走"角色注入"这条路
- **技能进模型的两条路**：
  - **主路径（生产用）**：角色的 `Skills`/`BuiltinSkills` 字段 → `pkg/agent/roles.go:107-143` 把技能正文以 `<role_skills>### Skill: X\n{body}</role_skills>` **注入角色系统提示词**。任何模型从第 0 轮就揣着技能指令。
  - **Skill 工具（动态按需加载）**：`pkg/tool/builtin/lsptools.go:232 Call()` 是**占位实现**，返回"请去 Claude Code/Cursor 加载"，**不真注入正文**。对 kimi/gemma 一视同仁——这是框架限制，不是 gemma 的问题。
- **实测**（用 claude-go 真实注入格式打本地 gemma）：
  - 测试 A：注入 `think-before-code` 技能 + 要求写 Go 函数 → gemma **先输出 `<think>` 块、完成全部 6 项思考、再写代码**（rune 反转正确），5/5 思考项命中、顺序正确 → ✅ 严格遵守注入的技能。
  - 测试 B：技能清单 + Skill 工具 + 匹配任务 → gemma 正确发出 `Skill` tool_use 并选中 `think-before-code` → ✅ 能驱动技能选择协议。
- **对本方案的硬约束**：DB 运维角色的"运维知识"（采集口径、诊断手册、安全规则）必须做成**角色绑定技能**（`BuiltinSkills`/`Skills`），靠注入生效；**不要依赖运行时 Skill 工具动态加载**（占位、不生效）。已验证 gemma 会照注入的技能执行。

### 3.3 周期触发 —— C2 的调度骨架（已存在）
- `pkg/agent/cron.go`：`CronScheduler` 每分钟 tick（`cron.go:226 tickLoop`），`CronJob{Schedule,JobType,Payload,Workflow,ChatID}`（`cron.go:35`），`JobType` 支持 `workflow/query/command/wiki-*`，结果经 `CronExecutor.Notify` 推飞书。持久化 `cron_jobs.json`。
- 这就是"自动监控"的定时器：**定时跑一个采集工作流/命令并把结果落盘或告警**。

### 3.4 动态工作流 —— C1/C3 的装配机制（已存在）
- `pkg/agent/dynamic_workflow.go`：运行时 `RegisterWorkflow`/`UnregisterWorkflow`，`SaveWorkflowToDir`/`LoadWorkflowsFromDir`（`~/.claude-go/workflows/*.json`），**每个 stage 可绑不同 role**，DAG（Kahn 无环校验）。
- 意味着"每类 DB 一个运维工作流"可以**纯 JSON 定义**，不必改 Go 代码。

### 3.5 告警 + 循环 + 编排（已存在）
- 告警：`pkg/swarm_intel/types.go:123 NotifyFunc`、`pkg/agent/teams.go:166`，推飞书 markdown。
- 循环：`pkg/orchestrator/runner.go:97 CompositeRunner`（T-A-O，带 `TerminationPolicy`）、对抗循环、`/api/verify-goal` 目标验收。
- 编排：Coordinator/`runPipelineWithRecovery`（系统A）+ `pkg/orchestrator` DAG（系统B）+ SwarmOrchestrator。

### 3.6 群体智能底座：可复用的 Swarm / Team / Workflow / Orchestrator

上文 §3.1–§3.5 已经把 DB-Ops 所需的“单条工作流”底座盘点清楚；本节进一步说明 **claude-go 已有的群体智能能力可以直接复用**，无需从零实现。这些能力落在四个包里：`pkg/swarm_intel`、`pkg/agent`（Teams + SwarmOrchestrator）、`pkg/agent/dynamic_workflow`、`pkg/orchestrator`。

#### 3.6.1 Swarm 智能引擎 —— 复杂根因分析的“多分析师 + 辩论 + 融合”

`pkg/swarm_intel` 已经实现了一套完整的群体智能引擎：

- **核心流水线**：`Engine` 注释明确其为 **5+2 阶段流水线** —— `Decompose → Scout → Predict → Debate → Fuse → Calibrate → Learn`（`pkg/swarm_intel/engine.go:14-15`）。
- **多分析师与辩论**：`Config.NumAnalysts` 默认 3、`MaxDebateRounds` 默认 2（`engine.go:47-73`）。每个分析师独立给出概率估计与理由，随后通过 `Fuser` 做贝叶斯/对数意见池融合，输出 `Consensus` 与 `BrierScore`（`types.go:39-51`）。
- **信素记忆与学习**：`PheromoneMemory` + `PheromoneStore` 维护“假设→证据→强度”路径（`types.go:79-88`），`ReasoningBank` / `PredictionHistory` 持久化历史推理，用于后续相似问题快速收敛（`engine.go:28-31`）。
- **模拟与校准**：`Simulator` 支持 `social/game/montecarlo` 三种模式，可对多个候选方案做推演（`types.go:90-107`）；`ConformalCalibrator` 做在线校准，`ByzantineFuser` 修剪拜占庭/异常意见，`BanditRouter` 动态选择分析师（`engine.go:34-38`）。
- **工程可靠性**：`ResilientCaller` 封装 LLM 调用，带重试/熔断/降级（`engine.go:25`），远程 kimi 调用不稳时自动兜底。

`pkg/agent/swarm.go` 把这套引擎封装成 `SwarmOrchestrator`（`swarm.go:89`），对外提供：

- **智能角色路由**：`routeRoles` 根据任务复杂度推荐最小角色子集（`swarm.go:326`）。
- **动态任务分解**：`decomposeWithRoles` 把目标拆成带依赖的 `SubTask` DAG（`swarm.go:457`）。
- **拓扑排序并行执行**：`topologicalLevels` + 逐层并行，每层带质量门控和检查点恢复（`swarm.go:122-200+`）。
- **置信度加权汇聚**：低质量子结果降权合并，最终输出结构化结论。

**在 DB-Ops 中的复用点**：

| 场景 | 当前方案 | 复用群体智能后 |
|------|----------|----------------|
| 复杂/跨域故障 | 单 `mysql-diagnostician`（kimi）一次诊断 | 启动 `workflow: "swarm"` 诊断团队，多个分析师并行论证，融合后给出共识根因与置信度 |
| 根因假设冲突 | 依赖一次 LLM 自洽 | 用 `Debate → Fuse` 让不同角色（optimist/pessimist/neutral/contrarian）交锋，`Consensus` 低时自动告警人工介入 |
| 修复方案选择 | kimi 直接给单一方案 | 用 `Simulator` 对 2-3 个候选方案做蒙特卡洛/博弈推演，选预期风险最小的 |
| 经验沉淀 | 每次重新诊断 | 成功处置写入 `PheromoneMemory` / `ReasoningBank`，下次同类故障直接命中高信素路径，kimi 调用次数下降 |
| 远程模型抖动 | kimi 失败则任务失败 | `ResilientCaller` 自动重试/熔断，必要时降级到 gemma 或人工 |

> ⚠️ **安全边界不变**：Swarm 输出的是**建议**，任何写操作仍需经过 §11.7 的确定性护栏，不因为“群体智能更可信”就放松。

#### 3.6.2 生产级 Agent Teams —— Blackboard 共享上下文 + 动态角色路由

`pkg/agent/teams.go` 已实现 `ProductionTeamManager`（`teams.go:219`），核心特征：

- **进程级单例**：统一管理团队生命周期、持久化、飞书通知、V2 Task 追踪（`teams.go:1-28`）。
- **Blackboard 共享上下文（bMAS）**：所有阶段执行前读完整黑板，执行后写回结果，阶段间做结构化 Handoff（`teams.go:7-12`）。
- **workflow 路由**：`CreateTeam(name, workflow, objective, chatID)` 时，若 `workflow != "swarm"` 走标准 WorkflowDef；若 `workflow == "swarm"` 直接调用 `executeSwarm`（`teams.go:445-465`、`1071-1128`）。
- **通知与记忆**：执行结果自动飞书通知（`teams.go:166`），完成后写入高权重团队记忆并触发 `evolution.LearnFromTeam`（`teams.go:1140-1150`）。

**在 DB-Ops 中的复用点**：

- 每类 DB / 每个实例异常时，由 cron job 调用 `team/create db-ops-mysql <objective> <chatID>`，自动拉起一个受 `ProductionTeamManager` 管理的诊断团队。
- `objective` 中携带 cron 已采集的快照 JSON，团队通过 Blackboard 共享，避免 `diagnose` / `remediate` / `verify` 各阶段重复采集。
- 团队跑完后自动沉淀到 memory，作为后续基线与 runbook。

#### 3.6.3 动态工作流 —— JSON 定义、DAG 校验、无需重编译

`pkg/agent/dynamic_workflow.go` 已经实现运行时工作流注册：

- 支持 `pipeline`、`fanout`、`adversarial`、`adversarial_dev`、`orchestrated` 五种可由 JSON 动态定义的模式（`dynamic_workflow.go:24-30`）。
- `RegisterWorkflow` 校验并注册到 `customWFs`（`dynamic_workflow.go:38-55`）。
- `WorkflowDef.Validate` 做基础校验，包括 DAG 无环检查（`dynamic_workflow.go:112-120+`）。

**在 DB-Ops 中的复用点**：

- §9.2 / §11.15 中的 `db-ops-mysql.json` 可以直接通过 `POST /api/workflows` 注册，无需改 Go 代码。
- 如果需要“多专家评审”的对抗模式，只需把 `mode` 改为 `adversarial`，复用已有执行路径。

#### 3.6.4 Orchestrator 引擎 —— DAG 调度、对抗循环、可观测性

`pkg/orchestrator` 是 claude-go 的第二套工作流执行引擎，比系统 A 的 Coordinator 更偏“结构化执行”：

- **事件驱动 DAG 调度**：`Engine.Run` 完成 Graph 构建、Filter→Score→Dispatch、并发 goroutine 池、背压、RPM 限制、停滞恢复、检查点（`engine.go:39-130`、`engine.go:229+`）。
- **对抗循环**：`CompositeRunner` 支持按顺序链式调用多个 Runner 并带 `TerminationPolicy`（`runner.go:97-174`）；`AdversarialRunner` 实现 generator ↔ reviewers 的多轮反馈（`llm_runner.go:225-240+`）。
- **质量自适应终止**：`QualityTermination` 根据评分达标/收敛/退化自动停止循环（`llm_runner.go:141-204`）。
- **LLM 模板变量**：`LLMRunner` 自动把 `{objective}`、`{prev_result}`、`{dep:taskID}` 替换为 Blackboard 中的上游输出（`llm_runner.go:25-84`）。
- **可观测性桥接**：`ObservabilityBridge` 把 Engine 生命周期事件发到 observability 事件总线（`orchestrator_bridge.go:11`）。

**在 DB-Ops 中的复用点**：

- 诊断阶段可用 `adversarial` 模式：一个 generator（kimi diagnostician）出方案，多个 reviewer（skeptic / safety / operations）评审，直到 `QualityTermination` 收敛。
- 复杂修复管线可用 `orchestrated` 模式，由 `pkg/orchestrator` 引擎调度 `safety-gate`、`remediate`、`verify` 等 TaskRunner，自动审计。
- `ObservabilityBridge` 让每次团队执行自动进入现有审计链路，无需额外埋点。

#### 3.6.5 Cron + Notify —— 周期触发与告警

`pkg/agent/cron.go` 的 `CronScheduler` 已实现：

- 每分钟 tick，标准 cron 表达式（`cron.go:35-50`、`cron.go:86-89`）。
- `JobType` 支持 `workflow`、`query`、`command`（`cron.go:39`）。
- 执行结果通过 `CronExecutor.Notify` 推飞书（`cron.go:52-57`）。

**在 DB-Ops 中的复用点**：

- §5.3-B 两级触发中的“廉价 gemma 采集 + gate”直接用 cron `command` 实现；判定异常后由该 job 调用 `team/create`。
- §11.11 的实时告警/日报直接复用 `NotifyFunc` 推飞书 markdown 卡片。

---

## 4. 差距清单（要改的）

| # | 差距 | 现状 | 影响能力 |
|---|------|------|----------|
| G1 | **无 DB 运维角色** | `pkg/agent/roles.go` 92 个角色无 DBA/SRE/运维/监控类 | C1/C3 |
| G2 | **无数据库/运维工具** | `pkg/tool/builtin/` 无 mysql/pg/redis/SQL/kubectl/prometheus 工具；只有 Bash | C2/C3/C5 |
| G3 | **沙箱默认禁网** | `sandbox.networkDisabled=true`、`requiredForTeam=true`；连库/kubectl/抓指标都要网络 | C2/C3 |
| G4 | **无常驻守护 agent** | 只有 cron 周期 tick，没有 long-running watcher。"实时监控"实为"周期巡检" | C2（设定预期） |
| G5 | **双模型未接线** | plans 里没有 db-ops plan，没有 decider/operator 角色绑定 | C4/C5 |
| G6 | **无运维安全护栏** | agent 能跑任意 Bash；对生产库自治执行 DROP/kill/restart/scale **极危险** | **C3 前置** |
| G7 | **无基线/趋势状态** | 每次 cron tick 无上下文，抓不到"连接数过去一小时在爬" | C2 |
| G8 | **条件升级缺失** | 静态 DAG 无法"健康就跳过 kimi"；裸跑会每库每分钟烧 kimi | C4 成本 |

---

## 5. 目标架构

### 5.1 核心范式："大脑–手" 双角色团队
每类 DB = **一个多角色工作流**（不是单个 agent —— 按角色路由只在 team/workflow 路径生效，独立 agent 只能拿全局 `ai.modelAlias`）。角色分两类：

```
                     ┌─────────────── cron tick（每 N 分钟，便宜）───────────────┐
                     ▼                                                            │
  [采集 collect]  gemma ── 抓指标/状态/慢日志/复制延迟 ──► 结构化健康快照          │
       │                                                                          │
       ▼  health gate（确定性判断：阈值/基线对比）                                │
   健康？──是──► 写基线到 memory，告警静默，结束（绝大多数 tick 停在这里，0×kimi）─┘
       │否（异常）
       ▼
  [诊断 diagnose] kimi ── 根因分析 + 风险评估 + 生成运维方案(plan，只读、不动手)
       │
       ▼  safety gate（确定性护栏：allowlist / 只读默认 / 危险动作需审批）
       ▼
  [执行 remediate] gemma ── 按 plan 拆成具体命令，dry-run→执行（受护栏约束）
       │
       ▼
  [验收 verify]  kimi/gemma ── verify-goal 确认已解决；未解决带 gap 回诊断（≤N 轮）
       │
       ▼
  告警 + 审计（feishu / observability / memory 沉淀 runbook）
```

**成本模型**：高频、便宜、低风险的**采集与执行用 gemma**；只有 gate 判定异常时才唤醒 **kimi 做诊断决策**（低频、贵）。这正好对上"执行类大部分用 gemma、推理用 kimi"。

### 5.2 分层职责

| 层 | 模型 | 频率 | 职责 |
|----|------|------|------|
| 采集层 | gemma | 高（每 tick） | 拉指标/状态/慢日志/复制延迟/磁盘，输出结构化快照 |
| 判定层 | 无 LLM（确定性代码）/ gemma | 高 | 阈值 + 基线对比，决定是否升级；**不烧 kimi** |
| 诊断决策层 | kimi | 低（仅异常） | 根因、风险、运维方案（只读分析） |
| 执行层 | gemma | 中 | 在护栏内执行方案；只读优先、危险动作走审批 |
| 编排/调度层 | — | — | cron 触发；工作流 DAG；目标验收循环 |
| 告警/审计/记忆层 | — | — | 飞书告警；observability 审计；memory 存基线与 runbook |

### 5.3 条件升级的落地（G8）——两种实现，推荐 B
- **A. 单工作流 + gate stage**：采集后接 `content_gate` 式门禁，健康则短路结束。受限于现有 gate 语义。
- **B.（推荐）两级触发**：cron 跑一个**廉价 gemma 采集+判定 job**（`JobType=command/query`），仅当判定异常时由该 job **再起一个诊断团队**（kimi）。天然条件化，N 个库每分钟都只跑 gemma，kimi 仅按需唤醒。复用现有 `team/create` + cron Notify。

---

## 6. 关键设计决策

1. **双模型 = 双角色工作流，不改引擎**。decider→kimi、operator→gemma 用 `ai.plans.<plan>.roles` 表达；引擎"一个 runner 绑一个模型直到结束"的限制不影响本方案。
2. **安全与模型解耦**。是否允许 DROP/kill/restart/scale 由**确定性护栏**（allowlist + 只读默认 + 审批门 + dry-run + blast-radius 限制）决定，**与 gemma/kimi 的判断无关**。gemma 负责"动手"，但"准不准动手"由代码守住。
3. **监控 = 周期巡检 + 基线状态**，不是真常驻 watcher（G4/G7）。把上一轮快照/基线写 memory，下一轮对比，才能抓住趋势型异常。
4. **每个 DB 是一个 workflow（多角色），不是单 agent**（按角色路由的前提，§5.1）。`plans` 的 key 必须等于该 team 用的 workflow 名（`workflow.go:1449 Resolve(team.Workflow, role)`）。
5. **只读监控先上线，写操作后置**。先交付"gemma 采集 → kimi 诊断 → 飞书告警"的**零写权限**闭环（有价值且安全）；任何 stage 能写之前，护栏必须已落地。
6. **复杂诊断走群体智能，不走单一路径**。当 gate 判定为 `critical`、或症状涉及多个相互矛盾的根因假设、或需要跨域（MySQL + 主机 + 网络）联合分析时，把 `diagnose` 升级为 `workflow: "swarm"` 或 `mode: "adversarial"`，复用 `SwarmOrchestrator` 的多分析师辩论融合（`pkg/agent/swarm.go:122`）与 `AdversarialRunner` 的 generator-reviewer 循环（`pkg/orchestrator/llm_runner.go:225`）。Swarm 输出的是带置信度的建议，仍需过 §11.7 的确定性护栏才能执行。

---

## 7. 改造分期

> 原则：**安全是写操作的前置依赖**，不是某个靠后阶段。M4/M5 在 M3 落地前一律只读。

### M0 — 可行性收口（基本已完成，补两个小修）
- [x] 确认角色路由能把 operator 真路由到 gemma 端点（§3.1）。
- [x] 实测 gemma 在 Anthropic 端点发 `tool_use`（§3.2）。
- [ ] **修1**：`ConfiguredCloneFull` 透传慢模型超时（`FirstTokenTimeoutSec/CallTimeoutSec`）到克隆体，否则 gemma 大概率超时。
- [ ] **修2**：确认/打开 operator 角色的 ollama 请求体 `think:false`（省 token、提速）。
- **验收**：用一个临时 2 角色工作流（decider=kimi, operator=gemma）跑通一次端到端，operator 确实命中 gemma 且能多轮调用工具不超时。

### M1 — DB 运维角色（先用动态工作流 JSON，不动 roles.go）
- 通用角色对（跨 DB 复用，DB 类型作参数）：`db-collector`(gemma)、`db-diagnostician`(kimi)、`db-operator`(gemma)。
- 每类 DB 一层薄人格：`mysql-ops` / `redis-ops` / `pg-ops`…（系统提示词含该 DB 的指标口径与常见故障）。
- 落地：`~/.claude-go/workflows/db-ops-<engine>.json`，stage 绑上述角色。验证稳定后再考虑固化进 `roles.go`。
- **验收**：`POST /api/workflows` 注册成功、`GET /api/roles` 可见、Validate 通过。

### M2 — 工具与采集（C2/C5）
- 先**薄封装**：用现有 Bash + 各 DB 的 CLI 客户端（mysql/redis-cli/psql）+ 一个"DB 连接信息"skill，让 gemma 通过 Bash 跑只读查询。**最快见效，无需新 Go 代码**。
- 再**结构化工具**（可选增强）：`pkg/tool/builtin/db_query.go`（只读 SQL/命令，连接走配置白名单）、指标工具（prometheus query / SHOW STATUS 解析）。结构化工具比裸 Bash 更易加护栏与审计。
- **验收**：gemma 采集出结构化健康快照（连接数/QPS/复制延迟/慢查询/磁盘），逐字段核对真实值。

### M3 — 运维安全护栏（C3 的前置，**最关键**）
- **只读默认**：collector/diagnostician 角色**零写权限**（工具集只给只读）。
- **写操作 allowlist**：operator 仅允许白名单内的运维动作；危险动作（DROP/TRUNCATE/kill/restart/scale/flush）默认拒绝。
- **审批门**：危险动作走 `AskUserQuestion`/飞书审批，人确认后才执行（复用 `permissions` + content_gate 模式）。
- **dry-run + blast-radius**：先出"将执行的命令"预览；限制影响面（如禁止无 WHERE 的写、限制单次影响行数）。
- **独立执行 lane**：运维命令需要网络（连库/kubectl），不能跑在 `networkDisabled` 沙箱里。给 operator 一个**网络放开但命令受 allowlist 约束**的执行通道（而非整体关沙箱）。
- **审计**：每个写动作落 observability + memory。
- **验收**：构造危险动作必被拦/必走审批；只读动作放行；审计可查。

### M4 — 监控编排上线（先只读）
- 每类 DB 一个 ops 工作流 + 一条 cron（推荐 §5.3-B：cron 跑 gemma 采集判定，异常才起 kimi 诊断团队）。
- 基线/趋势：每轮快照写 memory，判定层对比上一轮（G7）。
- 异常→诊断→**告警**（此阶段不自动写，只给人看方案）。
- **验收**：人为造一个异常（如压连接数），cron 周期内被采集→判定→升级 kimi 诊断→飞书告警，且健康时**不触发 kimi**。

### M5 — 自治闭环 + 学习（M3 落地后才开写）
- 接 verify-goal 验收循环：执行后确认已解决，未解决带 gap 重试（≤N 轮）。
- 经验沉淀：成功处置写 memory/evolution 形成 runbook，下次相似故障优先复用。
- **验收**：一个允许自治的低风险动作（如清理临时表/重建索引在测试库）走完"诊断→护栏→执行→验收→沉淀"全链。

---

## 8. 风险与缓解

| 风险 | 说明 | 缓解 |
|------|------|------|
| gemma 多轮工具可靠性 | 单轮已验证；长链路运维可能漂移/误调 | gemma 限于采集/只读/执行已定方案；**判断权交 kimi，安全交确定性护栏**（决策2） |
| 本地 gemma 慢 | CPU/iGPU ~22–25 tok/s，长链路耗时 | M0 修超时透传；采集任务设短上限；只在异常才上 kimi |
| 自治误操作 | 在生产库动手 = 高危 | M3 护栏为写前置；只读先行；危险动作审批 |
| 沙箱禁网 vs 连库 | 默认禁网挡住一切运维 | M3 独立执行 lane（放网 + allowlist），不整体关沙箱 |
| 成本失控 | 每库每分钟烧 kimi | §5.3-B 两级触发，健康 tick 0×kimi |
| 克隆超时/think 默认开 | gemma 调用超时或浪费 token | M0 两个小修 |

---

## 9. 附录

### 9.1 plans 配置示例（接线 C4/C5）
> 注：`plans` 的 key 必须等于工作流名（`Resolve(team.Workflow, role)`）。

```jsonc
"ai": {
  "plans": {
    "db-ops-mysql": {
      "modelAlias": "ollama:gemma4:26b-a4b-it-qat",   // 默认走本地 gemma
      "fallbackAliases": ["kimi:kimi-k2"],
      "roles": {
        "db-collector":     "ollama:gemma4:26b-a4b-it-qat",  // 采集=gemma
        "db-operator":      "ollama:gemma4:26b-a4b-it-qat",  // 执行=gemma
        "db-diagnostician": "kimi:kimi-k2",                  // 诊断决策=kimi
        "mysql-ops":        "kimi:kimi-k2"
      }
    }
  }
}
```

### 9.2 工作流 JSON 骨架（`~/.claude-go/workflows/db-ops-mysql.json`）
```jsonc
{
  "name": "db-ops-mysql",
  "mode": "pipeline",
  "producesCode": false,
  "stages": [
    {"name":"collect",   "role":"db-collector",     "prompt":"采集 MySQL 健康快照：连接数/QPS/复制延迟/慢查询/磁盘。只读，输出结构化 JSON。目标:{objective}"},
    {"name":"gate",      "role":"db-collector",     "prompt":"对比基线 {prev_result}，判定 healthy|degraded|critical，并说明触发项。"},
    {"name":"diagnose",  "role":"db-diagnostician", "prompt":"基于 {collect} 与 {gate} 做根因分析、风险评估，产出运维方案（只读，不执行）。", "dependsOn":["gate"]},
    {"name":"remediate", "role":"db-operator",      "prompt":"按 {diagnose} 方案执行；危险动作必须先 dry-run 并请求审批。", "dependsOn":["diagnose"]},
    {"name":"verify",    "role":"db-diagnostician", "prompt":"验收 {objective} 是否解决，未解决给 gap。", "dependsOn":["remediate"]}
  ]
}
```
> 上线初期建议删掉/禁用 `remediate`，先跑"采集→判定→诊断→告警"的只读链（决策5）。

### 9.3 cron 示例（两级触发，§5.3-B）
```jsonc
// ~/.claude-go/cron/cron_jobs.json 中一条
{
  "schedule": "*/5 * * * *",
  "jobType": "command",
  "payload": "gemma 采集 MySQL 健康快照并判定；critical 时调用 team/create 起 db-ops-mysql 诊断团队",
  "chatID": "<飞书运维群>"
}
```

---

## 10. 一页纸落地顺序
1. **M0** 收口（补超时透传 + think:false，端到端验一次双模型）→
2. **M1** 三个通用角色 + 各 DB 薄人格（JSON）→
3. **M2** Bash+CLI 薄封装先采集到结构化快照 →
4. **M3** 安全护栏（写前置）→
5. **M4** 只读监控编排 + cron 两级触发 + 基线 →
6. **M5** 放开受控自治 + 经验沉淀。

先交付 M0–M2 + M4(只读)，就已经是"一群 gemma 采集监控、异常时 kimi 诊断告警"的可用系统；M3+M5 再把"自动运维动手"安全地打开。

---

# 11. MySQL 自动运维 Agent —— 功能设计（首发）

> 本节是 §5 通用架构在 MySQL 上的具体化，作为 Fleet 的**第一个落地实例**。
> 设计原则：**只读监控先行、写操作受确定性护栏约束、推理=kimi / 采集与执行=gemma、知识用角色绑定技能注入**（§3.2b 已实测 gemma 遵守注入技能）。

## 11.0 关键实现约束（代码核实后，先看这条）
这四条改变了实现顺序与若干设计细节，已回填到下文：

1. **采集必须走"进程内 `db_query` Go 工具"，不是 Bash+mysql 客户端**。`pkg/tool/builtin/bash.go:184` 是唯一经 `sandbox.RunProcessGuarded` 的工具，沙箱默认 `networkDisabled:true`（docker `--network none`）→ **Bash 连不上远程 MySQL**。而 `quote_tool.go:176`/`webfetch.go:164` 这类内置工具是**进程内直接发网络、不过沙箱**。故新建 `pkg/tool/builtin/db_query.go`（`database/sql`，进程内连库）**既绕开沙箱网络限制，又直接返回结构化行**。这是"只读监控先行"能成立的前提——**db_query 工具是首发的承重件，不是可选增强**（原 M2 描述已修正）。
2. **判定层只吃结构化数据，绝不解析 LLM 散文**。gate 读 db_query 返回的行 / 落盘的快照 JSON，**不依赖 gemma 自由生成合法 JSON**（Test A 验的是"散文+代码"遵从，未验严格 schema；ollama 的 `format`/约束解码大概率走不通 Anthropic 协议的 api.Client）。采集尽量由 db_query 工具产出确定结构，gemma 只做"调哪些查询/汇总要点"。
3. **执行模型以 §11.9 两级触发为准**（非单一长 pipeline）。cron 跑 gemma 采集+gate（廉价、常态）；**仅异常时**起一个**只含 `diagnose → safety-gate → remediate → verify` 的诊断团队**，喂入 cron 已采的快照。pipeline 模式不会因 gate 判 healthy 自动短路（除非把 gate 接成 content_gate 终止），所以健康分支由 cron job 里的确定性代码处理，不进 LLM 工作流。
4. **host 级信号需要 DB 宿主上的 shell**（§11.4 的磁盘/CPU/iostat、错误日志、慢日志文件）。**本设计默认自建 MySQL（有宿主 shell）**；若是 RDS/托管实例无 shell，这些信号改由 exporter/CloudWatch 提供，能从 SQL 拿到的（status/innodb/复制/容量）仍走 db_query。

## 11.1 定位与边界
- **管什么**：一个或多个 MySQL 实例（主/从）的**可用性、性能、复制、容量、锁/事务**健康，以及异常时的**诊断与受控修复**。
- **不管什么（首版）**：schema 设计评审、SQL 业务优化建议（可后续作为独立工作流）、跨实例容灾切换（高危，仅出方案不自动执行）。
- **自治级别（默认）**：**L0/L1 自动，L2 审批，L3 禁止**（见 §11.7）。首版上线建议先 **alert-only**（关闭 L1 自动写），稳定后再开 L1。

## 11.2 团队组成（双模型三角色）
工作流名 `db-ops-mysql`（plan key 必须同名，§6 决策4）。

| 角色 | 模型 | 工具权限 | 绑定技能 | 职责 |
|------|------|----------|----------|------|
| `mysql-collector` | **gemma** | 只读：`db_query`(只读账号) / Bash(白名单只读命令) | `mysql-collect` | 高频采集指标/状态/慢日志，输出结构化健康快照 |
| `mysql-diagnostician` | **kimi** | 只读：`db_query`(只读) + 读 memory 基线 | `mysql-diagnose` | 异常时根因分析、风险评估、生成分级运维方案（**不执行**） |
| `mysql-operator` | **gemma** | 受限写：`db_query`(ops 账号, 受 allowlist) / Bash(运维白名单) | `mysql-safe-ops` | 按方案执行；危险动作 dry-run + 审批门 |

> 判定层（health gate）是**确定性 Go 代码 / 廉价 gemma 调用**，不算独立 LLM 角色（§5.2）。

## 11.3 技能设计（知识即技能，角色绑定注入）
把"运维知识"做成三个 `BuiltinSkills`（放 `pkg/skills/builtin/` 或 `~/.claude-go/skills/`，绑到对应角色）：

- **`mysql-collect`**（绑 collector）：规定采集口径——必须采哪些 `SHOW GLOBAL STATUS`/`SHOW VARIABLES`/`SHOW SLAVE STATUS`/`SHOW ENGINE INNODB STATUS`/`information_schema` 项；输出**固定 JSON schema**（字段名、单位）；只读铁律（禁任何写）。
- **`mysql-diagnose`**（绑 diagnostician）：症状→根因→方案手册（§11.6）；输出**结构化方案**（每个动作带 `level`/`command`/`risk`/`rollback`/`expected`）；要求引用具体指标数值佐证。
- **`mysql-safe-ops`**（绑 operator）：安全铁律——只执行方案里 `level<=允许级` 的动作；任何写先 dry-run/EXPLAIN；禁止无 WHERE 的 DML、禁止 DROP/TRUNCATE；超出 allowlist 一律拒绝并上报。
> 已验证 gemma 会严格遵守注入技能里的"先思考/先 dry-run/铁律"类指令（§3.2b 测试 A）。
> ⚠️ **`mysql-safe-ops` 技能是"纵深防御"，不是安全边界本身**。Test A 证明的是 gemma 会**照指令多做一步**（additive compliance），**没有**证明它会在压力下**拒绝**违规动作（constraint compliance，更难）。真正拦住危险写的是 §11.7 的**确定性守卫**（allowlist/denylist/blast-radius），它独立于 gemma/kimi 的判断。技能只是让模型更可能自觉合规，**不得**用"gemma 会守技能"替代确定性护栏。

## 11.4 监控指标体系（采集层 gemma）
| 维度 | 关键指标 | 数据来源 |
|------|----------|----------|
| 可用性 | ping 通断、`Uptime`、error log 新增 ERROR | `mysqladmin ping`、`SHOW GLOBAL STATUS`、错误日志 |
| 连接 | `Threads_connected`/`max_connections` 占比、`Threads_running`、`Aborted_connects`、`Max_used_connections` | `SHOW GLOBAL STATUS/VARIABLES` |
| 吞吐 | QPS(`Questions`Δ)、TPS(`Com_commit`+`Com_rollback`Δ)、`Slow_queries`Δ | status 差分 |
| 慢查询 | 慢日志条数、Top 慢 SQL 指纹 | slow log / `events_statements_summary_by_digest` |
| InnoDB | Buffer Pool 命中率、`Innodb_row_lock_waits`/`_time_avg`、死锁、History List Length、脏页比例 | `SHOW ENGINE INNODB STATUS`、status |
| 锁/事务 | 长事务(>N s)、`innodb_trx` 运行时长、阻塞链、元数据锁 | `information_schema.innodb_trx`、`sys.innodb_lock_waits`、`processlist` |
| 复制 | `Seconds_Behind_Master`、`Slave_IO_Running`/`Slave_SQL_Running`、GTID gap、`Last_*_Error` | `SHOW SLAVE STATUS` / `SHOW REPLICA STATUS` |
| 容量 | 数据目录/binlog 磁盘占用与增速、binlog 堆积、大表 Top、表碎片 | `information_schema.tables`、`du`/`df`、`SHOW BINARY LOGS` |
| 主机 | CPU/内存/IO/负载（运行 MySQL 的宿主） | Bash(`top`/`free`/`iostat`) 或 prometheus |

采集层输出统一**结构化健康快照 JSON**（带时间戳 + 实例标识），供 gate 与基线对比。

## 11.5 健康判定与分级（gate，确定性）
判定层对快照做**阈值 + 基线对比**，产出 `healthy | degraded | critical` + 触发项列表。示例阈值（可配，存配置）：

| 触发项 | degraded | critical |
|--------|----------|----------|
| 连接占比 `Threads_connected/max_connections` | >70% | >90% |
| `Threads_running` | >CPU 核数 | >2×核数且持续 |
| 复制延迟 `Seconds_Behind_Master` | >30s | >300s 或线程 No |
| Buffer Pool 命中率 | <99% | <95% |
| 长事务时长 | >60s | >600s |
| 慢查询增速 | 基线 ×3 | 基线 ×10 |
| 磁盘可用 | <20% | <10% |
| 死锁/分钟 | >0 | 频发 |

**只有 `degraded/critical` 才升级 kimi 诊断**；`healthy` 写基线后结束（成本模型 §5.3）。

## 11.6 诊断手册（决策层 kimi，`mysql-diagnose` 技能内置）
症状 → 候选根因 → 分级方案（节选）：

- **连接打满**：根因（连接泄漏/慢查询堆积/`wait_timeout` 过大/突发流量）→ L0 列 `processlist` 找元凶；L1 kill 指定 sleep 超长连接；L2 调 `max_connections`/上连接池治理（审批）。
- **复制延迟**：根因（大事务/从库单线程/IO 瓶颈/锁等待）→ L0 看 `SHOW SLAVE STATUS` + relay 落后点；L1 无（多需结构治理）；L2 开并行复制/扩从（审批）。
- **慢查询飙升**：根因（缺索引/统计信息过期/计划突变）→ L0 `pt-query-digest`/digest 表定位 Top SQL + EXPLAIN；L1 无；L2 加索引/ANALYZE TABLE（审批，大表慎重）。
- **磁盘将满**：根因（binlog 堆积/大表暴涨/临时文件）→ L0 定位占用 Top；L1 `PURGE BINARY LOGS BEFORE ...`（在确认从库已消费后，审批或低风险自动）；L2 扩容/归档。
- **死锁/长事务**：L0 抓死锁图 + `innodb_trx`；L1 kill 指定阻塞事务（审批，需确认非关键写）；L2 业务侧治理。

输出**结构化方案**：`[{level, title, command, risk, blast_radius, rollback, expected_effect, evidence}]`。

## 11.7 运维动作分级与护栏（执行层 gemma + 确定性守卫）
**安全与模型解耦**（§6 决策2）：是否放行由**确定性守卫**判定，不取决于 gemma/kimi 的判断。

| 级别 | 含义 | 例子 | 放行策略 |
|------|------|------|----------|
| **L0** | 只读诊断 | `SHOW *`、`EXPLAIN`、`processlist`、`pt-query-digest` | 自动放行 |
| **L1** | 低风险、可回滚、影响面小 | kill 单个指定 sleep>阈值连接、`PURGE BINARY LOGS`(从库已消费)、`ANALYZE TABLE`(小表) | 默认**审批**；稳定后可白名单内自动 |
| **L2** | 高风险/影响面大 | 改全局变量、加索引/大表 DDL、扩从、调 `max_connections` | **强制人工审批**（飞书） |
| **L3** | 破坏性 | `DROP`/`TRUNCATE`/无 WHERE 的 `DELETE/UPDATE`、`RESET MASTER`、`STOP SLAVE` 永久 | **直接拒绝**（守卫硬拦） |

护栏组件（**全部是确定性 Go 代码，不依赖 LLM 判断**）：
- **命令 allowlist + L3 denylist**：正则/AST 双重匹配，拒绝即上报。
- **dry-run / EXPLAIN 预览**：写动作先出"将执行什么 + 预计影响行数"，再决定。
- **Blast radius 限制**：DML 必须带 WHERE 且预计影响行数 < 阈值；DDL 大表（>阈值）一律转 L2。
- **陈旧目标复核（TOCTOU 必须防）**：kimi 方案里的 `KILL <id>`/目标会话来自采集时刻的快照，执行时该连接 id 可能已被 MySQL 复用给**另一个**会话 → operator 在 kill 前**必须用确定性代码重新核对目标的 user/host/command/查询指纹/存活时长**与方案一致，不一致就**放弃并上报**，绝不按陈旧 id 直接动手。同理 `PURGE BINARY LOGS BEFORE x`：**"从库已消费到 x"必须由确定性查询当场校验**（`SHOW SLAVE STATUS` 的已读位点），不能信 gemma/kimi 的转述。
- **审批门**：L1(可选)/L2 经飞书卡片，人点确认才执行（复用 `permissions` + `AskUserQuestion` + content_gate 模式）。
- **审计**：每个写动作（命令 + dry-run 结果 + 审批人 + 结果）落 observability + memory。
- **幂等/超时**：单动作超时即中止并告警，不重试危险写。

## 11.8 连接与凭证（最小权限三账号）
| 账号 | 权限 | 用途 |
|------|------|------|
| `cg_ro` | `SELECT, REPLICATION CLIENT, PROCESS`（只读 + 看状态/processlist） | collector / diagnostician |
| `cg_ops` | 在 `cg_ro` 基础上加**受限**写（如 `PROCESS`+kill、特定库 DDL），**不给** `DROP`/`SUPER`(按需) | operator 执行 L1/L2 |
| —— | 不存任何高权限账号给 LLM 直接拼接 | —— |
- 凭证存配置（不入库的本地文件 / 环境变量），实例清单（host/port/role/账号）做成配置；**操作走网络放开但命令受 allowlist 的执行 lane**（§7-M3），不整体关沙箱（§4 G3）。

## 11.9 编排与调度（cron 两级触发，**本节为权威执行模型**）
```
cron(*/N min) ─► 取实例锁(忙则跳过本次) ─► [db_query 采集 + gemma 汇总] ─► 写快照JSON
                                                  ▼
                                  gate 判定(确定性代码, 读快照)
                                                  │healthy → 更新基线, 释放锁, 结束 (0×kimi, 不进LLM工作流)
                                                  │degraded/critical
                                                  ▼
              team/create db-ops-mysql {objective: 实例+触发项, snapshot: 已采快照}
                                                  ▼
              工作流(仅这4阶段): diagnose(kimi) → safety-gate(确定性) → remediate(gemma,L0/L1|审批L2) → verify
                                                  ▼
                                  告警 + 审计 + 沉淀 ─► 释放实例锁
```
- **采集与 gate 在 cron job 里完成**（确定性短路 healthy），**不**放进 LLM 工作流——pipeline 模式不会因 gate 判健康而自动停（除非把 gate 接成会终止的 content_gate）。异常工作流因此**只含 `diagnose→safety-gate→remediate→verify`**，并把 cron 已采的快照随 objective 喂入，避免重复采集。
- **每实例并发锁（必须）**：cron 每分钟触发，而一次诊断/修复可能跑几分钟（kimi 延迟 + gemma ~22 tok/s）→ 下一 tick 会对同一实例**再起一个 operator**。用 per-instance 锁/"该实例已有 running 运行则跳过"（复用 webapp `recentOrch`+running 检查的同款思路）。
- 采集 job 用 cron `JobType=command/query`（§3.3）；异常才 `team/create`（§5.3-B），N 实例常态只跑 gemma。
- 每实例一条 cron（schedule 可不同：核心库 1min、次要库 5min）。
- **复杂/高风险诊断的群体智能升级路径**：当 gate 输出 `critical` 且触发项满足以下任一条件时，cron job 在 `team/create` 时把工作流名指定为 `"swarm"`（而非 `db-ops-mysql`），直接复用 `pkg/agent/swarm.go:122` 的蜂群模式：
  - 单一根因假设无法解释所有异常指标；
  - 涉及跨域（DB + 主机 + 网络）联合根因分析；
  - 历史 `ReasoningBank` 提示该症状存在多种可能根因且置信度分散。
  此时 `SwarmOrchestrator` 会先做 `routeRoles`（`swarm.go:326`）决定分析师数量，再做 `decomposeWithRoles`（`swarm.go:457`）生成子任务 DAG，逐层并行执行并融合结论，最终输出带 `Consensus`/`BrierScore` 的诊断建议。若 `Consensus < 阈值` 或 `BrierScore` 差，则**不进入 remediate，直接告警人工介入**（这是比单 kimi 更安全的默认）。
- 对中等复杂度的异常，也可保持 `db-ops-mysql` 工作流，但把 `mode` 设为 `adversarial`，用 `pkg/orchestrator/llm_runner.go:225` 的 `AdversarialRunner` 让 diagnostician 生成方案后由 skeptic/safety 角色评审，直到 `QualityTermination` 收敛。

## 11.10 基线与趋势状态（memory）
- 每轮快照写 `memory`（key 形如 `mysql/<instance>/snapshot/<ts>`，滚动窗口保留近 M 条）+ 维护 `mysql/<instance>/baseline`（各指标的均值/分位）。
- gate 用基线判"趋势型异常"（连接数一小时持续爬升、慢查询相对基线 ×N），弥补"每次 tick 无上下文"（§4 G7）。

## 11.11 告警与报告
- **实时告警**（critical/审批请求）：飞书卡片，含实例、触发项、关键指标值、kimi 方案摘要、审批按钮。
- **日报**：每日定时汇总各实例健康趋势、发生的处置、待人工跟进项（cron `JobType=workflow`）。
- 告警去重/抑制：同一触发项窗口内合并，避免刷屏（参考 bot 已有指纹去重）。

## 11.12 闭环与学习
- **验收**：执行后接 `/api/verify-goal`，确认指标回落（如连接占比 <70%）；未解决带 gap 回诊断（≤N 轮）。**验收前必须用 db_query 重新采一次当前指标**——拿修复后的新数据判，绝不能用 `{prev_result}` 里修复前的陈旧快照（否则是在对修复前的数字"确认成功"）。
- **runbook 沉淀**：成功处置（症状→根因→有效动作）写 memory/evolution，下次相似故障 diagnostician 优先复用（kimi prompt 注入历史 runbook）。

## 11.13 失败模式与降级
| 故障 | 表现 | 降级 |
|------|------|------|
| gemma(ollama) 不可用 | 采集失败 | 退 kimi 采集（贵，限频）或仅告警"监控降级" |
| kimi 不可用 | 无法诊断 | gate 命中直接告警原始触发项，人工介入；operator 不自治 |
| MySQL 不可达 | ping 失败 | **只告警，不尝试任何写**；标记实例 down |
| 审批超时 | L2 动作无人确认 | 不执行，持续告警直到人处理或转 runbook |

## 11.14 MySQL 专项落地顺序
1. **M0**：补 §3.1/§3.2 两个小修；建 `cg_ro` 只读账号；配 1 个 MySQL 实例。
2. **M2（承重，先做）**：写**进程内 `pkg/tool/builtin/db_query.go`**（`database/sql`，`cg_ro` 连接，**只读**：只允许 `SELECT/SHOW/EXPLAIN`，绕开 Bash 沙箱网络，返回结构化行）+ 采出 §11.4 快照，逐字段核对真值。host 级信号（磁盘/CPU）若需要：自建实例用 DB 宿主上的轻量 exporter 或一个**网络放开的运维执行 lane**，不要靠默认沙箱 Bash。
3. **M1**：写 `mysql-collect`/`mysql-diagnose`/`mysql-safe-ops` 三技能 + `db-ops-mysql.json`**异常工作流（diagnose→safety-gate→remediate→verify）** + plan 模型路由。
4. **M4(只读先行)**：cron 两级触发（采集+gate 在 job 里）+ **每实例并发锁** + 基线 memory，**alert-only**（不挂 remediate）；造异常验证 采集→判定→kimi 诊断→飞书告警，且健康不烧 kimi。
5. **M3**：上 `cg_ops` + allowlist/denylist + dry-run + **陈旧目标复核** + 审批门 + 审计；db_query 工具加写模式（受守卫约束）；先放 L0，再灰度 L1。
6. **M5**：verify-goal 闭环（**验收前重采**）+ runbook 沉淀；按信心逐步开放 L1 自动、L2 审批自治。

## 11.15 配置样例
**plan 路由**（`config.json` 的 `ai.plans`）：
```jsonc
"db-ops-mysql": {
  "modelAlias": "ollama:gemma4:26b-a4b-it-qat",
  "fallbackAliases": ["kimi:kimi-k2"],
  "roles": {
    "mysql-collector":     "ollama:gemma4:26b-a4b-it-qat",
    "mysql-operator":      "ollama:gemma4:26b-a4b-it-qat",
    "mysql-diagnostician": "kimi:kimi-k2"
  }
}
```
**异常工作流**（`~/.claude-go/workflows/db-ops-mysql.json`）——**采集与 gate 在 cron job 里、不在此工作流**（§11.0-3 / §11.9）；该工作流仅在 cron 判定异常后被 `team/create`，快照随 objective 注入。上线初期删 `remediate` 跑"诊断→告警"只读链：
```jsonc
{
  "name": "db-ops-mysql", "mode": "pipeline", "producesCode": false,
  "stages": [
    {"name":"diagnose", "role":"mysql-diagnostician", "prompt":"已采快照与触发项: {objective}。做根因分析与分级方案(结构化, 引用指标数值, 不执行)。"},
    {"name":"remediate","role":"mysql-operator",      "prompt":"执行 {diagnose} 中 level<=允许级 的动作; 写前先 dry-run; KILL 类先用 db_query 复核目标一致性; 危险动作请求审批。", "dependsOn":["diagnose"]},
    {"name":"verify",   "role":"mysql-diagnostician", "prompt":"用 db_query 重新采集当前指标, 验收是否回落, 未解决给 gap。", "dependsOn":["remediate"]}
  ]
}
```
> `collect`+`gate` 不作为工作流 stage（确定性短路 healthy 的需求 pipeline 满足不了，见 §11.0-3）；它们由 cron job 内的 db_query 工具 + Go 判定代码完成。
**cron**（每实例一条，异常才升级）：
```jsonc
{"schedule":"*/1 * * * *","jobType":"command",
 "payload":"采集 mysql 主库快照并 gate; critical 时 team/create db-ops-mysql objective=主库",
 "chatID":"<运维群>"}
```

**群体智能诊断工作流（可选升级）**：

- **方案 A：Swarm 模式**（复杂/跨域/高歧义根因）。把工作流名直接设 `"swarm"`，`ProductionTeamManager` 会进入 `executeSwarm`（`pkg/agent/teams.go:1071`）。此模式下不再按 `db-ops-mysql.json` 的固定 stage 执行，而是由 `SwarmOrchestrator` 动态分解子任务、多分析师并行、融合结论。
  ```jsonc
  // cron 在 critical 且触发群体智能条件时调用
  {"schedule":"*/1 * * * *","jobType":"command",
   "payload":"采集 mysql 主库快照并 gate; critical 且多根因假设时 team/create db-ops-mysql-swarm objective=主库",
   "chatID":"<运维群>"}
  ```
  团队创建时指定 `workflow: "swarm"`：
  ```jsonc
  {"name":"db-ops-mysql-swarm","workflow":"swarm","objective":"主库 critical: ... 快照: {...}","chatID":"<运维群>"}
  ```

- **方案 B：Adversarial 模式**（中等复杂度，需要多轮评审收敛）。保持 `db-ops-mysql.json`，但把 `mode` 改为 `adversarial`；`diagnose` 阶段会由 generator 出方案，内置或自定义 reviewer 角色评审，直到 `QualityTermination` 收敛。
  ```jsonc
  {
    "name": "db-ops-mysql-adversarial", "mode": "adversarial", "producesCode": false,
    "stages": [
      {"name":"diagnose",  "role":"mysql-diagnostician", "prompt":"..."},
      {"name":"safety-review", "role":"mysql-safety-reviewer", "prompt":"评审 {diagnose} 的安全性, 给出 pass/fail 与修改意见。", "dependsOn":["diagnose"]},
      {"name":"ops-review",    "role":"mysql-ops-reviewer",    "prompt":"评审 {diagnose} 的可操作性与回滚方案。", "dependsOn":["diagnose"]},
      {"name":"remediate", "role":"mysql-operator", "prompt":"仅执行已通过评审的 level<=允许级 动作; 先 dry-run。", "dependsOn":["safety-review","ops-review"]},
      {"name":"verify",    "role":"mysql-diagnostician", "prompt":"...", "dependsOn":["remediate"]}
    ]
  }
  ```

> 无论方案 A 还是 B，最终执行前都必须经过 §11.7 的确定性安全护栏。群体智能只影响“诊断建议的质量”，不影响“能否自动动手”。

## 11.16 群体智能复用落地清单

| 阶段 | 复用点 | 落地动作 | 依赖 |
|------|--------|----------|------|
| M0 | `SwarmOrchestrator` / `AdversarialRunner` 的端到端可行性 | 用 `workflow:"swarm"` 跑一个非 DB 的测试团队，确认 `notify`、检查点、融合输出正常 | 无 |
| M1 | 角色注册 + 动态工作流 | 注册 `db-ops-mysql`（pipeline）与 `db-ops-mysql-adversarial`（adversarial）两个 JSON | §3.6.3 |
| M2 | `LLMRunner` 模板变量 | `diagnose` prompt 中用 `{objective}` 注入快照，用 `{prev_result}` 读取上游输出 | §3.6.4 |
| M4 | cron + `team/create` 拉起标准/群体智能团队 | 健康时用 cron `command` 跑采集+gate；异常时按阈值决定 `team/create db-ops-mysql` 还是 `db-ops-mysql-swarm` | §3.6.2、§3.6.5 |
| M4 | `PheromoneMemory` / `ReasoningBank` 沉淀 | 每次成功诊断把“症状→根因→方案”写入 swarm_intel 持久化目录，后续同类异常优先命中 | §3.6.1 |
| M5 | `verify-goal` + `AdversarialRunner` 自治闭环 | 执行后由 `verify` 阶段重新采集并验收；未解决则把 gap 作为 objective 再起一轮 adversarial 诊断（≤N 轮） | §3.6.4 |
| 持续 | `ObservabilityBridge` 审计 | 所有 orchestrator/swarm 执行自动进入 observability 事件总线，供 Grafana/飞书追溯 | §3.6.4 |