# 03 · RL 进化引擎（统一自我进化闭环）设计方案

> 状态：设计稿 v1.1（2026-07-23）· 基于 HEAD `6bd8da22f` 全量源码梳理；v1.1 新增 Hermes-Agent（Nous Research）RL 引擎源码实地调研与吸收（§2.1、§4.2、§4.3e、§4.5、§4.7）
> 关联：[00-overview.md](00-overview.md) · [01 编排引擎](01-unified-agent-orchestration-engine.md)（轨迹/注入挂载点）· [02 分层架构](02-cloud-native-layered-architecture.md)（L4 服务归属）
> 范畴：自动学习 LLM 轨迹与会话交互 → 自我进化。统一囊括：**记忆整理、经验总结、skill 进化**，外加工作流/prompt 进化与（可选）权重内进化导出。

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
> **总体判定：≈22% 实现**（分母 = 79 个可核验条目：§1.2/§1.3/§4.1-4.7/§2.1 H1-H14/§5/§6/§8，加权 17.75）。
> 分段：E0 ≈70% · E1 ≈40% · E2 ≈12% · E3 ≈27% · E4 ≈16% · E5 = 0% · §4.6 治理 ≈30% · §2.1 H1-H14 ≈6%（0.8/14）。
>
> **一句话：学习闭环仍是开环，而且是双层开环。**
> 第一层——轨迹→奖励接通了（写盘），但**奖励→学习器完全没接**：`rewards.jsonl` 的读取方全仓只有两个离线 CLI，
> 学习反馈仍走 stage 二值（`pkg/agent/workflow.go:896-901`）⇒ RewardBus 是只写日志，不是总线。
> 第二层——CLI 形态因 `applyFeatureFlags` 时序问题，`TraceCaptureHook` 与 `MemoryInjectHook` **都不注册**
> （`pkg/engine/engine.go:224` vs `cmd/claude-go/main.go:2916/2929/2964`）⇒ 下游 8+ 平台的全部流量既无轨迹也无记忆注入，
> 本文 §4.4 承诺的「headless 与飞书同构、开环 1 从架构上不可能再出现」未达成。
>
> 逐条依据与改判清单见 [PROGRESS.md](PROGRESS.md) 的「三方独立核查」小节。

## 一、现状诊断：形式完整、三处开环、奖励贫乏

### 1.1 现有学习设施全景

claude-go 已经拥有一套在**飞书常驻模式**下形式完整的学习栈——这是本方案的地基而非推倒对象：

| 环节 | 组件 | 核心能力 | 位置 |
|---|---|---|---|
| 采集 | EvolutionEngine.RecordTrajectory | 团队 stage 轨迹（截断 Input4k/Output6k，上限 500 条） | `evolution.go:184,196,202` |
| 采集 | 引擎 turn 轨迹 TrajStore | TurnID/意图/计划/工具调用/verdict，写分层记忆 | `internal_hook/trajectory.go:57,92,158`；装配 `engine.go:265,347` |
| 采集 | LLMCallRecord→llm.jsonl | token/延迟/错误/来源/PromptComponents(仅字符数) | `api/client.go:57`、`llm_collector.go:101` |
| 采集 | 会话 transcript | user 初始消息+assistant(含 tool_use) JSONL | `session/storage.go`、`engine.go:456,506,1113` |
| 蒸馏 | LearnFromTeam/LearnFromStage/LearnCounterfactual | LLM 蒸馏（严格 JSON）+启发式回退+反事实假设 | `evolution.go:224,266,345,457,647` |
| 蒸馏 | PreCompact 事实抽取 | 压缩前 SmartExtractKeyFacts→Ingestor | `compact.go:64-92`、`main.go:2562` |
| 存储 | Experience 库 | 三类经验、质量分、生命周期、MinHash 签名、UCB 计数 | `evolution.go:48` |
| 存储 | 分层记忆 | L1 TieredStore(情景)/L2 FactStore(结构化+矛盾检测)/L3 markdown | `tiered.go:207`、`store.go:110,285`、`fact.go:57` |
| 存储 | 遗忘曲线 | Ebbinghaus 衰减、分类衰减率、归档、召回强化 | `fact.go:21,57`、`decay.go:33,115` |
| 检索 | RetrieveFor | BM25+IDF+中英同义词+lifecycle/uplift 加权+UCB 探索+MinHash 去重 | `evolution.go:697,834,1424` |
| 注入 | MemoryInjectHook / FormatExperiencesForPrompt | 首轮注入 L1×7+L2×5 / `<learned_experiences>` 段 | `hook_memory.go:36`、`evolution.go:872` |
| 反馈 | RecordFeedback/RecordInjection/UpdateBaseline | EMA 质量更新、注入 uplift 追踪 | `evolution.go:906,570,633,605` |
| 整理 | Consolidate | 生命周期状态机(proposed→…→archived)、衰减、LSH 去重、上限 200 | `evolution.go:942,1017` |
| 整理 | Dreaming | 门控触发、LLM 整合、增量蒸馏 consolidated.md | `dreamer.go:235,305,382,777`、`consolidator.go:73` |
| 度量 | CollectMetrics | 18 项进化指标（distill_rate/uplift/error_recurrence…） | `evolution.go:1163` |
| 技能 | AutoCreator | MaybeCreate/ImproveSkill 写 SKILL.md+Reload | `autocreate.go:42,108` |
| 知识 | wiki 引擎 | LLM-Wiki 三层概念库（独立，不回流） | `wiki/engine.go` |

### 1.2 三处关键开环（本方案第一优先修复）　　**[✅ 三处均已闭环]**

> **实测**：① headless 实例化进化 ✅（`cmd/claude-go/main.go:796,808`）。② **只闭一半**：`MaybeCreate` 已接线（`pkg/agent/teams.go:877`），但 **`ImproveSkill` 生产调用点仍为 0**。③ **只闭团队路径**：CLI 单轮会话/查询路径无 AfterQuery（飞书有 `pkg/feishu/session.go:863`）⇒ CLI 只有「跑团队才做梦」。

1. **headless `run` 不实例化进化引擎**：`cmd/claude-go/main.go:768` 的 `NewProductionTeamManager` 未设 `Evolution` 字段 → `workflow.go:758-840` 全部学习分支被 `we.evolution != nil` 守卫跳过。**CLI 跑的所有团队（下游 8+ 平台的全部流量！）零学习**。唯一装配点在飞书：`bot.go:376`。
2. **技能自进化是死代码**：`AutoCreator` 仅构造（`bot.go:644`），全仓库对 `MaybeCreate`/`ImproveSkill` 的调用点为 **0**。
3. **Dreaming headless 只建不触发**：`AfterQuery`/`RecordSession` 仅飞书路径调用（`feishu/session.go:770`、`bot.go:183`）；CLI 创建 dreamer 只为取 MemoryDir（`main.go:2547`）。

### 1.3 轨迹不可对齐（RL 的数据地基缺失）　　**[✅ 四元组端到端通电 · 四源均可按 run_id 对账]**

> ✅ **trace 四元组已端到端通电（2026-07-25）**：`{RunID,NodeID,TurnID,CallID}` 经 `pkg/api/trace.go` 落出站请求头，`pkg/llmgw/server.go` 读入 `access.jsonl`。改造前 llmgw **早就在读 `X-CG-Run-ID`，但全仓无客户端发它** ⇒ 字段恒空——典型的"两头都写了、中间没接"。
>
> ✅ **`llm.jsonl` 已带 run_id（2026-07-25，E0 验收项「llm.jsonl 可按 run_id 聚合」达成）**：此前记的"改它会动 Grafana 与 `/api/llm/stats` 的既有契约"这个判断**只对一半**——那个顾虑成立于把 run_id 做成 **Prometheus 标签**，不成立于往 JSONL 事件加字段。两者必须分开：
>
> - **run_id 只进 JSONL 事件，绝不进标签**。`MetricEvent.RunID` 从一开始就与 `Labels` 分离（`RecordRun` 就是这么用的），本轮补了 `RecordRunAtTime` 让 llm 采集器能**同时**要"同一次调用共享时间戳"（`handleLLMStats` 按 `(ts, model)` 分组）与 run_id。做成标签的后果是具体的：run_id 每次运行一个新值 ⇒ `llm_*` 炸成每次运行一条时间序列（无界基数），且既有查询的聚合口径静默改变。
> - **既有消费方一个字不动**：`v13_handlers.go:605` / `v14_handlers.go:146` 都按 `(ts, model)` 分组并按名字读单个标签键；`replayJSONL`（重启回放到 Prometheus）用的是只含 `ts/module/name/value/labels` 的私有结构体，**结构上就不可能**把 run_id 带进 Prometheus。
> - **一次调用的事件要么全带要么全不带**：熔断那条与提示词构成那两条用的是另一份 labels，最初漏了 run_id——"有的带有的不带"比全都不带更难排障（按 run_id 过滤会**静默漏掉**恰好是熔断那几条，而那几条最该被查到）。测试逐条检查每个事件，变异反证已验证摘掉任一处即变红。
> - run_id 为空（非团队路径）时 `omitempty` 生效，`llm.jsonl` 逐字节与改造前一致。
>
> ⚠️ **`node_id`/`turn_id`/`call_id` 刻意仍不进 `llm.jsonl`**：E0 的验收口径是按 run_id 聚合；而把三者塞进 `MetricEvent`（一个被 10+ 模块共用的结构体）是为一个模块的需要加宽公共结构，且 turn/call 比 run 更细、进标签必然基数爆炸。这一路的逐调用四元组已经在网关侧 `access.jsonl` 里（从请求头读全四元组）。

