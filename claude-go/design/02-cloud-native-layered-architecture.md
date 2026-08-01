# 02 · 云端分离分层架构（分布式部署能力）设计方案

> 状态：设计稿 v1（2026-07-23）· 基于 HEAD `6bd8da22f` 全量源码梳理
> 关联：[00-overview.md](00-overview.md) · [01 编排引擎](01-unified-agent-orchestration-engine.md)（L2 内核）· [03 RL 进化引擎](03-rl-evolution-engine.md)（L4 组件）

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
> **总体判定：≈32% 实现**（分母 = 102 条可核验承诺：§1.2 假设 19 + §1.4 缺陷 3 + §3 五层子承诺 47 + §4 部署形态 5 + §6 覆盖矩阵 23 + §7 里程碑 5，加权 32.75）。
> 分段：§1.2 ≈14% · §1.4 ≈83% · §3 ≈27% · §4 ≈45% · §6 ≈47% · §7 ≈39%。
> 按里程碑：R0 ≈50% · R1 ≈60% · R2 ≈30% · R3 ≈35% · R4 ≈20%。
>
> **一句话**：接口层（R0/R1）与机制层（R3）确实写出来了且单测扎实，但**两个最关键的通电动作没做**——
> 编排器从不向队列投任务（`Enqueue` 唯一生产调用方是 HTTP handler，`pkg/agent` 零 `pkg/cluster` import），
> 生产代码从不经过 `LLMGateway`/`EventBus` 接口（`llmgw.NewLocal` 唯一调用方是自己的测试）。
> 当前状态是「分布式脚手架已就位，分布式数据流零字节」。
>
> 逐条依据与改判清单见 [PROGRESS.md](PROGRESS.md) 的「三方独立核查」小节。
>
> ⚠️ **本块总判定是 2026-07-25 首轮核查的历史快照（2026-07-27 复核，基线 b41930b2a + 工作区，落后 84+ 提交），保留作对照；现状以 §七 重算表为准（R0 ✅ · R1 70%+ · R2 70% · R3 ✅ · R4 60%）**。三句关键结论已失效：「编排器从不向队列投任务」→ `pkg/worker/broker.go:523` `remoteRuntime.Execute` 入队，经 `WrapAgentFactory`（`teams.go:361`，`--dispatch-mode queue` 下装配）接进编排执行工厂（「`pkg/agent` 零 `pkg/cluster` import」字面仍真，但结论不再成立——路径经 `pkg/worker` 间接存在）；「生产代码从不经过 LLMGateway/EventBus 接口」→ `bot.go:381` `llmgw.NewFromEnv` + 飞书 8 处 `SimpleClient{GW:}` 注入 + dashboard `SharedGateway()`，EventBus 已通 1 条生产链路（`llm_event_bus.go:114`）；「分布式数据流零字节」→ `deploy/k8s-e2e.sh` 已在真集群逐条断言分布式链路。

## 一、现状：单进程"上帝对象"架构

### 1.1 基本事实

- **整个系统是一个 Go 单进程**：飞书 `Bot`（`pkg/feishu/bot.go`）在 `NewBot` 里装配全部子系统；`SessionManager`（`session.go:106`）直接持有 apiClient / mcpMgr / skillReg / dreamer / memoryStore / taskStore / evolution / teamMgr 所有依赖指针。
- **LLM 全部走 HTTP**：唯一出口 `pkg/api.Client`（`client.go:598,983,1470`），Anthropic Messages 协议兼容 Kimi/DashScope 等网关；**不存在对外部 `claude`/`codex` CLI 的子进程调用**。
- **层间耦合四种形态**：直接函数调用（主）、进程级全局单例（`dynmcp.Manager`、`llmGlobal` 指标、observability bus）、文件系统约定（`~/.claude-go/` 目录树，`basedir.go:15`）、同进程惰性回调（dashboard→Bot 的 `TeamAction/LLMComplete/ReloadSkills/CronController`，`main.go:1091-1127`——唯一被显式设计成可分离的接缝）。
- **对外只有一个端口 :18080**：wiki API + dashboard 挂同一 mux（`wiki/api.go:66`、`dashboard/server.go:129`）；飞书经 WebSocket 长连接收消息（`bot.go:674,854`）。

### 1.2 单机假设清单（分布式障碍，19 条）　　**[🟠 9 条已破除 / 7 条部分 / 3 条原样]**

> **重算（2026-07-25，逐条核实源码）**：旧数字「0 条完全兑现」是改造前的诊断。
>
> **已破除（8 条）**：状态外置（`pkg/statestore/sqlitestore.go` sqlite 后端）· 执行体跨机（`pkg/worker/` 真 worker + `remoteRuntime`）· 共享工作区（cwd 三档位 local/pvc/git，pvc 有双向握手、git 有 ff-only 合入，**跨机产码真机 `go build` PASS**）· 跨进程任务队列（`pkg/cluster` + 租约 + 认领原子性靠 `os.Rename`）· 任务派发不再进程内（k8s-job runtime 按节点起 Job）· LLM 出口集中（网关 9 处生产依赖）· 轨迹归因跨进程（trace 四元组落请求头 → `access.jsonl`）· 动作队列跨进程消费（`ConsumeActions` + 主进程注册消费方）。
>
> **部分（7 条，2026-07-27 从「原样」移入两条）**：事件总线——`ChanBus` 是**进程内**且满即丢，只通了 1 条链路（LLM 事件 → 飞书播报），其余订阅者不迁是因为"接了会降级"而非没来得及 · 会话仍单进程（无一致性哈希）· `FileStore` 锁 per-instance、跨进程无锁（只有动作认领是原子的）· cron 仍单副本（worker 进程显式关掉它以免重复触发）· 技能/配置仍读磁盘目录（R2 的 SkillStore 入库未做）· `team.json` 投影化已实现但默认关（`pkg/agent/team_projection.go` + `CLAUDE_GO_TEAM_PROJECTION`，且 pipeline 路径仍以 checkpoints 为真源，见 design/01 §六）· 比例灰度已在 prompt 实验层通电（`learners/canary.go` + `prompt_canary.go`），skill 层仍二值。
>
> ✅ **团队 cwd 进程级共享已可隔离（2026-07-26）**：`pkg/agent/team_workspace.go`（3 测试 + 变异反证）。原缺陷是 `CreateTeam` 给每个团队都写同一个 `Cwd: ptm.cwd` ⇒ 两个并发产码团队写同一目录、互相覆盖，且**编译门禁在混着两个团队的树上跑 `go build`**——A 的门禁可能因 B 写坏的文件而失败，而报告会说是 A 的代码有问题。
>
> - **git 档本就已隔离**（`pkg/worker/workspace.go` 分支模板 `claude-go/ws/{team}`，worker 侧各自 clone/worktree），所以这条只剩**进程内直跑**这一档；那个文件的注释里也点名了这处上游边界。
> - **默认关**（`CLAUDE_GO_TEAM_WORKSPACE`）：打开后产物落点从 `<cwd>/…` 变成 `<cwd>/<团队名>/…`，而下游平台按约定路径采产物。默认打开等于在不通知下游的情况下改交付物位置——**那不是修 bug，是行为变更**。并发产码的部署应当开它。
> - **不用 `os.MkdirTemp` 给每次运行一个新目录**：同一团队的多次 run（精修/图层重试）必须接力同一份代码，临时目录会让重试从空目录跑编译门禁——与 git 档"按团队而不是按 run 分支"同一条理由。
> - **团队名来自用户输入**（飞书消息 / HTTP 载荷），所以净化 + 一道**结构性** `filepath.Rel` 越界检查两道闸都在。⚠️ 诚实记一笔：变异反证摘掉字符净化那道时测试**没红**——两道闸各自独立充分（防御纵深），不是测试无牙；已写进测试注释，提醒将来"清理冗余判断"的人。
>
> **原样（3 条；2026-07-27 修正——此处原重复粘贴了两遍「原样（5 条）」，且 `team.json` 投影化与比例灰度已移入上方「部分」）**：NATS/redis+pg 后端 · 会话一致性哈希 · RL logprob 采集（**刻意不做**：经网关拿不到）。

