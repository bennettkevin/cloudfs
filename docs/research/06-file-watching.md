# Spike 06: File watching

- **Issue:** #6
- **Status:** Decided. See [ADR-0014](../adr/0014-local-change-detection.md). Section 9 recommendations adopted: events as hints + scans (startup, ~10 s / ~60 s quiet periods, hourly jittered rescan), `rjeczalik/notify` with cgo on macOS, baseline-first echo suppression, NFC names, symlinks and special files skipped, built-in ignore list (`.cloudfsignore` deferred). fsnotify README, rjeczalik/notify cgo build tags and Syncthing watcher/rescan defaults re-checked on 2026-11-23.
- **Question:** How does the client reliably detect local changes in the sync folder on macOS and Linux (Windows later), and how does it apply remote changes without clobbering local edits or re-uploading its own writes?

---

## Constraints already decided

- Single sync folder, single user, Go daemon, single binary per platform ([ADR-0001](../adr/0001-use-go.md)).
- The client keeps a **synced baseline** per node: synced revisions, content hash, size + mtime ([ADR-0003](../adr/0003-sync-model.md), design §9).
- Stable node IDs; the client must turn local renames into moves ([ADR-0004](../adr/0004-stable-file-ids.md)). The matching *logic* belongs to spike 05; this spike covers what the OS gives us.
- Conflicts are resolved client-side, and **downloads must not overwrite unsynced local edits** ([ADR-0005](../adr/0005-conflict-resolution.md), research 02 section 6).
- Before chunking, check size + mtime; re-check after; retry on mismatch ([ADR-0009](../adr/0009-chunking-and-chunk-ids.md), research 04 section 7).
- Names are unique per folder case-insensitively on the server (ADR-0005).

---

## The one-paragraph answer

No OS notification API is reliable enough to be the source of truth. Events get coalesced, dropped on overflow, reported without pairing, or missed while the daemon is stopped. Every serious sync tool treats **events as hints** that say "look here soon", and backs them with **periodic full rescans** that compare the disk against the baseline. The design question is not "which watcher is perfect" but "which watcher gives cheap, timely hints, and how cheap can the rescan be".

---

## 1. What each OS provides

### macOS: FSEvents (the native recursive API)

