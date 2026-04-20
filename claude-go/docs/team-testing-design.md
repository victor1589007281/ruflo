# 研发测试团队设计方案

> **版本**: 1.0 | **日期**: 2026-04-21
> **定位**: 专业集成测试团队, 自主制定方案/拆解/执行/汇总, 引入混沌测试等新理念

---

## 一、调研来源

| 领域 | 参考 | 核心启发 |
|:----:|:----:|:--------:|
| **LLM 测试生成** | TestCase-Eval(ACL'25), TestEval(NAACL'25) | 覆盖率/变异/oracle 质量评估 |
| **混沌工程** | Chaos Engineering (Netflix), LitmusChaos | 故障注入验证系统韧性 |
| **变异测试** | PIT, go-mutesting | 用变异分数衡量测试强度 |
| **属性测试** | QuickCheck, Hypothesis, testing/quick | LLM 提出属性, PBT 引擎验证 |
| **工具** | Qodo Gen, CodiumAI | IDE 级测试生成 + 质量门禁 |

---

## 二、设计原则

```
┌─────────────────────────────────────────────────────────────┐
│  P1: 自主规划 — LLM 分析代码自行制定测试方案                   │
│  P2: 分层测试 — 单元 → 集成 → 属性 → 混沌, 逐层升级            │
│  P3: 混沌注入 — 模拟 LLM 故障/网络/超时/数据异常               │
│  P4: 变异驱动 — 用变异测试衡量测试质量, 杀死率 > 80%           │
│  P5: 测试即文档 — 测试报告含覆盖率/变异率/风险矩阵             │
└─────────────────────────────────────────────────────────────┘
```

---

## 三、工作流 DAG

```
AnalyzeCode ──▶ RiskModel ──▶ TestPlanDecompose ──┬──▶ GenUnitTests
                                                   ├──▶ GenIntegrationTests
                                                   ├──▶ GenPropertyTests
                                                   └──▶ GenChaosScenarios
                                                           │
                                                     RunAllTests ──▶ FlakeDetect ──▶ MutationAnalysis ──▶ QualityGate ──▶ ReportAssembly
```

### 阶段详解

| # | 阶段 | 角色 | 说明 |
|:-:|:-----|:-----|:-----|
| 1 | **AnalyzeCode** | analyst | 分析代码结构、依赖、复杂度 |
| 2 | **RiskModel** | risk_assessor | 识别高风险模块 (变更频率/复杂度/关键路径) |
| 3 | **TestPlanDecompose** | planner | 基于风险生成测试矩阵, 拆解为具体测试任务 |
| 4 | **GenUnitTests** | unit_gen | 生成表驱动单元测试 |
| 5 | **GenIntegrationTests** | integration_gen | 生成跨模块集成测试 |
| 6 | **GenPropertyTests** | property_gen | 提出不变量属性, 生成属性测试 |
| 7 | **GenChaosScenarios** | chaos_engineer | 设计混沌场景: 超时/限流/OOM/数据损坏 |
| 8 | **RunAllTests** | runner | 沙箱执行, 收集结果和覆盖率 |
| 9 | **FlakeDetect** | flake_detector | 多次运行检测 flaky 测试 |
| 10 | **MutationAnalysis** | mutation_analyst | 变异测试评估测试强度 |
| 11 | **QualityGate** | gate_keeper | 门禁: 覆盖率 ≥ 80%, 变异杀死率 ≥ 70% |
| 12 | **ReportAssembly** | reporter | 汇总输出完整测试报告 |

### 混沌测试场景

| 场景 | 注入方式 | 验证目标 |
|:----:|:--------:|:--------:|
| LLM 超时 | 延迟注入 | 超时处理和重试 |
| LLM 限流 (429) | 错误注入 | 背压和退避 |
| 网络断连 | 连接切断 | 优雅降级 |
| 数据损坏 | 乱序/截断 | 输入校验 |
| OOM | 大响应注入 | 内存限制 |

---

## 四、测试报告格式

```json
{
  "summary": {
    "total_tests": 142,
    "passed": 135,
    "failed": 5,
    "flaky": 2,
    "coverage": "83.5%",
    "mutation_kill_rate": "76.2%",
    "quality_gate": "PASSED"
  },
  "risk_matrix": [...],
  "failures": [...],
  "recommendations": [...]
}
```

---

## 五、编排引擎集成

- 4 路测试生成并行 (DAG 自动调度)
- `CompositeRunner` 实现变异循环 (生成变异 → 运行测试 → 统计)
- 背压控制 LLM 并发调用
- 检查点: 每类测试完成后保存, 支持断点续跑
