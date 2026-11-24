# ADR-0009: Keyed content-defined chunking (FastCDC) and HMAC-SHA256 chunk IDs

- **Status:** Accepted
- **Date:** 2026-11-20
- **Research:** [Spike 04](../research/04-chunking.md) (#4)

## Context

Goal 6 requires that a small change to a large file uploads only the changed part. With fixed-size chunks, an insertion shifts every later boundary and the rest of the file looks new. End-to-end encryption ([ADR-0007](0007-end-to-end-encryption.md)) requires chunk IDs that don't let the server confirm a known file, and requires chunking before encryption.

## Decision

- **Algorithm:** FastCDC content-defined chunking with a **keyed gear table** derived from the master key (HKDF, dedicated sub-key). Without the key, cut points can't be predicted.
- **Sizes:** minimum 256 KiB, average 1 MiB, maximum 4 MiB. Files smaller than the minimum are a single chunk. Every chunk (with header and padding) fits in a single presigned PUT, so no multipart upload is needed.
- **Chunk ID:** `HMAC-SHA256(chunk_id_key, plaintext_chunk)` with its own HKDF sub-key. Identical plaintext gives an identical ID, so dedup works across files, devices and versions for the one user.
- **ID scheme field:** every chunk record stores which scheme produced its ID, so the hash (e.g. keyed BLAKE3) or chunker parameters can change later without ambiguity. The chunker algorithm and parameters are pinned per scheme version, since a change in cut-point logic breaks dedup against old chunks.
- **File content hash** (the client baseline, design §9): a keyed hash over the file's ordered list of chunk IDs. No second pass over the file.
- **Key separation:** content encryption, names, chunk IDs and the chunker gear table each use a separate HKDF sub-key. No key is used for two purposes.

## Alternatives considered

- **Fixed-size chunks (Dropbox 4 MB blocks, Syncthing per-file block sizes)** — simpler, but insertions re-upload the rest of the file, which fails goal 6 for many edit patterns.
- **Rabin chunking (`restic/chunker`, secret polynomial)** — proven and keyable; FastCDC is faster and the keyed gear table gives the same property. Remains a fallback if no FastCDC library fits and a hand-written implementation proves troublesome.
- **Keyed BLAKE3 for chunk IDs** — faster, but a third-party dependency; hashing isn't expected to be the bottleneck next to encryption and network. Switchable later through the ID scheme field.
- **Larger chunks (e.g. 4 MiB average)** — fewer objects for large files, but coarser dedup. Object count is driven by small files anyway.

## Consequences

- Library choice is open: `PlakarKorp/go-cdc-chunkers` (ISC) or `tigerwill90/fastcdc` (MIT) if they accept a custom gear table, otherwise a small in-house FastCDC (~100 lines). Decided at implementation.
- A keyed chunker alone does not stop chunk-size leakage: the 2025 paper by Alexeev, Percival and Zhang shows the keyed parameters can be recovered from observed chunk sizes. The storage layout mitigates this ([ADR-0010](0010-chunk-storage-and-manifests.md)).
- Files changing while being chunked are handled by the file-watching design (spike 06): check size + mtime before and after reading, and retry on mismatch.
