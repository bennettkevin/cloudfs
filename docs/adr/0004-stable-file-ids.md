# ADR-0004: Identify files by stable ID, not by path

- **Status:** Accepted
- **Date:** 2026-11-19
- **Research:** [Spike 01](../research/01-sync-model.md) (#1)

## Context

The metadata model needs a primary identity for files and folders. Keying by path makes the path the identity; keying by a stable ID makes the path an attribute. This choice shapes every table and the sync protocol, and is close to impossible to change later. Dropbox's Nucleus rewrite named the lack of stable IDs as the core flaw of its previous engine.

## Decision

Every file and folder gets a server-assigned stable ID at creation that never changes. Name and parent folder are attributes of that ID. Renames and moves are a single attribute change on the ID, and deletes are a tombstone on the ID.

## Alternatives considered

- **Path as identity** — simplest model, but a rename is delete + create, a folder rename touches every descendant (thousands of log entries), and edit-vs-rename races across devices become ambiguous conflicts.

## Consequences

- Renaming or moving a folder is one change regardless of how many files it contains.
- Concurrent edit on one device and rename on another can be reconciled: they touch different attributes of the same ID.
- The client must map local paths to IDs and detect moves locally. Operating systems often report a rename as delete + create, so the client has to pair those events (inode, content hash) before sending a move. Covered by spikes 05 and 06.
- Path uniqueness (no two live files with the same name in one folder) becomes a constraint to enforce, and a source of name-collision conflicts for spike 02.
