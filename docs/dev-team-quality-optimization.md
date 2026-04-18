# 研发团队对抗循环质量优化方案

> **版本**: v1.0 | **日期**: 2026-04-05
> **问题**: 对抗循环 5 轮后质量下降, best-of-N 回滚到第 1 轮; 输出不可编译

---

## 1. 问题诊断

### 1.1 运行日志分析 (go-dev-2724)

| 轮次 | Correctness | Completeness | Security | CodeQuality | DesignAlign | 编译 | 决策 |
|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---|
| **1** | 7 | **3** | 6 | 8 | 5 | **失败** | 保留 (最高分) |
| **2** | **2** | **3** | 5 | 4 | 3 | **失败** | ⏪ revert→R1 |
| **3** | 4 | **3** | 5 | 6 | 4 | **失败** | ⏪ revert→R1 |
| **4** | 7 | **3** | 5 | 8 | 6 | **失败** | 保留 |
| **5** | 6 | **3** | 6 | 7 | 5 | **失败** | ⏪ best-of-N→R1 |

**关键发现**:
- **Completeness 全程固定=3**: 5 轮都只输出 config 模块, 核心模块 (checker/scheduler/output/main) **全部缺失**
- **编译全程失败**: 每轮都有新的编译错误, 从未产出可编译代码
- **Best-of-N 回滚到 R1**: R1 本身也不可编译, 回滚后等于"选了最不差的残品"

### 1.2 根因分析 (5 大核心问题)

```
┌──────────────────────────────────────────────────────────────────────┐
│                    质量下降的 5 大根因                                │
├──────────────────────────────────────────────────────────────────────┤
│                                                                      │
│  ① 单轮输出截断 ←── LLM 单次输出 token 上限不够覆盖完整多文件项目     │
│       │                                                              │
│       ▼                                                              │
│  ② 整体式代码生成 ←── 在一条消息里要求输出整个 repo (5+ 文件)        │
│       │                                                              │
│       ▼                                                              │
│  ③ 编译门禁无效 ←── 发现编译错误后只是追加到反馈, 不做强制重试        │
│       │                                                              │
│       ▼                                                              │
│  ④ 上下文膨胀 ←── 每轮累加前一轮 16K 输出 + reviewer 反馈           │
│       │            Round 3 时 prompt 已 >40K tokens                   │
│       ▼                                                              │
│  ⑤ Reviewer 反馈漂移 ←── 反馈互相矛盾 / 只见局部 / 要求超出容量     │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘
```

#### ① 单轮输出截断 (占影响 40%)
- LLM 的 max_output_tokens 有限 (通常 4K-8K), 要求在一条消息中输出 5+ 个完整 Go 文件是不可能的
- 每轮 Reviewer 都报告 "代码输出被截断, 仅看到 config 模块部分实现"
- **核心矛盾**: 设计要求 8 个模块, 但 LLM 只能输出 1-2 个模块

#### ② 整体式代码生成 (占影响 25%)
- 当前模式: Coder 收到完整设计 → 一次性输出全部代码 → Reviewer 审查
- 正确模式: 按文件/模块逐个生成 → 每个文件编译验证 → 再生成下一个

#### ③ 编译门禁无效 (占影响 15%)
- `runBuildGate` 发现编译错误后只是追加到 `lastEvalFeedback`, 不做强制重试
- Coder 下一轮收到 "编译错误 + reviewer 反馈", 但试图同时修复两者导致新错误
- 编译错误应该作为**硬阻断**, 不通过编译的轮次不应该进入 Reviewer 评审

#### ④ 上下文膨胀 (占影响 10%)
- `buildFeedbackSection`: 每轮注入前一轮 16K 输出 + reviewer 反馈
- Round 3 时 prompt 已 >40K tokens, LLM 注意力被分散
- 参考 Kimi K2 的溢出策略: 应该渐进式压缩, 而非全量注入

#### ⑤ Reviewer 反馈漂移 (占影响 10%)
- Round 2 Reviewer 要求 "使用标准库 string 处理"
- Round 3 Coder 引入 `strings.Clean()` (不存在的 API) 试图遵循反馈, 引入新编译错误
- Reviewer 每轮提出新要求 (SSRF, context 传播, worker pool, tests), 超出单轮改进容量

### 1.3 最终输出是否可运行?

