# Spike 02: Conflict resolution

- **Issue:** #2
- **Status:** Decided. See [ADR-0005](../adr/0005-conflict-resolution.md) (resolution) and [ADR-0006](../adr/0006-separate-content-and-location-revisions.md) (separate revisions). Section 7 recommendations were adopted as written.
- **Question:** When the server rejects a commit because the base revision is stale (ADR-0003), or two devices make incompatible structural changes, what happens so that no edit is silently lost?

---

## Where this spike starts

[ADR-0003](../adr/0003-sync-model.md) already decides **detection**: a commit carries the revision it was edited from, and the server rejects it if the file has moved on. [ADR-0004](../adr/0004-stable-file-ids.md) makes name and parent folder attributes of a stable node ID, and live `(parent_id, name)` must be unique.

This spike decides **resolution**: what the client and server do after a rejection, for every kind of conflict, under the design doc's goal 4, "no silent data loss".

Two kinds of conflict exist, and they behave differently:

- **Content conflicts:** two devices changed the bytes of the same file. Only the user can say which content is right.
- **Structure conflicts:** renames, moves, deletes and name collisions. Most of these can be resolved automatically with a fixed rule, because no bytes are lost either way.

---

## 1. Resolution strategies

### Conflicted copy (keep both)

Keep one version under the original name and save the other next to it under a new name. Every major sync product does this.

| Product | Naming (verified unless noted) | Which version gets renamed |
|---|---|---|
| Dropbox | Editor's name, "conflicted copy", and the date are added to the filename. The help page gives no exact grammar or example. | "The last version saved will always appear as the conflicted copy." |
| Nextcloud desktop | `mydata (conflicted copy 2018-04-10 093612).txt` | The **local** version is renamed; the remote version keeps the original name. Conflict files are **not uploaded** by default (an experimental option enables it). |
| Syncthing | `<filename>.sync-conflict-<date>-<time>-<modifiedBy>.<ext>` | The version with the **older** modification time; ties broken by device ID. Conflict copies sync to all devices like normal files. |
| OneDrive | Computer name appended, e.g. `MyFile-ComputerName.txt`. **Unverified:** seen only in third-party and university IT pages; Microsoft's own page didn't confirm it. | The local copy (unverified). |

- **Pros:** never loses bytes, needs no file-format knowledge, and users already recognise it.
- **Cons:** clutter. The user has to notice the copy, compare it, and clean up.

### Last writer wins (LWW)

The newest edit overwrites the other. The "loser" is discarded.

This **violates goal 4 by definition**: one edit disappears without the user being told. "Newest" is also unreliable, because device clocks drift and modification times can be preserved or faked by copy tools. Syncthing uses modification time only to pick *which copy gets renamed*, never to discard one.

LWW is acceptable for **metadata where nothing is lost**, such as rename/rename (both names are just labels for the same bytes). It isn't acceptable for content.

### Automatic merge (three-way)

