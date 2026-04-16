---
name: coding-standards
description: Cross-language engineering standards inspired by ECC. Use for implementation, review, and refactoring work that should stay readable, safe, and testable.
when_to_use: Use when writing or reviewing production code in any language.
version: 1.0
---
# Coding Standards

## Core defaults
- Prefer small, cohesive files and functions over giant modules.
- Optimize for clarity first; avoid cleverness that hides intent.
- Make side effects explicit and keep pure logic easy to test.
- Validate inputs at boundaries and fail with actionable errors.
- Do not silently swallow errors or ignore failed operations.

## Delivery checklist
- Keep naming consistent with the domain, not the implementation detail.
- Add focused comments only where the why is not obvious from the code.
- Remove dead code, stale TODOs, and temporary debug output before finishing.
- Update tests with every behavior change.
- Prefer incremental, reviewable edits over broad rewrites.

## Review lens
- Is the change easy to reason about?
- Are failure modes surfaced clearly?
- Does the code fit the existing architecture instead of creating a side path?
- Can another engineer extend this safely in a week?
