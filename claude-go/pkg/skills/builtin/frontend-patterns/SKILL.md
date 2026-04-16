---
name: frontend-patterns
description: ECC-inspired frontend guidance for component architecture, state boundaries, accessibility, and user-facing reliability.
when_to_use: Use when working on React, SPA, or UI-heavy web applications.
version: 1.0
---
# Frontend Patterns

## Structure
- Keep presentation components lean and move orchestration into hooks or feature services.
- Prefer feature folders over type-based folders when the UI is medium or large.
- Derive view state where possible; avoid storing duplicate state.

## UX quality
- Handle loading, empty, success, and error states explicitly.
- Keep forms resilient with schema-based validation and clear inline feedback.
- Treat accessibility as part of the feature: keyboard access, labels, focus order, and contrast.

## State and effects
- Keep remote data, derived data, and ephemeral UI state separate.
- Cancel stale async work and guard against race conditions in concurrent UI flows.
- Avoid cascading effects; model intent through event handlers and data flow instead.

## Review checklist
- Are components doing one job?
- Are user-visible edge cases covered?
- Is the UI resilient to slow networks and partial failures?
