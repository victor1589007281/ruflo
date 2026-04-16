---
name: django-security
description: ECC-inspired Django security guidance for auth, permissions, CSRF, secrets, and unsafe ORM patterns.
when_to_use: Use when reviewing or implementing Django code that handles auth, sessions, forms, or external input.
version: 1.0
---
# Django Security

## Baselines
- Enforce permission checks close to the view or service boundary.
- Keep CSRF enabled for session-authenticated browser flows.
- Never trust request data, query params, headers, or uploaded files.

## Dangerous areas
- Avoid raw SQL unless parameterized and justified.
- Treat admin actions and management commands as privileged surfaces.
- Keep secret handling in environment-backed settings, never in code.

## Review checklist
- Are object-level permissions enforced?
- Are user-visible errors free of sensitive internals?
- Are file uploads, redirects, and HTML rendering sanitized?
