# Spike 01: Sync model

- **Issue:** #1
- **Status:** Decided. See [ADR-0003](../adr/0003-sync-model.md) (sync model) and [ADR-0004](../adr/0004-stable-file-ids.md) (file identity).
- **Question:** How do devices find out what changed, and how do we detect that two devices changed the same file at the same time?

---

## The problem in one paragraph

Every device keeps a copy of the folder. When something changes on one device, the others need to learn about it, fetch it, and apply it. The hard part isn't moving bytes. It's answering two questions correctly every time:

1. **"What changed since I last looked?"** This is change discovery.
2. **"Did someone else change this file while I was also changing it?"** This is concurrency detection.

If question 2 gets a wrong "no", one device silently overwrites another's edit. That breaks goal #4 in the design doc (no silent data loss).

There's a third, quieter question that every model has to answer. Dropbox calls the answer the "synced tree":

3. **"What did this file look like the last time this device and the server agreed?"** Without that baseline, you can't tell "the server added this file" from "I deleted this file locally". Both show up as "the server has it and I don't".

---

## Option A: server change log + per-client cursor

### How it works

- The server keeps an **append-only log** of changes in Postgres. Every accepted change (create, modify, delete, move) gets the next number from one global sequence: 1, 2, 3, …
- Each client remembers one number: its **cursor**, the last sequence it has applied.
- To catch up, the client asks "give me everything after cursor N". It applies what it gets and saves the new cursor.
- To wait for changes, the client holds open a long-poll or WebSocket connection, and the server says "something happened, come fetch".

This is the shape of Dropbox's public API: `files/list_folder` returns entries plus a cursor, `files/list_folder/continue` returns changes since the cursor, and `files/list_folder/longpoll` blocks until there's something new.

### How concurrent edits are detected

The log by itself only orders changes. It doesn't detect conflicts. You add one rule: **each file has a revision, and an upload must say which revision it was based on.**

- The client edits `notes.md` while at revision 7 and uploads with "base = 7".
- If the server's current revision is still 7, it accepts, bumps to 8, and appends to the log.
- If another device already moved it to 8, the server rejects with a conflict, and the client handles it (spike 02).

This is compare-and-swap, and it's the same idea as Dropbox's `WriteMode.update(rev)`. Because the server is the single authority, it's the only place that needs to judge "concurrent or not", and judging is a simple integer comparison.

### State

| Where | What it stores |
|---|---|
| Server | `files` table (current state of each path or file ID: revision, size, chunk list, deleted flag); `changes` log (seq, file ID, revision, op) |
| Client | Its cursor; for each file, the **revision it last synced** plus the local hash or mtime at that moment (the "synced" baseline, used to spot local edits) |

### Offline for weeks

The client comes back and asks "everything after cursor N". If the log still goes back that far, it replays and it's done. If the log was **compacted** (old entries pruned to save space), the server answers "cursor too old, reset". The client then does a full listing and compares against its baseline. Dropbox's API has exactly this: a `reset` error on a stale cursor. You decide how long to keep log history; it's a retention knob, like the delete retention in spike 05.

### Server restored from backup

This is the dangerous case. Clients may hold cursors and revisions *newer* than what the restored server has. If the server simply resumes numbering, sequence numbers get reused for different changes, and a client might skip real changes. Standard mitigation: store a random **epoch / log ID** with the log, generated fresh on restore. Clients save it with their cursor; if it doesn't match, they discard the cursor and do a full reconcile against their baseline. Syncthing does the same thing with its "Index ID" (see below).

### Complexity for a solo Go learner

**Lowest of the three.** It's mostly CRUD plus one transaction per change: check the base revision, update the file row, insert the log row. Postgres gives you the sequence (`BIGSERIAL` or `GENERATED ... AS IDENTITY`) and transactional guarantees for free. The client logic is "fetch since cursor, apply, save cursor" plus the local baseline comparison. This is the model most hosted sync services and most sync APIs look like.

**Gotcha:** sequence numbers come from a Postgres sequence, and those are allocated before commit. Two transactions can commit out of order (seq 11 visible before seq 10), and a client reading "after 9" at the wrong moment could see 11, set its cursor to 11, and miss 10 forever. Fixes: serialize log writes (a single row lock or advisory lock, which is plenty for one user) or only serve entries up to the lowest in-flight sequence. Worth a test case.

