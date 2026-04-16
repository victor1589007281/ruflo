# 群体智能引擎 (Swarm Intelligence Engine) — 设计方案

> claude-go v2.1 · 2026-04-05 (第二版: 加入 2025-2026 LLM-native 研究 + M6/M7 设计)

---

## 1. 愿景

一句话：**启动一个目标，群体智能引擎自动分解、模拟、预测、融合，输出高置信度结论。**

参考 MiroFish 的多 Agent 社会模拟 + 学术界群体智能算法，在 claude-go 现有 team/swarm 系统之上构建新一代**预测与模拟引擎**。核心差异：

| 维度 | 现有 Swarm 模式 | 群体智能引擎 |
|------|----------------|-------------|
| **目标** | 任务分解 → 并行执行 → 合并 | 预测/模拟 → 多视角辩论 → 贝叶斯融合 → 置信度评分 |
| **Agent 协作** | DAG 依赖传递 | Boids 风格动态分群 + 信素记忆 + 辩论回合 |
| **输出** | 文本报告 | 结构化预测 (概率分布 + 置信区间 + 场景树) |
| **进化** | 经验蒸馏 | 预测校准 (Brier Score) + 信素强化学习 |

---

## 2. 架构总览

```
┌─────────────────────────────────────────────────────────┐
│                    CLI / REPL / Feishu                   │
│         /predict <目标>  ·  /simulate <场景>             │
├─────────────────────────────────────────────────────────┤
│              SwarmIntelligenceEngine                     │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐              │
│  │ Decomposer│  │ Simulator│  │  Fuser   │              │
│  │ (LLM分解) │  │ (多轮模拟)│  │ (贝叶斯  │              │
│  │           │  │          │  │  融合)   │              │
│  └─────┬────┘  └─────┬────┘  └─────┬────┘              │
│        │             │             │                    │
│  ┌─────▼─────────────▼─────────────▼────┐               │
│  │         Agent Swarm Layer            │               │
│  │  ┌────┐ ┌────┐ ┌────┐ ┌────┐        │               │
│  │  │ A1 │ │ A2 │ │ A3 │ │ An │  Boids │               │
│  │  └──┬─┘ └──┬─┘ └──┬─┘ └──┬─┘        │               │
│  │     └──────┼──────┼──────┘           │               │
│  │         Pheromone Memory             │               │
│  └──────────────────────────────────────┘               │
│                                                         │
│  ┌──────────────────────────────────────┐               │
│  │       Blackboard (共享状态)           │               │
│  │  hypotheses · evidence · votes       │               │
│  │  pheromone_trails · calibration      │               │
│  └──────────────────────────────────────┘               │
├─────────────────────────────────────────────────────────┤
│  ProductionTeamManager (workflow="predict")              │
│  EvolutionEngine · RoleRegistry · AgentPool             │
└─────────────────────────────────────────────────────────┘
```

---

## 3. 核心概念

### 3.1 预测域 (PredictionDomain)

将任意目标转化为可预测问题：

```go
type PredictionDomain struct {
    Question    string            // 核心问题
    Horizon     string            // 时间尺度 (short/medium/long)
    OutcomeType string            // binary/categorical/numeric/scenario
    Outcomes    []string          // 可能结果集
    Context     string            // 背景上下文
    Constraints []string          // 约束条件
}
```

### 3.2 Agent 角色 (参考 Society of Mind)

| 角色 | 职责 | 数量 |
|------|------|------|
| **Decomposer** | 将目标分解为可预测子问题 | 1 |
| **Scout** | 信息搜集、证据检索 | 2-3 |
| **Analyst** | 独立分析，生成初始预测 | 3-5 |
| **Critic** | 质疑预测假设，找反例 | 2-3 |
| **Synthesizer** | 贝叶斯融合多方预测 | 1 |
| **Calibrator** | 校准置信度，Brier Score | 1 |

### 3.3 Boids 协调规则

参考 Reynolds (1987) 经典 Boids 模型，适配到信念空间：

