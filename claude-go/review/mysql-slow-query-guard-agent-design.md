# MySQL 慢查询智能守卫 Agent 设计方案

> **版本**: v1.0  
> **日期**: 2026-05-21  
> **目标**: 解决 AI 生成 SQL 导致的 MySQL 慢查询泛滥问题，结合 `claude-go` 的 Agent/Orchestrator/Swarm 能力构建 proactive 的数据库查询守卫系统。

---

## 1. 执行摘要

AI 辅助编程工具（Copilot、Claude Code 等）极大提升了业务开发效率，但也带来了**副作用**：大量未经 DBA 审核的 SQL 被直接投入生产。这些 SQL 往往存在以下问题：

- `SELECT *` 全表扫描
- 缺失索引或错误使用索引
- `LIKE '%xxx'` 前缀模糊匹配
- 深度嵌套子查询 / 笛卡尔积
- 大表 `OFFSET` 深分页
- `JOIN` 顺序不合理或隐式转换

**结果**：数据库 CPU/IOPS 飙升、连接池耗尽、业务响应延迟，最终形成 "慢查询 → 重试 → 更慢" 的恶性循环。

本方案提出 **"Query Guard Agent"** —— 一个基于 `claude-go` 构建的多 Agent 协同系统，采用 **"事前拦截 + 事中熔断 + 事后根治"** 的三层防御模型，将传统 DBA 的被动救火转变为主动防御。

---

## 2. 问题分析与根因

### 2.1 AI 生成 SQL 的典型慢查询模式

| 模式 | 示例 | 危害 |
|------|------|------|
| **全表扫描** | `SELECT * FROM orders WHERE YEAR(created_at)=2025` | 函数导致索引失效，扫描数百万行 |
| **深分页** | `SELECT * FROM logs ORDER BY id LIMIT 100000, 20` | 排序 + 跳过大量行，IO 爆炸 |
| **无索引连接** | `SELECT * FROM A JOIN B ON A.x = B.y`（y 无索引）| Nested Loop Join 成 O(n²) |
| **IN 大列表** | `WHERE id IN (1,2,3,...,50000)` | 优化器退化为全表扫描 |
| **隐式转换** | `WHERE phone = 13800138000`（phone 为 varchar）| 类型转换导致索引失效 |
| **反范式聚合** | 多表 `GROUP BY` + `HAVING` + 子查询 | 临时表落盘，内存耗尽 |

### 2.2 为什么传统手段不够

- **慢查询日志（Slow Log）**: 事后分析， damage already done。
- **pt-query-digest**: 静态报表，无法实时干预。
- **DBA 人工审核**: 人力瓶颈，无法覆盖 AI 的生成速度（100x+）。
- **ORM 内置优化**: ORM 无法识别业务语义， generated SQL 越来越复杂。

### 2.3 核心诉求

> **在查询到达 MySQL 执行引擎之前或执行初期，自动识别风险并干预；同时对已发生的慢查询进行智能根因分析和自动优化建议。**

---

## 3. 业界最佳实践与调研

### 3.1 开源 SQL 审核与运维平台

#### 3.1.1 Yearning（MySQL 专用，Go + Vue）

- **定位**: 轻量级 MySQL SQL 审核平台，AGPL-3.0。
- **核心能力**: SQL 工单审核、DDL/DML 自动回滚语句生成、查询审计（数据源限制 + 脱敏）。
- **性能相关**: 集成 `pt-query-digest` 做慢日志分析，提供索引优化建议，集成 `pt-online-schema-change` 做大表 DDL。
- **局限**: 仅支持 MySQL；审核引擎 Juno 闭源；规则数约 46 条，相对有限。

#### 3.1.2 Archery（多数据库，Python/Django）

- **定位**: DBA 运维综合平台，Apache 2.0。
- **核心能力**: 多数据库支持（MySQL/PostgreSQL/Oracle/SQL Server）、SQL 查询 + 审核 + 执行 + 备份一体化。
- **性能相关**: **慢日志管理**、SQL 优化工具、Session 管理、参数配置。
- **优势**: 开源审核引擎 `goInception`；功能更全面，适合多数据库混合环境。

#### 3.1.3 SQLE（企业级，700+ 规则）

- **定位**: 企业级 SQL 质量管控平台，Apache 2.0。
- **核心能力**: 支持 MySQL（免费）、PostgreSQL 等；**700+ 审核规则**，覆盖命名规范、性能、安全性。
- **优势**: 规则深度最强，适合对 SQL 质量有严苛要求的大型组织。

#### 3.1.4 Bytebase（DevOps + GitOps）

- **定位**: 开源数据库 DevOps 工具，强调 Schema Change 的 GitOps 工作流。
- **核心能力**: SQL Review CI（可集成到 GitHub/GitLab CI）、SQL Review Policy（可配置规则集）、AI SQL Advisor。
- **优势**: **CI/CD 集成能力强**，能在代码合并前拦截问题 SQL；支持变更发布的版本化管理。

**启示**：这些平台解决了 "人审 SQL" 的流程问题，但**缺乏实时运行时的主动防御**和**AI 驱动的自适应优化能力**。

