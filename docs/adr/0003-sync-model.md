# ADR-0003: Server change log with cursors and base-revision conflict detection

- **Status:** Accepted
- **Date:** 2026-11-19
- **Research:** [Spike 01](../research/01-sync-model.md) (#1)

## Context

Devices need to (1) discover what changed since they last synced, (2) detect when two devices changed the same file concurrently, and (3) tell local changes apart from remote ones. CloudFS has a central server with Postgres metadata ([ADR-0002](0002-single-user-first.md)), so a central authority is available.

## Decision

**Change discovery: server change log + per-device cursor.**
- Every accepted change (create, modify, move, delete) is appended to a `changes` log in Postgres with a monotonically increasing sequence number.
- Each device stores a cursor (the last sequence it applied) and fetches "everything after cursor N". Live updates use a long-poll/WebSocket "something changed, come fetch" signal.
- Log writes are serialized (row or advisory lock) so sequence numbers become visible in order. Postgres sequences can commit out of order, and a device could otherwise skip an entry permanently. This gets a dedicated test.

**Conflict detection: base revision (compare-and-swap).**
- Each file has a revision. A commit states the revision it was edited from.
- If it matches the server's current revision, the commit is accepted: revision + 1, log entry appended, in one transaction.
- If not, the server rejects with a conflict response. That rejection is a signal to the client, not a user-facing outcome; what the client does next is decided in spike 02 (#2). Silently overwriting is ruled out by the design doc's no-silent-data-loss goal.

**Local baseline.** Each device stores, per file, the revision and content hash it last agreed on with the server (Dropbox's "synced tree"). Comparing local state, the baseline, and the server state classifies every difference as local change, remote change, or potential conflict.

**Log retention: 90 days, configurable.** A device whose cursor is older than the retained log gets a "cursor too old" response and does a full reconcile (local files vs baseline vs server listing) instead of a replay. No data is lost, it's just slower.

**Server restore: log epoch.** The log carries a random epoch ID, regenerated whenever the database is restored from backup. A device that sees a different epoch discards its cursor and does a full reconcile. Accepted consequence: files deleted between the backup and the restore may reappear and need deleting again. A reappearing file is preferred over a lost one. Restore events are logged clearly so the cause is visible.

## Alternatives considered

- **Per-file version vectors (Syncthing-style)** — detects concurrency without a central judge, which CloudFS doesn't need. Much higher implementation cost (vector merge/pruning, multi-peer convergence), and it still needs per-peer cursors for discovery. Can be layered on later if device-to-device sync becomes a goal (e.g. over the phase 4 network) without replacing the change log.
- **Snapshot diffing with no log (rclone bisync-style)** — no server-side history; renames degrade to delete + create, and offline devices can't distinguish deletes from additions without extra state.
- **No epoch on restore** — devices would silently miss changes after a restore. Rejected.
- **Halt sync on restore until manual intervention** — safe but high friction for a personal tool. Rejected.

## Consequences

- Server logic is mostly one transaction per change; fits Postgres directly.
- The change log can double as the event source for background workers (consume directly or via an outbox; decide in phase 2).
- Rejected commits leave uploaded chunks unreferenced, so chunk garbage collection becomes a worker job (spikes 04, 07).
- Deletes must be log entries (tombstones) so offline devices learn about them (spike 05).
- Log retention and epoch handling are operational settings to document in the design doc's infrastructure section.
