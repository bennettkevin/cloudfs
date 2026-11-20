# Spike 10: Local S3-compatible object storage (MinIO replacement)

- **Issue:** #13
- **Status:** Decided. See [ADR-0011](../adr/0011-local-object-storage.md): minimal S3 contract, Garage single-node as the pinned local default and the only store tested during initial development. SeaweedFS, versitygw and Silo are documented as **untested** alternatives. Optional `gofakes3` unit tests (section 7, point 3) were not adopted for now. MinIO image, LocalStack archive, Garage releases (incl. checksum changes), SeaweedFS and Silo status re-checked on 2026-11-20.
- **Question:** MinIO's community edition is no longer maintained. What S3-compatible store does CloudFS run locally (dev Compose stack and self-hosters), and what do automated tests run against, so the same Go code works locally and on AWS S3 (design goal 7)?

All repo and image facts below were checked on 2026-11-20 (GitHub API, Docker Hub API, project docs) unless marked *unverified*.

---

## 1. What CloudFS actually needs from the object store

Requirements are smaller than "full S3", because of decisions already made:

| Need | Why | Required? |
|---|---|---|
| PutObject / GetObject / HeadObject / DeleteObject | Store, fetch, check, and garbage-collect encrypted chunks | **Yes** |
| Presigned PUT/GET (SigV4 query auth) | Clients upload and download chunks directly (design §8) | **Yes** |
| ListObjectsV2 | Garbage collection and repair scans | **Yes** |
| Path-style addressing (`host/bucket/key`) | Simplest for local endpoints with no wildcard DNS | **Yes** (locally) |
| Works with aws-sdk-go-v2's default checksums | The SDK sends CRC32 checksums by default since Jan 2025 (section 6) | **Yes**, or config to turn them off |
| Multipart upload | Only if a single chunk is bigger than one PUT allows. Chunks are likely a few MB (spike 04), so probably not needed | Probably not |
| Conditional writes (`If-None-Match: *`) | Idempotent chunk upload ("only write if absent") | Nice to have. Postgres is the source of truth for which chunks exist ([ADR-0003](../adr/0003-sync-model.md)), so it's not required |
| Bucket event notifications | Trigger workers on upload | **Not required.** Per ADR-0003 the change log can be the event source |
| Bucket versioning | Keep old object versions | **Not required.** File versions are manifests in Postgres pointing to chunks |
| Server-side encryption | At-rest encryption | **Not required.** Content is end-to-end encrypted ([ADR-0007](../adr/0007-end-to-end-encryption.md)); use disk encryption on the host |
| CopyObject | Server-side copy | Not needed. Only the SSE-KMS upgrade path in [ADR-0008](../adr/0008-at-rest-encryption-aws.md) uses it, and that runs on AWS |

**Main takeaway:** if CloudFS sticks to this small subset, almost any maintained S3-compatible store works, and the choice is about simplicity, health and licence more than features.

---

## 2. MinIO's current state (confirmed)

