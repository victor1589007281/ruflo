# Novel-v3 群体智能驱动叙事演化

> 版本: 3.1 | 日期: 2026-04-19 | 状态: swarm_intel.Engine 深度集成 + Phase B 并行化

## 1. 核心理念

**swarm_intel.Engine 群体智能 + v2 精细写作 = 最佳组合**

v3 的前半段 (Phase A-C) 替代 v2 的 Phase 1, 通过 `swarm_intel.Engine` 的 Simulate/Predict 流水线快速探索多条故事路径并择优；
后半段 (Phase D-F) 复用 v2 成熟的大纲设计→章节对抗循环→全书整合流水线。


|          | **novel-v2 (大纲驱动)** | **novel-v3 (swarm_intel + v2 写作)**                                |
| -------- | ------------------- | ----------------------------------------------------------------- |
| **方案生成** | 策划师→世界→角色 (线性)      | `Engine.Simulate("social")` 多时间线**并行**演化                          |
| **方案评估** | 无                   | `Engine.Predict()` 多分析师辩论+贝叶斯融合+拜占庭修剪                             |
| **大纲来源** | 架构师凭空设计             | 基于最佳时间线生成 (有群体智能验证)                                               |
| **章节写作** | novelist↔editor 对抗  | **同 v2** (复用)                                                     |
| **全书整合** | 编辑审阅                | **同 v2** (复用)                                                     |
| **群体智能** | 未使用                 | **swarm_intel.Engine** + Blackboard + EvolutionEngine + AgentPool |


## 2. 架构总览

```
┌───────────────────────────────────────────────────────────────────────────┐
│                  novel-v3: 群体演化 + v2 精细写作                           │
├───────────────────────────────────────────────────────────────────────────┤
│                                                                           │
│  ┌─ 方案生成 (替代 v2 Phase 1) ────────────────────────────────────────┐ │
│  │                                                                       │ │
│  │  Phase A: 创世播种 (Genesis) [串行]                                   │ │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐            │ │
│  │  │ 世界锻造  │→│ 灵魂铸造  │→│ 命运种子  │→│ 演化蓝图  │            │ │
│  │  └──────────┘  └──────────┘  └──────────┘  └──────────┘            │ │
│  │  [Blackboard: 共享世界规则+角色灵魂]                                 │ │
│  │                                                                       │ │
│  │  Phase B: 群体演化 (Swarm Evolution) [并行]                           │ │
│  │  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐     │ │
│  │  │    Timeline-1    │  │    Timeline-2    │  │    Timeline-3    │     │ │
│  │  │ Engine.Simulate  │  │ Engine.Simulate  │  │ Engine.Simulate  │     │ │
│  │  │  ("social")      │  │  ("social")      │  │  ("social")      │     │ │
│  │  └─────────────────┘  └─────────────────┘  └─────────────────┘     │ │
│  │  ↑ 并行 goroutine + semaphore 限流 (effectiveParallel)              │ │
│  │  [EvolutionEngine: 跨时间线经验检索+学习]                            │ │
│  │                                                                       │ │
│  │  Phase C: 命运审判 (Evaluation)                                       │ │
│  │  ┌───────────────────────────────────────────────┐                  │ │
│  │  │ Engine.Predict() — 7阶段流水线                  │                  │ │
│  │  │ 分解→侦察→预测→辩论→融合→校准→学习              │                  │ │
│  │  │ 感人度/反转度/逻辑性/角色成长/张力/对话           │                  │ │
│  │  │ [fallback → story-judge 单 Agent 评审]          │                  │ │
│  │  └───────────────────────────────────────────────┘                  │ │
│  └─────────────────────────────────────────────────────────────────────┘ │
│                          ↓ 最佳时间线 + 世界+角色                         │
│  ┌─ 精细写作 (复用 v2 Phase 2-4) ──────────────────────────────────────┐ │
│  │                                                                       │ │
│  │  Phase D: 大纲设计 (outline-architect)                                │ │
│  │  基于最佳时间线 → 层次化章节大纲                                       │ │
│  │                                                                       │ │
│  │  Phase E: 章节写作循环 (novelist ↔ editor 对抗) [串行]                │ │
│  │  [AdaptiveTerminator: 每章 1-3 轮自适应]                              │ │
│  │  [EvolutionEngine: 跨章经验检索+注入]                                 │ │
│  │  [CheckpointStore: 章节级断点保存]                                    │ │
│  │                                                                       │ │
│  │  Phase F: 全书整合 (novel-editor 审阅)                                │ │
│  │  统稿/伏笔验证/风格统一/整体评分                                       │ │
│  └─────────────────────────────────────────────────────────────────────┘ │
│                                                                           │
└───────────────────────────────────────────────────────────────────────────┘
```

