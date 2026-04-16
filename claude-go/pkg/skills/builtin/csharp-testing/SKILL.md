---
name: csharp-testing
description: ECC-inspired C# testing patterns for xUnit or NUnit, async verification, dependency seams, and reliable mocks.
when_to_use: Use when creating or fixing tests in .NET projects.
version: 1.0
---
# C# Testing

## Preferred approach
- Test domain and application services directly before controller glue.
- Keep async tests truly async and assert cancellation or timeout behavior explicitly.
- Mock only external boundaries such as HTTP, storage, queues, or clocks.

## Coverage checklist
- Validation failures, exception mapping, and authorization paths.
- Serialization and contract regressions for APIs.
- Retry or background-worker behavior for job systems.
