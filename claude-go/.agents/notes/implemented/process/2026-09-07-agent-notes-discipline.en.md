# Agent Note: F9 Agent Notes — one bilingual decision record per feature

Status: implemented

English | [中文](2026-09-07-agent-notes-discipline.md)

## Problem

claude-go's decision-record duty is carried by design/ (systematic design) plus design/PROGRESS.md (a dated review ledger, including self-corrections of three rounds of labeling mistakes) — honest, but with a **granularity gap**: the full reasoning chain of one major decision (like "orphan subsystem retirement", or the semantic divergences in this batch's F10-F13) — motivation, rejected alternatives, verification evidence — lay scattered in long review-log paragraphs, findable only by hand, with no mechanical constraint ensuring "behavior changed ⇒ record updated".

This is item F9 of the dsh adoption survey (process class): dsh's `.agents/notes/` keeps one Chinese/English architecture decision record per feature — proposal → verdict → verification chain — with a `verify-agent-note-format` mechanical gate.

## Decision

**`.agents/notes/{lifecycle}/{class}/yyyy-mm-dd-topic.md` layout + Chinese/English bilingual pair + a mechanical check test. Three deliberate divergences from dsh:**

1. **The primary side is Chinese** (dsh: English primary + `.zh.md` copy) — this repo's working language is Chinese; design/ and PROGRESS.md are entirely Chinese. The pair remains, with `.en.md` carrying equal authority for cross-language search.
2. **Enforcement is a Go test** (`tests/unit/agent_notes_test.go`), not a standalone script — this repo has no pnpm/script-gate infrastructure; a test joins the `go test ./...` main path. **A discipline without teeth is not a discipline.**
3. **The pairing record is a trailing file comment** (dsh: a separate `.i18n.yaml` recording git blob hashes) — our note volume is small (tens, not dsh's 618); a trailing comment saves a file and is directly readable by the test.

Adopted from dsh: the three lifecycles (implemented/proposed/rejected, with the Status line cross-checked against the folder), the closed class set (feature/bug-fix/simplification/architecture/process/testing), relative-link cross-references, implemented notes kept current with what shipped (facts only, never the decision), no centralized INDEX. dsh's frozen `archived/` tree is not absorbed — our note volume is far from needing it.

### Division of labor

An Agent Note records **the why and the given-up of a single decision**; design/ records systematic design; PROGRESS.md records the dated review ledger and self-corrections. Complementary, not overlapping: one major decision = a design section in design/ + a reasoning chain in one note + one review line in PROGRESS.md.

## Verification

- `tests/unit/agent_notes_test.go` (-race green): header format, status/folder agreement, closed class set, bilingual-pair completeness, relative links resolve.
- This batch's six notes (one each for F8-F13) are the first sample: every one follows the Problem → Decision → Verification skeleton.

<!-- pairing: 2026-09-07-agent-notes-discipline.en.md@self confirmed 2026-09-07 -->