- **Community repo:** `minio/minio` is **archived** (last push 2026-04-24). The README says "THIS REPOSITORY IS NO LONGER MAINTAINED" and points to AIStor Free and AIStor Enterprise.
- **Last community release:** `RELEASE.2025-10-15T17-29-55Z` (published 2025-10-16).
- **Docker image:** an anonymous pull of `minio/minio` from Docker Hub **fails** (the Docker Hub API returns "object not found", and the registry refuses an anonymous token; `dxflrs/garage` succeeds with the same check). Old compose files that reference `minio/minio` no longer work out of the box.
- **AIStor Free licence** ([agreement](https://www.min.io/legal/aistor-free-agreement), fetched). **Not usable for CloudFS:**
  - non-commercial use only; "deployment on one single node for development and test purposes only"; "You may not use the Software in a production environment of any kind"
  - up to 10 TiB on one node, no clustering
  - no redistribution, and you may not "connect the licensed software to any third party's software"
  - no modification or derivative works; no publishing benchmarks; MinIO may terminate for convenience
  - MinIO's launch blog says the free tier has "no limits, no sign-up required" (*search summary only*), which contradicts the agreement. The agreement is what binds.

  An open-source project can't make this its default, and self-hosters couldn't use it for their real files.
- **Maintained community fork:** **PGSTY Silo** (`pgsty/silo`, formerly `pgsty/minio`). AGPL-3.0, about 4.8k stars, last release `RELEASE.2026-11-12`, multi-arch images (`pgsty/silo`, amd64/arm64), and it keeps the web console. Pigsty runs it in production for PostgreSQL backups. It is a drop-in MinIO replacement (same protocol and data format), but it is **one company's fork** of a codebase whose original maintainers left: a bus-factor risk.
- `OpenMaxIO/openmaxio-object-browser` is only a fork of the console UI (last push 2025-06), not a server.

---

## 3. Candidates

| Store | Licence | Health (2026-11-20) | Docker image | Footprint | Notes |
|---|---|---|---|---|---|
| **Garage** | AGPL-3.0 | Active. v2.5.0 (2026-10-24); project since ~2020. Main repo on its own forge, with a GitHub mirror (~4.9k stars) | `dxflrs/garage`: amd64, arm64, **arm (32-bit)**, 386 | Small. Rust single binary built for small self-hosted clusters *(RAM figures unverified)* | `garage server --single-node` (since v2.3) creates a working one-node setup with no layout steps |
| **SeaweedFS** | Apache-2.0 | Very active. 4.54 (2026-11-16), ~36k stars | `chrislusf/seaweedfs`: amd64, arm64, arm, more | Moderate. A larger system (master, volume, filer, S3 gateway) wrapped by `weed mini` | README: `weed mini` "is fine for single-node production, such as an S3 gateway that issues presigned URLs" |
| **Versity Gateway (versitygw)** | Apache-2.0 | Active. v1.11.0 (2026-11-06), ~3.2k stars | `versity/versitygw`: amd64, arm64 | Light. Go gateway over a POSIX directory *(unverified)* | Objects stored as plain files in a directory. One command to run |
| **RustFS** | Apache-2.0 | Very active. 1.0.6 (2026-11-15), ~36k stars. **1.0.0 only shipped 2026-09-16** | `rustfs/rustfs`: amd64, arm64 | Light *(unverified)* | Positions itself as a MinIO replacement. Young as a stable product |
| **PGSTY Silo** | AGPL-3.0 | Active fork (see section 2) | `pgsty/silo`: amd64, arm64 | Same as MinIO | Drop-in for existing MinIO setups |
| **Zenko CloudServer** | Apache-2.0 | Repo active (9.3.32, 2026-11-20) | Docker Hub `zenko/cloudserver` tags last updated **2023**, amd64 only. Newer images may be published elsewhere *(unverified)* | Node.js | Scality's S3 front end. Mostly used for dev/test |
| **LocalStack** | Repo archived 2026-03-23 | Moved to a single image that **needs an account and auth token** since March 2026. The free plan is for non-commercial, individual use; CI on the free plan is unclear | `localstack/localstack`: amd64, arm64 | Heavy (emulates all of AWS) | **Ruled out** as a default for an open-source project: contributors and self-hosters would need a LocalStack account |
| **s3proxy** | Apache-2.0 | Active. 4.2.0 (2026-11-18) | `andrewgaul/s3proxy`: amd64, arm64 | Java (JVM) | S3 API in front of other backends (filesystem, other clouds). Better as a test tool than a self-host store |
| **Ceph RGW** | LGPL (mixed) | Active | Yes | **Too heavy** for a laptop or home server; needs a Ceph cluster | Ruled out for this use |
| **AIStor Free** | Proprietary | — | — | — | **Ruled out** by licence (section 2) |

### S3 features that matter (subset from section 1)

| | Presigned URLs | Multipart | ListObjectsV2 | Path-style | Conditional writes (`If-None-Match`) | SDK default checksums | Bucket notifications | Versioning |
|---|---|---|---|---|---|---|---|---|
| **AWS S3** | ✅ | ✅ | ✅ | ✅ | ✅ (docs: [conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)) | ✅ | ✅ | ✅ |
| **Garage** | ✅ | ✅ | ✅ | ✅ (vhost too) | ❌ **by design**: "structurally impossible… due to the lack of a consensus algorithm" (known-issues doc) | ✅ CRC64NVME and `x-amz-checksum-type` since v2.0; "don't panic on missing checksum" fixed in v2.3; composite-checksum fix in v2.5 | ❌ Missing | ❌ Missing |
| **SeaweedFS** | ✅ | ✅ | ✅ | ✅ | ✅ (README: "conditional reads and writes") | ✅ (README: "checksums") | *Unverified* for S3-style bucket notifications (it has filer-level notifications) | ✅ |
| **versitygw** | ✅ *(README; not tested)* | ✅ *(unverified)* | ✅ *(unverified)* | ✅ *(unverified)* | ✅ likely: precondition handling in `object-put.go` *(code search, not tested)* | *Unverified* | *Unverified* | ✅ (`--versioning-dir`) |
| **RustFS** | ✅ *(unverified)* | ✅ *(unverified)* | ✅ *(unverified)* | ✅ *(unverified)* | *Unverified* | *Unverified* | ✅ (README: "Event Notifications", webhook) | *Unverified* |
| **Silo** | ✅ | ✅ | ✅ | ✅ | ✅ (inherits MinIO) *(unverified for the fork)* | Inherits MinIO's support *(unverified)* | ✅ | ✅ |

---

## 4. How faithfully each one behaves like real S3

Local tests only mean something if the local store behaves like S3 where CloudFS depends on it.

- **Garage:** honest about its gaps (a published compatibility matrix and known-issues page). The gaps (conditional writes, notifications, versioning) are all features section 1 marks as not required. Garage has an `O(n²)` metadata cost for objects with thousands of blocks, and slows down past about 10 million objects per bucket (known-issues doc). Neither matters for CloudFS: chunks are small objects, and a personal library is tens of thousands of objects, not millions. **Risk:** if CloudFS ever relies on `If-None-Match`, it would work on AWS and silently not be enforced on Garage. Avoid relying on it, or treat it only as an optimisation.
- **SeaweedFS:** widest API coverage of the open options, and runs an S3 compatibility suite in CI (README). The cost is a bigger, more complex system for a single-node user.
- **versitygw:** translates S3 calls onto a normal filesystem. Simple and transparent, but object-store semantics on a POSIX filesystem have edge cases (atomic overwrite, listing order) *(unverified)*.
- **Silo / MinIO lineage:** historically the most S3-faithful of the self-hosted options. That fidelity is the main argument for it.
- **RustFS:** claims broad compatibility; too new as a stable release to judge.
- **No local store is a perfect stand-in for AWS.** Section 5 suggests also testing against real S3 once phase 2 exists.

---

## 5. Two roles, possibly two tools

### (a) The local "real" store: dev Compose stack and self-hosters

What matters: single-node simplicity, durability on one disk, easy upgrades, ARM images (Apple Silicon, Raspberry Pi), and a licence that a self-hoster and an open-source project can live with.

- **Single node, one disk:** every option above stores one copy of your chunks on that disk. Durability comes from your other devices (each has the full folder) plus host backups, not from the store. Worth saying plainly in the self-hosting docs.
- **ARM:** Garage ships 32-bit arm images as well (older Raspberry Pis). Garage, SeaweedFS, versitygw, RustFS and Silo all ship arm64.
- **Upgrades:** Garage has had breaking config changes across majors (e.g. v2.0 replaced `replication_mode` with `replication_factor` + `consistency_mode`). Pin image versions in Compose and read the notes before major upgrades. This applies to any choice.
- **Licence:** AGPL (Garage, Silo) applies to the store itself. Running it unmodified as a separate container next to MIT-licensed CloudFS does not, under the common reading, make CloudFS AGPL *(interpretation, not legal advice)*. Apache-2.0 options (SeaweedFS, versitygw, RustFS) avoid the question entirely.

### (b) The test double: automated tests and CI

- **Same store as Compose, started per test run** with `testcontainers-go`. That library has ready-made **Garage**, MinIO and LocalStack modules (checked in its repo). Tests then run against exactly what developers run.
- **In-process fake** for fast unit tests: `gofakes3` (MIT, Go, active, last push 2026-11-12). No Docker needed, but it's a simplified fake, so not a fidelity check.
- **Real AWS S3** in phase 2: a small nightly contract-test run against a real bucket catches differences no local store will. It costs cents at this volume (exact cost: spike 09).
- **Ruled out for tests:** LocalStack (needs an account) and the old `minio/minio` image (not pullable).

---

## 6. Go SDK (aws-sdk-go-v2) against non-AWS endpoints

- **Default checksums changed in January 2025.** From the aws-sdk-go-v2 S3 changelog, v1.73.0 (2025-01-15): "S3 client behavior is updated to always calculate a checksum by default for operations that support it (such as PutObject or UploadPart)… The checksum algorithm used by default now becomes CRC32." Response validation became on-by-default in v1.74.1 (2025-01-24). Many S3-compatible stores broke on this at the time.
  - **Switch:** `RequestChecksumCalculation` / `ResponseChecksumValidation` set to `WhenRequired` in code, or `AWS_REQUEST_CHECKSUM_CALCULATION` / `AWS_RESPONSE_CHECKSUM_VALIDATION` env vars, or `request_checksum_calculation` in shared config (AWS SDK [data integrity docs](https://docs.aws.amazon.com/sdkref/latest/guide/feature-dataintegrity.html)).
  - **Suggested default:** make it a CloudFS config option. Use the SDK default (`WhenSupported`) on AWS, and allow `WhenRequired` for local stores that reject the headers. Current Garage (v2.x) supports the new checksum headers per its release notes, so it may not need the switch. Test it.
- **Path-style:** set `UsePathStyle: true` for local endpoints. Virtual-host style needs wildcard DNS (`bucket.host`), which local setups usually lack.
- **Custom endpoint:** use `BaseEndpoint` on the S3 client options, plus a region string the store accepts (Garage's is configurable; many stores accept any).
- **Presigned URLs and checksums:** the Go SDK has had presign bugs around checksum headers (issues #2387 and #2610, both closed). Whether a presigned PUT from the current SDK includes checksum headers the uploader must then send is a **spike 07** question. Test it against both AWS and the chosen local store.

---

## 7. Recommendation (for discussion, not a decision)

1. **Write CloudFS against a minimal S3 contract** (section 1): Put, Get, Head, Delete, ListObjectsV2, presigned PUT/GET, path-style, configurable checksum mode. **No reliance on** conditional writes, bucket notifications, bucket versioning or server-side encryption. This already follows from ADR-0003 (Postgres is the source of truth and the change log is the event source) and ADR-0007 (E2E). It keeps self-hosters free to point CloudFS at any S3-compatible store.
2. **Default local store: Garage**, run with `--single-node` in the Compose stack.
   - Built for small self-hosted deployments, a small Rust binary, mature (around six years old, at v2.5), honest compatibility docs, images including 32-bit ARM.
   - Its gaps are exactly the features point 1 says not to depend on.
   - Has a ready-made `testcontainers-go` module, so tests and Compose use the same store.
3. **Tests:** Garage via `testcontainers-go` for integration tests, optional `gofakes3` for fast unit tests, and (phase 2) a small nightly suite against real AWS S3.
4. **Document alternatives** for self-hosters: SeaweedFS (Apache-2.0, most complete API), versitygw (files on a plain disk), and Silo (for anyone migrating an existing MinIO setup).

**Runner-up: SeaweedFS.** Choose it instead if you want conditional writes available locally (e.g. to rely on `If-None-Match` for idempotent uploads), or prefer an Apache-2.0 licence for the default. The cost is a bigger, more complex system for a single-node user.

**Not recommended as the default:** RustFS (stable for only two months; revisit later), Silo (single-company fork), LocalStack (account required), AIStor Free (licence), Ceph (too heavy), CloudServer (Docker Hub image stale).

---

## 8. Decisions for the user

1. **Minimal S3 contract:** OK to rule out depending on conditional writes, bucket notifications, bucket versioning and server-side encryption? This is what keeps the store swappable.
2. **Default local store:** Garage (recommended), SeaweedFS, or another?
3. **Licence preference for the default:** is AGPL for a separately run store fine, or prefer Apache-2.0 (which points to SeaweedFS)?
4. **Testing approach:** same store via `testcontainers-go`, plus a nightly real-S3 suite in phase 2?
5. **Self-hosting docs:** list supported alternatives (SeaweedFS, versitygw, Silo) as "should work, not tested in CI", or test more than one in CI?

---

## References

Fetched or queried directly:
- MinIO repo (archived, README notice, last release): https://github.com/minio/minio
- Docker Hub `minio/minio` (API "object not found", anonymous pull refused; checked 2026-11-20): https://hub.docker.com/r/minio/minio
- AIStor Free agreement: https://www.min.io/legal/aistor-free-agreement
- PGSTY Silo (MinIO fork): https://github.com/pgsty/silo
- Garage S3 compatibility matrix: https://garagehq.deuxfleurs.fr/documentation/reference-manual/s3-compatibility/
- Garage known issues (no conditional writes; large-object and metadata-scale limits): https://github.com/deuxfleurs-org/garage/blob/main/doc/book/reference-manual/known-issues.md
- Garage releases (v2.0 checksums, v2.3 `--single-node`, v2.5): https://git.deuxfleurs.fr/Deuxfleurs/garage/releases
- SeaweedFS README (`weed mini`, S3 feature list): https://github.com/seaweedfs/seaweedfs
- Versity Gateway README and source: https://github.com/versity/versitygw
- RustFS README and releases (1.0.0 on 2026-09-16): https://github.com/rustfs/rustfs
- Zenko CloudServer: https://github.com/scality/cloudserver; Docker Hub tags: https://hub.docker.com/r/zenko/cloudserver
- LocalStack repo archive notice: https://github.com/localstack/localstack; single-image change: https://blog.localstack.cloud/localstack-single-image-next-steps/
- s3proxy: https://github.com/gaul/s3proxy
- AWS S3 conditional writes: https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html
- AWS SDK data integrity (checksum settings): https://docs.aws.amazon.com/sdkref/latest/guide/feature-dataintegrity.html
- aws-sdk-go-v2 S3 changelog (v1.73.0, v1.74.1): https://github.com/aws/aws-sdk-go-v2/blob/main/service/s3/CHANGELOG.md
- testcontainers-go modules (Garage, MinIO, LocalStack): https://github.com/testcontainers/testcontainers-go/tree/main/modules
- gofakes3: https://github.com/johannesboyne/gofakes3

Search summaries only (not fetched in full):
- MinIO blog on AIStor tiers ("no limits, no sign-up"): https://blog.min.io/introducing-new-subscription-tiers-for-minio-aistor-free-enterprise-lite-and-enterprise
- LocalStack pricing/licensing pages: https://localstack.cloud/pricing, https://docs.localstack.cloud/aws/licensing/
