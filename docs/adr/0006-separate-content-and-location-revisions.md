# ADR-0006: Separate content and location revisions per node

- **Status:** Accepted
- **Date:** 2026-11-19
- **Amends:** [ADR-0003](0003-sync-model.md) (conflict detection)
- **Research:** [Spike 02](../research/02-conflicts.md) (#2)

## Context

ADR-0003 gives each node a single revision and rejects any commit with a stale base revision. With stable IDs ([ADR-0004](0004-stable-file-ids.md)), an edit on one device and a rename on another touch different attributes of the same node and should both apply. A single revision can't tell them apart, so the second one would be rejected as a false conflict.

## Decision

Each node has two revisions:
- **`content_rev`**: bumped when the file's bytes change (or the node is deleted/restored).
- **`location_rev`**: bumped when `name` or `parent_id` changes.

A commit states the base revision only for the group it changes, and the server compares only that one. A commit that changes both (rare) must match both. Deletes check `content_rev`, so an edit the deleting device never saw causes a rejection, and "edit wins" applies per [ADR-0005](0005-conflict-resolution.md).

Everything else in ADR-0003 is unchanged: change log, cursors, baseline, retention, epoch.

## Alternatives considered

- **Server-side rebase on rejection** — keep one revision and, on a mismatch, have the server compare the incoming change with changes since the base, applying it if they touch different attributes. Works, but harder to reason about and test than two independent compare-and-swaps.
- **Single revision (status quo)** — produces false conflicts for edit/rename, undercutting ADR-0004.

## Consequences

- Edit/rename and edit/move across devices merge cleanly with no conflict.
- The client baseline stores both revisions per node.
- Change log entries record which revision changed.