> **实测（2026-07-25 首轮快照，已被上方重算取代，保留作对照）**：逐条核实（详见 `PROGRESS.md` 的 19 条表）：已部分整改的是 #1 会话（新增 KV 快照但无 owner 路由）、#7 cron（租约真在 tick 路径但**仅当 `CLAUDE_GO_CRON_LEASE_DIR` 非空**，~~且漏了 `pkg/sync` 第二个进程内调度器~~——sync 已改线 TaskService（`pkg/synctask`），此条过时）、#11/#12 监听地址（新增 `wiki.apiHost` 但**默认值为空 = 绑 0.0.0.0**——这句 2026-07-27 复核仍准确，`pkg/feishu/types.go:184-188`）、#14/#15 边缘。~~其余 14 条原样~~（与上方重算矛盾，以重算 9/7/3 为准）。

| # | 假设 | 位置 | 影响 |
|---|---|---|---|
| 1 | 会话全内存、按 chatID 无法跨节点 | `feishu/session.go:107,44`；`engine.go:1512` | 多副本会话不一致 |
| 2 | transcript 绑定本机 HOME | `session/storage.go:31-37,54-64` | 会话不可迁移 |
| 3 | 输入历史本机文件 | `session/history.go:31-40` | 同上 |
| 4 | 配置层级依赖 HOME+CWD | `settings/settings.go:165-171` | 节点间配置漂移 |
| 5 | stateDir 从 CWD 推导，全子系统写本地目录树 | `basedir.go:78-86` | 状态不可共享 |
| 6 | skill 目录本机化 | `skills/skills.go:146-160` | 技能不同步 |
| 7 | cron 进程内 tick + 本地落盘 | `agent/cron.go:88-90,267-268,477` | **多实例重复触发** |
| 8 | 心跳/watchdog 只看本进程 | `coordinator.go:28-86` | 无跨节点故障检测 |
| 9 | stdio MCP=本机子进程；dynmcp 进程级单例 | `mcp/client.go:26`、`dynmcp/manager.go:31` | 远程节点无法共享 |
| 10 | 热加载轮询本机文件 | `hotreload/watcher.go:131-137` | 配置分发无通道 |
| 11 | dashboard/wiki 绑 loopback，dashboard→bot 走 127.0.0.1:18080 | `dashboard/server.go:48-51,89`、`main.go:1346,1381,1441` | 隐含同机 |
| 12 | wiki API 本机监听 | `wiki/api.go:66-69` | — |
| 13 | 指标写本地 JSONL、Prometheus 抓本地 | `metrics/llm_collector.go:69`、`jsonl_scraper.go:25,35`、`observability/global.go:41-42` | 多实例指标互不可见 |
| 14 | sandbox 默认本机执行 | `sandbox/manager.go:26-35` | — |
| 15 | 本机 LLM 端点假设（ollama localhost） | `api/client.go:545,560-578` | — |
| 16 | codeintel 索引落本机 HOME | `codeintel/global_index.go:69` 等 | 已有集中目录配置雏形 |
| 17 | 记忆/群体智能落本机 HOME | `memory/memory.go:62`、`swarm_intel/engine.go:67` | 学习状态孤岛 |
| 18 | 团队数据本机目录 | `teams.go:466-467,1633,1755-1756` | 编排状态孤岛 |
| 19 | 飞书 WS 单连接单消费者 | `bot.go:674,854` | 多副本重复消费 |

### 1.3 已有的"远程接缝"（演进抓手，6 处）

1. **K8s 沙箱执行**：命令/团队验证以 K8s Job 下发 + 共享 PVC 工作区（`sandbox/k8s_runner.go:35-58,137`）——已是"执行层横向扩展"雏形。
2. **集中式代码索引**：`CodeIntelSection.IndexBaseDir` 支持共享盘。
3. **MCP 双向远程**：客户端可连 http/SSE 远端；codeintel 可作 MCP server 对外（`codeintel/mcp_transport.go:156-158`）。
4. **Hook 远程调用**：HookConfig 支持 HTTP URL / gRPC / OPA（`hooks_grpc.go:23`）。
5. **LLM 多端点故障转移**：FallbackModels/FallbackBaseURL + 429 自动切换（`client.go:115-122,1421`）——模型层天然可独立。
6. **dashboard↔Bot 伪 RPC 接缝**：惰性回调 + `BotAPIURL`（`server.go:51`），改跨进程成本最低。

### 1.4 附带缺陷（分层时一并修）　　**[✅ token 记账 · 会话历史已修 / 🟠 `/api/*` 鉴权仍 fail-open]**

> **实测**：token 记账已修（`pkg/api/client.go:101-104` 估算回填 + `InputEstimated`），~~但设计承诺的「去掉 `>0` 守卫改显式记 0」**没做**（守卫仍在 `pkg/metrics/llm_collector.go:148`）~~。飞书会话历史已落盘 ✅。`/api/*` 鉴权 🟠：中间件真实（`pkg/httpauth`）但 **fail-open 且默认空 secret + 默认绑 0.0.0.0 ⇒ 默认部署仍完全无鉴权**。
>
> ✅ **`>0` 守卫已去掉（2026-07-25）**：改造前**那行注释就已经写着"不再用 `>0` 守卫"而守卫还在**——注释与代码相反，正是本仓吃过的"注释漂移"那一类。现在真的去掉了，并补上这个包**第一批测试**（此前 `pkg/metrics` 一个测试文件都没有）+ 3 条变异反证。
>
> - **断言的是"input 样本数 == 调用数"**，不是"有一条 `value=0` 的事件"：后者用 `if rec.InputTokens >= 0` 这种假修复也能过。守卫在时该断言给出 `got 1 want 2`。
> - **`output`/`cache`/`total`/`retry` 四项保留守卫**，这不是不一致：它们为 0 时是"这次调用确实没有该项"（没走缓存 / 没重试），显式记 0 只会给每次调用凭空多出四条恒零样本；**只有 input 为 0 是"信息缺失"这一独立事实**（Kimi 类网关常不回 `input_tokens`，是常态而非边角）。有专门测试防"顺手把四个守卫一起删了"。
> - **顺带补 `input_estimated` 标签**：估算值与网关真回的值进的是同一个 `llm_input_tokens`，不打标签的话"总量不偏小是靠字符数估算撑起来的"完全不可见——**而有人会拿这个数去和账单对账**。只在为 true 时加（与既有各条同风格），基数增量上限 ×2 而非无界；没有估算发生的部署标签集逐字节不变。

- **token 记账只计输出**：流式 input tokens 只从 `message_start` 读（`client.go:921-924`），Kimi 类网关常不回→`InputTokens=0` 被 `>0` 守卫跳过（`llm_collector.go:146-150`、`metrics_emitter.go:47-51`）。
- **dashboard `/api/*` 无鉴权**，仅靠绑 127.0.0.1（`wiki/api.go:91-101` 只保护 `/wiki/*`、`/sync/*`）。
- **飞书会话历史纯内存**，进程重启即丢（仅 team/memory 落盘）。

