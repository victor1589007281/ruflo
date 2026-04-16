---
name: dotnet-patterns
description: ECC-inspired .NET patterns for ASP.NET services, dependency injection, async flows, and domain-oriented organization.
when_to_use: Use when implementing or reviewing C# or .NET applications.
version: 1.0
---
# .NET Patterns

## Architecture
- Keep controllers thin and move workflows into services or handlers.
- Model domain concepts explicitly instead of passing anonymous dictionaries or untyped blobs.
- Use dependency injection for infrastructure seams, not for every tiny helper.

## Reliability
- Make async cancellation tokens flow through IO paths.
- Keep DTOs separate from persistence entities when API contracts matter.
- Centralize exception translation and logging.