---

### 3.2 Percona 工具链（黄金标准）

#### 3.2.1 pt-query-digest — 慢查询分析之王

- **Query Fingerprinting**: 将相似查询聚类（忽略字面量），识别模式而非单条 SQL。
- **统计聚合**: 计算每组查询的执行次数、总/平均/最大执行时间、扫描行数/发送行数。
- **影响排序**: 按总耗时（而非单次耗时）排序，聚焦高影响查询。
- **历史追踪**: 可将结果存入 `query_review` / `query_history` 表，追踪趋势。
- **输入多样性**: 支持慢日志、general log、binlog、`SHOW PROCESSLIST`、甚至 `tcpdump` 抓包。

#### 3.2.2 pt-kill — 慢查询狙击手

- **运行时保护**: 监控 `SHOW PROCESSLIST`，自动杀死匹配条件的查询。
- **典型用法**:
  ```bash
  pt-kill --host=db --busy-time=60 --kill --interval=10 --print
  ```
- **精细过滤**: `--match-user`, `--match-info`（Perl 正则），`--match-state`（如 "Sending data"）。
- **局限**: 是**事后熔断**（查询已在执行），无法预防；高并发下频繁轮询有开销。

#### 3.2.3 pt-online-schema-change — 无损变更

- 解决大表加索引导致的锁表和慢查询问题。Agent 在推荐索引后，可用此工具安全应用。

#### 3.2.4 pt-index-usage

- 从慢日志分析索引使用情况，识别冗余/未使用索引，帮助清理索引膨胀。

**启示**：Percona 工具链是**事后分析和应急止血**的标杆，但需要与**事前拦截**和**智能分析**结合才能形成闭环。

---

### 3.3 MySQL 内置机制

#### 3.3.1 Query Rewrite Plugin（查询重写插件）

- **机制**: MySQL 5.7+ 内置，在解析阶段拦截并改写 SQL。
- **规则存储**: `query_rewrite.rewrite_rules` 表，`flush_rewrite_rules()` 生效。
- **支持语句**: `SELECT` / `INSERT` / `REPLACE` / `UPDATE` / `DELETE`（8.0.12+）。
- **防护场景**:
  - `SELECT * FROM huge_table` → 自动加 `LIMIT 1000`
  - 低效模式改写：注入 `USE INDEX`、调整 `JOIN` 顺序
  - 强制分页：给无 `LIMIT` 的查询自动追加
- **注意**: Percona 警告高并发下插件的 mutex 锁可能成为瓶颈，规则数量需控制。

#### 3.3.2 Performance Schema + sys Schema

- **`events_statements_history_long`**: 最近 N 条语句事件，用于慢查询现场还原。
- **`events_statements_summary_by_digest`**: 归一化查询指纹的聚合统计（执行次数、总耗时、平均耗时、扫描行数）。
- **`sys.statements_with_runtimes_in_95th_percentile`**: 95 分位慢查询视图。
- **`sys.statements_with_full_table_scans`**: 全表扫描查询视图。
- **最佳实践**: `history_long_size` 需根据负载调整；聚合视图比单条记录更能反映真实影响。

#### 3.3.3 MySQL 8.0.18+ EXPLAIN ANALYZE

- 提供**实际执行**的耗时和行数，而非优化器估算。Agent 可用此精确验证优化效果。

**启示**：MySQL 内置机制提供了**规则改写**和**性能数据**的基础设施，但规则管理仍依赖人工，缺乏**智能决策**。

---

### 3.4 代理层防护：ProxySQL Firewall

- **Firewall Whitelist**: ProxySQL 2.0.9+ 支持基于查询模式的白名单。
- **机制**: 学习正常查询模式 → 建立白名单 → 非白名单查询可被阻止或记录。
- **应用**: 可作为 AI 生成 SQL 的**第一道闸门**，防止明显异常的查询模式进入数据库。
- **局限**: 白名单模式适合业务稳定的查询，对探索性/报表类查询不友好；需要维护成本。

**启示**：ProxySQL Firewall 是**网络层/代理层**的有效补充，但粒度较粗，需与细粒度的 SQL 语义分析结合。

---

### 3.5 AI / ML 驱动的数据库优化

#### 3.5.1 EverSQL（已被 Aiven 收购）

- **能力**: AI 自动查询重写 + 索引推荐 + 持续性能监控。
- **效果**: 客户报告平均提速 25 倍；某案例从 20 分钟降至 370 毫秒。
- **特点**: 非侵入式（不访问敏感数据）；提供 Optimization API 可集成到 CI/CD。
- **局限**: SaaS 服务，数据隐私需考量；对复杂业务逻辑改写可能不完全准确。

#### 3.5.2 OtterTune（CMU，SIGMOD 2017 / VLDB 2018）

- **技术**: 基于 **Gaussian Process / Bayesian Optimization** 的数据库配置参数（knob）自动调优。
- **范围**: 调优 `innodb_buffer_pool_size`、`work_mem` 等参数，**不直接优化 SQL**。
- **优势**: 跨工作负载复用训练数据；1-2 小时可收敛。

