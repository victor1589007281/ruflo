# claude-go 金融交易决策系统 v2 — 设计文档

> **版本**: 2.0 | **日期**: 2026-04-17
> **对标**: TradingAgents (arXiv:2412.20138), FinDebate, Trading-R1, QuantAgent

---

## 1. 现状差距分析

### claude-go 现有金融团队 vs TradingAgents

| 维度 | claude-go finance (v1) | TradingAgents | 差距 |
|:---|:---|:---|:---|
| **架构** | 固定 6 阶段 pipeline | LangGraph 状态机, 条件跳转 | ❌ 无辩论/对抗机制 |
| **分析师** | 4 个并行分析师 (技术/情绪/财务/新闻) | 4 个分析师 + 工具调用循环 | ≈ 对齐 |
| **研究辩论** | ❌ 无 | Bull/Bear 多轮对抗辩论 + Research Manager 裁决 | ❌ **关键缺失** |
| **交易决策** | risk-assessor → trade-advisor (2 步) | Trader → Aggressive/Conservative/Neutral 三方风险辩论 → PM 裁决 | ❌ **关键缺失** |
| **信号输出** | 自然语言评级 (无结构化) | SignalProcessor → BUY/OVERWEIGHT/HOLD/UNDERWEIGHT/SELL | ❌ 无结构化信号 |
| **记忆系统** | Blackboard (当次) | BM25 历史记忆 + Reflection | ❌ 无跨次记忆 |
| **数据源** | WebSearch (间接) | yfinance + Alpha Vantage (结构化 OHLCV/财报/新闻) | ⚠️ 依赖搜索引擎 |
| **风险管理** | 提示词层面 (无代码约束) | 三方辩论 (无代码约束) | 双方均弱 |
| **回测** | ❌ 无 | ❌ 声明有但未实现 | 双方均无 |

### 核心差距 (需补齐)

1. **Bull/Bear 对抗辩论** — TradingAgents 的核心创新, 当前完全缺失
2. **三方风险辩论** (激进/保守/中性) — 风险评估不应是单一视角
3. **结构化信号提取** — 最终输出应为机器可读的交易信号
4. **跨次反思记忆** — 复用历史交易的经验教训
5. **量化风控层** — 仓位管理、止损、风险预算 (双方均缺, 需超越 TradingAgents)

---

## 2. 系统架构

```
                        ┌──────────────────────────────────┐
                        │     Financial Trading System v2   │
                        └──────────────────────────────────┘
                                       │
                ┌──────────────────────┼──────────────────────┐
                │                      │                      │
        ┌───────▼──────┐    ┌─────────▼────────┐   ┌────────▼────────┐
        │ Analysis Tier │    │  Debate Tier      │   │ Decision Tier   │
        │ (4 并行分析师) │    │  (多轮对抗辩论)   │   │ (风控+最终决策)  │
        └───────┬──────┘    └─────────┬────────┘   └────────┬────────┘
                │                      │                      │
     ┌──────┬──────┬──────┐   ┌───────┴───────┐    ┌────────┴────────┐
     │Tech  │Sent  │Fund  │   │Bull    Bear   │    │Aggr  Cons  Neut │
     │Analyst│Analyst│Analyst│  │Researcher     │    │Risk  Risk  Risk │
     │      │      │      │   │  ↕ N轮辩论     │    │  ↕ N轮辩论      │
     │News  │      │      │   │Research Manager│    │Portfolio Manager│
     │Analyst│      │      │   │(裁决)         │    │(最终裁决)       │
     └──────┴──────┴──────┘   └───────────────┘    └─────────────────┘
                │                      │                      │
                └──────────────────────┼──────────────────────┘
                                       │
                               ┌───────▼───────┐
                               │Signal Extractor│
                               │(结构化信号)     │
                               └───────┬───────┘
                                       │
                               ┌───────▼───────┐
                               │Risk Guardrail  │
                               │(量化风控层)     │
                               └───────────────┘
```

### 三层架构

| 层 | 角色数 | 模式 | 来源 |
|:---|:---:|:---|:---|
| **分析层** | 4 | 并行执行, 每个分析师独立产出报告 | claude-go v1 + TradingAgents |
| **辩论层** | 3 | Bull/Bear 多轮对抗 → Research Manager 裁决 | **TradingAgents 核心创新** |
| **决策层** | 4 | Trader 提案 → 激进/保守/中性三方辩论 → PM 裁决 | **TradingAgents 核心创新** |
| **信号层** | 1 | 从 PM 输出提取结构化信号 | TradingAgents SignalProcessor |
| **风控层** | 代码 | 仓位限制/止损/风险预算 (确定性规则) | **超越 TradingAgents** |

---

## 3. Agent 角色定义

### 3.1 分析层 (Analysis Tier)

| Agent | 角色 | 输入 | 输出 |
|:---|:---|:---|:---|
| **Market Analyst** | 技术分析师 | 标的名称 | 技术指标分析 + 评分 |
| **Sentiment Analyst** | 情绪分析师 | 标的名称 | 市场情绪 + 资金流向 + 评分 |
| **Fundamentals Analyst** | 财务分析师 | 标的名称 | 财报解读 + 估值 + 评分 |
| **News Analyst** | 新闻追踪师 | 标的名称 | 事件驱动 + 催化剂 + 评分 |