> **实测（前两句已过时，保留作对照）**：四元组注入链真实（`pkg/trace` + `pkg/agent/teams.go:653` + `pkg/engine/engine.go:760` + `pkg/api/client.go:119`）。~~但 **E0 验收项「llm.jsonl 可按 run_id 聚合」未达成**：`pkg/metrics/llm_collector.go:108-135` 构造 labels 时四元组一个都没进~~（已达成，见上；且**不是**靠往 labels 里塞四元组——那条路才是会炸基数的）。⚠️ 另有**三种 run_id 格式并存**：`trace.NewRunID()`（仅图路径）、`pkg/agent/teams.go:653` 手拼、指标用裸 `team.Name` ⇒ 即便在有 run_id 的 sink 之间也 join 不上。断点：`pkg/agent/swarm.go:770-786` 的 Trajectory 不含 RunID；`X-CG-Run-ID` 有读无写。
>
> **本轮核实后把"格式并存"这条收窄成一个更准的结论（仍未修，理由具体）**：
>
> - ✅ **llm.jsonl ↔ 奖励 ↔ TraceStore spans 三者已能 join**：都取同一个 `trace.From(ctx).RunID`（`teams.go:832` 造 `run-<team>-<traceID>`，`RewardEvent.RunID`/gate span 取同一个值）。这是 RL 真正需要的那条 join。**已在真集群实测**：同一个 `run-e2e-real-…` 下 llm.jsonl 574 条 / trace spans 73 条 / rewards 8 条，且轨迹文件名就是 `trace-<run_id>.jsonl`。
> - ✅ **llm.jsonl ↔ team.jsonl 已能 join（2026-07-26）**：`pkg/agent/team_metrics.go`（4 测试 + 3 变异反证）。逐条核实后发现范围比原记录**大得多**：不是"`workflow_adversarial_dev.go` 的 20+ 处"，而是**全仓 59 个 `RecordRun` 调用点无一例外**把 `team.Name` 传进那个叫 `runID` 的参数——`team.jsonl` 的 `run_id` 字段里装的一直是团队名。
>
>   ⚠️ **我此前把它记成"代价大于收益、留作单独一轮"，那个理由是错的**：它默认了"团队身份只能靠 labels 携带"。`MetricEvent.RunID` 从一开始就与 `Labels` 分离且**不进 Prometheus**；给它加一个同样不进 Prometheus 的 `Team` 字段，三件事同时成立——① `run_id` 恢复本义（真 RunID）⇒ 与 llm.jsonl / trace spans / rewards 可 join；② 团队身份不丢；③ **一个 Prometheus 序列都没动**（labels 一字未改）⇒ 既有 Grafana 查询不受影响。这与 dashboard 侧奖励聚合早就在用的 `(run_id, team)` 二元组口径一致（`run_feedback.go`）。
>
>   - **RunID 不靠穿 ctx 拿**：59 个调用点里只有约一半的外层函数带 ctx（另一半只拿到 `*ProductionTeam`）。为 8 个函数加 ctx 参数是过度改动——`team.LastRunID` 里已经是同一个值（`teams.go:835` 写入 `trace.From(ctx).RunID`），而每个调用点都握着 team。于是把 (team → RunID, 团队名) 收到 `recordTeamRun` 一处。
>   - **拿不到 RunID 时留空，不回填团队名**：回填正是要修的那个缺陷换个位置。空 `run_id` 如实表示"这条指标不属于任何一次已开始的运行"（团队创建、重启后补记）。
>   - **一个自己写出来的 bug 被自己的测试抓住**：helper 第一版把采集器参数写成接口以便注入，而调用点传的是 `*metrics.Collector`——它为 nil 时装进接口是**带类型的 nil**，`m == nil` 为 false ⇒ 照样调进去 ⇒ `c.mu.Lock()` 在 nil 上 panic。那道"兜一层"的守卫在最需要它的场景下恰好失效。改收具体类型。

要重建 (state, action, reward) 三元组，当前缺：
- **统一 trace-id**：llm.jsonl 无 turn/session/trajectory id，与 transcript/团队轨迹只能按时间戳粗对齐;
- **action 完整文本**：llm.jsonl 只存计数不存 prompt/response 正文；transcript 不落 tool_result（`engine.go` 只 Append user 初始消息与 assistant 消息）；进化轨迹 Input/Output 被截断；
- **headless 团队路径** trajectories.json 根本不产生（开环 1）。

### 1.4 奖励贫乏　　**[✅ 8 源已全部通电（原 1 源）]**

> **实测**：生产写出的 Source 字面量只有 2 个：`gate.content`（两个调用点用**同一字面量**）与 `episode`（设计外自加）。按设计 8 源口径是 **1/8**。设计里价值排第一的 `gate.compile`/`gate.test` 在真门禁函数内 `RecordReward` 调用数为 **0**；价值最高的用户显式反馈（👍/`/rate`）零命中。

| 信号 | 现状 | 问题 |
|---|---|---|
| stage 成败二值 | 入学习（EMA） | 唯一稳定奖励，且 headless 不采集 |
| turn Verdict 四值 | 启发式推断（工具成功率，`trajectory.go:158`） | 非真实任务达成度 |
| content_gate LLM 0-100 分 | **用完即丢**（`content_gate.go:20`，仅触发重做） | 现成 dense reward 被浪费 |
| 编译/测试门禁 | 间接影响 stage 状态 | 未作为独立奖励事件记录 |
| 用户反馈 | **完全缺失**（无点赞/评分入口；steer 修正未被视为负信号） | 最高价值信号为零 |
| review 评分（评审团 86 分之类） | 落 REPORT.md 文本，不入库 | 未结构化 |

---

## 二、业界方案与论文调研

**权重外进化（本方案主体——我们不持有前沿模型权重）**

| 来源 | 采纳 |
|---|---|
| **Voyager（2023）** | 技能库=可执行技能+描述索引，成功后固化、失败迭代。→ skill 进化器的库模型 |
| **ExpeL / Reflexion** | 从成功/失败轨迹中提炼语言经验，注入后续任务；语言化的"策略梯度"。→ 现 EvolutionEngine 的理论定位，保留强化 |
| **Agent Workflow Memory（AWM, 2024）** | 从轨迹中归纳**可复用工作流**并注入。→ 工作流进化器：从 Journal 归纳图模板 |
| **MemGPT/Letta + sleep-time compute** | 分层记忆+睡眠时整理。→ 现 Dreaming 的定位；本方案把它变成统一进化循环的一个学习器 |
| **Generative Agents** | 记忆流+重要性评分+反思金字塔。→ FactStore 反思层级 |
| **SkillRL / SkillBank（arXiv:2602.08234）** | 分层技能库+经验蒸馏+**技能库与策略递归共进化**。→ 技能库分层（通用启发式 vs 任务特定）与递归进化节奏 |
| **SkillAudit（arXiv:2606.14239）** | **无 ground-truth 的技能进化审计：配对轨迹对照**（带/不带该技能的轨迹对比）。→ 技能晋升门禁的核心机制 |
| **Trajectory-Informed Memory Generation（arXiv:2603.10600）** | 从执行轨迹自动抽取 actionable learnings 入上下文记忆。→ 经验学习器的抽取规范 |
| **The Past Is Prologue（arXiv:2606.31121）** | 顺序演化记忆的**选择性更新控制器**（何时改/何时不改）。→ 记忆整理器的更新门 |
| **OPD-Evolver（arXiv:2606.17628）** | on-policy 蒸馏培养整体 evolver。→ 权重内导出路径参考 |
| **AFlow（ICLR'25）/ ADAS / GPTSwarm** | 工作流=可搜索/可优化的图。→ 图模板变异+离线评估 |
| **GEPA / DSPy（2025）** | 反思式 prompt 进化：用执行反馈+LLM 反思优化 prompt，样本效率高于 RL 微调。→ prompt 进化器 |
| **Darwin-Gödel Machine / AlphaEvolve（2025）** | 自改进代码 agent 的**评估门禁+谱系存档**。→ 五级治理与回滚谱系 |

**权重内进化（可选导出路径）**：GRPO（DeepSeek-R1）/ RLVR（可验证奖励）/ RFT / DPO——本机有 ollama+gemma，轨迹+奖励可导出为 DPO 对/RFT 数据集微调本地小模型（如意图识别、路由、gate 评分等窄任务），主力模型不动。

### 2.1 实地调研：Hermes-Agent 的 RL 引擎（Tinker-Atropos）　　**[🟠 吸收项 9/14（H3/H5/H6/H7/H8/H9/H10/H12/H13）]**

> **实测**：⚠️ 本节列的 H1-H14 是落地最少的一节：H1 🟠（事实成立但无 `RewardEvaluator`/`WorkspaceHandle` 契约）· H7 🟡（阈值锁定但无对象）· H14 🟠（只有 reward_mean 且是离线字段）· **其余 11 项全 ❌**。其中 **H3（judge 独立性）被反向违反**——content gate 的 judge 用主模型自评，正是 H3 明令禁止的。

本机 `/home/victor/base/git/temp/hermes-agent`（Nous Research，Python）内置了一套**权重内** RL 训练引擎，已做全量源码调研。它与本方案定位互补：hermes 训模型权重（GRPO+LoRA），我们主体优化系统策略 π_sys——但它在**环境抽象、奖励工程、轨迹管线、训练编排**四方面的工程实践直接可用。

