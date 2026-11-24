# Spike 04: Chunking

- **Issue:** #4
- **Status:** Decided. See [ADR-0009](../adr/0009-chunking-and-chunk-ids.md) (keyed FastCDC, HMAC-SHA256 IDs) and [ADR-0010](../adr/0010-chunk-storage-and-manifests.md) (one padded object per chunk, location index, encrypted manifests; packs deferred). Section 8 recommendations adopted. The IACR 2025/532 paper, restic 0.18.0 changelog and S3 request prices were re-checked on 2026-11-20.
- **Question:** How are files split into chunks, how big are the chunks, how are chunks named and stored, and how does a file revision point to its chunks? All of this has to work under end-to-end encryption ([ADR-0007](../adr/0007-end-to-end-encryption.md)).

---

## Constraints already decided

- **Goal 6:** a small change to a large file uploads only the changed part.
- **ADR-0007:**
  - chunk IDs are a **keyed** hash, so the server can't confirm a known file
  - chunks are encrypted **after** chunking
  - each stored chunk has a **versioned header**
  - no server-side worker reads content
- **Single user:** dedup within one key is all the dedup there is.
- **Storage:** S3 on AWS, an S3-compatible store locally (spike 10). Language: Go.

---

## 1. Fixed-size vs content-defined chunking

### The boundary-shift problem

**Fixed-size chunking** cuts a file every N bytes. Take 4 MB chunks:

```
original:   [ A: 0–4MB ][ B: 4–8MB ][ C: 8–12MB ]
insert 10 bytes at the start:
new:        [ A': 0–4MB ][ B': 4–8MB ][ C': 8–12MB ][ D: 10 bytes ]
```

Every boundary shifts by 10 bytes, so **every chunk's content changes**, and the whole file is re-uploaded. Edits that only overwrite bytes in place (databases, VM images) are fine. Inserts and deletes, which are what text editors, many document formats and archives produce, are not.

**Content-defined chunking (CDC)** puts the boundaries where the *content* matches a pattern. A rolling hash runs over a small sliding window (e.g. 48–64 bytes), and wherever the hash's low bits are all zero, that's a cut point. After an insert, only the chunk containing the insert changes. The next cut point is found at the same content as before, and everything after it lines up again.

```
original:   [ A ][ B ][ C ]       cuts are at content "landmarks"
insert 10 bytes inside A:
new:        [ A' ][ B ][ C ]      only A changes; B and C are identical → not re-uploaded
```

### CDC algorithms

