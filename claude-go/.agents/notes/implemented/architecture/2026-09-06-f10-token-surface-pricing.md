# Agent Note: F10 表面定价 — prompt 组件的 usage 锚定 token 再分配

Status: implemented

[English](2026-09-06-f10-token-surface-pricing.en.md) | 中文

## Problem

llm.jsonl 早就按调用记账 token (`llm_input_tokens` / `llm_output_tokens`, design/02 §1.4), 也早已逐组件记录启发式估算 (`llm_prompt_component_chars` / `llm_prompt_component_tokens`, chars/4)。但启发式只回答"构成大概长什么样", 回答不了 **"这次请求里 skill 清单吃掉多少真实 token、哪段 system 占比最高"** ——因为没有 usage 锚点, 各组件的估算值加起来不等于网关真实计费的 InputTokens, 拿它和账单对账会错。

这是 dsh 吸收调研 (docforge planning-dsh-adopt 13.6.2) 的 F10 项: dsh 的 `packages/llm/token-meter` 有 `TokenMeasurement`/`TokenSurfaceNode` 按"上下文表面"逐节点定价, 含 usage 锚点与 logRevision; 本仓当时只有调用级记账, 无表面级定价。

## Decision

**双轨并存, 不互相替代, 也不互相修正。**

1. **启发式轨 (既有, 不动)**: `llm_prompt_component_chars` / `llm_prompt_component_tokens` 每次调用全量 9 条 (含零值), 回答"构成形状"。它每调用都发, 与是否有 usage 锚点无关——这是它的呈现定位, 不是缺陷。
2. **锚定轨 (新增)**: `llm_prompt_surface_tokens` ——把真实 `InputTokens` 按**启发式占比**再分配到各组件, 逐组件一条 histogram 样本。它只回答"真实计费在构成上怎么分布"。
3. **键缺失不捏造** (dsh projection.ts 的纪律, "never as a total"): `InputTokens<=0` (Kimi 类网关不回 input 是常态) 或全部组件为零时, **一条表面样本都不发**。拿启发式冒充总量是被禁止的呈现。启发式轨照常全量记录, 两种缺失在指标上因此可区分。

### 分配算法 (`api.AllocatePromptSurface`)

- `tokens = int(est/estTotal × anchor)` 逐组件截断; **余数 (`anchor - Σ`) 给启发式最大份额组件**——保证 Σ tokens 恰等于锚点, 失配即静默错价, 必须在分配处收敛。
- 全零组件或非正锚点 → `Surfaces = nil` (不捏造键)。
- 输出按 Component 字典序 (确定性, 同输入逐字节同结果); `SharePct = tokens/anchor×100` 一位小数。

### logRevision

每条表面事件都带 `pricing_rev` 标签 (取 `api.TokenPricingRevision`, 当前 1)——定价规则版本随事件走, 历史样本与新样本混在同一文件里时口径可辨。但它**不进 Prometheus 序列身份**: 不在 `llmComponentNeed` 标签清单里, `fillLabels` 会滤掉, series 身份与启发式孪生保持一致 (与 run_id 只进 JSONL 事件不进标签同一条无界基数纪律)。

## Verification

- `pkg/metrics/llm_surface_test.go` 4 测试 (-race 绿): 锚定总量守恒 (8000:4000:4000 → 2000:1000:1000, Σ=4000)、缺锚点/零组件零样本 (启发式照常 18 条)、pricing_rev 随事件不进标签、分配函数表驱动不变式 (整除/难整除/单组件/全组件: Σ==anchor、确定性、SharePct 越界检、字典序)。
- 全仓 `go test ./...` 零失败 (2026-09-07)。

<!-- pairing: 2026-09-06-f10-token-surface-pricing.en.md@pending confirmed 2026-09-07 -->
