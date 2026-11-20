# CloudFS Design Doc

- **Version:** 0.5 (draft, Sprint 0)
- **Last reviewed:** 2026-11-19

This is a living document. It's updated at each sprint review to match what's actually decided and built. Individual decisions and their reasoning live in [ADRs](adr/), and research findings in [research/](research/).

---

## 1. Vision

CloudFS keeps a folder in sync across a person's devices, using a backend they own and control. It runs on a laptop or home server with Docker Compose, or in AWS from a single `terraform apply`, with the same code in both places.

Secondary goal: CloudFS is a learning and portfolio project for cloud architecture. Design choices favour patterns that are standard in production cloud systems (infrastructure as code, managed services, event-driven processing, least-privilege IAM, observability), even where a simpler hack would work for one user.

## 2. Users

- **Primary:** the developer, syncing between 2–4 personal devices (macOS and Linux first).
- **Secondary:** technically capable self-hosters who want an open-source sync backend. They run their own instance and own its operation, data, and legal obligations.

## 3. Goals (v1)

1. Two-way sync of one designated folder across multiple devices.
2. When devices are online, changes propagate within seconds.
3. Offline edits reconcile when the device reconnects.
4. **No silent data loss.** Conflicting edits keep both versions.
5. Previous file versions are retained for a configurable period.
6. A small change to a large file uploads only the changed part.
7. The same codebase runs locally (an S3-compatible object store and Postgres in Compose) and in AWS (S3, RDS, ECS), switched by configuration. The local object store is pending spike 10, since MinIO's community edition is no longer maintained.
8. AWS infrastructure is fully reproducible from Terraform and fully removable with `terraform destroy`.
9. File content is end-to-end encrypted: the server never holds plaintext content or keys ([ADR-0007](adr/0007-end-to-end-encryption.md)).

## 4. Non-goals (v1)

- Multiple user accounts, sharing, or permissions between users ([ADR-0002](adr/0002-single-user-first.md)).
- Running CloudFS as a hosted service for other people.
- Mobile clients.
- Real-time collaborative editing of the same file.
- Files-on-demand placeholders (OneDrive-style "online-only" files).
- A web file browser UI. Possible later epic.

## 5. Success criteria

Phase-level checks that define "done". Numbers marked TBD get set once spikes give us a baseline.

- Sync a mixed folder (≥1 GB, ≥10k files) between two devices with no differences afterwards.
- Edit 1 MB inside a 1 GB file: upload size is a small fraction of the file (target TBD after the chunking spike).
- Edit the same file on two offline devices, then reconnect both: both versions survive, and the conflict is visible to the user.
- Fresh AWS account → working deployment via Terraform and CI, with no manual console steps.
- `terraform destroy` leaves no billable resources.
- Documented idle and active monthly AWS cost (budget TBD, spike 09).

## 6. Constraints

- Solo, part-time developer working in 2-week sprints. Size work for school weeks, not for best-case weeks.
- Go is new to the developer ([ADR-0001](adr/0001-use-go.md)). Early sprints include learning time.
- Minimal AWS spend: use the free tier where possible, set a budget alarm before first deploy, and tear down when idle.
- Open source under the MIT License.

## 7. Open questions

Each one maps to a research spike ([index](research/README.md)).

- ~~Sync model: server change log + cursor vs version vectors (spike 01)~~ Decided: [ADR-0003](adr/0003-sync-model.md), [ADR-0004](adr/0004-stable-file-ids.md)
- ~~Conflict resolution behaviour (spike 02)~~ Decided: [ADR-0005](adr/0005-conflict-resolution.md), [ADR-0006](adr/0006-separate-content-and-location-revisions.md)
- ~~End-to-end encryption, yes or no (spike 03)~~ Decided: [ADR-0007](adr/0007-end-to-end-encryption.md) (E2E in v1), [ADR-0008](adr/0008-at-rest-encryption-aws.md) (AWS at-rest defaults)
- File/folder name encryption and blind index for unique names (spike 11)
- Key hierarchy, passphrase/recovery key, new-device onboarding (designed in phase 1 encryption work)
- Local S3-compatible storage to replace MinIO (spike 10)
- ~~Chunking strategy, chunk size, keyed chunk ID scheme (spike 04)~~ Decided: [ADR-0009](adr/0009-chunking-and-chunk-ids.md), [ADR-0010](adr/0010-chunk-storage-and-manifests.md)
- Delete/rename semantics and retention (spike 05)
- Windows support timing (spike 06 informs)
- Device authentication and revocation (spike 08)
- AWS monthly budget target (spike 09)

---

## 8. Architecture

*Pending Sprint 0 spikes.* Starting sketch:

