---
name: django-tdd
description: ECC-inspired Django TDD workflow covering models, services, serializers, views, and migrations.
when_to_use: Use when building Django features or bug fixes with a test-first workflow.
version: 1.0
---
# Django TDD

## Test order
- Start with service or API behavior, then implement the minimal model or view changes.
- Add migration tests or assertions when schema behavior changes.

## Test layers
- Unit test model helpers and service functions.
- Integration test serializers, permissions, and queryset behavior.
- Use API tests for user-facing workflows and auth boundaries.

## Keep tests fast
- Use factories and focused fixtures.
- Prefer transaction-heavy tests only when behavior truly requires them.
