---
name: cpp-testing
description: ECC-inspired C++ testing workflow using GoogleTest, CTest, sanitizers, and behavior-focused assertions.
when_to_use: Use when adding or debugging C++ tests or build-verification flows.
version: 1.0
---
# C++ Testing

## Test stack
- Prefer GoogleTest or GoogleMock for unit and component tests.
- Use CTest or the project build runner so tests stay integrated with CI.
- Run sanitizers for memory, thread, and undefined behavior where available.

## Test strategy
- Assert observable behavior instead of internal implementation details.
- Cover ownership, lifetime, and exception or error-return behavior.
- Add regression tests for concurrency and boundary conditions in native code.
