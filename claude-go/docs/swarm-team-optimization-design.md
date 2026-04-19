# 蜂群模式优化 + 团队深度思考增强方案

> **版本**: 1.0 | **日期**: 2026-04-19
> **目标**: 解决蜂群模式的三大核心问题(动态角色选择/运行稳定性/质量防退化)；
> 借鉴大模型深度思考设计优化研究/TechBlog/辩论团队。

---

## 一、蜂群模式现状分析

### 1.1 蜂群 vs 研发团队(Pipeline)对比

```
┌─────────────────────────────────────────────────────────────────────────┐
│                  蜂群 vs 研发团队 差异分析                                │
├─────────────────┬──────────────────────┬────────────────────────────────┤
│     **维度**     │    **蜂群 (Swarm)**    │     **研发团队 (Pipeline)**     │
├─────────────────┼──────────────────────┼────────────────────────────────┤
│ 角色定义         │ LLM 动态生成          │ WorkflowDef 固定定义           │
│ 任务分解         │ 单次 LLM 分解         │ 人工精心设计的 Stage 序列       │
│ 质量闭环         │ ❌ 无 (仅输出校验)     │ ✅ AdaptiveTerminator 多维评分  │
│ 检查点恢复       │ ❌ 不走 Coordinator    │ ✅ CheckpointStore 阶段级恢复   │
│ 失败处理         │ 部分失败继续 merge     │ 阶段失败则整体失败             │
│ 角色提示词       │ 通用 "你的角色: X"     │ 深度定制的领域 Prompt          │
│ 灵活性           │ ✅ 可动态只选某些角色   │ ❌ 必须激活所有 Stage 角色      │
│ 上下文传递       │ Blackboard + 依赖注入  │ prev_result 结构化传递         │
│ 执行可预期性     │ ❌ 不确定 (LLM 分解)   │ ✅ 高度可预期                  │
└─────────────────┴──────────────────────┴────────────────────────────────┘
```

### 1.2 蜂群核心问题 (三个根因)

| **#** | **问题**              | **根因**                                            | **影响**                  |
|:-----:|:---------------------:|:---------------------------------------------------:|:-------------------------:|
| **1** | 动态角色选择不智能      | decompose 的角色列表是硬编码的；LLM 选角无任务复杂度评估 | 简单任务也分配 8 个角色    |
| **2** | 运行不稳定             | 无检查点、无超时保护升级、decompose 单点失败          | 中断后从零开始             |
| **3** | 质量退化               | 无对抗评分闭环、无中间质量门控、merge 建立在残缺结果上 | 最终报告可能基于空洞子任务 |

### 1.3 业界解决方案调研汇总

| **方案**                              | **来源**                  | **核心思想**                          | **借鉴点**                      |
|:-------------------------------------:|:-------------------------:|:-------------------------------------:|:-------------------------------:|
| PARL 并行约束                          | Kimi K2.5                 | 完成率驱动降并行 + 防伪并行检测        | 已部分实现，需增强               |
| Dynamic Role Assignment (DRA)         | arXiv:2601.17152          | 按任务需求动态分配角色 + Meta评审      | 智能选角 + 执行后评审            |
| Agent Drift 防退化                    | arXiv:2601.04170          | 量化多agent长交互行为偏移             | 目标锚定 + 记忆巩固              |
| iMAD 按需辩论                          | arXiv:2511.11306 (AAAI26) | 仅高价值时触发辩论，大幅降 token        | 选择性质量门控                   |
| OpenAI Swarm Handoff                  | openai/swarm              | 最小原语: Agent + Handoff              | 显式控制权转移                   |
| Boids 涌现协作                         | OpenReview:46LJ81Yqm2    | 局部规则 → 涌现全局行为               | 分离/对齐/凝聚三规则             |
| CoVe (Chain of Verification)          | Meta, ACL 2024            | 草稿→验证问题→独立验证→修正           | 研究/辩论团队的验证环节           |
| DoVer 多agent调试                      | arXiv:2512.06749          | 假设-干预-验证 自动调试               | 失败子任务的诊断修复              |

---

## 二、蜂群优化方案设计

### 2.1 智能角色路由器 (Smart Role Router)

**问题**: 当前 decompose 提示词列出 20+ 角色供 LLM 选择，但 LLM 常过度分解。

