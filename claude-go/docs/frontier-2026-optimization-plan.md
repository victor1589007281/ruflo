# claude-go 前沿优化方案（2026-06）

> 基于：① `claude-go` 源码全量分析（pkg/engine, agent, orchestrator, memory, dreaming, evolution, swarm_intel 等）；② 2025–2026 业界/学术 frontier 调研（多源检索 + 对抗式事实核查，24/25 条主张通过 3-0 / 2-1 验证）。
> 目标：在**不破坏现有主链**的前提下，补齐 claude-go 相对真实 SOTA 的增量缺口。

---

## 0. TL;DR

`claude-go` 已经是一个**异常成熟**的实现 —— 它已落地了大部分"明显"的前沿能力（prompt cache、5 级 token budget、auto/micro compaction、tiered memory + 艾宾浩斯衰减、dreaming 离线固化、evolution 经验蒸馏、swarm 群体智能、DAG 编排、AIMD 背压、对抗式 best-of-N）。本次调研把它和 2025–2026 已发表且**可核查**的论文逐一对标后，结论是：

- **大部分前沿设计已被验证为"对齐 SOTA"**（Anthropic 多智能体、LLMCompiler、EvolveR 直接验证了现有 swarm/DAG/evolution 架构）。
- **真正未覆盖、且代码确认缺失的高杠杆缺口只有 4 个**：
  1. **按难度的模型路由 / 级联（RouteLLM / FrugalGPT）** —— 当前只有"出错才切备用模型"，没有按任务难度选 Haiku/Sonnet/Opus 的路由器。**成本可降 >2x。最高杠杆。**
  2. **学习型压缩指引（ACON）** —— 当前 compaction 是启发式截断；ACON 从成功/失败轨迹学"该压什么"。现有 TrajectoryStore 正好是它的输入。
  3. **抗坍缩的增量 playbook（ACE）** —— 当前 dreaming 用"整体重写"做记忆固化，正是 ACE 警告的 *context collapse* 失效模式。
  4. **LLMLingua 式提示压缩** —— 针对注入的 memory / wiki / RAG 上下文，做困惑度级压缩。
- **一类条件适用**：KV-cache 压缩 / 投机解码（Expected Attention、CodeComp、speculative decoding）—— **仅当自托管推理时有效**；若 claude-go 只调用托管 API（Claude/外部 LLM），则不适用，列为待定。

> ⚠️ 关于现有设计文档：`docs/query-engine-frontier-optimization.md` 等引用了 Claude 4.7 / GLM 5.1 / Kimi K2.5 / GPT 5.6 / Gemini Gamma 4 等模型。这些**晚于本次分析所用资料的知识窗口，无法从现有来源核实**（可能是真实的发布、也可能是占位）。因此本方案的所有量化依据，一律锚定**已核查的论文**（下文均附 arXiv 编号与验证票数），不依赖那些无法核实的引用。

---

## 1. 现状评估：哪些已经"对齐 SOTA"（验证，非缺口）

| 现有能力 | 代码位置 | 对标的已核查 SOTA | 调研结论 |
|---|---|---|---|
| Swarm / DAG 多智能体 + agent pool + 并行 | `pkg/orchestrator/engine.go`, `pkg/agent/pool.go`, `pkg/swarm_intel/` | **Anthropic Multi-Agent Research System**（多智能体 +90.2% eval；并行 3-5 子代理省时达 90%）| ✅ 架构方向正确。**关键洞察：token 用量单独解释 80% 的性能方差** → 应优先做"省 token"而非"加算力"。 |
| 并行工具调用（PartitionToolCalls）| `pkg/engine/engine.go`, `pkg/orchestrator/tool.go` | **LLMCompiler**（arXiv:2312.04511，ICML'24；延迟↓3.7x，成本↓6.7x）| ✅ 已覆盖批量并行。可借其 Planner/Task-Fetching-Unit 验证"轮内工具 DAG"（见 §3.5）。 |
| evolution（record→distill→retrieve→evolve）+ dreaming 离线固化 | `pkg/agent/evolution.go`, `pkg/dreaming/dreamer.go` | **EvolveR**（arXiv:2510.16079；离线蒸馏抽象策略 + 在线检索 + 策略 RL）| ✅ 闭环架构与 SOTA 一致。**唯一新增元素 = 策略级 RL（权重更新）**，现有为 prompt 级，属重型扩展（§4 待定）。 |
| best-of-N + meta-judge（HeavyMode / adversarial）| `pkg/agent/adversarial.go` | test-time compute 调度（多路 rollout）| ✅ 已是一种 test-time compute；带降级回滚与 resample，设计良好。 |
| AIMD 背压 + token bucket + 5 级 budget | `pkg/orchestrator/backpressure.go`, `pkg/engine/internal_hook/budget.go` | 多智能体 15x token 成本的必要护栏 | ✅ 正好对冲多智能体的 token 爆炸。 |
| prompt cache（静态前缀 hash）| `pkg/engine/internal_hook/cache.go` | Anthropic Prompt Caching 最佳实践 | ✅ 已做稳定前缀分离。可加强 §3.1 的"缓存友好顺序硬约束"。 |

