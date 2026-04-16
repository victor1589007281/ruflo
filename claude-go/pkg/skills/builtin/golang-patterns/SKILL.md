---
name: golang-patterns
description: ECC-inspired Go development patterns for package design, error handling, interfaces, concurrency, and maintainable services.
when_to_use: Use when implementing or reviewing Go code.
version: 1.0
---
# Go Development Patterns

## Package design
- Organize by capability, not by technical layer names alone.
- Export the smallest surface that other packages actually need.
- Keep constructors explicit and dependency injection simple.

## Idiomatic Go
- Return errors with context; avoid panic for expected failures.
- Prefer concrete types until an interface is needed at a consumer boundary.
- Use small interfaces defined by the caller, not giant shared abstractions.

## Concurrency
- Tie goroutine lifetime to context or ownership.
- Make channel direction and shutdown behavior explicit.
- Protect shared state deliberately; do not mix ad hoc locking and channels without a reason.

## Review checklist
- Is the zero value meaningful or intentionally prevented?
- Are contexts propagated to IO and cancellations?
- Are errors wrapped with enough detail to debug production issues?
