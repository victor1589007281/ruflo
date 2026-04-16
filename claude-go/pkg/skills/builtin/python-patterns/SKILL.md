---
name: python-patterns
description: ECC-inspired Python patterns for modules, typing, validation, async boundaries, and maintainable service code.
when_to_use: Use when implementing or reviewing Python applications or libraries.
version: 1.0
---
# Python Patterns

## Code shape
- Keep modules focused and import direction simple.
- Prefer explicit domain objects and typed protocols over sprawling dictionaries.
- Use `dataclass`, `TypedDict`, or Pydantic-style models where structure matters.

## Reliability
- Validate external input early.
- Raise precise exceptions and catch them at workflow boundaries.
- Keep async code and sync code clearly separated; do not hide event loop assumptions.

## Review checklist
- Are functions doing one job?
- Are implicit `None` cases and mutable defaults avoided?
- Is file, network, or database IO wrapped so tests stay cheap?
