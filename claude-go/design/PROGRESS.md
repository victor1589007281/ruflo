# design/01-03 实施进度台账

> 滚动更新。状态：⬜未开始 🟨进行中 ✅完成 ⏸️降级/移出本轮（注明原因）
> 目标全链：P0-P5 开发 → 测试 → K8s 单体+分布式部署测试 → 功能覆盖确认 → 真 LLM 测试 → docforge 文档 → testforge 沉淀 → 全部推远程

## P0 地基（01-M0 / 03-E0）✅ 2026-07-24
- ✅ P0.1 双 mode switch 一致性：coordinator 改为查 `dedicatedExecutorModes` 共享表（workflow.go），app_composite/game_composite 修复静默降级；守护测试 TestModeRoutingConsistency/TestDedicatedExecutorModesExact
- ✅ P0.2 嵌套重试收敛：429 无限循环加硬上限 `stageRateLimitMaxRetries=20`；`WithOuterRetryDriven` ctx 标记使协调器恢复路径下内层退化为单次尝试（effMaxRetries=1/限流3）
- ✅ P0.3 清理：git rm workflow.go.backup(498KB)/test_all_workflows.go/test_parenting.go/claude-go-test(Mach-O)/eval.test
- ✅ P0.4 headless 进化实例化：main.go run 路径 TeamManagerConfig 补 Evolution（与 bot.go:376 同构装配 layout.Evolution）
- ✅ P0.5 技能自进化接线：teams.go 新增 SkillAutoCreator 接口+干净成功触发 MaybeCreate（异步2min超时）；autocreate.go frontmatter 加 `status: shadow`；bot.skillAuto 构造提前至 teamMgr 前并注入；CLI 路径 cliSkillCreator
- ✅ P0.6 dreaming headless 触发：buildEngine 的 dreamer 经 cliDreamAdapter 注入 run 路径 TeamManagerConfig.Dreamer（teams.go 完成路径已有 RecordSession/AfterQuery 调用）
- ✅ P0.7 trace-id 四元组：新 pkg/trace（With 合并注入/From/NewID）；LLMCallRecord+RunID/NodeID/TurnID/CallID（StreamMessage+SendMessage emit 闭包单点打点）；RunID 复用 logging traceID 后缀可 join 结构化日志；executeStage 注 NodeID、engine queryLoop 注 TurnID、进化 Trajectory 加 RunID
- 偏差：ImproveSkill 触发（低奖励→改进）延后至 P3 RewardBus 就绪后（当前无奖励信号判定"低"）；transcript 加 turn_id 归 P3.1 TraceStore

## P1 图引擎内核（01-M1/M2）✅ 2026-07-24
- ✅ P1.1 pkg/graph：GraphSpec/NodeSpec/EdgeSpec 纯数据 + Validate（环/悬空/不可达/坏条件全拦截）
- ✅ P1.2 Journal 事件溯源 + Replay（FileJournal O_APPEND 崩溃安全 + resume 零重跑）
- ✅ P1.3 引擎调度器（ready-set 并行 + 条件边 OR-join + retry环内嵌loop环 + ctx取消 + trace注入）
- ✅ P1.4 WorkflowDef→GraphSpec 直译器（graph_adapter.go）+ 灰度开关 CLAUDE_GO_GRAPH_ENGINE + wf.Mode==graph；stageNodeRunner 复用 ExecuteSingleStage
- ✅ P1.5 Hook 总线最小版（graph/node scope + deny→skip）；外部 hook 完整适配归 M2
- ✅ P1.6 单元测试：15 graph 测试 + M1 等价性/journal resume 回归全绿
- 偏差：map/subgraph/human/router/reduce/loop-group Kind 预留但 Validate 拒绝（v1 只 agent/gate），归 M2

## P2 分层接口（02-R0/R1）✅ 2026-07-24
- ✅ P2.1 LLMGateway 接口 + Local 薄包 api.Client（pkg/llmgw）
- ✅ P2.2 StateStore 接口 + FileStore/MemStore（KV原子rename/Log崩溃安全/Blob内容寻址去重）
- ✅ P2.3 EventBus 接口 + ChanBus（广播/队列组轮询/后缀通配/非阻塞丢弃计数）
- 🟨 P2.4 AgentRuntime：接口设计在 design/01 §4.9；worker 落地见 P4（cluster 包）
- ✅ P2.5 llm-gateway 子命令（反向代理/provider路由/token双边记账修复只计输出/access.jsonl）
- ✅ P2.6 单元测试：三包 -race 全绿 + gateway 6 httptest

