# 前沿模型精华吸收设计方案

> 基于 GLM 5.1, Kimi K2.5, MiniMax M2.7, DeepSeek Prover-V2, MAgICoRe, CaRT 的论文研究

## 一、精准复刻偏差分析

### 当前实现 vs 论文核心机制的偏差

| 机制 | 论文原意 | 当前实现 | 偏差 | 修正方案 |
|:---|:---|:---|:---|:---|
| **IterationMemory** | MiniMax: 结构化日志 {方法, 失败原因, 指标变化} | `Approach` 字段从未赋值 | 记忆链缺少方法摘要 | coder 输出后用 LLM 提取 approach 摘要 |
| **Keep/Revert** | MiniMax: ΔMetric ≥ τ ∧ 无回归 → accept | 仅比较 AvgScore vs BestScore | 缺少多维度回归检测 | 逐维度检测: 任一维度下降>1.0 即 revert |
| **策略转换** | GLM 5.1: benchmark 识别瓶颈类型, 针对性转换策略 | 通用 "换思路" prompt | 缺少瓶颈类型到策略的映射 | 根据 Bottleneck.Type 生成针对性转换 prompt |
| **蜂群并行度** | Kimi PARL: R=α·r_para + β·r_finish + γ·r_task | 仅用完成率 (completed/total) | 缺少有效输出验证 | 加入 artifact 有效性检查 (非空+长度+关键词) |
| **lastScore 塌缩** | MAgICoRe: 多维度评分驱动迭代 | workflow.go 把多维分数塌缩为单一 avg | 丢失维度信息 | 保留完整 EvalScore 跨轮传递 |
| **研究员 prompt** | Kimi K2: 假设→证据→验证 循环 | 中英文不一致, 缺少验证步骤 | 研究深度不足 | 引入假设驱动的研究 prompt |
| **进化系统** | MiniMax: 失败轨迹挖掘+反例入库 | 仅 failure 触发学习, 成功不提炼 | 正样本浪费 | 成功也提炼 what-worked 经验 |
| **Dreaming** | 睡眠时重放+重要性加权 Q/A | 简单去重拼接 | 记忆整合质量低 | 引入向量检索+重要性加权合成 |

## 二、Kimi 2.5 深度思考改进方案

### 核心理念
Kimi K2 Thinking 的核心: **假设生成 → 证据搜集 → 验证** 的深度循环。

### 改进 researcher 和 design 阶段

1. **假设驱动研究 (Hypothesis-Driven Research)**
   - researcher 先生成 3-5 个技术假设
   - 逐个搜集证据 (WebSearch / 代码分析)
   - 交叉验证: 寻找反例
   - 输出: 假设表 + 证据链 + 置信度

2. **多视角设计 (Multi-Perspective Design)**
   - architect 生成 2-3 个方案
   - 每个方案附: 优势/劣势/风险/复杂度
   - planner 选最优方案并标注决策理由

3. **Heavy Mode 聚合**
   - 关键决策节点: 并行 3 路 rollout
   - meta-judge 选最优 (非简单投票, 而是理由论证)

## 三、自动进化系统改进方案

### 当前问题
1. 成功阶段不触发精炼 → 正样本浪费
2. BM25 检索 → 语义泛化弱
3. 无反事实对比 → 无法学习 "哪个决策更好"
4. Consolidate 是 O(n²) Jaccard → 规模受限

### 改进设计 (参考 MiniMax + GLM 5.1)

1. **双向学习**: 成功+失败都提炼
   - 成功: {what-worked, 方法摘要, 关键决策}
   - 失败: {root-cause, 错误模式, 修复方向}

2. **结构化经验**: 不再是自由文本
   ```json
   {
     "pattern": "error-handling",
     "context": "Go HTTP handler",
     "strategy": "wrap errors with fmt.Errorf + %w",
     "evidence": "3 successes, 0 failures",
     "contraindication": "performance-critical paths"
   }
   ```

3. **经验效果追踪**: 注入经验后 A/B 对比
   - 记录注入了哪些经验
   - 对比注入 vs 未注入的成功率
   - 自动调整经验的 Quality 权重

4. **失败轨迹反例库**: MiniMax 风格
   - 失败时存储 {症状, 根因, 补丁, 指标变化}
   - 匹配到相似症状时主动注入修复方向

## 四、提示词缓存 + 长时间编程效率优化

### Prompt 缓存设计
1. **静态前缀最大化**: system prompt + tool defs + repo map + style guide → 固定前缀
2. **缓存键**: SHA256(static_prefix) → 复用率追踪
3. **布局规则**: 不变内容在前, 可变内容在后
4. **指标**: cache_hit_rate, cached_tokens, cost_savings

### 长会话效率优化
1. **渐进式摘要**: 旧 tool 输出 → 结构化摘要 (保留文件名+行号+结论)
2. **滑动窗口**: 最近 N 轮完整, 更早的压缩
3. **工作集选择**: 只保留当前 DAG 路径相关的上下文
4. **检查点恢复**: 可从任意检查点恢复, 不需重放全部历史

## 五、蜂群模式对标 Kimi 未实现精髓

### 已实现
- LLM 动态分解 → 拓扑排序 → 逐层并行 ✅
- 完成率评估 → 动态并行度 ✅
- Evolution 集成 ✅

### 未实现 (需复刻)
1. **防伪并行检查**: 每个 worker 产出必须是非平凡 artifact
2. **并行 rollout + meta-judge 聚合**: 重要决策多路执行, 选最优
3. **跨 worker 共识**: 冲突结果合并前先投票/裁决
4. **渐进式任务缩减**: 前层完成后动态调整后层任务 (不只是并行度)
5. **worker 间依赖结果验证**: 下游 worker 校验上游产出可用性

## 六、上下文退化 + 记忆召回改进 Dreaming

### 当前问题
1. localConsolidate 是简单去重拼接
2. 重要性不影响合并策略
3. 无向量检索
4. 无记忆衰减机制 (由 tiered_memory 处理, 但 dreaming 不参与)

### 改进设计
1. **睡眠时重放**: 高重要性记忆生成 Q/A 对, 存入向量索引
2. **重要性加权合并**: 重要记忆保留细节, 低重要性记忆压缩
3. **矛盾检测**: 新旧记忆冲突时 trust git/tests > prose
4. **记忆分层**: working (当前任务) / episodic (近期会话) / semantic (长期模式)
5. **衰减+强化**: 被召回的记忆强化权重, 未用的自然衰减

## 七、实施优先级

| 优先级 | 改进项 | 影响模块 | 预期效果 |
|:---|:---|:---|:---|
| **P0** | lastScore 多维度保留 | workflow.go | 修复评分塌缩, 提升对抗质量 |
| **P0** | IterationMemory.Approach 赋值 | orchestrator.go | 消除重复犯错 |
| **P0** | 进化双向学习 | evolution.go | 正样本利用率 +100% |
| **P1** | 假设驱动研究 prompt | roles.go, workflow.go | 研究深度 +50% |
| **P1** | 蜂群防伪并行+共识 | swarm.go | 有效产出率 +30% |
| **P1** | Prompt 缓存机制 | workflow.go, orchestrator.go | 成本 -25~30% |
| **P2** | Dreaming 睡眠重放 | dreamer.go | 记忆召回质量 +40% |
| **P2** | 策略转换瓶颈映射 | adversarial.go, orchestrator.go | 局部最优逃逸率 +20% |
| **P2** | 多维度回归检测 | adversarial.go | 避免单维度退化被掩盖 |