```
client daemon (watch → chunk → hash)
   │ 1. "I have these chunk hashes"   ──► API (auth, metadata) ──► Postgres
   │ 2. presigned URLs for missing chunks
   │ 3. upload chunks directly        ──► object storage (S3 / MinIO)
                                              │ event
                                              ▼
                                           queue ──► workers (versioning, pruning)
API ──► change notifications ──► other clients pull changes
```

## 9. Data model

Partially decided. Files are identified by stable ID ([ADR-0004](adr/0004-stable-file-ids.md)); changes are tracked in a server log ([ADR-0003](adr/0003-sync-model.md)); content and location are versioned separately ([ADR-0006](adr/0006-separate-content-and-location-revisions.md)); content is stored as keyed chunks with encrypted manifests ([ADR-0009](adr/0009-chunking-and-chunk-ids.md), [ADR-0010](adr/0010-chunk-storage-and-manifests.md)). Delete retention pending spike 05.

**Server (Postgres)**

| Table | Key columns | Notes |
|---|---|---|
| `nodes` | `id` (stable, server-assigned), `parent_id`, `name`, `kind` (file/folder), `content_rev`, `location_rev`, `deleted` | Path is derived from `parent_id` + `name`. Live names unique per folder, **case-insensitive**. No folder cycles. Current content is the `revisions` row matching `content_rev`. |
| `revisions` | `node_id`, `content_rev`, `manifest` (encrypted blob), `created_at` | One row per retained content revision (version history, goal 5). The manifest (ordered chunk IDs and plaintext lengths) is readable only by clients. |
| `revision_refs` | `node_id`, `content_rev`, `chunk_id` | Unordered set of chunks a revision uses. Server-visible, for refcounting and GC. |
| `chunks` | `chunk_id` (keyed HMAC-SHA256), `id_scheme`, `object_key`, `offset`, `length`, `created_at`, `unreferenced_since` | Location index: in v1 each chunk is its own padded object at offset 0, ready for pack objects later. GC deletes chunks unreferenced longer than the grace period (24 h, configurable). |
| `changes` | `seq` (monotonic), `node_id`, `content_rev`, `location_rev`, `op` (create/modify/move/delete), `created_at` | Append-only. Writes serialized so `seq` becomes visible in order. Pruned after the retention window. |
| `log_meta` | `epoch` (random ID) | Regenerated when the database is restored from backup. |

**Client (local store, per device)**

- Server `epoch` and `cursor` (last applied `seq`).
- Per node: `id`, local path, synced `content_rev` and `location_rev`, content hash at last sync, size + mtime at last sync. This is the synced baseline used to classify local vs remote changes. The content hash is a keyed hash over the file's ordered chunk IDs.

**Object store**

- One object per chunk: versioned header (format version, scheme, key ID), padded ciphertext, AEAD tag. The chunk ID is bound into the AEAD associated data.

## 10. Sync protocol

Partially decided ([ADR-0003](adr/0003-sync-model.md), [ADR-0005](adr/0005-conflict-resolution.md), [ADR-0006](adr/0006-separate-content-and-location-revisions.md), [ADR-0010](adr/0010-chunk-storage-and-manifests.md)). Exact upload API and SDK behaviour pending spike 07.

**Uploading content** (outline)
1. Client chunks the file with keyed FastCDC (256 KiB / 1 MiB / 4 MiB) and computes keyed chunk IDs ([ADR-0009](adr/0009-chunking-and-chunk-ids.md)).
2. Client asks the server which chunk IDs it doesn't already have (one batch request).
3. For each missing chunk: encrypt, pad, compute the ciphertext SHA-256, request a presigned PUT with that checksum signed in, upload directly to the object store.
4. Client commits the new content revision: base `content_rev`, encrypted manifest, and the unordered chunk reference set.

**Pulling remote changes**
1. Device sends `epoch` + `cursor`; server returns changes with `seq > cursor` and a new cursor.
2. Device applies them against its baseline and saves the new cursor.
3. A long-poll/WebSocket signal tells online devices to pull when new changes land.

**Committing a local change**
1. Device sends the node ID, the change, and the **base revision** for the group it changes: `content_rev` for edits and deletes, `location_rev` for renames and moves.
2. In one transaction the server checks that base revision and the structural invariants (unique case-insensitive name, no cycle, live parent). On success it bumps the revision and appends to `changes`.
3. On a stale base or an invariant failure, the server rejects with a conflict response, and the device resolves it with ordinary commits.