- **Alignment (对齐)**: 共享证据协议，Agent 在每轮后同步关键证据
- **Separation (分离)**: 强制多样性，相似度过高的假设会被惩罚
- **Cohesion (聚合)**: 保持在合理假设空间内，避免极端偏离

```go
type BoidsConfig struct {
    AlignmentWeight  float64 // 证据对齐权重 (默认 0.3)
    SeparationWeight float64 // 假设分离权重 (默认 0.4)
    CohesionWeight   float64 // 共识聚合权重 (默认 0.3)
    MaxDivergence    float64 // 最大分歧阈值 (超过则加轮辩论)
}
```

### 3.4 信素记忆 (Pheromone Memory)

参考蚁群算法 (Dorigo 1997)：

```go
type PheromoneTrail struct {
    HypothesisID string
    Strength     float64 // 信素浓度 (0-1)
    Evidence     []string
    Supporters   int
    LastUpdate   time.Time
    DecayRate    float64 // 衰减率 (默认 0.1/轮)
}
```

- **正反馈**: 被多个 Agent 支持的假设 → 信素增强
- **挥发**: 未被支持的假设 → 信素自然衰减
- **路径记忆**: 成功的推理路径保留，下次类似问题可快速复用

### 3.5 预测融合 (Bayesian Belief Fusion)

参考 Du et al. (2023) Multi-Agent Debate + 贝叶斯聚合：

```go
type Prediction struct {
    OutcomeID    string
    Probability  float64           // 单个 Agent 的估计
    Confidence   float64           // 自评置信度
    Rationale    string            // 推理过程
    Evidence     []string          // 支撑证据
    AgentRole    string
}

type FusedPrediction struct {
    Question     string
    Outcomes     []OutcomePrediction // 各结果的融合概率
    Consensus    float64             // 共识度 (0-1)
    BrierScore   float64             // 校准分数 (越低越好)
    Rounds       int                 // 辩论轮数
    Methodology  string              // 使用的融合方法
}
```

融合算法（对数意见池）：

```
log P*(outcome) ∝ Σ w_i × log P_i(outcome)
```

其中 `w_i` 由 Agent 的历史校准表现决定。

---

## 4. 执行流水线

```
┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐
│  Phase 1  │    │  Phase 2  │    │  Phase 3  │    │  Phase 4  │    │  Phase 5  │
│  Decompose│ →  │   Scout   │ →  │  Predict  │ →  │  Debate   │ →  │   Fuse    │
│  分解目标  │    │  搜集证据  │    │  独立预测  │    │  辩论质疑  │    │  融合校准  │
└──────────┘    └──────────┘    └──────────┘    └──────────┘    └──────────┘
     1轮              2轮           3-5轮          2-3轮            1轮
```

### Phase 1: Decompose (分解)
- Decomposer Agent 将目标拆成子问题
- 判断 OutcomeType (binary/numeric/scenario)
- 确定 Horizon 和评估标准

### Phase 2: Scout (信息侦察)
- Scout Agent 并行搜集相关信息
- 写入 Blackboard evidence 区
- 信素初始化: 基于证据创建初始假设路径

### Phase 3: Predict (独立预测)
- 多个 Analyst Agent 独立给出预测
- 每个 Agent 有不同视角 (乐观/悲观/中立/黑天鹅)
- Boids Separation: 检查预测多样性，过于一致则强制差异化

### Phase 4: Debate (辩论)
- Critic Agent 质疑每个预测的薄弱点
- Analyst Agent 修正预测
- Boids Alignment: 交换证据，更新共同认知
- 重复 2-3 轮直到 Boids Cohesion 达到阈值

### Phase 5: Fuse (融合)
- Synthesizer: 对数意见池融合概率
- Calibrator: Brier Score 校准 + 置信区间
- 输出 FusedPrediction 结构化结果

---

## 5. 与现有系统的集成点

### 5.1 Workflow 注册

在 `GetWorkflow()` 中新增 `"predict"` 工作流：

