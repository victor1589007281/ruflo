---
name: dart-flutter-patterns
description: ECC-inspired Dart and Flutter patterns for widget composition, state ownership, async UI, and feature boundaries.
when_to_use: Use when implementing or reviewing Flutter applications.
version: 1.0
---
# Dart and Flutter Patterns

## UI structure
- Keep widgets small and composition-friendly.
- Separate presentation from state orchestration and side effects.
- Derive view models or presentation state instead of mutating widget trees ad hoc.

## Reliability
- Handle loading, empty, error, and recovered states explicitly.
- Keep navigation, analytics, and repository calls out of leaf widgets.
- Make async cancellation and stale-response handling part of the design.