**Resolving a rejection** ([ADR-0005](adr/0005-conflict-resolution.md)). Rule: if bytes could be lost, keep both; if only names or locations differ, a fixed rule decides.
- **Edit / edit:** the server's version keeps the name; the rejected device commits its version as a new node named `<name> (conflicted copy, <device>, <YYYY-MM-DD>).<ext>`. Identical content is not a conflict.
- **Edit / delete:** edit wins, and the file is restored.
- **Rename / rename, move / move:** first committed wins; the device matches it and logs the event.
- **Name collision** (including case-only and file-vs-folder): the later node gets a conflicted-copy name.
- **Folder deleted while a file inside it changed:** restore only the folders needed to hold that file.
- **Folder cycle:** the move is rejected and reverted locally.
- Conflicted copies sync to every device. Resolution is idempotent across retries.
- The user is notified through the filename, a warning log line, and `cloudfs conflicts`.

**Full reconcile** (compare local files, baseline, and a full server listing) happens when:
- the cursor is older than the log retention window (**cursor too old**), or
- the server's `epoch` differs from the device's (**server restored**). Files deleted between backup and restore may reappear, an accepted trade-off.

## 11. Security

Baseline commitments: TLS everywhere, least-privilege IAM per service, no long-lived cloud credentials in CI (OIDC), secrets only in a secret store or gitignored `.env`.

**Encryption layers**

| Layer | Choice | Protects against |
|---|---|---|
| In transit | TLS (phase 4 adds WireGuard) | Network eavesdropping and tampering |
| End-to-end | File content encrypted on the client before upload ([ADR-0007](adr/0007-end-to-end-encryption.md)) | Compromised server, cloud provider, leaked bucket or database |
| At rest, AWS | SSE-S3 default; RDS encrypted at creation with the AWS-managed key ([ADR-0008](adr/0008-at-rest-encryption-aws.md)) | Stolen disks; defence in depth |
| At rest, self-hosted | Disk encryption recommended in the self-hosting docs | Stolen or disposed machine |

**End-to-end constraints**
- No server-side workers read file content.
- Chunk IDs are a keyed hash, so the bucket can't be used to confirm a known file is stored (HMAC-SHA256, [ADR-0009](adr/0009-chunking-and-chunk-ids.md)). Exact chunk sizes are hidden by padding; pack objects later close the remaining size leak ([ADR-0010](adr/0010-chunk-storage-and-manifests.md)).
- Chunks are encrypted after chunking and carry a versioned header (format version, scheme, key ID).
- Losing the key loses the data, so onboarding must include a recovery key.
- Device authentication (spike 08) is separate from encryption keys. Revoking a login doesn't revoke keys a device already has.

*Pending:* name encryption (spike 11), key hierarchy and onboarding (phase 1), device auth (spike 08).

## 12. Infrastructure

*Pending spike 09.* Phase 1 local (Compose), phase 2 AWS (Terraform).

Operational settings decided so far:
- **Change log retention:** 90 days, configurable ([ADR-0003](adr/0003-sync-model.md)).
- **Database restore:** regenerate `log_meta.epoch` as part of the restore runbook, and log the event clearly.
- **At-rest encryption (AWS):** S3 default SSE-S3; RDS encryption enabled at creation (can't be added later). Upgrade path to a customer-managed KMS key is in [ADR-0008](adr/0008-at-rest-encryption-aws.md).

## 13. Observability

*Defined in phase 3.*

## 14. Roadmap

| Phase | Epic | Outcome |
|---|---|---|
| 0 | Discovery | Design doc v1.0, ADRs for one-way-door decisions, Sprint 1 backlog |
| 1 | Local MVP | Two-way, end-to-end encrypted sync across devices against the Compose stack |
| 2 | AWS + Terraform | Same system deployed to AWS via IaC and CI with OIDC |
| 3 | Operate | Metrics, autoscaling, load test, failure write-up, cost report |
| 4 | Private network (stretch) | API reachable only through a self-run WireGuard gateway |

## Changelog

- **0.5** (2026-11-20): Spike 04 decided. Keyed FastCDC + HMAC-SHA256 chunk IDs (ADR-0009); one padded object per chunk with a location index and encrypted manifests (ADR-0010); §9 gains `revisions`, `revision_refs`, `chunks` and the object format; §10 gains the upload outline.
- **0.4** (2026-11-19): Spike 03 decided. End-to-end encryption in v1 (ADR-0007) and free at-rest defaults on AWS with a KMS upgrade path (ADR-0008); §11 Security filled in; goal 9 added; goal 7 notes the MinIO replacement (spike 10); new open questions for spikes 10 and 11.
- **0.3** (2026-11-19): Spike 02 decided. Conflict resolution (ADR-0005) and separate content/location revisions (ADR-0006, amends ADR-0003); §9 and §10 updated.
- **0.2** (2026-11-19): Spike 01 decided. Sync model (ADR-0003) and stable file IDs (ADR-0004); first cut of §9 Data model and §10 Sync protocol; log retention and restore settings in §12.
- **0.1** (2026-11-19): Vision, scope, goals, constraints, and open questions. Architecture sections stubbed.
