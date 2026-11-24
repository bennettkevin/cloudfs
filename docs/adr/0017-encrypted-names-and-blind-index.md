# ADR-0017: Encrypted file names with a per-folder blind index

- **Status:** Accepted
- **Date:** 2026-11-24
- **Research:** [Spike 11](../research/11-name-encryption.md) (#14)

## Context

File content is end-to-end encrypted ([ADR-0007](0007-end-to-end-encryption.md)), but names often say what a file contains. The server must still enforce case-insensitive unique live names per folder ([ADR-0005](0005-conflict-resolution.md)). It needs nothing else from names: cycle and parent checks use IDs, and conflicted-copy names, trash listings and sorting all happen on the client.

Spike 11 measured the cost: about 1.3 µs per name to encrypt and index, so 0.13–0.15 s for 100,000 names (Apple M5), which is about the CPU of encrypting 160 MB of content. It also found that this Mac's APFS treats names as equal under Unicode canonical caseless matching, and that Go's `x/text` case folding is wrong for the 86 Cherokee capital letters (golang/go#46101).

## Decision

**Names are encrypted on the client** with randomized XChaCha20-Poly1305 under a `name_key` (HKDF sub-key of the master key):
- `name_ct = nonce ‖ AEAD(pad32(len ‖ NFC(name)), AD = node_id ‖ parent_id)`
- Padding to 32-byte blocks hides exact name length.
- The AD binds the name to its node and folder: the server can't swap names or silently move a node, because the client's decryption would fail. Every move sends a new `name_ct`.

**A blind index enforces uniqueness:** `name_index = HMAC-SHA256(index_key, parent_id ‖ IndexForm(name))`, with a separate `index_key` sub-key. The server enforces a unique `(parent_id, name_index)` for live nodes. Including `parent_id` means equal names in different folders give unrelated indexes.

**"Same name" = Unicode canonical caseless matching**, which matches APFS (measured):
- `IndexForm(name) = NFC(fold(NFD(name)))`, where `fold` is full default case folding (`CaseFolding.txt` statuses C and F).
- Not NFKC: `①.txt` and `1.txt` stay different names, as they are on APFS.
- Not plain lowercasing, which misses `ß`/`SS`, NFD forms, ligatures and Greek sigma.

**CloudFS owns its fold function:** `x/text`'s `cases.Fold` plus a fix that maps Cherokee small letters to capitals. A test checks every code point against a checked-in `CaseFolding.txt` matching the pinned `x/text` Unicode version. Re-check golang/go#46101 on every `x/text` upgrade.

**Every name carries a `name_scheme` number**, so a future change to the format or equality rule can be migrated by clients re-encrypting and re-indexing.

**No AES-SIV.** Deterministic encryption is needed only when the ciphertext is looked up by name (Syncthing, Cryptomator). CloudFS addresses nodes by ID.

## Alternatives considered

- **Plaintext names** — the most revealing metadata stays readable by the server and cloud provider, for a cost of microseconds per name.
- **Deterministic AES-SIV** — leaks "renamed back to the same name", still needs a separate index for case-insensitive uniqueness, and Go has no maintained library (`miscreant.go` archived).
- **NFKC + case folding** — formally covered by Unicode's stability policy, but broader than APFS: names that every target filesystem keeps apart would become conflicted copies.
- **Global blind index (no `parent_id`)** — reveals the same name across folders, for no benefit, since uniqueness is per folder.
- **No padding** — reveals each name's exact length.

## Consequences

- `nodes.name` is replaced by `name_ct`, `name_index` and `name_scheme`. Tombstones keep them through the trash period; purge clears them ([ADR-0012](0012-deletes-tombstones-and-trash.md)).
- Create, rename, move and restore carry `name_ct` and `name_index`. Restore re-checks the index against live siblings; a collision follows ADR-0005.
- The server can't sort, search or display names. Listings are sorted by the client; debugging needs client-side commands that decrypt; server logs carry node IDs only.
- Storage grows from about 22 to about 110 bytes per node (measured average), about 11 MB per 100,000 nodes.
- The key hierarchy (phase 1) must derive separate `name_key` and `index_key`. Rotating either means re-committing every node's name.
- Linux filesystems are usually case-sensitive, so two local names can map to one server name; the second becomes a conflicted copy (ADR-0005). Windows (NTFS) uses its own uppercase table and needs checking when Windows support starts.
- The server stores less readable user data than before: names become ciphertext plus a keyed hash.
