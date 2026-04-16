---
name: django-patterns
description: ECC-inspired Django patterns for apps, models, services, serializers, and view separation.
when_to_use: Use when working in Django or Django REST Framework projects.
version: 1.0
---
# Django Patterns

## App boundaries
- Keep models, selectors, services, serializers, and views from collapsing into one layer.
- Put business workflows in service functions or service classes, not in views or signals.

## Models and queries
- Encode invariants in models and validators.
- Use `select_related` and `prefetch_related` intentionally for query-heavy endpoints.
- Keep queryset helpers reusable and named by business meaning.

## API shape
- Let serializers validate transport concerns.
- Keep views thin and explicit about permissions, pagination, and filtering.
