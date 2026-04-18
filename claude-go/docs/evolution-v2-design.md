# Claude-Go 进化系统 V2 设计方案

> 基于业界调研 + 源码分析 + 用户需求的全面升级

## 一、业界调研总结

| 方案 | 核心机制 | 关键指标 | 适用场景 |
|:---|:---|:---|:---|
| **EvolveR** (2025) | 离线自蒸馏 + 在线检索注入 → 战略原则库 | 多跳QA/Agent评测 | 离线批量提炼 |
| **ExpeL** (AAAI 2024) | 轨迹→洞察(ADD/UPVOTE/DOWNVOTE/EDIT)→相似检索 | HotpotQA/ALFWorld/WebShop | 结构化洞察管理 |
| **Voyager** (NeurIPS 2023) | 自动课程+可执行技能库+环境反馈 | 探索覆盖/物品多样性 | 技能库积累 |
| **Reflexion** (NeurIPS 2023) | 语言RL: 标量/语言反馈→自省文本→情景记忆 | HumanEval coding | 自反思改进 |
| **Live-Evo** (2026) | ExperienceBank+MetaGuidelineBank+动态权重 | 实时预测基准 | 在线持续进化 |
| **Self-Refine** (NeurIPS 2023) | FEEDBACK→REFINE循环, 无需训练 | 人类偏好+自动指标 | 轻量级迭代 |
| **LATS** (ICML 2024) | MCTS+LM值函数/反思 | HumanEval/WebShop | 规划型任务 |
| **UCB/Bandit选择** | 探索/利用平衡, 臂=经验簇 | 遗憾界/在线成功率 | 经验选择策略 |
| **MinHash+LSH** | 近似Jaccard, 亚线性复杂度 | 精度/召回/假阳性率 | 大规模去重 |

### 关键启发

1. **双银行架构** (Live-Evo): ExperienceBank (原始轨迹) + GuidelineBank (提炼规则)
2. **结构化经验** (ExpeL): 区分 Pattern/Strategy/Antidote/Evidence
3. **动态权重+衰减** (Live-Evo): 有效经验增强, 误导经验淘汰
4. **UCB选择** (Bandit): 替代 top-K, 平衡探索与利用
5. **注入效果追踪** (A/B实验): 量化经验注入的实际效果
6. **MinHash去重** (LSH): O(n) 替代 O(n²) Jaccard
7. **生命周期状态机** (Voyager): proposed→validated→promoted→active→decaying→archived

---

## 二、6个核心问题修复方案

### P0 — 立即修复

| 问题 | 根因 | 修复方案 |
|:---|:---|:---|
| **P1: 单向学习** | workflow.go 仅在 `TaskFailed` 时调 `LearnFromStage` | 在 workflow 成功路径也调用 `LearnFromStage`, 已有双向支持 |
| **P2: 无效果验证** | 注入后不追踪结果, 无法计算 Uplift | 新增 `InjectionTracker`, 记录 injectionID→outcome |

### P1 — 短期修复

| 问题 | 根因 | 修复方案 |
|:---|:---|:---|
| **P3: 经验结构松散** | 纯文本 content, 无法区分类型 | 新增 `Pattern` 字段: strategy/pattern/antidote/evidence |
| **P4: 检索语义缺失** | 纯 BM25 字面匹配 | BM25 + 同义词扩展 + 标签匹配增强 |

### P2 — 中期修复

| 问题 | 根因 | 修复方案 |
|:---|:---|:---|
| **P5: O(n²) Consolidate** | 两两 Jaccard 比较 | MinHash 签名 + LSH 桶, 仅桶内比较 |
| **P6: 无反事实学习** | 不探索替代方案 | 失败时生成 "如果...会更好" 假设, 存为 strategy 经验 |

---

## 三、18项进化效果衡量指标

### 维度一: 学习质量 (Learn)

| # | 指标名 | 定义 | 目标值 | 计算方式 |
|:--|:---|:---|:---|:---|
| 1 | `evo_distill_rate` | 每条轨迹产出的经验数 | ≥0.3 | 新经验数 / 轨迹总数 |
| 2 | `evo_survival_rate` | 30天后仍保留的经验占比 | ≥40% | 存活经验 / 历史提炼总数 |
| 3 | `evo_pattern_diversity` | 不同 Pattern 类型的数量 | ≥15 | COUNT(DISTINCT pattern) |

### 维度二: 检索效能 (Retrieve)

| # | 指标名 | 定义 | 目标值 | 计算方式 |
|:--|:---|:---|:---|:---|
| 4 | `evo_retrieval_hit_rate` | 注入后任务成功的占比 | ≥75% | 成功任务 / 有注入的任务 |
| 5 | `evo_retrieval_relevance` | 经验与任务的相关度 | ≥0.7 | BM25 命中分均值 |
| 6 | `evo_retrieval_coverage` | 被使用过的经验占总经验比 | ≥60% | 使用过的经验 / 总经验 |

### 维度三: 进化效果 (Evolve)