**结论: 不可运行。**
- 5 轮编译全部失败, best-of-N 回滚到 R1 (也不可编译)
- 设计要求 8 个模块, 实际只输出 config 模块 (完整度 ~12%)
- JSON 序列化、并发调度、HTTP 检查等核心功能完全缺失
- 最终产物: 一个不完整的 config 包 + 大量注释填充

---

## 2. 业界调研: LLM 代码生成与迭代优化

### 2.1 论文与技术参考

| 论文/系统 | arXiv | 核心策略 | 可借鉴点 |
|:---|:---|:---|:---|
| **Self-Debugging** | 2304.05128 | 执行反馈+解释修复 | **编译结果驱动修复, 非纯文本审查** |
| **Self-Refine** | 2303.17651 | 单模型迭代反馈 | 修复增益在前 2 轮最大, 后续边际递减 |
| **Reflexion** | 2303.11366 | 言语反射+情景记忆 | **结构化失败记忆, 避免重复犯错** |
| **Accuracy-Correction Paradox** | 2601.00828 | 自我纠正局限性 | **更多批评文本不等于更好修复** |
| **Iterative Self-Repair** | 2604.10508 | 多模型多基准测试 | **前 2 轮修复最有效, 之后考虑重采样** |
| **MAgICoRe** | EMNLP 2025 | 外部 reward 驱动 | 自适应 + 阶梯策略转换 |
| **SWE-agent** | 2405.15793 | 计算机接口优化 | **减少自由格式, 提供结构化反馈** |
| **MapCoder** | 2405.11403 | 多角色分离 | **规划/编码/调试/测试 独立角色** |
| **Kimi K2.5** | 2602.02276 | Agent Swarm 并行 | **任务分解 + 并行子代理, 4.5× 加速** |
| **GLM-5** | 2602.15763 | DSA + 异步 Agent RL | **解耦生成与训练, 长时序学习** |
| **DeepSeek-R1** | 2501.12948 | GRPO RL on CoT | **RL 在可验证信号(测试)上最有效** |

### 2.2 业界核心共识

```
┌───────────────────────────────────────────────────────────┐
│              六大核心原则 (证据级别: 多论文交叉验证)         │
├───────────────────────────────────────────────────────────┤
│                                                           │
│  1. 文件级增量生成 > 整体式一次性生成                       │
│     (MapCoder + SWE-agent: 分解=成功)                     │
│                                                           │
│  2. 编译/测试验证 > 纯文本 Reviewer 反馈                   │
│     (Self-Debugging: 执行反馈最可靠)                       │
│                                                           │
│  3. 前 2 轮修复最有效, 之后重采样优于继续修补               │
│     (2604.10508: "repair vs resample" 取舍)               │
│                                                           │
│  4. 上下文必须受控压缩, 全量注入有害                        │
│     (Kimi K2 溢出策略 + SWE-agent ACI)                    │
│                                                           │
│  5. 结构化记忆 > 全量历史                                  │
│     (Reflexion: 失败摘要+关键修复项)                       │
│                                                           │
│  6. 确定性门禁 > LLM 评审                                  │
│     (go build/go test 的判断 100% 可靠)                    │
│                                                           │
└───────────────────────────────────────────────────────────┘
```

---

## 3. 设计方案: 六层质量保障体系

### 3.1 架构总览

```
                    ┌──────────────────────┐
                    │   Planner WBS 任务图  │
                    │  (task1→task2→task3)  │
                    └──────────┬───────────┘
                               │ 逐任务分发
                    ┌──────────▼───────────┐
        ┌───────────│  L1: 文件级增量生成    │───────────┐
        │           │  (单文件 → 编译 → 下一个)│           │
        │           └──────────┬───────────┘           │
        │                      │ 每文件                  │
        │           ┌──────────▼───────────┐           │
        │           │  L2: 编译硬门禁       │           │
        │           │  (go build 不过=重试)  │           │
        │           └──────────┬───────────┘           │
        │                      │ 编译通过                │
        │           ┌──────────▼───────────┐           │
        │           │  L3: 微测试门禁       │           │
        │           │  (go test 关键路径)    │           │
        │           └──────────┬───────────┘           │
        │                      │ 测试通过                │
        │           ┌──────────▼───────────┐           │
        │           │  L4: 智能 Reviewer    │           │
  失败回退 ◄─────────│  (聚焦增量 diff 审查)  │           │
        │           └──────────┬───────────┘           │
        │                      │ 审查通过                │
        │           ┌──────────▼───────────┐           │
        │           │  L5: 上下文压缩器     │           │
        │           │  (渐进摘要+记忆链)    │           │
        │           └──────────┬───────────┘           │
        │                      │                        │
        │           ┌──────────▼───────────┐           │
        └──────────►│  L6: 智能终止决策     │───────────┘
                    │  (重采样 vs 修补)     │
                    └──────────────────────┘
```