---

## 二、业界参考

| 来源 | 采纳 |
|---|---|
| **Temporal / durable execution** | 控制面（编排决策+事件历史）与 worker（真正执行）分离；worker **拉取**任务而非被推送；worker 无状态可任意扩缩。2026 共识：为 LLM 工作负载定制 durable 层，token 预算一等公民 |
| **Ray / actor 模型** | 有状态会话 = actor，按 key（chatID）路由到唯一 owner |
| **K8s operator 模式** | 期望状态落存储，控制器调和；心跳=lease |
| **A2A 协议** | 远程 agent 能力发现（AgentCard）、任务委派、HTTP 全双工流式——RemoteRuntime 线协议基础 |
| **MCP** | 工具能力跨机标准；平台自身能力也以 MCP server 形态对外 |
| **12-factor / 云原生** | 配置经环境+中心化、状态外置、无状态进程、端口绑定、日志作事件流 |
| **事件驱动企业级编排（arXiv:2606.20058）** | 事件总线为中枢，天然多副本 |
| **多 agent 编排综述（arXiv:2601.13671）** | 编排器/协议/运行时三分与治理边界 |
| **GoalfyMax（arXiv:2507.09497）** | 协议驱动分层：经验层独立于编排层（与 design/03 呼应） |

**核心原则：先接口后进程，先进程后网络。** 每层先抽成 Go interface + 默认本地实现（all-in-one 不变），再按需把某层换成远程实现。任何时刻 `claude-go feishu` 单二进制形态必须继续工作——这是现有 8+ 个下游平台（storyloom/mediaforge/testforge/growring…全部打 :18080）的兼容底线。

---

## 三、目标五层架构

```
┌─────────────────────────────────────────────────────────────────┐
│ L5 用户层 (Access)                                               │
│   feishu-adapter │ wechat-publisher │ CLI/REPL │ dashboard-web  │
│   │ platform-MCP-server │ HTTP OpenAPI (:18080 兼容层)           │
├─────────────────────────────────────────────────────────────────┤
│ L2 Agent 编排层 (Orchestration Control Plane)                    │
│   AgentGraph 引擎(design/01) │ TaskService │ GraphRun Journal    │
│   │ RuntimeRegistry │ 放置调度 │ watchdog                        │
├─────────────────────────────────────────────────────────────────┤
│ L3 Agent 运行时 (Agent Runtime / Worker)                         │
│   QueryEngine loop │ prompt 组装 │ skills │ tools │ sandbox      │
│   │ MCP client │ permissions │ compact                           │
├─────────────────────────────────────────────────────────────────┤
│ L4 辅助系统 (Auxiliary Services)                                 │
│   任务系统(TaskStore/队列) │ 通信系统(EventBus/Blackboard/Mailbox)│
│   │ 定时任务(cron) │ 心跳系统(registry/lease) │ 记忆·进化(design/03)│
│   │ 观测(metrics/logs/trace) │ 备份 │ wiki │ sync                │
├─────────────────────────────────────────────────────────────────┤
│ L1 LLM 引擎层 (LLM Gateway)                                      │
│   供应商路由(modelconfig) │ 重试/熔断/限流 │ fallback │ prompt cache│
│   │ token 记账 │ 配额 │ LLMCallRecord(trace-id)                   │
└─────────────────────────────────────────────────────────────────┘
```

依赖方向：L5→L2→L3→L1，L4 被 L2/L3/L5 共享；**严禁反向依赖**（现状 Bot 上帝对象即全层互抱）。

### 3.1 L1 · LLM 引擎层（LLM Gateway）　　**[🟠 TeamManagerConfig.LLM 已收编 / SessionManager 保持现状（结论）]**

> ✅ **`TeamManagerConfig.LLM` 已收编（2026-07-25）**：给 `LLMGateway` 加 `Diag`，两处 `we.llm.(*api.Client)` 具体类型断言改成 `agent.DiagLLMClient` 接口。**改造前那两处断言的失败分支是 `else { SimpleComplete }`——产出照出、日志照打，只有 diag 那一栏永远空着**，没有 error 没有 panic。
>
> ✅ **`SessionManager` 保持现状——这是结论不是待办**：引擎主链路拿的不是"发一次请求"，而是 `api.Client` 上三十余个具体字段/方法（`Guard` 还要与 advisor 客户端**共享同一实例**以免双客户端各自打满 RPM、熔断三件套、`FallbackModels` + 独立端点 + 429 连续计数触发的模型切换与冷却回退、`PromptCacheMode` 及其"因 API 错误自适应关闭"的状态位、克隆语义）。全塞进接口 = 把接口写成 `api.Client` 的镜像，抽象收益为零；**只挑几个塞更坏**——熔断/配额这类状态**跨克隆共享**才有意义，接口里丢一个字段不编译报错、只让线上少一层保护。前置条件是本节的 remote 网关（熔断/配额/fallback/prompt cache 集中到网关进程），那时上层才**不需要**这些字段。

> **实测**：`LLMGateway` 接口存在（`pkg/llmgw/gateway.go:26-33`）但 **`NewLocal` 全仓唯一调用方是自己的测试**；生产 `pkg/feishu/bot.go:305,419` 直接 `api.NewClient` ⇒「上层只依赖本接口」未发生。~~`ChatRequest` **无 Trace 字段**~~（已补：`gateway.go:37-39`，零值沿用 ctx trace）。网关只做路由 + access.jsonl：**fallback/429 熔断/配额/prompt cache 全无**，OpenAI 协议未实现。✅ **`CLAUDE_GO_LLM_GATEWAY` 已通电（2026-07-25）**：新增 `modelconfig.ApplyGatewayOverride`，覆盖点选在 `ConfigResolver` 的 4 个出口而非各 `api.NewClient` 调用点（后者散落 feishu/CLI/advisor/worker 多处，逐个改必然漏）；**fallback 端点一并改指网关**，否则主端点走网关而降级直连，集中记账在最需要时失效。~~⚠️ 但 `LLMGateway` **接口本身**仍未被生产依赖（`NewLocal` 唯一调用方仍是自测）~~ ✅ **接口已被生产依赖（2026-07-27 复核）**：`bot.go:381` `llmgw.NewFromEnv(aiClient)` 装配 + 飞书 8 处 `llmgw.SimpleClient{GW:}` 注入 + dashboard `SharedGateway()`（`llm_client.go:148`）；**remote 档也已实现**（`pkg/llmgw/remote.go`，`CLAUDE_GO_LLM_GATEWAY_MODE=remote`、无地址 fail-closed，默认 local 是刻意决定）；`ChatRequest` 已带 Trace 字段（`gateway.go:37-39`）。SessionManager 主引擎链路仍直连 `api.Client`（见上方「保持现状——结论」），其收编前置条件（熔断/配额/fallback 集中到网关）在 remote 档默认开启前不成立，故本层为：接口通电 ✅ / 主链路收编待 remote 集中化。

**现状归属**：`pkg/api.Client`（全部出口）、`pkg/agent/modelconfig`（别名→端点解析）、`ConfiguredCloneFull`（`client.go:201`）。

**抽象**：

```go
// pkg/llmgw/gateway.go
type LLMGateway interface {
    Stream(ctx context.Context, req ChatRequest) (EventStream, error)   // StreamMessage 等价
    Complete(ctx context.Context, req ChatRequest) (ChatResponse, error) // SendMessage/SimpleComplete 等价
    Models() []ModelAlias                                                // 别名表
}
// ChatRequest 必带 Trace: {RunID, NodeID, TurnID, CallID}（design/03 轨迹底座）
```

