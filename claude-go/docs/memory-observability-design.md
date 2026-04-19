# 记忆系统可观测性指标体系设计

> **版本**: 1.0 | **日期**: 2026-04-19
> **目标**: 为 Dreaming / 记忆系统 / 失忆问题提供全方位的可量化观测能力

---

## 一、现有指标覆盖分析

### 1.1 已有指标 (12 个)

```
┌──────────────────────────────────────────────────────────────────────┐
│               现有指标覆盖图 (✅ 有 / ❌ 无)                          │
├──────────────┬──────────────────────────────────┬────────────────────┤
│   **类别**    │         **已有指标**                │  **缺失维度**       │
├──────────────┼──────────────────────────────────┼────────────────────┤
│ Dreaming     │ ✅ dream_count (次数)             │ ❌ 触发健康度        │
│              │ ✅ dream_sessions_input (输入)    │ ❌ 门控拦截原因      │
│              │ ✅ dream_compression_ratio        │ ❌ 待处理会话积压     │
│              │ ✅ dream_duration_sec             │ ❌ 距上次间隔       │
│              │ ✅ dream_error_count              │ ❌ Consolidator 产出 │
│              │ ✅ dream_output_size              │                    │
├──────────────┼──────────────────────────────────┼────────────────────┤
│ Memory (L1)  │ ✅ mem_entry_count               │ ❌ L2 FactStore 状态│
│              │ ✅ mem_avg_access_count           │ ❌ 分类分布         │
│              │ ✅ mem_prune_count                │ ❌ 衰减归档数       │
│              │ ✅ mem_retrieval_count            │ ❌ 保留率分布       │
├──────────────┼──────────────────────────────────┼────────────────────┤
│ 失忆风险     │ (无)                              │ ❌ 综合风险评分      │
│              │                                  │ ❌ PreCompact 抢救  │
│              │                                  │ ❌ 上下文丢失事件   │
└──────────────┴──────────────────────────────────┴────────────────────┘
```

### 1.2 核心问题

| **#** | **盲区**                              | **后果**                                          |
|:-----:|:-------------------------------------:|:-------------------------------------------------:|
| 1     | 无法观测 Dreaming 为什么没触发          | 4 天不触发，无法从 metrics 发现原因                  |
| 2     | sessionsSinceDream 不可观测            | 无法知道积压了多少待整理的会话                        |
| 3     | L2 FactStore 完全不可观测              | 不知道反失忆系统是否在工作                           |
| 4     | 无"失忆风险"的综合评分                  | 只能在出事后人工排查，无法预警                        |
| 5     | PreCompact 蒸馏无计数                  | 不知道 Compact 前抢救了多少信息                      |

---

## 二、新增指标设计

### 2.1 Dreaming 健康度指标 (7 个新增)

| **指标名**                       | **类型** | **单位** | **含义**                              | **告警条件**              |
|:--------------------------------:|:--------:|:--------:|:-------------------------------------:|:-------------------------:|
| `dream_sessions_pending`         | gauge    | count    | 当前待整理会话数 (sessionsSinceDream)  | > 20 → 积压警告            |
| `dream_hours_since_last`         | gauge    | hours    | 距上次 dream 的小时数                  | > 48h → 超时警告           |
| `dream_gate_block_reason`        | counter  | count    | 门控拦截次数 (按原因分)                | sessions_low 频繁 → 阈值问题 |
| `dream_consolidator_facts`       | counter  | count    | Consolidator 产出事实数               | 0 → 整合器失效             |
| `dream_consolidator_contradictions` | counter | count  | 矛盾检测数                            | 快速增长 → 知识不一致        |
| `dream_consolidator_patterns`    | counter  | count    | 模式发现数                            | —                         |
| `dream_trigger_source`           | counter  | count    | 触发来源 (按 label: afterquery/force/idle_fallback) | — |

**门控拦截原因 (dream_gate_block_reason labels)**:

| **label: reason**   | **含义**                                |
|:-------------------:|:---------------------------------------:|
| `sessions_low`      | 会话数不足 minSessions                   |
| `time_short`        | 距上次 dream 不足 minHours               |
| `already_dreaming`  | 已有 dream 在执行中                      |
| `lock_held`         | 文件锁被其他进程持有                      |
| `scan_throttle`     | 10 分钟扫描节流                          |

### 2.2 L2 FactStore 指标 (8 个新增)