Given the **base** version (from the client's synced baseline), "mine" and "theirs", a merge tool like `diff3` combines non-overlapping edits. Git works this way.

- **Safe:** line-based plain text, edits in different regions, and you still have the base content.
- **Unsafe:** any binary format (images, PDFs, Office documents, which are zip files), overlapping edits, structured text where a line merge can produce invalid output (JSON, YAML), and anything with no base available.
- **Requires the base content**, not just the base revision. CloudFS would need to keep the content of each file's last-synced version (on the server via version history, or locally).
- Unison supports this as opt-in per path pattern (`merge = Name *.txt -> diff3 ...`); it is not the default.

A good v2 enhancement, too risky as v1 default behaviour.

### Prompt the user

Stop and ask the user which version to keep. Unison's default works this way: in interactive mode it asks, and in `batch` mode "conflicts will be skipped" (left unsynced until resolved).

- **Pros:** the user decides with full information.
- **Cons:** a background daemon has nobody to ask in the moment. The file stays out of sync until the user acts, and other devices keep seeing the old state.

For a v1 CLI daemon, prompting fits best as an **optional** extra (`cloudfs conflicts` to list and resolve), not as the mechanism that keeps data safe.

### Server version history as a safety net

The server keeps previous revisions of each file (design doc goal 5). Any strategy that loses something can then be recovered from history. Unison's `backup` option and OneDrive's guidance to "restore from version history" use the same idea.

This isn't a resolution strategy on its own, but it changes the risk of the others. With history retained, even a wrong automatic choice can be undone. It also needs a decision on whether the rejected version gets committed to history.

---

## 2. Conflict case matrix (ID-based model)

Terminology: device **A** commits first and is accepted, then device **B** commits from the same base and is rejected (or its commit no longer makes sense). "CC" = conflicted copy.

| # | Case | How it's detected (ADR-0003/0004) | Suggested handling | Notes |
|---|---|---|---|---|
| 1 | **Edit / edit** (same node) | B's base revision ≠ current. Rejected. | **CC.** B downloads A's version under the original name and commits its own version as a **new node** named as a conflicted copy. | The only true content conflict. If content hashes are identical, there's no conflict: just fast-forward the baseline. |
| 2a | **Edit, then delete** (A edits, B deletes the old revision) | B's delete carries a stale base. Rejected. | **Edit wins.** B restores the file locally with A's content. | A delete is "I don't need this version"; it shouldn't destroy an edit B never saw. |
| 2b | **Delete, then edit** (A deletes, B edits) | B's edit targets a tombstoned node. Rejected. | **Edit wins.** B's version is committed as a new node (or the tombstone is revived) at the original path. | Syncthing can let the delete win (it uses its normal modification-time rule); when it does, the modified file is kept as a conflict copy. Both avoid loss; "edit wins" leaves less clutter. |
| 3 | **Delete / delete** | B deletes an already-tombstoned node. | **No conflict.** Treat as already applied; update B's baseline. | Make the server idempotent here. |
| 4 | **Rename / rename** (same node, different names) | B's rename has a stale base revision. | **First committed wins** (A's name). B renames locally to match. Optionally log it. | Pure metadata, no bytes lost. Making a copy here would duplicate data for nothing. |
| 5 | **Edit / rename** (A edits content, B renames) | Different attributes of the same node. | **Not a conflict.** Merge both: new name + new content. | Needs the server to compare *which attributes* changed, not just the revision number (see section 3). This is the payoff of ADR-0004. |
| 6 | **Move / move** (same node into different folders) | Stale base on B's move. | **First committed wins**, like rename/rename. | Same reasoning: metadata only. |
| 7 | **Name collision** (two different nodes created with the same name in the same folder) | Unique `(parent_id, name)` constraint fails on B's create. | **Rename B's node**, e.g. `report (conflicted copy, laptop, 2026-11-20).txt`. | Not a revision conflict; two separate files want one name. Both are kept. Includes "file vs folder with the same name". |
| 8 | **Delete folder / add or edit inside it** | B's child commit names a tombstoned `parent_id`. | **Content wins.** Restore the folder (or the path down to the edited file) so B's file has a home. Other deleted children stay deleted. | The most common way real sync tools lose data. Restoring only the path needed keeps the rest of A's delete. |
| 9 | **Move folder into its own descendant** (A moves X into Y; B moves Y into X) | Each is valid alone. Together they make a cycle. The server must check "is the new parent a descendant of this node?" in the commit transaction. | **Reject the second move**; B undoes it locally and logs it. | Server-side validation the data model must include. Never happens with path-based sync, but is possible with IDs. |
| 10 | **Case-only names** (`Notes.md` and `notes.md` in one folder) | Linux (case-sensitive) allows both; macOS APFS by default and Windows don't. The server needs a policy. | **Option 1:** server enforces case-insensitive uniqueness, so case 7 handling applies. **Option 2:** allow on the server, and macOS/Windows clients rename one locally with a marker. Dropbox's desktop app appends "(Case Conflict)". | Also covers a case-only rename (`notes.md` → `Notes.md`) on macOS, which some watchers report oddly (spike 06). Unicode normalisation (macOS NFD vs Linux NFC) is a related trap. |
| 11 | **File vs folder with same name** | Unique `(parent_id, name)` fails. | Same as case 7: rename the later one. | A folder can't be "a conflicted copy" in a meaningful way, so rename the file side when possible. |

