---
name: golang-testing
description: ECC-inspired Go testing patterns including table-driven tests, subtests, benchmarks, fuzzing, and TDD-friendly structure.
when_to_use: Use when writing or fixing Go tests.
version: 1.0
---
# Go Testing Patterns

## Core workflow
- Follow red, green, refactor for new behavior.
- Keep tests in the same package when validating exported behavior; use external package tests when checking public API shape.

## Patterns
- Prefer table-driven tests for input or state matrices.
- Use subtests to separate scenarios and keep failures readable.
- Add benchmarks or fuzz tests when parsing, validation, or hot paths matter.

## Coverage targets
- Normal path, error path, and boundary values.
- Context cancellation and timeout behavior for IO-heavy code.
- Concurrency safety where shared state or goroutines exist.
