# ADR-0016: Upload reservations, orphan cleanup and idempotent commits

- **Status:** Accepted
- **Date:** 2026-11-24
- **Research:** [Spike 07](../research/07-uploads.md) (#7)

## Context

With direct uploads ([ADR-0015](0015-direct-presigned-chunk-transfer.md)), a client can upload a chunk and then crash before committing. That object is an **orphan**: stored and billed, but referenced by nothing. ADR-0010's GC only deletes chunks it knows about, so it needs a row for every object a client might upload. GC can also race an upload or commit that is still in progress.

Separately, a commit is a compare-and-swap on the base revision ([ADR-0003](0003-sync-model.md)). If the server applies a commit but the response is lost, the client's retry looks like a stale base, and the client would create a conflicted copy of its own change ([ADR-0005](0005-conflict-resolution.md)). The server can't spot "same content" because manifests are encrypted with a random nonce.

## Decision

**Reservation row when a URL is issued.** The upload-URL request upserts a `chunks` row with `uploaded_at = NULL` and `unreferenced_since = now()`. If no commit ever references it, the existing GC (unreferenced for more than 24 h) deletes the object and the row. No new GC mechanism.

**Closing the GC race.**
- The missing-chunk query and the upload-URL request both refresh `unreferenced_since = now()` on unreferenced rows they touch.
- GC locks the row while it deletes: one transaction does `SELECT … FOR UPDATE`, re-checks it's still unreferenced past the grace period, deletes the object, deletes the row, commits. A concurrent request waits, then sees the chunk as missing.
- URL expiry (15 min) stays far shorter than the grace period (24 h), checked at startup, so any URL still usable belongs to a row GC won't touch.
- HEAD at commit (ADR-0015) is the final safety net.

**Weekly repair sweep.** A ListObjectsV2 scan deletes objects that have no `chunks` row and are older than the grace period (bugs, manual uploads, objects uploaded after the backup a restored database came from). About $0.0007 per sweep on AWS at ADR-0010's size estimate.

**Idempotent commits.** The client generates a random `commit_id` (UUID) per logical commit and reuses it on every retry. The server stores it on the `changes` row with a unique constraint. A retry with a known `commit_id` returns the original result instead of being re-evaluated. It's kept as long as the change log (90 days).

**API batch limits** (design choices, not store limits): up to 1,000 chunk IDs per missing-chunk query, up to 256 URLs per upload-URL request. The client runs about 8–16 concurrent PUTs and fetches more URLs as its queue runs low. One commit per file stays the unit.

## Alternatives considered

- **Sweep only (no reservation rows)** — every orphan waits for the weekly scan, and the scan is the only thing stopping storage leaks.
- **Reservation rows only (no sweep)** — leaves objects without a row (bugs, database restores) forever.
- **Separate `pending_uploads` table** — duplicates what the `chunks` row with `uploaded_at = NULL` already says.
- **Rely on the conflict path for lost responses** — every lost response creates a spurious conflicted copy.
- **`commit_id` on `revisions`** — doesn't cover moves, renames and deletes, which create no revision. Every commit appends to `changes`.

## Consequences

- Schema (all additive): `chunks.uploaded_at` (nullable), `changes.commit_id` (unique).
- A "missing" chunk now includes rows with `uploaded_at IS NULL`, so a second device whose upload is still pending uploads its own copy. Harmless: last writer wins and both are valid.
- The client needs a durable `commit_id` per pending commit in its local store, so retries after a restart reuse it.
- A GC implementation detail (row lock across a store DELETE) is now part of the design; GC transactions hold a lock for one network call per chunk.