**方案**: 在 decompose 前增加**任务复杂度评估 + 角色推荐**层。

```
┌─────────────────────────────────────────────────────────────┐
│                  Smart Role Router                           │
│                                                              │
│  1. analyzeComplexity(objective) → ComplexityProfile         │
│     - 代码密度 (是否涉及编码)                                 │
│     - 领域广度 (跨几个技术领域)                               │
│     - 深度需求 (调研/分析/实现)                               │
│                                                              │
│  2. recommendRoles(profile) → []RecommendedRole              │
│     - 根据复杂度自适应选择 2-6 个角色                         │
│     - 简单研究: researcher + synthesizer (2个)               │
│     - 中等开发: architect + coder + reviewer (3个)           │
│     - 复杂项目: architect + coder×2 + tester + reviewer (5个)│
│                                                              │
│  3. decompose 时仅提供推荐的角色子集                          │
└─────────────────────────────────────────────────────────────┘
```

**实现**: 在 `SwarmOrchestrator.decompose` 前加 `routeRoles(ctx, objective)` 调用。

### 2.2 检查点与恢复 (Checkpoint & Recovery)

**问题**: 蜂群绕过 Coordinator，无阶段检查点。

**方案**: 蜂群完成每一层后，将 `resultMap` + `plan` 持久化到 `team.stateDir`。

```go
type SwarmCheckpoint struct {
    Plan          *DecompositionPlan    `json:"plan"`
    CompletedIDs  []string              `json:"completedIds"`
    ResultMap     map[string]string     `json:"resultMap"`
    CurrentLevel  int                   `json:"currentLevel"`
    Timestamp     time.Time             `json:"timestamp"`
}
```

### 2.3 子任务质量门控 (Quality Gate)

**问题**: 子任务产出无评分，merge 建立在可能残缺的结果上。

**方案**: 借鉴 iMAD "按需辩论" + AdaptiveTerminator 思路，对关键子任务增加轻量评审。

```
┌──────────────────────────────────────────────────┐
│              Sub-Task Quality Gate                 │
│                                                    │
│  executeSubTask(t) → result                       │
│     ↓                                              │
│  qualityGate(result, t.Role, t.Priority)          │
│     ├─ priority=0 (普通): IsNonTrivialArtifact    │
│     ├─ priority=1 (高):   快速自评 (LLM 1次调用)   │
│     └─ priority=2 (关键): 完整多维评分 (EvalScore)  │
│                                                    │
│  if score < threshold && retries < 1:             │
│     → 重试 1 次, 注入失败原因作为提示               │
│  else:                                             │
│     → 标记为 degraded, merge 时降权处理            │
└──────────────────────────────────────────────────┘
```

### 2.4 增强的 Merge 策略 (Consensus Merge)

**问题**: 当前 merge 无法处理残缺/矛盾的子任务结果。

**方案**: merge 前进行子任务结果的可信度评估。

```go
type SubTaskScore struct {
    TaskID     string  
    Confidence float64   // 0-1: 输出质量置信度
    Degraded   bool      // 是否为降级结果
}
```

merge 时：
1. 对每个子任务结果计算 `confidence` (基于长度、结构、是否有实质内容)
2. `degraded` 结果在 merge 提示中标注 `⚠️ 低置信度`
3. 要求 LLM 在综合时优先引用高置信度结果

---

## 三、团队深度思考增强方案

### 3.1 Research 团队: 预规划 + 验证循环

**借鉴**: Claude Extended Thinking (先想后做) + CoVe (验证链)

```
当前流程:
  researchers(3并行) → synthesizer

优化后:
  ┌─ pre-think: 合成师先规划调研框架 (类似 extended thinking)
  │  输出: 调研框架 + 每位研究员的关键问题清单
  ├─ researchers(3并行): 基于清单执行, 更有针对性
  ├─ cross-verify: 三方结果交叉验证 (CoVe: 生成验证问题→独立回答→比对)
  └─ synthesize: 基于已验证结果综合
```

**实现**: 在 `researchWorkflow` 中:
- 在 3 个并行 researcher 前，增加 `research-planning` 阶段
- 在 synthesize 前，增加 `cross-verification` 阶段

### 3.2 Debate 团队: iMAD + CoVe + 动态角色

