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

## P1 图引擎内核（01-M1/M2）
- ⬜ P1.1 pkg/graph：GraphSpec/NodeSpec/EdgeSpec 纯数据 + Validate
- ⬜ P1.2 Journal 事件溯源 + 重放
- ⬜ P1.3 引擎调度器（ready-set + 条件边 + loop）
- ⬜ P1.4 WorkflowDef→GraphSpec 直译器 + feature flag 接入
- ⬜ P1.5 Hook 总线（外部 hook 配置兼容适配）
- ⬜ P1.6 单元测试（spec/journal/engine/直译器）

## P2 分层接口（02-R0/R1）
- ⬜ P2.1 LLMGateway 接口 + local 实现（包装 api.Client）
- ⬜ P2.2 StateStore 接口 + file 后端（basedir 布局兼容）
- ⬜ P2.3 EventBus 接口 + 进程内 channel 实现
- ⬜ P2.4 AgentRuntime 接口 + local 实现
- ⬜ P2.5 llm-gateway 子命令（独立进程模式）+ token 双边记账修复
- ⬜ P2.6 单元测试

## P3 进化闭环（03-E1/E2）
- ⬜ P3.1 TraceStore（Span 模型 + 落盘）
- ⬜ P3.2 RewardBus（多源奖励事件 + content_gate 分数持久化）
- ⬜ P3.3 学习器收编（经验/记忆触发权归 EvolutionLoop）
- ⬜ P3.4 单元测试

## P4 分布式（02-R2/R3 精简）
- ⬜ P4.1 worker 子命令（HTTP 拉取 NodeTask）
- ⬜ P4.2 控制面任务下发 + 心跳 lease
- ⬜ P4.3 cron 租约防重复触发
- ⬜ P4.4 单元/集成测试

## P5 评估与进化操作台（03-E3/E4 精简）
- ⬜ P5.1 离线回放 harness（tests/eval 扩展）
- ⬜ P5.2 evo_* 工具最小集（list/smoke/status + 锁定字段护栏）
- ⬜ P5.3 单元测试

## 验收链
- ⬜ V1 全量 go build/vet/test 绿
- ⬜ V2 K8s 部署：单体模式（all-in-one Pod）冒烟
- ⬜ V3 K8s 部署：分布式模式（control-plane + worker + gateway 多 Pod）冒烟
- ⬜ V4 功能覆盖确认（覆盖矩阵逐项核对）
- ⬜ V5 真实 LLM E2E（图引擎跑真团队 + 进化闭环出经验 + 奖励入账）
- ⬜ V6 docforge 技术文档更新（修正旧文档偏差）
- ⬜ V7 testforge 沉淀回归测试资产
- ⬜ V8 全部仓库提交推远程

## 偏差记录
（设计与实现的偏差在此登记，并回写对应设计文档）