#### 3.5.3 Bao（SIGMOD 2021 Best Paper）

- **技术**: **Contextual Multi-Armed Bandit**（强化学习），通过 Tree CNN 预测查询计划延迟。
- **机制**: **引导（Steer）** 而非替换 PostgreSQL 优化器 —— 为每条查询选择最优 hint set（如禁用 nested loop、启用 hash join 等）。
- **训练时间**: ~1-2 小时。
- **局限**: 主要针对 PostgreSQL；MySQL 优化器 hint 系统相对有限。

#### 3.5.4 Neo（VLDB 2019）

- **技术**: **Deep Q-Learning**，端到端生成完整查询计划（join order + scan choice）。
- **训练时间**: 20-40+ 小时。
- **局限**: 对动态工作负载适应性较弱；主要面向 PostgreSQL/SQL Server。

#### 3.5.5 QTune（SIGMOD 2022）

- **技术**: **Deep RL (DDPG)**，**Query-Aware** 的配置调优。
- **对比 OtterTune**: 针对单个查询做配置优化，而非全局 workload 调优；避免了信息损失。

#### 3.5.6 CDBTune+（VLDB/TKDE 2019-2021）

- **技术**: **Deep RL (DDPG)**，面向云数据库的高维连续空间自动调参。

**启示**：学术界在 **Learned Query Optimization** 和 **Auto-Tuning** 上已有成熟探索，但**落地到 MySQL 生产环境**仍需工程化包装。Agent 系统可以**集成这些思想**，利用 LLM 的推理能力替代部分 RL 训练，实现轻量级自适应优化。

---

### 3.6 论文与关键技术总结

| 论文/系统 | 年份 | 方法 | 聚焦点 | MySQL 适用性 |
|-----------|------|------|--------|-------------|
| OtterTune | 2017 | GP / Bayesian Opt | 参数调优 (Knobs) | 高 |
| Neo | 2019 | Deep Q-Learning | 端到端计划生成 | 中（需适配） |
| Bao | 2021 | Contextual Bandit | Hint 选择 / 优化器引导 | 中（MySQL hints 有限） |
| QTune | 2022 | DDPG | Query-Aware 参数调优 | 高 |
| EverSQL | 2019+ | 启发式 + ML | 查询重写 + 索引推荐 | 高 |
| Self-Healing DB (arXiv 2025) | 2025 | Meta-Learning | 依赖驱动的自愈 | 高 |

**核心洞察**：对于 MySQL 生产环境，最务实的路径是 **"LLM + 规则引擎 + Performance Schema 数据"** 的混合方案，而非完全依赖端到端 RL。LLM 负责语义理解和复杂推理，规则引擎负责快速拒绝，P_S 数据负责精准定位。

---

## 4. 解决方案架构：三层防御模型

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        Query Guard Agent 总体架构                         │
├─────────────────────────────────────────────────────────────────────────┤
│  LAYER 1: 事前拦截 (Pre-Execution Guard)                                 │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                  │
│  │ SQL Parser   │→│ Rule Engine  │→│ Risk Scorer  │                  │
│  │ (AST提取)    │  │ (700+规则)   │  │ (0-100分)    │                  │
│  └──────────────┘  └──────────────┘  └──────┬───────┘                  │
│                                              ↓                          │
│  ┌──────────────────────────────────────────────────────────────┐      │
│  │  LLM Semantic Analyzer (claude-go Agent)                      │      │
│  │  - 识别 AI 生成 SQL 的典型反模式                               │      │
│  │  - 结合表结构/索引/统计信息做深度分析                           │      │
│  │  - 生成改写建议或阻断理由                                       │      │
│  └──────────────────────────────────────────────────────────────┘      │
│                                              ↓                          │
│                                    [PASS / REWRITE / BLOCK]             │
├─────────────────────────────────────────────────────────────────────────┤
│  LAYER 2: 事中熔断 (In-Execution Guard)                                  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                  │
│  │ Processlist  │→│ Anomaly Det. │→│ Auto Kill    │                  │
│  │ Monitor      │  │ (异常检测)    │  │ (pt-kill 逻辑)│                  │
│  └──────────────┘  └──────────────┘  └──────────────┘                  │
│         ↑                              ↓                                │
│  ┌──────────────────────────────────────────────────────────────┐      │
│  │  Query Rewrite Plugin (MySQL) / ProxySQL Rule                 │      │
│  │  - 实时注入 LIMIT / HINT                                      │      │
│  │  - 拦截已知高危模式                                           │      │
│  └──────────────────────────────────────────────────────────────┘      │
├─────────────────────────────────────────────────────────────────────────┤
│  LAYER 3: 事后根治 (Post-Execution Optimization)                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                  │
│  │ Slow Log     │→│ pt-query-    │→│ Root Cause   │                  │
│  │ / P_S Digest │  │ digest-like  │  │ Analyzer     │                  │
│  └──────────────┘  └──────────────┘  └──────┬───────┘                  │
│                                              ↓                          │
│  ┌──────────────────────────────────────────────────────────────┐      │
│  │  Optimization Advisor (claude-go Swarm Intel)                 │      │
│  │  - 索引推荐 (冗余/缺失识别)                                    │      │
│  │  - 查询重写建议                                               │      │
│  │  - Schema 优化建议                                            │      │
│  │  - 自动产出 EXPLAIN ANALYZE 对比报告                           │      │
│  └──────────────────────────────────────────────────────────────┘      │
│                                              ↓                          │
│                              [Ticket / Auto-Fix / DBA Review]           │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 5. Agent 设计方案（结合 claude-go 能力）