**借鉴**: 
- iMAD (仅在分歧大时触发深度辩论)
- CoVe (裁判验证双方论据)
- DRA (动态分配辩论强弱侧)

```
当前流程:
  proposer → opponent → (固定3轮) → judge

优化后:
  ┌─ pre-analysis: 分析命题，识别核心争议点 (类 DeepThink)
  ├─ round-1: proposer ↔ opponent
  ├─ divergence-check: 测量分歧度 (Boids MeasureDivergence)
  │   ├─ 分歧低 → 直接进入裁判 (iMAD: 按需辩论)
  │   └─ 分歧高 → 继续辩论 (最多 maxRounds)
  ├─ evidence-verify: 裁判独立验证双方引用的证据 (CoVe)
  └─ judgment: 基于验证后的证据做最终裁决
```

**实现**: 
- 修改 `executeAdversarial`，增加 `divergenceCheck` 逻辑
- 在 judge 阶段前增加 evidence-verify 步骤

### 3.3 TechBlog 团队: 迭代自审 + 深度思考

**借鉴**: Qwen3 thinking mode (先推理后输出) + self-critique

```
当前流程:
  source-analysis ∥ investigation → fact-check → write → format

优化后:
  source-analysis ∥ investigation → fact-check → write
    → self-critique (作者自审: 深度够不够? 有没有独到见解?)
    → revision (如果自审评分 < 7, 修改一轮)
    → format
```

**实现**: 在 `article-writing` 后增加自审+修改阶段

---

## 四、实现优先级

| **优先级** | **改进项**                           | **影响面** | **实现复杂度** |
|:----------:|:------------------------------------:|:----------:|:--------------:|
| **P0**     | 蜂群: 智能角色路由 (routeRoles)      | 高         | 中             |
| **P0**     | 蜂群: 子任务质量门控 (qualityGate)   | 高         | 中             |
| **P0**     | 蜂群: 增强 merge (confidence 加权)   | 高         | 低             |
| **P1**     | 蜂群: 检查点恢复                     | 中         | 中             |
| **P1**     | 研究团队: 预规划 + 交叉验证          | 高         | 中             |
| **P1**     | 辩论团队: iMAD 按需辩论              | 高         | 中             |
| **P2**     | TechBlog: 自审修改循环               | 中         | 低             |

---

## 五、指标衡量

### 5.1 蜂群效果对比指标

| **指标**               | **当前** (预期) | **优化后** (目标) |
|:----------------------:|:---------------:|:-----------------:|
| 子任务有效产出率        | ~60%            | ≥85%              |
| 平均角色使用数          | 6-8 个          | 2-5 个 (自适应)    |
| merge 报告质量          | 中 (含空洞子结果) | 高 (置信度加权)    |
| 失败恢复成功率          | 0%              | ≥80%              |
| 单次执行 LLM 调用数     | N+2             | N+3 (增加路由+门控) |

### 5.2 团队效果对比指标

| **团队**    | **指标**           | **当前** | **目标** |
|:-----------:|:------------------:|:--------:|:--------:|
| Research    | 交叉验证覆盖率      | 0%       | ≥80%     |
| Research    | 报告包含负面案例数   | 偶尔     | ≥2       |
| Debate      | 平均辩论轮数        | 固定 3   | 1-3 自适应|
| Debate      | 证据验证率          | 0%       | ≥50%     |
| TechBlog    | 文章自审通过率      | N/A      | ≥70%     |

---

## 参考文献

1. Kimi K2/K2.5: Agent Swarm + PARL (arXiv:2507.20534, 2602.02276)
2. Dynamic Role Assignment for MAD (arXiv:2601.17152)
3. Agent Drift (arXiv:2601.04170)
4. iMAD: Intelligent MAD (arXiv:2511.11306, AAAI 2026)
5. CoVe: Chain of Verification (Meta, ACL 2024)
6. DoVer: Multi-Agent Debugging (arXiv:2512.06749)
7. Debate or Vote (arXiv:2508.17536, NeurIPS Spotlight)
8. LLM-Flock: Decentralized Boids (arXiv:2505.06513)
9. OpenAI Swarm: Agent + Handoff (openai/swarm)
10. DRAMA: Dynamic Robust Allocation MAS (arXiv:2508.04332)
