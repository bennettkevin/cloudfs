# Research spikes

Each spike is time-boxed, tracked as a GitHub issue, and ends with a findings file here (`NN-topic.md`) plus an ADR if it produced a decision.

## Sprint 0 spikes

Ordered roughly by how much they constrain everything else.

| # | Spike | Question to answer | Feeds |
|---|---|---|---|
| 01 | Sync model ([findings](01-sync-model.md), decided) | Server change log + per-client cursor, or per-file version vectors? | Data model, protocol |
| 02 | Conflict resolution ([findings](02-conflicts.md), decided) | What happens when two devices edit the same file offline? | Protocol, client |
| 03 | Encryption ([findings](03-encryption.md), decided) | Server-side at rest only, or client-side end-to-end? (E2E breaks server-side dedup/processing) | Chunking, workers |
| 04 | Chunking ([findings](04-chunking.md), decided) | Fixed-size vs content-defined (FastCDC); chunk size; hash function | Storage layout |
| 05 | Deletes and renames ([findings](05-deletes-renames.md), decided) | Tombstones, move detection, how long deletes are retained | Data model |
| 06 | File watching ([findings](06-file-watching.md), decided) | FSEvents/inotify quirks, atomic-save editors, dropped events, rescans | Client |
| 07 | Uploads ([findings](07-uploads.md), decided) | Presigned uploads/downloads, checksums, verification, orphans, idempotency (MinIO replaced by Garage, ADR-0011) | Protocol, infra |
| 08 | Device auth | How a headless daemon authenticates and how devices are revoked | API |
| 09 | AWS cost model | Idle cost of NAT gateway, RDS, ALB, Fargate; what to design around | Phase 2 infra |
| 10 | Local object storage ([findings](10-local-object-storage.md), decided) | Replacement for MinIO (archived 2026-04): Garage, SeaweedFS, AIStor Free, forks | Goal 7, infra |
| 11 | Name encryption | Encrypt file/folder names? Blind index for case-insensitive uniqueness; compute cost; Unicode normalization | Data model, protocol |