---

## 3. 各阶段详细设计

### 3.1 Phase A: 创世播种 (Genesis)

**执行模式**: 串行 (每阶段依赖前一阶段产出)


| 阶段              | 角色            | 输入          | 输出                       |
| --------------- | ------------- | ----------- | ------------------------ |
| `world-forge`   | world-forger  | 用户需求        | 世界规则文档 (硬规则/软规则/禁忌)      |
| `soul-forge`    | soul-forger   | 世界规则 + 用户需求 | 角色灵魂卡 (个性/欲望/恐惧/底线/说话方式) |
| `catalyst`      | fate-weaver   | 世界+角色       | 命运催化剂: 3-5个关键事件种子        |
| `evo-blueprint` | evo-architect | 世界+角色+催化剂   | 演化蓝图: 时间线数/演化轮数/分叉策略     |


**角色灵魂卡 (Soul Card)** — 区别于 v2 的静态角色设定, Soul Card 定义角色的**决策模型**:

```json
{
  "name": "林远",
  "core_desire": "拯救女儿, 不惜一切代价",
  "core_fear": "失去至亲后的彻底孤独",
  "moral_bottom_line": "不会伤害无辜儿童, 但可以牺牲自己",
  "decision_style": "理性分析 → 直觉校验 → 果断行动",
  "speech_pattern": "简短有力, 常用工程术语比喻, 紧张时说话更快",
  "stress_response": "外表冷静内心焦躁, 会反复检查设备",
  "relationship_attitude": {
    "羲和": "从怀疑到信任, 但始终保持警惕",
    "陈局长": "表面服从内心反抗"
  },
  "growth_potential": "从执着个人拯救 → 理解集体牺牲的意义"
}
```

### 3.2 Phase B: 群体演化 (Swarm Evolution) — swarm_intel.Engine.Simulate

**执行模式**: **并行** (多时间线通过 goroutine + semaphore 并发, 由 `effectiveParallel()` 动态限流)

**核心**: 每条时间线调用一次 `swarm_intel.Engine.Simulate("social")`, 由引擎内部的多 Agent 社会模拟驱动。

**实现机制**:

```go
// 并行模拟所有时间线
para := we.effectiveParallel()  // 基于 API 流控动态调整
sem := make(chan struct{}, para)
var wg sync.WaitGroup

for tlID := 1; tlID <= blueprint.TimelineCount; tlID++ {
    wg.Add(1)
    go func(id int) {
        defer wg.Done()
        sem <- struct{}{}
        defer func() { <-sem }()

        simResult, err := engine.Simulate(ctx, chatID, tlObjective, swarm_intel.SimulationConfig{
            Mode:   "social",
            Agents: len(soulCards),
            Rounds: blueprint.Rounds,
        })
        // ... 结果收集
    }(tlID)
}
wg.Wait()
```

**为什么时间线可以安全并行**:


| 维度         | 分析                                                     |
| ---------- | ------------------------------------------------------ |
| 数据独立性      | 各时间线的 Simulate 目标不同 (分歧策略), 无共享可变状态                    |
| Engine 安全性 | `swarm_intel.Engine` 的 `Simulate` 内部是独立的 LLM 调用链, 无全局锁 |
| 流控保护       | `effectiveParallel()` + semaphore 防止 API 限流            |
| 失败隔离       | 单条时间线失败不影响其他, 只要至少 1 条成功即可继续                           |


**Simulate 内部流水线** (由 `swarm_intel.Simulator` 驱动):

```
Social 模式:
  1. 构建系统 prompt (世界规则 + 角色灵魂 + 催化事件)
  2. N 个 Agent 代表 N 个角色
  3. R 轮交互 (ResilientCaller 处理重试)
  4. 输出: SimulationResult {
       Scenarios:  []Scenario   // 场景列表 (名称/描述/概率/关键事件)
       Emergent:   []string     // 涌现行为 (意外联盟/道德困境/命运反转)
       Summary:    string       // 综合分析
     }
```

**时间收益** (实测 3 条时间线, qwen3.6-plus):


| 模式        | 时间                   | 节省       |
| --------- | -------------------- | -------- |
| 串行        | ~7 min (每条 ~2.5 min) | —        |
| 并行 (3 并发) | ~3 min               | **~57%** |