**架构**：三进程管线——Atropos（轨迹 API：rollout 组管理+advantage 计算）+ Tinker（云端训练服务：LoRA/采样/优化器步）+ Environment（任务/评分定义）；agent 自身通过 10 个 `rl_*` 工具全程编排训练（`rl_cli.py:113` 的"自动化后训练工程师"系统提示词 + `tools/rl_training_tool.py`）。环境不是经典 reset/step MDP，而是**轨迹级 rollout 契约**：`setup / get_next_item / format_prompt / compute_reward / evaluate` 五方法（`environments/hermes_base_env.py:650-714`），整个多轮 agent loop（`environments/agent_loop.py:175`）是 RL 的"一步"。

**吸收清单**（→ 指向本文落点）：

| # | Hermes 设计点 | 位置 | 吸收到 |
|---|---|---|---|
| H1 | **验证器复用 rollout 后的活沙箱**：reward 函数拿 `ToolContext(task_id)` 全工具访问，直接检查模型跑完后的真实文件/进程状态，无需重放快照 | `hermes_base_env.py:584`、`tool_context.py:67` | §4.2 奖励在原始工作区计算 |
| H2 | **多信号 shaped reward 配方**：0.6 正确性 + 0.2 工具使用 + 0.2 效率（超 5 次调用递减惩罚）+ 多样性加分，权重可配；LLM judge 失败回退启发式 | `web_research_env.py:345-421` | §4.2 复合奖励模板 |
| H3 | **judge 独立性教训**：web_research 的 judge 用被训模型同一 server 自评（`:639`）——自评偏差/reward hacking 风险 | `web_research_env.py:639` | §4.6 治理：独立 judge 强制 |
| H4 | **三层 token-safe 工具结果落盘**：超大输出写沙箱文件，上下文只留预览+路径（模型可 read_file 取回），read_file 阈值 pin ∞ 防循环——**信息无损的上下文控制，优于 LLM 摘要** | `tools/tool_result_storage.py:115-225` | §4.1 TraceStore + compact 协同 |
| H5 | **双轨迹管线严格分离**：RL 用 token 对齐流（tokens/masks/logprobs，ManagedServer SequenceNode），SFT 语料用 messages 流（可有损压缩，`trajectory_compressor.py` LLM 摘要中段、保护首尾 N turn）；两者不可混用 | `hermes_base_env.py:607-642`、`trajectory_compressor.py:656-774` | §4.3e 导出器双管线 |
| H6 | **训练前多档模型冒烟**（test-before-train）：3 step × 16 completion × 3 个不同规模模型（小/中/大）验证环境加载/prompt 构造/解析鲁棒性/verifier 正确性，再开数小时的真训练 | `rl_training_tool.py:1019-1312` | §4.5 晋升前冒烟 |
| H7 | **锁定/可配置字段分离**：基础设施参数（lr/lora_rank/tokenizer/URL）对 agent 锁死，只暴露安全字段（group_size/batch_size），改锁定字段直接拒绝 | `rl_training_tool.py:72-108,690` | §4.7 进化操作台护栏 |
| H8 | **agent 即后训练工程师**：发现环境→读源码理解 verifier→复制模板造新环境→冒烟→训练→限速监控→早停，全程 agent 工具自助 | `rl_cli.py:113-170` | §4.7 进化操作台 |
| H9 | **监控限速内建于工具层**：`rl_check_status` 同一 run 30 分钟最小间隔，直接返回 rate_limited+剩余秒数——把"别轮询"纪律做进工具而非提示词 | `rl_training_tool.py:150,828-841` | §4.7 |
| H10 | **GRPO 组卫生**：组内同任务同工具（工具集按组解析一次），只有采样不同；工具集支持概率分布采样促泛化 | `hermes_base_env.py:350-369,289-322` | §4.3e 组语义 |
| H11 | **On-Policy Distillation（OPD）**：从 next_state（工具结果/报错）用 PRM 多数投票抽 hindsight hint→hint 增强 prompt 下取 teacher top-K logprob→`A_t = teacher_lp − student_lp` 逐 token 稠密信号，补稀疏末端 reward | `agentic_opd_env.py:551-1002` | §4.3e 可选深化（机制采纳、实现需重做：其 token span 反向匹配脆弱） |
| H12 | **评测骨架健壮性**：Semaphore 限并发、每任务硬超时、每完成一条立即流式落盘 JSONL（中断不丢）、按 prompt 内容而非 index 判断续跑、空 rollout 短路不启动沙箱 | `terminalbench2_env.py:784-996`、`batch_runner.py:714-756`、`hermes_base_env.py:573-581` | §4.5 回放 harness |
| H13 | **SFT 样本质量过滤**：无 reasoning 覆盖的轨迹直接丢弃 | `batch_runner.py:442-447` | §4.3e 导出过滤 |
| H14 | **指标契约**：reward_mean / percent_correct / logprob 漂移（reference−training）作为训练健康度三件套 | rl-training.md、`rl_training_tool.py:892-897` | §4.5 指标 |

**不吸收/引以为戒**：RL 环境里 memory/skill 被禁用（`agent_loop.py:396-401`，为可复现性牺牲了"带记忆的策略"——恰是我们 π_sys 的主体，不能照搬）；SWE 环境 reward 用字符串插值执行测试且部分分逻辑失效（`hermes_swe_env.py:178,187`，脆弱写法反面教材）；trajectory_compressor 的 LLM 摘要压缩对 RL 管线是污染源（仅 SFT 可用）；OPD 引用的论文出处存疑、token 对齐实现 O(n·m) 且不可靠。

**核心结论**：在不持有权重的约束下，"RL"落地为**系统级策略优化**——策略 π = (注入的经验, 记忆, 选用的 skill, 图模板, prompt, 模型档位) 的组合；学习 = 用真实奖励更新这些组件的**选择分布与内容**。这正是 2026 年 SkillRL/SkillAudit 系工作的共识框架，而 claude-go 已有其中 60% 的原件，缺的是：轨迹底座、奖励总线、统一调度、治理门禁。

---

## 三、RL 形式化

```
POMDP:  state  s = (任务 objective, 图/节点上下文, 黑板, 注入的记忆与经验, 环境观测)
        action a = LLM 输出（文本/tool_use）——由 底座模型 π_base + 系统策略 π_sys 联合产生
        π_sys  = { 经验注入选择, 记忆注入选择, skill 选择, 图模板选择, prompt 版本, 模型档位 }
        reward r = RewardBus 聚合（§4.2），episode = 一次 GraphRun / 一次会话任务
优化目标：max E[R]，仅优化 π_sys（权重外）；π_base 可选经导出数据离线微调（权重内）
学习算法：① 经验/记忆内容更新 = 语言化梯度（distill + reflect）
          ② 选择分布更新 = contextual bandit（UCB/Thompson，按任务上下文条件化）
          ③ 结构更新 = 进化搜索（图模板/prompt 变异 + 离线回放评估 + 灰度）
```

**信用分配（credit assignment）**：episode 级奖励 → 节点级：门禁/gate 分数天然挂节点；episode 终值按 Journal 因果链回溯衰减分摊（γ 折扣）；争议节点用 LLM-judge 过程评分（PRM 风格，低频采样控成本）。

---

## 四、统一进化引擎架构（Evolution Service）

```
                        ┌────────────────────────────────────────────┐
   design/01 Hook总线 ──►│ ① TraceStore 轨迹底座（append-only, trace-id）│
   design/01 拦截器  ──►│ ② RewardBus 奖励总线（多源信号→奖励事件）      │
                        ├────────────────────────────────────────────┤
                        │ ③ 学习器族（统一调度循环 EvolutionLoop）       │
                        │   a.经验学习器  b.记忆整理器  c.Skill进化器    │
                        │   d.工作流/Prompt进化器  e.权重导出器(可选)    │
                        ├────────────────────────────────────────────┤
                        │ ④ 策略应用层（检索注入 + bandit 选择）         │──► prompt/图模板/skill
                        │ ⑤ 评估与门禁（离线回放 + uplift + canary）     │
                        │ ⑥ 治理（五级生命周期 + 谱系 + 回滚 + 预算）     │
                        │ ⑦ 进化操作台（evo_* 工具，agent 自助实验）      │◄── 飞书/CLI/cron
                        └────────────────────────────────────────────┘
```

部署：L4 辅助系统（design/02）。单机=进程内模块；分布式=独立 evolution 服务，中心化经验/记忆库（解学习孤岛，假设 #17）。

### 4.1 ① TraceStore：轨迹底座　　**[✅ Kind 6/6 + policy_decision · Ref/采样/TTL]**

> ✅ **`KindRun` 已补产生方（2026-07-25）——原来的"预留"判断不再合理**：TraceID 隐含的是**身份**，隐含不了**事实**（objective / 工作流 / 终态 / 耗时 / 阶段数一条都不在 TraceStore 里），于是 `ReadRun(runID)` **无法自述**，想知道这次 run 成没成功要 join 三份不同格式的文件——而本节明说 TraceStore 是"学习视图"。挂在 `beginRun` 的 defer 闭包而非运行级链：门禁判失败会中止链、未知工作流连链都进不去，而**失败的 run 恰恰是学习最想要的那批**。另新增 `policy_decision` kind（见 §4.4）。