**实现两态**：
- `local`：现 `api.Client` 原样包装（默认，零网络开销）。
- `remote`：指向独立 llm-gateway 进程（OpenAI 兼容 `/v1/chat/completions` + Anthropic `/v1/messages` 双协议），网关内集中：供应商路由、fallback、429 熔断、prompt cache、**配额与预算扣减**、全量 LLMCallRecord。

**集中化的收益**：多副本/多平台（storyloom、aiops 数字员工等）共享同一配额池与熔断状态——当前每进程独立熔断，一个下游打爆限流其他进程感知不到。

**顺带修复**：token 记账双边——流式在 `message_delta`/终帧同时读 usage，input 缺失时按请求侧估算（tiktoken 近似）回填并打 `estimated=true` 标记；去掉 `>0` 守卫改为显式记 0+原因（`llm_collector.go:146-150`）。

**thinking/effort**：网关统一注入 provider 方言（Kimi thinking 开关、effort 映射），下游只声明抽象档位；Kimi「thinking 计入 max_tokens」的坑（`client.go:1463-1466`）在网关内集中处理。

### 3.2 L2 · Agent 编排层（控制面）　　**[🟠 队列/注册表/派发 ✅ · 单副本假设仍在]**

> **细化（2026-07-25）**：原标注只写"部分"，太笼统。已落地：任务队列 + worker 注册表 + 租约续期 + 能力标签路由（`pkg/cluster` + `pkg/worker/broker.go`）· 三种 `AgentRuntime` 的放置求解（硬约束过滤 + 软偏好打分 + 团队亲和，同分按名字升序保证确定性）· 动作队列真被消费。
>
> **仍在的单副本假设**：cron 单副本（worker 进程显式 `Stop()` 掉它以免重复触发同一 job）· 会话无一致性哈希 · 控制面 `taskState` 每 300ms 轮询 `Queue.Get`（file 后端下是整桶读，T2 规模够用、大规模需队列侧 watch）。

> **实测（2026-07-25，三句已过时）**：Journal + Replay 真实，journal 仍是 `os.OpenFile` 本地文件、**不经 StateStore**（现 `pkg/graph/journal.go:194`，仍真）。~~`NodeTask` 类型不存在~~（现有 `RuntimeNodeTask`，`pkg/agent/runtime.go:93`）；~~**编排器从不入队**~~（`pkg/worker/broker.go:523` `remoteRuntime.Execute` 入队，经 `--dispatch-mode queue` 的 `WrapAgentFactory` 接进编排执行工厂）。per-run 单主租约 `orchestrator-lease/<runID>` 零命中（仍真——多副本单主至今未做）；~~`Queue.Extend` 无执行侧调用方~~（worker keepalive 每 15s 续租：`pkg/worker/worker.go:395` → `POST /cluster/extend` → `pkg/cluster/http.go:118`）。

即 design/01 的 AgentGraph 引擎。分布式语义补充：

- **控制面无状态化**：GraphRun 的唯一真源是 Journal（StateStore 持久化）；编排器实例崩溃后任何实例可 `Replay` 接管。多副本经**每 run 单主**（lease：`orchestrator-lease/<runID>`）避免双跑。
- **节点执行下发**：`NodeTask` 写入任务队列（EventBus 的 task subject），L3 worker 按能力标签拉取；结果以 `node.completed` 事件回流。本地模式下队列=进程内 channel，行为与现状同步调用等价。
- **watchdog 分布式化**：节点级心跳变为 worker 定期上报 lease 续约；lease 过期=停滞，编排器按 RetryPolicy 重派（等价 `coordinator.go:264` 双层检测）。

### 3.3 L3 · Agent 运行时（Worker）　　**[✅ 执行体 · Placement · cwd 三档位 · swarm · k8s-job]**

> ✅ **执行体已真实（2026-07-25）**：新增 `pkg/worker`（协议/Broker/远程 runtime/worker 循环，写时 29 个测试、2026-07-27 复点已增至 74 个测试函数）+ 独立二进制 `cmd/claude-go-worker`。改造前**两头都断**：控制面侧 `pkg/agent` 零 `pkg/cluster` import、`RuntimeRegistry` 里只有 `localRuntime`——控制面**没有任何办法**把节点派出去；worker 侧 `cmd/claude-go/main.go:1702 executeWorkerTask` 把 payload 回显成 `{"echo":...}` 并自带注释「v1 简化实现」。
>
> **接在 `CreateAgentFunc` 而不是 `graph.NodeRunner`**（最关键的决策）：换 factory 让 journal 记账天然一致（事件是引擎在 `RunNode` **外面**记的，远程走同一调用点）、gate/map/reduce/loop 的 prompt 组装一行不用抄、且 **`pkg/graph` 零改动**。自写 NodeRunner 旁路 `stageNodeRunner` 就得复制它的 prompt 组装并自补 journal 语义，两份必然漂移。
>
> 几处必须这样定的语义：**终态只认队列，事件流只是观测流**（事件走 best-effort HTTP，拿它当终态时丢包＝静默成功；增量事件非阻塞发、终态阻塞发——否则缓冲满会把终态挤掉，`CollectRuntimeOutput` 读成「空产出＋nil 错误」）；`StageResult` **带协议标记**（旧桩的回显 JSON 能被任何宽松解码器"成功"解析成空产出）；**队列级 `MaxAttempts=1`**（§4.3 重试单层化是本仓成文教训，队列再补一层就是第三层；worker 崩溃靠图层 RetryPolicy 重跑，那时死 worker 已被租约剔除，`Pick` 自然换机器）；**长任务必须续租**（默认租约 5min 而编码阶段动辄 10min+，`Queue.Extend` 此前全仓无执行侧调用方）；**远程 worker 不得叫 `local-*`**（否则 `isLocalRuntimeName` 会让 `Prefer:"local"` 给它加分＝把跨机执行伪装成本机执行）。
>
> 顺带修 `pkg/cluster/http.go` 的一处 **fail-open**：`Client.post` 拿到状态码却只是返回它，而 `Heartbeat`/`Complete`/`Fail` 三个调用方全写成 `_, err :=`——401（没配 token）与 400（"任务不在你的租约内"）在 worker 侧**全是静默成功**；上报终态被拒却当成功，任务就永远停在 `leased` 直到租约过期。
>
> ⚠️ **仍缺**：**逐节点 Placement**（`graph.AgentSpec` 没有 `Placement` 字段，现只有进程级默认 + 团队亲和，"需要 browser 的那个节点去 browser 池"做不到）；✅ **cwd 三档位已实现且跨机产码闭环（2026-07-25，设计风险④ 已解）**：`pkg/worker/{workspace,gitws}.go`。**pvc 档不是"互相声明就信"**——加了双向握手（控制面派任务前在共享卷写 `.control`，worker 读不到就拒绝执行；worker 完成写 `.worker` 回执，控制面读不到就判失败），这是唯一能检出"同路径不同数据"的手段。**git 档的闭环在控制面这一侧**：worker 推完后 broker `fetch` + **ff-only** 合入 `team.Cwd` 并校验提交可达，做完这步 `<team.Cwd>/go.mod` 上的 `go build` 才真看得见远程产码（真机实测 PASS）。冲突走 rebase 不 merge（保持线性，控制面永远能用 ff-only）、绝不 force push、大文件超闸即失败并列路径（悄悄跳过等于下一阶段找不到它）。真机验证抓出 4 个实现 bug（执行体自己 commit / worker 垃圾进仓 / 第二个团队被 `origin/HEAD` 永不设置卡死 / 跨团队串味），全部修掉并做了反向确认。~~⚠️ **一处既有约束未修**：`team.Cwd` 是进程级的（`teams.go:609`）~~ ✅ **已可隔离（2026-07-26，默认关）**：`CreateTeam` 改为 `Cwd: teamWorkspaceDir(ptm.cwd, name)`（`teams.go:651`，实现 `pkg/agent/team_workspace.go`，开关 `CLAUDE_GO_TEAM_WORKSPACE`）；默认行为仍进程级共享（改产物落点是下游可感知的行为变更），并发产码部署应显式开启——详见 §1.2 新增块。✅ **swarm 路径已接（2026-07-25）**：`AgentPool.factory` 与 `ProductionTeamManager.factory` 是**两个独立字段**，上一轮只换了后者——症状（swarm 子任务在本机跑）与"远程 worker 没上线"一模一样，现场无法归因。现让 `WrapAgentFactory` **同时**接管池，装配处零改动（多一个接线点就多一个"某个部署忘了调"的静默降级面）。✅ **k8s-job runtime 已接**（见 design/01 §4.9）。旧 `claude-go worker` 子命令仍是桩（仓里暂有两个 worker 入口）。

