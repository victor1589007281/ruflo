# Agent Note: F11 asOf projection — pure fold and time travel over tracestore

Status: implemented

English | [中文](2026-09-06-f11-asof-projection.md)

## Problem

TraceStore can replay the Span stream for a run, but "reconstructing the context as of that moment" required hand-written span assembly every time — for the learning pipeline to want "the session view as of turn N", someone rewrote the per-Kind reassembly logic; assembly code lay scattered, with no watermark and no correctness definition.

This is item F11 of the dsh adoption survey: dsh `packages/session/session-projection` has `ProjectionDefinition` fold + `asOfSeq` time travel (a session view at any historical moment) + checkpoint restore.

## Decision

**`pkg/evolution/tracestore/projection.go`: generic fold + prefix replay + checkpoint restore — all three semantics anchored to dsh, one deliberate divergence.**

1. **`ProjectionDefinition[S, E]`**: `{Key, StateVersion, Init, Apply}` — one pure synchronous fold unit. `Apply` must be a pure function (no IO/time/globals); the correctness of asOf replay rests entirely on it. This is a direct translation of dsh's `ProjectionDefinition`; dsh's whole-value rule (state-bearing events must carry the full post-change state) needs no separate mechanism here — Spans are whole-value events by construction, and idempotent replayability follows directly from the pure function.
2. **`AsOf(traceID, asOf)` prefix replay**: take all Spans for the TraceID, apply up to (0-based, exclusive) the asOf-th. Time travel = replaying a prefix only: the append-only log's write order is stable, so the same cut always yields the same state. `Projected[S]` returns `{AsOfSeq, State, Count}` — value and watermark come from the same log cut; the view self-describes which event it saw up to (dsh asOfSeq semantics). asOf<0 / out of bounds → full view.
3. **`Restore` checkpoint continuation**: `(Key → {ver, seq, val})`; if the version matches and the watermark is usable, replay the suffix from the checkpoint; otherwise **silently degrade to full replay**.
4. **Assembly errors fail explicitly**: nil store → the `ErrNoProjection` sentinel; missing Key/Init/Apply → error at assembly time. Assembly mistakes are not silenced.

### Deliberate divergence: Restore does not fail loud

dsh **fails loud** when baseSeq>0 and the checkpoint is unusable — because its persisted row is the only non-replay source. Here the full Span text lives in the Log, so **full replay is always a viable fallback**; "silently degrade to replay" fits this repo's existing "trace gaps must not affect delivery" discipline better than "error out and make the caller retry" (the divergence is documented in prose at projection.go).

Also: our Spans carry no explicit seq field, so asOf uses the 0-based write index — Log.Append order is the global order, semantically identical to dsh's seq.

## Verification

- `pkg/evolution/tracestore/projection_test.go`, 4 tests (-race green): session-view replay matches hand assembly (exactly the assembly style F11 exists to kill), asOf time travel (cut-N view == full view of a log containing only the first N events, compared for k=0..3), checkpoint restore (ver=1 continuation == full replay; ver=0/seq=99 degradation still agrees), assembly discipline (nil store / missing fold / nil projection all error explicitly).

<!-- pairing: 2026-09-06-f11-asof-projection.en.md@self confirmed 2026-09-07 -->
