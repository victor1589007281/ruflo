---
name: backend-patterns
description: ECC-inspired backend service patterns for layering, validation, observability, and operational safety.
when_to_use: Use when building APIs, services, workers, or persistence-heavy backend systems.
version: 1.0
---
# Backend Patterns

## Service design
- Separate transport, domain logic, and persistence concerns.
- Keep handlers thin: parse, validate, call service, map result.
- Make invariants live in domain code, not in controllers or routes.

## Safety
- Validate all external input.
- Use structured errors with context that operators can act on.
- Prefer idempotent operations and safe retries for side-effecting workflows.

## Data and observability
- Keep transaction boundaries explicit.
- Log with request or job context, not with ad hoc print statements.
- Emit metrics around latency, failures, retries, and queue depth where relevant.

## Review checklist
- Are side effects isolated?
- Are failure paths observable?
- Do API contracts stay stable and explicit?