```go
// workflow.go
case "predict", "prediction", "forecast":
    return &WorkflowDef{
        Name: "predict",
        Mode: "predict",  // 新模式
        Stages: predictWorkflowStages(),
    }
```

### 5.2 团队管理

`executeWorkflow` 中增加 predict 分支：

```go
if team.Workflow == "predict" {
    ptm.executePrediction(ctx, team)
    return
}
```

### 5.3 REPL 命令

```
/predict <问题>              — 启动预测
/simulate <场景>             — 启动场景模拟
/predict status              — 查看预测进度
/predict history             — 查看历史预测
```

### 5.4 Blackboard 扩展

新增 category:

| Category | 用途 |
|----------|------|
| `hypothesis` | 假设 |
| `evidence` | 证据 |
| `prediction` | 单 Agent 预测 |
| `debate` | 辩论记录 |
| `pheromone` | 信素状态 |
| `fusion` | 融合结果 |

---

## 6. 数据结构设计

### 6.1 核心引擎

```go
type SwarmIntelligenceEngine struct {
    llm         LLMClient
    pool        *AgentPool
    teamMgr     *ProductionTeamManager
    pheromones  *PheromoneMemory
    calibration *CalibrationStore
    boids       BoidsConfig
}
```

### 6.2 信素记忆

```go
type PheromoneMemory struct {
    trails map[string]*PheromoneTrail
    mu     sync.RWMutex
}
```

### 6.3 校准存储

```go
type CalibrationStore struct {
    history []CalibrationEntry // 历史预测 vs 实际结果
}

type CalibrationEntry struct {
    Question    string
    Predicted   float64
    Actual      float64 // 事后填入
    BrierScore  float64
    AgentWeights map[string]float64
}
```

---

## 7. 模拟能力 (SimulateAnything)

除了直接预测，引擎还支持**场景模拟** (参考 MiroFish):

### 7.1 社会模拟
- N 个 Agent 模拟不同利益相关者
- 在虚拟社交环境中互动 (类似 MiroFish 的 Twitter/Reddit 模拟)
- 观察涌现行为

### 7.2 博弈模拟
- Agent 代表不同博弈方
- 通过多轮策略互动
- 纳什均衡近似

### 7.3 蒙特卡洛场景树
- 多个并行世界线
- 每条线由不同 Agent 探索
- 概率加权合并

```go
type SimulationConfig struct {
    Mode        string    // "social" / "game" / "montecarlo"
    Agents      int       // Agent 数量
    Rounds      int       // 模拟轮数
    Scenarios   []string  // 场景分支
    StopCond    string    // 停止条件
}
```

---

## 8. 学术参考

### 8.1 经典基础 (奠基理论)

| 论文/方法 | 年份 | 本引擎中的映射 |
|-----------|------|--------------|
| Boids (Reynolds) | 1987 | Agent 协调规则 |
| Society of Mind (Minsky) | 1986 | Agent 角色分工 |
| PSO (Kennedy & Eberhart) | 1995 | 信念空间粒子优化 |
| ACO (Dorigo) | 1997 | 信素记忆 + 路径强化 |
| Delphi Method | 1950s | 多轮匿名预测迭代 |
| Prediction Markets (LMSR) | 2003 | 内部市场化信念聚合 |

### 8.2 LLM-Native 前沿研究 (2025-2026)

> 以下论文直接驱动了 v2.1 的架构演进。

#### 🔬 群体规模与涌现病理

| 论文 | 来源 | 关键发现 | 引擎映射 |
|------|------|---------|---------|
| **Do Agent Societies Develop Intellectual Elites?** (Venkatesh & Cui) | arXiv:2604.02674, Apr 2026 | ~1.5M交互研究：重尾协调级联 + 优先连接 → "知识精英"; **Deficit-Triggered Integration (DTI)** 选择性增强整合 | → M6 DTI 门控: 当协调不均衡时强制交叉检查 |
| **Evaluating Collective Behaviour of Hundreds of LLM Agents** (Willis et al.) | arXiv:2602.16662, Feb 2026 | 更强模型在大规模时可能导致更差集体结果; 文化进化选择风险 | → Agent 池多样性调控 + 激励对齐机制 |
| **Value Diversity in Multi-Agent Communities** (Huang et al.) | arXiv:2512.10665, Dec 2025 | Schwartz 价值多样性提升稳定性,极端异质性则失稳 | → Boids SeparationWeight 自适应: 多样性作为超参数 |