---

## Option B: per-file version vectors (Syncthing-style)

### How it works

- There's **no central authority**. Every device is an equal peer.
- Each file carries a **version vector**: a small map of `device → counter`. When device A edits the file, it increments its own counter: `{A:3, B:1}` → `{A:4, B:1}`.
- Devices exchange indexes (lists of files with their vectors) and compare.

Syncthing's protocol (BEP) works this way: "The version field is a version vector describing the updates performed to a file by all members in the cluster." It's also how Riak and Dynamo-style databases track concurrent writes.

### How concurrent edits are detected

Compare two vectors:
- If every counter in X is ≥ the matching counter in Y, X is newer, so take X.
- If X has some counters bigger and Y has others bigger (`{A:4, B:1}` vs `{A:3, B:2}`), **neither saw the other's change**, and it's a true conflict.

This detects concurrency **without needing a server to judge**, which is the whole point. It's mathematically clean, and it works even if devices sync directly with each other in any order.

### Change discovery

Syncthing pairs vectors with a **per-device local sequence number** for efficient catch-up: "The sequence field is the value of a device local monotonic clock at the time of last local database update to a file." Peers remember "I've seen device X up to sequence N" and request only newer entries. That's really one cursor per peer, so Option B still needs Option A's cursor idea for discovery. The vectors solve conflict detection, not discovery.

### State

| Where | What it stores |
|---|---|
| Each device | Full index of every file with its version vector, plus "last sequence seen" per peer |
| Server (if any) | Just another peer with the same data, or a relay |

### Offline for weeks

This works naturally. On reconnect, devices exchange indexes and compare vectors. Vectors keep growing with every device ever used, so you need a way to prune retired devices.

### Server restored from backup (or device database reset)

Syncthing handles this with a random Index ID per folder index. If it changes, peers know the old sequence numbers are meaningless and request a full index. Version vectors themselves still compare correctly, but a restored node with *older* vectors could look like it's missing changes rather than conflicting. It needs care.

### Complexity for a solo Go learner

**Highest.** You implement vector compare/merge, index exchange, per-peer sequence tracking, vector pruning, and convergence when three or more devices sync in different orders. The payoff is peer-to-peer and offline mesh sync, which CloudFS doesn't need in v1 because there's a central server.

---

## Option C: hybrid, a server log for ordering plus a base revision for conflicts

This is really **Option A as described above**, and it's worth naming because it's what you end up building. In more detail:

