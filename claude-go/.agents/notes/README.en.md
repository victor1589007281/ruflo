# Agent Notes — claude-go decision records

[中文](README.md) | English

One kind of document lives here. An **Agent Note** records a decision or proposal that affects this codebase — the *why* and *what we gave up*, the parts code and design docs can't carry. This file defines where notes live, when to write one, and [the in-file format](#in-file-format).

## Layout and naming

Every note has two axes, both encoded in its **path**: `{lifecycle}/{class}/yyyy-mm-dd-topic-title.md`:

- **Lifecycle** (top-level folder) is the note's status, and a note moves between folders as that status changes:
  - **`implemented/`** — the decision shipped. The file records what was decided and what was rejected, and is **kept current with what actually shipped**: when code later moves a file, renames a package, or changes a key/default, the note's factual content is updated in the same change (facts only — paths, names, structure — never the decision itself).
  - **`proposed/`** — proposals reviewed before implementation; not yet built (or only partly).
  - **`rejected/`** — the proposal was considered and declined. Keep it only while its rationale prevents a tempting, meaningful mistake; otherwise delete it together with its bilingual pair.
- **Class** (nested folder) is the *kind* of decision, from the closed set below. Adding a class requires updating the closed-set list in `tests/unit/agent_notes_test.go`.

The date in the filename is when the topic was **first proposed**. Cross-references between notes use relative markdown links (`[topic](../../implemented/architecture/2026-…-….md)`) — never bare prose or numbers — so links are mechanically checkable and survive moves between folders.

The active lifecycle tree is the working inventory: browse or search it. Do not add a centralized `INDEX.md`.

## Classification (closed set)

| Class | What it covers |
|---|---|
| `feature` | A new user- or model-facing capability. |
| `bug-fix` | Corrects a defect or closes a gap a postmortem surfaced. |
| `simplification` | Removes code, behavior, or surface area without adding a capability. |
| `architecture` | A structural decision about the **shipped source** — how packages relate, what the runtime vocabulary is. |
| `process` | Tooling, policy, or workflow **around** the code — gates, scripts, directory conventions — not runtime behavior. |
| `testing` | Test infrastructure and strategy. |

The `architecture` / `process` line: **architecture** is about the source we ship; **process** is the surrounding tooling and workflow.

## When to write one

Every non-trivial change MUST add or update at least one Agent Note in the same change. Non-trivial means: alters behavior, architecture, a contract shared across files or packages, process or tooling, testing strategy, an on-disk / wire / configuration format, or any decision a maintainer may reasonably revisit. A decided change goes straight into `implemented/`; undecided proposals start in `proposed/`. Pick the class folder that matches the decision.

Updating the note that already owns the decision satisfies the rule; do not create duplicates. Purely mechanical or local edits with no change to behavior, contracts, structure, process, or rationale are exempt. A note is never edited into a *different decision*: supersede it with a new one and cross-link.

## In-file format

The first three lines of every active note are exactly:

```markdown
# Agent Note: <title>

Status: <status>
```

The `Status:` value must agree with the lifecycle folder (the check test cross-verifies):

- `Status: implemented`
- `Status: proposed`
- `Status: rejected — <why, in one line>`

### Bilingual pair

Every note is a **Chinese/English bilingual pair**: the primary file (Chinese, this repo's working language) plus an `.en.md` copy (English). Both sides carry **equal authority** with one-to-one structure (translated title, same sections, same decision order). Editing either side must bring the other along in the same change. All three files (primary, `.en.md`, the pairing record) move or get deleted together.

### Body skeleton

Every note opens with `## Problem` (the motivation, written to stand without the solution), followed by `## Decision` (the verdict and rejected alternatives). The lifecycle-specific recurring section is `## Verification` (shipped evidence). Genuinely bespoke sections (package topology, wire contracts) remain free-form between the required ones.

### Pairing record

A trailing comment records the bilingual pair's git blob hashes and last confirmed-consistent state:

```
<!-- pairing: <name>.en.md@<blob-hash-short> confirmed 2026-09-07 -->
```

## Enforcement

`tests/unit/agent_notes_test.go` mechanically checks: header format, status/folder agreement, the closed class set, bilingual-pair completeness (every note has its `.en.md` copy), and that relative links point at real files. **A discipline without teeth is not a discipline** — this is the delivered form of F9 (dsh adoption item, docforge planning-dsh-adopt 13.6.2 group C): claude-go's design/ + PROGRESS.md already carry the decision-record duty with higher honesty (including self-corrections), so the absorbed point is **granularity** only — major decisions (like the five in this batch, F10-F13) get their own note pages instead of living scattered in review logs.
