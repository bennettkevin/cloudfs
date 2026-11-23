# Spike 05: Deletes and renames

- **Issue:** #5
- **Status:** Decided. See [ADR-0012](../adr/0012-deletes-tombstones-and-trash.md) (tombstone stubs forever, 30-day trash, same-ID restore, single-change folder deletes and moves, plain local deletes) and [ADR-0013](../adr/0013-move-detection-and-mass-delete-guard.md) (move detection path → inode → content hash with a ~60 s delete hold; root marker + 50% / 1,000-file guard). Section 9 recommendations adopted. Syncthing v2.0 delete retention, rclone bisync `--max-delete` default and Syncthing `.stfolder` re-checked on 2026-11-23.
- **Question:** How are deletes and renames represented, how long is delete information kept, how does a device recognise that a local delete + create was really a move, and how do we stop a mistake (an unmounted folder, a bad script) from deleting everything?

---

## Constraints already decided

- Files and folders have stable server-assigned IDs; path = `parent_id` + `name` ([ADR-0004](../adr/0004-stable-file-ids.md)).
- Change log with per-device cursors, 90-day retention, "cursor too old" full reconcile, and an epoch that changes on database restore ([ADR-0003](../adr/0003-sync-model.md)).
- Separate `content_rev` and `location_rev` per node ([ADR-0006](../adr/0006-separate-content-and-location-revisions.md)).
- Edit beats delete; deleting a folder while a file inside it changed restores only the folders needed to hold that file; deletes are tombstones ([ADR-0005](../adr/0005-conflict-resolution.md)).
- Version history = retained revisions with encrypted manifests; GC removes chunks unreferenced for more than 24 h ([ADR-0010](../adr/0010-chunk-storage-and-manifests.md)).
- Goal 4: no silent data loss. Goal 5: previous versions retained for a configurable period.
- Names may be encrypted (spike 11 pending), so the server may not be able to read names.

---

## 1. Tombstones

### What a tombstone is

A **tombstone** is the record that says "this node existed and was deleted". The node's row stays in the database with `deleted = true` instead of disappearing. Without it, a device that was offline can't tell "the server deleted this file" from "the server never heard of this file".

A delete commit (from the device that deleted the file):
1. Sends node ID + base `content_rev` (deletes check `content_rev`, so an edit the deleting device never saw causes a rejection and "edit wins", ADR-0005/0006).
2. Server sets `deleted = true`, `deleted_at = now()`, bumps `content_rev`, appends `changes(op = delete)`.
3. The node's revisions (and so its chunks) are **kept**, because that's what makes undelete possible (section 3).

### How long to keep tombstones: the resurrection problem

Picture a device that's been offline longer than the 90-day log retention. It comes back, gets "cursor too old", and does a full reconcile: it compares its local files, its baseline, and the server's listing.

For a file that's **in the baseline and on disk, but not in the server's listing**, there are three possible stories:

| Story | Correct action |
|---|---|
| A. Another device deleted it, and the tombstone was later purged | Delete it locally |
| B. The server database was restored from a backup that predates the file | Re-upload it (ADR-0003 accepted this) |
| C. It was deleted on the server, **and** this device edited it while offline | Keep it: edit beats delete |

If tombstones are purged, A and B look identical from the server listing alone. Guessing wrong either way is bad: guess B for case A and a deleted file **comes back** (resurrection); guess A for case B and a real file is deleted.

**The epoch already separates them:**
- **Epoch changed** → story B → re-upload (the accepted ADR-0003 behaviour).
- **Epoch unchanged** → the server never forgets a node without first tombstoning it, so the node was deleted and later purged → story A, unless the local copy differs from the baseline (local hash ≠ baseline hash), which is story C → re-upload as a new node (edit wins).

So purging tombstones is *safe* as long as that rule is implemented exactly. The risk is in the implementation: it's a rarely exercised code path that decides whether to delete a user's file.

**The simpler alternative: never fully forget.** After the trash period (section 3), purge a deleted node's revisions and its encrypted name, but keep a **stub row** (ID, `deleted_at`, `purged_at`, ~tens of bytes). Then the server listing can always say "N was deleted" explicitly, and full reconcile never has to infer it. Storage cost for one user is tiny (a million lifetime deletes ≈ tens of MB of stubs).

### What others do

