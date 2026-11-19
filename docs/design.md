# CloudFS Design Doc

- **Version:** 0.2 (draft, Sprint 0)
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
7. The same codebase runs locally (MinIO, Postgres in Compose) and in AWS (S3, RDS, ECS), switched by configuration.
8. AWS infrastructure is fully reproducible from Terraform and fully removable with `terraform destroy`.

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
- Conflict resolution behaviour (spike 02)
- End-to-end encryption, yes or no (spike 03). This is decided early because it constrains chunking and server-side processing.
- Chunking strategy and chunk size (spike 04)
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

Partially decided. Files are identified by stable ID ([ADR-0004](adr/0004-stable-file-ids.md)); changes are tracked in a server log ([ADR-0003](adr/0003-sync-model.md)). Content representation is pending spike 04, delete retention pending spike 05.

**Server (Postgres)**

| Table | Key columns | Notes |
|---|---|---|
| `nodes` | `id` (stable, server-assigned), `parent_id`, `name`, `kind` (file/folder), `revision`, `deleted` | Path is derived from `parent_id` + `name`. Unique live `(parent_id, name)`. Content reference (chunk list) pending spike 04. |
| `changes` | `seq` (monotonic), `node_id`, `revision`, `op` (create/modify/move/delete), `created_at` | Append-only. Writes serialized so `seq` becomes visible in order. Pruned after the retention window. |
| `log_meta` | `epoch` (random ID) | Regenerated when the database is restored from backup. |

**Client (local store, per device)**

- Server `epoch` and `cursor` (last applied `seq`).
- Per node: `id`, local path, synced `revision`, content hash at last sync. This is the synced baseline used to classify local vs remote changes.

## 10. Sync protocol

Partially decided ([ADR-0003](adr/0003-sync-model.md)). Upload mechanics pending spike 07, conflict resolution pending spike 02.

**Pulling remote changes**
1. Device sends `epoch` + `cursor`; server returns changes with `seq > cursor` and a new cursor.
2. Device applies them against its baseline and saves the new cursor.
3. A long-poll/WebSocket signal tells online devices to pull when new changes land.

**Committing a local change**
1. Device sends the node ID, the change, and the **base revision** it edited from.
2. In one transaction the server checks the base revision against the current one. On a match it bumps the revision and appends to `changes`.
3. On a mismatch the server rejects with a conflict response. Client behaviour after that is spike 02; overwriting is not an option (goal 4).

**Full reconcile** (compare local files, baseline, and a full server listing) happens when:
- the cursor is older than the log retention window (**cursor too old**), or
- the server's `epoch` differs from the device's (**server restored**). Files deleted between backup and restore may reappear, an accepted trade-off.

## 11. Security

*Pending spikes 03, 08.* Baseline commitments: TLS everywhere, least-privilege IAM per service, no long-lived cloud credentials in CI (OIDC), secrets only in a secret store or gitignored `.env`.

## 12. Infrastructure

*Pending spike 09.* Phase 1 local (Compose), phase 2 AWS (Terraform).

Operational settings decided so far:
- **Change log retention:** 90 days, configurable ([ADR-0003](adr/0003-sync-model.md)).
- **Database restore:** regenerate `log_meta.epoch` as part of the restore runbook, and log the event clearly.

## 13. Observability

*Defined in phase 3.*

## 14. Roadmap

| Phase | Epic | Outcome |
|---|---|---|
| 0 | Discovery | Design doc v1.0, ADRs for one-way-door decisions, Sprint 1 backlog |
| 1 | Local MVP | Two-way sync across devices against Compose stack |
| 2 | AWS + Terraform | Same system deployed to AWS via IaC and CI with OIDC |
| 3 | Operate | Metrics, autoscaling, load test, failure write-up, cost report |
| 4 | Private network (stretch) | API reachable only through a self-run WireGuard gateway |

## Changelog

- **0.2** (2026-11-19): Spike 01 decided. Sync model (ADR-0003) and stable file IDs (ADR-0004); first cut of §9 Data model and §10 Sync protocol; log retention and restore settings in §12.
- **0.1** (2026-11-19): Vision, scope, goals, constraints, and open questions. Architecture sections stubbed.