| **指标名**                  | **类型** | **单位** | **含义**                              |
|:---------------------------:|:--------:|:--------:|:-------------------------------------:|
| `fact_total_count`          | gauge    | count    | L2 事实总数 (含归档)                    |
| `fact_active_count`         | gauge    | count    | 活跃事实数                             |
| `fact_archived_count`       | gauge    | count    | 已归档事实数                           |
| `fact_evergreen_count`      | gauge    | count    | 永久豁免事实数                          |
| `fact_avg_retention`        | gauge    | ratio    | 活跃事实平均保留率 (0-1)                |
| `fact_ingest_count`         | counter  | count    | 新事实摄入总数                          |
| `fact_decay_archived_count` | counter  | count    | 衰减归档总数                           |
| `fact_connection_count`     | gauge    | count    | 事实间关联数                           |

### 2.3 失忆风险指标 (3 个新增)

| **指标名**                  | **类型** | **单位** | **含义**                              | **告警条件**              |
|:---------------------------:|:--------:|:--------:|:-------------------------------------:|:-------------------------:|
| `amnesia_risk_score`        | gauge    | score    | 综合失忆风险评分 (0-100)               | > 70 → 高风险警告          |
| `precompact_facts_saved`    | counter  | count    | PreCompact 蒸馏抢救的事实数            | 0 且有 compact → 信息丢失  |
| `context_loss_events`       | counter  | count    | 上下文丢失事件数                       | > 0 → 立即关注            |

### 2.4 失忆风险评分算法 (amnesia_risk_score)

```
amnesia_risk = w₁ × dream_staleness      // 权重 30%: Dreaming 过期度
             + w₂ × session_backlog       // 权重 25%: 会话积压度
             + w₃ × retention_decay       // 权重 20%: 记忆衰减度
             + w₄ × fact_scarcity         // 权重 15%: 事实稀缺度
             + w₅ × compact_loss_rate     // 权重 10%: 压缩损失率

其中:
  dream_staleness  = min(hours_since_dream / 72, 1.0) × 100     // 72h 内线性增长
  session_backlog  = min(pending_sessions / 20, 1.0) × 100      // 20 条内线性
  retention_decay  = (1.0 - avg_retention) × 100                 // 保留率越低风险越高
  fact_scarcity    = max(0, 1.0 - active_facts/50) × 100        // 50 条以下风险增长
  compact_loss_rate = compacts_without_precompact / total_compacts × 100
```

---

## 三、指标在 Dashboard 的展示建议

### 3.1 Dreaming 面板

```
┌─────────────────────────────────────────────────────────────────┐
│                    🧠 Dreaming 健康面板                          │
├─────────────────────────┬───────────────────────────────────────┤
│  ⏱️ 距上次 Dream         │  📊 触发次数趋势                      │
│  ┌────────────────┐     │  ┌────────────────────────────┐      │
│  │   4d 2h 35m    │     │  │  ▇▇▇▇░░░▇▇▇▇▇░░░░░░░░░░  │      │
│  │   ⚠️ 超时!      │     │  │  04-12  04-14  04-16  04-19│      │
│  └────────────────┘     │  └────────────────────────────┘      │
│                         │                                      │
│  📦 待处理会话            │  🚫 门控拦截分布                      │
│  ┌────────────────┐     │  ┌────────────────────────────┐      │
│  │     0 条        │     │  │ sessions_low  ████████ 89% │      │
│  │  (计数器已重置)  │     │  │ time_short    ██       8%  │      │
│  └────────────────┘     │  │ lock_held     █        3%  │      │
│                         │  └────────────────────────────┘      │
├─────────────────────────┴───────────────────────────────────────┤
│  📈 整合质量                                                     │
│  ┌───────────────────────────────────────────────────┐          │
│  │ 事实产出: +12  矛盾检测: 2  模式发现: 3            │          │
│  │ 压缩率: 3.5x  耗时: 45s                           │          │
│  └───────────────────────────────────────────────────┘          │
└─────────────────────────────────────────────────────────────────┘
```

### 3.2 记忆健康面板

