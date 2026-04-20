# 代码审查团队设计方案

> **版本**: 1.0 | **日期**: 2026-04-21
> **定位**: 专业 AI 代码审查团队, 支持 MR/PR/仓库级别的深度审查

---

## 一、调研来源

| 领域 | 参考 | 核心启发 |
|:----:|:----:|:--------:|
| **LLM审查** | CodeReviewer(Microsoft), CodeAgent(EMNLP'24), CodeReviewQA(ACL'25) | 分解审查推理, 多角色协作 |
| **商业工具** | CodeRabbit, Qodo/PR-Agent, Sourcery, Codacy AI | PR Bot, 策略包, 严重级别 |
| **对抗审查** | BlueCodeAgent(MSR), Skeptical Reviewer | Red/Blue 双视角, 质疑假设 |
| **测试驱动** | CodiumAI, TestPilot | 生成测试验证代码正确性 |
| **厂商** | Claude, GPT, Gemini, Kimi, GLM, DeepSeek, MiniMax | 长上下文, function calling, 结构化输出 |

---

## 二、设计原则

```
┌─────────────────────────────────────────────────────────────┐
│  P1: 多角色协作 — 静态分析 + 逻辑审查 + 安全审查 + 对抗质疑  │
│  P2: 测试驱动审查 — 为变更生成测试, 用测试结果作为客观证据    │
│  P3: 结构化输出 — 统一 Finding 格式: 严重级别+类别+证据+建议  │
│  P4: 对抗循环 — Skeptical Reviewer 质疑发现, 减少误报          │
│  P5: 编排引擎驱动 — 使用 pkg/orchestrator 的 DAG 调度          │
└─────────────────────────────────────────────────────────────┘
```

---

## 三、工作流 DAG

```
IngestCode ──┬──▶ StaticAnalysis ──┐
             │                      ├──▶ MergeContext ──▶ SpecialistReview ──┬──▶ AdversarialPair ──▶ TestDrivenVerify ──▶ ReportAssembly
             └──▶ RetrieveContext ──┘                    (并行4路)           │
                                                         ├─ Logic           │
                                                         ├─ Security        │
                                                         ├─ Performance     │
                                                         └─ Style           │
```

### 阶段详解

| # | 阶段 | 角色 | 说明 |
|:-:|:-----|:-----|:-----|
| 1 | **IngestCode** | ingester | 解析 diff/文件, 提取变更范围和上下文 |
| 2 | **StaticAnalysis** | analyzer | 运行 go vet/staticcheck/gosec 等静态工具 |
| 3 | **RetrieveContext** | retriever | 检索项目规范/ADR/历史审查记录 |
| 4 | **MergeContext** | merger | 合并静态分析结果+上下文为审查输入 |
| 5 | **SpecialistReview** | reviewer(×4) | 4 路并行: 逻辑/安全/性能/风格 |
| 6 | **AdversarialPair** | red+blue | Red 质疑发现, Blue 辩护, 3 轮对抗 |
| 7 | **TestDrivenVerify** | tester | 为关键发现生成测试代码并执行验证 |
| 8 | **ReportAssembly** | reporter | 汇总、去重、排序, 输出结构化审查报告 |

### Finding 输出格式

```json
{
  "severity": "high",
  "category": "security",
  "file": "pkg/api/handler.go",
  "line": 42,
  "title": "SQL 注入风险",
  "description": "用户输入直接拼接到 SQL 语句",
  "suggestion": "使用参数化查询 db.Query(\"SELECT ... WHERE id = ?\", id)",
  "confidence": 0.92,
  "verified_by_test": true,
  "evidence": "生成的测试 TestSQLInjection 验证了该漏洞"
}
```

---

## 四、客户端接口

### /review 命令
```
/review <path_or_url>         — 审查指定路径或 Git URL
/review --diff <branch>       — 审查当前分支与指定分支的差异
/review --severity high       — 仅显示高严重级别
/review --focus security      — 聚焦安全审查
```

### 团队启动方式
```
/go code-review <objective>   — 标准团队启动
```

---

## 五、编排引擎集成

使用 `pkg/orchestrator.Engine` 驱动:
- 每个阶段映射为 `Task`, 通过 `FuncRunner` 封装 LLM 调用
- 4 路 Specialist 并行由 DAG 自动调度
- 对抗轮次使用 `CompositeRunner` + `TerminationPolicy`
- 失败分治: 静态分析超时 → 瞬态重试, LLM 拒绝 → 永久失败

---

## 六、与现有系统对比

| 维度 | 传统 code review | 本方案 |
|:----:|:----------------:|:------:|
| 审查深度 | 单 LLM 提示 | 多角色 + 对抗 + 测试验证 |
| 误报率 | 高 (单视角) | 低 (Skeptical Reviewer + 测试验证) |
| 覆盖面 | 通用 | 逻辑+安全+性能+风格 4 维 |
| 可扩展 | 固定 | Filter/Score 插件式 |
| 调度 | 串行 | DAG 并行 + 背压控制 |