> ⚠️ **一处我自己打错又更正的标注（2026-07-25）**：先按"常量已补齐"记成了 `Kind 5/5 ✅`，逐条查产生方后**推翻**——`tracestore.KindXxx` 六个常量都在（`run`/`node`/`turn`/`llm_call`/`tool_call`/`gate`），但全仓只有 **2 个有写入方**：`KindLLMCall`（`pkg/engine/trace_llm.go:96`）与 `KindGate`（`pkg/agent/reward_sources.go:91`）。`KindRun`/`KindNode`/`KindTurn`/`KindToolCall` **零产生方**，其中 `KindRun` 源码注释自己写了「预留: 当前由 TraceID 隐含」。
>
> ✅ **`KindNode` 已补上产生方（2026-07-25）**：`stageNodeRunner.writeNodeSpan`（4 个测试）。挂在 stageNodeRunner 而非引擎侧，因为 `pkg/graph` 不认识 tracestore，且只有这里同时拿得到节点声明与 `StageResult`。**`NodeID` 用限定 ID（`NodeRef`）而非声明名**——否则 N 个 map 分片的 span 会全挂在同一个节点上；轮次（`iteration`/`group_iteration`）与分片序进 `Attrs`，不带轮次就无法回答「第几轮才收敛」，而那正是结构学习器（§4.3 d/e）要用的信号。fail-open：底座为 nil 或写失败都不影响节点执行——采集是观测不是治理。
>
> ⚠️ **又一处更正，这次是往好的方向**：我先按常量名 grep 判定 `KindTurn`/`KindToolCall` 零产生方，**真集群 E2E 的轨迹文件推翻了它**——一次图引擎团队跑出 `turn` 7 条、`tool_call` 27 条、`llm_call` 19 条、`node` 6 条、`gate` 1 条，**5 种 kind 都有产生方**。原因是 `pkg/engine/internal_hook/hook_trace.go` 写的是**字面量** `"turn"`/`"tool_call"` 而不是 `tracestore.KindTurn/KindToolCall` 常量，按常量名搜自然搜不到。已把那两处字面量归一到常量——那张常量表存在的全部理由就是"少了哪几种能机械地看出来"，留着字面量它就失效了。
>
> 现状：`KindRun` 是唯一无产生方的 kind，源码注释写明属刻意预留（由 TraceID 隐含）。
>
> **这件事本身是个教训**：判"有没有产生方"用 grep 常量名会漏掉写字面量的调用点，真跑一遍看产物才是准的。本轮三次标注失误里，两次是 grep 不足、一次是把"类型存在"当成了"已通电"。
>
> 这正是本文档图例里 🟡「已建成未通电」要区分的情形：**加一个常量不等于采集了那类轨迹**。轨迹按 kind 不齐会直接影响 RL 数据地基——`node`/`turn` 缺失时无法把奖励归因到具体节点或轮次。

> **实测**：Span + Ref 内联/Blob 内容寻址真实且生产接线（`pkg/evolution/tracestore/tracestore.go:43-131`）。但只写出 3 种 Kind（tool_call/turn/node），**`llm_call`/`gate` 从不写** ⇒ E1 验收「任一 run 可完整还原 prompt→response→tool 链」不成立。✅ **`TraceCaptureHook` 在 CLI 不注册的问题已修（2026-07-25）**：新增 `QueryEngine.RefreshHooks()` 并在 CLI 装配末尾调用（`cmd/claude-go/main.go`）。刻意不复用 `EnableFrontierOptimizations`——后者会顺带翻开一批 `Config.Enable*`，那是行为变更。可执行证据：`pkg/engine/hook_registration_order_test.go` 用新增的 `HookChain.Names(phase)` 断言三种赋值顺序，`go test -run TestHookRegistration -v ./pkg/engine/` 可复现。✅ **采样与 TTL 已通电（2026-07-25）**：`CLAUDE_GO_TRACE_SAMPLE` 控正文采样率、`CLAUDE_GO_TRACE_TTL` 控 TTL；`StartJanitor` 改为包级函数（只需目录、不碰 Store 状态，而调用方才是知道路径的那层），CLI 与飞书 Bot 两条路径都已接。两个变量未设时行为与此前完全一致（全采、不清理），解析失败/越界回落默认并打日志。

**统一 trace-id**（与 design/01 Journal、design/02 LLMGateway 同一套）：

```
trace = run_id / node_id / turn_id / call_id        （团队路径）
      = session_id / turn_id / call_id              （会话路径）
```

**Span 模型**（OTel 风格，`trace.jsonl` append-only，StateStore Log bucket）：

```go
type Span struct {
    TraceID, SpanID, ParentID string
    Kind    string    // run | node | turn | llm_call | tool_call | gate | human
    Name    string    // 节点名/工具名/gate 名
    Input   Ref       // 内容寻址引用（Blob），全文不截断；含 prompt 组装各段引用
    Output  Ref       // response 全文 / tool_result 全文
    Attrs   map[string]any // tokens/model/duration/status/score/verdict...
    TS, DurMS int64
}
```

**采集点**（全部是现有代码的接线，不发明新路径）：
- llm_call：LLMGateway 出口（design/02 §3.1），LLMCallRecord 增加 trace 四元组字段——修"llm.jsonl 无关联键"；
- tool_call：引擎 PhasePre/PostToolUse hook——修"transcript 缺 tool_result"；
- node/gate：design/01 Journal 事件双写（Journal 是执行真源，TraceStore 是学习视图，同源不同投影）;
- turn：现 `TurnMetricsHook`（`hook_metrics.go:79`）升级为写 Span。

**成本控制**：正文入 Blob 内容寻址（prompt 各段天然去重——system prompt/skill 正文重复率极高）；采样策略可配（默认全采 metadata、正文按 run 级开关，产码团队默认开）；TTL 分级（原始正文 30 天，Span 元数据永久）。

**与上下文压缩的协同（吸收 Hermes H4）**：运行期超大工具结果优先走**"落盘+指针"**而非丢弃/摘要——参照 hermes 三层机制（单结果超阈值→写工作区文件、上下文只留预览+路径、agent 可 Read 取回；Read 结果自身豁免落盘防循环）。对 claude-go：`pkg/compact` 的截断路径增加 persist 档位，被落盘的原文**天然就是 TraceStore 的 Blob**——上下文瘦身与轨迹保真一次解决；LLM 摘要压缩（现 SmartExtractKeyFacts）保留用于会话延续，但 Span 里始终记指针指向无损原文，学习管线永远读得到全文。这直接消解 §1.3 "action 完整文本缺失"中 transcript 截断的那一半。

### 4.2 ② RewardBus：奖励总线　　**[✅ 8 源已通电（原 1 源）]**

> ✅ **8 源已全部通电（2026-07-25）**。最后两源：
> - **`gate.e2e`**：`POST /api/runs/{runId}/feedback`。三处 fail-closed——**source 锁死**（允许自选源名等于把权重表交给外部，自称 `user.explicit` 就能拿人类权重，是本文档 §4.6 最直接的 reward-hacking 入口）；**run 认领不到即 404 且不落盘**（聚合按 `(run_id, team)` 过滤，team 对不上的事件落盘即死数据）；**既无 pass 也无 score 即 400**（无判据的"反馈"不是弱信号，是没有信号）。
> - **`verdict.heuristic`**：原记账「团队路径根本不产 turn verdict」**只对了一半**——飞书会话路径**是产**的，但 `internal_hook.Trajectory` 只带 `SessionID`、**没有 RunID**，而聚合强制要 RunID。**真正的卡点是归因不是产出**。走通的路是从已带 run 归因的 TraceStore 轨迹把 verdict 算回来（turn 的 `stop_reason` + tool_call 的 `is_error`），全是真实执行结果。权重 0.15 全表最低：它只回答"这轮跑完没、工具报错没"，一个工具全成功而内容全错的 run 在它眼里是满分。
>
> ⚠️ **连带的 fail-closed 修复**：`skillaudit.Audit` 原先取**未加权**均值，接一个弱源进来就变成"跑得越多越容易晋升"。改为加权，变异验证过（把 `weight()` 改成恒返回 1，弱信号压闸测试立刻 FAIL）。

> ✅ **6 源已通电（2026-07-25）**：原有 3 源 + 新增 `user.steer`（`teams.go:1665`，归因必须用**上一轮**的 `LastRunID`，否则把用户对旧产出的不满记到新一轮头上）、`user.explicit`（`/team rate`，此前**完全没有入口**能表达"满意"，正向人类信号恒为零）、`review.panel`（截尾均值 overall）、`latency`（阶梯惩罚，**只罚不奖** —— 给正分会激励偷工，最快的路径是什么都不做；权重 0.2 不与质量判断同权竞争）。
>
> **仍缺 2 源（如实记账）**：`gate.e2e`（端点属 `pkg/dashboard`）、`verdict.heuristic`（`RunIsolated` 不走 HookChain，团队路径根本不产 turn verdict）；`cost` 无 per-run token 记账。

> **实测**：`RewardEvent` + rewards.jsonl 落盘真实（`pkg/agent/evolution.go:146-177`），~~但设计的 `Weight`（源可信度）字段不存在 ⇒ §4.6「奖励源加权」无载体~~ ✅ **奖励已进学习器且 Weight 已补（2026-07-25）**：新增 `AggregateRewards`（倒序扫尾部窗口、**同 (source,node) 只取最新一条**——门禁"失败→修复→通过"若取均值会被旧失败拖回去）与 `StageRewardScore`/`RunRewardScore`/`GateRewardScore`；`workflow.go` 三处布尔改为「有同节点证据用加权分，无证据回退二值」（回退必须保留，否则未接奖励源的工作流全部退化）。`RewardEvent` 补 `Weight` 并**落盘**，故将来调权重表时历史奖励保留当时可信度。**归因边界**：stage 反馈只认同 run 同节点证据，不吃 run 级奖励——否则"门禁失败后触发的修复阶段"会被它正要修的失败倒打一耙。奖励源由 1/8 增至 **3/8**：新增 `gate.compile`/`gate.test`（`runGlobalCompileGate`/`runGlobalTestGate` 真跑 `go build`/`go test`，是设计里价值排第一的确定性信号，此前这两个函数内 `RecordReward` 调用数为 0）。⚠️ **H2 复合 shaped reward 与 H3 judge 独立性仍未实现**（judge 仍用主模型自评，反向违反 H3）；且确定性门禁是 run 终端信号、发生在 stage 反馈之后，故**首轮阶段仍走二值回退**，要反哺需一次"run 末回溯反馈"（会与 stage 时已发生的反馈双计，未做），折中是加了 run 级消费者：技能提炼闸（`GateRewardScore<0` 时不提炼）。