### 5.1 claude-go 核心能力映射

`claude-go` 项目具备以下可直接复用的基础设施：

| claude-go 模块 | 能力 | 在本方案中的角色 |
|----------------|------|------------------|
| `pkg/agent` | Agent 生命周期管理、子代理派发、Team 协作 | 多角色 Agent 的创建与协调 |
| `pkg/orchestrator` | DAG 工作流编排、事件驱动调度、检查点/重试 | 三层防御的流水线编排 |
| `pkg/swarm_intel` | 群体智能引擎（Decompose→Scout→Predict→Debate→Fuse→Calibrate→Learn） | 复杂查询的分布式分析、共识决策 |
| `pkg/codeintel` | Graphify 代码图谱、向量检索、LSH | SQL 指纹的向量化存储与相似查询检索 |
| `pkg/skills` | 角色专属 Skill 注册与注入 | DBA Skill、SQL Audit Skill、Optimizer Skill |
| `pkg/metrics` | Prometheus 指标采集、LLM Collector | 慢查询指标、Agent 决策指标的观测 |
| `pkg/memory` | AgentDB / HNSW 向量记忆 | 历史慢查询模式、优化方案的记忆与检索 |
| `pkg/hooks` | 生命周期 Hook（pre-task / post-task / post-edit） | Agent 执行前后触发监控/告警/学习 |

---

### 5.2 Agent 角色设计

采用 **6 角色协同** 的 Hierarchical Swarm 拓扑（参考 claude-go 的 `hierarchical-coordinator` 模式）：

```
                    ┌─────────────────┐
                    │  DBA Captain    │  ← 协调者，最终决策
                    │  (Coordinator)  │
                    └────────┬────────┘
                             │
        ┌────────────────────┼────────────────────┐
        │                    │                    │
   ┌────▼────┐         ┌─────▼─────┐       ┌─────▼─────┐
   │ Auditor │         │ Sentinel  │       │ Optimizer │
   │(事前审核)│         │ (事中监控)│       │ (事后优化)│
   └────┬────┘         └─────┬─────┘       └─────┬─────┘
        │                    │                    │
   ┌────▼────┐         ┌─────▼─────┐       ┌─────▼─────┐
   │  Rule   │         │  Anomaly  │       │  Index    │
   │ Engine  │         │  Detector │       │ Advisor   │
   └─────────┘         └───────────┘       └───────────┘
```

#### Role 1: DBA Captain（协调者）

- **类型**: `hierarchical-coordinator`
- **职责**: 接收慢查询告警 / SQL 审核请求 → 分解任务 → 派发子 Agent → 综合结果 → 决策（阻断/放行/优化）。
- **System Prompt 要点**:
  - 你是一位拥有 15 年经验的 MySQL DBA Team Lead。
  - 你的目标是保护数据库免受低效 SQL 的侵害，同时不阻塞正常业务。
  - 你必须在 `Safety` 和 `Availability` 之间做权衡决策。
- **Skills**: `sql-audit-policy`, `incident-response-playbook`

#### Role 2: Auditor（事前审核 Agent）

- **类型**: `standalone` + `workflow`
- **职责**: 对进入系统的 SQL（来自应用日志、代码提交、或 ProxySQL 捕获）进行静态审核。
- **工作流**:
  1. **Parse**: 提取 AST（表、列、JOIN、WHERE、ORDER BY、LIMIT、聚合）。
  2. **Schema Fetch**: 从 `INFORMATION_SCHEMA` 获取表结构、索引、行数估算。
  3. **Rule Check**: 执行 700+ 条规则（复用 SQLE/Archery 思想）。
  4. **LLM Deep Analysis**: 对规则无法覆盖的复杂查询，调用 LLM 分析执行计划风险。
  5. **Report**: 输出风险评分（0-100）、命中规则列表、改写建议。
- **Skills**: `mysql-expert`, `sql-rewrite-patterns`, `index-design-principles`

#### Role 3: Sentinel（事中监控 Agent）

- **类型**: `standalone`（常驻 Daemon）
- **职责**: 持续监控 MySQL 运行状态，识别并熔断异常查询。
- **数据源**:
  - `SHOW PROCESSLIST`（高频轮询，如 5s）
  - `performance_schema.events_statements_current`
  - `performance_schema.threads` + `events_waits_current`
- **决策逻辑**:
  - 规则引擎：执行时间 > 阈值 / 扫描行数 > 阈值 / State = "Copying to tmp table" 等。
  - 异常检测：基于历史基线的 3-sigma 异常检测（由 `Anomaly Detector` 子 Agent 完成）。
  - 熔断动作：`KILL QUERY`（保留连接）或 `KILL CONNECTION`；记录到审计日志。