#### 🧠 LLM-Native 协调 (非经典 Boids)

| 论文 | 来源 | 关键发现 | 引擎映射 |
|------|------|---------|---------|
| **REDEREF: Training-Free Probabilistic Control** (Hosseini et al.) | arXiv:2603.13256, Feb 2026 | Thompson Sampling 路由 + 反思式重路由 + 证据评分; **-28% token, -17% 调用, -19% 延迟** | → M7 在线 Bandit 路由替代固定拓扑 |
| **Self-Organizing LLM Agents Outperform Designed Structures** (Dochkina et al.) | arXiv:2603.28990, Mar 2026 | 最小脚手架 → 自发角色涌现; Sequential 协议优于中心化协调 ~14% | → 强模型下减少硬编码角色, 允许自组织 |
| **Communication-Centric Survey of LLM-MAS** | arXiv:2502.14321, Jun 2025 | 围绕通信(协议/对象/策略)设计 LLM-MAS; 强调效率/安全/基准 | → 消息类型分类 + 带宽限制设计清单 |

#### ⚔️ 辩论与审议

| 论文 | 来源 | 关键发现 | 引擎映射 |
|------|------|---------|---------|
| **DCI: From Debate to Deliberation** (Prakash et al.) | arXiv:2603.11781, Mar 2026 | 14种类型化认知行为 + 共享工作区 + **少数报告**; 非常规任务强 | → Phase 4 改进: 保留异议分布而非仅点估计 |
| **DOWN: Debate Only When Necessary** (Eo et al.) | arXiv:2504.05047, May 2025 | 置信度门控辩论 → **~6x 效率提升**; 减少不必要辩论的错误传播 | → **认知不确定性门控**: 仅在 entropy > 阈值时启动辩论 |
| **Adversarial Persuasion in Multi-Agent Debate** (Kraidia et al.) | *Sci Rep* 16:11640, Apr 2026 | 单个策略性 Agent 可降低准确率 10-40%; BoN/RAG 可能放大说服力 | → **拜占庭容错聚合**: 信任分数 + 独立证据验证 |
| **OPTAGENT: Verbal RL for Multi-Agent** (Bi et al.) | arXiv:2510.18032, Oct 2025 | 言语 RL 重塑协作图 + 奖励沟通质量 | → 学习哪些交互边改善 Brier Score |

#### 📐 校准与聚合

| 论文 | 来源 | 关键发现 | 引擎映射 |
|------|------|---------|---------|
| **Calib-n: Multi-LLM Calibration** (Xia et al.) | ACL 2025 | 辅助模型用 **跨模型一致性** 聚合; focal/AUC 损失 | → M7 最终概率消费跨模型一致性特征 |
| **SCA/MACE: Semantic Confidence Aggregation** (Ni et al.) | arXiv:2602.07842, Feb 2026 | 多正确答案下语义等价类聚合修复系统性欠置信 | → 开放式/多模态预测目标的语义等价类处理 |
| **ORCA: Online Reasoning Calibration** (Zhou et al.) | arXiv:2604.01170, Apr 2026 | 保形推理 + 测试时校准; 强 token 节省 + 分布偏移下风险控制 | → **分布自由不确定性集合** 附加到群体预测 |
| **CAPO: Calibration-Aware Policy Optimization** (Wang et al.) | arXiv:2604.12632, Apr 2026 | 不确定性感知优势修复 GRPO 过度自信; ~15% 校准提升 + 弃权 | → 训练专家预测器时优先校准感知 RL 目标 |

#### 🐝 群体基准与大规模社会模拟