- **Syncthing:** deleted files are marked with a deleted flag in the index. Until v2.0 they were kept forever; **v2.0 changed this so deleted items are forgotten after six months** (configurable via `--db-delete-retention-interval`, 0 = forever), to bound database growth. Syncthing is peer-to-peer, so every device stores every tombstone, which is why growth mattered. CloudFS keeps them only on the server.
- **Dropbox:** deleted files are recoverable for **30 days** (Basic, Plus, Family), **180 days** (Professional, Essentials, Standard, Business) or **365 days** (Advanced, Business Plus, Enterprise, Education).
- **Nextcloud:** trash bin default `auto`: keeps files for **at least 30 days**, then deletes them when space is needed. Configurable minimum and maximum days.

---

## 2. Folder deletes

Deleting a folder with 10,000 files inside. Two ways to record it:

| | One tombstone on the folder (implicit) | Recursive tombstones (mark every descendant) |
|---|---|---|
| Change log | 1 entry | 10,001 entries if each is logged |
| "Is this node live?" query | Must walk up the ancestors (recursive query) | Read one column |
| Restore | Revive one row | Revive every descendant marked by this delete |
| Offline edit to a descendant | Rejected by the "live parent" invariant | Rejected by stale `content_rev` |

**Hybrid (common pattern):** log **one** `changes(op = delete)` entry for the folder, but in the same transaction mark every descendant `deleted = true` with `deleted_by = <seq of the folder delete>`. The log stays small (devices process one entry and delete the subtree), liveness queries stay simple, and restore can revive exactly the set that delete removed (not files that were deleted separately earlier). Marking 10k–100k rows in one Postgres transaction is a single `UPDATE` over a recursive query: seconds at most *(estimate, not benchmarked)*.

**Applying a remote folder delete on a device:**
1. Walk the local subtree.
2. Files whose local hash matches the baseline (no unsynced edits): delete.
3. Files that differ from the baseline, or new local files not in the baseline at all: **keep** (content wins, ADR-0005). The device commits a restore of the folder chain needed to hold them, plus the files.
4. Remove now-empty folders.

---

## 3. Trash and recoverability

### Trash is separate from version history

- **Version history (goal 5):** old *revisions of a live file*. "Give me yesterday's version of `notes.md`."
- **Trash:** *deleted nodes* that can be undeleted. "Bring back the folder I deleted last week."

Both are retained revisions under the hood, but with separate retention settings, because users think about them differently (Dropbox, Nextcloud and OneDrive all expose them separately).

### Lifecycle of a deleted node

```
live ──delete──► in trash (deleted=true, revisions kept)
                    │  restore within trash period
                    ├──────────────────────────────► live (same node ID)
                    │  trash period expires
                    ▼
                 purged (revisions dropped → chunks unreferenced → GC after 24 h grace)
                    │
                    ▼
                 stub (ID + timestamps only) — or removed entirely, see section 1
```

### How restore works

**Revive the same node ID** rather than creating a new node:
- `deleted = false`, `content_rev + 1` with a new revision row pointing at the last manifest, `changes(op = restore)`.
- Version history stays attached to the file.
- **Location:** original parent if it's live; otherwise restore the parent chain (as in ADR-0005's folder rule) or place it at the root. If the name is now taken, the name-collision rule applies (conflicted-copy name).
- For a folder deleted with the hybrid scheme, restore revives every descendant with that `deleted_by`.

A new node on restore would be simpler but breaks the link to version history and to any device that still has the old ID in its baseline.

**Encrypted names (spike 11):** the server can't show a readable trash listing; `cloudfs trash list` decrypts names on the client. The server just returns deleted nodes and their encrypted names.

### Retention relationships

To stay consistent, retention settings need:
- **trash period ≤ tombstone (or stub) retention** — you can't restore something the server has forgotten.
- **tombstone retention ≥ change log retention (90 days)** — otherwise a device that can still replay the log could receive a change referring to a node the server has already forgotten. Spike 02 noted the same rule.

---

## 4. Renames and moves on the server

- A rename or move is **one commit**: base `location_rev`, new `name` and/or `parent_id`; server bumps `location_rev` and appends **one** `changes(op = move)` entry.
- **Moving a folder is one change** regardless of size: descendants' rows are untouched, because their paths are derived from `parent_id` chains. This is the payoff of ADR-0004.
- **Server checks** (already in ADR-0005): target name unique in the new folder (case-insensitive, or via blind index if names are encrypted), new parent live, no cycle.
- **Rename onto an existing name:** the unique constraint rejects it; the client applies the name-collision rule (its node gets a conflicted-copy name).

