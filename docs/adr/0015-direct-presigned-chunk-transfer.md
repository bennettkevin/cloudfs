# ADR-0015: Direct presigned chunk transfer with signed checksum and length

- **Status:** Accepted
- **Date:** 2026-11-24
- **Research:** [Spike 07](../research/07-uploads.md) (#7)

## Context

Clients upload and download encrypted chunks ([ADR-0009](0009-chunking-and-chunk-ids.md), [ADR-0010](0010-chunk-storage-and-manifests.md)). The bytes can go through the CloudFS API, or straight to the object store using presigned URLs: short-lived URLs the server signs with its own store credentials so a client can do one specific request without holding any credentials.

The server can't check chunk content (no key, no plaintext; [ADR-0007](0007-end-to-end-encryption.md)), so the store has to reject bad bytes at upload time, and the server needs a cheap way to confirm an upload landed before a commit refers to it. The store must be Garage locally and AWS S3 in phase 2, with the same code ([ADR-0011](0011-local-object-storage.md)).

Spike 07 tested 14 cases against Garage v2.5.0 and gofakes3 v1.3.0 (research section 11). Garage rejected every tampered, expired, re-hosted, wrong-length or wrong-range request; gofakes3 accepted all of them.

## Decision

**Direct transfer.** Clients PUT and GET chunk objects directly on the store with presigned URLs. The API is a control plane only (which chunks are missing, URLs, commits). No proxy mode in v1.

**Upload URLs bind the exact bytes.**
- The server derives the object key from the chunk ID (`chunks/<hex id>`). It never accepts a key from the client.
- Each presigned PUT signs `x-amz-checksum-sha256` (Base64 SHA-256 of the ciphertext) and `Content-Length`. The server rejects a declared length above the maximum padded chunk size.
- `ChecksumAlgorithm`, `Content-MD5` and `If-None-Match` are not set. Garage ignores `If-None-Match`, and CloudFS doesn't need it: two devices uploading the same chunk write different ciphertexts of the same length that decrypt to the same content, so whichever write lands last is correct.
- Clients send exactly the headers the server returns with each URL.
- One PUT per chunk; no multipart (chunks are ≤ ~4.6 MiB, single PUT limit is 5 GB).

**Download URLs** are presigned GETs with the same expiry. In v1 each chunk is a whole object. When packs arrive, the server signs a `Range` header from the location index.

**Expiry: 15 minutes**, returned to the client as `expires_at`. The server checks at startup that URL expiry is far shorter than the GC grace period (24 h). In phase 2 it is also capped by the signer's temporary credential lifetime.

**Presign against the public endpoint.** The host is part of the signature, so the server keeps two S3 client configs: an internal one for HEAD, DELETE and LIST, and one with the public endpoint used only for presigning. Self-hosters expose Garage's S3 API on its own HTTPS subdomain (e.g. `s3.cloudfs.example`); a stripped path prefix on the API host would break signatures.

**Verify on commit with HEAD.** Before opening the commit transaction, the server HEADs each referenced chunk not yet confirmed (`uploaded_at IS NULL`), checks it exists with the expected length, and sets `uploaded_at`. Each chunk is HEADed once in its lifetime. Refs that aren't confirmed come back as `missing: [ids]`. No bucket notifications.

**Client error handling.** Error codes differ by store (Garage `InvalidDigest` vs AWS `BadDigest`; 400 vs 403 for some cases), so the client doesn't branch on exact codes:
- network error, 5xx, `SlowDown`: retry the same URL with exponential backoff and full jitter until it expires
- any 4xx: request a new URL and retry once (re-encrypting and re-hashing if the checksum failed), then log and stop

**Security.**
- Never log full presigned URLs (the signature is in the query string); log the key and expiry.
- Clients never hold store credentials. Signer credentials are scoped to the one bucket: Garage key with read/write on that bucket only; in phase 2 the ECS task role limited to the bucket ARN and the `chunks/*` prefix.

**Garage pin: v2.5.0.** It passed every case. Move to v2.6.0 (tagged 2026-11-24, image not yet published) only after it passes the spike 07 program.

## Alternatives considered

- **Proxy all chunk bytes through the API** — one public endpoint for self-hosters, but more code (streaming, timeouts, back-pressure), and on AWS extra ALB/Fargate cost for every byte. Could be added later as an option if self-hosters ask.
- **Hybrid (proxy locally, direct on AWS)** — two data paths to build and test.
- **No signed checksum** — a leaked PUT URL could overwrite a live chunk with garbage. The AEAD would detect it on download, but the chunk would be lost if no device still had it.
- **`Content-MD5`** — weaker hash, and the SHA-256 is already computed.
- **Trust the client at commit** — a buggy client could commit refs to chunks it never uploaded, and other devices would sync a broken revision.
- **Bucket notifications (S3 → SQS)** — Garage doesn't support them, so two code paths; ruled out by ADR-0011.
- **`If-None-Match: *`** — makes AWS (first writer wins) and Garage (ignored, last writer wins) behave differently for no benefit.
- **Longer expiry (1 h+)** — more time for a leaked URL to be used, and pushes against the GC grace invariant.

## Consequences

- `chunks` gains `uploaded_at` (nullable). The ciphertext SHA-256 is not stored as an authoritative column: it describes whichever device asked last, not necessarily the bytes that won.
- Self-hosters need a second HTTPS hostname for Garage. The Compose stack and self-hosting docs must show it.
- Integrity and auth behaviour can only be tested against a real store. Unit tests use gofakes3 for the flow; `spikes/07-uploads` becomes the seed of the Garage contract test suite (testcontainers-go). The same suite runs against AWS in phase 2, confirming the AWS rows that are still expected-from-docs.
- With direct transfer, the object store (AWS in phase 2) sees device IP addresses in its access logs. To be noted in any future privacy policy for hosted deployments.
- Request costs on AWS are small: about $0.0054 per 1,000 new chunks (PUT + HEAD), us-east-1, 2026-11-21 price list.