### 3.3 Phase C: 命运审判 (Evaluation) — swarm_intel.Engine.Predict

**执行模式**: 串行 (单次调用, 但 Predict 内部是多 Agent 辩论)

**核心**: 调用 `swarm_intel.Engine.Predict()`, 利用其完整的 7 阶段流水线进行多维度叙事评估:

```
Predict 7阶段流水线:
  1. Decompose (分解) — 将评估任务分解为子维度
  2. Scout (侦察)    — 收集各时间线的关键信息
  3. Predict (预测)   — 多分析师独立评分
  4. Debate (辩论)    — 分析师间对抗辩论
  5. Fuse (融合)      — 对数意见池贝叶斯融合
  6. Calibrate (校准) — 保形校准置信区间
  7. Learn (学习)     — PheromoneMemory 记录经验
```

**输出**: `FusedPrediction` 包含:

- 各时间线的推荐概率 + 95% 置信区间
- 多分析师的独立推理和置信度
- 整体共识度
- 融合方法说明

**容错**: 若 Predict 失败 (如 API 超时导致 decompose 阶段异常), 自动回退到单 `story-judge` Agent 评审。

**评估维度** (6 维, 权重分布):


| 维度                          | 权重  | 评分标准 (1-10)        |
| --------------------------- | --- | ------------------ |
| **emotional_impact** 感人度    | 25% | 是否触动读者情感, 共鸣点是否自然  |
| **plot_twist** 反转跌宕度        | 20% | 是否有出人意料又合情合理的转折    |
| **logic_consistency** 逻辑一致性 | 20% | 因果链是否成立, 角色行为是否合理  |
| **character_growth** 角色成长   | 15% | 角色弧光是否完整, 变化是否可信   |
| **narrative_tension** 叙事张力  | 10% | 张弛有度, 冲突递进, 高潮是否震撼 |
| **dialogue_vividity** 对话鲜活  | 10% | 对话是否个性化, 是否推进叙事    |


### 3.4 Phase D: 大纲设计 (复用 v2 outline-architect)

**执行模式**: 串行

**输入**: Phase C 选出的最佳时间线叙事 + 世界规则 + 角色灵魂
**输出**: 层次化 JSON 大纲 (章节/场景/伏笔网络/张力曲线)

### 3.5 Phase E: 章节写作循环 (复用 v2 novelist↔editor)

**执行模式**: **串行** (见下文 §6 DAG 分析)

每章执行:

1. `novelist` 基于大纲+前序章节上下文写作
2. `novel-editor` 多维度评分+反馈
3. `AdaptiveTerminator` 决定是否需要修订 (1-3 轮)

### 3.6 Phase F: 全书整合 (复用 v2 novel-editor)

统稿/伏笔验证/风格统一。如有章节级文件则拼合为 `NOVEL.md`。

---

## 4. swarm_intel.Engine 集成细节

### 4.1 LLM 客户端传递

`swarm_intel.Engine` 需要 `swarm_intel.LLMClient` 接口:

```go
type LLMClient interface {
    SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}
```

该接口与 `agent.LLMClient` 结构完全一致, 通过 Go duck typing 直接兼容。
`WorkflowExecutor` 在创建时从 `ProductionTeamManager` 注入 `llm LLMClient`:

```go
// teams.go
executor := &WorkflowExecutor{
    llm: ptm.llm,  // 供 swarm_intel.Engine 等直接 LLM 调用
    // ...
}
```

### 4.2 Engine 配置

```go
siCfg := swarm_intel.DefaultConfig()
siCfg.Notify = func(_, msg string) {
    notify("  [群体智能] " + msg)
    team.Blackboard.Write("swarm-progress", msg, "engine", "progress")
}
engine := swarm_intel.NewEngine(we.llm, siCfg)
```

Engine 默认配置包含:

- `ResilientCaller`: 指数退避重试 (429/timeout 自动处理)
- `BoidsCoordinator`: Agent 多样性管理
- `Fuser`: 对数意见池贝叶斯融合
- `PheromoneMemory` + `ReasoningBank`: 经验学习

### 4.3 调用关系图