**Applying a remote folder move on a device:** one OS rename of the local directory (atomic within one filesystem), then update the baseline. If the baseline stores each node's **`parent_id` + name** rather than its full path, a folder move updates one baseline row, not every descendant. Recommended for that reason.

---

## 5. Client-side move detection (matching logic)

Spike 06 covers *how* the OS reports changes. This section covers what the client does with "path P disappeared" and "path Q appeared" once it has them.

### Why it matters

Operating systems and scans often show a rename as a delete plus a create. If the client sends that as-is:
- No data is lost (the create uploads the file).
- Re-upload is nearly free: chunks are content-addressed, so the "which chunks are missing?" check returns none and zero bytes are uploaded.
- **But** version history and trash links are lost (the new node starts fresh), other devices delete and re-create the file (and may re-download it), and a folder move becomes thousands of separate changes.

So move detection is an efficiency and history feature, not a safety feature. **A failed detection is harmless; a wrong detection is the thing to avoid.**

### Matching order

For each new local path Q:
1. **Same path as a baseline entry** → it's that node. If the inode changed, it's a **modify**, not a move. This is the atomic-save case: many editors save by writing a temp file and renaming it over the original, which gives the same path a new inode.
2. **Inode (+ device ID) matches a baseline entry whose path disappeared** → **move** of that node. Inodes survive renames within one filesystem on Linux and macOS (APFS).
3. **No inode match** (cross-filesystem move, a tool that copies then deletes, a filesystem without stable inodes): if exactly **one** disappeared baseline entry has the **same size and the same content hash** (the keyed chunk-ID root, ADR-0009) → treat as a move.
4. Otherwise → **create**. Disappeared entries left unmatched → **delete**.

### Guards

- **Uniqueness:** if two disappeared entries could match (identical files, hard links sharing an inode), don't guess: fall back to delete + create.
- **Hold deletes briefly** so a matching create can arrive. Syncthing's watcher accumulates changes for 10 s and **delays deletes a further 1 minute** for this kind of reason. CloudFS can hold local deletes for a few seconds to a minute before committing.
- **Folder moves:** match the folder by its inode and commit **one** move for the folder; descendants that kept their relative path inside it need nothing.
- **Copy then delete the original** matched by content hash: results in a move, which is the same end state, so a harmless false positive.

### Interaction with remote deletes

If another device's delete and this device's move cross, the move commit targets a tombstoned node → rejected → ADR-0005 applies (content wins: restore at the new location).

---

## 6. Mass-delete safety

The classic sync disaster: the sync folder is on an unmounted drive, or a script empties it, and the client faithfully propagates "everything was deleted" to every device.

What others do:
- **Syncthing:** keeps a marker directory, `.stfolder`, in each synced folder. If it's missing (e.g. a USB drive was removed or a filesystem unmounted), Syncthing assumes something is wrong and **stops syncing that folder** instead of treating missing files as deletions.
- **rclone bisync:** `--max-delete PERCENT`, a safety check that **aborts the run if more than 50% (default) of files would be deleted**, explicitly to protect against "a failed listing being interpreted as all the files were deleted". `--force` overrides.
- **Dropbox, OneDrive:** show mass-delete warnings *(not verified in this spike)*.

Options for CloudFS (they stack):
1. **Root marker:** a marker inside the sync root (e.g. `.cloudfs-root`). If it's missing, stop syncing and alert; never treat it as deletes.
2. **Threshold guard:** if a single scan would delete more than X% of files or more than N files (e.g. 50% or 1,000), pause local delete propagation and require `cloudfs confirm-deletes`.
3. **Server trash** (section 3): even if deletes do propagate, everything is recoverable for the trash period.

**Applying remote deletes:** with the edit check in section 2 and server trash, a device can delete local files normally when it receives a remote delete. Moving them to the OS trash instead would be extra safety at the cost of clutter; optional.

---

## 7. Data model additions

**Server**

| Change | Purpose |
|---|---|
| `nodes.deleted_at` | When the node entered the trash |
| `nodes.deleted_by` | `seq` of the delete that tombstoned it (folder cascade + exact restore) |
| `nodes.purged_at` | Revisions purged; row is now a stub (if stubs are kept) |
| `changes.op` gains `restore` | Undelete is a logged change devices apply like a create |
| Settings: `trash_retention` (e.g. 30 days), `tombstone_retention` (≥ max(trash, log retention), or "forever" for stubs), `version_retention` (goal 5, still to set) | |