- **Skills**: `mysql-internals`, `pt-kill-patterns`, `performance-schema-expert`

#### Role 4: Optimizer（事后优化 Agent）

- **类型**: `workflow`
- **职责**: 对慢查询日志 / Performance Schema 摘要进行深度分析，产出优化方案。
- **工作流（7 阶段，对应 swarm_intel 流水线）**:
  1. **Decompose**: 将慢查询按指纹分组，识别 Top-N 高影响查询（`SUM_TIMER_WAIT DESC`）。
  2. **Scout**: 并行采集每个查询的 `EXPLAIN ANALYZE`、表统计信息、索引使用情况。
  3. **Predict**: 利用历史数据预测优化后的收益（基于相似查询的 past optimization record）。
  4. **Debate**: 多个 Analyst Agent 辩论最优方案（加索引 vs 改写 SQL vs 分表 vs 缓存）。
  5. **Fuse**: 共识聚合，输出综合优化建议。
  6. **Calibrate**: 用 Conformal Prediction 评估建议的置信度。
  7. **Learn**: 将优化结果存入 ReasoningBank，更新 PheromoneMemory。
- **Skills**: `query-optimization-master`, `explain-analysis`, `schema-design-patterns`

#### Role 5: Anomaly Detector（异常检测子 Agent）

- **类型**: `standalone`
- **职责**: 为 Sentinel 提供异常检测服务。
- **方法**: 基于 `swarm_intel` 的预测能力，对每个查询指纹维护：
  - 历史执行时间分布（EWMA + 标准差）
  - 扫描行数基线
  - 执行频率基线
- **输出**: 当当前查询偏离基线 > 3σ 时，标记为 `ANOMALY` 并附置信度。

#### Role 6: Index Advisor（索引顾问子 Agent）

- **类型**: `standalone`
- **职责**: 为 Optimizer 提供专业的索引分析。
- **能力**:
  - 分析 `sys.schema_unused_indexes` 识别冗余索引。
  - 基于 WHERE/JOIN/ORDER BY 列推荐复合索引（最左前缀匹配）。
  - 评估索引维护成本（写放大）vs 查询收益。
  - 生成 `pt-online-schema-change` 命令用于安全添加索引。
- **Skills**: `index-optimization`, `pt-osc-expert`, `write-amplification-analysis`

---

### 5.3 工作流编排设计（利用 pkg/orchestrator）

三层防御对应三条 DAG 流水线：

#### DAG 1: Pre-Execution Audit（事前审核流）

```
[SQL Input] → [Parser: Extract AST] → [Schema Fetch]
                                              ↓
                         ┌────────────────────┼────────────────────┐
                         ↓                    ↓                    ↓
                   [Rule Engine]      [LLM Analyzer]      [Risk Scorer]
                         ↓                    ↓                    ↓
                         └────────────────────┼────────────────────┘
                                              ↓
                                    [DBA Captain Decision]
                                              ↓
                              [PASS] / [REWRITE] / [BLOCK]
```

- **调度策略**: `Filter→Score→Dispatch`（借鉴 K8s 调度器）。
- **超时**: 单条 SQL 审核 < 3s（Rule Engine < 100ms，LLM < 2.9s）。
- **检查点**: 每批 100 条 SQL 保存 checkpoint，支持断点续跑。

#### DAG 2: In-Execution Monitoring（事中监控流）

```
[Processlist Snapshot] → [Fingerprint] → [Baseline Lookup]
                                              ↓
                                    [Anomaly Detection]
                                              ↓
                                    [Risk Classification]
                                              ↓
                         ┌────────────────────┼────────────────────┐
                         ↓                    ↓                    ↓
                   [LOW: Log]          [MED: Alert]         [HIGH: Kill]
```

- **调度策略**: 循环执行（`interval=5s`），非 DAG 退出。
- **反压**: 当待处理 processlist 快照堆积时，动态降低采样频率（`BackpressureCtrl`）。

#### DAG 3: Post-Execution Optimization（事后优化流）

```
[Slow Log / P_S Digest] → [pt-query-digest-like Aggregation]
                                              ↓
                                    [Top-N Selector]
                                              ↓
                         ┌────────────────────┼────────────────────┐
                         ↓                    ↓                    ↓
                   [Scout Agent]      [Debate Agent]       [Predict Agent]
                         ↓                    ↓                    ↓
                         └────────────────────┼────────────────────┘
                                              ↓
                                    [Fuse / Calibrate]
                                              ↓
                                    [Optimization Report]
                                              ↓
                              [Index DDL] / [Query Rewrite] / [Schema Change]
```

- **调度策略**: 每天凌晨 3:00 触发（cron），或按事件触发（慢查询突增告警）。
- **并行度**: Scout 阶段可并行分析 N 条查询（`MaxParallel=8`，与 claude-go 默认一致）。

---

### 5.4 感知-决策-行动循环（OODA Loop）

