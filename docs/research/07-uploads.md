# Spike 07: Uploads

- **Issue:** #7
- **Status:** Decided. See [ADR-0015](../adr/0015-direct-presigned-chunk-transfer.md) (direct presigned transfer, signed SHA-256 and length, 15-minute URLs, HEAD on commit, Garage pinned at v2.5.0) and [ADR-0016](../adr/0016-upload-reservations-and-idempotent-commits.md) (reservation rows, weekly orphan sweep, `commit_id`). Hands-on run against gofakes3 v1.3.0 and Garage v2.5.0 on 2026-11-24 (section 11); AWS cases deferred to phase 2. Re-check the Garage v2.6.0 image before moving the pin.
- **Question:** How does a client get encrypted chunks into the object store and commit a revision that points to them, safely, cheaply, and with the same code on Garage and AWS S3? Covers presigned URLs, checksums, verification, retries, orphan cleanup, downloads and cost. (The issue text still says "MinIO"; MinIO is archived and replaced by Garage, [ADR-0011](../adr/0011-local-object-storage.md).)

Facts below were checked on 2026-11-24 unless marked *unverified*. Three kinds of evidence are used, and each claim says which:
- **Probe:** small Go programs run locally in the scratchpad against `aws-sdk-go-v2` **service/s3 v1.116.0** (2026-11-18) and core v1.48.0. They use fake credentials and make **no network calls**. The gofakes3 probe drives gofakes3 v1.3.0 as an in-process `http.Handler` (no port is opened).
- **Source:** reading the code of Garage (tag **v2.6.0**, tagged 2026-11-24; v2.5.0 tagged 2026-10-23), aws-sdk-go-v2 and gofakes3.
- **Docs:** AWS documentation and the AWS public price list (published 2026-11-21).

Nothing was run against Garage or AWS. Section 11 lists the tests that would confirm the remaining points.

---

## 0. Decisions this builds on

- E2E encryption: the client chunks, then encrypts. The server never sees plaintext ([ADR-0007](../adr/0007-end-to-end-encryption.md)).
- Keyed FastCDC with 256 KiB / 1 MiB / 4 MiB (min / average / max) chunks. Chunk IDs are HMAC-SHA256 ([ADR-0009](../adr/0009-chunking-and-chunk-ids.md)).
- Each chunk is **one padded object** (Padmé). The `chunks` table is a location index. Manifests are encrypted and stored in Postgres, alongside an unordered ref set that the server can see. GC uses a 24 h grace period ([ADR-0010](../adr/0010-chunk-storage-and-manifests.md)).
- The object store uses a minimal S3 contract behind an `ObjectStore` interface: Garage locally, S3 in phase 2, gofakes3 in unit tests. CloudFS does not rely on conditional writes, bucket notifications or versioning ([ADR-0011](../adr/0011-local-object-storage.md)).
- The Postgres change log is the event source ([ADR-0003](../adr/0003-sync-model.md)).

---

## 1. Upload flow: direct, proxied, or hybrid

**Jargon:**
- A **presigned URL** is a normal HTTPS URL with a signature in its query string. The server makes it using its own store credentials. Anyone holding the URL can do exactly one thing (e.g. PUT to one key) until it expires, without having any credentials of their own.
- **Egress** is data leaving AWS for the internet. It is billed per GB. Data coming in (ingress) is free.

### Options

| | (a) Direct presigned | (b) Proxy through CloudFS API | (c) Hybrid |
|---|---|---|---|
| Data path | client ⇄ store | client ⇄ API ⇄ store | direct by default, proxy as a config option |
| API server load | Control plane only (small JSON) | Every byte passes through the API: CPU, memory, bandwidth | Depends on mode |
| Ports to expose (self-host) | API **and** the store's S3 endpoint (Garage :3900, behind TLS) | API only | Both code paths exist |
| Integrity check | Store checks the signed SHA-256 (section 3) | API can hash while streaming | Both |
| Auth surface | API + presigned URLs (bearer tokens, short-lived) | API only; store reachable only from the server | Union of both |
| AWS cost | S3 ingress is free; ECS never touches chunk bytes | Same S3 charges **plus** ALB processing, Fargate CPU/network, and NAT data processing if tasks reach S3 through a NAT gateway (*rates unverified; spike 09*) | — |
| AWS egress (downloads) | S3 → internet, $0.09/GB after the free tier | Same egress, now billed as ECS/ALB → internet. Proxying does not avoid it | — |
| Scaling | S3 scales on its own | API tasks have to scale with transfer volume | — |
| Code | Presign + verify on commit | Streaming proxy, back-pressure, timeouts, large bodies through the HTTP server | Most code |

**Self-hosted single node.** Bandwidth is not the deciding factor here, since the API and Garage share one machine and one uplink. The cost of (a) is operational: Garage's S3 endpoint must be reachable by clients over TLS. Garage's own docs recommend a reverse proxy for TLS and show `https://s3.example.com` style setups (Garage cookbook, reverse-proxy page). A **subdomain** (`s3.cloudfs.example`) works. A **path prefix** on the API host (`cloudfs.example/s3/...`) does not, if the proxy strips the prefix, because SigV4 signs the request path (*reasoned from SigV4, not tested*).