| 论文 | 来源 | 关键发现 | 引擎映射 |
|------|------|---------|---------|
| **SwarmBench** (Ruan et al.) | arXiv:2505.04364, Oct 2025 | 去中心化2D任务(追逐/同步/觅食/集群); LLM在局部感知下远程规划有限 | → 压力测试: 局部可观测性下的预测回路 |
| **Society of HiveMind** (Mamie & Rao) | arXiv:2503.05473, Mar 2025 | 进化式多基础模型编排; 推理重任务获益,纯知识任务无收益 | → 推理重预测走多模型群体, 知识轻路径走低成本 |
| **AgentSociety: 10k+ Agents** (Piao et al.) | arXiv:2502.08691, Apr 2026 | 10k+ Agent, ~5M 交互; 政策/社会现象(极化/冲击/UBI) | → 反事实社会模拟生成场景分布 |
| **Self-MoA / Mixture-of-Agents** (Li et al. / Wang et al.) | arXiv:2502.00674 / ICLR 2025 | 单强模型自集成常胜异构 MoA; 先保质量再加多样性 | → 默认质量加权自集成, 仅在误差独立性有证据时混合模型 |
| **HiddenBench** (Li et al.) | arXiv:2505.11556, Feb 2026 | 分布式信息下多 Agent 仅 ~30.1% vs 单 Agent 全信息 ~80.7%; **潜在信息不对称** | → 强制披露轮 + "未知于群体"的显式槽位 |

#### 🎯 超对齐与预测市场

| 论文 | 来源 | 关键发现 | 引擎映射 |
|------|------|---------|---------|
| **Superalignment with Dynamic Human Values** (Mai et al.) | arXiv:2503.13621, Mar 2025 | 对齐分解子任务使人类可审计 → 对齐完整解 | → 规范性预测分解为人类可审计子步骤 |
| **PredIQt on Polymarket** | 实际运营, Jan 2026 | AI Agent 群体在预测市场实盘竞争 | → M7 预测市场基准: 用 PnL/Brier 校准群体策略 |

---

## 9. 文件结构

```
pkg/swarm_intel/
├── engine.go          // SwarmIntelligenceEngine 核心
├── prediction.go      // 预测融合逻辑 (对数意见池 + 校准)
├── boids.go           // Boids 协调规则
├── pheromone.go       // 信素记忆
├── simulation.go      // 场景模拟 (social/game/montecarlo)
└── types.go           // 核心类型定义

pkg/agent/
├── workflow.go        // + predict workflow
└── teams.go           // + executePrediction

pkg/commands/
└── team_commands.go   // + /predict, /simulate

docs/
└── swarm-intelligence-engine.md  // 本文档
```

---

## 10. M6: 信素持久化 + 历史学习

### 10.1 设计目标

将内存中的信素轨迹和预测历史持久化到磁盘, 实现跨会话的经验积累和"站在历史上预测"。

### 10.2 持久化存储

```go
// PheromoneStore 信素持久化层
type PheromoneStore struct {
    dir    string           // ~/.claude-go/swarm_intel/pheromones/
    index  map[string]int64 // hypothesis → file offset (内存索引)
}
```

存储格式: JSONL (每行一条信素快照), 按主题分文件:

```
~/.claude-go/swarm_intel/
├── pheromones/
│   ├── tech.jsonl      // 科技领域信素
│   ├── finance.jsonl   // 金融领域信素
│   └── general.jsonl   // 通用信素
├── predictions/
│   ├── history.jsonl   // 所有预测历史
│   └── calibration.jsonl // 校准记录
└── learning/
    ├── agent_weights.json   // Agent 权重 (基于历史 Brier Score)
    └── reasoning_bank.jsonl // 成功推理路径库
```

### 10.3 DTI 门控 (源自 arXiv:2604.02674)

检测"知识精英"垄断 — 当少数 Agent 主导预测时触发强制交叉检查:

```go
type DTIController struct {
    coordinationHist []float64 // 协调级联历史
    integrationHist  []float64 // 整合强度历史
    threshold        float64   // DTI 触发阈值 (默认 0.7)
}

func (d *DTIController) ShouldTrigger(agentPredictions []AgentPrediction) bool
```