> **实测**：worker 拉取 + 心跳循环真实（`cmd/claude-go/main.go:1548-1600`），但 **`executeWorkerTask` 只回显 payload**（`:1609-1621`，自带注释「v1 简化实现…R3 后续接完整引擎」）⇒ 整条 L3 远程执行为 0。「worker 不依赖本机状态目录」成立只因它什么都不做——没有引擎装配、没有 prompt/skills/tools。cwd 三档位（local/pvc/git）未实现。

**现状归属**：`pkg/engine`（QueryEngine loop + internal_hook）、`pkg/prompt`（系统提示词组装，`prompt.go:76,126`：身份→工具规则→环境段→hooks→MCP→CLAUDE.md 记忆→dream 长期记忆）、`pkg/skills`（Registry+SKILL.md）、`pkg/tool`+`builtin`（4 档 profile）、`pkg/toolskill`（本机 CLI 质量门）、`pkg/permissions`、`pkg/sandbox`（native/docker/process/k8s）、`pkg/mcp`+`dynmcp`、`pkg/compact`。

**Worker 模型**：

```go
// cmd/claude-go-worker（或 all-in-one 内嵌 goroutine 池）
// 启动：向 RuntimeRegistry 注册 RuntimeCaps{Tools, Sandboxes, Browser, GPU, MaxConcurrent, Labels}
// 主循环：Pull(taskQueue, caps) → 装配 QueryEngine（prompt/skills/tools/constraints 均来自 NodeTask 自包含描述）
//        → 执行 → 流式回传 NodeEvent → ack
```

关键设计：**NodeTask 自包含**——role 系统提示词、skill 正文（或内容寻址引用）、约束、模型策略全部随任务下发或经 StateStore 内容寻址获取，worker 本地不依赖 `~/.claude-go` 目录内容。这消解假设 #5/#6：技能与角色定义只有中心一份（SkillStore），版本随任务钉死。

**工作区（cwd）问题**：产码类节点需要共享文件系统。三个档位：
- `local`：单机 all-in-one，团队目录同现状；
- `pvc`：K8s worker 挂共享 PVC（复用 `k8s_runner.go:35-36` 已有约定）；
- `git`：worker 各自 clone/worktree + 结果以 commit/patch 回传（远程分布式首选，天然并发隔离）。
节点 `Placement.Affinity: team` 保证需要共享 cwd 的节点串到同一 worker（design/01 §4.9）。

**MCP/stdio 工具**：stdio MCP 子进程属于 worker 本地资源，在 RuntimeCaps 里声明为能力标签（如 `mcp:playwright`）；需要该工具的节点被路由到具备标签的 worker——不再假设"所有工具处处可用"。

### 3.4 L4 · 辅助系统　　**[🟠 StateStore · TaskService ✅ / EventBus 通了 1 条链路]**

> ⚠️ **一处核实结论：仓里有两条总线**。`pkg/eventbus`（本节说的那个）确实零生产调用方；但 `pkg/observability` 里**另有一条一直在跑的总线**（生产 producer 6 处、consumer 在 `bot.go:354`）。所以"事件总线没通电"这句要拆开说——是本节这个没通电，不是仓里没有总线。两条并存需要一个"谁收编谁"的决定：`observability.Bus.Emit` 是**同步**交付，直接换成 `ChanBus` 会把 jsonl 落盘变成异步可丢。
>
> ✅ **已通 1 条链路（2026-07-25）**：LLM 运行事件 → 飞书播报。选它是因为它同时满足产生方真热、消费方真在，且改造能**修掉一个静默缺陷**——生产里有 **7 处 `api.NewClient`**，而播报挂在 `Client.OnLLMEvent` 这个**普通字段**上、只有飞书那个被赋了值，于是 advisor / `/model` 切换 / dashboard 三个客户端的 429 重试、熔断开合、"N 次全部失败"**从来没有任何人收到过，连日志都没有**（少赋一个字段不会编译报错）。顺带一处：`fireEvent` 在重试循环**内部**同步调，旧回调在里面逐个团队发飞书 HTTP——观测把背压传染给了执行。
>
> 产生方**不在 `pkg/api` 里直接 import eventbus**（L1 依赖 L4 违反 §3 的依赖方向），改成全局 sink、接线放装配层。播报口径逐字不变：只放行 Tag 以 `feishu` 开头的客户端，advisor/dashboard 事件只进总线与日志、**不进飞书**——否则 `:18080` 的消息量凭空变多，那是下游能感知的默认行为变化。
>
> ⚠️ **仍未迁**：黑板 `Watch`、dashboard SSE、轨迹采集三个订阅者；`task.dispatch.*`/`heartbeat.*`/`cron.fire` 未接。**不接的理由是"接了会降级"而不是"没来得及"**：`ChanBus` 满即丢，而团队进度、cron 触发、worker 事件确认都要求无损（`Publish` 没法回答"有没有人在听"，而 worker 的 `ack.Unknown` 正需要这个答案）。

> **实测**：StateStore 三后端齐备，但 **sqlite 零生产调用**（唯一调用方是 regression 测试）、无 `state:` 配置项、FileStore **无跨进程锁**（自己声明不保证）⇒ R2 验收「双进程读写一致」未达成。**`pkg/eventbus` 零生产 import** ⇒ §3.4.2 承诺的全部订阅者一个都没迁。✅ **`TaskService` 已实现**（`pkg/agent/taskservice.go`，含 :7777 动作队列的消费方——此前那个队列只写不读）。cron 选主 ✅（opt-in）。⚠️ §3.4.1 桶映射表大半未落地，且 `bb/<t>`、`journal/<runID>` 这类**层级桶名结构性不可表达**（`validateBucket` 白名单排除 `/`）；~~生产实际只有 4 个桶~~（已过时：现有 `dist-tasks`/`dist-workers`/`tasks`+`tasks-idem`+`tasks-active`+`tasks-journal`/`mailbox-*`/transcript 快照桶/trace 桶+Blob 等两位数个，层级名经 `flattenBucket`（taskservice.go:306）展平绕过 `/` 限制）。

#### 3.4.1 状态存储 StateStore（一切外置的地基）