**Pattern:** if bytes could be lost, keep both (content wins over delete, both edits kept). If only names or locations are at stake, a deterministic rule (first committed wins) is enough. The server always validates structural invariants: unique names, no cycles, live parent.

---

## 3. Who resolves: client or server

### Client-side resolution

The server only detects and rejects. The client that lost the race does the work. For edit/edit, it downloads the winning version, renames its own copy, and commits the copy as a new node.

- **Pros:** the server stays simple (it validates and rejects). Conflicted copies are ordinary nodes in the change log, so devices not involved in the conflict see an ordinary "new file" change and need no special logic.
- **Cons:** each client must implement resolution correctly. If a client crashes mid-resolution, it must resume safely; the local baseline plus re-pulling makes that workable.

### Server-side resolution

The server accepts both commits and stores the loser as a conflict revision, or creates the conflicted-copy node itself.

- **Pros:** one implementation. Works even if a client is buggy or old.
- **Cons:** the server needs naming policy and device names, and the "reject on stale base" rule from ADR-0003 becomes "accept and fork", which changes the protocol. Harder to reason about.

### Attribute-level comparison (needed either way for case 5)

A single per-node revision can't tell "content changed" from "name changed". Two ways to allow edit/rename without a false conflict:

- **Server-side rebase:** on rejection, the server compares B's change to the changes since B's base. If they touch different attributes (content vs name/parent), it applies B's change anyway.
- **Separate revisions per attribute group** (content revision, location revision), each compared independently.

Either is a data model detail for §9 and §10 of the design doc.

**Fit with CloudFS:** client-side resolution matches ADR-0003 as written. The server stays the single judge of *whether* there is a conflict, and the client decides *what to do*, using only normal commits. Any device that isn't involved just pulls ordinary changes from the log.

---

## 4. Naming and avoiding conflict loops

**Naming format options:**
- Dropbox-style, human-first: `notes (conflicted copy, laptop, 2026-11-20).md`
- Syncthing-style, machine-parseable: `notes.sync-conflict-20261120-153012-LAPTOP.md`

**Requirements whatever the format:**
- Keep the **original extension last**, so the file still opens in the right app.
- Include the **device name and timestamp**, so the user knows where it came from.
- Make the suffix **detectable**, so the client can list conflicts and the name doesn't nest (`x (conflicted copy) (conflicted copy).md`).

**Loop risks and guards:**
- **Conflicted copies are new nodes**, created from nothing, so they can't themselves conflict on revision. Collisions with other conflicted-copy names get a counter.
- **Identical content isn't a conflict.** Compare content hashes before creating a copy. Many real "phantom conflicts" (e.g. Dropbox forum reports of conflicted copies for files nobody edited) come from metadata-only changes or re-saves with no content change.
- **Don't resolve the same rejection twice.** Retry logic after a network error must check whether the conflicted copy already exists (idempotency key, or check by content hash).
- **Upload or keep local?** Nextcloud keeps conflict files local by default; Syncthing and Dropbox sync them everywhere. Syncing them is simpler and fits "every device has the same view", but spreads clutter.

---

## 5. Notifying the user

Proportionate options for a v1 CLI daemon:

| Option | Effort | Notes |
|---|---|---|
| Filename only | None | Easy to miss. |
| Structured log line (`level=warn event=conflict node=... copy=...`) | Low | Needed anyway for debugging and observability (phase 3). |
| `cloudfs status` / `cloudfs conflicts` command | Low–medium | Lists unresolved conflicted copies (found via the detectable suffix). |
| OS notifications | Medium | Platform-specific; nice later. Nextcloud uses system notifications plus a tray badge. |

Filename + log + a `conflicts` command covers v1. OS notifications fit with a later GUI or tray app.

---

## 6. Interactions with other spikes

