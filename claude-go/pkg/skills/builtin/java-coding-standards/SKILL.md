---
name: java-coding-standards
description: ECC-inspired Java standards for Spring Boot style services, immutability, Optional usage, exceptions, and package layout.
when_to_use: Use when writing or reviewing modern Java code.
version: 1.0
---
# Java Coding Standards

## Defaults
- Prefer immutable data carriers such as records and final fields.
- Keep package structure aligned to domain capabilities.
- Use `Optional` for absent query results, not for every field.

## Error handling
- Throw meaningful domain exceptions.
- Avoid broad `catch (Exception)` unless centralizing translation and logging.
- Keep stream pipelines short and readable; switch to loops when clarity wins.

## Review checklist
- Are generics explicit and type safe?
- Are mutable shared objects minimized?
- Does the service layer stay thinner than the domain logic it coordinates?