```
        Observe（观察）
   ┌─────────────────────┐
   │  - Performance Schema│
   │  - Slow Query Log    │
   │  - SHOW PROCESSLIST  │
   │  - ProxySQL Metrics  │
   │  - Application Logs  │
   └──────────┬──────────┘
              ↓
        Orient（研判）
   ┌─────────────────────┐
   │  - 指纹归一化        │
   │  - 基线对比          │
   │  - 影响面评估        │
   │  - 历史模式检索      │←── AgentDB / HNSW
   └──────────┬──────────┘
              ↓
        Decide（决策）
   ┌─────────────────────┐
   │  - Rule Engine       │
   │  - LLM Reasoning     │
   │  - Swarm Consensus   │←── swarm_intel Debate+Fuse
   │  - Human-in-Loop     │←── DBA Captain 最终仲裁
   └──────────┬──────────┘
              ↓
        Act（行动）
   ┌─────────────────────┐
   │  - BLOCK / REWRITE   │
   │  - KILL QUERY        │
   │  - ADD INDEX (pt-osc)│
   │  - NOTIFY Developer  │←── Feishu/Lark/Slack
   │  - UPDATE Rewrite Rule│
   └─────────────────────┘
```

---

### 5.5 与 claude-go 能力的深度结合点

#### 5.5.1 利用 `swarm_intel` 做复杂查询的分布式分析

对于 AI 生成的复杂嵌套查询（如 5+ 层子查询），单 Agent 可能分析不全。可启动 `swarm_intel.Engine`：

- **Decompose**: 将查询按 CTE / 子查询拆分为子图。
- **Scout**: 多个 Scout Agent 分别分析每个子图的执行计划。
- **Debate**: Analysts 辩论整体 join order 的最优方案。
- **Fuse**: 综合为完整的优化建议。
- **Learn**: 存入 `PheromoneMemory`，下次遇到相似查询直接命中。

#### 5.5.2 利用 `codeintel` 做 SQL 指纹的向量检索

- 将慢查询指纹（digest text）通过 embedding 存入 `codeintel` 的向量存储。
- 新查询进入时，通过 HNSW 快速检索**最相似的历史慢查询**及其优化方案。
- 实现 "相似查询，秒级出方案"。

#### 5.5.3 利用 `memory` / `AgentDB` 做长期学习

- **ReasoningBank**: 存储每次优化的 "查询 → 诊断 → 方案 → 效果" 轨迹。
- **EWC++**: 防止新知识覆盖旧知识（不同业务线的优化经验共存）。
- **DTI (Decision Trajectory Integrity)**: 检测 Agent 是否做出自相矛盾的决策。

#### 5.5.4 利用 `metrics` 做全链路可观测

- `llm_collector`: 记录每个 Agent 的 LLM 调用次数、token 消耗、延迟。
- `module_metrics`: 记录 Rule Engine 吞吐量、Sentinel 拦截率、Optimizer 成功率。
- 输出到 Prometheus，配套 Grafana Dashboard（claude-go 已有 `grafana-dashboard-claude-go.json` 可参考）。

#### 5.5.5 利用 `hooks` 做自动化流水线触发

```json
{
  "pre-task": "mysql_guard_sql_audit",
  "post-task": "mysql_guard_learn_pattern",
  "post-command": "mysql_guard_notify_dba"
}
```

- `pre-task`: SQL 执行前自动触发审核。
- `post-task`: 优化完成后自动训练模式。
- `post-command`: 高危操作后自动通知 DBA。

---

## 6. 技术实现细节

### 6.1 SQL 指纹算法

采用 **pt-query-digest** 风格的指纹规则：

1. 将所有字面量替换为 `?`：`WHERE id = 123` → `WHERE id = ?`
2. 将 `IN (...)` 列表替换为 `IN(?)`：避免列表长度变化导致指纹分散。
3. 统一空白与大小写。
4. 保留表名、列名、操作符结构。

**存储**: `(fingerprint_hash, fingerprint_text, first_seen, last_seen, count, total_time, avg_time)`

### 6.2 风险评分模型

```
RiskScore = w1 * RuleSeverity + w2 * EstimatedScanRows + w3 * TableSizeFactor
            + w4 * IndexMissPenalty + w5 * AIFingerprintAnomaly
```

- `RuleSeverity`: 命中规则的严重性（INFO/WARNING/CRITICAL）。
- `EstimatedScanRows`: 基于 `EXPLAIN` 的 `rows` 估算。
- `TableSizeFactor`: 表行数 > 1000万 时倍增。
- `IndexMissPenalty`: WHERE 条件列无可用索引时加分。
- `AIFingerprintAnomaly`: 该指纹是否被历史标记为 "AI 生成的危险模式"。

**阈值**:
- 0-30: LOW（记录日志）
- 31-70: MEDIUM（告警 + 建议）
- 71-100: HIGH（阻断或要求人工审批）

### 6.3 与 MySQL 的集成点