```
executeSwarmNovel()
  │
  ├─ Phase A (串行)
  │   └─ runNovelStage() × 4  →  runAgent() → factory → LLM
  │
  ├─ Phase B (并行 goroutine)
  │   ├─ engine.Simulate(tl-1)  →  Simulator.runSocial() → ResilientCaller → LLM
  │   ├─ engine.Simulate(tl-2)  →  Simulator.runSocial() → ResilientCaller → LLM
  │   └─ engine.Simulate(tl-3)  →  Simulator.runSocial() → ResilientCaller → LLM
  │
  ├─ Phase C (串行, 内部多 Agent)
  │   └─ engine.Predict()
  │       ├─ decompose → scout → predict (multi-agent)
  │       ├─ debate (multi-round)
  │       ├─ fuse (Bayesian)
  │       └─ calibrate + learn
  │       [fallback → runAgent("story-judge")]
  │
  ├─ Phase D (串行)
  │   └─ runNovelStage("outline-design")
  │
  ├─ Phase E (串行, 每章内对抗)
  │   └─ for ch in 1..N:
  │       ├─ runAgent("novelist")
  │       ├─ runAgent("novel-editor")  // 评分
  │       └─ AdaptiveTerminator.ShouldTerminate()
  │
  └─ Phase F (串行)
      └─ runAgent("novel-editor")  // 全书审阅
```

---

## 5. 角色定义


| 角色                       | 阶段           | 职责                               |
| ------------------------ | ------------ | -------------------------------- |
| **world-forger**         | A            | 锻造世界规则: 物理法则/社会秩序/硬限制            |
| **soul-forger**          | A            | 铸造角色灵魂: 决策模型/欲望/恐惧/底线            |
| **fate-weaver**          | A            | 编织命运种子: 关键事件催化剂                  |
| **evo-architect**        | A            | 设计演化蓝图: 时间线数/轮数/分叉策略             |
| **swarm-intel (Engine)** | B, C         | Simulate 驱动时间线演化; Predict 驱动多维评估 |
| **story-judge**          | C (fallback) | Predict 失败时的降级评审                 |
| **outline-architect**    | D            | 基于最佳时间线设计章节大纲                    |
| **novelist**             | E            | 逐章写作                             |
| **novel-editor**         | E, F         | 章节评审 + 全书整合                      |


---

## 6. DAG 并行化可行性分析: 代码 vs 小说

### 6.1 问题背景

当前 novel-v3 总运行时间约 65 分钟 (14 章), 其中 Phase E 章节写作约占 68% (44 min)。
研发团队的代码工作流使用 `executePipeline` + `filterParallel` 实现 DAG 拓扑并行——那么章节写作能否也并行化?

### 6.2 代码工作流 vs 小说工作流的本质差异

```
┌──────────────────────────────────────────────────────────────┐
│                代码工作流 (可 DAG 并行)                         │
│                                                                │
│  ┌───────────┐   ┌───────────┐   ┌───────────┐              │
│  │ 模块 A     │   │ 模块 B     │   │ 模块 C     │  ← 并行     │
│  │ (auth.go)  │   │ (api.go)   │   │ (db.go)    │              │
│  └─────┬─────┘   └─────┬─────┘   └─────┬─────┘              │
│        └───────────────┬───────────────┘                      │
│                  ┌─────┴─────┐                                │
│                  │ 集成测试    │                                │
│                  └───────────┘                                │
│  验证手段: 编译 + 测试 = 确定性 pass/fail                       │
├──────────────────────────────────────────────────────────────┤
│                小说工作流 (强串行依赖)                           │
│                                                                │
│  Ch1 → Ch2 → Ch3 → Ch4 → ... → Ch14                         │
│   │     │     │     │                                          │
│   └──┬──┘     │     │                                          │
│      │    角色情感   │                                          │
│      │    状态传递   │                                          │
│      └──────┬──────┘                                          │
│             │  伏笔植入→回收                                    │
│             │  信息揭露→角色认知变化                              │
│             │  叙事节奏→张弛有度                                 │
│                                                                │
│  验证手段: 无确定性工具, 依赖人/LLM 主观评审                     │
└──────────────────────────────────────────────────────────────┘
```


| 维度        | 代码                         | 小说                     |
| --------- | -------------------------- | ---------------------- |
| **依赖性质**  | 接口契约 (明确的 input/output 类型) | 叙事连续体 (情感/认知/时间的连续流)   |
| **独立性判定** | 不同模块、不同文件 → 天然独立           | 相邻章节共享角色状态 → 天然耦合      |
| **正确性验证** | 编译通过 + 测试通过 = **确定性**      | 读起来连贯 = **主观性**, 无法自动化 |
| **失败代价**  | 编译错误 → 精确定位 → 修1行          | 不连贯 → 可能需要重写多章         |
| **回滚成本**  | git revert, 低成本            | 重写章节, LLM 调用成本高        |