- The server log handles **discovery and total ordering**.
- The per-file **base revision** check handles **concurrency detection**, with the server as the only judge.
- The client's local **synced baseline** (last-agreed revision plus hash per file) handles telling local edits from remote ones. This is the role of Dropbox Nucleus's "Synced Tree", which the Dropbox team calls the merge base and "the key innovation that lets us unambiguously derive the correct sync result" ([Testing sync at Dropbox](https://dropbox.tech/infrastructure/-testing-our-new-sync-engine)).

On the client, Dropbox's model keeps three views:
- **Remote tree:** what the server says now.
- **Local tree:** what's on disk now.
- **Synced tree:** where the two last agreed.

Each sync step compares the three. Remote ≠ synced means a remote change; local ≠ synced means a local change; both differ means a potential conflict. It's a useful mental model for the CloudFS client, even if implemented as a single table with columns per view.

You can upgrade to vectors later. If a future phase wants P2P (for example, LAN sync between devices over the phase 4 WireGuard network), per-file revisions can be replaced by vectors without changing the change log.

---

## Side-by-side

| | A / C: server log + base revision | B: version vectors |
|---|---|---|
| Who decides "conflict"? | Server, by integer compare at write time | Any device, by vector compare |
| Change discovery | One cursor per client | One cursor per peer, per peer |
| Needs a central server | Yes | No |
| Client state | Cursor + per-file synced revision/hash | Full index + vectors + per-peer sequences |
| Offline for weeks | Replay log, or reset + full reconcile if compacted | Index exchange |
| Server restore | Epoch/log ID invalidates cursors → full reconcile | Index ID invalidates sequences; vectors need care |
| Fits Postgres + workers | Directly: the log *is* an event stream workers can consume | Awkward: no natural single stream |
| Implementation effort | Low–medium | High |
| Real-world examples | Dropbox API, Google Drive changes API, most hosted sync | Syncthing, Riak/Dynamo |

---

## How this fits CloudFS

- **Central server, single user (ADR-0002).** The main thing vectors buy you is not needing a central judge, and CloudFS has one by design.
- **Postgres metadata.** The change log is one table, and the revision check is one transaction. This plays to Postgres's strengths.
- **Event-driven workers.** The change log doubles as the event source: workers for versioning or pruning can read it the same way clients do, or a trigger/outbox can publish to the queue. This ties the sync model to the phase 1→2 architecture.
- **Lagging devices.** Log retention (how far back cursors stay valid) becomes an explicit operational setting. Worth listing in the design doc's infrastructure section.

### Interactions with other spikes

- **Spike 02 (conflicts):** this spike decides *how a conflict is detected* (base revision mismatch). Spike 02 decides *what happens next* (conflicted copy, merge, prompt). Edit-vs-delete and rename-vs-edit become cases where the base revision check fails.
- **Spike 05 (deletes/renames):** deletes must be log entries, as tombstones with a revision, or offline clients never learn about them. Renames depend on whether files have a **stable file ID** (Dropbox's Nucleus rewrite called the lack of stable IDs the old engine's core flaw) or are keyed by path. Decide the identity model (path vs file ID) alongside this spike. It's arguably the most foundational call here.
- **Spike 04 (chunking):** a file revision points to an ordered list of chunk hashes. Uploading chunks is separate from *committing* a revision: chunks go to S3 first, then one metadata transaction does the revision check and log append. A rejected commit leaves orphan chunks, so garbage collection becomes a worker job.
- **Spike 07 (uploads):** the commit step above is where presigned uploads hand back to the API.

---

## Questions to answer before deciding

1. **Will CloudFS ever sync device-to-device without the server?** For example, LAN sync, or the phase 4 network. If "not in v1, maybe later", A/C now with a path to vectors is reasonable. If "yes, core feature", B deserves more weight.
2. **Are files identified by path or by a stable file ID?** This affects renames (spike 05) and the shape of every table.
3. **How long should a device be able to stay offline and still catch up incrementally?** This sets log retention; after that, a full reconcile.
4. **What happens on server restore?** Are we OK with every client doing a full reconcile (epoch change), given that the local baseline prevents data loss?
5. **Is learning distributed-systems theory (vectors) a goal in itself?** It's a legitimate portfolio angle but costs a lot of time, and you could do it as a later enhancement.
6. **Should workers consume the change log directly, or should the API publish separate events to the queue (outbox pattern)?** This can be deferred to phase 2, but it's worth knowing the log could serve both.

---

## References

- Dropbox, "Rewriting the heart of our sync engine" (Nucleus): https://dropbox.tech/infrastructure/rewriting-the-heart-of-our-sync-engine. Stable file IDs, atomic moves, "changing the foundational nouns of a system is often impossible to do in small pieces."
- Dropbox, "Testing sync at Dropbox" (three trees: remote/local/synced, randomized testing): https://dropbox.tech/infrastructure/-testing-our-new-sync-engine
- Syncthing Block Exchange Protocol v1 (version vectors, sequence numbers, Index ID, Index/IndexUpdate): https://docs.syncthing.net/specs/bep-v1.html
- Dropbox HTTP API docs (`list_folder`, `list_folder/continue`, `longpoll`, cursor `reset`, `WriteMode.update`): https://www.dropbox.com/developers/documentation/http/documentation. The page did not render during research, so these API details are from prior knowledge, not re-read.
- rclone bisync (snapshot-diff sync with no change log; a useful contrast showing what you lose without one, e.g. renames become delete + create): https://rclone.org/bisync/
- Hughes, Pierce, Arts, Norell, "Mysteries of Dropbox: Property-Based Testing of a Distributed Synchronization Service" (formal testing of sync behaviour; good for thinking about invariants): https://www.cis.upenn.edu/~bcpierce/papers/mysteriesofdropbox.pdf
