# ADR-0013: Client-side move detection and mass-delete protection

- **Status:** Accepted
- **Date:** 2026-11-23
- **Research:** [Spike 05](../research/05-deletes-renames.md) (#5); OS inputs from spike 06 (#6)

## Context

Operating systems often report a rename as a delete plus a create, and editors often save by writing a temp file and renaming it over the original. With stable IDs ([ADR-0004](0004-stable-file-ids.md)), the client has to turn these into the right commit: modify, move, or delete + create. Separately, an unmounted or accidentally emptied sync folder looks to the client like "every file was deleted", which must never propagate.

## Decision

**Move detection.** When the client sees local paths disappear and appear, it classifies them in this order:
1. **Same path as a baseline entry** → modify (covers editors that save by replacing the file).
2. **Same inode and device ID** as a baseline entry that disappeared → move.
3. **Exactly one** disappeared baseline entry with the same size and content hash (keyed hash over chunk IDs, [ADR-0009](0009-chunking-and-chunk-ids.md)) → move.
4. Otherwise → delete + create.

Local deletes are held for about **60 seconds** (the delete quiet period from spike 06) so a matching create can pair with them before anything is committed. A missed match loses no data: the "new" file's chunks are already stored, so nothing is re-uploaded; only the version-history link is lost. The client baseline stores `inode` and `device_id` per node.

**Mass-delete protection.**
- **Root marker:** the sync folder contains a hidden marker file. If it's missing (folder unmounted, replaced or wiped), the client stops syncing and reports an error instead of committing deletes. Same idea as Syncthing's `.stfolder`.
- **Threshold:** if a pending batch would delete more than **50% of synced files or more than 1,000 files**, whichever comes first, the client pauses and requires explicit confirmation (`cloudfs` command). Both limits are configurable. rclone bisync uses a similar 50% default.
- **Server trash** ([ADR-0012](0012-deletes-tombstones-and-trash.md)) is the last safety net.

## Alternatives considered

- **No move detection (always delete + create)** — no data loss with content-addressed chunks, but every rename loses version history and generates noisy change-log entries.
- **Trust OS rename events alone** — inotify's rename pairing is racy and FSEvents doesn't pair renames (spike 06); the stat-based inode check is more reliable.
- **Content-hash matching without the uniqueness rule** — ambiguous when several identical files disappear (e.g. copies), and could attach history to the wrong file.
- **No mass-delete guard, rely on trash only** — recoverable, but restoring thousands of files is painful and every device would apply the deletes first.

## Consequences

- Client baseline gains `inode`, `device_id` and a pending-delete queue with timestamps.
- Deletes reach the server about a minute after they happen locally.
- Inode stability across renames on APFS and Linux filesystems is assumed (prior knowledge, to be confirmed in watcher tests); content-hash matching covers cases where it isn't.
- Copy-then-delete of the original is detected as a move when the copy has a new inode but identical content and is the only match. That's acceptable: the content and name end up correct either way.