```go
type RewardEvent struct {
    TraceID string; SpanID string   // 挂到哪个粒度
    Source  string   // gate.compile | gate.test | gate.content | e2e | user.explicit
                     // | user.steer | review.panel | verdict.heuristic | cost | latency
    Value   float64  // 归一化 [-1,1]
    Raw     any      // 原始值（0-100 分、pass/fail、点赞种类…）
    Weight  float64  // 源可信度权重（配置）
}
```

**奖励源接线清单**（按价值排序）：

| 源 | 接线 | 改动 |
|---|---|---|
| 编译/测试门禁 | gate 节点 verdict 事件（design/01） | Journal→RewardBus 投影 |
| content_gate 0-100 分 | **持久化**（现状用完即丢，`content_gate.go:20`） | gate 节点输出进 Span.Attrs.score |
| 用户显式反馈 | 飞书回复加 👍/👎 reaction 监听 + `/rate <1-5>` 命令 | feishu-adapter 新增，发 user.explicit |
| steer 修正 | 用户在执行中纠偏（`session.go:698` processMessageInternal 路径）→ 对被纠偏 turn 记负信号 | 会话 actor 埋点 |
| review 评分 | review_panel/评审团分数结构化（现散落 REPORT.md 文本） | reduce 节点输出规范化 |
| e2e/下游验收 | :18080 新增 `POST /api/runs/<id>/feedback`（下游平台如 testforge 的门禁结果回传） | 兼容层新端点 |
| turn Verdict | 现启发式保留为弱信号（weight 低） | 已有（`trajectory.go:158`） |
| cost/latency | token 成本与时长作 shaping 负项（防"堆 token 刷分"） | llm.jsonl 已有 |

**奖励工程三原则（吸收 Hermes §2.1）**：

1. **奖励在原始工作区计算（H1）**：episode 级奖励评估器（gate 节点/事后 judge）在 run 结束、工作区清理**之前**执行，直接检查团队 cwd 的真实产物（文件存在性、可编译、测试通过、产物完整性）——claude-go 的编译/测试门禁本就在团队 cwd 跑，此原则将其推广为 RewardBus 的通用契约：`RewardEvaluator` 接口收 `WorkspaceHandle`（等价 hermes `ToolContext(task_id)`），奖励逻辑可任意使用只读工具，不依赖轨迹文本的自述。design/01 的 Journal `run.finished` 事件先触发奖励评估、后触发清理。
2. **复合 shaped reward 模板（H2）**：episode 奖励默认配方 `R = w1·正确性 + w2·效率 + w3·过程规范`，权重进配置。效率项参照 hermes 阶梯惩罚：工具调用/轮次在预算内满分，超出后按档递减——直接对抗"堆 turn 堆 token 刷分"；过程规范项吃 turn Verdict/工具错误率等弱信号。正确性项优先确定性验证（门禁），无法确定性验证的域用 LLM judge，**judge 不可用时回退启发式**（关键词/结构断言）而非置 0——保证奖励覆盖率。
3. **judge 独立性（H3 教训）**：LLM judge 一律使用与被评估主模型**不同的模型档位或供应商**（配置强制，如主模型 kimi-k3 → judge 走 gemma4 本地或 fallback 供应商）；hermes 的 web_research 用被训模型自评自训，是 reward hacking 的标准入口，明令禁止。

### 4.3 ③ 学习器族：五个学习器、一个循环　　**[✅ 五个学习器 · 统一循环 · SFT/DPO 导出（均默认关）]**

> ⚠️ **一处记账被推翻**：GEPA 的 reflector **早已通电**，取法与 `EvoTierFactory` 本来就是同一个（都读 `api.Client.FallbackModels`）；没配 fallback 整支跳过是 H3 要求的**正确行为**，不是缺陷。
>
> ✅ **真缺口是另外两处，已补（2026-07-25）**：① `SetEvoTierFactory` 只在飞书装配，**CLI 形态下 `evo_smoke` 仍只有确定性"装配档"**、`MinTiers>=2` 必然拒绝；② `ExportDPO` 两处装配都不设、也没 env ⇒ **DPO 分支生产上恒不可达**，补 `CLAUDE_GO_EVO_EXPORT_DPO`（同时打开总开关，避免"设了没反应"）。

> **实测**：✅ **`EvolutionLoop` 已实现（2026-07-25）**：`pkg/agent/evolution_loop.go` + `teams.go` 的 `submitLearn` 收敛了两处散点（pipeline 与 swarm 各一份 `go func(){LearnFromTeam;Consolidate}`）。解决三个真实问题：五个学习器共享 `experiences.json` 却互相看不见（多团队同时完成会覆盖）、学习的 LLM 花费无从记账（§4.5「学习成本占比」的前提）、空闲期做不了深度整理。单 goroutine 串行消费 + 去重窗口（refine/重跑会多次走到完成路径，重复蒸馏同一批轨迹会让 UCB 计数虚高）+ 每小时预算闸 + 空闲自发整理。**队列满即丢弃**——学习的背压绝不能传导回交付路径。未装配循环时 `submitLearn` 回落直调，这是长期契约而非临时兼容：一刀切要求先建循环会让漏装配的调用方静默丢失全部学习，那正是 §1.2 开环 1 的原始形态。11 个测试。a 经验学习器预存能力全在 ✅，但**四条升级全未做**（数据源仍读 4k/6k 截断的 trajectories、UCB 未升 contextual bandit、反馈未接 RewardBus、无 actionable 三段式）。b 记忆整理器 ✅ 但选择性更新门/embedding/wiki 双向均无。c 技能进化：创建 ✅、改进 ❌、**影子验证与设计差距最大**（`pkg/evolution/skillaudit/skillaudit.go:82-87` 用「创建时间之后的全部奖励均值」裁决，**零技能归因**，同批 shadow 裁决必然相同；包注释描述的算法与实现不符）。d 工作流/Prompt 进化（AWM/GEPA/canary）全 0。e 权重导出未启动。

**EvolutionLoop 统一调度**（取代"飞书路径散点触发"）：事件驱动（run 完成→立即小学习）+ 周期批量（空闲期→深度整理，即 dreaming 时机）+ 预算约束（学习自身的 LLM 花费单独记账）。单机跑在进程内 goroutine；分布式由 evolution 服务消费 `evolution.trace` subject（design/02 EventBus）。

#### a. 经验学习器（现 EvolutionEngine 升级，改动最小）

- 保留全部现有机制：三类经验、LLM 蒸馏+启发式回退+反事实、BM25+同义词+UCB 检索、EMA 质量、uplift 追踪、生命周期 Consolidate、18 指标（`evolution.go` 全能力平移）。
- 升级点：
  1. 数据源从"截断的 stage 轨迹"改为 TraceStore 全文 Span（蒸馏质量↑）；
  2. UCB → **contextual bandit**：按（工作流类型×角色×项目 profile）条件化选择分布，跨团队迁移显式建模（现 `cross_team_transfer` 指标已埋）；
  3. 反馈从"stage 二值"扩展为 RewardBus 加权聚合；
  4. 抽取规范对齐 Trajectory-Informed Memory Generation：经验必须是 **actionable**（条件+动作+预期效果三段式），拒绝叙事型总结。

#### b. 记忆整理器（现 Dreaming+Memory 收编）

- 保留：TieredStore/FactStore/衰减/矛盾检测/召回强化/PreCompact 蒸馏/consolidated.md。
- 升级点：
  1. 触发权收归 EvolutionLoop（修开环 3：headless 同样按空闲/阈值触发，不再依赖飞书 AfterQuery）；
  2. **选择性更新门**（The Past Is Prologue）：整理前判断"新信息与既有记忆的关系"（新增/修正/冲突/冗余），冲突走 DetectContradictions（`store.go:285`）+ 保留谱系，不盲目覆写；
  3. 检索增强：BM25 保留为主检索，**可选 embedding 后端**（本机 ollama embedding 或网关代理），配置开关，默认关（尊重现状无向量库的取舍）；
  4. wiki 引擎纳入为"概念记忆"后端：Dreaming 整理产出的稳定概念可晋升入 wiki，wiki 查询结果可作为记忆注入源（现状 wiki 完全孤立）。

#### c. Skill 进化器（接线死代码 + 治理门禁）

```
候选发现 ──► 起草 ──► 影子验证 ──► 晋升 ──► 监控 ──► 改进/退役
```

