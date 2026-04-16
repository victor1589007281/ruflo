---
name: typescript-patterns
description: ECC-inspired TypeScript patterns covering type-first APIs, runtime validation, module boundaries, and predictable async code.
when_to_use: Use when writing or reviewing TypeScript or JavaScript in Node.js or modern frontend applications.
version: 1.0
---
# TypeScript Patterns

## Types first
- Prefer explicit domain types over loose object literals.
- Narrow unions with dedicated guards instead of repeated casting.
- Use discriminated unions for workflow states and command results.

## Runtime safety
- Validate external data at runtime; types alone are not enough.
- Keep `unknown` at the boundary, then parse into domain types.
- Avoid `any` except at unavoidable integration seams.

## Modules and async
- Keep modules intention-revealing and dependency direction simple.
- Wrap IO with small adapters so business logic stays testable.
- Prefer async flows that return typed results instead of throwing mixed exceptions.

## Review checklist
- Do exported APIs communicate real invariants?
- Are unsafe casts minimized and justified?
- Is error handling consistent across promise chains and `async` functions?
