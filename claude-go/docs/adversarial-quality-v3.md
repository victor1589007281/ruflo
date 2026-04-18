# 对抗循环质量优化 V3 方案

> 基于 go-dev-2450 (C++) + go-dev-2578 (Rust) 实时运行分析
> 结合 Claude 4.7, DeepSeek V4, GPT 5.6, Qwen 3.6 等大模型调研
> 日期: 2026-04-18

## 1. 问题诊断 (从运行日志中提取)

### 1.1 准确-纠正悖论 (核心问题)

```
C++ go-dev-2450:
  初始化 CMake: R1=8/6/6/7 → R2=5/5/7/7↓ → R3=5/6/5/5↓ → R4=5/6/5/5↓ → R5=6/5/6/7↓ → 回滚R1
  并发原语:     R1=3/4/5/5 → R2=3/4/5/5= → R3=3/4/5/5= → R4=3/4/5/5= → 饱和停止

Rust go-dev-2578:
  项目初始化:   R1=8/6/6/7 → R2=7/6/8/6↓ → R3=5/6/6/7↓ → R4=4/6/7/4↓ → R5=6/8/7/7✅
  KV DashMap:   R1=6/5/8/8 → R2=6/8/6/6↓ → R3=4/5/6/6↓ → R4=6/5/6/8↓ → R5=4/5/6/7↓ → 回滚R1
```

**规律**: 60%以上任务在 R2-R4 退化后回滚到 R1, 浪费 3-4 轮 LLM 调用。

**业界论文对应**:
- arXiv:2601.00828 "Accuracy-Correction Paradox": 自修复不一定提升, 强模型修改好代码反而引入错误
- Chen et al. "Self-Debugging" ICLR 2024: 无执行反馈的自修复 ≈ 随机; 有执行反馈才有效
- "Reflexion" arXiv:2303.11366: 仅靠 verbal reflection 的修复成功率 ~30-50%

### 1.2 Reviewer 反馈引导偏移

Reviewer 给出的反馈在多轮后引导 coder 偏离原始好方案:
- R1 得分 8/6/6/7 (好), Reviewer 提出 5 条改进
- Coder 尝试修复 → 引入新问题 → R2 得分 5/5/7/7 (退化)
- 越改越偏, "反馈漂移" 问题

**业界方案**:
- GPT 5.6 Codex: 操作Agent与研究Agent分离, 审查结果只标记 MUST-FIX vs NICE-TO-HAVE
- Qwen 3.6 Coder-Next: 分层反馈, 编译错误优先于逻辑问题
- Claude 4.7 Opus: "Tool-use verification" — 工具执行结果>语言审查

### 1.3 对抗轮次效率低

| 达标轮次分布 | C++ (10完成) | Rust (11完成) |
|:---|:---|:---|
| R1 达标 | 1 (10%) | 0 (0%) |
| R2 达标 | 1 (10%) | 3 (27%) |
| R4-R5 达标 | 0 | 2 (18%) |
| 未达标 (跑满5轮) | 8 (80%) | 6 (55%) |

80% C++ 任务 和 55% Rust 任务跑满5轮也未达标, 每个浪费 15-25min。

### 1.4 Micro-test 瓶颈分类器误报

大量 "编译/语法错误" 瓶颈在编译已通过的情况下被报出:
```
🟢 第 1 轮编译通过  ← 编译已通过
🔴 重复瓶颈: 编译/语法错误 (连续 2 轮)  ← 但瓶颈分类器仍报编译错误
```
原因: micro-test 由 LLM 审查, 可能将代码风格问题误分类为编译错误。

## 2. 优化方案

### 2.1 L7: "R1保底+差分修复" 策略 (解决准确-纠正悖论)

**核心思想**: 参考 DeepSeek V4 的 FIM 和 Agentless 的 "localize→repair→validate"