| Algorithm | Idea | Used by |
|---|---|---|
| **Rabin fingerprint** | Polynomial rolling hash. Mathematically clean, slower. | **restic**, via a per-repository random polynomial (verified: restic's `config` stores a `chunker_polynomial`, and the `restic/chunker` library is Rabin-based) |
| **Buzhash** | Rolling hash via table lookups and rotation. | **borg**, with a key-derived seed *(unverified this session)*. Also casync *(unverified)*. |
| **Gear / FastCDC** | Very cheap rolling hash (one shift, one add, one table lookup per byte). FastCDC adds **normalized chunking**: a stricter cut condition below the target size and a looser one above it, so sizes cluster tightly around the average. | Many newer tools, e.g. Plakar's `go-cdc-chunkers` |

FastCDC is generally reported to be several times faster than Rabin with similar dedup, and its tighter size spread means fewer tiny or huge chunks. *(Speed ratio from prior knowledge; benchmark before relying on it.)*

### What others do

| System | Chunking | Source |
|---|---|---|
| **Dropbox** | Fixed **4 MB** blocks. Its public content hash is defined over 4,194,304-byte blocks with SHA-256 per block. | Verified (content-hash spec). That the storage backend uses the same 4 MB blocks is widely reported but *unverified here*. |
| **Syncthing** | **Fixed** block size per file, a power of two from 128 KiB to 16 MiB: "the smallest block size that results in fewer than 2000 blocks". 128 KiB for files up to 250 MiB, 256 KiB up to 500 MiB, and so on. | Verified (BEP spec) |
| **restic** | CDC (Rabin), chunks roughly 512 KiB–8 MiB, average ~1 MiB, **packed** into larger pack files | Rabin and packs verified; size parameters from prior knowledge |
| **borg** | CDC (Buzhash), keyed | Unverified |
| **Tarsnap** | CDC; its author co-wrote the 2025 chunking-attack paper (section 4) | Paper authorship verified |

**Takeaway:** sync products (Dropbox, Syncthing) have used fixed blocks. Backup tools (restic, borg, Tarsnap) use CDC because they dedup across versions and files. CloudFS has goal 6 and keeps version history, which is the backup-tool profile.

---

## 2. Chunk size

### Tradeoffs

| Smaller chunks (e.g. 256 KiB) | Larger chunks (e.g. 4 MiB) |
|---|---|
| Finer dedup: an edit re-uploads less | Coarser: an edit re-uploads up to one big chunk |
| More objects, requests, DB rows and per-request latency | Fewer objects, faster bulk transfer |
| Per-chunk crypto overhead matters more | Overhead negligible |

CDC takes three parameters: **min** (no cut before this), **average** (target), **max** (force a cut). A common shape is min = avg/4 and max = avg×4 (e.g. 256 KiB / 1 MiB / 4 MiB).

**Small files:** a file smaller than the minimum is one chunk. Most real folders are dominated by *counts* of small files and *bytes* of a few large ones, so small files drive object count.

**Per-chunk overhead under E2E:** the versioned header (~8–40 bytes) plus the XChaCha20-Poly1305 nonce (24 bytes) and tag (16 bytes) is under 100 bytes. Negligible for 1 MiB chunks; it only matters for tiny files.

### Cost estimate: 50 GB, 100,000 files

Assumption (illustrative): 90,000 small files totalling 10 GB (~110 KB average) and 10,000 larger files totalling 40 GB.

S3 Standard request pricing, checked against the AWS pricing page's worked examples: **PUT $0.005 per 1,000; GET $0.0004 per 1,000.**

| Layout | Objects | Initial upload (PUTs) | Full download to a new device (GETs) |
|---|---|---|---|
| One object per chunk, avg 1 MiB | ~90k small + ~41k large ≈ **131k** | ~$0.66 | ~$0.05 |
| One object per chunk, avg 4 MiB | ~90k + ~10k ≈ **100k** | ~$0.50 | ~$0.04 |
| Packed into 16 MiB pack objects | ≈ **3.2k** | ~$0.02 | ~$0.001 (+ range GETs) |

**Request cost isn't a deciding factor at this scale:** it's cents to a dollar, one-off. Storage itself (~$0.023/GB-month for S3 Standard, so ~$1.15/month for 50 GB; *price from prior knowledge*) costs more than all the requests. Also note that **object count is driven by small files, not chunk size.** Going from 1 MiB to 4 MiB average barely changes it.

The real reasons to care about packing are **leakage** (section 4), **listing and GC work**, and **per-request latency** for many tiny files, not the bill.

Postgres rows: ~130k chunk rows plus references is trivial.

### One object per chunk vs packs

| | One object per chunk | Pack objects (restic-style) |
|---|---|---|
| Upload | One presigned PUT per new chunk; simple | Client assembles packs and uploads one presigned PUT per pack |
| Download | GET by chunk ID | Range GET (offset, length) inside a pack |
| Dedup | Chunk ID = object key, so the existence check is a DB lookup | Same check, plus an index `chunk_id → pack, offset, length` |
| GC | Delete unreferenced objects | **Repack**: rewrite packs that hold both live and dead chunks. Real complexity. |
| Leakage | Server sees each chunk's exact size | Chunk sizes hidden inside opaque packs |
| Object count | ~files + large-file chunks | Very low |

**A "pack-ready" middle path:** from day one, store a *location* per chunk (`object_key, offset, length`) in the chunk index instead of assuming `object_key = chunk_id`. In v1, each chunk is its own object at offset 0. Moving to packs later then needs no change to manifests or the protocol, only how new chunks get written plus a repack job.

**Tiny-file option:** files under a few KiB could be stored *inline* in the encrypted manifest (section 6), with no S3 object. That cuts object count sharply for folders full of tiny files. Optional.

---

## 3. Chunk IDs and hashes

### The keyed chunk ID

Research 03 established: `chunk_id = keyed_hash(chunk_id_key, plaintext_chunk)`, with the key derived from the master key via HKDF.
- Identical plaintext gives an identical ID, so the user's own dedup works across files, devices and versions.
- Without the key, the server can't compute IDs for a known file, so it can't confirm the file is stored.
- 256-bit output; accidental collisions aren't a practical concern.

**Two candidates:**

| | HMAC-SHA256 | Keyed BLAKE3 |
|---|---|---|
| Go | Standard library (`crypto/hmac` + `crypto/sha256`); no dependency | Third-party: `lukechampine.com/blake3` (MIT, actively maintained, last push 2026-11) or `github.com/zeebo/blake3` (active, last push 2026-10; license shows as unrecognised on GitHub, check before use) |
| Speed | Hardware SHA-256 instructions on modern x86 and ARM (including Apple Silicon) make it fast | Usually faster, especially with SIMD on x86; varies by platform and library *(benchmark)* |
| Keying | HMAC construction | Native keyed mode |

Either is sound. HMAC-SHA256 avoids a dependency, and hashing is unlikely to be the bottleneck next to encryption and network. The **ID scheme field** on each chunk record (research 03) means this can change later without ambiguity.

### The client baseline's "content hash"

Design §9 keeps a per-file content hash in each device's baseline. Rather than hashing the file a second time, define the **file content hash as a keyed hash over the ordered list of its chunk IDs** (a tiny Merkle-style root). The chunk IDs are already computed during chunking, so it's free, and it changes exactly when content changes. To decide *whether* to re-chunk, the client checks size + mtime first (spike 06).

### Can the server verify uploads?

- It **can't** verify that a chunk matches its keyed plaintext ID. It has neither the key nor the plaintext.
- The **client** catches corruption or tampering on download: the AEAD tag fails to verify, and after decryption the recomputed keyed ID must match.
- The **server can check transport integrity of the ciphertext:** the client sends the ciphertext's SHA-256 when requesting the presigned URL, and the API signs `x-amz-checksum-sha256` into the URL. S3 then rejects a body that doesn't match.
  - Reports agree the header **must be included in the signature**, otherwise S3 rejects it as an unsigned header.
  - The value must be Base64 of the raw digest, not hex.
  - Exact Go SDK usage is unverified; it belongs in spike 07.
- Binding the chunk ID into the AEAD's associated data stops someone with bucket access from swapping one valid chunk for another under a different ID.

---

## 4. Leakage from chunk sizes under E2E

This is the most important finding of this spike.

- **The paper:** Alexeev, Percival and Zhang, ["Chunking Attacks on File Backup Services using Content-Defined Chunking"](https://eprint.iacr.org/2025/532) (2025).
  - Attackers who can **observe chunk sizes for a file they know** can **extract the secret chunking parameters**, even when those parameters are per-user secrets. Once the parameters are known, they can test whether other known files are stored.
  - The attacks are protocol-specific, but the authors say the ideas generalize to many CDC schemes.
- **restic's response (0.18.0, 2025-03-27, verified in its changelog):**
  - restic keeps a secret Rabin polynomial, but notes that an attacker who sees chunk sizes for a known file "can derive the secret polynomial".
  - A practical attack was already hard, because restic merges chunks into **opaque pack files**.
  - restic added a mitigation that **randomizes how chunks are assembled into pack files**, so attackers can't learn chunk sizes.

**What this means for CloudFS:**
- **One object per chunk shows the server every chunk's exact (ciphertext) size**, and if manifests are plaintext, which chunks belong to which file in order. That's exactly what the attack needs.
- **A keyed chunker** (secret polynomial, or for FastCDC a gear table derived from a secret key) is necessary but **not sufficient**. The paper's point is that keyed parameters can be extracted.
- **Mitigations:**
  1. **Packs** with randomized chunk placement (restic's approach). Strongest; costs GC complexity.
  2. **Padding** chunk ciphertexts to coarse size buckets. Hides exact sizes at some storage cost. A scheme like Padmé (from the PURBs paper; *from prior knowledge*) keeps overhead to roughly ≤12%. Naive "round up to 64 KiB" would waste ~30% on small files. Whether padding alone defeats parameter extraction is **not established**; it reduces the signal.
  3. **Encrypt manifests** (section 6), so the server doesn't see which chunks make up a file or in what order.
- **Threat realism:** the attacker is someone with access to the server or bucket who has a specific file and wants to confirm you store it. For a personal tool that's a narrow threat, but it's exactly the class ADR-0007 chose to defend against.

---

## 5. Go libraries for CDC

Checked on GitHub, 2026-11-20:

| Library | Algorithm | License | Status |
|---|---|---|---|
| `github.com/restic/chunker` | Rabin, with a configurable polynomial (keyable) | BSD-2-Clause | Maintained (pushed 2026-06), production use in restic |
| `github.com/PlakarKorp/go-cdc-chunkers` | FastCDC, UltraCDC, JC behind one interface, "each with a spec-faithful versioned variant" | ISC | Active (pushed 2026-11) |
| `github.com/tigerwill90/fastcdc` | FastCDC | MIT | Active (pushed 2026-09), small |
| `github.com/jotfs/fastcdc-go` | FastCDC | Apache-2.0 | Stale (last push 2023-08) |

Whether each FastCDC library lets you supply a **keyed gear table** isn't verified. If none does, implementing FastCDC is ~100 lines and a reasonable Go learning exercise; restic/chunker supports a secret polynomial directly.

Versioned chunker variants matter: if a library changes its cut-point logic, dedup against old chunks breaks. Pin the algorithm and parameters in the chunk ID scheme or format version.

---

## 6. File representation: manifests

A file's **content revision** points to an ordered list of chunks, its **manifest**: `[(chunk_id, plaintext_length), …]`.

| Option | What the server sees | GC and refcounting |
|---|---|---|
| **Plaintext manifest rows in Postgres** (`revision_chunks(revision, position, chunk_id)`) | Exact ordered chunk list per file | Simple SQL refcount |
| **Encrypted manifest blob** (in Postgres `bytea` or as an S3 object) **plus an unordered reference set** (`revision_refs(revision, chunk_id)`) | Which chunks a revision uses, but not their order or plaintext lengths | Refcount from the unordered set |
| **Fully encrypted manifest, no server-visible refs** | Nothing | GC must be done by a client. Breaks the "server-side GC worker" plan. |

**Version history (goal 5):** each retained content revision keeps its manifest. A chunk is live while any retained revision references it. Pruning old revisions drops references, and GC removes zero-reference chunks.

**GC race to design around (spike 05/07):** a client asks "which of these chunks do you already have?", is told chunk X exists, and skips uploading it. Meanwhile GC deletes X because nothing referenced it yet. Standard fixes: a **grace period** (only delete chunks unreferenced for, say, 24 hours), or a "pending" mark that a commit must claim. Also orphan chunks from rejected commits (ADR-0005) are cleaned up by the same mechanism.

**Small-file inlining** (section 2) fits here: the encrypted manifest can carry tiny file contents directly.

---

## 7. Interactions with other spikes

- **Spike 05 (deletes):** GC grace period and orphan cleanup; tombstoned nodes keep revisions (and so chunks) alive until their retention ends.
- **Spike 06 (file watching):** a file can change while it's being chunked. Record size + mtime before reading and re-check after; if they differ, discard the result and retry later. Use size + mtime to avoid re-chunking unchanged files.
- **Spike 07 (uploads):**
  - With max chunk ≤ 4 MiB, or packs of ~16 MiB, every upload fits a **single presigned PUT** (S3's single-PUT limit is 5 GB), so no multipart is needed.
  - The presigned URL should sign `x-amz-checksum-sha256` of the ciphertext.
  - A batch "which chunks are missing?" endpoint keeps round trips low.
- **Spike 09 (costs):** requests are cents to dollars (section 2); storage dominates.
- **Spike 11 (name encryption):** same key hierarchy. HKDF derives separate sub-keys for content, names, chunk IDs, and the chunker seed/gear table. Never reuse one key for two purposes.
- **Spike 10 (local storage):** whichever store is picked needs single PUT with checksum headers and range GET (if packs). Not evaluated here.

---

## 8. Recommendation (for discussion, not a decision)

- **Chunking:** **FastCDC** with a **keyed gear table** (derived via HKDF from the master key). **Parameters:** min 256 KiB, average 1 MiB, max 4 MiB. Files below the minimum are one chunk.
- **Chunk ID:** **HMAC-SHA256** with a dedicated chunk-ID key. Standard library, fast enough, and switchable later via the ID scheme field.
- **File content hash** for the baseline: a keyed hash over the ordered chunk-ID list.
- **Storage layout:** **one object per chunk in v1, padded to size buckets (Padmé-style)**, with a chunk **location index** (`object_key, offset, length`) from day one, so **packs** can be introduced later without protocol or manifest changes.
- **Manifests:** **encrypted manifest blob plus an unordered server-visible reference set** for GC.
- **Integrity:** the ciphertext SHA-256 is signed into presigned PUTs, and the chunk ID is bound into the AEAD associated data.
- **Defer:** packs (unless you want maximum leakage resistance from v1), tiny-file inlining.

**Reasoning:** this meets goal 6 and ADR-0007 and keeps v1's upload and GC paths simple. The leak the 2025 paper exploits is narrowed by keyed chunking, padding and encrypted manifests, and fully closed later by packs, with the location index making that a non-breaking change.

**The case for packs in v1 instead:** they're the only mitigation proven in practice (restic), they cut object count ~40×, and doing them first avoids a later repack migration. The cost is repacking GC, which is the hardest piece of code in this design.

---

## 9. Decisions for the user

1. **Chunking:** content-defined (FastCDC recommended, or restic's Rabin chunker) or fixed-size?
2. **Chunk size:** 256 KiB / 1 MiB / 4 MiB (min/avg/max), or larger?
3. **Chunk ID hash:** HMAC-SHA256 (standard library) or keyed BLAKE3 (faster, third-party)?
4. **Storage layout:** one object per chunk with padding and a location index (packs later), or packs from v1?
5. **Manifests:** encrypted blob + unordered reference set (recommended), or plaintext ordered rows?
6. **Tiny-file inlining:** now or later?

---

## References

- Alexeev, Percival, Zhang, "Chunking Attacks on File Backup Services using Content-Defined Chunking" (IACR ePrint 2025/532), abstract read: https://eprint.iacr.org/2025/532
- restic changelog, 0.18.0 (2025-03-27), "Security #5291: Mitigate attack on content-defined chunking algorithm": https://github.com/restic/restic/blob/master/CHANGELOG.md
- restic design reference (`chunker_polynomial` in repository config, pack format, IV‖ciphertext‖MAC): https://restic.readthedocs.io/en/latest/100_references.html
- restic/chunker (Rabin CDC, BSD-2-Clause): https://github.com/restic/chunker
- restic blog, "Introducing Content Defined Chunking" (linked from the chunker README; not re-read): https://restic.net/blog/2015-09-12/restic-foundation1-cdc/
- Dropbox content hash (4 MB blocks, SHA-256 per block): https://www.dropbox.com/developers/reference/content-hash
- Syncthing BEP v1, block size selection (128 KiB–16 MiB, fewer than 2000 blocks): https://docs.syncthing.net/specs/bep-v1.html
- PlakarKorp/go-cdc-chunkers (FastCDC/UltraCDC/JC, ISC): https://github.com/PlakarKorp/go-cdc-chunkers
- tigerwill90/fastcdc (MIT): https://github.com/tigerwill90/fastcdc
- jotfs/fastcdc-go (Apache-2.0, stale since 2023): https://github.com/jotfs/fastcdc-go
- lukechampine/blake3 (MIT): https://github.com/lukechampine/blake3
- zeebo/blake3: https://github.com/zeebo/blake3
- AWS S3 pricing (PUT $0.005/1k, GET $0.0004/1k, from worked examples on the page): https://aws.amazon.com/s3/pricing/
- Presigned PUT with `x-amz-checksum-sha256` (must be signed; Base64 digest), from community and SDK sources, not AWS reference docs: https://repost.aws/questions/QUgivVIUn6QrGVpETR1wQ4KQ/s3-sha256-checksum-for-presigned-url-in-file-upload, https://cdn.jsdelivr.net/npm/@aws-sdk/s3-request-presigner@3.1075.0/README.md, https://builder.aws.com/content/3K8m18ashGyrI7CH5eozaHw2S26/presigned-s-uploads-integrity-is-not-authorization

**From prior knowledge, not verified this session:**
- borg's and casync's use of Buzhash, and borg's keyed seed
- FastCDC's speed relative to Rabin
- restic's exact min/avg/max chunk sizes
- Dropbox's backend block size
- S3 Standard storage price ($0.023/GB-month)
- the Padmé padding scheme's overhead bound
- the 5 GB single-PUT limit
- whether the FastCDC libraries accept a custom gear table
- BLAKE3 vs SHA-256 speed in Go on Apple Silicon