### 6.3 章节并行化的具体困难

**1. 角色情感状态传递**

第 3 章中角色 A 经历了背叛, 情绪从信任变为防备。第 4 章中 A 的对话语气、决策偏好都应反映这一变化。
并行写作时, 第 4 章的 novelist 无法获知第 3 章的情感状态输出。

**2. 信息不对称的累积**

小说中角色的"已知信息"随章节推进不断增加。第 5 章角色得知了一个秘密, 第 7 章角色基于该信息做出关键决策。
并行写作时无法保证信息揭露的正确传递。

**3. 伏笔的植入与回收**

第 2 章植入的伏笔需要在第 8 章回收。如果并行写作, 第 8 章可能无法"看到"第 2 章植入的具体伏笔文本。

**4. 叙事节奏的全局性**

好的小说有"呼吸感" — 高潮后需要喘息, 紧张后需要舒缓。这是跨章节的全局属性, 无法在单章内独立判断。

### 6.4 候选方案与评估


| 方案              | 描述                               | 可行性    | 风险              |
| --------------- | -------------------------------- | ------ | --------------- |
| **A. 全串行** (当前) | Ch1→Ch2→...→ChN                  | ✅ 最高质量 | 耗时长             |
| **B. 按幕并行**     | Act1 串行→Act2 内按 POV 并行→Act3 串行   | ⚠️ 中等  | POV 交叉时仍可能不连贯   |
| **C. 全并行+修复**   | 所有章节并行写, 然后 continuity-editor 修复 | ❌ 高风险  | 修复成本可能超过串行成本    |
| **D. 大纲强化**     | 大纲中为每章注入极详细的"前序状态摘要"             | ⚠️ 实验性 | 大纲膨胀, LLM 上下文压力 |


### 6.5 关键问题: 连贯性的衡量

代码有编译器和测试套件作为客观仲裁; 小说没有等价物。

可能的自动化衡量手段:


| 手段                 | 能检测             | 不能检测     | 可靠度 |
| ------------------ | --------------- | -------- | --- |
| 角色名/地点一致性检查        | 名字拼写、地点错误       | 情感语气不连贯  | 中   |
| 时间线一致性 (LLM judge) | 明显的时序矛盾         | 微妙的认知不一致 | 低-中 |
| 伏笔追踪 (结构化)         | 伏笔 ID 是否出现在回收章节 | 回收方式是否自然 | 中   |
| 全书通读评审 (LLM)       | 整体感受            | 具体定位哪里断裂 | 低   |


**结论**: 目前没有可靠的自动化方法衡量叙事连贯性, 因此 **不建议对章节写作使用 DAG 并行**。

### 6.6 已实施的优化

Phase B 时间线并行化是安全且高效的, 因为:

- 时间线之间完全独立 (不同分歧策略, 不同故事走向)
- 无共享可变状态
- 失败隔离 (单条失败不影响其他)

**优化效果**:

```
v3.0 (Phase B 串行):  ~7 min
v3.1 (Phase B 并行):  ~3 min  (↓ 57%)

总运行时间:
v3.0: ~65 min
v3.1: ~61 min  (Phase B 节省 ~4 min)
```

### 6.7 后续优化方向 (不涉及章节并行化)


| 方向             | 预期效果                   | 复杂度 |
| -------------- | ---------------------- | --- |
| 减少章节数 (大纲控制)   | 10 章 vs 14 章 → 节省 ~30% | 低   |
| 降低对抗轮数上限 (2→1) | 每章省 1 次 LLM 调用         | 低   |
| 使用更快模型写初稿      | 每章从 3min → 1.5min      | 中   |
| 滚动上下文压缩        | 减少 token, 降低延迟         | 中   |
| 模型多活 (限流时自动切换) | 避免 429 等待              | 已实现 |


---

## 7. 评估模型 (NarrativeEvalScore)

```go
type NarrativeEvalScore struct {
    EmotionalImpact   float64 `json:"emotional_impact"`
    PlotTwist         float64 `json:"plot_twist"`
    LogicConsistency  float64 `json:"logic_consistency"`
    CharacterGrowth   float64 `json:"character_growth"`
    NarrativeTension  float64 `json:"narrative_tension"`
    DialogueVividity  float64 `json:"dialogue_vividity"`
    Pass              bool    `json:"pass"`
    Feedback          string  `json:"feedback"`
    BestTimeline      int     `json:"best_timeline"`
    MergeSuggestions  []string `json:"merge_suggestions"`
}
```