```go
type StateStore interface { // bucket 化 KV + 追加日志 + 内容寻址 blob
    KV(bucket string) KVStore          // team 投影/cron jobs/registry/settings 快照
    Log(bucket string) AppendLog       // journal/llm.jsonl/trace/reward —— append-only
    Blob() BlobStore                   // REPORT.md/NOVEL.md/媒体产物/skill 正文（内容寻址）
}
```

后端：`file`（默认，目录布局=现 `basedir.Layout`，**磁盘格式不变**）→ `sqlite`（单机加强）→ `redis+pg / nats-jetstream`（分布式）。现有目录树逐桶映射：

| 现文件 | bucket | 备注 |
|---|---|---|
| `teams/<t>/team.json` | KV `teams` | 变 Journal 投影（design/01） |
| `teams/<t>/blackboard.json` | KV `bb/<t>` | Blackboard 接口后端 |
| `teams/<t>/checkpoints.json`+`goals.json`+`tasks.json` | Log `journal/<runID>` | 四合一 |
| `projects/<h>/<sid>.jsonl` | Log `transcript/<sid>` | 飞书会话历史同样落此（修"重启即丢"） |
| `metrics/llm.jsonl` 等 | Log `metrics/*` | 中心化聚合自然获得 |
| `cron/`、`skills/`、`workflows/*.json` | KV/Blob | 版本化 |
| `memory/`、`evolution/` | design/03 专属 bucket | |

#### 3.4.2 通信系统 EventBus

```go
type EventBus interface {
    Publish(subject string, ev Event) error
    Subscribe(subject string, group string) (<-chan Event, error) // group=队列组语义（任务分发）
}
// subjects: task.dispatch.<caps-hash> / run.events.<runID> / user.msg.<chatID>
//           / heartbeat.<kind> / cron.fire / notify.feishu / evolution.trace
```

后端：进程内 channel（默认）→ NATS JetStream（分布式首选：subject 通配+队列组+持久化齐备）。黑板 `Watch`、dashboard SSE、飞书进度播报、design/03 轨迹采集全部是 subject 订阅者。

#### 3.4.3 任务系统

design/01 TaskService 落在此层：Submit 幂等键（收编 `teams.go:328` 并发去重）、file-queue 兼容 :7777 队列目录、分布式后端=EventBus 队列组。

#### 3.4.4 定时任务系统

现 `CronScheduler`（`cron.go:66`）保留 job 模型（workflow/query/command/wiki/sync）与磁盘格式，执行变更：tick 时**先抢租约** `cron-lease/<jobID>`（StateStore 原子 CAS），抢到才发 `cron.fire` 事件——多副本不重复触发（修假设 #7）；执行体统一为"向 TaskService 提交"，cron 自身不再直接跑工作流。

#### 3.4.5 心跳系统

```go
// 三级心跳，统一 lease 模型（K8s 风格）
// ① runtime 级：worker 注册后周期续约 → RuntimeRegistry 摘除过期节点
// ② run/node 级：执行中节点续约 → 编排层 watchdog 判停滞（取代 coordinator.go:264 进程内检测）
// ③ 服务级：feishu-adapter/llm-gateway/evolution 各服务 liveness → dashboard 总览与告警
```

#### 3.4.6 观测

- trace-id（RunID/NodeID/TurnID/CallID）贯穿全部日志与 LLMCallRecord（design/03 依赖）。
- 指标经 EventBus 汇聚到中心 Log bucket；Prometheus 抓取端点由"本地 JSONL 扫描"（`jsonl_scraper.go`）改为中心聚合导出，单机模式行为不变。
- 备份（`pkg/backup`）改为对 StateStore 逐桶导出，file 后端下与现 tar.gz 等价。

#### 3.4.7 记忆·进化服务

design/03 的 Evolution Service 整体作为 L4 组件：单机=进程内模块，分布式=独立服务（中心化经验/记忆库，解假设 #17 学习孤岛）。

### 3.5 L5 · 用户层　　**[🟠 契约测试 · platform-mcp · 只读面板经控制面 · sync→TaskService ✅ / CLI --server、adapter 拆分 ❌]**

> ✅ **只读 dashboard 独立部署并真经控制面（2026-07-25）**：`deploy/k8s/distributed.yaml` 新增 `claude-go-dashboard` Deployment+Service（`CLAUDE_GO_BOT_API_URL` 指向 `claude-go-control:18080`）。
>
> ⚠️ **加这个 pod 时验证救了一次**：先只加了清单，真集群一测发现它**是装饰品**——回包说「动作已落盘，但本进程未注册消费方」。查下去发现转发**只对 `team.stop/restart/delete/resume/refine/fork` 存在**，`team.create`/`team.run` 根本不转发，只落盘无人消费，回包甚至提示用户「可手动运行 `claude-go team create ...`」——一半动作能生效一半不能，且不能的那一半连提示都在教用户绕过产品。
>
> 顺带修掉一个**潜在 bug**：原转发代码把 `r.Body` 直接交给转发请求，但 `handleAction` 更早处已 `io.ReadAll` 把它读空了——**转发出去的请求体是空的**。`team.stop` 那组载荷通常为空所以从未暴露；换成 `team.create` 就是"工作流和目标全丢"。改为把已解析的 payload 重新编码。
>
> 真集群端到端证据：只读面板 POST `team/create/fwd-ok` → 控制面上团队真存在且 `workflow: research`、objective 完整（沿用旧 `r.Body` 时这两项会是空）。本进程自己有执行能力时**不转发**（那是回环且会双跑），控制面拒绝时**不谎报成功**。
>
> 只读面板刻意**不给 LLM 凭据**、**不与控制面共享状态卷**——它读的是经 HTTP 拿到的控制面数据，挂同一个卷会让人误以为它能直接改控制面状态。

> **实测（2026-07-25 快照，多句已过时）**：飞书 Bot 上帝对象原样（仍真）；CLI `--server` 零命中（仍真）；~~`platform-mcp-server` 零命中~~（已落地 `pkg/platformmcp`，见上方 ✅）；~~`BotAPIURL` **硬编码回环**~~（默认仍回环，但已可经 `CLAUDE_GO_BOT_API_URL` 覆盖——`pkg/dashboard/l5_control_plane.go:26`，`distributed.yaml:162` 已使用）。:18080 端点契约未改 ✅；~~**表驱动契约测试不存在**~~（已存在 68 条：`pkg/wiki/contract_18080_test.go` 9 条 + `pkg/dashboard/contract_18080_test.go` 59 条，断言方式见 §六）。

| 通道 | 现状 | 目标 |
|---|---|---|
| 飞书 | Bot 上帝对象 + WS 长连接（`bot.go:674`） | **薄适配器** feishu-adapter：WS 收消息→发 `user.msg.<chatID>` 事件→订阅回复事件推送。多副本经"chatID 队列组+单消费者"防重复消费（修假设 #19）。会话 actor 归属 L2/L3（按 chatID 一致性哈希路由到 owner worker，历史落 transcript bucket 修"重启即丢"） |
| CLI/REPL | `main.go` 直接装配引擎 | 保留本地直连（all-in-one 库调用）；新增 `--server` 模式走 :18080 OpenAPI |
| dashboard | :7777 只读+文件队列 / :18080 直连回调 | 全部改走控制面 OpenAPI；伪 RPC 惰性回调（`main.go:1091-1127`）升级为真 HTTP 客户端，接口签名不变 |
| HTTP :18080 | wiki+dashboard 混挂 | **兼容层**：现有全部端点（`/wiki/*`、`/sync/*`、`/api/*` 全清单）路径与语义冻结，内部转发到分层服务——下游 8+ 平台零改动 |
| MCP | client + codeintel server | 新增 **platform-mcp-server**：把 TaskService/workflows/skills/teams 以 MCP 工具对外（远程 agent 管理的标准入口之一） |
| wechat | 出站发布 CLI（`wechat_cmd.go`） | 归为 Notifier 的一个 sink，不动 |
| sync（IMA/WeRead） | `pkg/sync` 进程内调度 | 触发端点不变，执行体改提交 TaskService |