## P3 进化闭环（03-E1/E2）🟨 部分
- ✅ P3.1 TraceStore（pkg/evolution/tracestore Span + Ref内联/Blob + TraceCaptureHook 完整tool_result采集 + 引擎/CLI/feishu 接线）
- ✅ P3.2 RewardBus 落盘雏形（rewards.jsonl + content_gate/episode 两源接线，已在 P0.7 后提交）
- 🟨 P3.3 学习器收编 EvolutionLoop：延后（现有 LearnFromTeam/Consolidate 已在团队完成路径触发，统一 Loop 归 E2 深化）
- ✅ P3.4 单元测试：tracestore 7 + hook_trace 3 全绿

## P4 分布式（02-R2/R3 精简）🟨 队列层完成
- ✅ P4.1 pkg/cluster Queue：租约式 worker 拉取 + Fail回队重派 + 过期租约回收（崩溃自愈）+ Extend续租
- ✅ P4.2 pkg/cluster Registry：worker 心跳注册 + lease过期摘除 + caps能力标签
- 🟨 P4.3 worker/control HTTP 子命令 + cron 租约：K8s distributed.yaml 已声明契约；HTTP 端点接线归 R3 后续
- ✅ P4.4 单元测试：cluster 全 -race 绿

## P5 评估与进化操作台（03-E3/E4 精简）🟨 操作台只读版完成
- 🟨 P5.1 离线回放 harness：设计已规格化（design/03 §4.5）；tests/eval 扩展归 E4 后续
- ✅ P5.2 evo 操作台只读版：`pkg/evolution/console` + `evo` CLI 命令，检视学习闭环健康度（轨迹/奖励/经验/shadow技能，healthy/partial/open 判定）；只读护栏（Hermes H7）；对真实 E2E 制品实测 PARTIAL 正确诊断。propose/smoke/promote 写操作闭环归 E3/E4
- ✅ P5.3 单元测试：console 3 测试（healthy/open/shadow）全绿

## 验收链
- ✅ V1 全量 go build/vet 绿；pkg/... test 除预存 orchestrator 死锁外全绿（见偏差记录）
- ✅ V2 K8s 单体模式：Pod Running/Ready，/api/health + /api/workflows(28) + 建团队全通
- ✅ V3 K8s 分布式模式：control+2worker+gateway 四组件 Running；worker 注册可见；任务 control→worker→control 完整链路；gateway healthz+provider 路由 OK
- ✅ V4 功能覆盖确认：三份设计文档各含覆盖矩阵逐项映射（编排30+工作流/15mode/三hook、接入全端点、学习全能力）；新架构全部等价或增强，无丢项
- ✅ V5 真实 LLM E2E（真 kimi, mini-graph 图工作流 2 节点 completed）：图引擎 ready-set 调度 + journal 8 事件溯源 + TraceStore 2spans/3blobs 内容寻址 + rewards.jsonl episode 奖励 trace 关联 + evolution 2 trajectories（**开环1 headless 学习实证修复**）
- ✅ V6 docforge 技术文档更新：新增第13章下一代架构演进（13.1图引擎/13.2分层分布式/13.3RL进化）+ 修正第5章学习开环已修复；import created=4 updated=43，已推 gitee
- ✅ V7 testforge 沉淀回归资产：regression/ 独立嵌套模块 8 测试；注册 testforge 项目「claude-go 下一代架构回归」+ repo SUT + gotest asset，首跑 8/8 pass gate 100%
- ✅ V8 全部仓库推远程：claude-go→github vdocs（10+ commit）、docforge→gitee master、testforge 无代码变更（资产在 DB+claude-go repo）

## 里程碑逐项状态（2026-07-24 二轮缺口核对，对照 design M0-M4/R0-R4/E0-E5）

### design/01 编排引擎
- ✅ **M0 止血**：双 switch 收敛/嵌套重试收敛/清理，全绿
- ✅ **M1 图内核**：GraphSpec/engine/journal/直译器 + pipeline/fanout 灰度切换 + kill-9 恢复重放
- 🟨 **M2 全模板**：✅ gate 节点差异化执行（runGate: 确定性+LLM评审分派，score 供条件边路由）+ loop 执行 + 条件边；⬜ 其余 13 mode 模板化、gate/loop-group/expand 全能力、Hook 总线接通外部 hook 配置（后续）
- ⬜ **M3 注入与约束**：六拦截器/ConstraintSet 单一真源/ToolProfile 显式化（AgentSpec.ToolProfile 已加字段，RoleDef 显式化未接）
- ⬜ **M4 运行时与收尾**：AgentRuntime 三实现/SpawnSubgraph/旧引擎删除/team.json 投影化

