# Agent Note: F8 ledger 折叠器 — 指标是账本上的纯函数

Status: implemented

[English](2026-09-06-f8-ledger-fold.en.md) | 中文

## Problem

metrics/evolution.jsonl 已是 append-only 账本 (实测 21 个 evo_* 指标、每指标 260+ 快照), 但 21 个指标各自在 `pkg/agent/evolution.go` 维护内存快照——**无统一 seq、无"从账本重算"契约**。指标定义错了只能"从今天起才对", 改不了历史; 想验证某次指标口径变更的影响, 只能等新数据累积。dsh `packages/telemetry/ledger` 的核心思想: 事件账本不可变, 一切指标由账本折叠派生。

## Decision

**指标 = 纯函数 (`FoldSpec`), 账本 = 唯一输入; `Fold(stateDir, from, to, specs...)` 是重算任意指标的单一入口。**

1. **`FoldSpec`**: `{Name, From(Entry), Acc(acc, value)}`——每个指标声明为"从事件提取值 + 折叠"的纯函数组合。同一账本区间确定性; 定义修正 → 改 From/Acc → 全历史重算。
2. **折叠值而非快照**: `Fold` 返回区间折叠值; gauge 型指标 (learning_cost_ratio) 的分子分母各自折叠后相除, 而非对"比值"再折叠——口径写进 ledger.go 头注释, 每个 FoldSpec 必须带单测锁定语义 (输入样例 → 期望输出), 治"指标口径漂移"。
3. **双 ts 格式统一吸收**: 账本里两种时间戳格式并存 (历史遗留), Entry 解析时两种都认, Fold 内按统一 unix-nano 比较——账本永不改写, 兼容负担由读取侧吸收。
4. **行损坏静默跳过**: 账本尾部崩溃残行不阻塞折叠 (轨迹缺失不影响交付); 配套 `pkg/evolution/ledger_uplift.go` 做配对 uplift (injection 前后的 paired 差分)。
5. **阈值常量集中** (ledger.go): `LearningCostRatioWarn=0.10` / `RewardDistKSWarn=0.3` / `CanaryWinRateFloor=0.4` / `PromoteSurvivalFloor=0.7`——判定阈值与折叠逻辑同处, 避免散落。

### 被否方案

把账本改成带 seq 的结构化事件流 (dsh 形态)——已有的 JSONL 是生产数据, 迁移收益不抵破坏兼容; 纯函数折叠在不改账本的前提下达成同样的"可重算"目标。

## Verification

- `pkg/evolution/ledger_test.go` (-race 绿): 每个 FoldSpec 的语义锁定 (样例输入 → 期望输出)、双 ts 解析、区间确定性。
- 集群实测 (2026-09-06): 6 新指标真实落账本——learning_cost_ratio=0.01858 (<10% 闸)、injection_uplift_paired=0.125 (团队完成后 28s)。

<!-- pairing: 2026-09-06-f8-ledger-fold.en.md@pending confirmed 2026-09-07 -->