```
┌─────────────────────────────────────────────────────────────────┐
│                    💾 记忆系统健康面板                             │
├─────────────────────────┬───────────────────────────────────────┤
│  **失忆风险评分**         │  **事实存储分布**                      │
│  ┌────────────────┐     │  ┌────────────────────────────┐      │
│  │                 │     │  │ Fact       ████████    42  │      │
│  │      35/100     │     │  │ Preference ██████      28  │      │
│  │   (低风险 ✅)    │     │  │ Goal       ████        15  │      │
│  │                 │     │  │ Event      ██████      30  │      │
│  └────────────────┘     │  │ Context    ████        18  │      │
│                         │  │ Archived   ████████    45  │      │
│  **平均保留率**          │  │ Evergreen  ██          12  │      │
│  ┌────────────────┐     │  └────────────────────────────┘      │
│  │    0.72 / 1.0   │     │                                      │
│  │    ▓▓▓▓▓▓░░░░   │     │                                      │
│  └────────────────┘     │                                      │
├─────────────────────────┴───────────────────────────────────────┤
│  **PreCompact 抢救率**                                           │
│  ┌───────────────────────────────────────────────────┐          │
│  │ 压缩 15 次 → 抢救事实 47 条 → 抢救率 100%           │          │
│  └───────────────────────────────────────────────────┘          │
└─────────────────────────────────────────────────────────────────┘
```

---

## 四、告警规则建议

| **规则名**             | **条件**                                        | **严重度** | **动作**                     |
|:----------------------:|:-----------------------------------------------:|:----------:|:----------------------------:|
| Dream 超时             | `dream_hours_since_last > 48`                   | Warning    | 日志告警 + Dashboard 标红     |
| Dream 严重超时          | `dream_hours_since_last > 168` (7天)            | Critical   | 强制触发 ForceDream            |
| 会话积压               | `dream_sessions_pending > 20`                   | Warning    | 降低 minSessions 动态触发      |
| 记忆衰减严重            | `fact_avg_retention < 0.3`                      | Warning    | 加速衰减周期                   |
| 失忆高风险             | `amnesia_risk_score > 70`                       | Critical   | 自动触发整合 + 通知用户         |
| PreCompact 失效        | `compact > 0 && precompact_facts_saved == 0`    | Warning    | 检查 Ingestor 连接            |
| Consolidator 空转      | `dream_count > 0 && dream_consolidator_facts == 0` | Warning | 检查 LLM 连通性              |

---

## 五、与现有 4 天未触发问题的关联

### 5.1 如果有这些指标，能快速定位问题

```
时间线复盘 (假设新指标已上线):

04-13 08:16  进程重启
  → dream_sessions_pending: 0 (计数器清零)
  → dream_hours_since_last: 0 (时间戳清零)       ← ⚠️ 问题1: 不应清零!
  
04-13 ~ 04-15  团队工作流运行
  → dream_sessions_pending: 0→3→8→15...          ← 团队 RecordSession 不触发 AfterQuery
  → dream_gate_block_reason{sessions_low}: +100   ← ⚠️ 问题2: 飞书无消息，仅靠团队

04-15 19:42  最后一次 dream 成功
  → dream_sessions_pending: 0 (重置)
  → dream_hours_since_last: 0

04-15 ~ 04-19  零飞书消息
  → dream_sessions_pending: 始终 0
  → dream_hours_since_last: 0→12→24→48→72→96     ← ⚠️ 问题3: 持续增长但无告警
  → amnesia_risk_score: 20→35→50→65→80            ← 逐渐进入高风险区
```

### 5.2 修复后的预期指标行为

```
04-19 重启后 (修复后):
  → dream_sessions_pending: 从磁盘恢复 (非0)       ✅ 持久化修复
  → dream_hours_since_last: 从磁盘恢复             ✅ 持久化修复
  → 团队工作流完成后 → AfterQuery → 门控检查        ✅ 团队触发修复
  → dream_hours_since_last > 48 → idle_fallback    ✅ 超时兜底触发
```

---

## 六、实现优先级

| **优先级** | **指标**                              | **实现复杂度** | **价值** |
|:----------:|:-------------------------------------:|:--------------:|:--------:|
| P0         | `dream_sessions_pending`             | 低             | 极高     |
| P0         | `dream_hours_since_last`             | 低             | 极高     |
| P0         | `dream_gate_block_reason`            | 中             | 极高     |
| P0         | `amnesia_risk_score`                 | 中             | 极高     |
| P1         | `fact_*` (8 个 FactStore 指标)        | 低             | 高       |
| P1         | `precompact_facts_saved`             | 低             | 高       |
| P1         | `dream_consolidator_*` (3个)          | 低             | 中       |
| P2         | `dream_trigger_source`               | 低             | 中       |
| P2         | `context_loss_events`                | 高             | 中       |

---

## 参考

- [anti-amnesia-design.md](anti-amnesia-design.md) — 反失忆系统设计方案
- OpenTelemetry Metrics 规范 — Counter/Gauge/Histogram 语义
- Google SRE — SLI/SLO 驱动的可观测性
- Prometheus metric_metadata 标准