**不再让 Coder 基于完整上轮输出 + Reviewer 反馈重写**,
而是:
1. **R1 输出保底**, 存储为 `baseline`
2. **R2+ 只做差分修复**: Coder 收到的不是"全量重写", 而是:
   - `baseline` 代码 (不可修改)
   - Reviewer 标记的 **最多 3 个 MUST-FIX** 问题 (精确到函数级别)
   - Coder **仅修改**被标记的函数, 其余保持不变
3. **评审比较**: 修复后与 `baseline` 比较, 只有整体提升才接受

**Reviewer 反馈格式限制**:
```json
{
  "must_fix": [
    {"file": "kv_store.cpp", "function": "Get()", "issue": "并发不安全", "severity": "blocking"}
  ],
  "nice_to_have": [...],
  "do_not_change": ["整体架构", "数据结构定义"]
}
```

### 2.2 L8: 智能早期终止 (解决 80% 跑满轮次问题)

**当前**: 连续退化 → 触发策略转换 → 继续跑, 最终回滚 R1

**改进**: 
- **R1 得分 ≥ 7/6/6/6 → 直接终止** (已经是"可接受质量")
- **连续 2 轮退化 → 立即终止** (不再浪费 R3-R5)
- **饱和计数 ≥ 1 → 终止** (策略转换后1轮无改善直接停)

**预计节省**: 每个任务减少 2-3 轮 → 节省 8-15min → 整体缩短 40-60%

### 2.3 L9: 按编译结果覆盖 Micro-test 瓶颈 (解决误报)

如果 `runBuildCheckLang()` 返回空(编译通过), 则:
- 覆盖 micro-test 中的 "编译/语法错误" 瓶颈为 "已通过编译"
- 避免瓶颈累积影响 AdaptiveTerminator 决策

### 2.4 L10: Reviewer MUST-FIX 上限 (解决反馈漂移)

当前 Reviewer 反馈"最多5条", 但实际常超过导致 Coder 分心。

**改进**:
- R2: MUST-FIX 最多 **3 条**
- R3: MUST-FIX 最多 **2 条** (只聚焦最关键)
- R4+: MUST-FIX 最多 **1 条**

### 2.5 L11: 可接受质量阈值降低

当前: ALL dimensions ≥ 6 才算达标。

**改进**: 采用加权总分:
- `total = C*0.3 + Co*0.25 + S*0.2 + Q*0.25`
- `total ≥ 6.0` 即达标 (允许个别维度5分但总体合格)

### 2.6 L12: 增强文件物化引擎 (解决代码不落盘)

**问题**: C++ 团队 18 个任务只物化了 1 个文件, Rust 团队 0 个文件。代码全在 REPORT.md 里。

**根因**: `MaterializeCode` 只匹配严格格式 `` ```lang // File: path/to/file.ext ```, 但 LLM 实际输出多种格式:
- `` ```cpp\npath/to/file.cpp ``
- `` ### file.cpp\n```cpp ``
- 纯文本 `// filename: xxx.cpp` + 代码
- 嵌在 markdown 段落中的代码块

**改进**: 增加多模式文件名提取:
1. `// File: path` 或 `// filename: path` (当前)
2. `` ```lang:path `` (新增)
3. `` ```lang\n// path/to/file.ext `` (新增)
4. markdown header `### path/to/file.ext` 后紧跟代码块 (新增)
5. Coder prompt 中强制要求使用统一标记格式

## 3. 实施计划

| 优先级 | 改动 | 文件 | 影响 |
|:---|:---|:---|:---|
| P0 | L8 智能早期终止 | orchestrator.go | 减少60%无效轮次 |
| P0 | L7 R1保底+差分修复 | orchestrator.go | 解决准确-纠正悖论 |
| P1 | L9 编译覆盖瓶颈 | orchestrator.go | 修正误报 |
| P1 | L10 MUST-FIX上限 | orchestrator.go | 减少反馈漂移 |
| P2 | L11 加权总分 | adversarial.go | 提高达标率 |
