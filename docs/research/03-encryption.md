# Spike 03: Encryption

- **Issue:** #3
- **Status:** Decided. See [ADR-0007](../adr/0007-end-to-end-encryption.md) (option B: E2E in v1, chosen over the section 7 recommendation of option C) and [ADR-0008](../adr/0008-at-rest-encryption-aws.md) (free SSE-S3 + RDS defaults, KMS upgrade path). Name encryption moved to spike 11; MinIO replacement to spike 10. KMS pricing re-checked against the AWS pricing page on 2026-11-19.
- **Question:** Which layers of encryption does CloudFS use: in transit, at rest on the server, and/or client-side end-to-end (E2E)? If not E2E now, what do we do today so it can be added later without a rewrite?

---

## Where this spike starts

Decided so far:
- A central server with Postgres metadata and S3/MinIO chunk storage ([ADR-0003](../adr/0003-sync-model.md)).
- Stable node IDs, with name and parent as attributes ([ADR-0004](../adr/0004-stable-file-ids.md)).
- **The server enforces case-insensitive unique names per folder, no cycles, and a live parent** ([ADR-0005](../adr/0005-conflict-resolution.md)).
- Client-side conflict resolution.
- Single user ([ADR-0002](../adr/0002-single-user-first.md)).

Planned server-side workers handle versioning, pruning and chunk garbage collection; thumbnails were floated once. Spike 04 plans content-addressed chunks with dedup.

Encryption is decided early because it constrains three of those: how chunks are named (spike 04), what workers can see, and the server's name checks from ADR-0005.

---

## 1. The layers, and what each actually defends against

| Layer | What it is | Defends against | Does **not** defend against |
|---|---|---|---|
| **In transit: TLS** | HTTPS between client, API and S3. Phase 4 adds WireGuard. | Network eavesdroppers; tampering on the wire. | Anyone who can read the server or the bucket. |
| **At rest: disk encryption** (self-hosted) | LUKS, FileVault or BitLocker on the machine running Postgres and object storage. | A stolen or discarded disk/machine while powered off. | Anything while the machine is running and unlocked; leaked credentials. |
| **At rest: S3 SSE-S3** | S3 encrypts objects with keys AWS manages. **On by default for all new objects since 5 Jan 2023, and it can't be turned off.** | Physical theft of AWS drives. That's essentially it: S3 decrypts transparently for any request with `s3:GetObject`. | Leaked credentials, a misconfigured public bucket, AWS insiders, subpoena. |
| **At rest: S3 SSE-KMS** | S3 encrypts with a data key protected by a KMS key you control. Reading requires `s3:GetObject` **and** `kms:Decrypt` on that key. | Everything SSE-S3 does, plus: credentials or policies that grant S3 read but not KMS decrypt. Key disable/delete as a kill switch. CloudTrail audit of every key use. | A compromised API server (it holds both permissions); AWS itself under legal compulsion. |
| **At rest: S3 SSE-C** | The client sends its own key with every request; S3 encrypts and discards the key. | Similar to E2E for the at-rest copy, but S3 sees the key and the plaintext during each request. | **Practically ruled out:** since April 2026, SSE-C is **disabled by default on new buckets**, and AWS steers users to SSE-KMS. It also makes presigned URLs awkward (the key has to travel with each request). |
| **At rest: RDS encryption** | Encrypts the Postgres storage, snapshots and backups with KMS. | Stolen storage, leaked snapshots. | Anyone with DB credentials. *(From prior knowledge: it must be enabled when the instance is created. Unverified.)* |
| **Client-side end-to-end (E2E)** | Devices encrypt chunks (and optionally names) before upload. The server only ever stores ciphertext and never holds the key. | **All of the above, plus** a compromised server, a cloud provider or admin, leaked bucket or DB dumps, and subpoena of the provider. | A compromised *device* (it has the key). Metadata the design leaves visible (section 3). Losing the key = losing the data. |

**The plain-English summary:** server-side encryption protects against someone walking off with the disks. KMS adds protection against leaked or overly broad credentials. Only E2E protects against the server itself.

---

## 2. Threat model for CloudFS

CloudFS is single-user and self-hosted. The realistic attackers:

