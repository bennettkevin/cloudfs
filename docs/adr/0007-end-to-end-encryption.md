# ADR-0007: End-to-end encryption of file content in v1

- **Status:** Accepted
- **Date:** 2026-11-19
- **Research:** [Spike 03](../research/03-encryption.md) (#3)

## Context

Server-side encryption (S3 SSE, RDS encryption) protects against stolen disks and, with KMS, leaked storage credentials. It does not protect against a compromised server, the cloud provider, or a leaked bucket or database dump being read by whoever has the server's access. CloudFS is meant to be self-hosted by people who may not fully trust their host, and file contents can be sensitive.

## Decision

File content is encrypted on the client before upload, in v1. The server, the object store and the database never hold plaintext file content or the keys to decrypt it.

What follows from this decision:
- **No server-side workers read file content** (no thumbnails, previews, indexing). Versioning, pruning and chunk garbage collection still work, because they operate on metadata and chunk IDs only.
- **Chunk IDs must not reveal content.** A plain hash of the plaintext would let anyone with the bucket confirm whether a known file is stored. Chunk IDs are a keyed hash (exact scheme decided in spike 04).
- **Chunks are encrypted after chunking**, so content-defined chunk boundaries (spike 04) still work.
- **Stored chunks carry a versioned header** (format version, encryption scheme, key ID), so the format can evolve without ambiguity.
- **Losing the key means losing the data.** A recovery path (e.g. a printed recovery key) is mandatory in the onboarding design.
- **Authentication and encryption are separate.** Revoking a device's login (spike 08) stops it syncing but doesn't take away keys it already has.

Accepted cost: the full sync engine takes longer to deliver (roughly 1–2 extra sprints, estimated).

**Not decided here** (tracked separately):
- File and folder name encryption and how the server keeps case-insensitive uniqueness: spike 11 (#14). Current leaning: encrypt names, and the client sends a keyed hash of the normalized name (blind index).
- Key hierarchy, passphrase KDF, recovery key, and new-device onboarding: designed as part of the encryption work in phase 1. The research file's section 3 is the starting point (random master key wrapped by a passphrase-derived key and a recovery key; HKDF sub-keys; XChaCha20-Poly1305).

## Alternatives considered

- **Server-side encryption only (A)** — covers the most likely incidents (credential or configuration mistakes) but not a compromised server or provider.
- **Server-side now with an E2E-ready format, E2E later (C)** — cheaper up front. Rejected because the protection is wanted from the start, and retrofitting later still means migrating chunk IDs and names.
- **E2E as a per-deployment option (D)** — two code paths to build and test. May be revisited if a deployment ever needs server-side content processing.

## Consequences

- Strongest protection for file content against anything on the server side.
- Debugging needs client-side tooling (e.g. a `cloudfs debug decrypt` command); you can't inspect chunks in the bucket.
- Server-side encryption at rest is still used underneath as defence in depth ([ADR-0008](0008-at-rest-encryption-aws.md)).
- Single-user dedup is preserved with keyed chunk IDs; cross-user dedup is not possible (no second user under [ADR-0002](0002-single-user-first.md)).
