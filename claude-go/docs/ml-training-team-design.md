# ML 训练 & 大模型微调团队设计方案

> **版本**: 1.0 | **日期**: 2026-04-20
> **工作流名**: `ml-training` / `ml` / `finetune`

---

## 一、业界调研汇总

| **方案**                     | **来源**                  | **核心思想**                    | **借鉴点**               |
|:----------------------------:|:-------------------------:|:-------------------------------:|:------------------------:|
| AutoML-Agent                 | PMLR v267                 | 多智能体全流程 AutoML           | 角色分工: 数据/模型/调优  |
| Centaur 混合 HPO             | arXiv:2603.24647          | 经典优化器+LLM 联合超参调优     | 混合搜索比纯LLM更稳      |
| FeRG-LLM                    | NAACL 2025                | LLM 特征工程 + DPO             | 表格数据特征生成          |
| DSPy                         | Stanford                  | 签名+模块+优化器自改进          | 声明式 ML 管线           |
| On-Policy Distillation       | arXiv:2604.00626          | 在线策略蒸馏                    | 蒸馏训练范式              |
| LoRA/QLoRA/DoRA              | HuggingFace PEFT          | 参数高效微调                    | 微调实现标准              |
| GaLore                       | ICML 2024                 | 梯度低秩投影省显存              | 训练优化                  |
| MLflow GenAI + W&B Weave     | 社区                      | Agent 轨迹追踪与评估            | 可观测性                  |

## 二、工作流设计

```
┌──────────────────────────────────────────────────────────────────┐
│                   ML Training Pipeline                           │
│                                                                  │
│  ┌──────────┐   ┌───────────┐   ┌──────────┐   ┌────────────┐  │
│  │ 数据分析  │──▶│ 特征工程  │──▶│ 模型设计 │──▶│ 训练执行   │  │
│  │ (analyst) │   │(engineer) │   │(architect)│   │(trainer)   │  │
│  └──────────┘   └───────────┘   └──────────┘   └────────────┘  │
│                                                       │          │
│                                                       ▼          │
│  ┌──────────────┐   ┌──────────────┐   ┌────────────────────┐  │
│  │ 蒸馏/微调    │◀──│ 评估优化     │◀──│ 实验结果分析       │  │
│  │(distiller)   │   │(optimizer)   │   │(evaluator)         │  │
│  └──────────────┘   └──────────────┘   └────────────────────┘  │
│                              │                                   │
│                              ▼                                   │
│                     ┌────────────────┐                          │
│                     │ 综合报告       │                          │
│                     │(synthesizer)   │                          │
│                     └────────────────┘                          │
└──────────────────────────────────────────────────────────────────┘
```

### 角色定义

| **角色**        | **职责**                                          |
|:---------------:|:-------------------------------------------------:|
| data-analyst    | 数据质量分析、统计特征、缺失值/异常值检测          |
| ml-engineer     | 特征工程、特征选择、数据预处理管线设计              |
| ml-architect    | 模型选择、架构设计、超参搜索策略                    |
| ml-trainer      | 训练脚本编写、分布式训练配置、实验执行              |
| ml-evaluator    | 模型评估、指标分析、AB 测试设计                    |
| ml-optimizer    | 性能优化、显存优化、推理加速                       |
| distiller       | 知识蒸馏、模型压缩、LoRA/QLoRA 微调               |
| synthesizer     | 综合报告、实验对比、最终建议                       |

### 阶段设计

| **#** | **阶段**          | **Mode**  | **依赖**            |
|:-----:|:-----------------:|:---------:|:-------------------:|
| 1     | data-analysis     | parallel  | -                   |
| 2     | feature-engineering| parallel  | -                   |
| 3     | model-design      | serial    | data-analysis       |
| 4     | training          | serial    | feature-engineering, model-design |
| 5     | evaluation        | serial    | training            |
| 6     | optimization      | serial    | evaluation          |
| 7     | distillation      | serial    | optimization        |
| 8     | synthesis         | serial    | distillation        |

## 三、关键特性

1. **混合 HPO**: 训练阶段建议 TPE/CMA-ES 搜索 + LLM 分析失败原因
2. **代码优先**: 所有阶段必须输出可执行的 Python 代码
3. **实验追踪**: 建议集成 MLflow/W&B 追踪模板
4. **微调管线**: 支持 LoRA/QLoRA/full-finetune 三种路径