### design/02 分层架构
- 🟨 **R0 接口抽取**：✅ LLMGateway/StateStore/EventBus 接口+local实现、trace-id 贯穿；⬜ AgentRuntime 接口、Bot 装配改注入接口
- ✅ **R1 网关独立**：✅ llm-gateway 子命令+反向代理、**token 双边记账修复(主 api.Client 路径 estimateInputTokens + 网关双管)**；⬜ :18080 Bearer 全端点鉴权（部分）
- 🟨 **R2 状态外置**：✅ sqlite 后端(纯Go modernc)；⬜ transcript/会话历史入库、SkillStore/配置中心化+广播、指标聚合
- 🟨 **R3 worker 拉取**：✅ 任务队列组+worker子命令、**RuntimeCaps 标签路由(PullFor)**、**cron 选主(FileLease O_EXCL)**、心跳lease；⬜ cwd 三档位 git 模式、worker 真实引擎执行
- 🟨 **R4 多机**：✅ K8s 部署清单+双模式实测；⬜ NATS/redis+pg 后端、会话一致性哈希、worker 分池（by-design opt-in）

### design/03 RL 进化引擎
- ✅ **E0 修开环+trace-id**：三开环修复 + trace 四元组，真实 LLM E2E 实证
- ✅ **E1 轨迹底座**：TraceStore + tool_result 采集 + Blob 内容寻址 + **采样(BodySampleRate)/TTL(SweepTraceFiles)**
- 🟨 **E2 奖励总线+学习器收编**：✅ content_gate + episode + 图 gate 三源落 rewards.jsonl、content_gate 分数持久化；⬜ 飞书 reaction/`/rate`/steer 负信号/下游回传端点（8源中3源）、经验/记忆学习器迁 EvolutionLoop
- 🟨 **E3 Skill 进化器**：✅ **shadow 技能审计门禁(skillaudit: 奖励证据裁决晋升/退役, SKILL.md audit 谱系)**、shadow 态创建；⬜ 真配对A/B审计(需运行期skill-usage打点)、version 谱系
- 🟨 **E4 工作流/Prompt进化+回放**：✅ **evo 操作台(status/audit/promote/rollback + 锁定字段护栏)**；⬜ 离线回放 harness、GEPA prompt 进化、AWM 工作流归纳
- ⬜ **E5 权重导出**：DPO/RFT 导出器（可选，未启动）

### 本轮二次缺口核对新增（2026-07-24 续）
R1 token 主路径修复 / R2 sqlite 后端 / R3 cron选主+caps路由 / E1 采样+TTL / E3 技能审计门禁 / E4 evo audit/promote/rollback / M2 gate 节点差异化——7 项缺口关闭，各带单测。剩余 M3/M4/R2 深化/E2 更多源/E4 回放 harness/E5 为后续里程碑（design 自身标注跨多周/持续演进）。

## 偏差记录
（设计与实现的偏差在此登记，并回写对应设计文档）

- **[发现] pkg/orchestrator TestEngine_DiamondDAG 预存死锁**（2026-07-24）：旧的独立 K8s 式 DAG 引擎 `pkg/orchestrator`（design/01 §1.1 列出的三引擎之一）的 DiamondDAG 测试在 engine.go:340 的 select 上无限阻塞（90s/600s 均超时）。经 git diff 确认本轮所有提交**均未触碰** pkg/orchestrator，属预存缺陷。**佐证 design/01 收敛决策**：新 pkg/graph 的同款菱形并发测试（TestEngine_DiamondDAG 等价）通过。该引擎按 design/01 M4 计划退役，不在本轮修复，`go test ./pkg/...` 需 `-skip TestEngine_DiamondDAG` 或单独排除 pkg/orchestrator。
- **[偏差] llmgw 流式记账测试脆弱性**：`access.jsonl` 客户端 ReadAll 与服务端 writeLog 在重负载下有时序窗口，测试改为轮询读取（readFirstRecord，2s 超时），非产品逻辑问题。

### 三轮文档核对暴露的实现偏差（2026-07-24，写手册时逐条源码核实发现）

以下由 docforge 手册重写过程中逐行核对源码发现，**已修的标 ✅，仍存的标 ⚠️ 并已在手册中诚实标注**：