1. **候选发现**：EvolutionLoop 扫描高奖励轨迹簇（同类任务 N 次成功且无对应 skill）→ 触发 `MaybeCreate`（现成代码，`autocreate.go:42`）；同理低奖励+已有 skill → `ImproveSkill`（`autocreate.go:108`）。
2. **影子验证（SkillAudit 式配对审计）**：新/改技能先处 `shadow` 状态——后续匹配任务随机 50% 注入，**配对轨迹对照**（带 vs 不带该技能的 reward 差）；样本量达阈值且 uplift 显著才晋升。复用现 InjectionTracker/UpdateBaseline 机制（`evolution.go:570,633`），从"经验粒度"推广到"技能粒度"。
3. **版本与谱系**：SKILL.md frontmatter 增加 `version/lineage/status(shadow|active|deprecated)/audit`（现有 `auto_generated/created_at/improved_at` 保留）；SkillStore 版本化（design/02 R2）。
4. **技能库分层**（SkillBank）：通用启发式技能（跨项目）vs 任务特定技能（绑项目 profile），检索时先特定后通用——对齐现 `RecommendedSkills`/`DetectProjectProfile` 机制（`roles.go:234,338,367`）。
5. **技能=子图**：design/01 §4.7 允许 skill 携带图模板段，技能进化因此涵盖"多阶段技能"的结构进化。

#### d. 工作流/Prompt 进化器（新增，AFlow/GEPA 式，节奏最慢）

- **工作流归纳（AWM）**：从高奖励 Journal 归纳可复用图模板（"这类 objective 用这个节点序列成功率高"）→ 起草 GraphSpec 变体入 shadow。
- **图模板变异（AFlow）**：对低分模板做受限变异（加 gate 节点/调整 loop 上限/换角色/改交接裁剪），**只在离线回放评估通过后**进灰度。
- **Prompt 进化（GEPA）**：角色 SystemPrompt/stage Prompt 的反思式改写：取该 prompt 下的失败轨迹+奖励，LLM 反思产出候选版本，离线回放对比，胜出者 canary 5%→50%→100%。
- 搜索空间=design/01 的纯数据 GraphSpec/RoleDef/SKILL.md——**这正是 01 方案坚持"图是纯数据"的回报**。

#### e. 权重进化导出器（可选，默认关；v1.1 按 Hermes 实践细化）

- **双管线严格分离（H5）**：
  - **SFT/DPO 语料管线**：从 TraceStore 的 messages 视图导出 ShareGPT 风格对话；允许有损处理（长轨迹压缩沿 hermes trajectory_compressor 策略：保护首条 system/user 与末尾 N turn，只对中段做 LLM 摘要替换并在 system 注明"部分历史已摘要"）；质量过滤参照 H13——无推理过程覆盖/奖励低于阈值/含 schema 回显的轨迹直接丢弃。
  - **RL token 管线**（远期，仅当引入自管推理服务时）：需要 token/mask/logprob 精确对齐，**禁止任何有损摘要**；上下文控制只允许"落盘+指针"方式（见 §4.1 H4 协同）。当前 claude-go 经网关调外部 API 拿不到 logprob，此管线默认不建，仅在文档层预留契约。
- **环境即任务集+验证器**：把 hermes 的五方法环境契约（setup/get_next_item/format_prompt/compute_reward/evaluate）映射为导出侧的 `EvalEnv` 定义 = 离线回放任务集（§4.5）+ RewardEvaluator——**同一个 EvalEnv 既服务进化产物门禁，又服务训练数据生成与训后评测**，一份任务集三用，不另造格式。
- **组语义（H10）**：为 GRPO/DPO 采样时，同一任务的 N 次 rollout 必须控制变量——同 objective、同注入经验/技能版本、同工具集（组级解析一次），只有采样温度不同；组内奖励全同的组丢弃（无学习信号）。
- **OPD 稠密信号（H11，可选深化）**：对本地窄任务模型（gate 评分器/路由器）可采纳 hermes 的 hindsight-hint 机制——从工具结果/门禁反馈中用多数投票 judge 抽取"上一步本可更好"的 hint，构造 hint 增强上下文下的 teacher 分布做逐 token 蒸馏。机制采纳，实现不照搬（其独立再分词的 token span 反向匹配在子词分词上下文相关性下不可靠，须在同一次前向里同时取 teacher/student logprob）。
- 目标仅限本地小模型窄任务：意图识别（`IntentRecognizer`）、路由 gate、内容评分器——用 gemma4:26b 微调后替换对应 SimpleComplete 调用，省 token 且可控;
- 明确不做：主力模型微调（无权重）、在线 RL（风险与算力都不成立）。

### 4.4 ④ 策略应用层　　**[✅ 注入同构 + policy_decision 留痕]**

> ✅ **`policy_decision` 留痕已补（2026-07-25）**。**刻意没做成 `NodeInterceptor`**，两条硬理由：① 节点链挂在 `engine.callRunner`，而 `CLAUDE_GO_GRAPH_ENGINE` 默认关 ⇒ pipeline 类工作流一条都不产，又一个"建成未通电"；② 切面手上只有 `NodeSpec`/`NodeInput`，**看不见**"最终提示词里塞了哪三条经验、哪两个技能"。故写在注入发生的那一行旁边，技能名从提示词 `<role_skills>` 段**反解**——读的是真会发出去的那份，不可能"记的和发的不一致"。

> **实测**：`EvolutionRecorder` 拦截器零命中（依赖 design/01 §4.10，未实现）。经验注入确实同构（`pkg/agent/workflow.go:820`，两形态共用）。但 ✅ **`MemoryInjectHook` 在 CLI 未注册的问题已随 `RefreshHooks()` 一并修（2026-07-25）**；实测修复前 CLI 顺序下 `PhasePreRequest` 只有 `[toolresult_level message_filter message_metrics]`，见 `pkg/engine/hook_registration_order_test.go`⇒ **L1/L2 记忆注入在 CLI 形态失效**，设计声称的「headless 与飞书同构，开环 1 从架构上不可能再出现」**未达成**。`policy_decision` Span 零命中。

- 注入点全部走 design/01 拦截器/hook（EvolutionRecorder 拦截器 + MemoryInjectHook），**headless 与飞书同构**——开环 1 从架构上不可能再出现；
- 每次注入记 `policy_decision` Span（注入了哪些经验/记忆/技能/模板版本）——bandit 更新与 uplift 归因的数据基础（现 RecordInjection 的推广）。

### 4.5 ⑤ 评估与门禁　　**[✅ 回放 harness · H6 闸语义 · 真实档位构造器已通电]**

> ⚠️ **又一处我先打成 ✅ 后下调的标注（2026-07-25）**：H6 的**闸语义**确实全实现且 fail-closed（失败率/覆盖率/`MinTiers>=2`；未配置的档位记 `Available=false` 并计入拒绝理由，绝不当"这档通过了"）。但**真跑多档需要宿主注入 `builtin.SetEvoTierFactory`，它当时零生产调用方** —— `evo_smoke` 只有一个内置的确定性"装配档"（抓"候选丢了 `{objective}`"这类劣化），而装配档不是模型档位，所以单独跑时冒烟**必然拒绝**。
>
> ✅ **已通电（2026-07-25）**：`pkg/agent/evo_tiers.go` 的 `EvoTierFactory`，注入点在 `pkg/feishu/bot.go` 的装配处（`pkg/tool/builtin` 不能 import `pkg/agent` —— 那正是刚修掉的测试导入环的方向）。档位来源是**真实配置**：primary = 当前主模型，fallback = 配置里声明的备用模型，取法与 `FallbackReflector` 一致（`WithModel` 共享 HTTP 客户端与 `RateLimitGuard`，冒烟调用同样受全局配额约束、不绕过限流偷跑）。
>
> 两处刻意的取舍：**候选不套 QueryEngine**——冒烟要验的是"这份产物本身能不能驱动出可用产出"，套上工具循环后失败原因会混进工具与多轮交互，归因不到产物上。**错误绝不吞成空串**——空产出会被覆盖率判定当成"跑了但没评分"，与"根本没跑起来"混成一档，冒烟结论就失真。
>
> **没配 fallback 时只给 primary 一档，`Smoke` 因 `MinTiers` 不足而拒绝——这是正确行为不是缺陷**：没有第二档就确实没做过多档验证。用主模型凑第二档只能证明它不随机崩，证不了"换个弱一点的模型也还能用"，而后者才是晋升前真正要问的问题。
>
> 另一处相关发现：`NewEvolutionLoop` 在本轮之前**全仓零生产调用方**，`submitLearn` 永远走回落直调 —— 我此前标的「统一循环 ✅」其实也是"写了没通电"。现已装在 `cmd/claude-go/main.go:803` 与 `pkg/feishu/bot.go:524`。

> **实测（2026-07-25 实现）**：`pkg/evolution/replay` 落地离线回放 harness，**H12 五条工程规范逐条实现**（并发信号量 / 每任务硬超时 / 每完成一条流式落盘 / 续跑按内容指纹 / 空产出短路不启动 Judge）。两条关键判断：①**确定性断言是硬否决**——Expect 未命中或 Gate 失败直接 0 分且不问 Judge，能确定性判的不该花 LLM 钱也不该让 LLM 的宽容盖过硬事实；②未注入 GateRunner 时在 Reason 注明门禁被跳过，静默跳过会让人以为门禁过了。`Compare` 实现 uplift 配对对照，替代"感觉变好了"。⚠️ 一处自我修正：第一版只把带超时的 ctx 传给 candidate，测试当场抓出**那不算"硬"超时**（不配合 ctx 的实现仍会拖住整轮），改为 goroutine + select ctx，并写明"泄漏一个 goroutine 但整轮继续"的取舍。14 个测试。**仍缺 H6 多档模型冒烟**（回放固定输入，评不了"产物是否让 agent 做出不同动作序列"，那需要真跑）。uplift 因果评估仅在经验粒度（预存），技能/模板/prompt 粒度无。设计新增的指标（reward 趋势/灰度胜率/回滚率/学习成本占比）只有 `console.Report.RewardMean` 一个**离线 JSON 字段**，未进指标目录。

