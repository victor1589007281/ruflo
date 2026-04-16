---
name: django-verification
description: ECC-inspired Django verification checklist for migrations, queries, permissions, and deployment-facing correctness.
when_to_use: Use when reviewing Django changes before merge or release.
version: 1.0
---
# Django Verification

## Verify before shipping
- Migrations apply cleanly and are reversible when expected.
- Query counts stay reasonable on hot paths.
- Permissions and tenant scoping are enforced in list and detail views.
- Background jobs and signals do not duplicate side effects.

## Regression checks
- Admin actions behave safely.
- API contracts remain backward compatible where required.
- Time zone, localization, and serialization behavior are covered where relevant.
