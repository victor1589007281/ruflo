# 前沿编程模型论文参考

## 核心论文列表

| 模型/论文 | 标题 | URL | 日期 |
|:---|:---|:---|:---|
| **GLM-5** | GLM-5: from Vibe Coding to Agentic Engineering | https://arxiv.org/abs/2602.15763 | 2026-02-17 |
| **Kimi K2.5** | Kimi K2.5: Visual Agentic Intelligence | https://arxiv.org/abs/2602.02276 | 2026-02-02 |
| **Kimi K2 Thinking** | Introducing Kimi K2 Thinking | https://medium.com/@kimi_moonshot/introducing-kimi-k2-thinking-a61c95f6e59a | 2025-11-10 |
| **MiniMax M2.7** | MiniMax M2.7 Release | https://www.minimax.io/news/minimax-m27-en | 2026 |
| **DeepSeek-Prover-V2** | RL for Subgoal Decomposition | https://arxiv.org/abs/2504.21801 | 2025-04-30 |
| **DeepSeek-V3** | DeepSeek-V3 Technical Report | https://arxiv.org/abs/2412.19437 | 2024-12 |
| **DeepSeek-R1** | Incentivizing Reasoning via RL | https://arxiv.org/abs/2501.12948 | 2025-01 |
| **MAgICoRe** | Multi-Agent Iterative Coarse-to-Fine Refinement | https://aclanthology.org/2025.emnlp-main.1660/ | EMNLP 2025 |
| **CaRT** | Teaching LLM Agents to Know When They Know Enough | https://arxiv.org/abs/2510.08517 | 2025-10-09 |
| **mHC** | Manifold-Constrained Hyper-Connections | https://arxiv.org/abs/2512.24880 | 2025-12-31 |

## 各模型核心技术

### GLM 5.1 — 阶梯式优化 + 异步 Agent RL
- **DSA**: 长上下文保真的架构
- **异步 RL**: 生成与训练解耦, rollout workers + 经验回放
- **Agentic 信用分配**: 按 tool span 分段归因奖励
- **实验→分析→优化 闭环**: benchmark-driven 瓶颈识别

### Kimi K2.5 — Agent Swarm + PARL
- **Agent Swarm**: 自主并行编排, 任务分解+并发执行, 延迟降低 4.5×
- **PARL 奖励公式**: R_t = α_t·r_para + β_t·r_finish + γ_t·r_task (α↓, γ↑)
- **防伪并行**: 要求每个 worker 产出非平凡 artifact (diff/test/file)
- **深度思考**: 200-300 tool 调用, 假设→证据→验证 循环
- **Heavy Mode**: 8 路并行 rollout + 反思聚合 (best-of-N + meta-judge)
- **溢出策略**: 上下文溢出时隐藏旧 tool 输出

### MiniMax M2.7 — 自进化循环
- **短期记忆工件**: 每步产出结构化 markdown 日志
- **失败轨迹挖掘**: {症状, 根因假设, 补丁, 指标前后} 元组
- **Keep/Revert 决策**: ΔMetric ≥ τ ∧ 无回归 → accept; else revert + 反例入库
- **自反馈**: 自评→自优化→下轮计划

### DeepSeek Prover-V2 — 子目标分解 + 形式验证
- **大模型分解子目标**: 递归 pipeline
- **小模型证明子目标**: CoT + 形式证明
- **二值奖励**: verifier ∈ {0, 1} (证明检查/单元测试/CI)
- **稀疏终端奖励 + shaped 中间奖励** (子目标完成)

### MAgICoRe — 难易分流 + 多角色迭代精炼
- **Hard/Easy 分类器**: c(x) 基于熵/一致性/RM 不确定性
- **Easy**: self-consistency / majority vote, 小 k
- **Hard**: {critic, fixer, verifier} 角色, 迭代至 step RM 饱和
- **核心**: 用 <50% 算力击败强基线

### CaRT — 何时停止搜集信息
- **反事实轨迹对**: (τ, stop) vs (τ', continue), 最小编辑
- **训练**: SFT/偏好优化, 预测 stop + 下一步期望效用