### 10.4 推理路径复用 (ReasoningBank)

当新问题与历史问题的语义相似度 > 0.7 时, 自动加载历史推理路径作为先验:

```go
type ReasoningEntry struct {
    Question      string    `json:"question"`
    Reasoning     string    `json:"reasoning"`
    BrierScore    float64   `json:"brier_score"`
    Confidence    float64   `json:"confidence"`
    CreatedAt     time.Time `json:"created_at"`
}
```

---

## 11. M7: 在线校准 + 预测市场 + Bandit 路由

### 11.1 在线校准 (源自 ORCA arXiv:2604.01170)

在测试时动态校准群体预测的不确定性:

```go
type ConformalCalibrator struct {
    alpha      float64              // 显著性水平 (默认 0.05)
    scoreHist  []float64            // 保形分数历史
    quantile   float64              // 当前分位数
}

func (c *ConformalCalibrator) CalibrateSet(pred *FusedPrediction) ConfidenceSet
```

### 11.2 认知不确定性门控 (源自 DOWN arXiv:2504.05047)

仅在认知不确定性高时启动昂贵的辩论轮:

```go
type DebateGate struct {
    entropyThreshold float64 // Shannon 熵阈值 (默认 0.8)
}

func (g *DebateGate) ShouldDebate(predictions []AgentPrediction) bool {
    // 计算预测分布的 Shannon 熵
    // 熵 > threshold → 辩论; 否则直接融合 (节省 ~6x token)
}
```

### 11.3 拜占庭容错聚合 (源自 Sci Rep 16:11640)

防止策略性 Agent 操纵群体预测:

```go
type ByzantineFuser struct {
    trustScores map[string]float64 // Agent → 信任分数 (0-1)
    trimRatio   float64            // 修剪比例 (默认 0.2, 去掉最极端的 20%)
}

func (bf *ByzantineFuser) TrimmedFuse(preds []AgentPrediction) *FusedPrediction
```

### 11.4 Bandit 路由 (源自 REDEREF arXiv:2603.13256)

用 Thompson Sampling 动态路由预测任务到最合适的 Agent:

```go
type BanditRouter struct {
    alphas map[string]float64 // Agent → Beta 分布 alpha (成功次数)
    betas  map[string]float64 // Agent → Beta 分布 beta (失败次数)
}

func (br *BanditRouter) SelectAgents(task string, k int) []string {
    // Thompson Sampling: 从每个 Agent 的 Beta(α,β) 采样, 选 top-k
}
```

### 11.5 内部预测市场

Agent 用虚拟货币对预测下注, 市场价格反映集体信念:

```go
type PredictionMarket struct {
    liquidity float64               // LMSR 流动性参数
    shares    map[string]float64    // outcome → 已购买份额
    budgets   map[string]float64    // agent → 剩余预算
}

func (pm *PredictionMarket) Price(outcome string) float64 // LMSR 定价
func (pm *PredictionMarket) Buy(agent, outcome string, amount float64) error
```

---

## 12. 里程碑

| 阶段 | 内容 | 状态 |
|------|------|------|
| M1 | 核心类型定义 + 引擎骨架 | ✅ 已完成 |
| M2 | 5 阶段流水线 + Boids 协调 | ✅ 已完成 |
| M3 | 贝叶斯融合 + 校准 | ✅ 已完成 |
| M4 | predict workflow + REPL 集成 | ✅ 已完成 |
| M5 | 场景模拟 (social/game/montecarlo) | ✅ 已完成 |
| M5b | 飞书 /predict /simulate 集成 | ✅ 已完成 |
| M6 | 信素持久化 + 历史学习 + DTI 门控 | ✅ 已完成 |
| M7 | 在线校准 + 预测市场 + Bandit 路由 | ✅ 已完成 |
| M8 | 扩展场景 (危机推演/组织模拟/创意涌现等) | ✅ 已完成 |
| M9 | 可观测性指标体系 + MiroFish 对比 | ✅ 已完成 |
