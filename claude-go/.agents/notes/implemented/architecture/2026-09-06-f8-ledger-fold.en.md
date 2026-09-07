# Agent Note: F8 ledger folder — metrics are pure functions over the ledger

Status: implemented

English | [中文](2026-09-06-f8-ledger-fold.md)

## Problem

metrics/evolution.jsonl was already an append-only ledger (measured: 21 evo_* metrics, 260+ snapshots each), but the 21 metrics each maintained their own in-memory snapshot in `pkg/agent/evolution.go` — **no unified seq, no "recompute from the ledger" contract**. A wrong metric definition could only be fixed "going forward"; history was unfixable, and validating the impact of a definitional change meant waiting for new data. dsh `packages/telemetry/ledger`'s core idea: the event ledger is immutable; every metric is derived by folding it.

## Decision

**Metric = pure function (`FoldSpec`), ledger = sole input; `Fold(stateDir, from, to, specs...)` is the single entry point for recomputing any metric.**

1. **`FoldSpec`**: `{Name, From(Entry), Acc(acc, value)}` — each metric declared as a pure composition of "extract value from event" + "fold". Deterministic over the same ledger range; fix a definition by changing From/Acc and recomputing all history.
2. **Folded values, not snapshots**: `Fold` returns range-folded values; gauge-type metrics (learning_cost_ratio) fold numerator and denominator separately and then divide, rather than folding the ratio — the rationale lives in the ledger.go header comment, and every FoldSpec must carry a unit test locking its semantics (sample input → expected output), to cure "metric drift".
3. **Dual ts formats absorbed uniformly**: two timestamp formats coexist in the ledger (historical legacy); Entry parsing accepts both and Fold compares via unified unix-nano — the ledger is never rewritten; the read side absorbs the compatibility burden.
4. **Corrupt lines are skipped silently**: a crash-truncated tail line does not block folding (trace gaps must not affect delivery); `pkg/evolution/ledger_uplift.go` provides paired uplift (paired differencing before/after injection).
5. **Threshold constants centralized** (ledger.go): `LearningCostRatioWarn=0.10` / `RewardDistKSWarn=0.3` / `CanaryWinRateFloor=0.4` / `PromoteSurvivalFloor=0.7` — decision thresholds live with folding logic, not scattered.

### Rejected alternative

Migrating the ledger to a structured seq-bearing event stream (the dsh shape) — the existing JSONL is production data; the migration payoff does not justify breaking compatibility. Pure-function folding achieves the same "recomputable" goal without touching the ledger.

## Verification

- `pkg/evolution/ledger_test.go` (-race green): per-FoldSpec semantic locks (sample input → expected output), dual-ts parsing, range determinism.
- Cluster measurement (2026-09-06): 6 new metrics landed in the real ledger — learning_cost_ratio=0.01858 (<10% gate), injection_uplift_paired=0.125 (28s after team completion).

<!-- pairing: 2026-09-06-f8-ledger-fold.en.md@self confirmed 2026-09-07 -->