**安全**（分布式后必须补）：:18080 兼容层统一 Bearer（`/api/*` 纳入鉴权，修 §1.4）；服务间 mTLS 或共享 token；worker 最小权限（仅队列拉取+StateStore 指定桶）。

---

## 四、部署形态　　**[🟠 T0 ✅ / T1 🟡 断链 / T2 ✅（worker + 共享工作区） / T3 🟠 声明级]**

> **一处标注更正（2026-07-25）**：此前把「worker 分离」错记在 T1 名下。**T1 是网关分离**（`+claude-go llm-gateway`），它的断链是 remote LLM 实现未落地（属 `pkg/llmgw`）；**worker 分离是 T2**。2026-07-25 接通的是 §3.3 执行体 + T2 的 worker 腿 + R3（worker 拉取 / RuntimeCaps 标签路由 / 心跳续租），**不是 T1**。

> **实测**：⚠️ ~~**「实测跑通」的证据强度需下调**：仓内零 shell 脚本、零 Makefile、零 CI、零 kubectl 输出，V2/V3 结论只存在于 `PROGRESS.md` 文字里~~（已过时：`deploy/k8s-e2e.sh` 已入库——13 步 / 30+ 断言，含构建新鲜度硬断言与跨轮 env 泄漏自检；Makefile 与 CI 确实仍无）。~~且 monolith 与 control 都没传 `--config`，而配置自动发现路径不含 `/etc/claude-go/` ⇒ 挂载的 ConfigMap 从未被读取~~ ✅ **已修（2026-07-25）**：`ResolveJSONConfigPath` 的发现列表补上容器约定路径 `/etc/claude-go/config.json`（放最后，本机开发时家目录配置优先），挂载的 ConfigMap 现在会被读取。⚠️ 但此前记录的 V2/V3"双模式实测全通"仍应按**存活冒烟**理解——那次跑的是修复前的二进制，Pod 落在占位 provider 分支、不接触 LLM。T3 的 control state 是 emptyDir 且 replicas>1 会立即分裂。`layers: {llm,bus,state}` 配置项不存在；「进程间嵌入式 NATS」无依赖。

| 形态 | 进程 | 适用 | 说明 |
|---|---|---|---|
| T0 all-in-one（默认） | 1：`claude-go feishu` | 现状全部用户 | 全层进程内装配，接口本地实现，行为/文件格式/端口完全兼容 |
| T1 网关分离 | 2：+`claude-go llm-gateway` | 多平台共享配额 | 本机所有 claude-go 系进程与下游平台共用一个网关（熔断/配额/记账集中） |
| T2 单机多进程 | 4：adapter / control-plane / worker×N / gateway | 隔离与滚动升级 | StateStore=sqlite，EventBus=进程间 NATS（嵌入式） |
| T3 多机/K8s | 各层多副本 | 规模化 | StateStore=redis+pg，EventBus=NATS 集群，worker 按能力标签分池（browser 池/k8s-sandbox 池/GPU 池），cron 选主，会话一致性哈希 |

同一二进制多入口：`claude-go feishu|serve|worker|llm-gateway|cron`，由配置决定内嵌还是远程连接（`layers: {llm: local|url, bus: chan|nats://, state: file|sqlite|redis://}`）。

---

## 五、19 条单机假设 → 整改映射

| # | 整改 | 所在层/阶段 |
|---|---|---|
| 1/2/3 | 会话 actor 化 + transcript 入 StateStore Log | L3/L4 · R2 |
| 4 | 配置=中心 KV 快照 + 本地文件回退；热更走 EventBus 广播（取代轮询 #10） | L4 · R2 |
| 5 | basedir.Layout 变 file 后端的实现细节，上层只见 StateStore | L4 · R0 |
| 6 | SkillStore 中心化 + 内容寻址下发 | L3/L4 · R2 |
| 7 | cron 租约选主 | L4 · R3 |
| 8 | 三级 lease 心跳 | L4 · R3 |
| 9 | RuntimeCaps 能力标签路由 | L3 · R3 |
| 10 | EventBus 配置广播 | L4 · R2 |
| 11/12 | :18080 兼容层 + 服务间真 HTTP | L5 · R1 |
| 13 | 指标中心聚合 | L4 · R2 |
| 14 | sandbox 属 worker 本地能力，标签化 | L3 · R3 |
| 15 | ollama 等本地端点=网关的一个 provider，带节点标签 | L1 · R1 |
| 16 | IndexBaseDir 已支持共享盘，纳入 Blob 桶 | L4 · R2 |
| 17 | Evolution Service 中心化（design/03） | L4 · R2/E2 |
| 18 | 团队数据=Journal+Blob 桶；cwd 三档位（local/pvc/git） | L2/L3 · R2/R3 |
| 19 | adapter 队列组单消费 | L5 · R3 |

---

## 六、现有功能覆盖矩阵（接入与服务域）　　**[🟠 ≈75%：契约已冻结并有测试守护 / 三项拆分未做]**