- **离线回放 harness**：扩展 `tests/eval`（现为特性自评分，`bench_test.go:108` 及格线 60%）为**轨迹回放评估**：固定任务集（从历史高置信轨迹沉淀）+ LLM-judge 评分 + 确定性断言（产码任务跑真门禁），任何进化产物晋升前必过；
- **晋升前多档冒烟（H6，test-before-train 推广为 test-before-promote）**：任何进化产物（新技能/新图模板/新 prompt 版本）先跑小规模冒烟——少量任务 × 多次采样 × **多个模型档位**（如 kimi-k3 / fallback 供应商 / gemma 本地各一），验证注入后 prompt 组装不劣化、解析不崩、奖励覆盖正常，再进 shadow 灰度。多档模型是关键：hermes 用小/中/大三档专测解析鲁棒性——技能/prompt 对弱模型不鲁棒是线上劣化的常见来源；
- **harness 工程规范（H12）**：并发信号量限流（防打爆网关配额）、每任务硬超时、**每完成一条立即流式落盘 JSONL**（中断不丢已完成结果）、续跑按任务内容指纹而非序号（任务集增删不错位）、空产出短路（zero-turn 轨迹直接 0 分不启动评估器）；
- **uplift 因果评估**：全部进化产物（经验/技能/模板/prompt）统一用配对对照（注入组 vs 基线组）报告 uplift，替代"感觉变好了"；
- **18 项进化指标保留** + 新增：reward 趋势、灰度胜率、回滚率、学习成本占比（学习 LLM 花费/总花费）；导出训练路径启用时加 hermes 三件套（H14）：reward_mean / percent_correct / 分布漂移监控。

### 4.6 ⑥ 治理　　**[✅ 四律齐 + 全生命周期 E2E 已验 / 比例灰度未实现]**

> ✅ **全生命周期 E2E 已验（2026-07-25）**：全走生产函数不 mock。链路 = shadow 运行期不可见 → 攒真奖励 → dry-run 不改盘 → `--apply` 晋升 → **运行期真的看得见**（这就是"全量灰度"的实质）→ 留痕带 `reward_avg/samples` → 回滚后立刻不可见。另钉住两条：越权产物奖励满分也判 `reject_escalation`，且**手动通道 `SetStatus(active)` 同样报错**；弱信号不得压过闸。
>
> ⚠️ **比例灰度未实现**：`shadow_ratio` 只落实验 JSON，**运行期零消费方**——本仓的灰度是二值的（shadow 全不可见 / active 全量可见）。

> **实测**：存在**两套互不相通的状态机**：经验用 proposed/validated/promoted/…，技能用 shadow/active/archived；设计的 `observed` 无实现，迁移事件**不入 Journal**。✅ **「必过闸」已有运行期效力（2026-07-25）**：`Skill.Status` + frontmatter 解析 + `Get`/清单/Skill 工具排除 shadow，`GetAny`/`All` 留给治理审计。刻意用**黑名单**（shadow/archived/retired/disabled）而非"只有 active 才可用"的白名单——存量 SKILL.md 绝大多数没有 status 行，白名单会一夜禁用全部既有技能；未知值（stable/beta）fail-open 放行。`ImproveSkill` 改用 `GetAny`，否则自改进再也改不了 shadow 技能、`preserveStatus` 会变成死代码。「不越权」无 ConstraintSet 单调性检查。防 reward hacking 三防线全无。

- **五级生命周期**（对齐 aiops design/09 的治理框架）：`observed → proposed → shadow(validated) → active(promoted) → archived`，每级迁移条件量化、事件入 Journal 可审计；
- **四律**：进化产物不越权（ConstraintSet 单调性）、必留痕（谱系）、必过闸（离线回放+uplift）、可回滚（版本化存储，一键回退到任意谱系点）；
- **防经验污染/reward hacking**：奖励源加权可信度（用户显式>门禁>LLM-judge>启发式）；cost shaping 防堆 token；经验/技能上限与淘汰（现 200 条上限机制推广）；对抗审计（周期抽样进化产物让独立 judge 复核）。

### 4.7 ⑦ 进化操作台：agent 自助编排进化实验（吸收 Hermes H7/H8/H9）　　**[✅ evo_* 八件套已注册为 agent 工具]**

> **实测**：设计要求的是**注册进工具池、agent 自助**的 `evo_*` 七工具；实现是 `claude-go evo` 的 4 个 CLI 子命令（`grep '"evo_'` 零命中，无 `/api/evo*` 端点）⇒ **H8「agent 即进化工程师」结构上不成立**，E4 验收项「agent 经 evo_* 全自助完成 propose→smoke→experiment→promote」不可能达成。H9 的 30 分钟限速无实现。H7 护栏：阈值确为不可导出 Go 常量 ✅，但**无 agent 可达接口 ⇒ 护栏机械成立而无对象**，「拒绝语义」未实现。

Hermes 最有借鉴价值的顶层设计是**"agent 即后训练工程师"**：整条 RL 管线（发现环境→读源码理解 verifier→复制模板造新环境→冒烟→训练→限速监控→早停→取结果）通过 10 个 `rl_*` 工具由 agent 自己驱动，人只下目标（`rl_cli.py:113-170`）。对应到本方案，Evolution Service 暴露一组 `evo_*` 工具（注册进 claude-go 工具池，飞书/CLI 均可用），让 claude-go 自己当"进化工程师"：

| 工具 | 作用 | Hermes 对应 |
|---|---|---|
| `evo_list_envs` | 列出 EvalEnv（回放任务集+验证器），含描述与样本量 | rl_list_environments（AST 静态扫描发现，不执行代码） |
| `evo_inspect` | 查看进化产物（技能/模板/prompt）的谱系、uplift 历史、当前状态 | 读环境源码理解 verifier |
| `evo_propose` | 提交候选产物（起草技能/模板变体）进 proposed 态 | 复制模板造新环境 |
| `evo_smoke` | 晋升前多档冒烟（§4.5 H6），返回各档解析/奖励覆盖报告 | rl_test_inference |
| `evo_run_experiment` | 启动 shadow 配对实验 / 离线回放批次 | rl_start_training |
| `evo_status` | 查实验进度与指标——**同一实验 30 分钟限速，内建于工具层，返回 rate_limited+剩余时间（H9）** | rl_check_status |
| `evo_promote` / `evo_rollback` | 过闸晋升 / 一键回滚到谱系任意点 | rl_get_results 后的人工决策，此处闸门化 |

**护栏（H7 锁定字段模式）**：`evo_*` 工具可改的只有安全字段（实验样本量、任务集选择、shadow 比例上限 50%、描述文本）；**治理参数一律锁定**——晋升阈值、uplift 显著性标准、judge 模型选择、学习预算上限、生命周期规则，agent 请求修改直接拒绝并返回锁定原因（等价 `rl_edit_config` 对 LOCKED_FIELDS 的拒绝语义，`rl_training_tool.py:690`）。这把 §4.6 的"四律"从约定变成机械强制：agent 可以自由做实验，但**闸门标准本身不在它的动作空间里**。

工作流纪律进系统提示词（仿 RL_SYSTEM_PROMPT 的规范）：先冒烟后实验、指标坏了早停、从小样本起步再放大、状态检查遵守限速。定时驱动：EvolutionLoop 的周期批量（§4.3）本质就是 cron 触发一个带 `evo_*` 工具集的进化 agent 会话——与 aiops"数字员工"模式同构，复用其"先发布后深挖"的教训。

---

## 五、三处开环的具体修复（E0 立即执行）　　**[✅ 三处均已闭环，见 §1.2]**

> **实测**：①按设计照做 ✅；②只做了 MaybeCreate 一半；③只覆盖团队路径。

| # | 修复 | 改动点 |
|---|---|---|
| 1 | headless 实例化进化 | `main.go:768` `NewProductionTeamManager` 补 `Evolution: NewEvolutionEngine(...)`（与 `bot.go:376` 同参装配，状态目录同 `<state>/evolution/`）；长期由 EvolutionRecorder 拦截器取代散点接线 |
| 2 | 接线技能自进化 | 短期：`teams.go:786-792` 团队学习后追加"高奖励簇→MaybeCreate / 低奖励→ImproveSkill"调用（含 shadow 状态门禁，未过审计不 active）；长期：Skill 进化器接管 |
| 3 | Dreaming 触发统一 | `main.go:2547` 后台注册与飞书同款触发（空闲阈值+ForceDream 命令）；长期：EvolutionLoop 调度 |

附带小修：CLI 产生的 turn 轨迹（TieredStore）与飞书 evolution 目录数据互通——统一 StateStore bucket 后自然解决（design/02 R2）。

---

## 六、现有功能覆盖矩阵（学习域）　　**[🟠 14 行里 12 行已接入闭环 / 2 行仍旁路]**