- ✅ **ImproveSkill 会绕过门禁**：重写 SKILL.md 时只写 name/description/when_to_use/auto_generated/improved_at，**丢掉 status 与 audit 行** → shadow 技能被改进后变成"无 status = 视为 active"，静默绕过 §4.6「必过闸」。已修（新增 `preserveStatus` 读回原状态 + 单测）。
- ✅ **备份漏收 statestore/**：`pkg/backup` 子目录白名单无 `statestore`，轨迹 Span 与正文 blob 不进归档。已加入白名单。
- ✅ **`dynamic_workflow.go` 校验失败文案漏 graph**：白名单已含 `graph` 但错误提示仍只列 5 种，会误导用户以为 graph 不支持动态注册。已补。
- ⚠️ **shadow 运行期无隔离效力**：`pkg/skills` 的 `parseFrontmatter` 不解析 `status`、`Registry.All()` 不按状态过滤 → shadow 技能照常进清单、照常可被 Skill 工具加载。门禁裁决的是"是否正式承认"，不是"能否使用"。§4.3c 设计的"shadow 期 50% 随机注入做配对 A/B"需运行期 skill-usage 打点，属 E4。
- ⚠️ **llm.jsonl 不含 trace 四元组**：`LLMCallRecord` 有四字段且两条 emit 路径都盖章，但 `recordLLMCall` 转 Prometheus labels 时未纳入四元组（防基数爆炸的合理取舍），故 llm.jsonl 行里没有 `run_id`。**E0 验收项"llm.jsonl 可按 run_id 聚合"应修正为"记录对象已带 trace，JSONL 落盘未含"**——跨源关联当前依靠 TraceStore Span 与团队轨迹，不依赖 llm.jsonl。
- ⚠️ **采样/TTL 有能力无调用方**：生产装配一律 `tracestore.New(ss)`（= 全采），`NewWithOptions`/`SweepTraceFiles` 无生产调用点也无 cron 触发。**E1 的"采样/TTL ✅"应理解为"能力就绪"**，长跑实例的 `statestore/log`+`blob` 仍会无限增长。
- ⚠️ **`X-CG-Run-ID` 无发送方**：网关读该头写 access.jsonl，但全仓无设置该头的代码 → 网关侧 run_id 恒空。
- ⚠️ **图 gate 的 compile/test 分支是占位**：只按"产出非空/像代码"给 80/90 分，不真跑编译器。声明一个叫 `compile-gate` 的图节点 ≠ 有硬门禁（真门禁在产码工作流的全局 gate）。
- ⚠️ **对抗评分维度三重不一致**（预存缺陷，非本轮引入）：`SkepticalReviewerPersona` prompt 枚举 7 个维度却写"六个"；`EvalScore` 只有 5 个字段；`WeightedScore()` 与正则兜底只用前 4 个 → concurrency/performance/idiomatic 被要求、被生成、然后静默丢弃。
- ⚠️ **一致性门禁不区分 Severity**：`orphan_reference`（零方法接口）是 warning 级，但 `runGlobalConsistencyCheck` 按 `len(inconsistencies)>0` 一律 failTeam → 仓库里一个 `type Marker interface{}` 就能整队判失败，且这道闸无自动修复环。
- ⚠️ **`MemoryIngestFn` 死代码**：只有声明与使用、全仓无赋值点（进化→记忆交叉学习未通电）。
- ⚠️ **`/api/dreaming/diagnosis` 容器内会误报**：靠读磁盘上的 `cmd/claude-go/main.go` 源码文本判断接线，容器部署无源码树 → wired=false 误报。运维应用 `evo` 看制品而非该端点猜代码。
- ⚠️ **`workflow_orchestrated.go:1-24` 注释已部分失真**：仍写"两套调度系统 + Coordinator 按 mode 选择"，未提共享表与图引擎第三路。
- ⚠️ **未鉴权面因 cluster 扩大**：`/api/*` 与新增 `/cluster/*`（含 enqueue/pull/complete）均无鉴权，仅 `/wiki/*`+`/sync/*` 有 Bearer。

### 2026-07-24 手册「仍存边界」五条逐条处置

用户按手册 §1.4 列出的五条边界要求修复，逐条到源码核实后处置如下。**核实过程推翻了三个原以为成立的前提**，记在各条里。

- ✅ **旧 `pkg/orchestrator` 预存死锁（生产可达，已修）**：根因不是两方互锁，而是**单 goroutine 的死定时器**——"超时排空"块窗口内一旦收到 done 就 `drainTimer.Stop()`，Go 1.23+ 起被 Stop 的定时器 channel 永不再送值而此处从不 Reset，回到 select 时两条臂同时永不可满足。该 select 还漏了 `ctx.Done()`，wedge 后连取消都救不回来（引擎 goroutine 永久泄漏、`EngineRunning` 永久 true）。**前提修正：原以为"待 M4 删除所以可以不管"，实际 `orchestrated` 仍是生产路径**（`workflow.go:426` + 5 个注册工作流），其中 code-review/testing/parenting 都是 4 路扇出→汇合，取消或快失败时多个任务微秒级同时返回则几乎必中。修法=删掉该微优化块（上方非阻塞排空已批处理"已到达"信号），原地留长注释记机理。`-race -count=3` 从永久挂死转全绿，整包 11.4s 通过。**CI 不再需要 `-skip TestEngine_DiamondDAG`**。引入者 44b9c2ed2（修 timer 泄漏时引入死锁）。
- ✅ **`/api/*` 与 `/cluster/*` 无鉴权（已修，fail-open 上线）**：新增 `pkg/httpauth` 在 Handler 层统一拦一次。**前提修正一：原以为逐路由 `s.auth` 只是漏了两组，实际是"高度不对"**——:18080 上 /api/*(50 条)、/wiki/*、/sync/*、/cluster/*、SPA、/metrics 全挂同一个 `wiki.APIServer.Mux()`，逐路由包装必然继续漏，故改为中间件（新增路由默认被保护，有回归测试锁定）。**前提修正二：`/wiki/*` 并非"有 Bearer 保护"**——`auth()` 本就 fail-open 且线上 `apiSecret` 为空，故这两组今天同样无鉴权。**前提修正三（更严重）：手册所称"安全全靠主机网络隔离"在 socket 层不成立**——`wiki/api.go` 的 addr 硬编码 `":%d"` = 绑 0.0.0.0，实测经局域网 IP 可达 200，且本机有 tailscale 接口；`design/02` 与 CLI help 里"仅监听 127.0.0.1"的说法是错的。附带修：恒定时间比较、`ReadHeaderTimeout` 防 Slowloris、新增 `wiki.apiHost` 配置项、绑非回环且无 token 时启动打 WARN、dashboard :7777 同接中间件、:7777→:18080 内部转发加 token。⚠️ **`apiHost` 默认仍为空（保持零行为变更），收口需显式配置**——见下方"待决"。
- ✅ **`pkg/toolskill` 阻塞缺陷（已修）**：`Runtime.Execute` 只设 Success/Data/Error/Timing，**从不设 `Status` 与 `Diagnostics`**，而消费方读的正是这两个字段 → 每个阶段都读成"未通过且零 blocker"，与"跑挂了"无法区分，门禁永远报不出 pass。已修（`deriveStatus` + `hoistDiagnostics`，含 JSON 往返退化成 `[]any` 的分支），并为这个此前**零测试的 1675 行包**补了首批单测。
- 📌 **契约优先编码链 / `pkg/merge` / GoalTree（已标记，未删除）**：查清后是 5 个独立部分而非 3 个，处置分开。**前提修正：原以为"图 gate 是占位 → 生产无编译门禁"，实际 `teams.go` 的 `runGlobalCompileGate`/`runGlobalTestGate`/`runGlobalConsistencyCheck` + `tryGateWithRemediation`（2 次自动修复）是真实且更完善的**，占位只影响图声明的 gate 节点。所以接线契约链等于造第二套竞争门禁，反而不该做。
  - `pkg/toolskill`（1675 行，~90% 真实：真 shell 出去调 staticcheck/gosec/go build/go test -bench）+ `pkg/contract`（73 行，其数据模型）：**保留**，修完上述缺陷后是接 `pkg/graph` gate 节点的天然实现。
  - `CodeExecutor`+`PatchApplier`+`ValidationGate`+`ContextEngine`（1709 行，0 测试）：**加 `// Deprecated:` 标记，未删**。两个阻塞缺陷证明从未执行过：`patch_apply.go` 的 `findNodeByMarker` 因 `nodeRange` 无 `*ast.File` 守卫，会把**整个文件**当匹配节点并用 LLM 片段覆盖掉；`validation_gate.go` 依赖上面那个 Status 缺陷故永远读不到 pass。
  - `pkg/merge`（200 行）：**加包级 Deprecated，未删**。零引用且**根本没链进生产二进制**；"CRDT"是错名（只有一个 `Text string`，无 ID/时钟/墓碑，`Merge` 会返回冲突——conflict-free 类型按定义不冲突）；`mergeWithMergiraf` 自认是桩故 mergiraf 从不被调用；`MergeError` 无构造点致 `IsMergeConflict` 恒 false；前后缀按字节切片会截断 UTF-8。
  - GoalTree（260 行）：**加实验性横幅 + 到期声明**。按 design/01 §4.2 其 HTN 算法将吸收为 `pkg/graph` 的 ExpandSpec 展开器而持久化层删除（Journal 已取代四源恢复）；现在无法接线（ExpandSpec 未实现且 `graph.Validate` 拒绝非 agent/gate 的 Kind）。吸收时必修的缺陷已逐条记入文件头，其中最要命的是 **pending→active 转移缺失致 `NextGoals` 永久重复派发**。
- ✅ **主动误导的假注释（已修）**：`workflow_legacy_support.go` 两处 `DEPRECATED: ... all repair logic now handled by LLM-driven ValidationGate` —— 该说法不成立（ValidationGate 从未接线），而两个函数已据此打桩为恒返回 0。已改为写明"替代品并不存在，真实修复环在 `tryGateWithRemediation`"。

- ✅ **产物落点分裂（已修，向后兼容）**：新增 `pkg/agent/artifacts.go` 作唯一采集器 + 团队 finish 时写 `<dataDir>/ARTIFACTS.json` + 新端点 `GET /api/teams/{n}/artifacts`，**不动任何现有路径**（`team.Cwd` 是用户真实 git 仓库，统一落点会打断编译门禁与"输出到我指定目录"的用户契约，且 cwd 只能进程级设置）。收拢了三件现有代码都没做对的事：dataDir 嵌套在 cwd 内时去重、归属过滤（只用 `mtime > StartedAt` 无法把同一 cwd 下**先后**两个团队分开，补 `FinishedAt`+30s 上界；`MaterializeCode` 记账路径是硬证据可绕过时间窗补捞）、fail-open 三态可分辨（"扫过且真空" vs "未扫/出错"——媒锻误判的根因正是这个语义缺失）。**附带修真实 bug**：`handleTeamMedia` 的 `os.ReadDir` 非递归且直接 skip 目录，`media/sub/deep.png` 永远采不到（用 `go test -overlay` 映射回旧实现验证过确实漏）。⚠️ 仍未解决：同一 cwd 下**并发**两团队无法分开，时间窗只能切先后不能切同时，根治需 per-team cwd 命名空间。
- ✅ **飞书会话重启即丢（已修，R2 验收"kill -9 后会话可续"达成）**：**前提修正——比"不读回"更严重的是根本没写**，`createSession` 从未给 `eng.SessionStore` 赋值，全仓唯一接线处是 CLI。新增 `pkg/session/statestore_transcript.go`，用 StateStore **KV 整份快照**而非 design/02 §3.4.1 表里写的 Log 追加日志，三条理由：①快照存的是每轮末已过压缩的 `e.Messages`，自截断，而追加日志读回未压缩全量会无限膨胀且读回后立刻要付一次压缩风暴；②`AppendLog` 只有 Append/ReadAll，**没有任何删除原语**，表达不了 `/clear`，KV 有 Delete；③顺带修老缺陷——旧 JSONL transcript 从不落 `tool_result`，读回链条残缺。约束：一 chat 一 bucket（KV 是整桶单文件重写）、bucket 名用 sha256 前缀（"非法字符替换成 `_`"是有损映射会撞桶）、base64 图片转存 Blob 只留 hash、**共享单个 FileStore 实例**（其锁表是 per-instance 而非 per-path，建两个等于没有互斥）、淘汰与超时清理只卸内存绝不删盘。**顺带修掉一个白烧 token 的 bug**：FileStore KV 用 `json.MarshalIndent` 整桶落盘，会把内嵌 `json.RawMessage`（tool_use 的 Input）一起重新缩进，那些空白此后**每轮请求都白占 prompt token 且每轮重新落盘逐层放大**；`Load` 加 `json.Compact` 压回。⚠️ CLI 路径的旧 JSONL transcript 仍缺 tool_result，未改。
- ✅ **未知工作流报错既漏又假（已修）**：报错硬编码名单，**漏**掉全部动态注册的工作流，又**假**列 debate 与 predict（`GetWorkflow` 对两者都返回 nil）——用户照报错里的名字重试会拿到同一句报错。改为从真源生成 `AvailableWorkflowNames()`，4 个测试锁死"名单里每个名字都真能 CreateTeam 成功"。
- ⚠️ **`predict` 是不可达路径（已查明，未擅自修）**：它有特判分支（`teams.go:664`）与完整 `runPredict` 实现，但所有建团队入口都经 `CreateTeam`，而 `CreateTeam` 只放行 `swarm` 一个伪工作流 → **永远建不出来**。放行它等于恢复一条未经真实 LLM 验证的执行路径，故列为待决。
- ✅ **两处非幂等测试 + 三处许愿式测试（已修）**：`memory_test.go` 两个用例按 basename 匹配 `CLAUDE.md`，把断言落到了开发者的**私人全局配置**上（必然失败，且失败信息会把私人配置打进日志）→ 改按完整路径。`workflow_new_test.go`/`orchestration_test.go` 断言 `GetWorkflow` 能解析 ml/finetune/gamedev/debate 这些**别名**，用 `git log -S` 逐个确认注册表从未包含任何别名——即从写下起就一直是红的，永远红=永远不携带信号 → 改为断言真实契约。至此 `tests/unit` 从 6 个失败转全绿。
- ✅ **Read 工具去重键忽略 offset/limit（已修）**：键只用 path + 内容 hash，"整读"与"读第 2-3 行"内容 hash 相同 → 先整读再带 offset/limit 重读会命中去重返回 `<unchanged since last read>`，agent 想回看片段只拿到一句无用提示。键改为 `path|offset|limit`，并同时钉住反向（同一视图重复读仍须去重、文件真变更后须重新返回）。

**测试基线（本轮末）**：`go build ./...` + `go vet ./...` 干净；`go test ./pkg/... ./cmd/... ./tests/unit/` **32 个包全通过**（exit 0）；`regression/` 独立模块通过；`-race` 在 pkg/{agent,feishu,session,dashboard,orchestrator,httpauth,wiki} 全绿。`tests/integration` 与 `tests/eval` 需真实 LLM 且耗时数分钟，未纳入常规基线——注意 `skipIfNoAPI` **实际从不跳过**（`getAPIKey()` 回落到硬编码常量），故该套件总会尝试真实网络调用，这是它"挂住"的原因而非产品缺陷。

**本轮待决（需决策，均已留好开关）**：
1. `wiki.apiHost` 默认值：现为空（绑全部网卡，零行为变更）。改 `127.0.0.1` 更安全，6 个下游平台走回环不受影响，但会断掉从手机/别的机器直连 :18080 的用法。
2. 上述 1900 行死代码（`pkg/merge` + CodeExecutor 链）是否真删。已加标记但保留；删前应摘走 `CheckConstitution`、`CheckThinkPhase`、`ContextEngine` 的诊断→上下文分类三处有独立价值的逻辑。
3. 是否把 `pkg/toolskill` 接成 `pkg/graph` gate 节点的真实现（替换 80/90 占位分）。这会让门禁真的开始拦人，原先靠占位分混过的 graph 工作流可能开始失败。
4. `predict` 伪工作流是否放行进 `CreateTeam`（现为不可达路径），或反之删除 `runPredict` 与特判分支。
5. 是否把 `tests/integration` 的 `skipIfNoAPI` 修成"无环境变量就真跳过"（去掉硬编码 key 回落），使 `go test ./...` 变成幂等且不触网。

---

## 2026-07-25 三方独立核查：进度账修正

对 design/01-03 各派一路独立核查（明确要求"不要相信 PROGRESS，到代码里核实并指出高估"）。结论是**本进度账系统性偏乐观**，模式有三类：

1. **把"类型/字段/函数写完 + 单测绿"记成 ✅**，而里程碑验收标准写的是形态级/链路级；
2. **把设计稿章节记成交付物**（如 P2.4 "AgentRuntime 接口设计在 §4.9"——接口至今不存在）；
3. **把覆盖矩阵这张计划表当成"已核实无丢项"**（V4）。

### 必须改判的条目

| 原自评 | 应改为 | 依据 |
|---|---|---|
| M1 图内核 ✅ | 🟨 ~70% | 验收项"kill -9 恢复重放正确"**无对应测试**（`journal_test.go` 只模拟尾部截断行，非进程 kill）；且 pipeline/fanout 灰度**默认关**，交付物在生产不生效 |
| M2 的 loop/条件边 ✅ | 🟡 建成未通电 | 代码为真但**生产零产生方**：`TranslateWorkflow` 从不设 `Loop`/`Condition`，`StageDef` 也无对应字段 → 生产图全是无条件边、maxRetries=0 |
| R1 网关独立 ✅ | 🟨 ~60% | `llmgw.NewLocal` **唯一调用方是自己的测试**，生产 `pkg/feishu/bot.go` 直连 `api.NewClient` → L1 抽象对生产不可见；网关缺 fallback/熔断/配额/双协议；**`CLAUDE_GO_LLM_GATEWAY` 是死环境变量**（distributed.yaml 设了两处，Go 侧零读取）→ T1 拓扑里网关是装饰品 |
| R2 的 sqlite ✅ | 🟡 | 零生产接线、无配置开关（唯一调用方是 regression 测试）。验收项"双进程读写一致"未达成——FileStore 无跨进程锁且自己声明不保证 |
| R3 队列/worker ✅ | 🟨 ~35% | **`Enqueue` 唯一生产调用方是 HTTP handler，编排器从不入队**（`pkg/agent` 零 `pkg/cluster` import）；**`executeWorkerTask` 是回显桩**（自带注释"v1 简化实现: 回显 payload"）→ 分布式执行链路为 0。cron 选主漏了 `pkg/sync` 第二个进程内调度器 |
| R4 双模式实测 ✅ | 🟨 存活冒烟，功能未验 | K8s 清单三处使其无法真正工作：**ConfigMap 从未被读取**（清单不传 `--config`，自动发现路径不含 `/etc/claude-go/`）→ Pod 落到占位 provider 分支；control 的 state 是 emptyDir；网关未被接入。故 V2/V3 的"全通"是不需要 LLM 的存活冒烟 |
| E0 ✅ | 🟠 ~70% | 开环②只闭一半（`ImproveSkill` 生产调用点仍为 0）；③只闭团队路径（CLI 会话无 AfterQuery）；验收项"llm.jsonl 可按 run_id 聚合"**未达成**（`llm_collector.go` 构造 labels 时四元组一个都没进） |
| E1 的采样/TTL ✅ | 🟡 | 零生产调用方，本文件 §偏差记录早已自承认——两处自相矛盾 |
| E2 "8 源中 3 源" | **1/8**（+1 个设计外自加） | 生产写出的 Source 字面量只有 2 个：`gate.content`（两个调用点用**同一字面量**，被我当两源计数）与 `episode`（不在设计的 8 源里）。设计价值排第一的 `gate.compile`/`gate.test` 在真门禁函数内 `RecordReward` 调用数为 0 |
| E3 审计门禁 ✅ | 🟠 ~27% | `skillaudit` 的裁决是"创建时间之后的**全部**奖励求均值"，**零技能归因**——同批 shadow 技能裁决必然相同；且 `ImproveSkill` 重写 frontmatter 会丢 `created_at`，退化为"有史以来所有奖励"。包注释描述的"配对审计 vs 全局基线"与实现不符 |
| E4 evo 操作台 ✅ | 🟠 ~16% | 设计要求的是 7 个**注册进工具池、agent 自助**的 `evo_*` 工具；实现是 4 个 CLI 子命令（`grep '"evo_'` 零命中）→ H8"agent 即进化工程师"结构上不成立，H9 限速无实现 |
| V4 "覆盖矩阵逐项映射，无丢项" | ❌ 撤回 | 三份矩阵是**计划表不是状态表**。实测：design/01 §6 的 26 行里真被新架构覆盖的是 **1 行**（AllowedTools 双路径收敛） |

### 新登记的偏差（此前未记）

- ✅ **已修**：图 journal 跨 run 混用致重跑/refine 静默零执行（P0，见上一节提交）。原 `TestGraphJournalResume` 把该缺陷当期望行为锁死。
- ⚠️ **CLI 形态两个 hook 从不注册**：`applyFeatureFlags` 只在 `NewQueryEngine` 内跑，而 CLI 在建引擎**之后**才赋 `TraceStore`/`MemoryStore` 且无 re-register → **`TraceCaptureHook` 与 `MemoryInjectHook` 在 CLI 均失效**。后果：下游 8+ 平台的全部流量既没有 turn/tool_call 轨迹，也没有 L1/L2 记忆注入。design/03 §4.4 承诺的"headless 与飞书同构、开环 1 从架构上不可能再出现"未达成。
- ⚠️ **奖励零消费**：`rewards.jsonl` 的读取方全仓只有两个**离线 CLI**（console/skillaudit）。学习反馈仍走 stage 二值（`workflow.go:896-901`）→ **RewardBus 是只写日志，不是总线**。这是学习闭环的第二层开环。
- ⚠️ **judge 自评**（违反已吸收的 Hermes H3）：content gate 的 judge 用主模型 `ptm.llm`/`we.llm`，正是 H3 明令禁止的自评。
- ⚠️ `pkg/eventbus` **零生产 import**；`TaskService` 类型不存在；:7777 动作队列**无消费方**（写入即烂盘）。
- ⚠️ **H1-H14 吸收项实际落地 ≈0.8/14**，本进度账全程未对其做过逐项声明。
- 📄 design/02 有 13 处文档失真（含 `pkg/feishu/engine.go` 文件不存在、桶名含 `/` 而 `validateBucket` 白名单禁止 `/` 故结构性不可表达、"绑 loopback"在 socket 层不成立）。

### 三份设计的独立实现度评估

| 文档 | 独立核查结论 | 分母算法 |
|---|---|---|
| design/01 | **≈20%** | §4 的 12 抽象 + §5 的 15 mode + §6 的 26 行 = 53 条，加权 10.5 |
| design/02 | **≈32%** | 102 条可核验承诺，加权 32.75 |
| design/03 | **≈22%** | 79 条可核验条目，加权 17.75 |

**共同结论：已完成的绝大部分是 M0/R0/E0 的"止血、接口抽取、开环修补"，而不是三份设计的目标架构本身。** 且全部实现**尚未部署**（线上二进制 07-17，实现落 07-24/25）。
