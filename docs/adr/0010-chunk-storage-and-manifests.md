# ADR-0010: One padded object per chunk with a location index; encrypted manifests

- **Status:** Accepted
- **Date:** 2026-11-20
- **Research:** [Spike 04](../research/04-chunking.md) (#4)

## Context

Encrypted chunks need to be stored in S3 (or a compatible store), and each file revision needs to record its chunks. Two pressures pull against each other:
- **Leakage.** Alexeev, Percival and Zhang ("Chunking Attacks on File Backup Services using Content-Defined Chunking", IACR ePrint 2025/532) show that an attacker who sees exact chunk sizes for a file they know can recover keyed chunking parameters, then test whether other known files are stored. restic 0.18.0 responded by randomising how chunks are placed into opaque pack files.
- **Complexity.** Packs hide chunk sizes, but deleting data means rewriting packs that mix live and dead chunks (repacking), the hardest code in the design. The developer is learning Go.

Request cost isn't a deciding factor: for 50 GB / 100k files, any layout costs cents to about a dollar in requests.

## Decision

**Storage layout (v1): one object per chunk, padded.**
- Each encrypted chunk is its own object: versioned header, ciphertext, AEAD tag.
- The ciphertext is **padded to coarse size buckets** (Padmé-style), so the server doesn't see exact chunk sizes. Padding overhead target ≤ ~12%, to be confirmed at implementation. Padding narrows the leak but is not proven to defeat the paper's attack; packs are the full fix.
- The chunk ID is bound into the AEAD associated data, so a chunk can't be swapped for another under a different ID.

**Location index from day one.** The chunk record stores `object_key, offset, length` instead of assuming `object_key = chunk_id`. In v1 every chunk is its own object at offset 0. Moving to packs later changes only how new chunks are written, plus a repack job, with no protocol or manifest change.

**Manifests.** A file's content revision has:
- an **encrypted manifest**: the ordered list of `(chunk_id, plaintext_length)`, readable only by clients; and
- an **unordered reference set** visible to the server, used for garbage collection and refcounting.

The server learns which chunks a revision uses, not their order or plaintext sizes.

**Garbage collection.** A chunk is live while any retained revision references it. Unreferenced chunks are deleted only after a **grace period** (initially 24 hours, configurable), so a chunk the server just told a client "already exists" isn't deleted before the client's commit references it. Orphans from rejected commits ([ADR-0005](0005-conflict-resolution.md)) are cleaned up the same way.

**Integrity in transit.** The client sends the ciphertext's SHA-256 when requesting a presigned PUT; the API signs `x-amz-checksum-sha256` into the URL, so the store rejects a corrupted body. Exact SDK behaviour is verified in spike 07.

**Deferred:**
- **Pack objects:** planned for later, tracked as a backlog issue.
- **Inlining tiny files** into the encrypted manifest.

## Alternatives considered

- **Packs from v1** — the only mitigation proven in practice, ~40× fewer objects, and avoids a later migration. Deferred because repacking GC is too much to take on while learning Go; the location index keeps the door open.
- **Unpadded one-object-per-chunk** — simplest, but hands the attack exact sizes.
- **Plaintext ordered manifest rows in Postgres** — simplest refcounting, but exposes each file's exact chunk sequence.
- **Fully encrypted manifests with no server-visible references** — leaks least, but garbage collection would have to run on a client, breaking the server-side worker plan.

## Consequences

- v1 upload, download and GC paths stay simple: one presigned PUT/GET per chunk, delete-unreferenced-after-grace for GC.
- Object count is roughly one per small file plus one per large-file chunk (~131k objects for 50 GB / 100k files).
- Residual leakage, approximate padded chunk sizes and which chunks a revision uses, is documented and closed later by packs.
- Version history (goal 5) is retained manifests; pruning a revision drops its references and lets GC reclaim chunks.