**Gotcha for (a):** the URL's `Host` is part of the signature. The probe shows `host` is always in `X-Amz-SignedHeaders`. So the server must presign against the **public** endpoint that clients use, not the internal Docker hostname (`garage:3900`). In practice that means two S3 client configs:
- an internal one for HEAD, DELETE and LIST
- a public one used only for presigning (`BaseEndpoint = https://s3.cloudfs.example`)

**Leaning:** (a) on both Garage and AWS. It is what the architecture sketch in design §8 already assumes. It is also the standard production pattern, which serves the portfolio goal. A proxy mode can be added later if self-hosters ask for a single port. It is not worth building two data paths in v1.

### Proposed end-to-end flow

```
client                                API (Postgres)                          store
  │ 1 POST /chunks/query {ids≤1000}    │ lookup chunks; for present+unreferenced
  │ ◄─ {missing:[...]}                 │   rows set unreferenced_since=now()
  │ 2 encrypt+pad each missing chunk; SHA-256 of ciphertext
  │ 3 POST /chunks/uploads             │ derive object_key from chunk_id
  │   {[{id, length, sha256}]≤256}     │ upsert chunks row (uploaded_at NULL,
  │                                    │   length, unreferenced_since=now())
  │ ◄─ {[{id, url, headers, expires_at}]}  presign locally (no network)
  │ 4 PUT url (+ exact signed headers) ──────────────────────────────────────► verify sig, expiry, SHA-256
  │ 5 POST /nodes/{id}/content         │ HEAD each ref with uploaded_at NULL ──► exists? length?
  │   {base_rev, manifest, refs, commit_id}  set uploaded_at; then one tx:
  │                                    │   all refs uploaded? CAS base_rev,
  │                                    │   insert revision + refs, clear
  │                                    │   unreferenced_since, append change
  │ ◄─ ok / conflict / missing:[ids]
```

Notes on the flow:
- **Steps 1 and 3 are separate calls on purpose.** The client only spends CPU encrypting the chunks the server doesn't already have.
- **The server picks the object key.** It derives it from the chunk ID (e.g. `chunks/<hex id>`). It never accepts a key from the client, which is the presign version of "don't trust a client-supplied ID".
- **Step 3 rejects lengths above the maximum padded chunk size** (4 MiB max chunk plus Padmé plus header, roughly ≤ 4.6 MiB; the exact figure comes from the format). The length is signed in as `Content-Length` (section 2), so a URL can't be used to upload anything larger.
- **"Missing" includes rows with `uploaded_at IS NULL`.** A second device whose upload is still pending is not counted as "present", so it uploads its own copy too (section 5).

---

## 2. Presigned PUT details

### Is multipart needed? No.

- A single PUT can be up to **5 GB** (AWS integrity docs: "upload an object of up to 5 GB in a single operation").
- CloudFS chunks are at most about 4.6 MiB after padding. Even future ~16 MiB packs are far below the limit.
- **Multipart upload** (uploading one object as several numbered parts) has a 5 MiB minimum part size on S3 (*from prior knowledge*), so it couldn't be used meaningfully at this size anyway.
- Dropping multipart removes create/complete/abort calls, part checksums, and the cleanup rule for incomplete uploads.

### What the Go SDK puts in the URL (probe, s3 v1.116.0)

| `PutObjectInput` fields set | `X-Amz-SignedHeaders` | Extra query params | Headers the uploader **must** send |
|---|---|---|---|
| `ChecksumSHA256` | `host;x-amz-checksum-sha256` | none | `x-amz-checksum-sha256: <base64>` |
| `ChecksumSHA256` + `ChecksumAlgorithm: SHA256` | `host;x-amz-checksum-sha256` | `X-Amz-Sdk-Checksum-Algorithm=SHA256` | same |
| `ChecksumSHA256` + `ContentLength` + `IfNoneMatch: "*"` | `content-length;host;if-none-match;x-amz-checksum-sha256` | none | all three |
| `ContentMD5` | `content-md5;host` | none | `Content-MD5` |
| nothing (client set to `WhenSupported`) | `host` | none | none |

Findings:
- **The checksum goes in a signed header, not the query string.** The presign result's `SignedHeader` map lists the exact headers the client must send. If a signed header is missing or different, the signature fails and the store returns 403 `SignatureDoesNotMatch`.
- **The presign client ignores the client's `RequestChecksumCalculation` setting.** `PresignPutObject` adds `withNoDefaultChecksumAPIOption`, which forces `WhenRequired` (source, `api_op_PutObject.go`). The SDK's January 2025 default-CRC32 behaviour therefore never leaks into presigned URLs. Design §12's checksum-mode switch only affects the server's own direct calls.
- **Older SDK bugs here are fixed:** aws-sdk-go-v2 #2387 (checksum header hoisting, closed 2023-11-27) and #2610 (`PresignPutObject` ignores `ChecksumAlgorithm`, closed 2024-04-18).
- **Leave `ChecksumAlgorithm` unset.** It only adds an `X-Amz-Sdk-Checksum-Algorithm` query param, which neither store needs. AWS's REST guidance says "don't use the `x-amz-sdk-checksum-algorithm` parameter" when calling the REST API directly. The existing `spikes/07-uploads/main.go` sets it; that is harmless but unnecessary.
- **Also sign `ContentLength`.** It costs nothing and caps the upload size at the signature level.

