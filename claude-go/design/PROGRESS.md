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

## P5 评估与进化操作台（03-E3/E4 精简）⬜ 待启动
- ⬜ P5.1 离线回放 harness（tests/eval 扩展）
- ⬜ P5.2 evo_* 工具最小集（list/smoke/status + 锁定字段护栏）
- ⬜ P5.3 单元测试

## 验收链
- ✅ V1 全量 go build/vet 绿；pkg/... test 除预存 orchestrator 死锁外全绿（见偏差记录）
- ✅ V2 K8s 单体模式：Pod Running/Ready，/api/health + /api/workflows(28) + 建团队全通
- ✅ V3 K8s 分布式模式：control+2worker+gateway 四组件 Running；worker 注册可见；任务 control→worker→control 完整链路；gateway healthz+provider 路由 OK
- ✅ V4 功能覆盖确认：三份设计文档各含覆盖矩阵逐项映射（编排30+工作流/15mode/三hook、接入全端点、学习全能力）；新架构全部等价或增强，无丢项
- ✅ V5 真实 LLM E2E（真 kimi, mini-graph 图工作流 2 节点 completed）：图引擎 ready-set 调度 + journal 8 事件溯源 + TraceStore 2spans/3blobs 内容寻址 + rewards.jsonl episode 奖励 trace 关联 + evolution 2 trajectories（**开环1 headless 学习实证修复**）
- 🟨 V6 docforge 技术文档更新（进行中）
- 🟨 V7 testforge 沉淀回归测试资产（进行中）
- 🟨 V8 全部仓库提交推远程（claude-go 已多次提交，待 push）

## 偏差记录
（设计与实现的偏差在此登记，并回写对应设计文档）

- **[发现] pkg/orchestrator TestEngine_DiamondDAG 预存死锁**（2026-07-24）：旧的独立 K8s 式 DAG 引擎 `pkg/orchestrator`（design/01 §1.1 列出的三引擎之一）的 DiamondDAG 测试在 engine.go:340 的 select 上无限阻塞（90s/600s 均超时）。经 git diff 确认本轮所有提交**均未触碰** pkg/orchestrator，属预存缺陷。**佐证 design/01 收敛决策**：新 pkg/graph 的同款菱形并发测试（TestEngine_DiamondDAG 等价）通过。该引擎按 design/01 M4 计划退役，不在本轮修复，`go test ./pkg/...` 需 `-skip TestEngine_DiamondDAG` 或单独排除 pkg/orchestrator。
- **[偏差] llmgw 流式记账测试脆弱性**：`access.jsonl` 客户端 ReadAll 与服务端 writeLog 在重负载下有时序窗口，测试改为轮询读取（readFirstRecord，2s 超时），非产品逻辑问题。