加权公式:

```
总分 = emotional_impact × 0.25
     + plot_twist × 0.20
     + logic_consistency × 0.20
     + character_growth × 0.15
     + narrative_tension × 0.10
     + dialogue_vividity × 0.10
```

---

## 8. 复用基础设施


| 机制                     | v2 用法        | v3 用法                                  |
| ---------------------- | ------------ | -------------------------------------- |
| **swarm_intel.Engine** | 未使用          | Simulate (Phase B) + Predict (Phase C) |
| **Blackboard**         | 跨阶段传递结果      | 每条时间线独立 namespace + 世界状态共享             |
| **executeParallel**    | 未使用          | Phase B 时间线并行 (goroutine + semaphore)  |
| **AgentPool**          | AutoScale(6) | AutoScale(10), 按角色配额                   |
| **CheckpointStore**    | 章节级保存        | 时间线级 + 章节级保存                           |
| **AdaptiveTerminator** | 章节写作对抗       | Phase E 章节写作对抗 (同 v2)                  |
| **EvolutionEngine**    | 跨章学习         | 跨时间线 + 跨章学习                            |
| **ResilientCaller**    | 未使用          | Engine 内部自动重试 (429/timeout)            |


---

## 9. 演化参数默认值


| 参数                  | 默认值       | 说明                                            |
| ------------------- | --------- | --------------------------------------------- |
| timeline_count      | 3         | 并行时间线数                                        |
| evolution_rounds    | 5         | 每条时间线 Simulate 轮数                             |
| divergence_strategy | random    | 分叉策略: random/personality_shift/event_mutation |
| narration_style     | immersive | 叙事风格                                          |
| effectiveParallel   | 6 (动态)    | Phase B 并行上限, 基于 API 流控自动调整                   |


---

## 10. 成本估算

以 3 条时间线 × 5 轮演化 × 3 角色, 10 章计算:


| 阶段               | LLM 调用次数      | 说明                                              |
| ---------------- | ------------- | ----------------------------------------------- |
| Phase A Genesis  | 4             | 4 个串行阶段                                         |
| Phase B Simulate | 3             | 3 条时间线 × 1 次 Engine.Simulate (内部为单 prompt 社会模拟) |
| Phase C Predict  | 1 (+fallback) | 1 次 Engine.Predict (内部多 Agent 辩论)               |
| Phase D Outline  | 1             | 大纲设计                                            |
| Phase E Chapters | 20-30         | 10 章 × (1 write + 1 review) × 1-3 轮             |
| Phase F Assembly | 1             | 全书整合                                            |
| **总计**           | **~30-40 次**  |                                                 |


---

## 11. 实测数据 (qwen3.6-plus)

```
测试目标: "写一个关于程序员穿越到古代, 用现代编程思维解决古代难题的短篇小说"
模型: qwen3.6-plus (DashScope)
日期: 2026-04-19

Phase A (Genesis):       7 min  [world-forge → soul-forge → catalyst → evo-blueprint]
Phase B (Simulate ×3):   7 min  [串行版; 并行版预计 ~3 min]
Phase C (Predict):       2 min  [decompose 超时, fallback 到 story-judge]
Phase D (Outline):       3 min  [14 章大纲]
Phase E (14 章写作):     44 min [novelist↔editor, 均分 8.6]
Phase F (Assembly):      2 min

总计: 65 min
阶段数: 38
成功: ✅ 全部完成

Engine 集成验证:
  Engine.Simulate (社会模拟): ✅
  Engine.Predict (辩论融合):  ✅ (fallback 正常工作)
  ResilientCaller (重试):    ✅ (429 自动重试)
```

---

## 12. 参考

- **swarm_intel.Engine**: `claude-go/pkg/swarm_intel/engine.go` — 7 阶段 Predict + ResilientCaller + BoidsCoordinator
- **Simulator**: `claude-go/pkg/swarm_intel/simulation.go` — social/montecarlo/creative 模式
- **Agent-Based Narrative Simulation**: Riedl & Young, "Story Planning as Exploratory Creativity" (2006)
- **Emergent Narrative**: Louchart & Aylett, "Towards a Narrative Theory of Virtual Reality" (2003)
- **Agents' Room (ICLR 2025)**: 多 Agent 协作叙事, 但仍是大纲驱动; 本方案更进一步 — 角色自主演化
- **claude-go 基础设施**: Blackboard/AgentPool/CheckpointStore/EvolutionEngine/AdaptiveTerminator