- Watches a **directory tree** with one stream. Recursive by design.
- Historically directory-level ("something in this folder changed"); a per-file mode exists (`kFSEventStreamCreateFlagFileEvents`, since 10.7) *(from prior knowledge)*.
- **Coalescing:** Apple's guide: "If an event in a directory occurs at about the same time as one or more events in a subdirectory of that directory, the events may be coalesced into a single event," flagged `kFSEventStreamEventFlagMustScanSubDirs`, and "you must recursively rescan the path listed in the event." Dropped events also set that flag.
- **History / resume:** events carry IDs, and a stream can start from a `sinceWhen` event ID (Apple's guide describes `FSEventStreamCreateRelativeToDevice` with `sinceWhen`). So a daemon can restart and ask "what changed since event N" instead of rescanning everything. Not guaranteed: if the history isn't available (e.g. the volume's event log was reset), you get a "history done / must rescan" signal and fall back to a full scan.
- **Rename events** are flagged (`ItemRenamed`) but **not paired**: you get the old and new paths as separate events with no cookie. Pairing relies on inode/file ID (see section 5).

### macOS: kqueue

- Needs **one open file descriptor per watched file or directory** (fsnotify README: "if you're watching a directory with five files then that's six file descriptors"). For 100k files that hits `kern.maxfilesperproc`.
- Not recursive. Not suitable as the main macOS watcher for a sync folder of any size.

### Linux: inotify

- **Per-directory watches, not recursive** (man page: "Inotify monitoring of directories is not recursive"). You add a watch for every directory in the tree.
- **New-subdirectory race:** by the time you add a watch for a new directory, files may already exist in it; "you may want to scan the contents of the subdirectory immediately after adding the watch."
- **Limits:** `fs.inotify.max_user_watches` caps watches per user. The old kernel default was 8192; a 2020 patch by Waiman Long raised the default to scale with memory, using at most 1% of RAM, clamped to **8192–1,048,576**, at roughly 1 KB per watch (patch thread on LKML). Distros may override. One watch per *directory*, so a 100k-file folder with ~5–10k directories usually fits, but small VMs/Raspberry Pis may need the limit raised.
- **Overflow:** events beyond `max_queued_events` "are dropped, but an IN_Q_OVERFLOW event is always generated." Robust applications must handle lost events, e.g. by rebuilding (i.e. rescanning).
- **Renames:** `IN_MOVED_FROM` + `IN_MOVED_TO` share a **cookie**. The man page warns matching is "inherently racy": the pair isn't guaranteed consecutive or atomically queued, and a move out of the watched tree has no `IN_MOVED_TO`.
- No history: anything that happens while the daemon is stopped is invisible. Startup needs a scan.

### Linux: fanotify

- Since Linux 5.13 (and 5.10.220), unprivileged users can use fanotify, but only with limited functionality: they must use file-handle reporting (`FAN_REPORT_FID`) and are "limited to only mark inodes"; mount and filesystem marks need `CAP_SYS_ADMIN` (fanotify_init(2)).
- So **unprivileged fanotify doesn't give recursive watching**, which was its main attraction. Whole-filesystem marks would require running the daemon as root. Not worth it for v1.
- fsnotify lists fanotify as "Not yet".

### Windows: ReadDirectoryChangesW (later)

- Recursive (`bWatchSubtree`), buffer-based; overflow means "rescan". The USN change journal is the robust alternative. fsnotify supports ReadDirectoryChangesW. Deferred with Windows.

---

## 2. Go libraries

Checked on GitHub, 2026-11-20:

| Library | License | Status | macOS backend | Recursive | cgo |
|---|---|---|---|---|---|
| `github.com/fsnotify/fsnotify` | BSD-3-Clause | Active, v1.10.1 (2026-05-04), ~10.8k stars | **kqueue**. FSEvents "Needs support in x/sys/unix" | **No** ("you must add watches for any directory you want to watch"; recursive is on the roadmap, #18) | No |
| `github.com/rjeczalik/notify` | MIT | Maintained (pushed 2026-06), ~0.9k stars | **FSEvents when built with cgo**; kqueue when `!cgo` (build tags `darwin && !kqueue && cgo` vs `darwin && !cgo`) | **Yes** (`dir/...` recursive watches; inotify recursion emulated) | Yes on macOS for FSEvents |
| `github.com/syncthing/notify` | MIT | Syncthing's fork of rjeczalik/notify (pushed 2025-05) | same as above | Yes | Yes on macOS |
| `github.com/fsnotify/fsevents` | BSD-3-Clause | Low activity (pushed 2024-05) | FSEvents only (macOS-only library) | Yes | Yes |

**cgo and the single-binary goal (ADR-0001):**
- Linux builds stay pure Go (inotify via syscalls), so static Linux binaries and cross-compilation still work.
- **FSEvents needs cgo** (it's a CoreServices C API). A cgo macOS build is still a single binary, but it must be **built on macOS** (or with a macOS cross toolchain). In CI that means a macOS runner for macOS builds. Budgeroo's Electron workflow already uses an OS matrix, so this is a known pattern.
- Avoiding cgo on macOS means kqueue, which doesn't scale (section 1).

**What Syncthing does:** it uses its notify fork (FSEvents on macOS, inotify on Linux) for hints, plus full rescans (section 4). That's the closest real-world precedent to CloudFS.

---

## 3. Quirks we must handle

### Atomic saves by editors and apps

Many programs save by writing a temp file and renaming it over the original. The watcher then sees: create `.file.swp`/`file~`/random temp → write → rename temp → original (and on inotify, sometimes a delete of the original first). fsnotify's README describes this pattern and recommends watching directories, not files.
- Examples *(from prior knowledge, not re-checked)*: vim (depends on `backupcopy`), JetBrains IDEs ("safe write", on by default), many macOS apps via `NSDocument` safe-save, Microsoft Office (temp files plus `~$` lock files).
- **Consequence:** the inode of "the file" changes on every save. Move detection by inode alone would wrongly decide "file deleted, new file created". The path is the same, so the rule is: **a rename onto an existing synced path is an edit of that node**, not a move.
- Temp and lock files must be ignored or debounced so they don't sync (section 7).

### Our own writes

When the client applies a remote change, the watcher fires for it. If unhandled, the client sees "local change" and re-uploads what it just downloaded (an echo), or worse, raises a false conflict.
- **Suppression by baseline, not by timing:** after applying a remote change, the client writes the new baseline (revision, content hash, size, mtime, inode) for that node *before* the event is processed. When the event arrives, the scan sees disk == baseline and does nothing. This is self-correcting: no "ignore events for 2 seconds" window that can swallow a genuine user edit.

### Partially written files

A large copy or download produces many write events over seconds or minutes.
- **Quiet period (debounce):** collect events and only process a path after it has been quiet for N seconds. Syncthing accumulates changes for 10s by default (`fsWatcherDelayS`) and delays deletes a further minute.
- **Stability check** at processing time: size + mtime before chunking, again after (already decided in research 04). Mismatch → retry later.
- Files open for writing can't be detected portably; the quiet period + stability check is the practical answer.

### Case-only renames and Unicode normalization (macOS)

- **APFS** (Apple's APFS FAQ): case-insensitive by default on macOS; **preserves** both case and normalization of names; **normalization-insensitive** (since High Sierra), so two names that differ only in Unicode normalization can't coexist in one directory and either form finds the file.
- **Linux** filesystems (ext4 etc.) are byte-exact: `café` in NFC and NFD are two different names.
- **Consequences for the client:**
  - Compare names using a **normalized form** (NFC) plus case folding when matching disk ↔ baseline, and send names to the server in one canonical normalization (NFC is the common choice) while keeping the on-disk spelling locally. This ties directly into spike 11's blind index (normalize, then case-fold, then hash).
  - A case-only rename (`notes.md` → `Notes.md`) on macOS is a real rename that the watcher may report oddly (same inode, path "changes" only in case). Treat "same inode, names equal case-insensitively" as a location change.
  - On Linux, two local files that collide case-insensitively (`a.txt`, `A.txt`) are allowed locally but rejected by the server (ADR-0005). The client should detect this before committing and apply the name-collision rule.

### Symlinks, hardlinks, special files

- **Symlinks:** don't follow (following can escape the sync folder or loop). v1 options: skip and log, or sync the link target as text. Skip is the conservative v1 choice; Syncthing syncs symlinks as symlinks *(from prior knowledge)*.
- **Hardlinks:** two paths, one inode. They will sync as two independent files (content deduplicated by chunk IDs anyway). Their inode equality must not confuse move detection.
- **FIFOs, sockets, device files:** skip.

### Timestamps

- mtime granularity varies (APFS and ext4: nanoseconds; FAT/exFAT on USB drives: 2 seconds). Equal mtime is not proof of no change; the size + mtime check is a fast filter, and the content hash (keyed hash over chunk IDs) is the truth.
- Clock skew between devices doesn't matter here: all comparisons are local disk vs local baseline. Server ordering comes from revisions (ADR-0003), never from mtimes.

---

## 4. Reliability strategy: hints + rescans

### What others do

- **Syncthing** (docs): watcher enabled by default, and "full scans are done once per hour" (`rescanIntervalS`), randomized between ¾ and 5⁄4 of the interval. Watcher changes are accumulated for 10s, deletes for a further 1 min.
- Dropbox and Nextcloud desktop also combine OS notifications with periodic/remote-triggered scans *(from prior knowledge, not re-checked)*.

### Proposed shape for CloudFS

1. **Startup:** full reconcile scan (disk vs baseline). On macOS, optionally start the FSEvents stream from the last saved event ID to narrow the scan; still fall back to a full scan if history is unavailable.
2. **Running:** watcher events go into a set of "dirty paths". After a quiet period (e.g. 5–10s; deletes longer), scan just those paths (and for `MustScanSubDirs`/`IN_Q_OVERFLOW`, the whole subtree or tree).
3. **Periodic full rescan:** e.g. hourly with jitter, as a safety net for anything missed.
4. **Inotify limit hit:** fall back to polling-by-rescan at a shorter interval and log clearly how to raise `max_user_watches`.

### Rescan cost

A rescan is a **stat-only walk**: for each entry, `lstat` gives size, mtime, inode, type. Compare against the baseline; only entries that differ get chunked and hashed.
- For ~100k files, a stat walk on an SSD typically takes on the order of a second or a few seconds when the metadata is cached, longer cold *(estimate, not measured; worth benchmarking once code exists)*.
- Hashing is the expensive part and only happens for entries whose size/mtime/inode changed.

---

## 5. Move detection inputs (what the OS gives us)

Spike 05 owns the matching logic. The raw inputs:

| Signal | Linux | macOS | Reliability |
|---|---|---|---|
| Paired rename events | `IN_MOVED_FROM`/`IN_MOVED_TO` with cookie | No pairing; `ItemRenamed` flag on both paths | inotify pairing is "inherently racy" (man page); moves out of/into the tree appear as delete/create |
| Inode number (`stat`) | Yes | Yes (APFS file IDs, via `stat`) | Stable across rename within a filesystem; **changes on atomic saves**; can be reused after delete |
| Device ID (`stat`) | Yes | Yes | Distinguishes filesystems; a move across filesystems is a copy + delete |
| FSEvents extended data with inode | — | Available on newer macOS (`UseExtendedData`) *(from prior knowledge, unverified)* | Would help pair renames |
| Content hash | Yes (we compute it) | Yes | Strong but expensive; good tie-breaker for "deleted here, identical content appeared there" |

Practical upshot for spike 05: **inode + device from the scan's stat walk** is the main signal (renames keep the inode), with the rule from section 3 that a rename *onto* an existing synced path is an edit, not a move. Events only tell us where to look.

---

## 6. Applying remote changes safely

Order of operations for a download (extends research 02 section 6):

1. Download and decrypt into a **temp file on the same filesystem** as the target (atomic `rename` only works within one filesystem).
2. `fsync` the temp file.
3. **Re-check the local target against the baseline** (size + mtime + inode, and content hash if they differ). If the local file changed since the last sync, **don't overwrite**: it's a conflict, handled per ADR-0005.
4. `rename` temp → target (atomic replace). Optionally `fsync` the directory.
5. Update the baseline (revision, hash, size, mtime, inode) for that node, which also suppresses the watcher echo (section 3).

There's a small window between the check in step 3 and the rename in step 4 where a user edit could land. It can't be closed fully on POSIX without file locking that editors don't respect; keeping the window tiny and re-checking right before rename is what other tools do *(the general approach is documented; exact tool behaviour from prior knowledge)*.

**Temp file location:** a hidden directory **inside the sync folder root**, e.g. `<root>/.cloudfs/tmp/`. Inside the root guarantees the same filesystem (unless the user mounts something inside the folder, which can be detected via device ID). The watcher and scanner ignore `.cloudfs/` entirely. The same directory can hold the local state database.

---

## 7. Ignore rules

Proportionate v1:
- **Always ignored:** `.cloudfs/` (our own state and temp files).
- **Built-in defaults:** `.DS_Store`, `._*` (macOS AppleDouble), `.Spotlight-V100`, `.Trashes`, `Thumbs.db`, `desktop.ini`, `~$*` (Office lock files), common editor temp/swap patterns (`*.swp`, `*~`, `.*.tmp`).
- **User file:** a `.cloudfsignore` with gitignore-style patterns, if wanted. Could be deferred. Note that the ignore file itself would sync (so devices agree), and that ignoring a path that's already synced needs a defined behaviour (stop syncing it locally, don't delete it remotely).

---

## 8. Testing the watcher

- **Real temp directories** (`t.TempDir()`) on the CI runner OS, not a fake filesystem. The bugs are in OS behaviour, so fakes hide them. Run on both macOS and Linux runners.
- **Scenario tests:** simulate editor saves (write temp + rename over original), case-only renames, NFC vs NFD names (macOS), large file written in slow chunks, mass moves of a folder, delete + recreate same name.
- **Overflow:** force `IN_Q_OVERFLOW` by lowering `max_queued_events` in a privileged container, or inject an "overflow" event through the watcher interface; assert that a full rescan happens.
- **Interface seam:** put the OS watcher behind a small interface (`Events() <-chan PathHint`), so the scanner/debouncer logic can be tested with scripted hint sequences, while separate tests cover the real backends.
- **Invariant check:** after any scenario, a full rescan must find disk == baseline (no pending work). Cheap to assert, and it catches missed events.

---

## 9. Recommendation (for discussion, not a decision)

- **Events are hints; the scanner is the truth.** Startup full scan, debounced targeted scans on events, full rescan hourly with jitter, full rescan on overflow/`MustScanSubDirs`.
- **Library:** `rjeczalik/notify` (or Syncthing's fork) for **recursive watching with FSEvents on macOS (cgo) and inotify on Linux**. Accept cgo for macOS builds and build them on a macOS CI runner. Wrap it behind our own interface so it can be swapped (e.g. for fsnotify plus our own recursion, or a pure-Go FSEvents later).
  - **Alternative:** fsnotify everywhere, adding a watch per directory ourselves. Pure Go and the most popular library, but on macOS it's kqueue (one fd per file), which doesn't scale to a large sync folder.
- **Quiet period:** ~5–10s for changes, longer for deletes (Syncthing: 10s / +1 min).
- **Echo suppression via baseline-first updates**, not timing windows.
- **Downloads:** temp in `<root>/.cloudfs/tmp/`, fsync, re-check against baseline, atomic rename, update baseline.
- **Names:** compare and send in NFC; case-fold for collision checks (shared with spike 11).
- **Skip** symlinks (log them), FIFOs, sockets, devices in v1. Built-in ignore list; `.cloudfsignore` optional/deferred.
- **macOS FSEvents history resume:** nice optimisation, defer to after v1 works with startup full scans.

---

## 10. Decisions for the user

1. **Hints + rescans** as the reliability model (startup scan, debounced event scans, hourly full rescan)?
2. **Library:** `rjeczalik/notify`-style recursive watcher with FSEvents via cgo on macOS (needs a macOS CI runner for macOS builds), or pure-Go fsnotify with our own recursion (kqueue on macOS, poor scaling)?
3. **Quiet period values:** ~10s changes / ~60s deletes, or shorter for snappier sync?
4. **Full rescan interval:** hourly (Syncthing's default), or different?
5. **Symlinks:** skip and log in v1, or sync them as links?
6. **User ignore file (`.cloudfsignore`):** v1 or later? Built-in OS-junk defaults either way.
7. **Name normalization:** send names to the server in NFC (and keep local spelling), aligning with spike 11?

---

## References

- fsnotify README (backends table: kqueue on macOS, FSEvents not yet, fanotify not yet; not recursive; inotify limits; kqueue fd per file; atomic-save behaviour; Chmod noise from Spotlight): https://github.com/fsnotify/fsnotify
- rjeczalik/notify source (build tags: FSEvents with cgo, kqueue without): https://github.com/rjeczalik/notify
- Syncthing fork of notify: https://github.com/syncthing/notify
- fsnotify/fsevents: https://github.com/fsnotify/fsevents
- inotify(7) man page (not recursive; new-subdirectory race; IN_Q_OVERFLOW; racy rename pairing; limits): https://man7.org/linux/man-pages/man7/inotify.7.html
- fanotify_init(2) man page (unprivileged since 5.13 / 5.10.220; FID reporting required; inode marks only): https://man7.org/linux/man-pages/man2/fanotify_init.2.html
- inotify max_user_watches default patch (8192 → up to 1,048,576, ≤1% of memory, ~1 KB per watch): https://lkml.iu.edu/hypermail/linux/kernel/2010.3/01777.html. The kernel release it landed in (believed 5.11) was not confirmed.
- Apple, File System Events Programming Guide (coalescing, `MustScanSubDirs`, dropped events, `sinceWhen`): https://developer.apple.com/library/archive/documentation/Darwin/Conceptual/FSEvents_ProgGuide/UsingtheFSEventsFramework/UsingtheFSEventsFramework.html
- Apple, APFS FAQ (case-insensitive default on macOS; preserves case and normalization; normalization-insensitive since High Sierra): https://developer.apple.com/library/archive/documentation/FileManagement/Conceptual/APFS_Guide/FAQ/FAQ.html
- Syncthing docs, syncing (watcher on by default, hourly full scans with ¾–5⁄4 jitter, 10s accumulation, deletes +1 min): https://docs.syncthing.net/users/syncing.html
- Syncthing config docs (`rescanIntervalS`, `fsWatcherEnabled`, `fsWatcherDelayS`): https://docs.syncthing.net/users/config.html
- **From prior knowledge, not re-checked:** FSEvents per-file events flag (10.7+) and extended data with inode; specific editor save behaviours (vim, JetBrains, NSDocument, Office); Dropbox/Nextcloud scan strategies; Syncthing symlink handling; Windows ReadDirectoryChangesW/USN details; stat-walk timing estimate.