- **Spike 05 (deletes and renames):**
  - Cases 2b and 8 need tombstones that last long enough for an offline edit to be matched to its deleted node. Tombstone retention ≥ log retention (90 days) is the natural rule.
  - "Revive tombstone" vs "create new node" for delete-then-edit is a spike 05 detail.
- **Spike 06 (file watching):**
  - If the user edits a file while the client is downloading a newer version over it, the client must not overwrite the local edit. Download to a temp file, re-check the local hash against the baseline right before an atomic rename, and treat a mismatch as a conflict.
  - Case-only renames and Unicode normalisation (case 10) show up as watcher quirks.
- **Spike 04 (chunking):** a conflicted copy with the same content as other files reuses existing chunks, so keeping both versions costs little storage.
- **Version history (goal 5):** retained revisions are the safety net for any automatic choice (rename/rename, first-move-wins). If history exists, structure-conflict rules can be simpler because nothing is unrecoverable. Decide whether conflicted copies count against history retention.
- **Spike 03 (encryption):** client-side three-way merge works with end-to-end encryption. Server-side resolution or merge would not, since the server can't read content.

---

## 7. Recommendation (for discussion, not a decision)

- **Client-side resolution**, keeping ADR-0003's "server detects, rejects".
- **Content conflicts:** conflicted copy. **Edit beats delete.** Identical content isn't a conflict.
- **Structure conflicts:** deterministic rules (first committed wins for rename/move), server-validated invariants (unique name, no cycles, live parent), and restoring only the path needed for delete-folder/edit-inside.
- **Edit/rename merged** via attribute-level comparison.
- **Conflicted copies sync** to all devices, with a Dropbox-style readable name plus a detectable suffix.
- **Notify** via log + `cloudfs conflicts` command.
- **Defer:** three-way merge (opt-in by file pattern) and OS notifications to later phases.

Reasoning: this loses no bytes, keeps the server simple, needs no file-format knowledge, and every resolution is an ordinary commit that other devices handle with no special logic.

---

## 8. Decisions for the user

1. **Who resolves:** client-side (fits ADR-0003) or server-side?
2. **Edit vs delete:** edit wins (restore the file), or delete wins with a conflicted copy (Syncthing's approach)?
3. **Which version keeps the original name in edit/edit:** the first committed (server's) version, or the local one? Dropbox and Nextcloud both keep the remote/first version under the original name.
4. **Structure conflicts:** OK with "first committed wins" for rename/rename and move/move, relying on logs and history instead of copies?
5. **Case sensitivity:** enforce case-insensitive name uniqueness on the server (simplest across macOS/Linux/Windows), or allow it and handle it per client?
6. **Conflicted copies:** sync them to every device, or keep them local to the device that lost the race?
7. **Naming format:** human-readable (Dropbox-style) or machine-parseable (Syncthing-style)?
8. **Three-way merge:** deferred, or opt-in for text files in v1?

---

## References

- Syncthing, "Understanding Synchronization", conflicting changes (naming format, older mtime renamed, modify vs delete, conflict copies propagate): https://docs.syncthing.net/users/syncing.html
- Dropbox, "What's a conflicted copy?" (username + "conflicted copy" + date; last save becomes the copy): https://help.dropbox.com/organize/conflicted-copy
- Dropbox, "What's a file case conflict?" ("(Case Conflict)" suffix on desktop): https://help.dropbox.com/installs-integrations/sync-uploads/case-conflict
- Nextcloud desktop client manual, conflicts (naming example, local version renamed, not uploaded by default, notifications): https://docs.nextcloud.com/server/stable/user_manual/pt_BR/desktop/conflicts.html (Portuguese page; English version didn't load)
- Unison manpage (`batch`: conflicts skipped; `prefer`; `merge`; `backup`/`maxbackups`): https://www.mankier.com/1/unison
- OneDrive appending the computer name on conflict: **unverified**. Only third-party/university IT pages, e.g. https://cts.gordon.edu/home/knowledge-base/onedrive-migration-sync-errors/. Microsoft's "Why has my filename changed?" page does not mention it.
- Unicode normalisation (NFD/NFC) on macOS vs Linux and three-way merge behaviour of `diff3`: from prior knowledge, not re-read.