| 集成方式 | 用途 | 实现 |
|----------|------|------|
| **Performance Schema** | 查询性能数据 | 直接 SQL 读取 `events_statements_summary_by_digest` |
| **sys Schema** | 便捷视图 | `sys.statements_with_full_table_scans` 等 |
| **Slow Query Log** | 离线分析 | `pt-query-digest` 或自研解析器 |
| **Query Rewrite Plugin** | 实时改写 | 自动写入 `rewrite_rules` 表并 flush |
| **Information Schema** | 元数据 | `TABLES`, `STATISTICS`, `COLUMNS` |
| **KILL command** | 熔断 | `KILL QUERY <thread_id>` |
| **ProxySQL** | 代理层拦截 | Admin 接口动态添加 firewall 规则 |

### 6.4 与现有 DBA 工具链的协作

```
┌─────────────┐     ┌─────────────────┐     ┌─────────────┐
│  Yearning   │←──→│  Query Guard    │←──→│  Archery    │
│  (工单审核)  │     │  Agent (协调中枢)│     │  (运维平台)  │
└─────────────┘     └─────────────────┘     └─────────────┘
                           ↓
                    ┌─────────────┐
                    │ Percona     │
                    │ Toolkit     │
                    │ (pt-kill,   │
                    │ pt-query-d, │
                    │ pt-osc)     │
                    └─────────────┘
```

- Agent 不取代现有工具，而是** orchestrator** —— 调用 Yearning API 发起审核工单、调用 `pt-kill` 做熔断、调用 `pt-online-schema-change` 执行索引变更。

---

## 7. 实施路线图

### Phase 1: MVP（4-6 周）— 事后分析 + 事中熔断

- [ ] 接入 Performance Schema / Slow Log 数据采集模块。
- [ ] 实现 SQL Fingerprint + 基础聚合统计。
- [ ] 部署 `Sentinel Agent`（pt-kill 逻辑的自动化封装，常驻进程）。
- [ ] 部署 `Optimizer Agent`（单 Agent 版本）：Top-N 慢查询自动 `EXPLAIN` + 索引建议。
- [ ] 集成飞书/钉钉通知（复用 claude-go `pkg/feishu`）。
- [ ] **预期收益**: 自动杀死 80%+ 的长运行查询；每日自动产出慢查询报告。

### Phase 2: 智能增强（6-8 周）— 事前拦截 + Swarm 分析

- [ ] 实现 `Auditor Agent` + Rule Engine（100 条核心规则）。
- [ ] 接入 ProxySQL / MySQL Query Rewrite Plugin 做实时拦截/改写。
- [ ] 引入 `swarm_intel.Engine`：复杂查询的多 Agent 辩论优化。
- [ ] 实现 `codeintel` 向量检索：相似查询历史方案复用。
- [ ] **预期收益**: 50%+ 的 AI 生成慢查询在到达 MySQL 前被拦截或改写。

### Phase 3: 自适应学习（8-10 周）— 闭环优化

- [ ] 完善 `AgentDB` / `ReasoningBank`：优化经验持久化。
- [ ] 实现自动索引评估 + `pt-online-schema-change` 自动执行（低风险场景）。
- [ ] 引入 Conformal Prediction 校准优化建议置信度。
- [ ] DBA Captain 的 Human-in-Loop 决策记录用于微调（RLHF）。
- [ ] **预期收益**: 系统具备 "自学习" 能力，相似问题复发率降低 90%。

### Phase 4: 平台化（持续）— 多实例 + 多数据库

- [ ] 支持多 MySQL 实例统一管理。
- [ ] 扩展到 PostgreSQL（复用 Bao 思路）。
- [ ] 开放 Optimization API，供 CI/CD 调用。
- [ ] 与 Bytebase / Archery 深度集成，形成 DevOps 闭环。

---

## 8. 预期收益与度量指标

### 8.1 核心指标（KPIs）

| 指标 | 基线 | Phase 1 目标 | Phase 3 目标 |
|------|------|-------------|-------------|
| 慢查询数量（>1s）/ 日 | X | -30% | -80% |
| 慢查询总执行时间 | Y | -40% | -85% |
| 平均告警响应时间 | 人工 15min | < 10s（自动） | < 1s（预防） |
| AI 生成 SQL 拦截率 | 0% | 0% | > 50% |
| 索引推荐采纳率 | N/A | > 60% | > 85% |
| DBA 人工审核工作量 | 100% | -40% | -70% |

### 8.2 Agent 自身效率指标

| 指标 | 目标值 | 说明 |
|------|--------|------|
| SQL 审核延迟（Rule） | < 100ms | 纯规则引擎 |
| SQL 审核延迟（LLM） | < 3s | 复杂查询深度分析 |
| Sentinel 轮询间隔 | 5s | 可配置 |
| Kill 决策延迟 | < 1s | 从检测到执行 |
| 慢查询日报生成时间 | < 5min | Top-100 分析 |
| Agent 误杀率 | < 0.1% | 被人工复活的查询占比 |

---