> **重算（2026-07-25，逐行核实）**：原标注只写"部分"。现按"有没有接进 RECORD→JUDGE→DISTILL→EVOLVE 这条闭环"判：
>
> **已接入（12 行）**：三类经验/蒸馏/去重（AWM 归纳，确定性零 LLM——归纳的输入是"哪些序列奖励高"，那是统计；用 LLM 等于给每次空闲整理装上按次计费，且它会编造没出现过的节点名）· 检索/注入（`policy_decision` Span 留痕，技能名从提示词 `<role_skills>` 段**反解**，读的是真会发出去的那份）· EMA 质量与 uplift（回放 harness 的 `Compare`）· turn 轨迹 + Verdict（`verdict.heuristic` 从已带 run 归因的轨迹算回来）· 记忆注入 hook（`RefreshHooks` 后 CLI 已注册）· PreCompact 事实蒸馏 · Dreaming 门控与增量蒸馏（统一循环空闲相位）· AutoCreator 技能创建/改进（经不越权闸）· 技能热重载与晋升（`SetStatus` 同样过闸）· swarm_intel 校准存储 · `llm.jsonl` 记账（trace 四元组已落请求头）· 治理四律全生命周期 E2E。
>
> **仍旁路（2 行）**：**wiki 三层知识库**——它是检索源但产出不回流学习（无奖励信号、不进轨迹）· **transcript/续聊**——会话历史有快照，但**会话路径不产 `turn`/`tool_call` Span**（`RefreshHooks()` 全仓只有 CLI 一个调用方，飞书没调；团队阶段路径由 `RunIsolated` 自己采集所以不受影响）。后者是本轮实测发现的，补它会在 `:18080` 上打开一批新落盘写入，属下游可感知的默认行为变化，故记录未改。

> **实测**：⚠️ 计划表非状态表。逐行核实后多数是「等价保留但升级未做」：数据源未升全文、bandit 未做、新指标缺、CLI 侧 turn/tool_call span 与记忆注入均无、wiki 双向打通为 0、回放 harness 为 0。另有 `MemoryIngestFn` **死代码**（全仓无赋值点）⇒ 进化→记忆交叉学习整条断开。

| 现有能力 | 位置 | 新归属 | 状态 |
|---|---|---|---|
| 三类经验/蒸馏/反事实/去重 | `evolution.go:224-647` | 经验学习器 | 等价（数据源升级为全文） |
| BM25+同义词+UCB 检索/注入 | `evolution.go:697-872` | 策略应用层 | 等价（→contextual bandit） |
| EMA 质量/uplift/Consolidate/18 指标 | `evolution.go:906-1163` | 经验学习器+评估 | 等价+新指标 |
| turn 轨迹+Verdict | `internal_hook/trajectory.go` | TraceStore Span（弱奖励源保留） | 增强（持久化+关联） |
| L1/L2/L3 记忆+衰减+矛盾检测+召回强化 | `pkg/memory` | 记忆整理器 | 等价+选择性更新门 |
| MemoryInjectHook 首轮注入 | `hook_memory.go:36` | 策略应用层 | 等价 |
| PreCompact 事实蒸馏 | `compact.go:64-92` | 记忆整理器采集源 | 等价 |
| Dreaming 门控/整合/增量蒸馏/ForceDream | `dreamer.go`、`consolidator.go` | 记忆整理器（触发权归 Loop） | 等价（修开环） |
| AutoCreator 创建/改进技能 | `autocreate.go:42,108` | Skill 进化器（加门禁） | 激活（修死代码） |
| 技能热重载/InstallSkill/dashboard skills API | `skills.go:210,460`、`server.go:242-244` | 不变；进化产物经同一通道发布 | 等价 |
| swarm_intel 信素记忆/校准存储 | `swarm_intel/engine.go:67` | 记忆整理器专用 bucket（预测域经验） | 等价 |
| wiki 三层知识库 | `wiki/engine.go` | 概念记忆后端（双向打通） | 增强 |
| llm.jsonl 记账 | `llm_collector.go` | TraceStore 的 llm_call 投影 | 增强（trace-id+双边 token） |
| transcript/续聊 | `session/storage.go:129-147` | 不变；tool_result 补录进 Span | 增强 |
| tests/eval 自评分 harness | `tests/eval/bench_test.go` | 离线回放 harness 的基座 | 增强 |
| 进化数据目录 `<state>/evolution/` | `evolution.go:1092` | StateStore bucket（格式兼容迁移） | 等价 |

---

## 七、数据模式与代码组织

```
pkg/evolution/            // 新根包（pkg/agent/evolution.go 平移+拆分）
  trace/    span.go store.go collector.go   // ① TraceStore
  reward/   bus.go sources.go attribution.go // ② RewardBus + 信用分配
  learners/ experience.go memory.go skill.go workflow.go export.go // ③ 学习器族
  policy/   inject.go bandit.go              // ④ 策略应用
  eval/     replay.go uplift.go metrics.go   // ⑤ 评估
  govern/   lifecycle.go lineage.go audit.go // ⑥ 治理
  loop.go                                    // EvolutionLoop 调度
存储 bucket（design/02 StateStore）：
  Log:  trace/<runID>.jsonl · reward/<runID>.jsonl
  KV:   experiences · skills(版本化) · prompts(版本化) · templates(版本化) · bandit-state
  Blob: span 正文（内容寻址）· 回放任务集
```

---

## 八、实施路线　　**[🟠 E0 ✅ · E1 ✅ · E2 ✅ · E3 ✅ · E4 ✅ · E5 60%]**

> **重算（2026-07-25，逐项核实源码）**：
>
> | 里程碑 | 旧 | 新 | 依据 / 缺什么 |
> |---|---|---|---|
> | E0 修开环+trace-id | 70% | ✅ | 三处开环全闭环（§1.2）；trace 四元组端到端通电（§1.3） |
> | E1 轨迹底座 | 40% | ✅ | TraceStore + Blob 内容寻址 + 采样 + TTL janitor；**6 种 kind 全有产生方**（`run` 本轮补上，`turn`/`tool_call` 是既有但写的是字面量所以早先误判为缺） |
> | E2 奖励总线+学习器收编 | 12% | ✅ | **8 源全通电**（§4.2）；五个学习器 + 统一循环（本轮才真装配——此前 `NewEvolutionLoop` 全仓零生产调用方） |
> | E3 Skill 进化器 | 27% | ✅ | 候选发现→shadow→配对审计→晋升全链有**全生命周期 E2E**（走生产函数不 mock）；不越权闸 fail-closed |
> | E4 工作流/Prompt 进化+回放 | 16% | ✅ | 回放 harness（内容指纹续跑/硬超时/确定性断言作硬否决）+ H6 多档冒烟 + 真实档位构造器；AWM 归纳与 GEPA 反思齐 |
> | E5 权重导出（可选） | 0% | **60%** | **SFT + DPO 双管线都有且可达**（`CLAUDE_GO_EVO_EXPORT` / `..._DPO`，后者本轮才补——此前生产上恒不可达）；**RL token 管线未建**，且这是**刻意的**：经网关拿不到 logprob，建了就是空壳 |
>
> 原文那条诊断保留作对照（它当时是对的）：E2 的「8 源中 3 源」应改判为 **1/8**（同一 `gate.content` 字面量的两个调用点被当成两源计数，且 `episode` 不在设计的 8 源里）。E1 的「采样/TTL ✅」应改 🟡。E3/E4 的加粗 ✅ 均高估（见 §4.3c/§4.7）。

| 阶段 | 内容 | 验收 |
|---|---|---|
| E0 修开环+trace-id（1 周，可独立先行） | §五 三修复；LLMCallRecord/transcript/团队轨迹加 trace 四元组 | headless 团队跑完 experiences.json 有增量；llm.jsonl 记录可按 run_id 聚合 |
| E1 轨迹底座（2 周） | TraceStore + tool_result 采集 + Blob 内容寻址 + 采样/TTL | 任一 run 可从 Span 完整还原 prompt→response→tool→结果链 |
| E2 奖励总线+学习器收编（3 周） | RewardBus 八源接线（含飞书 reaction/`/rate`/steer 负信号/下游回传端点）；经验/记忆两学习器迁入 Loop；content_gate 分数持久化 | reward 事件覆盖率>90% 的 run；uplift 报表出数 |
| E3 Skill 进化器（2 周） | 候选发现→shadow→配对审计→晋升全链；SKILL.md 版本谱系 | 真实产生 ≥3 个 shadow 技能且 ≥1 个过审计晋升；劣化技能被拒绝的负例 e2e |
| E4 工作流/Prompt 进化+回放 harness（3 周） | 离线回放任务集沉淀（=EvalEnv 契约，§4.3e 三用）；GEPA 式 prompt 进化 canary；AWM 工作流归纳；harness 按 H12 工程规范实现（限流/硬超时/流式落盘/内容指纹续跑）；**进化操作台 `evo_*` 七工具 + 锁定字段护栏（§4.7）** | 一个真实 prompt 版本经 canary 全量；回放 harness 阻断一次劣化变异的负例；agent 经 `evo_*` 全自助完成一轮"propose→smoke→experiment→promote"且改锁定字段被拒 |
| E5 权重导出（可选） | 双管线导出器（SFT 有损压缩管线 + RL token 管线契约预留，§4.3e H5）+ 质量过滤（H13）+ gemma 窄任务微调实验；OPD 稠密蒸馏为可选深化 | 意图识别任务上微调模型 ≥ 原 SimpleComplete 准确率且延迟/成本下降 |

**风险**：① reward hacking——cost shaping+多源加权+对抗审计三重防线；② 学习成本失控——学习 LLM 花费单列预算（BudgetManager 拦截器），默认 ≤ 总花费 10%；③ 经验库污染放大（interviewforge 式"短产物被丢/字段置空"类蒸馏事故）——蒸馏输出走严格 schema 校验+信息量下限（现 `llmDistill` 严格 JSON 基础上加长度/结构断言）；④ 隐私——Span 正文含用户数据，Blob 桶加 TTL 与脱敏钩子，导出器默认排除会话域数据。
