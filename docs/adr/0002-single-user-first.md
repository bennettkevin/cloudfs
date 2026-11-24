# ADR-0002: Single user, multiple devices for v1

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Multi-user support adds accounts, sharing, per-user permissions and quotas, plus the obligations that come with storing other people's data. The project's primary user is its developer.

## Decision

v1 supports one user syncing across multiple devices. Authentication still exists (devices must prove who they are), but there is no sign-up, sharing, or per-user isolation.

## Alternatives considered

- **Multi-user from day one** — more realistic for a hosted product, but multiplies scope and creates privacy/ToS obligations the project doesn't want to take on.

## Consequences

- Data model can stay simple, but should not make a later `owner_id` column impossible (avoid assumptions baked into object key layout).
- Self-hosters who run CloudFS for others own their privacy policy and ToS (noted in README).
