# ADR-0020: Repository layout and core Go libraries

- **Status:** Accepted
- **Date:** 2026-11-24

## Context

Phase 1 starts with Sprint 1 (2026-12-03). Before the first production Go code, CloudFS needs a repository layout and a small set of core libraries. The developer is learning Go ([ADR-0001](0001-use-go.md)), so the choices favour the standard library and plain, readable code over frameworks. Every dependency should be maintained and permissively licensed (the project is MIT).

Library status below was checked on GitHub on 2026-11-24.

## Decision

**One Go module at the repository root**, `github.com/bennettkevin/cloudfs`:

```
cmd/cloudfs/           client daemon and CLI
cmd/cloudfs-server/    API server, workers and admin commands
internal/              all packages; nothing is a public Go API yet
  objectstore/         ObjectStore interface over aws-sdk-go-v2 (ADR-0011)
  db/                  pgx pool, migrations, queries
  server/              HTTP handlers and middleware
  ...                  more packages as features land
migrations/            goose SQL migrations, embedded into the server binary
deploy/compose/        Docker Compose stack for local development and self-hosting
spikes/                Sprint 0 spike programs, each its own module, not built by CI
```

Packages are added when a feature needs them, not up front.

**Core libraries and tools:**

| Need | Choice | Notes |
|---|---|---|
| HTTP routing | standard library `net/http` | Method and path patterns since Go 1.22; no framework |
| Postgres driver and pool | `github.com/jackc/pgx/v5` | MIT, v5.11.0. The pool can drop to zero idle connections (#26) |
| Queries | hand-written SQL with `pgx` | Revisit `sqlc` (MIT, v1.31.1) if the boilerplate grows |
| Migrations | `github.com/pressly/goose/v3` | MIT, v3.30.1. Plain SQL files, embedded with `go:embed`, run by `cloudfs-server migrate` |
| Logging | standard library `log/slog` | JSON in production, text locally |
| Object storage | `aws-sdk-go-v2` | As in ADR-0011 and ADR-0015 |
| Unit test S3 | `gofakes3` | Flow only; never for integrity or auth (ADR-0015) |
| Integration tests | `github.com/testcontainers/testcontainers-go` | MIT, v0.45.0. Garage and Postgres in throwaway containers |
| Lint | `golangci-lint` v2 | GPL-3.0, but only a development tool; it never becomes part of CloudFS |

**Configuration:** environment variables with command-line flag overrides; no config file in v1. A committed `.env.example` documents them, and real `.env` files stay gitignored.

**CI (GitHub Actions):** `go vet`, `golangci-lint` and unit tests on Linux and macOS; integration tests (testcontainers) on Linux.

**Database changes:** migrations are additive by default. Destructive changes follow the two-phase rule in the working agreement, once there's a deployed environment.

## Alternatives considered

- **Multiple Go modules** (client and server separately) — independent versioning isn't needed; one module keeps imports and CI simple.
- **`pkg/` for shared code** — nothing is meant to be imported by other projects yet; `internal/` keeps the API surface closed.
- **HTTP frameworks (chi, gin, echo)** — the standard router now covers what CloudFS needs, and learning `net/http` directly transfers everywhere.
- **`golang-migrate`** (MIT) — also good. Goose's Go API makes embedding and running migrations from the server binary straightforward.
- **`sqlc` from day one** — typed queries generated from SQL; common in industry, but another tool to learn in the first sprint.
- **An ORM (GORM, ent)** — hides the SQL that this project wants to learn and control.
- **A config file (YAML/TOML)** — environment variables suit containers (Compose, ECS) and are enough for now.

## Consequences

- `go build ./...` from the root builds both binaries. Spike modules are excluded from root builds, because each has its own `go.mod`.
- Moving to `sqlc` later is a contained change inside `internal/db`.
- CI needs a macOS runner from the start, which ADR-0014 also requires for the watcher.
- Library versions are pinned in `go.mod`; upgrades go through PRs with their release notes checked. `golang.org/x/text` upgrades must re-run the fold test (ADR-0017).
