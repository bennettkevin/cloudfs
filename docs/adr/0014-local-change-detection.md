# ADR-0014: Local change detection: OS events as hints, scans as the truth

- **Status:** Accepted
- **Date:** 2026-11-23
- **Research:** [Spike 06](../research/06-file-watching.md) (#6)

## Context

The client must notice every local change in the sync folder on macOS and Linux (Windows later), and apply remote changes without overwriting local edits or re-uploading its own writes. No OS notification mechanism is reliable on its own:
- **macOS FSEvents** coalesces events and sometimes only says "rescan this directory" (`MustScanSubDirs`).
- **Linux inotify** needs one watch per directory, drops events when its queue overflows (`IN_Q_OVERFLOW`), and races with newly created subdirectories; its rename pairing is described as inherently racy in its man page.

## Decision

**Events are hints; a scan of the actual files is the truth.**
- **Startup:** full scan against the baseline.
- **On events:** wait for a quiet period, then scan only the touched paths. **~10 s** for changes; deletes are held **~60 s** more for move pairing ([ADR-0013](0013-move-detection-and-mass-delete-guard.md)).
- **Periodic:** a full rescan **hourly**, with random jitter so devices don't scan in lockstep (Syncthing uses the same 10 s / hourly defaults).
- **On overflow** (`IN_Q_OVERFLOW`, `MustScanSubDirs`): an immediate full rescan.
- Scans compare size, mtime and inode against the baseline; only files that differ are re-read and re-chunked. A file is chunked only after its size and mtime are unchanged before and after reading ([ADR-0009](0009-chunking-and-chunk-ids.md)).

**Watcher library: `rjeczalik/notify`** (MIT, maintained; or Syncthing's fork `syncthing/notify`), behind our own small interface so it can be swapped.
- Recursive watching on both platforms: FSEvents on macOS, inotify on Linux.
- FSEvents requires **cgo on macOS**. macOS binaries are built on a macOS CI runner; Linux builds stay pure Go. Still one binary per platform ([ADR-0001](0001-use-go.md)).

**Echo suppression:** when applying a remote change, the client updates its baseline as part of the apply step, so the resulting OS event finds disk == baseline and nothing is uploaded. No timing-window suppression.

**Applying remote changes safely:**
1. Download and decrypt to a temp file in `<root>/.cloudfs/tmp/` (same filesystem as the target).
2. `fsync` the temp file.
3. Re-check the local target against the baseline (size, mtime, inode; content hash if those differ). If it changed, don't overwrite: it's a conflict ([ADR-0005](0005-conflict-resolution.md)).
4. Atomic `rename` over the target; `fsync` the directory.
5. Update the baseline.

**`.cloudfs/` directory** at the sync root holds temp files, the local state database and the root marker used by the mass-delete guard. The watcher and scanner always ignore it.

**Names:** sent to the server normalized to Unicode NFC; each device keeps its local spelling on disk. Collision checks are case-folded (exact normalization and case-folding rules shared with spike 11, #14). macOS APFS is case-insensitive by default and normalization-insensitive but preserving.

**Skipped in v1 (logged, not synced):** symlinks, FIFOs, sockets, device files.

**Ignore rules (v1):** built-in defaults only: `.cloudfs/`, `.DS_Store`, `._*`, `.Spotlight-V100`, `.Trashes`, `Thumbs.db`, `desktop.ini`, `~$*` (Office lock files), and common editor temp/swap patterns (`*.swp`, `*~`, `.*.tmp`). A user `.cloudfsignore` file is deferred.

**Atomic saves:** a rename onto a path that's already in the baseline is treated as a **modify** of that node, even though the inode changed.

## Alternatives considered

- **`fsnotify`** (BSD-3, most popular, pure Go) — not recursive, and on macOS it uses kqueue, which needs one open file descriptor per watched file and doesn't scale to a large sync folder.
- **Events only, no rescans** — misses changes on overflow, coalescing and new-directory races.
- **Polling only, no watcher** — simple, but either slow to notice changes or expensive to run often.
- **Resuming from FSEvents history on restart** — an optimisation; deferred until v1 works with startup full scans.
- **Timing-window echo suppression** — fragile when the system is slow; baseline-first is deterministic.
- **Syncing symlinks as links** — cross-platform semantics and security pitfalls; deferred.

## Consequences

- CI needs macOS and Linux runners: macOS for cgo builds and both for watcher tests.
- Watcher tests use real temp directories on each OS (bugs live in OS behaviour), scripted hint sequences through the interface, forced or injected overflows, and an invariant check that a full rescan finds disk == baseline after each scenario.
- A small window remains between the re-check (step 3) and the rename (step 4) where a user edit could land; it's kept as short as possible, as in other sync tools.
- Inode stability across renames on APFS and common Linux filesystems is assumed and confirmed by tests.
- Windows support (ReadDirectoryChangesW) is a later piece of work behind the same interface.
