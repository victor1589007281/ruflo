---
name: typescript-testing
description: ECC-inspired TypeScript testing patterns using Vitest or Jest, emphasizing TDD, boundary coverage, and reliable mocks.
when_to_use: Use when adding or fixing tests for TypeScript and JavaScript applications.
version: 1.0
---
# TypeScript Testing

## TDD loop
- Start with a failing test for the contract you want.
- Implement the smallest change that makes the test pass.
- Refactor only after behavior is protected.

## Test strategy
- Unit test pure logic directly.
- Test UI behavior through rendered output and user interactions, not internals.
- Test adapters and API clients at the boundary with contract-like fixtures.

## Reliability
- Prefer deterministic fakes over broad global mocks.
- Reset shared state between tests.
- Cover unhappy paths, retries, timeouts, and schema failures for IO-heavy code.