### 3.2 辩论层 (Research Debate Tier) — **新增**

| Agent | 角色 | 机制 |
|:---|:---|:---|
| **Bull Researcher** | 多头研究员 | 论证买入理由, 反驳空头观点 |
| **Bear Researcher** | 空头研究员 | 论证卖出理由, 反驳多头观点 |
| **Research Manager** | 研究主管 | 裁决辩论, 输出投资计划 |

辩论流程: Bull → Bear → Bull → Bear → ... → Research Manager (默认 2 轮)

### 3.3 决策层 (Risk Debate + Decision Tier) — **新增**

| Agent | 角色 | 视角 |
|:---|:---|:---|
| **Trader** | 交易员 | 基于研究报告制定交易方案 |
| **Aggressive Analyst** | 激进风控 | 高收益/高风险视角 |
| **Conservative Analyst** | 保守风控 | 资本保全/低风险视角 |
| **Neutral Analyst** | 中性风控 | 平衡两方, 挑战偏见 |
| **Portfolio Manager** | 投资组合经理 | 最终裁决: 交易信号 + 仓位 |

风险辩论流程: Aggressive → Conservative → Neutral → ... → PM (默认 2 轮)

### 3.4 信号提取 (Signal Extraction)

从 Portfolio Manager 输出中提取:
- **评级**: `BUY | OVERWEIGHT | HOLD | UNDERWEIGHT | SELL`
- **置信度**: `0.0 - 1.0`
- **仓位建议**: `0% - 100%`
- **止损位/目标价** (若有)

---

## 4. 核心算法

### 4.1 多轮对抗辩论 (参考 TradingAgents + FinDebate)

```
for round = 1 to max_debate_rounds:
    if round is odd:
        bull_argument = LLM(bull_prompt + bear_last_argument)
    else:
        bear_argument = LLM(bear_prompt + bull_last_argument)

manager_verdict = LLM(all_analyst_reports + debate_history)
```

**改进 (超越 TradingAgents)**:
- 每轮辩论要求引用具体数据/事实 (参考 Trading-R1 的 evidence-linked thesis)
- Research Manager 使用结构化裁决模板 (不是自由文本)

### 4.2 三方风险辩论 (参考 TradingAgents)

```
for round = 1 to max_risk_rounds:
    aggressive = LLM(trader_plan + prev_discussion, perspective="aggressive")
    conservative = LLM(trader_plan + prev_discussion, perspective="conservative")
    neutral = LLM(trader_plan + prev_discussion, perspective="neutral")

pm_decision = LLM(all_risk_opinions + trader_plan)
```

### 4.3 量化风控层 (超越 TradingAgents)

| 规则 | 参数 | 说明 |
|:---|:---|:---|
| 单一标的仓位上限 | 30% | 防止集中风险 |
| 止损幅度 | 由 PM 指定, 最大 15% | 硬性止损 |
| 盈亏比最低要求 | 1.5:1 | 不满足则降级为 HOLD |
| 置信度阈值 | < 0.6 → 降级 HOLD | 低置信度不操作 |

### 4.4 反思记忆 (参考 TradingAgents Reflector + BM25)

分析完成后, 将 (标的, 日期, 分析摘要, 最终信号, 实际表现) 存入记忆。
下次分析同标的时, 检索历史记忆注入分析师 prompt。

---

## 5. 参考文献

| 论文 | 贡献 | 引用 |
|:---|:---|:---|
| TradingAgents | 多 Agent 交易桌架构, Bull/Bear 辩论 | arXiv:2412.20138 |
| Trading-R1 | RL + 结构化推理 + 波动率感知 | arXiv:2509.11420 |
| FinDebate | 安全辩论协议, 校准置信度 | arXiv:2509.17395 |
| QuantAgent | 短周期多 Agent 价格驱动 | arXiv:2509.09995 |
| Fin-R1 | 金融推理强化学习 | arXiv:2503.16252 |
| FinRL-DeepSeek | CVaR 风险感知 PPO + LLM 信号 | arXiv:2502.07393 |
| LLM Black-Litterman | LLM 预测 → B-L 优化器 | arXiv:2504.14345 |
| RLFKV | 金融 RAG 幻觉缓解 | arXiv:2602.05723 |

---

## 6. 实现计划

### 工作流名: `trading-v2`

**阶段 (11 个 Agent, 5 层)**:

```
Layer 1 (并行): market-analyst, sentiment-analyst, fundamentals-analyst, news-analyst
Layer 2 (辩论): bull-researcher ↔ bear-researcher → research-manager
Layer 3 (决策): trader
Layer 4 (风险辩论): aggressive-risk ↔ conservative-risk ↔ neutral-risk → portfolio-manager
Layer 5 (提取): signal-extractor
```

### 新增文件

| 文件 | 内容 |
|:---|:---|
| `pkg/agent/workflow_trading_v2.go` | trading-v2 工作流定义 + 辩论引擎 |
| `pkg/agent/roles_trading_v2.go` | 11 个 Agent 角色定义 |
| `pkg/agent/signal_extractor.go` | 结构化信号提取 + 量化风控 |

### 复用组件

- `WorkflowExecutor` (已有)
- `Coordinator` (已有, 含检查点+重试)
- `ResilientCaller` (API 层, 已加 429 重试)
- `Blackboard` (Agent 间通信)
