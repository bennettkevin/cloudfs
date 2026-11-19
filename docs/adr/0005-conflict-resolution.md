# ADR-0005: Client-side conflict resolution; keep both versions when bytes are at stake

- **Status:** Accepted
- **Date:** 2026-11-19
- **Research:** [Spike 02](../research/02-conflicts.md) (#2)

## Context

[ADR-0003](0003-sync-model.md) decides how conflicts are detected: the server rejects a commit whose base revision is stale. This ADR decides what happens next, for every kind of conflict, under the design doc's goal 4 (no silent data loss).

Conflicts come in two kinds:
- **Content:** two devices changed the bytes of the same file. Only the user can say which is right.
- **Structure:** renames, moves, deletes, name collisions. No bytes are lost either way.

## Decision

**Guiding rule:** if bytes could be lost, keep both. If only names or locations differ, a deterministic rule decides.

**Who resolves:** the server only detects and rejects, and validates structural invariants. The device whose commit was rejected resolves the conflict using ordinary commits. Devices not involved see ordinary changes in the log and need no special logic.

**Case handling**

| Case | Resolution |
|---|---|
| Edit / edit | Conflicted copy. The version already on the server keeps the original name; the rejected device commits its version as a **new node** with a conflicted-copy name. Identical content hashes are not a conflict. |
| Edit / delete (either order) | **Edit wins.** The file is restored with the edited content. |
| Delete / delete | Not a conflict. The server treats the second delete as already applied (idempotent). |
| Rename / rename, move / move | **First committed wins.** The rejected device matches it locally; the event is logged. No copy is made. Version history is the safety net. |
| Edit / rename or move | Not a conflict. Content and location are versioned separately ([ADR-0006](0006-separate-content-and-location-revisions.md)). |
| Name collision (two nodes, same name, same folder, including file vs folder) | The later commit is renamed with a conflicted-copy name. Both are kept. |
| Folder deleted while another device adds/edits inside it | Content wins. Restore only the folders on the path needed to hold that file; other deleted children stay deleted. |
| Cross-device cycle (A moves X into Y, B moves Y into X) | The server rejects the move that would create a cycle (descendant check inside the commit transaction). The rejected device reverts it locally and logs it. |
| Case-only names (`Notes.md` vs `notes.md`) | The server enforces **case-insensitive** uniqueness of live names within a folder. Collisions follow the name-collision rule. |

**Server-validated invariants** (checked in the commit transaction): unique live name per folder (case-insensitive), no folder cycles, parent must be live (or restored per the rule above).

**Conflicted copies**
- Name format: `<name> (conflicted copy, <device>, <YYYY-MM-DD>).<ext>`. Extension stays last; a counter is added if that name is taken. The marker is fixed so the client can detect conflicted copies and never nest the suffix.
- Conflicted copies are ordinary nodes and **sync to every device**.
- Resolution is idempotent: on retry after a network error, the client checks whether the copy already exists (by content hash) before creating another.

**Notification (v1):** the filename, a structured warning log line, and a `cloudfs conflicts` command listing unresolved conflicted copies.

## Alternatives considered

- **Server-side resolution** — one implementation, but changes ADR-0003's "reject on stale base" into "accept and fork", needs naming policy and device names on the server, and doesn't work if content is end-to-end encrypted (spike 03).
- **Last writer wins for content** — silently discards an edit; violates goal 4. Device clocks are also unreliable.
- **Delete wins with a conflicted copy (Syncthing can do this)** — also avoids loss, but leaves a renamed copy where "edit wins" simply keeps the file.
- **Local version keeps the original name** — forces every other device to process a rename; server-first matches Dropbox and Nextcloud.
- **Keep conflicted copies local only (Nextcloud default)** — less clutter, but devices disagree about folder contents and the conflict can only be resolved on one machine.
- **Case-sensitive names on the server** — would allow names that can't coexist on macOS/Windows, pushing the problem to every client.
- **Prompt the user** — a background daemon has nobody to ask; the file would sit unsynced.
- **Automatic three-way text merge** — deferred. Only safe for plain text and requires storing base content. Candidate later feature, opt-in by file pattern.

## Consequences

- No conflict path discards bytes; the worst case is clutter (a conflicted copy or a restored file).
- Clients must implement resolution correctly and resume safely after a crash mid-resolution (baseline + re-pull makes this workable). Resolution logic is a priority for tests.
- Tombstones must be retained at least as long as the change log (90 days) so offline edits can be matched to deleted nodes (spike 05).
- Downloads must not overwrite unsynced local edits: write to a temp file, re-check the local hash against the baseline, then atomically rename; a mismatch is a conflict (spike 06).
- Version history (goal 5) backs the "first committed wins" rules.
- OS notifications and three-way merge are deferred.
