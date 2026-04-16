---
name: python-testing
description: ECC-inspired Python testing strategies using pytest, fixtures, parametrization, mocking discipline, and TDD.
when_to_use: Use when adding or debugging tests in Python projects.
version: 1.0
---
# Python Testing Patterns

## Preferred stack
- Use pytest for discovery, fixtures, parametrization, and readable failures.
- Reach for unittest.mock only at integration seams, not across whole modules.

## Strategy
- Parametrize scenario matrices instead of copy-pasting tests.
- Keep fixtures small and composable.
- Prefer factory helpers over giant fixture pyramids.

## Coverage checklist
- Happy path, validation failures, and exception translation.
- Boundary values and serialization or parsing errors.
- Async or background task behavior where applicable.
