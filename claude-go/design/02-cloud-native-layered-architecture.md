# 02 · 云端分离分层架构（分布式部署能力）设计方案

> 状态：设计稿 v1（2026-07-23）· 基于 HEAD `6bd8da22f` 全量源码梳理
> 关联：[00-overview.md](00-overview.md) · [01 编排引擎](01-unified-agent-orchestration-engine.md)（L2 内核）· [03 RL 进化引擎](03-rl-evolution-engine.md)（L4 组件）

---

## 一、现状：单进程"上帝对象"架构

### 1.1 基本事实

- **整个系统是一个 Go 单进程**：飞书 `Bot`（`pkg/feishu/bot.go`）在 `NewBot` 里装配全部子系统；`SessionManager`（`session.go:106`）直接持有 apiClient / mcpMgr / skillReg / dreamer / memoryStore / taskStore / evolution / teamMgr 所有依赖指针。
- **LLM 全部走 HTTP**：唯一出口 `pkg/api.Client`（`client.go:598,983,1470`），Anthropic Messages 协议兼容 Kimi/DashScope 等网关；**不存在对外部 `claude`/`codex` CLI 的子进程调用**。
- **层间耦合四种形态**：直接函数调用（主）、进程级全局单例（`dynmcp.Manager`、`llmGlobal` 指标、observability bus）、文件系统约定（`~/.claude-go/` 目录树，`basedir.go:15`）、同进程惰性回调（dashboard→Bot 的 `TeamAction/LLMComplete/ReloadSkills/CronController`，`main.go:1091-1127`——唯一被显式设计成可分离的接缝）。
- **对外只有一个端口 :18080**：wiki API + dashboard 挂同一 mux（`wiki/api.go:66`、`dashboard/server.go:129`）；飞书经 WebSocket 长连接收消息（`bot.go:674,854`）。

### 1.2 单机假设清单（分布式障碍，19 条）

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

### 1.4 附带缺陷（分层时一并修）

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

### 3.1 L1 · LLM 引擎层（LLM Gateway）

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

### 3.2 L2 · Agent 编排层（控制面）

即 design/01 的 AgentGraph 引擎。分布式语义补充：

- **控制面无状态化**：GraphRun 的唯一真源是 Journal（StateStore 持久化）；编排器实例崩溃后任何实例可 `Replay` 接管。多副本经**每 run 单主**（lease：`orchestrator-lease/<runID>`）避免双跑。
- **节点执行下发**：`NodeTask` 写入任务队列（EventBus 的 task subject），L3 worker 按能力标签拉取；结果以 `node.completed` 事件回流。本地模式下队列=进程内 channel，行为与现状同步调用等价。
- **watchdog 分布式化**：节点级心跳变为 worker 定期上报 lease 续约；lease 过期=停滞，编排器按 RetryPolicy 重派（等价 `coordinator.go:264` 双层检测）。

### 3.3 L3 · Agent 运行时（Worker）

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

### 3.4 L4 · 辅助系统

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

### 3.5 L5 · 用户层

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

## 四、部署形态

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

## 六、现有功能覆盖矩阵（接入与服务域）

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

## 七、迁移路线

| 阶段 | 内容 | 验收 |
|---|---|---|
| R0 接口抽取（2 周） | LLMGateway/StateStore/EventBus/AgentRuntime 四接口 + 全部 local/file/chan 默认实现；调用点替换（Bot 装配改为注入接口）；trace-id 贯穿（与 design/03 E0 同步） | all-in-one 全量回归零回退；磁盘格式不变 |
| R1 网关独立（1 周） | llm-gateway 子命令 + remote 实现；token 双边记账修复；:18080 兼容层挂 Bearer | T1 形态陪跑；下游平台无感 |
| R2 状态外置（3 周） | sqlite 后端；transcript/会话历史入库；SkillStore/配置中心化+广播；指标聚合 | kill -9 后会话可续；双进程读写一致 |
| R3 worker 拉取（3 周） | task 队列组 + worker 子命令 + RuntimeCaps 标签路由 + 三级心跳 + cron 选主；cwd 三档位（git 模式打通） | T2 形态：控制面滚动重启任务不断；节点 worker 崩溃自动重派 |
| R4 多机（按需） | NATS/redis+pg 后端；K8s 部署清单；会话一致性哈希；worker 分池 | T3 形态：双机 e2e；单机故障 run 存活 |

**风险**：① 兼容层是生命线——:18080 端点契约先写成表驱动测试再动内部；② file→sqlite 数据迁移工具与回滚开关；③ NATS 引入的运维复杂度——T0/T1 永远不需要它，分布式是 opt-in；④ 分布式 cwd 的 git 档位对"团队内高频互写文件"的工作流（app/game composite）有语义差异，先限定 pvc/local。