### 3.2 L1: 文件级增量生成 (核心改进)

**现状**: Coder 一次输出整个项目 → 输出截断 → Completeness=3

**方案**: 按 Planner WBS 任务逐文件生成, 每个文件独立编译验证

```
Before (失败模式):
  Coder → [config.go + checker.go + scheduler.go + ...] → 截断 → 只有 config.go

After (增量模式):
  Task1: Coder → config.go → go build ✅ → commit
  Task2: Coder → checker.go → go build ✅ → commit  
  Task3: Coder → scheduler.go → go build ✅ → commit
  ...
  TaskN: Coder → main.go → go build ✅ → go test ✅ → commit
```

**实现策略**:
- 改造 `runAdversarialLoop`, 当 Planner 输出了 WBS 任务列表时, 走 `Orchestrator` 路径 (DAG 逐任务执行)
- 每个任务的 Coder prompt 只要求输出 **1 个文件** + 对应的测试文件
- Prompt 模板: "请**仅**实现 `{file_path}`, 其他文件不要输出。文件的接口定义: `{interface_spec}`"

### 3.3 L2: 编译硬门禁 (编译不过 = 不进 Reviewer)

**现状**: `runBuildGate` 发现编译错误后只追加到反馈, 继续进 Reviewer

**方案**: 编译失败 → 强制 Coder 内部重试 (最多 2 次), 不消耗对抗轮次

```go
// 伪码: 编译硬门禁
for buildRetry := 0; buildRetry < maxBuildRetries; buildRetry++ {
    output := coder.Generate(ctx, fileTask)
    buildErr := runBuildCheck(cwd)
    if buildErr == "" {
        break // 编译通过, 继续进 Reviewer
    }
    // 编译失败: 仅反馈编译错误, 不累加 reviewer 反馈
    coder.FixBuild(ctx, buildErr) 
}
// 编译仍失败 → 标记该文件为 failed, 跳过 Reviewer
```

**关键设计**: 编译修复循环独立于对抗循环, 不消耗 `AdaptiveTerminator` 的轮次配额。

### 3.4 L3: 微测试门禁

**现状**: 测试阶段在 Phase 3 (E2E), 距离代码生成太远, 反馈滞后

**方案**: 每个文件生成后, 立即运行该文件的单元测试

- 如果 Planner 的任务包含 `acceptance` 条件 (如 "go test -run TestConfig 通过"), 在文件生成后立即验证
- 测试失败 → 当场修复, 不等到 Phase 3

### 3.5 L4: 智能 Reviewer (聚焦增量 diff)

**现状**: Reviewer 审查 coder 的全部输出 (24K 截断) → 看到截断后给 Completeness=3

**方案**: 文件级增量模式下, Reviewer 只审查**本文件的 diff + 设计对齐检查**

- Reviewer prompt 改为: "审查以下**单个文件**的实现, 对照设计文档中的接口定义"
- 减少 Reviewer 信息量: 从 24K → ~4K (单文件)
- Reviewer 评分只反映**该文件**的质量, 不因为"其他文件缺失"而扣 Completeness

### 3.6 L5: 上下文压缩器

**现状**: `buildFeedbackSection` 注入前一轮 16K 输出 + reviewer 反馈

**方案**: 渐进式压缩 + 结构化记忆

```
Round 1 → Coder: [设计文档(6K)] + [任务描述]
Round 2 → Coder: [设计文档(摘要2K)] + [R1关键问题(500字)] + [任务描述]  
Round 3 → Coder: [设计文档(摘要1K)] + [R1-R2记忆链(300字)] + [任务描述]
```

利用已有的 `IterationMemory` + `FormatMemoryChain` + `SummarizeOldOutput`, 但需要:
- 每轮自动提取 `KeyIssues` 并存入 `IterationMemory`
- 设计文档渐进摘要: Round 1 全量 → Round 2 接口签名+约束 → Round 3 仅约束列表

### 3.7 L6: 智能终止 (重采样 vs 继续修补)

**现状**: 对抗 5 轮后 best-of-N 回滚到 R1, 但 R1 本身也不可编译

**方案**: 基于 2604.10508 的 "repair vs resample" 决策

