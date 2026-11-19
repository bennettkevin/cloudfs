# ADR-0001: Use Go for the client daemon and server

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

CloudFS needs a long-running client daemon on each device (macOS, Linux, eventually Windows) and a server API. The client has to be easy to install and run without a language runtime.

## Decision

Write both the client daemon and the server in Go.

## Alternatives considered

- **TypeScript/Node** — familiar from prior projects, but a daemon would need a bundled runtime, and cross-platform file watching and concurrency are weaker fits.
- **Rust** — single binaries and strong performance, but a much steeper learning curve for a part-time project.

## Consequences

- Single static binary per platform; easy cross-compilation.
- Goroutines/channels suit the watcher → hasher → uploader pipeline.
- Same language as most cloud tooling (Kubernetes, Terraform, Tailscale's WireGuard libraries), which helps in phase 4.
- Go is new to the developer: early sprints should be sized with learning time included.
