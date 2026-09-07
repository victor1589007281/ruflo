# Agent Note: F10 token surface pricing — usage-anchored reallocation of prompt components

Status: implemented

English | [中文](2026-09-06-f10-token-surface-pricing.md)

## Problem

llm.jsonl has long recorded tokens per call (`llm_input_tokens` / `llm_output_tokens`, design/02 §1.4) and per-component heuristic estimates (`llm_prompt_component_chars` / `llm_prompt_component_tokens`, chars/4). But the heuristic only answers "what does the composition roughly look like" — it cannot answer **"how many real tokens did the skill listing eat in this request, which system section has the highest share"**, because without a usage anchor the per-component estimates do not sum to the gateway's actually-billed InputTokens, and reconciling them against an invoice would misprice.

This is item F10 of the dsh adoption survey (docforge planning-dsh-adopt 13.6.2): dsh's `packages/llm/token-meter` has `TokenMeasurement`/`TokenSurfaceNode` pricing per "context surface" node, with a usage anchor and logRevision. This repo had call-level accounting only — no surface-level pricing.

## Decision

**Two tracks coexist; neither substitutes for, nor corrects, the other.**

1. **Heuristic track (pre-existing, untouched)**: `llm_prompt_component_chars` / `llm_prompt_component_tokens` record all 9 components every call (including zeros), answering "composition shape". It emits unconditionally regardless of usage availability — that is its presentation role, not a defect.
2. **Anchored track (new)**: `llm_prompt_surface_tokens` — the real `InputTokens` reallocated onto components **proportionally to their heuristic share**, one histogram sample per component. It only answers "how is the actual billing distributed across the composition".
3. **Missing keys are never fabricated** (dsh projection.ts discipline, "never as a total"): when `InputTokens<=0` (gateways like Kimi routinely omit input) or all components are zero, **zero surface samples are emitted**. Passing heuristics off as the total is a forbidden presentation. The heuristic track keeps recording as usual, so the two kinds of absence remain distinguishable in metrics.

### Allocation algorithm (`api.AllocatePromptSurface`)

- `tokens = int(est/estTotal × anchor)` per component, truncated; the **remainder (`anchor - Σ`) goes to the largest heuristic share** — guaranteeing Σ tokens equals the anchor exactly; a mismatch is silent mispricing and must converge at allocation time.
- All-zero components or non-positive anchor → `Surfaces = nil` (no fabricated keys).
- Output sorted by Component (deterministic: same input, byte-identical result); `SharePct = tokens/anchor×100`, one decimal.

### logRevision

Every surface event carries a `pricing_rev` label (`api.TokenPricingRevision`, currently 1) — the pricing-rule version travels with the event, so mixed vintages in one file stay distinguishable. It does **not** enter Prometheus series identity: it is not in the `llmComponentNeed` label list, `fillLabels` filters it out, and series identity stays aligned with the heuristic twin (same unbounded-cardinality discipline as run_id living only in JSONL events, never labels).

## Verification

- `pkg/metrics/llm_surface_test.go`, 4 tests (-race green): anchored total conservation (8000:4000:4000 → 2000:1000:1000, Σ=4000), zero samples on missing anchor/zero components (heuristic still 18), pricing_rev on events not labels, table-driven allocation invariants (even/odd/single/all components: Σ==anchor, determinism, SharePct bounds, lexicographic order).
- Full-repo `go test ./...` zero failures (2026-09-07).

<!-- pairing: 2026-09-06-f10-token-surface-pricing.en.md@self confirmed 2026-09-07 -->