**结论**：claude-go 不是"缺前沿能力"，而是"在 4 个具体维度上还能再榨一层"。

---

## 2. 前沿调研结论（三方向 · 仅列已核查项）

### 方向一：节省 token / context engineering

| 技术 | 论文 | 核心数字（均为作者自报 best-case）| 验证 |
|---|---|---|---|
| **ACE**（Agentic Context Engineering）| arXiv:2510.04618（Stanford/SambaNova, ICLR'26）| agents **+10.6%** / finance **+8.6%**，无标注监督（用执行反馈）；显式对抗 *brevity bias* 与 *context collapse* | 3-0（"匹配 IBM CUGA"那条 2-1，属夸张表述） |
| **ACON**（Agent Context Optimization）| arXiv:2510.00615（Kang et al., 2025）| 峰值 token **↓26–54%** 且精度不降；自然语言空间学"压缩指引"，gradient-free、兼容闭源 API | 3-0 |
| **LLMLingua 家族** | 2310.05736 / 2403.12968 / LongLLMLingua | 最高 **20x** 压缩仅损 ~1.5pt；LLMLingua-2 快 3-6x；LongLLMLingua 用 **1/4 token** 让 RAG **+21.4%** | 3-0 |
| **训练免 KV-cache 压缩** | Expected Attention 2510.00636（NVIDIA KVPress）/ CodeComp 2604.10235 | 50% 压缩 Ruler-4K 得分 94.7（vs SnapKV 55.7）| 3-0；**⚠️ 仅自托管推理适用** |

### 方向二：执行效率 / 降延迟

| 技术 | 论文 | 核心数字 | 验证 |
|---|---|---|---|
| **多智能体研究系统** | Anthropic 工程博客（2025-06）| +90.2% eval；agent ~4x token、multi-agent ~15x；**token 用量解释 80% 方差**；并行省时达 90% | 3-0（厂商博客，非同行评审）|
| **LLMCompiler** | arXiv:2312.04511（ICML'24）| 延迟 **↓3.7x**、成本 **↓6.7x**、精度 +9%（vs ReAct）| 3-0 |
| **RouteLLM** | arXiv:2406.18665（2024）| 学习型路由器，成本 **↓>2x** 不降质 | 3-0 |
| **FrugalGPT** | arXiv:2305.05176（2023）| 级联匹配 GPT-4 质量、成本 **↓最高 98%**；或等成本 **+4%** 精度 | 3-0 |

### 方向三：自我进化

| 技术 | 论文 | 核心 | 验证 |
|---|---|---|---|
| **EvolveR** | arXiv:2510.16079 | 离线蒸馏可复用抽象原则 → 在线检索 → 策略 RL 闭环 | 3-0 |
| **ACE**（同上）| 2510.04618 | playbook 的 *generation→reflection→curation*，**增量 delta 更新**而非整体重写 | 3-0 |

> 被对抗式核查**否决**的主张（1-2）："现有 KV 压缩仅依赖注意力信号、丢弃结构关键 token" —— 已剔除，不作为依据。

---

## 3. 优化方案（按优先级）

### P0-A：按难度的模型路由 / 级联 —— **最高杠杆**

**对标**：RouteLLM（2406.18665，↓>2x）、FrugalGPT（2305.05176，↓最高 98%）。
**现状（代码确认）**：`pkg/api/client.go:806,1237` 只有 **出错→切 fallback 模型**；`pkg/agent/swarm.go:294 RouteRoles` 只选"哪些角色"，**不选"哪个模型"**。**完全没有按难度选模型**。

**改造**：新增 `pkg/engine/modelrouter`（或 `internal_hook/model_router.go`），两段式：

1. **轻量路由（RouteLLM 式）**：对每个 user turn / 每个 sub-agent 任务，用廉价信号（意图长度、是否含代码、工具类型、`RouteRoles` 已算的 ComplexityProfile、历史同类难度）估难度分 → 选 Tier：
   - 简单（格式化、单文件读取、确定性转换）→ 最便宜模型
   - 中等 → 中档
   - 复杂（架构、调试根因、跨模块）→ 最强模型