## 9. 风险评估与缓解

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| **误拦截正常查询** | 高 | 渐进式部署：先 Audit-Only 模式运行 2 周，收集误报率后再启用 Block；保留 DBA Captain 人工仲裁权。 |
| **Sentinel 频繁 Kill 导致业务中断** | 高 | 分级熔断：MED 先 Alert，HIGH 才 Kill；Kill 前发送 `SIGTERM` 式通知（如给应用返回 KILL 警告）。 |
| **LLM 分析成本过高** | 中 | Rule Engine 过滤 80% 简单查询，仅复杂查询走 LLM；使用 Haiku 做初筛，Sonnet 做深度分析（3-Tier Routing）。 |
| **Query Rewrite Plugin 性能瓶颈** | 中 | 控制规则数量 < 100 条；高并发场景改用 ProxySQL 改写。 |
| **索引推荐导致写性能下降** | 中 | Index Advisor 必须评估写放大；禁止在写密集型表高峰时段自动加索引。 |
| **Agent 自身成为单点故障** | 中 | Sentinel 以 Daemon Set 部署多实例；Orchestrator 支持 Checkpoint 断点续跑。 |

---

## 10. 总结

本方案提出的 **Query Guard Agent** 不是简单的 "AI 替代 DBA"，而是 **"AI 增强 DBA"** —— 利用 `claude-go` 的 Agent/Swarm/Orchestrator 能力，将 DBA 的专业知识编码为可自动执行的防御体系：

1. **事前**: Auditor Agent 像 Code Review 一样审核每一条 SQL，在代码合并或查询执行前拦截风险。
2. **事中**: Sentinel Agent 像 SRE 的 on-call 一样 7x24 监控数据库，异常查询秒级熔断。
3. **事后**: Optimizer Agent 像资深 DBA 一样深度分析慢查询，产出可执行的优化方案。

三层防御 + 群体智能 + 持续学习，构成一个**面向 AI 时代的 MySQL 查询安全网**。

---

## 参考资源

### 开源工具
- [Yearning](https://github.com/cookiey/yearning) — 开源 MySQL SQL 审核平台
- [Archery](https://github.com/hhyo/Archery) — SQL 审核查询平台
- [SQLE](https://github.com/actiontech/sqle) — 企业级 SQL 质量管控
- [Bytebase](https://github.com/bytebase/bytebase) — 数据库 DevOps 工具
- [Percona Toolkit](https://docs.percona.com/percona-toolkit/) — pt-query-digest, pt-kill, pt-online-schema-change

### MySQL 官方文档
- [MySQL Query Rewrite Plugin](https://dev.mysql.com/doc/refman/8.4/en/rewriter-query-rewrite-plugin.html)
- [ProxySQL Firewall Whitelist](https://proxysql.com/documentation/firewall-whitelist/)
- [Percona Server Slow Query Log](https://docs.percona.com/percona-server/8.4/slowlog-rotation.html)

### 学术论文
- [OtterTune: Automatic Database Management System Tuning Through Large-Scale Machine Learning](https://db.cs.cmu.edu/papers/2017/p1009-van-aken.pdf) — SIGMOD 2017
- [Bao: Making Learned Query Optimization Practical](https://15799.courses.cs.cmu.edu/spring2022/papers/17-queryopt1/marcus-sigmod2021.pdf) — SIGMOD 2021
- [Neo: A Learned Query Optimizer](https://www.vldb.org/pvldb/vol12/p1705-marcus.pdf) — VLDB 2019
- [QTune: A Query-Aware Database Tuning System with Deep Reinforcement Learning](https://15799.courses.cs.cmu.edu/spring2022/papers/08-knobs3/p2118-li.pdf) — SIGMOD 2022
- [Towards Dynamic and Safe Configuration Tuning for Cloud Databases](https://arxiv.org/pdf/2203.14473) — arXiv 2022
- [Is Your Learned Query Optimizer Behaving As You Expect?](https://www.vldb.org/pvldb/vol17/p1565-lehmann.pdf) — VLDB 2024

### 技术博客与文档
- [MySQL Tuning in 2025: Use Percona Toolkit for Query Optimization](https://mangohost.net/blog/mysql-tuning-in-2025-use-percona-toolkit-for-query-optimization/)
- [How to Analyze the MySQL Slow Query Log](https://oneuptime.com/blog/post/2026-03-31-mysql-how-to-analyze-the-mysql-slow-query-log/view)
- [How to Use pt-kill to Manage MySQL Queries](https://oneuptime.com/blog/post/2026-03-31-mysql-use-pt-kill-manage-queries/view)
- [Using Slow Query Log to Find High Load Spots in MySQL](https://www.percona.com/blog/identifying-high-load-mysql-slow-query-log-pt-query-digest/)
- [Advanced Strategies for Proactive MySQL Performance Optimization](https://releem.com/blog/advanced-strategies-proactive-mysql-performance-optimization)
- [Query Optimization with AI-Powered Assistants](https://www.refontelearning.com/blog/query-optimization-with-ai-powered-assistants)
- [How AI is Transforming SQL Query Optimization in 2025](https://ai2sql.io/how-ai-is-transforming-sql-query-optimization-2025)
- [对比国内主流开源 SQL 审核平台 Yearning vs Archery](https://segmentfault.com/a/1190000044384110)