> **重算（2026-07-25，逐行核实）**：≈47% → ≈75%。变化主要来自三件事：
>
> ① **契约从"靠人记得"变成"测试守着"**：`:18080` 与 wiki 侧共 60+ 条表驱动契约测试。四维断言里最要紧的一条是**比 `mux.Handler(req)` 返回的 pattern 而不是只看状态码**——dashboard 是 SPA 用 `/` 兜底路由，只看状态码的测试会因为拿到 `200 + index.html` 而全绿；另一条是**表与源码双向一致**（正则扫源文件枚举注册点），它防的正是"悄悄加端点没登记"，`/cluster/*` 漏鉴权就是这个错。
>
> ② **新增接入面**：`platform-mcp-server` 8 个工具（HTTP + stdio 两传输**共用同一 Backend**，不会出现"HTTP 有这工具 stdio 没有"）· 只读 dashboard 独立部署且动作**真转发**控制面（5 处 `forwardActionToControl`，含修掉"转发请求体是空的"那个潜在 bug）。
>
> ③ **出口收敛**：飞书/dashboard 侧 9 处 LLM 调用改经网关；LLM 事件从"只有 1 个客户端被赋值"变成全局 sink。
>
> **仍未做的两项拆分**（§3.5 逐条记账）：CLI `--server`（前置条件是 `:18080` 上没有"执行一次 prompt"的端点，那牵涉会话 owner 路由）· feishu-adapter 拆分（上帝对象原样）。~~sync→TaskService（`TaskSpec` 是团队形状，硬塞得先加通用任务类型）~~ ✅ **已做（2026-07-26 工作区）**：当时的顾虑正是用 `TaskSpec.Kind` + `KindRunner`（`pkg/agent/task_kind_runner.go`）解决——`pkg/synctask` Bridge 承担提交侧+执行侧、`wiki.SetSyncSubmitter` 让 /sync/* 优先走 TaskService、端点语义不变（202 `{jobId}`，双查新旧台账防 404）。

> **实测**：⚠️ 本矩阵是计划表不是状态表。其中「:7777 动作队列 → TaskService file-queue · 等价」**不成立**：无 TaskService，且该队列全仓无消费方。

| 现有功能 | 位置 | 新归属 | 状态 |
|---|---|---|---|
| 飞书 WS 全部消息/命令/steer | `bot.go`、`session.go:698` | feishu-adapter + 会话 actor | 等价 |
| :18080 wiki 9 端点（status/ingest/query/lint/organize/health-check/sync-ima/sync-weread/sync-status） | `wiki/api.go:33-42` | L5 兼容层→wiki 服务 | 等价 |
| :18080 dashboard ~50 端点（teams/cron/dreaming/evolution/workflows/skills/tools/mcp/logs/backups/llm/prom/diag/actions/SSE…） | `dashboard/server.go:220-290` | L5 兼容层→控制面 API | 等价（补鉴权） |
| :7777 只读 dashboard + 动作队列 | `extra_handlers.go:1089-1094` | TaskService file-queue | 等价 |
| CLI 全部子命令（run/dashboard/doctor/tools/wechat…） | `main.go` | 本地直连 + `--server` | 等价+新增 |
| REPL 斜杠命令与 slash 直通防护 | `main.go:700-703` | 不变（约束单调性，design/01） | 等价 |
| MCP client（stdio/http/SSE）+ dynmcp 动态增删 | `mcp/client.go`、`dynmcp/manager.go` | worker 本地能力 + 标签 | 等价 |
| codeintel MCP server（:9234） | `codeintel/mcp_transport.go` | 独立可部署服务 | 等价 |
| MCPToolSearch/Invoke 代理 | `feishu/mcp_proxy_tools.go` | worker 内不变 | 等价 |
| 模型别名/role>plan>global 优先级/fallback/429 切换/熔断/prompt cache | `modelconfig`、`client.go:115-122,1421` | L1 网关 | 等价（集中化增强） |
| LLMCallRecord + llm.jsonl + Grafana 看板 | `client.go:57`、`llm_collector.go` | L1 产出→L4 聚合 | 增强（trace-id、双边 token） |
| cron 四类 job + 落盘 | `cron.go:66,477` | L4 cron（+选主） | 等价 |
| 心跳/watchdog/停滞恢复 | `coordinator.go:28-86` | 三级 lease | 等价 |
| sync IMA/WeRead + jobId 查询 | `pkg/sync` | 端点不变，执行走 TaskService | 等价 |
| 热加载（MCP 配置/技能重载） | `hotreload/watcher.go`、`bot.go:776-787` | file 后端保留轮询；分布式走广播 | 等价 |
| settings 四级配置 | `settings.go:165-171` | KV 快照+本地回退 | 等价 |
| 备份/恢复 | `pkg/backup` | 逐桶导出 | 等价 |
| sandbox native/docker/process/k8s | `sandbox/manager.go:26-35` | worker 能力 | 等价 |
| toolskill 质量门（本机 CLI） | `pkg/toolskill` | worker 能力标签（如 `cli:golangci-lint`） | 等价 |
| prompt 组装全要素（身份/环境/CLAUDE.md/dream 记忆/skill 清单） | `prompt.go:76-228,369` | L3 worker 内不变；记忆来源改 design/03 服务 | 等价 |
| vision/intent/advisor/compact 等 SimpleComplete 消费方 | `bot.go:535-542` 等 | 全部改经 LLMGateway 接口 | 等价 |
| wechat 排版发布 | `wechat_cmd.go` | Notifier sink | 等价 |
| 会话超时清理/MaxSessions 淘汰/待处理队列 | `session.go:44,617,644` | 会话 actor 内策略不变 | 等价 |

---

## 七、迁移路线　　**[🟠 R0 ✅ · R1 70% · R2 70% · R3 ✅ · R4 60%]**

> **重算（2026-07-25，逐项核实源码）**：
>
> | 里程碑 | 旧 | 新 | 依据 / 缺什么 |
> |---|---|---|---|
> | R0 接口抽取 | 50% | ✅ | 四接口齐：`LLMGateway`（9 处生产依赖）/ `StateStore` / `EventBus`（1 条链路通电）/ `AgentRuntime`（三实现） |
> | R1 网关独立 | 60% | **70%+** | `llm-gateway` 子命令有；~~**`remote` 实现缺**~~ ✅ remote 档已实现（`pkg/llmgw/remote.go`：`NewRemote`/`NewFromEnv` 按 `CLAUDE_GO_LLM_GATEWAY_MODE` 选档、remote 无地址 fail-closed；默认 local 是刻意决定，T1「断链」已通，集中化收益部分兑现——经网关记账/配额观测有了，熔断/fallback 集中未开）。token 双边记账已有 `api.DefaultTokenLedger` + trace 四元组 |
> | R2 状态外置 | 30% | **70%** | sqlite 后端有（`pkg/statestore/sqlitestore.go`）；会话历史快照有；**SkillStore/配置入库未做**（技能仍读磁盘目录） |
> | R3 worker 拉取 | 35% | ✅ | 队列 + 独立 worker 二进制 + `RuntimeCaps` 标签路由 + 三级心跳 + 租约续期；cwd 三档位使**跨机产码闭环**（真机 `go build` PASS） |
> | R4 多机 | 20% | **60%** | K8s 清单 5 份（monolith/distributed/k8s-job/PVC×2）+ k8s-job runtime 真机含故障注入验证；**NATS/redis+pg 后端与会话一致性哈希未做**（`ChanBus` 是进程内、`FileStore` 锁 per-instance） |
>
> 原文那句诊断仍然值得留着——它描述的正是这份文档反复出现的失效模式：把「接口/包写完 + 单测绿」记为 ✅，而验收标准写的是形态级/链路级。三处最该改标：~~R1 由 ✅ 降 🟨（remote 实现从未落地、集中化收益一项未得、K8s 里网关未被接入）~~（R1 这条已过时——remote 已落地且网关在 e2e 里真被经过，见上表）；R2 的 sqlite ✅ 降 🟡（2026-07-27 复核仍成立：零生产接线、无配置开关）；R4 的「双模式实测 ✅」降为「存活冒烟，功能未验」（其后 k8s-e2e.sh 已补足真实 LLM 验收）。

| 阶段 | 内容 | 验收 |
|---|---|---|
| R0 接口抽取（2 周） | LLMGateway/StateStore/EventBus/AgentRuntime 四接口 + 全部 local/file/chan 默认实现；调用点替换（Bot 装配改为注入接口）；trace-id 贯穿（与 design/03 E0 同步） | all-in-one 全量回归零回退；磁盘格式不变 |
| R1 网关独立（1 周） | llm-gateway 子命令 + remote 实现；token 双边记账修复；:18080 兼容层挂 Bearer | T1 形态陪跑；下游平台无感 |
| R2 状态外置（3 周） | sqlite 后端；transcript/会话历史入库；SkillStore/配置中心化+广播；指标聚合 | kill -9 后会话可续；双进程读写一致 |
| R3 worker 拉取（3 周） | task 队列组 + worker 子命令 + RuntimeCaps 标签路由 + 三级心跳 + cron 选主；cwd 三档位（git 模式打通） | T2 形态：控制面滚动重启任务不断；节点 worker 崩溃自动重派 |
| R4 多机（按需） | NATS/redis+pg 后端；K8s 部署清单；会话一致性哈希；worker 分池 | T3 形态：双机 e2e；单机故障 run 存活 |

**风险**：① 兼容层是生命线——:18080 端点契约先写成表驱动测试再动内部；② file→sqlite 数据迁移工具与回滚开关；③ NATS 引入的运维复杂度——T0/T1 永远不需要它，分布式是 opt-in；④ 分布式 cwd 的 git 档位对"团队内高频互写文件"的工作流（app/game composite）有语义差异，先限定 pvc/local。