2. **级联（FrugalGPT 式，可选）**：先用便宜模型 + 一个 verifier 打分；分数低于阈值才升级到强模型。与现有 `adversarial.go` 的评分器天然复用。

**落地点**：在 `engine.go` queryLoop 选模型处、`agent/orchestrator.go` 分发 sub-agent 处接入。复用现有 `EngineMetrics` 暴露 `route_tier_distribution`、`cascade_upgrade_rate`、`cost_per_turn`。

**预期收益**：成本 ↓ 30–60%（保守，取决于简单任务占比），延迟在简单任务上同步下降。
**风险/开关**：`cfg.Engine.ModelRouter.Enabled`（默认 OFF→灰度）。**与 prompt cache 的交互需实测**：切模型会破坏缓存局部性，应保证"同一会话同一 tier 粘滞"（sticky routing），避免反复跨模型抖动反而涨成本（这是调研留下的 open question，需 A/B 验证）。

---

### P0-B：学习型压缩指引（ACON）

**对标**：ACON（2510.00615，峰值 token ↓26–54% 不降精度）。
**现状**：compaction 为启发式（`pkg/compact/compact.go` 截断 + `message_filter.go` 停用词删除），不"学"该压什么。但 **`pkg/engine/internal_hook/trajectory.go` 的成功/失败轨迹正好是 ACON 的输入**。

**改造**：新增离线任务 `pkg/dreaming` 内的 `compression_guideline`：定期取配对的（成功轨迹, 失败轨迹），用 LLM 在自然语言空间归纳"哪些观察/工具输出可安全压缩、哪些必须保留"的指引文本；把该指引注入 MicroCompact / Budget Degrade 的决策（而非纯按字符长度截断）。

**落地点**：guideline 文本作为新的 static-prefix 片段或 compaction 配置；消费方为 `internal_hook/budget.go` 的 `summarizeToolResult` 与 `compact.MicroCompact`。
**预期收益**：在长会话/多工具场景，token ↓ 额外 15–30%，且比盲截断更少丢关键信息。
**风险/开关**：`cfg.Compact.LearnedGuideline`（默认 OFF）。指引需版本化 + 可回退到纯启发式。

---

### P1-C：抗坍缩的增量 playbook（ACE）—— 修复 dreaming 的真实失效模式

**对标**：ACE（2510.04618）的核心是 *增量 delta 更新 + curation*，显式防 **context collapse**（整体重写导致信息坍缩/越改越短）。
**现状（真实风险）**：`pkg/dreaming/dreamer.go:516-563` 的固化是 **"按 topic 整体重写/合并"**，并有 "max 50 files" 的 prune —— 这正是 ACE 警告的"单体重写"路径，长期迭代有信息坍缩风险。

**改造**：把 dreaming/evolution 的记忆更新从"重写"改为 ACE 式三步：
- **Generation**：从新会话产出候选条目；
- **Reflection**：对照已有 playbook 标注"新增/强化/矛盾/冗余"；
- **Curation**：**append-and-localize delta**（只增量改动相关条目，保留 itemized 结构），而非整篇重写。
保留现有矛盾检测（`dreamer.go:641-669`）作为 Reflection 的一部分；保留艾宾浩斯衰减做"软遗忘"，但**禁止压缩阶段直接删条目**，改为标记降权。

**落地点**：`pkg/dreaming/consolidator.go` 的 merge 逻辑、`pkg/agent/evolution.go` 的 Consolidate。
**预期收益**：自我进化质量更稳（对标 ACE +10.6% 量级的"上下文质量"提升），消除长期记忆坍缩。
**风险/开关**：`cfg.Dreaming.IncrementalCuration`（默认 ON，因为它**降低**而非增加风险）。

---

### P1-D：LLMLingua 式提示压缩（针对注入上下文）

**对标**：LLMLingua（2310.05736，20x）、LongLLMLingua（RAG +21.4% @ 1/4 token）。
**现状**：`message_filter.go` 仅停用词删除（粗粒度）；注入的 memory/wiki/检索片段未做困惑度级压缩。

**改造**：在"记忆/wiki/RAG 片段注入 system prompt 之前"加一个压缩 pass。两种落地：
- **轻量**：用一个小模型（如最便宜的 Haiku 档，与 P0-A 路由器复用）做困惑度/重要性打分式删减；
- **离线**：对高频复用的 wiki 概念页预压缩并缓存。

**落地点**：`pkg/memory` retrieve → `pkg/prompt` 注入之间，新增 `prompt/compressor.go`。
**预期收益**：注入上下文 token ↓ 2–5x，长上下文场景明显。
**风险/开关**：`cfg.Prompt.Compressor`（默认 OFF）。**注意**：压缩本身要花一次小模型调用，需保证净收益（仅对 >N token 的片段启用）。