| Deployment | Realistic threats | What's sufficient |
|---|---|---|
| **Own hardware** (home server/laptop) | Stolen or disposed machine; someone on the LAN; a leaked admin password. | TLS + disk encryption. E2E protects against yourself, mostly. |
| **AWS** | Leaked IAM credentials (e.g. in a CI log or a laptop); a bucket policy mistake making objects public; a compromised API container; AWS insiders or legal requests (low probability, non-zero). | TLS + SSE-KMS + least-privilege IAM covers the likely ones. E2E is needed only if "the provider or a compromised server must not be able to read my files" is a requirement. |
| **Someone else self-hosting** for themselves | Same as above, but they may run on a cheap VPS they trust less. | E2E is the feature a privacy-minded self-hoster would value most. |

The honest summary: for the primary user on AWS, the most likely incident is a **credential or configuration mistake**. SSE-KMS plus IAM handles that well. E2E's main extra value is distrust of the server or provider, and it's a strong portfolio and open-source differentiator.

---

## 3. If E2E: what a design looks like

### Key hierarchy (the standard pattern)

```
passphrase ──Argon2id──► KEK (key-encryption key)
recovery key (random, printed) ──► KEK'
        │ wraps
        ▼
master key (random 256-bit, never leaves devices unwrapped)
        │ derives (HKDF)
        ├──► content key   (encrypts chunks)
        ├──► name key      (encrypts file/folder names)
        └──► chunk-ID key  (keyed hash for chunk IDs / dedup)
```

- **Why a master key and not the passphrase directly:** changing the passphrase only re-wraps the master key, not every file. Restic works this way: its key files hold the master key encrypted under a scrypt-derived key, and "a repository can have several different passwords". Cryptomator does the same: a random masterkey wrapped by a scrypt-derived KEK with AES key wrap.
- **The wrapped master key can live on the server.** It's useless without the passphrase, which is how a new device bootstraps: log in (spike 08), fetch the wrapped key, enter the passphrase.
- **Recovery key:** a second wrap of the master key, printed or stored offline. Without it, a forgotten passphrase means every file is gone.
- **Per-file keys** (Cryptomator: a random content key per file in a file header) vs **per-chunk keys derived from the master**: per-file keys make it possible to rotate or share individual files later, at the cost of a header per file. For single-user v1, one content key plus random nonces per chunk is enough.

### Building blocks in Go (from prior knowledge; well-established libraries)

- `golang.org/x/crypto/chacha20poly1305`: **XChaCha20-Poly1305** AEAD. Its 24-byte random nonces are safe to generate randomly per chunk. Syncthing's untrusted-device mode uses XChaCha20-Poly1305 for data.
- `golang.org/x/crypto/argon2`: Argon2id passphrase KDF. Restic, Cryptomator and Syncthing use scrypt; either is fine.
- `golang.org/x/crypto/hkdf`: derive sub-keys from the master key.
- AES-SIV (deterministic name encryption): not in x/crypto; third-party packages exist. Syncthing and Cryptomator both use AES-SIV for names.
- `filippo.io/age`: file encryption with X25519 or scrypt-passphrase recipients and a chunked STREAM payload with random-access decryption (`DecryptReaderAt`). Good for wrapping the master key, or for a device-to-device enrollment handoff. Less natural as the per-chunk format, because each age file carries its own header.

### What metadata leaks, and options to hide it

| Metadata | Leaks by default | Can we hide it? |
|---|---|---|
| File contents | No (encrypted) | — |
| File and folder **names** | Yes, unless encrypted | Encrypt names. **But see the ADR-0005 conflict below.** |
| **Folder structure** (`parent_id` graph) | Yes: the server needs it for cycle checks and moves | Not without moving structure validation to clients. Cryptomator flattens directories on storage; Syncthing hides structure entirely, at the cost of reusing blocks across files. |
| **Sizes** (file and chunk) | Yes, approximately | Padding reduces it; costs storage. Syncthing and Cryptomator both leave sizes roughly visible. |
| Timestamps and change rhythm | Yes (change log `created_at`, S3 object times) | Not practically. |
| Chunk IDs | Yes if they're a plain SHA-256 of content | Use a **keyed** hash (HMAC with the chunk-ID key), so the server can't test "do you have this known file?" |

**ADR-0005 interaction:** the server currently enforces case-insensitive unique names per folder. If names are encrypted, the server can't compare them. The standard fix is a **blind index**: the client also sends `HMAC(name_key, lowercase(normalized name))`, and the server enforces uniqueness on that value instead. This works, but it's an extra protocol field and leaks "these two names are equal ignoring case" (which is the point).