### Expiry

| Signer credentials | Effective URL lifetime | Source |
|---|---|---|
| IAM user access key (SigV4) | Up to 7 days | AWS presigned-URL docs |
| IAM role / STS session | Ends when the session ends, "even if you specify a longer expiration time". AssumeRole defaults to 1 h | AWS docs |
| ECS task role (phase 2) | Ends when the task's role credentials expire. AWS FAQ: these "typically rotate every 1-6 hours" | AWS docs |
| Garage access key | Up to 7 days. Garage returns 400 "X-Amz-Expires may not exceed a week" above that, and "Date is too old" after expiry | Garage source, `signature/payload.rs` |
| gofakes3 | **Not enforced** (signatures aren't checked at all) | Probe |

- **The SDK does not cap expiry.** A request for 8 days produced `X-Amz-Expires=691200` with no error (probe). The store rejects it later, at use time.
- **AWS checks expiry when a request starts.** A transfer that starts before expiry may finish after it. A retry after expiry fails (AWS docs).
- **The temporary-credential trap.** If the server signs with role credentials that have 3 minutes left, the URL dies in 3 minutes, whatever `X-Amz-Expires` says. Mitigations:
  - compute `expires_at = min(now + 15 min, credentials.Expires)` and return it to the client
  - the client treats a 403 on PUT as "get a new URL", not as a fatal error
  - optionally refresh credentials early via `aws.CredentialsCacheOptions.ExpiryWindow` (whether the ECS endpoint hands out fresh credentials early is *unverified*)

**Suggested expiry: 15 minutes**, which is also the SDK default (`900`).
- It is long enough: a 4.6 MiB PUT at 1 Mbit/s takes about 40 s.
- It is short enough to limit what a leaked URL can do.
- It must stay **far below the 24 h GC grace**. Section 6 relies on that invariant, so validate it in config at startup.

The client should fetch URLs **just in time**, in batches (section 8), rather than all at once for a 10,000-chunk initial sync.

---

## 3. Checksum integrity

### AWS S3 (docs)

- `x-amz-checksum-sha256` is "the Base64 encoded, 256-bit SHA256 digest of the object" (PutObject API reference). It is Base64 of the raw 32 bytes, not hex.
- On a single-part upload, "If the checksum value calculated by S3 matches your provided value, the request is accepted. If the values don't match, the request is rejected." The SDK section of the same page names the error **`BadDigest`**. The exact code and status for a mismatch on a *presigned* PUT are *unverified (test 2)*.
- S3 "never adds partial objects". Success means the whole object was stored (PutObject reference).
- SigV4 presigned URLs support SHA-256 and the other checksum algorithms. SigV2 supports MD5 only (presigned-URL docs).
- `x-amz-sdk-checksum-algorithm` is **not required**. If it is sent, a matching `x-amz-checksum-*` header (or trailer) must be sent too, or S3 returns 400.

### Garage v2.6.0 (source)

- **PutObject verifies `x-amz-checksum-sha256`.** `put.rs` reads any `x-amz-checksum-*` header into `ExpectedChecksums.extra`. `Checksums::verify` compares it with the SHA-256 it computes while streaming the body.
  - On mismatch Garage returns **400 `InvalidDigest`**, not `BadDigest`. A wrong `Content-MD5` gives 400 `BadDigest`.
  - **The client must treat both codes as "checksum failed".**
- **A failed upload doesn't replace the old object.** Verification runs before the new object version is marked `Complete`. On failure the version is cleaned up, so readers keep seeing the previous complete object.
- **Presigned requests must sign every `x-amz-*` header they send** (`verify_signed_headers`). A client can't drop or add a checksum header without breaking the signature.
- `x-amz-*` query params in presigned URLs are treated as headers. Since commit 86e613e (in **v2.6.0 only**, fixes Garage #1236), an *unsigned* checksum query param is ignored, as on AWS. Before v2.6.0 it would have been enforced. CloudFS isn't affected, because the Go SDK doesn't emit one (probe); it is a reason to move to v2.6.0 once its image is published and tested.
- Garage ignores `X-Amz-Sdk-Checksum-Algorithm` (nothing reads it).
- Release history: CRC64NVME and `x-amz-checksum-type` in v2.0; "don't panic on missing checksum" (#1389) on 2026-03-17; `x-amz-checksum-type` response header (#1511) on 2026-09-15; composite-checksum suffix (#1555) in v2.5.0.

### gofakes3 v1.3.0 (probe)

| Case | Result |
|---|---|
| Correct body + signed SHA-256 | 200 |
| **Tampered body + signed SHA-256** | **200 (accepted)** |
| Wrong `Content-MD5` | 400 `BadDigest` |
| Corrupted signature | 200 (signatures not checked) |
| URL used after expiry | 200 (expiry not checked) |
| `If-None-Match: *` first / second PUT | 200 / **412** |

**gofakes3 can't test integrity or auth.** It checks `Content-MD5` and streaming trailer checksums, but not a plain `x-amz-checksum-sha256` header. It checks no signatures and no expiry. This matches ADR-0011's rule that checksum and signature enforcement are asserted against Garage.

### Alternative: Content-MD5

- Both stores verify it, and so does gofakes3.
- MD5 is cryptographically broken. That doesn't matter much for detecting accidental corruption, but it would be a downgrade.
- SHA-256 is already computed and already in the design.

**Recommendation:** SHA-256 only. Don't send both, because Garage rejects more than one `x-amz-checksum-*` header ("multiple x-amz-checksum-* headers given"; MD5 is separate, so this is about not adding a second algorithm).

---

## 4. Verifying an upload before accepting a commit

The server can't check that a chunk matches its keyed ID (no key, no plaintext; ADR-0007). It can check that **an object exists at the expected key with the expected length**. The SHA-256 was already enforced by the store at PUT time.

| Option | How | Cost | Works on Garage? | Catches |
|---|---|---|---|---|
| Trust the client | Commit without checking | 0 | Yes | Nothing. A buggy client can commit refs to chunks it never uploaded. Other devices then sync a broken revision, which ADR-0010's model can't detect until download |
| **HEAD new chunks at commit** | HEAD each ref whose row has `uploaded_at IS NULL`, in parallel. Check it exists and `Content-Length == chunks.length`. Set `uploaded_at` | $0.0004 per 1,000 HEADs (section 10). One parallel round of API→store latency per commit with new chunks (same machine or same region; *latency unmeasured*) | Yes | Missing uploads, wrong-size uploads |
| Bucket notifications | S3 → SQS/EventBridge, a worker marks chunks uploaded | Per-event SQS/EventBridge charges | **No.** Garage lists `PutBucketNotificationConfiguration` as "❌ Missing" (S3 compatibility matrix) | Same as HEAD, but asynchronously |

**Recommendation: HEAD at commit, only for chunks not yet confirmed.**
- Each chunk is HEADed **once in its lifetime**: the first commit that references it sets `uploaded_at`.
- Do the HEADs **before** opening the commit transaction, so no DB transaction is held open across network calls.
- Inside the transaction, require `uploaded_at IS NOT NULL` for every ref. Otherwise reply `missing: [ids]` and the client re-uploads those.
- Notifications are ruled out by ADR-0011 and would mean two code paths.
- Optional: send `ChecksumMode: ENABLED` on the HEAD to read back the stored SHA-256, but see section 5 for why the server shouldn't treat that value as authoritative.

---

## 5. Idempotency and retries

### Retrying a PUT is safe

- Keys are derived from chunk IDs, so a retry writes the same key with the same bytes.
- S3 never stores partial objects.
- Garage keeps the previous complete version if a new upload fails or is interrupted.

### Two devices uploading the same chunk

Ciphertext isn't deterministic (random nonce), so device A and device B produce **different bytes** for the same chunk ID.

- **Length is identical.** Padmé depends only on plaintext size, and the header and tag are fixed size, so `chunks.length` stays correct whoever wins.
- **The object store is last-writer-wins.** AWS: "If it receives multiple write requests for the same object simultaneously, it overwrites all but the last object written" (PutObject reference). Either ciphertext decrypts to the same plaintext under the same key, and the chunk ID is bound into the AEAD associated data. **Either version is correct.**
- **Don't store the ciphertext SHA-256 as an authoritative column on `chunks`.** It would describe whichever device asked for a URL last, not necessarily the bytes that won. Keep it only on the upload request for the signed URL.
- **Edge case: a download that spans an overwrite.** A client that resumes a GET across two requests while the object is overwritten could mix bytes from two ciphertexts. The AEAD tag fails, and the client retries the whole chunk. This is rare and safe.

### Conditional write `If-None-Match: *`

**Jargon:** a conditional write tells the store "only write if no object with this key exists".

- **AWS:** supported. A second write gets 412 Precondition Failed. A delete racing the write gives 409, and the docs say to retry (conditional-writes docs). A bucket policy can *require* it via the `s3:if-none-match` condition key.
- **Garage:** **not supported, by design**: "structurally impossible to implement in Garage due to the lack of a consensus algorithm" (known-issues doc). There is no `If-None-Match` handling in Garage's PutObject (source; it only appears in GET/copy code). The header is accepted, signed, and **silently ignored**, so the second write overwrites.
- **gofakes3:** supported (probe: 412 on the second PUT).

**Does CloudFS need it?** No. Both outcomes, "first write wins" (AWS with the header) and "last write wins" (Garage, or AWS without it), are correct for CloudFS. The checksum already stops a leaked URL from writing arbitrary bytes. Using it would make AWS and Garage behave differently for no benefit, and would add "412 means success" handling to the client. **Leave it out in v1.** That is consistent with ADR-0011.

### Idempotent commit

- The commit is a compare-and-swap (CAS) on `base content_rev` (ADR-0003). **Jargon:** CAS means "apply only if the current value is still the one I based my change on".
- **Problem:** if the response is lost and the client retries, the server's revision has already moved on. The retry looks like a stale base, so the client would treat its own commit as a conflict and create a "conflicted copy" of itself.
- The server can't recognise "same content", because manifests are encrypted with a random nonce.
- ADR-0005's "check by content hash" idempotency covers conflicted-copy creation, not this case.

**Fix:** the client generates a random `commit_id` (UUID) per logical commit and reuses it on retries.
- The server stores it with a unique constraint, e.g. on `changes` or `revisions`, and keeps it at least as long as clients might retry. Using the 90-day change log retention is simplest.
- A retry with a known `commit_id` returns the original result instead of re-evaluating.
- This is the standard idempotency-key pattern.

### Retry and backoff guidance (client)

| Response | Meaning | Action |
|---|---|---|
| Network error, 5xx, 503 `SlowDown` | Transient | Retry the same URL with exponential backoff and full jitter (e.g. base 200 ms, cap 20 s). Get a new URL once it has expired |
| 403 (`AccessDenied`, `SignatureDoesNotMatch`, expired) | URL expired, clock skew, or a bug | Request a new URL once. Repeated 403s are a bug: log and stop |
| 400 `BadDigest` (AWS) / `InvalidDigest` (Garage) | Bytes changed between hashing and sending | Retry once with the same buffer. If it fails again, re-encrypt, re-hash and request a new URL |
| Commit → `missing: [ids]` | Upload never landed, or GC raced it | Re-upload those chunks, then retry the commit with the **same** `commit_id` |

For comparison, the SDK's own standard retryer uses 3 attempts and a 20 s maximum backoff (`aws/retry/standard.go`). The client's raw HTTP PUTs need their own retry loop, since they don't go through the SDK.

---

## 6. Orphans: uploaded but never committed

### Option A: reservation row at URL issue

Step 3 upserts a `chunks` row with `uploaded_at = NULL` and `unreferenced_since = now()`. If no commit ever references it, the existing GC (delete when unreferenced for more than 24 h) removes both the object and the row.
- No new GC mechanism; it reuses ADR-0010's grace period.
- A DeleteObject on a key that was never uploaded is harmless. S3 DELETE is idempotent (*AWS returns 204 for a missing key; not re-checked this session*).

### Option B: ListObjectsV2 sweep

List every key and delete those with no `chunks` row and `LastModified` older than the grace period.
- Catches objects with **no** row at all: bugs, manual uploads, or objects uploaded after the backup a restored database came from (ADR-0003's epoch case; devices re-upload anyway).
- Cost: LIST requests are billed at the PUT rate, but each returns up to 1,000 keys. About 131k objects (ADR-0010's estimate) is about 132 LISTs, roughly $0.0007 per sweep.

**Recommendation: A as the main path, plus B as a weekly repair job.**

### The race to close

GC could delete an object while a device is uploading or committing it. This is ADR-0010's "told it exists, then GC deleted it" race, plus a new upload-side variant.

**Fixes:**
1. **Refresh the grace period whenever the chunk is mentioned.**
   - `/chunks/query` sets `unreferenced_since = now()` on present-but-unreferenced rows it reports as present.
   - `/chunks/uploads` does the same on the rows it upserts.
2. **GC holds the row lock while it deletes.** In one transaction: `SELECT … FOR UPDATE` the row, still unreferenced past the grace period → `DeleteObject` → `DELETE` the row → commit. A concurrent query or upload request for that chunk waits, then sees no row and treats the chunk as missing.
3. **Invariant: URL expiry (15 min) ≪ GC grace (24 h).** Any URL that still works was issued less than 15 min ago, so its row's grace hasn't run out and GC won't touch it. Check this at startup.
4. **HEAD at commit (section 4)** is the final safety net if something still slips through.

These details belong to the GC implementation, but the upload API has to provide points 1 and 3.

---

## 7. Downloads

**Use presigned GETs, same as uploads.**
- **v1 needs no Range.** Each chunk is its own object at offset 0, so the client GETs the whole object.
- **Packs later.** The server presigns with `Range: bytes=<offset>-<offset+length-1>` taken from the location index.
  - The probe shows `Range` becomes a **signed header** (`host;range`), so the client must send exactly that range.
  - AWS FAQ: if `Range` is signed and the request also carries `If-Range`, then `If-Range` must be signed too.
- **Same expiry (15 min)** and same just-in-time batching as uploads.
- **Direct vs proxy:** same trade-off as section 1. Proxying downloads doesn't reduce AWS egress; it only moves it from S3 to ECS/ALB.
- **Integrity on download** comes from the AEAD tag plus recomputing the keyed chunk ID after decryption (spike 04). The client doesn't need S3's checksum mode.

---

## 8. Batching

- **Presigning is local CPU only.**
  - The probe presigned with fake credentials and no reachable endpoint, and succeeded. AWS docs: "Creating a presigned URL happens locally, so Amazon S3 doesn't generate an issuance event."
  - Benchmark (Apple M5, one goroutine): **~13.5 µs and ~28 KB of allocations per presigned PUT**, so 1,000 URLs take about 14 ms. It's cheap.
- **Suggested limits** (API design choices, not store limits):
  - `/chunks/query`: up to 1,000 IDs per call (32-byte IDs, about 70 KB of hex JSON)
  - `/chunks/uploads`: up to 256 URLs per call
  - the client keeps a pipeline of about 8–16 concurrent PUTs over keep-alive connections and fetches the next batch of URLs when the queue runs low
- **Many small files:** each one is one chunk and one PUT. For small files, the fixed per-request overhead (latency and $0.005 per 1,000) dominates.
  - 100k small files = 100k PUTs = $0.50 on AWS, a one-off.
  - Concurrency hides the latency.
  - Inlining tiny files into the manifest is already listed as deferred (ADR-0010) and would remove these PUTs if it's ever needed.
- **One commit per file** stays the unit (ADR-0003). Batching several file commits into one request is a possible later optimisation, not needed for v1.

---

## 9. Security

- **What a leaked URL allows:**

  | URL | Allows |
  |---|---|
  | PUT | Uploading **exactly those bytes** (checksum signed) to **one key** with **one method**, up to the signed length, for ≤ 15 min. Harmless: it can only rewrite what's already there |
  | GET | Downloading one chunk's **ciphertext** for ≤ 15 min. Content stays protected by E2E. It leaks the padded size and the existence of that chunk |

  **Without the signed checksum, a leaked PUT URL could overwrite a live chunk with garbage.** The AEAD would catch it on download, but the chunk would be lost if no device still had it. This is the main reason the signed checksum matters.
- **Don't log full URLs.** The signature is in the query string.
  - On AWS, server access logs and CloudTrail data events record the Request-URI, which "can contain credential information and an AWS STS security token" (AWS presigned-URL docs).
  - Log the key and expiry, not the URL.
- **Signer credentials:**
  - **Phase 2:** the ECS task role, scoped to the one bucket ARN (and a prefix such as `chunks/*`): `s3:PutObject`, `s3:GetObject`, `s3:DeleteObject` on objects and `s3:ListBucket` on the bucket. A presigned URL can never do more than its signer's permissions allow (AWS docs).
  - **Optional bucket policy:** deny `s3:signatureAge` > 15 min (the example in AWS docs) and deny non-TLS requests.
  - **Garage:** one access key allowed `--read --write` on the one bucket only, without `--owner`. Garage permissions are per bucket (read/write/owner), not per operation (*per Garage quick-start; finer policy not available*).
- **Clients never hold store credentials.** They hold only device auth for the API (spike 08) and short-lived URLs.
- **Every upload/download/commit endpoint needs device auth.** It doesn't check per-user ownership (single user, ADR-0002), but it does derive the object key server-side and checks that a commit's refs exist as rows.

### New user data stored or transmitted (flag for the plan)

| New data | Where | Notes |
|---|---|---|
| Ciphertext SHA-256 per chunk upload | Client → API; signed into the URL; stored by S3/Garage as object metadata | A hash of ciphertext; reveals nothing about plaintext. Keep it in the database only as long as needed, or not at all |
| `chunks.uploaded_at` (new nullable column) and the declared ciphertext length | Postgres | Metadata. Length is already in the design |
| `commit_id` (random UUID per commit) | Postgres | Not content. Retention follows the change log |
| Client IP address and user agent at the **object store** | S3 server access logs / CloudTrail (if enabled), Garage logs | **New recipient:** with direct uploads, the store (AWS as a processor in phase 2) sees device IPs. Before, only the API did |

**Privacy policy / ToS:** the repo has none yet (checked: no privacy or terms file in `cloudfs/`). One may eventually be needed for hosted deployments. The AWS-access-log point above should go into it when it exists.

---

## 10. Rough AWS cost (us-east-1, S3 Standard)

From the AWS public price list API (S3 offer published 2026-11-21; data transfer offer 2026-11-19):

| Item | Price | Per 1,000 uploads (avg ~1 MiB chunk) |
|---|---|---|
| PUT / COPY / POST / LIST | $0.005 per 1,000 | **$0.005** |
| GET and "all other requests" (HEAD falls here) | $0.0004 per 1,000 ($0.004 per 10,000) | HEAD verify: **$0.0004** |
| DELETE | Free (pricing page) | GC deletes: $0 |
| Data in from the internet | $0.00/GB | $0 |
| Storage | $0.023/GB-month (first 50 TB) | ~1 GB → ~$0.023/month |
| Egress to the internet (downloads) | $0.09/GB, first 10 TB/month, "beyond the global free tier" (100 GB/month per the pricing page) | Re-downloading the same 1,000 chunks: ~$0.09 + $0.0004 in GETs, if over the free tier |
| S3 → AWS services in the same region | Free | — |

**Uploading and verifying 1,000 chunks costs about $0.0054 in requests.** Storage and egress dominate. Request costs don't. This matches spike 04's conclusion. *Unverified:* whether S3 bills failed requests (403/400); AWS changed this for some unauthorized requests in 2024 (*not re-checked*).

---

## 11. Hands-on tests (run 2026-11-24)

`spikes/07-uploads/main.go` runs each case below and reports the HTTP status, the S3 error code, and whether the object exists afterwards. By default it runs against gofakes3 in-process; environment variables point it at a real store. The Garage run used a throwaway `dxflrs/garage:v2.5.0` container (`--single-node --default-bucket`), removed afterwards. Every presigned PUT signs `x-amz-checksum-sha256` and `Content-Length`; the SDK's signed headers were `Host, Content-Length, X-Amz-Checksum-Sha256`.

| # | Case | gofakes3 v1.3.0 (measured) | Garage v2.5.0 (measured) | AWS (expected from docs, **not run**; phase 2) |
|---|---|---|---|---|
| 1 | Correct body | 200 | 200 | 200 |
| 2 | Tampered body | **200, stored** | 400 `InvalidDigest`, not stored | 400 `BadDigest` (*confirm*) |
| 3 | Checksum header omitted | **200, stored** | 400 `InvalidRequest`, not stored | 403 `SignatureDoesNotMatch` (*confirm*) |
| 4 | Different checksum header value | **200, stored** | 403 `AccessDenied`, not stored | 403 |
| 5 | Tampered body over an existing object | **200, overwritten** | 400 `InvalidDigest`; old object intact | 400; old object intact |
| 6 | Body 1 byte shorter than signed `Content-Length` | **200, stored** | 403 `AccessDenied`, not stored | 403 (*confirm*) |
| 7 | `ChecksumAlgorithm` also set (adds `X-Amz-Sdk-Checksum-Algorithm` query param) | 200 | 200 | 200 (*confirm*) |
| 8 | `If-None-Match: *` signed, second PUT | 412 `PreconditionFailed`; first kept | **200; overwritten** (header ignored) | 412 |
| 9 | URL used after expiry (2 s) | **200, stored** | 400 `InvalidRequest` | 403 |
| 10 | Expiry 8 days | **200, stored** | 400 `InvalidRequest` | rejected (*confirm code*) |
| 11 | Signed for `localhost`, sent to `127.0.0.1` | **200, stored** | 403 `AccessDenied` | 403 |
| 12 | Presigned GET with signed `Range: bytes=100-199` | 206, correct 100 bytes | 206, correct 100 bytes | 206 |
| 12b | Same URL, client sends a different `Range` | **206, 1000 bytes** | 403 `AccessDenied` | 403 |
| 13 | Two concurrent PUTs, same key, different same-length ciphertexts | both 200; one stored | both 200; one stored | both 200; last writer wins |
| 14 | HEAD with `ChecksumMode: ENABLED` | length + SHA-256 returned | length + SHA-256 returned | returned |
| 15 | (AWS only) URL signed with short-lived STS credentials, used after they expire | n/a | n/a | 403 `ExpiredToken` (*confirm*) |

**What this shows**
- **Garage enforces everything CloudFS relies on:** checksum, length, expiry, host and range are all bound by the signature, and a failed upload never replaces an existing object.
- **gofakes3 enforces none of it** (bold cells). It's fine for testing the upload *flow* in unit tests, but integrity and auth tests must run against Garage (integration tests, testcontainers-go per ADR-0011).
- **Error codes differ by store** (`InvalidDigest` vs `BadDigest`, 400 vs 403 for missing header or expired URL). The client should treat any 4xx on a chunk PUT as "get a new URL and retry once, then report", not branch on the exact code.
- **Garage ignores `If-None-Match`**, confirming the source reading. CloudFS doesn't need it (section 5).
- **Garage v2.6.0** was tagged on 2026-11-24, but `dxflrs/garage:v2.6.0` was **not yet on Docker Hub** that day (`not found`), so it wasn't tested. Stay pinned to v2.5.0 and re-run this program before moving the pin.

---

## Recommendation (accepted 2026-11-24)

1. **Direct presigned PUT/GET** on both Garage and AWS. The API is a control plane only. Self-hosters expose Garage on a TLS subdomain. The server presigns with a separate "public endpoint" client config.
2. **Single PUT per chunk, no multipart.**
3. **Sign `x-amz-checksum-sha256` (Base64) and `Content-Length`.** Don't set `ChecksumAlgorithm`, `Content-MD5` or `If-None-Match`. Clients send exactly the returned `SignedHeader` set. Treat any 4xx on a chunk PUT as "new URL, retry once, then report" (codes differ by store, section 11). Keep Garage pinned at v2.5.0 (tested) until v2.6.0's image is published and passes the section 11 run.
4. **15-minute expiry**, capped by the signer's credential expiry and returned to the client as `expires_at`. Validate at startup that expiry ≪ GC grace.
5. **HEAD at commit, once per new chunk** (existence plus length), recorded as `chunks.uploaded_at`. No bucket notifications.
6. **Reservation row at URL issue plus the existing 24 h GC** for orphans; a **weekly ListObjectsV2 sweep** as a repair job. GC deletes under a row lock. Query and upload calls refresh `unreferenced_since`.
7. **Idempotency key (`commit_id`) on commits.** Client-side backoff as in the section 5 table.
8. **Batching:** 1,000 IDs per query, 256 URLs per upload call, 8–16 concurrent PUTs, URLs fetched just in time.
9. **Contract tests** (section 11) against Garage in CI and AWS nightly in phase 2. gofakes3 only for flow and unit tests, never for integrity or auth.

---

## Decisions for the user

1. **Data path.**
   - Options: (a) direct presigned, (b) proxy through the API, (c) hybrid.
   - **Recommended: (a).** It keeps the API small, costs nothing extra on AWS, and is the standard cloud pattern. Its only cost is exposing Garage on a TLS subdomain locally. Proxy mode can come later if self-hosters ask.
2. **Checksum mechanism.**
   - Options: signed `x-amz-checksum-sha256`; `Content-MD5`; none.
   - **Recommended: SHA-256.** It is already decided in spirit (ADR-0010), Garage and AWS both enforce it, and MD5 is weaker. Accept that gofakes3 can't test it.
3. **Upload verification before commit.**
   - Options: trust the client; HEAD new chunks; bucket notifications.
   - **Recommended: HEAD new chunks once.** It costs $0.0004 per 1,000 and works identically on both stores. Notifications break ADR-0011.
4. **Conditional writes (`If-None-Match`).**
   - Options: use on AWS only; never.
   - **Recommended: never in v1.** Last-writer-wins is correct here, and Garage silently ignores the header.
5. **Presigned URL expiry.**
   - Options: 5 min; 15 min; 1 h.
   - **Recommended: 15 min.** It is the SDK default, comfortably longer than one chunk upload, and far shorter than the GC grace.
6. **Orphan cleanup.**
   - Options: reservation rows only; LIST sweep only; both.
   - **Recommended: both.** Reservation rows handle the normal case through existing GC. A weekly sweep catches anything without a row, for well under a cent.
7. **Commit idempotency.**
   - Options: `commit_id` idempotency key; rely on the conflict path.
   - **Recommended: `commit_id`.** Without it, a lost response turns into a self-conflict and a spurious conflicted copy.
8. **Schema additions** (all additive, safe to apply any time): `chunks.uploaded_at`, plus `commit_id` (unique) on `changes` or `revisions`. Which table holds `commit_id` is a detail for implementation.
9. **Garage version pin:** stay on v2.5.0 (passed every case in section 11) or move to v2.6.0 (tagged 2026-11-24 with the presigned checksum query-param fix, but its Docker image wasn't published yet). CloudFS's own URLs don't trigger that bug. **Recommended: stay on v2.5.0; move to v2.6.0 once its image exists and passes the section 11 run.**

---

## References

Fetched or read directly (2026-11-24):
- AWS: presigned URLs (credential types, 7-day max, temporary-credential expiry, `s3:signatureAge`, logging warnings, Range/If-Range FAQ): https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html
- AWS: PutObject API reference (`x-amz-checksum-sha256`, `x-amz-sdk-checksum-algorithm`, `If-None-Match` 412/409, last-writer-wins, no partial objects): https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html
- AWS: checking object integrity for uploads (mismatch rejected, `BadDigest`, 5 GB single PUT, REST guidance on `x-amz-sdk-checksum-algorithm`): https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html
- AWS: conditional writes: https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html and enforcing them via bucket policy: https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes-enforce.html
- AWS price list API, S3 us-east-1 (published 2026-11-21): https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonS3/current/us-east-1/index.json
- AWS price list API, data transfer us-east-1 (published 2026-11-19): https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSDataTransfer/current/us-east-1/index.json
- AWS S3 pricing page (free DELETE, free inbound, 100 GB egress free tier, same-region transfer free): https://aws.amazon.com/s3/pricing/
- aws-sdk-go-v2 source, service/s3 v1.116.0: `api_op_PutObject.go` (`PresignPutObject`, `withNoDefaultChecksumAPIOption`), `api_client.go` (default 900 s expiry); core v1.48.0 `aws/retry/standard.go`, `aws/credential_cache.go`: https://github.com/aws/aws-sdk-go-v2
- aws-sdk-go-v2 issues #2387 and #2610 (closed): https://github.com/aws/aws-sdk-go-v2/issues/2387, https://github.com/aws/aws-sdk-go-v2/issues/2610
- Garage source at v2.6.0: `src/api/s3/put.rs`, `src/api/common/signature/{checksum.rs,payload.rs}`, `src/api/s3/error.rs`; commit 86e613e (fix #1236): https://git.deuxfleurs.fr/Deuxfleurs/garage
- Garage known issues (no conditional writes): https://git.deuxfleurs.fr/Deuxfleurs/garage/src/branch/main-v2/doc/book/reference-manual/known-issues.md
- Garage S3 compatibility matrix (notifications missing): https://garagehq.deuxfleurs.fr/documentation/reference-manual/s3-compatibility/
- Garage reverse-proxy cookbook (TLS, `public_endpoint`): https://garagehq.deuxfleurs.fr/documentation/cookbook/reverse-proxy/
- gofakes3 v1.3.0 (probe) and HEAD source (2026-11-22): https://github.com/johannesboyne/gofakes3

Search summaries only (not fetched in full):
- Third-party confirmations of the S3 request prices (superseded by the price list API above): https://www.manageengine.com/cloudspend/articles/s3-pricing-guide.html, https://upstash.com/blog/aws-s3-pricing-explained-2026

Local probes (throwaway, not in the repo): `presignprobe/` (SignedHeaders table, expiry, Range, benchmark) and `fakeprobe/` (gofakes3 behaviour table). The section 11 case table is in the repo: [`spikes/07-uploads/`](../../spikes/07-uploads/).
