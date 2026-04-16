---
name: cpp-coding-standards
description: ECC-inspired modern C++ standards grounded in RAII, type safety, smart ownership, and readable APIs.
when_to_use: Use when writing or reviewing C++17+ code.
version: 1.0
---
# C++ Coding Standards

## Core principles
- Use RAII for every owned resource.
- Prefer `const`, `constexpr`, and strong types to encode intent.
- Reach for smart pointers only when ownership semantics require them.

## Code shape
- Keep templates focused and diagnosable.
- Prefer `enum class` over unscoped enums.
- Avoid raw `new` and manual delete in application code.

## Review checklist
- Is ownership obvious?
- Are move and copy semantics deliberate?
- Does the interface hide implementation without hiding failure behavior?
