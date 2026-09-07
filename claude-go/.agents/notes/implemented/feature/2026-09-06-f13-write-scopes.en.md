# Agent Note: F13 writeScopes — declarative write domains and planner-level serialization

Status: implemented

English | [中文](2026-09-06-f13-write-scopes.md)

## Problem

The orchestrator already had two conflict defenses: `conflictKeys` (named conflict keys) and `writeFiles` (file paths). But when the planning layer declares a **domain-level write intent** — "I will touch shared state / a contract / a manifest" — there was no unified landing spot: either shoehorn it into conflictKeys or rely on manual coordination. The spec (docforge planning-dsh-adopt F13) requires tasks to declare write scopes, for serialization and observability.

## Decision

**`writeScopes` is a third declaration kind, with the same last-writer serialization as conflictKeys, and read-only tasks exempt.**

1. **The declaration chain is complete end to end**: WBS JSON `tasks[].writeScopes` → `parseWBSFromJSON` → `rawTask.writeScopes` → `RuntimeTask.WriteScopes` (orchestrator.go) / `TaskSpec.WriteScopes` (workflow.go, round-trip) → `AddTaskFull(..., writeScopes, ...)`.
2. **Serialization semantics** (orchestrator.go): for a non-read-only task (`isReadOnlyRawTask` false), each declared scope adds a dependency on the **previous task that declared the same scope** (both v2ID and depNum, the same last-writer pattern as conflictKeys) — same-scope tasks serialize, different scopes stay parallel. Empty-string scopes are skipped; read-only tasks are skipped entirely.
3. **Round-trip discipline**: `writeScopes` appears in the WBS serialization output (`writeScopes,omitempty`); whatever the planning model declares is what the orchestrator receives, with no implicit inference. A scope is a **domain key** (shared state / contract / manifest, abstract names), complementary to — not overlapping — writeFiles (concrete paths).

### Rejected alternative

Folding domain write intent into conflictKeys — it conflates semantics (a conflict key is "a fact that conflicts"; a scope is "the domain I intend to write") and makes it impossible to distinguish, on the observability side, "an intent declared by the planner" from "a conflict pattern learned from history". A separate field lets the 13.8 attribution pipeline aggregate by scope directly.

## Verification

- `pkg/agent/orchestrator_writescopes_test.go` + `pkg/tool/builtin/tasktools_writescopes_test.go` (-race green): same scope adds a dependency, different scopes don't serialize, read-only exemption, WBS round-trip fidelity.
- Full-repo `go test ./...` zero failures (2026-09-07).

<!-- pairing: 2026-09-06-f13-write-scopes.en.md@self confirmed 2026-09-07 -->