### Dedup and convergent encryption

- **Convergent encryption** derives the encryption key from the content itself (key = hash(content)), so identical plaintext gives identical ciphertext and the server can dedup across users. Its known weaknesses: **confirmation-of-a-file** (someone who has a file can check whether you store it) and **learn-the-remaining-information** (guess the unknown parts of a mostly known file, e.g. a form letter with a PIN, and confirm each guess). Both were laid out by Zooko Wilcox-O'Hearn and Brian Warner for Tahoe-LAFS in 2008. The fix is to mix a secret into the key, which also stops cross-user dedup.
- **For CloudFS this mostly doesn't matter:** there's one user, so "dedup within one secret key" is *all* the dedup there is. A keyed chunk ID (`HMAC(chunk_id_key, plaintext_chunk)`) plus random-nonce encryption keeps full dedup across the user's own files and devices, with no confirmation attack against the server. Restic does the same thing in spirit: blob IDs are the SHA-256 of the plaintext, stored only inside the encrypted repository, and encryption uses a fresh random IV.
- Two devices uploading the same chunk get the same ID but different ciphertext (random nonces). The server keeps whichever arrives first. That's fine, since both decrypt to the same plaintext.

---

## 4. What E2E breaks or costs in CloudFS

| Area | Impact |
|---|---|
| **Dedup** | Unaffected for a single user if chunk IDs are keyed (section 3). Cross-user dedup is gone, but there's no second user. |
| **Thumbnails / content workers** | Impossible on the server: it can't read content. Versioning, pruning and chunk GC still work, because they only use metadata and chunk IDs. |
| **Server-side three-way merge** | Impossible, but already deferred, and it would be client-side anyway (ADR-0005). |
| **Server name checks (ADR-0005)** | Needs a blind index for case-insensitive uniqueness. Cycle and live-parent checks are unaffected (IDs only). |
| **Key loss** | Forgotten passphrase + no recovery key = **all data unrecoverable**. That's data loss of a different kind from goal 4, so a recovery key and a clear onboarding warning are mandatory. |
| **Device revocation (spike 08)** | Revoking a device's *auth token* stops it syncing, but it still has the master key and anything it already downloaded. True revocation means rotating the master key; with a key hierarchy that re-wraps keys rather than re-encrypting all data, but only if the hierarchy uses per-file keys. |
| **Debuggability** | You can't inspect a chunk in the bucket to debug. Tooling (a `cloudfs debug decrypt` command) becomes necessary. |
| **Effort for a solo Go learner** | Moderate. The primitives are library calls. The risk is in the *design* details (nonce handling, binding chunk ciphertext to its ID via associated data, key wrapping, recovery flow) and in testing. Expect roughly 1–2 sprints on top of the base client, plus onboarding UX. *(Estimate, not measured.)* |

### Can E2E be added later?

Partly. What makes it expensive is what gets baked in now:

1. **Chunk IDs.** If v1 names chunks by plain `SHA-256(plaintext)`, switching to keyed IDs means recomputing every ID and re-uploading every chunk. For a single user with tens of GB that's a one-time, overnight re-upload. Annoying, not fatal.
2. **Object format.** If chunks are raw bytes with no header, there's no way to tell an encrypted chunk from a plain one. A versioned header avoids that.
3. **Plaintext names in Postgres** and server-side name checks. Migrating to encrypted names plus a blind index means a schema change and a re-commit of every node.
4. **Content-reading workers.** If thumbnails or other content processing get built, E2E later means removing them.

---

## 5. Middle-ground options

| Option | What it means |
|---|---|
| **A. Server-side only** | TLS everywhere. AWS: SSE-KMS with a customer-managed key and S3 Bucket Key, RDS encryption, least-privilege IAM. Self-hosted: disk encryption. No client crypto. |
| **B. E2E in v1** | Section 3's design from the start, on top of A (layers stack: S3 still applies SSE to the ciphertext). |
| **C. A now, "E2E-ready" format, E2E as a later epic** | Do A. Also, from day one: (1) every chunk object starts with a small **versioned header** (format byte, encryption scheme, key ID), so encrypted and plain chunks can coexist and be migrated gradually; (2) the chunk record stores an **ID scheme** field, so keyed IDs can be introduced without ambiguity; (3) keep the server's name checks behind one function, so swapping in a blind index is local; (4) no content-reading workers in v1. |
| **D. E2E opt-in per deployment** | C, then build E2E as a deployment setting (`encryption: e2e`). Self-hosters who don't trust their host turn it on; others keep server-side processing like thumbnails. Most flexible, and two code paths to test. |

