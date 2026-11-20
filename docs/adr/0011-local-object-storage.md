# ADR-0011: Minimal S3 contract; Garage as the local object store

- **Status:** Accepted
- **Date:** 2026-11-20
- **Research:** [Spike 10](../research/10-local-object-storage.md) (#13)

## Context

Design goal 7 requires the same code to run against a local S3-compatible store and AWS S3. The original plan named MinIO, but its community repository was archived on 2026-04-24, the `minio/minio` Docker image no longer pulls anonymously, and AIStor Free's license forbids production use and connecting it to third-party software. LocalStack's repository is also archived and now requires an account.

Earlier decisions shrink what CloudFS needs from the store: Postgres is the source of truth and the change log is the event source ([ADR-0003](0003-sync-model.md)), and content is end-to-end encrypted ([ADR-0007](0007-end-to-end-encryption.md)).

## Decision

**1. CloudFS depends only on a minimal S3 contract:**
- PutObject, GetObject, HeadObject, DeleteObject, ListObjectsV2
- Presigned PUT and GET (SigV4 query authentication), with `x-amz-checksum-sha256` signed into presigned PUTs ([ADR-0010](0010-chunk-storage-and-manifests.md))
- Path-style addressing
- Range GET (needed once pack objects arrive, #16)

CloudFS does **not** rely on conditional writes, bucket event notifications, bucket versioning or server-side encryption. Store access sits behind a small Go `ObjectStore` interface wrapping `aws-sdk-go-v2`; the same SDK is used for every store.

**2. Store selection is configuration only:** endpoint URL, region, `UsePathStyle` (on for local stores), and checksum mode. The SDK's checksum settings (`RequestChecksumCalculation` / `ResponseChecksumValidation`) default to `WhenRequired`. This avoids depending on each store's handling of the SDK's default CRC32 checksums (on by default since aws-sdk-go-v2 S3 v1.73.0, January 2025), and AWS S3 accepts it too.

**3. Default local store: Garage, single-node** (`--single-node`, v2.3+), with the version **pinned** in the Compose file. Upgrades are deliberate changes that run the test suite.

**4. Testing:**
- **Unit tests** use [`gofakes3`](https://github.com/johannesboyne/gofakes3) (MIT, actively maintained), an in-process fake S3 server. Tests exercise the real `ObjectStore` code and `aws-sdk-go-v2` calls without Docker, so they're fast and run anywhere.
- **Integration tests** run against the same pinned Garage, started with `testcontainers-go`.
- A shared object-store **contract test suite** covers the operations in point 1 and runs against Garage.
- Phase 2 adds a scheduled run of the same contract suite against real AWS S3.
- During initial development, **Garage is the only real store tested.** gofakes3 is a test double, not a supported store. Behaviour that depends on store fidelity (SigV4 signature checks, presigned URLs, signed checksum enforcement) is asserted in the Garage integration and contract tests, not in unit tests against the fake.

**Untested alternatives** (documented for self-hosters; expected to work if they implement the contract above, but not tested by CloudFS):
- **SeaweedFS** — Apache-2.0, very active, widest S3 coverage; a larger system to run.
- **Versity Gateway (versitygw)** — Apache-2.0, stores objects as plain files on a filesystem.
- **PGSTY Silo** — AGPL-3.0, maintained MinIO fork; relevant for anyone migrating from MinIO; maintained by a single company.

Any of these can be promoted to "tested" later by adding it to the contract test run.

## Alternatives considered

- **SeaweedFS as the default** — runner-up. Has conditional writes and an Apache-2.0 license, but is heavier for a laptop or single-node deployment, and CloudFS doesn't need conditional writes.
- **MinIO community edition / AIStor Free** — unmaintained / license prohibits the use case.
- **LocalStack** — archived, requires an account.
- **RustFS** — 1.0 shipped 2026-09-16; too new to depend on. Revisit later.
- **Zenko CloudServer** — Docker Hub image last updated in 2023.
- **Ceph RGW** — far too heavy for a single-user deployment.

## Consequences

- Garage lacks conditional writes, bucket notifications and versioning **by design**, so the contract rule above is enforced in practice: code that needs them fails local tests.
- Garage is AGPL-3.0. Running it unmodified as a separate container doesn't change CloudFS's MIT license (standard interpretation, not legal advice).
- Garage supports the SDK's newer checksum headers (checksum support since v1.0; CRC64NVME and `x-amz-checksum-type` in v2.0; fixes in v2.3 and v2.5). Remaining checksum edge cases are covered by `WhenRequired`, the version pin and the contract tests.
- Three test layers (gofakes3 unit, Garage integration/contract, AWS S3 scheduled) let most tests run quickly without Docker while store-specific behaviour is still checked against a real store.
- Spike 07 must verify presigned PUT behaviour with a signed `x-amz-checksum-sha256` against both Garage and AWS S3.
