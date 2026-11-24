# ADR-0012: Tombstones kept as stubs forever, a 30-day trash, and single-change folder deletes and moves

- **Status:** Accepted
- **Date:** 2026-11-23
- **Research:** [Spike 05](../research/05-deletes-renames.md) (#5)

## Context

[ADR-0003](0003-sync-model.md) keeps the change log for 90 days and makes long-offline devices do a full reconcile. [ADR-0005](0005-conflict-resolution.md) says edit beats delete and deletes are tombstones. During a full reconcile, a file that's in a device's baseline but missing from the server listing could have been (A) deleted and then forgotten by the server, (B) lost in a database restore, or (C) deleted on the server while this device edited it. If tombstones are purged, the server listing alone can't tell A from B, and a wrong guess either resurrects a deleted file or deletes a kept one.

## Decision

**Deletes are tombstones.** A delete commit carries the base `content_rev` ([ADR-0006](0006-separate-content-and-location-revisions.md)). The server sets `deleted = true`, `deleted_at`, `deleted_by` (the delete's `seq`), bumps `content_rev`, and appends `changes(op = delete)`. Revisions and chunks are kept, so the file can be restored.

**Trash, separate from version history.**
- Deleted nodes stay restorable for **30 days** by default (configurable `trash_retention`).
- **Restore revives the same node ID**: a new `content_rev` on the same node, logged as `changes(op = restore)`, so version history stays attached.
- After the trash period the node is **purged**: its revisions and encrypted name are dropped (GC then reclaims unreferenced chunks after its grace period, [ADR-0010](0010-chunk-storage-and-manifests.md)), and `purged_at` is set. Purge is not a logged change; devices already removed the file when they applied the delete.

**Tombstone stubs are kept forever.** After purge, a stub row remains (ID, `deleted_at`, `deleted_by`, `purged_at`, roughly tens of bytes). The server listing can always say "this node was deleted", so full reconcile never has to infer it. The epoch rule is implemented as a fallback for any node with no record at all:
- Epoch changed → database restore → re-upload (ADR-0003).
- Epoch unchanged → deleted and forgotten → delete locally, unless the local copy differs from the baseline, in which case re-upload as a new node (edit wins).

**Folder deletes are one change.** Deleting a folder appends one `delete` entry for the folder and, in the same transaction, marks every live descendant `deleted = true` with the same `deleted_by`. Restoring the folder restores exactly the nodes with that `deleted_by`. When a device applies a remote folder delete, files with unsynced local edits survive (edit beats delete; the needed folder path is restored per ADR-0005).

**Renames and moves are one change.** A rename or move bumps `location_rev` on that node only, so moving a folder is one change regardless of its contents. Devices store `parent_id` + `name` per node in their baseline (not full paths), so a folder move updates one baseline row. A rename onto an existing name follows ADR-0005's name-collision rule.

**Remote deletes are applied locally as plain deletes,** after the check that the local file has no unsynced edits. The server trash is the safety net. Moving files to the OS trash is a possible later option.

## Alternatives considered

- **Expire tombstones (e.g. Syncthing v2.0's six months) and rely on the epoch rule** — safe if implemented exactly, but puts the decision to delete user files on a rarely exercised inference. Syncthing expires them because every peer stores every tombstone; CloudFS stores them only on the server, so stubs are cheap (about tens of MB for a million lifetime deletes).
- **No trash; restore only from version history** — mixes two concepts and makes "undelete" harder to reason about. Comparable products: Dropbox 30/180/365 days by plan, Nextcloud at least 30 days by default.
- **Restore as a new node** — loses version history and the link to the original ID.
- **Fully recursive folder tombstones (one log entry per descendant)** — floods the change log for large folders.
- **Implicit descendant deletes (only the folder marked)** — makes exact restore and per-file queries harder.

## Consequences

- Server data model: `nodes.deleted_at`, `nodes.deleted_by`, `nodes.purged_at`; `changes.op` gains `restore`; settings `trash_retention` (30 days) and stubs-forever tombstones.
- A trash-purge worker runs alongside chunk GC.
- Version retention for live files (goal 5) is still to be set; it's independent of the trash period.
- Deleted files are recoverable for 30 days from any device via a restore command (CLI design later).
