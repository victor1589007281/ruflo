# design/ 总览：claude-go 下一代架构三部曲

> 日期：2026-07-23。基于当前 HEAD `6bd8da22f` 的全量源码梳理（pkg 约 11.4 万行 Go）。
> 本目录三份方案互相衔接，共同构成 claude-go 从「单进程单机 agent 系统」演进为「可分布式部署、可自我进化的统一 agent 编排平台」的完整蓝图。

## 三份方案

| 文档 | 主题 | 一句话 |
|---|---|---|
| [01-unified-agent-orchestration-engine.md](01-unified-agent-orchestration-engine.md) | 统一抽象 Agent 编排引擎 | 三套并行编排引擎收敛为一个 DAG 图引擎：一切皆 AgentNode，节点自带 hook/loop/rule/skill/subagent 能力，统一通信与任务机制，支持远程 agent 与全局逻辑注入 |
| [02-cloud-native-layered-architecture.md](02-cloud-native-layered-architecture.md) | 云端分离分层架构 | 单进程上帝对象拆成五层（LLM 引擎层 / 编排层 / Agent 运行时 / 辅助系统 / 用户层），同一代码库支持 single-binary 与分布式两种部署形态 |
| [03-rl-evolution-engine.md](03-rl-evolution-engine.md) | RL 进化引擎 | 把进化引擎、Dreaming、记忆、技能自创建统一为一个以轨迹为底座、以奖励为驱动的 RL 闭环：记忆整理 + 经验总结 + skill 进化 + 工作流/prompt 进化（v1.1 已吸收 Hermes-Agent Tinker-Atropos RL 引擎源码调研 14 项设计点） |

## 三者的依赖关系

```
                    ┌─────────────────────────────┐
                    │  02 分层架构（部署形态与边界）  │
                    │  L5 用户层                    │
                    │  L2 编排层 ◄─── 01 图引擎      │
                    │  L3 运行时                    │
                    │  L4 辅助系统 ◄── 03 进化引擎   │
                    │  L1 LLM 引擎层                │
                    └─────────────────────────────┘
```

- **01 是内核**：编排引擎是 02 分层中「Agent 编排层」的具体实现；01 定义的 Hook 总线与拦截器（Interceptor）是 03 采集轨迹、注入经验的挂载点。
- **02 是骨架**：定义每层的接口边界与状态归属；01 的 RuntimeRegistry（远程 agent 管理）依赖 02 的注册/心跳/通信总线。
- **03 是循环**：作为 02 的 L4 辅助系统之一运行，消费 01 产生的统一轨迹（trace-id 贯穿），产出的经验/技能/记忆经 01 的注入点回流。

## 共同设计原则

1. **抽象优先于协议，接口优先于进程**：先在单进程内把边界抽干净（Go interface），再决定哪条边界升级为网络协议。每一步都保持 all-in-one 单二进制可用。
2. **覆盖现有全部功能**：三份文档各含「现有功能覆盖矩阵」，30+ 工作流、15 种 mode、三套 hook、飞书/CLI/dashboard/MCP/wiki/sync/cron 全部入口、进化/dreaming/记忆全部能力，逐项映射到新架构，不丢任何一项。
3. **单一真源**：模式分发、检查点、黑板、工具授权、token 记账——凡是当前存在多份实现/多处状态的，收敛为一份。
4. **fail-closed 的治理，fail-open 的交付**：安全与授权默认拒绝；任务交付保留现有 `delivered_with_remediation` 的宽容语义。
5. **事件溯源**：图执行、轨迹、奖励全部落 append-only 事件日志，恢复=重放，学习=离线消费同一份日志。

## 推荐实施顺序（跨文档合并排期）

| 阶段 | 内容 | 来自 |
|---|---|---|
| P0（地基，~2 周） | 统一 trace-id 贯穿 llm.jsonl/transcript/团队轨迹；修三处学习开环（headless 进化、autocreate、dreaming 触发）；双 mode switch 一致性止血 | 03-E0 / 01-M0 |
| P1（内核，~4 周） | 图引擎 v1（吃掉 pipeline/fanout/adversarial 三类 mode）+ Hook 总线 + 统一 TaskStore/Journal | 01-M1/M2 |
| P2（分层，~4 周） | 接口抽取：LLMGateway/StateStore/EventBus/AgentRuntime 四大接口 + file/SQLite 默认实现；LLM gateway 可独立进程 | 02-R0/R1 |
| P3（闭环，~4 周） | 轨迹底座 TraceStore + RewardBus + 学习器族收编（经验/记忆/skill 三学习器） | 03-E1/E2 |
| P4（分布式，按需） | 状态外置（Redis/PG/NATS 后端）+ runtime worker 拉取模型 + 注册/心跳/cron 选主 + 多副本 | 02-R2~R4 |
| P5（进化，持续） | 工作流/prompt 进化器 + 离线回放评估 + 灰度治理；可选 RFT/DPO 数据导出 | 03-E3/E4 |