```
if round >= 2 && !buildPassed && bestScore.Completeness < 5:
    # 完整度太低 = 截断/方向错误 → 重采样 (换思路)
    strategy = "resample"  
    # 不是在已有代码上修补, 而是用新 prompt 重新生成
    
if round >= 2 && buildPassed && score.Correctness < 6:
    # 能编译但逻辑错 → 修补
    strategy = "repair"
    
if round >= 3 && score stagnant (delta < 0.3):
    # 已饱和 → 停止, 使用 best-of-N
    strategy = "terminate"
```

---

## 4. 实现计划

### 4.1 改动文件清单

| 文件 | 改动 | 优先级 |
|:---|:---|:---:|
| `pkg/agent/workflow.go` | L1: 增量模式分发; L2: 编译硬门禁; L5: 上下文压缩 | P0 |
| `pkg/agent/adversarial.go` | L5: IterationMemory 自动管理; L6: resample 决策 | P0 |
| `pkg/agent/orchestrator.go` | L3: 微测试门禁集成 | P1 |
| `pkg/agent/workflow.go` (prompts) | L4: Reviewer prompt 改为单文件审查 | P1 |

### 4.2 具体改动

#### 改动 1: 编译硬门禁 (L2) — `runAdversarialLoop` 中

在 Generator 执行后、Evaluator 审查前, 插入编译修复内循环:
- 最多 2 次内部重试
- 仅传递编译错误, 不传递 reviewer 反馈
- 编译通过后才进入 Evaluator

#### 改动 2: 上下文压缩 (L5) — `buildFeedbackSection` 改造

- Round 1: 全量设计文档 + 全量 plan
- Round 2+: `SummarizeOldOutput(design, 3000)` + `FormatMemoryChain(memories)` + 本轮任务
- 前一轮代码输出: 只保留文件清单 + 关键修改点, 不注入全文

#### 改动 3: 重采样决策 (L6) — `AdaptiveTerminator` 扩展

新增 `ShouldResample` 方法:
- 当 Completeness < 5 且编译失败 → 返回 `resample=true`
- 重采样: 清空 `lastGenOutput`, 用简化 prompt 重新生成 (不携带前一轮输出)

#### 改动 4: IterationMemory 自动化 (L5) — `runEvaluatorRound` 中

每轮 Evaluator 完成后:
1. 提取 `KeyIssues` 存入 `IterationMemory`
2. 记录 `Approach` (从 coder 输出中提取文件清单)
3. 下一轮 coder prompt 注入 `FormatMemoryChain`

#### 改动 5: 设计摘要渐进压缩 (L5) — 新增 `compressDesignForRound`

```
Round 1: 全量设计文档
Round 2: 仅接口签名 + 约束清单 + 文件结构
Round 3+: 仅约束清单 + 关键接口签名
```

---

## 5. 预期效果

| 指标 | 改进前 | 改进后 (预期) | 依据 |
|:---|:---:|:---:|:---|
| **编译通过率** | 0/5 (0%) | ≥3/5 (60%) | L2 编译硬门禁 |
| **Completeness** | 3/10 (不变) | ≥6/10 | L1 文件级增量避免截断 |
| **对抗轮次效率** | 5 轮无改善 | 2-3 轮收敛 | L6 智能终止+重采样 |
| **上下文利用率** | Round 3 时 >40K 膨胀 | 控制在 15K 以内 | L5 渐进压缩 |
| **最终可运行性** | ✘ 不可编译 | ✓ go build 通过 | L2+L3 双门禁 |

---

## 6. 参考文献

1. Chen et al. "Teaching Large Language Models to Self-Debug" (arXiv:2304.05128, ICLR 2024)
2. Madaan et al. "Self-Refine: Iterative Refinement with Self-Feedback" (arXiv:2303.17651)
3. Shinn et al. "Reflexion: Language Agents with Verbal Reinforcement Learning" (arXiv:2303.11366)
4. Yang et al. "Decomposing LLM Self-Correction" (arXiv:2601.00828)
5. Du et al. "How Many Tries Does It Take?" (arXiv:2604.10508)
6. Yang et al. "SWE-agent" (arXiv:2405.15793)
7. Islam et al. "MapCoder" (arXiv:2405.11403)
8. Kimi K2.5 "Agent Swarm" (arXiv:2602.02276)
9. GLM-5 "Agentic Engineering" (arXiv:2602.15763)
10. DeepSeek-R1 "RL on Reasoning" (arXiv:2501.12948)