Purge itself doesn't need to be a logged change: devices already removed the file when they applied the delete.

**Client baseline**

| Change | Purpose |
|---|---|
| `inode`, `device_id` per node | Move detection (section 5) |
| Store `parent_id` + `name` instead of full path | Folder moves touch one baseline row |
| Pending-delete queue with timestamps | Hold local deletes briefly for move pairing |

---

## 8. Interactions with other spikes

- **Spike 06 (file watching):** supplies the events and inode/mtime data that section 5 consumes; atomic-save, case-only renames (`notes.md` → `Notes.md` on macOS) and dropped events are watcher issues. Delete holding/debouncing should be designed together.
- **Spike 11 (name encryption):** trash listing and restore names are decrypted client-side; rename uniqueness uses the blind index.
- **ADR-0010 (GC):** purge drops revision refs; chunks become unreferenced and are deleted after the 24 h grace period.
- **Goal 5 (version retention):** still needs a default value; trash and version retention should be set together.

---

## 9. Recommendation (for discussion, not a decision)

- **Tombstones:** delete = tombstone with revisions kept. After the trash period, purge revisions and the name but **keep a stub row indefinitely** (optional compaction setting for very large deployments). Also implement the epoch-based reconcile rule from section 1 as a fallback. Reasoning: removes the riskiest inference ("was this deleted or lost?") from the code path that deletes user files, at negligible storage cost for one user.
- **Trash:** yes, separate from version history; **30 days** default, configurable; restore **revives the same node ID**.
- **Folder deletes:** one log entry + cascade marking descendants with `deleted_by` in the same transaction.
- **Renames/moves:** one `location_rev` bump; client baseline stores `parent_id` + `name`.
- **Move detection:** path → inode → unique size + content-hash match → else delete + create; hold local deletes for a short window (start around 10–60 s, tune in spike 06).
- **Mass-delete safety:** root marker + threshold guard (50% / 1,000 files, configurable) requiring confirmation, on top of server trash.
- **Remote deletes applied locally:** normal delete after the unsynced-edit check; OS trash optional later.

---

## 10. Decisions for the user

1. **Tombstone lifetime:** keep minimal stubs indefinitely (recommended), or expire tombstones after a period (≥ 90 days, e.g. Syncthing's 6 months) and rely on the epoch reconcile rule?
2. **Trash:** have one, separate from version history? Default period (30 days recommended)?
3. **Restore:** revive the same node ID (recommended) or create a new node?
4. **Folder delete representation:** one log entry + cascade marking (recommended), implicit only, or fully recursive?
5. **Move detection:** path → inode → content-hash fallback, with a short delete-holding window. OK as the approach (exact timings in spike 06)?
6. **Mass-delete guard:** root marker + threshold with confirmation? What threshold?
7. **Remote deletes locally:** plain delete, or move to the OS trash?

---

## References

- Syncthing v2.0.0 release notes (deleted items forgotten after six months; `--db-delete-retention-interval`; deletes can now win conflict resolution): https://github.com/syncthing/syncthing/releases/tag/v2.0.0
- Syncthing FAQ ("folder marker missing": `.stfolder` safety check stops syncing if the folder is unmounted; moving files between two Syncthing folders isn't specially handled and is mostly re-downloaded): https://docs.syncthing.net/users/faq.html
- Syncthing, Understanding Synchronization (watcher accumulates changes for 10 s; deleted files delayed a further 1 min): https://docs.syncthing.net/users/syncing.html
- Dropbox, recover deleted files (30 / 180 / 365-day recovery periods by plan): https://help.dropbox.com/delete-restore/recover-deleted-files-folders
- rclone bisync (`--max-delete` default 50%, "excessive deletes" protection against a failed listing, `--force`, `--check-access`): https://rclone.org/bisync/
- Nextcloud config reference, `trashbin_retention_obligation` (default `auto`: at least 30 days, then deleted when space is needed): https://docs.nextcloud.com/server/latest/admin_manual/configuration_server/config_sample_php_parameters.html

**From prior knowledge, not verified in this spike:**
- Inodes being stable across renames on APFS and Linux filesystems, and atomic-save editors producing a new inode at the same path.
- Dropbox/OneDrive mass-delete warnings.
- Postgres cascade-update timing for 10k–100k rows (estimate).
- Stub storage estimate (~tens of bytes per row).