---

### P1-E：轮内工具 DAG 规划（LLMCompiler 增量）

**对标**：LLMCompiler（2312.04511）的 Planner→Task-Fetching-Unit→Executor，依赖就绪即流式派发。
**现状**：现有并行只覆盖"模型在一条响应里同时发出的多个工具调用"；**不主动规划带依赖的工具 DAG**。
**改造**：对可预测的多步只读操作（如"读 3 个文件 + grep + glob"），让模型先产出一个轻量工具计划，由现有 DAG 调度器（`orchestrator/engine.go`）在轮内按依赖流式派发。属**锦上添花**，仅在 benchmark 证明收益后启用。

---

### P2（待定 / 条件适用）：KV-cache 压缩、投机解码、策略级 RL

- **KV-cache 压缩**（Expected Attention 2510.00636 / CodeComp 2604.10235）、**speculative decoding**：**仅当 claude-go 自托管推理时适用**。若全程调用托管 API（Claude/外部 LLM），**不适用**，不建议投入。→ 需先回答 open question：*claude-go 是否存在本地/自托管推理路径？*（`pkg/dashboard/llm_client.go` 暗示有外部 LLM client，需确认是否自托管。）
- **EvolveR 式策略级 RL**（权重更新）：现有 evolution 为 prompt 级；上升到 RL 需训练数据与训练栈，属重型长期演进，非当前性价比之选。

---

## 4. 实施路线图

| 阶段 | 内容 | 预期主收益 | 默认开关 |
|---|---|---|---|
| **Sprint 1** | P0-A 模型路由（先轻量路由，不上级联）+ 指标 | 成本 ↓30%+ | OFF→灰度 |
| **Sprint 1** | P1-C ACE 增量 curation（改 dreaming/evolution 合并）| 进化质量稳、防坍缩 | ON |
| **Sprint 2** | P0-B ACON 学习型压缩指引（复用 TrajectoryStore）| token 再 ↓15–30% | OFF |
| **Sprint 2** | P0-A 级联（FrugalGPT 式，复用 adversarial 评分器）| 成本进一步 ↓ | OFF |
| **Sprint 3** | P1-D LLMLingua 注入压缩 | 注入 token ↓2–5x | OFF |
| **Sprint 3** | P1-E 轮内工具 DAG（benchmark 后）| 延迟 ↓ | OFF |
| **待定** | P2（KV/投机/RL）| 取决于是否自托管 | — |

**实施原则**（沿用仓库既有 §2 设计哲学）：零破坏、单文件可插拔、`cfg.Enable*` 开关、指标先行、故障自动回退基线。

---

## 5. 验收指标

| 指标 | 当前 | 目标 | 关联项 |
|---|---|---|---|
| 每 session 平均成本 | 基线 | ↓ ≥30% | P0-A |
| 简单任务平均延迟 | 基线 | ↓ ≥40% | P0-A |
| 长会话注入/上下文 token | 基线 | ↓ ≥20% | P0-B / P1-D |
| 记忆固化坍缩率（条目信息量回归）| 未测 | 0 回退 | P1-C |
| 路由分布 / 级联升级率 | 无 | 可观测 | P0-A |
| prompt cache 命中率（启用路由后）| 现值 | **不下降** | P0-A 粘滞路由护栏 |

---

## 6. 来源与可信度说明

- **高可信（同行评审 / arXiv 主源，3-0 通过）**：ACE 2510.04618、ACON 2510.00615、LLMLingua 2310.05736 / 2403.12968、LLMCompiler 2312.04511、RouteLLM 2406.18665、FrugalGPT 2305.05176、EvolveR 2510.16079、Expected Attention 2510.00636、CodeComp 2604.10235。
- **中可信（厂商博客，非同行评审、专有 eval、未独立复现）**：Anthropic Multi-Agent Research System（"token 解释 80% 方差"等 best-case 数字）。
- **所有量化均为作者自报 best-case（"up to"）**，非平均；落地前以本仓 A/B + EngineMetrics 实测为准。
- **缺口分析已对代码二次确认**：模型路由仅 error-fallback（`api/client.go:806,1237`）、`RouteRoles` 只选角色（`swarm.go:294`）、`<guidelines>` 为硬编码（`prompt.go:191`）—— P0-A/P0-B 缺口成立。
- **未决问题**：claude-go 是否有自托管推理（决定 P2 是否适用）；路由跨模型是否破坏 cache 局部性而净涨成本（需 A/B）。
