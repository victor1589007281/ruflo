# Kimi 2.5 深度思考能力研究与改进方案

> 基于 Kimi K2.5 (arxiv:2602.02276) 和 Kimi K2 Thinking 的技术分析

## 一、Kimi 深度思考核心机制

### 1. 假设-证据-验证循环 (HEV Loop)
Kimi K2 Thinking 的研究模式:
```
Hypothesis Generation → Evidence Gathering → Verification → Refinement
    ↑                                                          |
    └──────────────────────────────────────────────────────────┘
```
- **假设生成**: 提出 3-5 个可能的技术方向
- **证据搜集**: 每个假设独立搜集证据 (搜索/代码分析/文档)
- **验证**: 交叉验证, 主动寻找反例
- **精炼**: 根据证据修正假设, 收敛到最优方案

### 2. Heavy Mode (多路 Rollout + 反思聚合)
- **并行 8 路 rollout**: 同一问题, 不同思考路径
- **反思聚合**: 不是简单投票, 而是 meta-judge 论证选优
- **适用**: 关键架构决策, 复杂 bug 根因分析

### 3. 长 Tool Chain 深度
- 单次任务最多 200-300 tool 调用
- **think → tool → think** 交错循环
- 上下文溢出时: 隐藏旧 tool 输出, 保留结论

### 4. QAT (量化感知训练)
- INT4 MoE 权重量化
- 长思考 session 更快更便宜
- 质量不坍塌

## 二、当前 claude-go 调研/设计阶段的差距

### 研究团队 (research workflow)
| 方面 | Kimi K2 | 当前实现 | 差距 |
|:---|:---|:---|:---|
| 研究方法 | 假设→证据→验证 循环 | "深入调研…给出方案" 单次 prompt | 缺乏系统性 |
| 反例搜集 | 主动寻找反证 | "包含负面案例" 一句提示 | 流于形式 |
| 多视角 | 多路 rollout + 聚合 | 单路执行 | 视角单一 |
| 量化论证 | 数据支撑每个结论 | "引用具体数据" 提示 | 执行力弱 |

### 研发团队 (development workflow)
| 方面 | Kimi K2 | 当前实现 | 差距 |
|:---|:---|:---|:---|
| 设计阶段 | 多方案对比 + 决策论证 | 单方案输出 | 缺少替代方案 |
| 计划阶段 | 子目标+验证器 | WBS 表格 | 缺少可验证性 |
| 实现阶段 | 增量实验+指标驱动 | 一次性生成 | 缺少实验反馈 |

## 三、改进方案: researcher 深度思考

### 3.1 假设驱动研究 Prompt (HEV Loop)

```
你是深度技术调研专家。使用 **假设→证据→验证** 方法论:

## Phase 1: 假设生成
生成 3-5 个技术假设 (每个假设包含):
- 假设内容 (一句话)
- 预期效果 (量化指标)
- 关键风险

## Phase 2: 证据搜集
对每个假设:
- 搜集支持证据 (文档/代码/案例)
- **主动搜集反例** (失败案例/局限性/替代方案)
- 标注证据强度: strong/moderate/weak

## Phase 3: 验证与收敛
- 交叉对比: 假设间是否矛盾
- 证据权重: strong>moderate>weak, 反例权重 ×1.5
- 输出: 每个假设的置信度 (0-100%), 推荐排序

## 输出格式
| 假设 | 支持证据 | 反例 | 置信度 | 推荐 |
```

### 3.2 architect 多方案对比 Prompt

```
你是系统架构师。对设计决策使用 **多方案对比** 方法:

为每个关键决策:
1. 生成 2-3 个可行方案
2. 每个方案评估:
   - 复杂度 (1-10)
   - 可维护性 (1-10)
   - 性能影响
   - 风险点
3. 明确推荐方案并论证选择理由
4. 记录被否决方案的否决原因 (防止后续重复探索)
```

### 3.3 planner 子目标验证器 Prompt

WBS JSON 中每个 task 增加 subGoals + verifier:
```json
{
  "id": 1,
  "title": "实现用户认证",
  "subGoals": [
    {"description": "接口定义完整", "verifier": "go build ./..."},
    {"description": "单元测试通过", "verifier": "go test -run TestAuth"},
    {"description": "错误处理完备", "verifier": "grep -r 'fmt.Errorf' auth/"}
  ]
}
```

## 四、改进方案: 研发团队深度思考

### 4.1 实验→分析→优化 闭环 (参考 GLM 5.1)

在 adversarial_dev 的 implement 阶段:
1. **实验**: coder 实现 + micro-test
2. **分析**: reviewer 评分 + 瓶颈分类
3. **优化**: 根据瓶颈类型选择策略
   - compilation → 修复语法/import
   - design_drift → 重新对齐设计文档
   - logic → 增加测试用例驱动
   - constraint → 检查约束清单

### 4.2 渐进式验证 (参考 DeepSeek Prover-V2)

每个 task 的 subGoals 逐个验证:
```
for each subGoal:
  if subGoal.verifier exists:
    result = execute(subGoal.verifier)
    subGoal.passed = result.success
  if !subGoal.passed:
    feedback += "子目标未达成: " + subGoal.description
    → coder 仅修复该子目标 (不重写全部)
```

## 五、实施清单

- [ ] 重写 researcher 角色 prompt → 假设驱动
- [ ] 重写 research workflow stages prompt → HEV 循环
- [ ] architect prompt 加入多方案对比
- [ ] planner prompt 加入子目标+验证器
- [ ] orchestrator 策略转换按瓶颈类型映射
- [ ] implement 阶段加入实验反馈循环