---

## 6. Interactions with other spikes

- **Spike 04 (chunking):** choose the chunk ID scheme with this in mind: plain SHA-256, keyed HMAC, or BLAKE3 keyed mode. Also the header/envelope format. Encryption must apply to *chunks after chunking*. Encrypting before chunking would destroy content-defined chunk boundaries.
  - Restic notes that chunk *sizes* can leak information about the chunker's parameters, and restic 0.18 mitigated it. A minor concern for us.
- **Spike 07 (uploads):** presigned PUT/GET works with SSE-S3 and SSE-KMS. The bucket's default encryption applies without the client sending encryption headers, and KMS-encrypted object requests must be SigV4-signed over TLS. The **API role that signs the URLs needs `kms:GenerateDataKey` and `kms:Decrypt`**, since presigned requests run with the signer's permissions *(the "signer's permissions" part is prior knowledge)*. SSE-C with presigned URLs would require clients to send the key header on every request, and is now blocked by default anyway.
- **Spike 08 (device auth):** keep **authentication** (which device may talk to the server: tokens, revocable) separate from **encryption keys** (who can read the data: the passphrase and master key). Revoking a token ≠ revoking key access (section 4).
- **Spike 09 (cost):** a customer-managed KMS key costs **$1/month**. Symmetric requests cost **$0.03 per 10,000**, with **20,000 free requests/month** across regions. **S3 Bucket Keys cut KMS request traffic by up to 99%**, so request cost is negligible at our scale. `terraform destroy` has a wrinkle: KMS keys are scheduled for deletion with a 7–30 day waiting period rather than deleted immediately. *(Waiting period and pending-deletion billing are prior knowledge; check against the success criterion "destroy leaves no billable resources".)*
- **Phase 4 (WireGuard):** an extra in-transit layer and a way to keep the API off the public internet. It doesn't change at-rest choices.

### Out of scope, but important: MinIO's status

While checking MinIO's encryption support: **the MinIO community repository was archived on 25 April 2026 and says "THIS REPOSITORY IS NO LONGER MAINTAINED."** The community edition has been source-only (no pre-built binaries or official Docker images) since around October 2025, and the admin console was removed from it in 2025. MinIO now points users to "AIStor Free", under a separate free license.

This affects design goal 7 ("MinIO in Compose") and spike 07, not just encryption. Alternatives to evaluate:
- **Garage** (open-source S3-compatible server). It supports SSE-C since v1.0, and its own docs recommend that people writing their own client "implement data encryption directly on the client side".
- **SeaweedFS** and **Ceph RGW** *(from prior knowledge, not evaluated)*.
- Community forks of MinIO.
- AIStor Free (check license terms).

Flagging this for the parent discussion; it probably deserves its own spike or a section in spike 07.

---

## 7. Recommendation (for discussion, not a decision)

**Option C: server-side encryption now, with an E2E-ready format, and E2E as a later epic** (possibly growing into D).

- **AWS:** SSE-KMS with a customer-managed key, S3 Bucket Key enabled, RDS encryption on, and IAM split so only the API role holds `kms:Decrypt`/`GenerateDataKey`. All of it in Terraform. That's directly portfolio-relevant (KMS key policies, IAM least privilege, CloudTrail audit) and covers the most likely real incident: leaked credentials or a misconfiguration.
- **Self-hosted:** TLS plus a documented recommendation to use disk encryption.
- **Day-one format choices** so E2E stays cheap later: versioned chunk header, chunk ID scheme field, name checks behind one function, no content-reading workers in v1.
- **Reasoning:** for one user on AWS, the threats most likely to occur are covered by KMS + IAM. E2E's main extra protection (against the server and provider) is real but less likely, and it costs 1–2 sprints plus key-loss risk and onboarding UX at a point where the core sync engine isn't built yet. Option C keeps the door open at low cost.
- **The case for B instead:** if you don't want AWS or a compromised server to be able to read your files at all, or you see E2E as the headline feature of the open-source project, then do B. Single-user keyed dedup makes E2E cheaper here than in multi-user systems, and section 3 is close to a complete design.

---