| # | 指标名 | 定义 | 目标值 | 计算方式 |
|:--|:---|:---|:---|:---|
| 7 | `evo_task_success_trend` | 连续N轮团队任务的平均成功率 | 单调递增 | 滑动窗口成功率 |
| 8 | `evo_injection_uplift` | 有经验vs无经验的成功率差 | ≥10% | P(success\|injected) - P(success\|baseline) |
| 9 | `evo_error_recurrence` | 同类错误重复出现的频率 | 逐轮递减 | 同类型 error pattern 计数趋势 |

### 维度四: 效率指标 (Efficiency)

| # | 指标名 | 定义 | 目标值 | 计算方式 |
|:--|:---|:---|:---|:---|
| 10 | `evo_distill_latency_ms` | 单次提炼平均耗时 | <15s | 平均蒸馏耗时 |
| 11 | `evo_retrieve_latency_ms` | RetrieveFor 平均耗时 | <5ms | p99 检索时间 |
| 12 | `evo_consolidate_latency_ms` | 去重/剪枝耗时 | <1s | 整理耗时 |
| 13 | `evo_growth_rate` | 经验库膨胀率 | <10%/周 | 周增量 / 当前总量 |

### 维度五: 泛化能力 (Generalization)

| # | 指标名 | 定义 | 目标值 | 计算方式 |
|:--|:---|:---|:---|:---|
| 14 | `evo_cross_team_transfer` | A团队学到的经验在B团队被使用率 | ≥30% | 跨团队使用次数 / 总使用 |
| 15 | `evo_new_role_adaptation` | 新角色首次到达标的轮数 | <3轮 | 轮数统计 |
| 16 | `evo_abstraction_rate` | 从具体→通用的晋升比例 | ≥20% | general 经验 / 总经验 |
| 17 | `evo_lifecycle_promoted` | 经过验证晋升为 active 的比例 | ≥50% | promoted 状态数 / 总数 |
| 18 | `evo_counterfactual_gen` | 反事实学习生成率 | ≥1/失败 | 假设数 / 失败轨迹数 |

---

## 四、技术实现设计

### 4.1 经验结构升级 (Experience V2)

```go
type Experience struct {
    // ... 原有字段 ...
    Pattern     string    // "strategy" | "pattern" | "antidote" | "evidence"
    Lifecycle   string    // "proposed" | "validated" | "promoted" | "active" | "decaying" | "archived"
    SourceTeam  string    // 来源团队 (跨团队迁移追踪)
    MinHash     []uint64  // MinHash 签名 (快速去重)

    // 注入效果追踪
    InjectionCount  int   // 被注入的次数
    InjectionSuccess int  // 注入后任务成功次数
}
```

### 4.2 注入效果追踪器 (InjectionTracker)

```go
type InjectionRecord struct {
    ExpIDs   []string  // 注入的经验 ID
    TaskID   string    // 任务标识
    TeamName string    // 团队名
    Success  bool      // 任务结果
    Timestamp time.Time
}

type InjectionTracker struct {
    records []InjectionRecord
    // Uplift = P(success|injected) - P(success|baseline)
}
```

### 4.3 MinHash 去重 (替代 O(n²) Jaccard)

```go
// MinHash 签名: k 个哈希函数, 取各自最小值
func computeMinHash(tokens []string, k int) []uint64

// LSH 桶: 将签名分 b 段, 相同段的为候选对
func lshBuckets(signatures map[string][]uint64, bands, rows int) map[uint64][]string
```

### 4.4 UCB 经验选择 (替代 top-K)

```go
// UCB1: score = mean_reward + c * sqrt(ln(N) / n_i)
// 平衡利用(高均值)和探索(少使用)
func ucbSelect(experiences []*Experience, totalSelections int, c float64) []*Experience
```

### 4.5 生命周期状态机

```
proposed → (验证通过) → validated → (注入成功率>50%) → promoted
         ↓                                                    ↓
    (验证失败)                                          → active
         ↓                                                    ↓
      archived                              (30天未用/质量<0.1) → decaying → archived
```

### 4.6 同义词扩展 (检索增强)

```go
var synonyms = map[string][]string{
    "error":      {"错误", "异常", "失败", "bug"},
    "handling":   {"处理", "处理方式", "解决"},
    "concurrent": {"并发", "并行", "多线程"},
    // ...
}
```

---

## 五、修复清单

| 修复项 | 文件 | 改动说明 |
|:---|:---|:---|
| P1双向学习 | workflow.go | 成功路径也调用 LearnFromStage |
| P2注入追踪 | evolution.go | 新增 InjectionTracker + RecordInjection |
| P3结构化经验 | evolution.go | Experience 增加 Pattern/Lifecycle 字段 |
| P4检索增强 | evolution.go | BM25 + 同义词扩展 + 标签匹配 |
| P5 MinHash去重 | evolution.go | Consolidate 使用 MinHash+LSH |
| P6反事实学习 | evolution.go | 失败时生成假设性经验 |
| 18项指标 | evolution.go | CollectMetrics 新增全部18项 |