## 8. Decisions for the user

1. **Scope:** A (server-side only), B (E2E in v1), C (server-side now, E2E-ready, E2E later), or D (E2E as a per-deployment option)?
2. **AWS at-rest:** SSE-KMS with a customer-managed key ($1/month, plus a KMS/IAM learning payoff), or the free SSE-S3 default?
3. **If E2E (now or later):** encrypt file names too (requires a blind index for ADR-0005's name checks), or content only?
4. **If E2E:** passphrase + printed recovery key as the onboarding model? (Device-to-device enrollment can come later.)
5. **Content-reading workers (thumbnails etc.):** rule them out for v1 to keep E2E possible?
6. **MinIO replacement:** acknowledge MinIO's archival and decide where to evaluate alternatives (separate spike vs part of spike 07).

---

## References

Fetched and read during this spike:
- AWS, S3 default encryption FAQ (SSE-S3 on all new objects since 5 Jan 2023; can't be disabled): https://docs.aws.amazon.com/AmazonS3/latest/userguide/default-encryption-faq.html
- AWS, SSE-KMS (permissions `kms:GenerateDataKey`/`kms:Decrypt`, default bucket encryption applies without client headers, SigV4 + TLS required, encryption context): https://docs.aws.amazon.com/AmazonS3/latest/userguide/UsingKMSEncryption.html
- AWS, SSE-C (disabled by default for new buckets since April 2026; key sent with every request): https://docs.aws.amazon.com/AmazonS3/latest/userguide/ServerSideEncryptionCustomerKeys.html
- AWS, S3 Bucket Keys (up to 99% fewer KMS requests): https://docs.aws.amazon.com/AmazonS3/latest/userguide/bucket-key.html
- AWS KMS pricing ($1/key/month, $0.03 per 10k symmetric requests, 20k free requests/month): https://aws.amazon.com/kms/pricing/
- MinIO GitHub repository (archived 25 Apr 2026, "no longer maintained", source-only, AIStor Free): https://github.com/minio/minio
- Blocks & Files on MinIO removing management features from the community edition (2025): https://blocksandfiles.com/2025/06/19/minio-removes-management-features-from-basic-community-edition-object-storage-code/
- Restic design (scrypt → master key, AES-256-CTR + Poly1305-AES, blob IDs = SHA-256 of plaintext, random IVs, chunker leak note): https://restic.readthedocs.io/en/latest/100_references.html
- Cryptomator security architecture (masterkey, scrypt KEK, AES key wrap): https://docs.cryptomator.org/security/architecture/
- Cryptomator vault cryptography (per-file content key in header, AES-GCM chunks, AES-SIV names with directory ID as associated data, flattened directories): https://docs.cryptomator.org/security/vault/
- Syncthing untrusted devices (beta; XChaCha20-Poly1305 + AES-SIV; scrypt from password + folder ID; sizes leak; no block reuse across files): https://docs.syncthing.net/users/untrusted.html
- `filippo.io/age` package docs (X25519/scrypt recipients; STREAM chunked payload; `DecryptReaderAt` random access): https://pkg.go.dev/filippo.io/age
- Convergent encryption attacks (confirmation-of-a-file, learn-the-remaining-information): Zooko Wilcox-O'Hearn, "Convergent encryption reconsidered" (2008), https://www.mail-archive.com/cryptography@metzdowd.com/msg08949.html; Tahoe-LAFS, https://tahoe-lafs.org/hacktahoelafs/drew_perttula.html; overview, https://en.wikipedia.org/wiki/Convergent_encryption *(read through search summaries, not full fetches)*
- Garage encryption cookbook (SSE-C since v1.0; recommends client-side encryption for custom clients): https://garagehq.deuxfleurs.fr/documentation/cookbook/encryption/ *(read through search summary)*

**Unverified (prior knowledge, not re-read):**
- RDS encryption must be enabled at instance creation.
- Presigned requests execute with the signer's IAM permissions, so the signer needs KMS permissions.
- The KMS key deletion waiting period (7–30 days), and whether keys pending deletion are billed.
- MinIO SSE requiring KES or an external KMS. Moot now that the community edition is unmaintained.
- Go library details: x/crypto chacha20poly1305 (XChaCha), argon2, hkdf; AES-SIV availability.
- The SeaweedFS and Ceph RGW alternatives.
- The 1–2 sprint E2E effort estimate.
