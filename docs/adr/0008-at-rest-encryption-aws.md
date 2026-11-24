# ADR-0008: At-rest encryption on AWS uses free defaults, with a documented upgrade path to a customer-managed KMS key

- **Status:** Accepted
- **Date:** 2026-11-19
- **Research:** [Spike 03](../research/03-encryption.md) (#3)

## Context

With end-to-end encryption ([ADR-0007](0007-end-to-end-encryption.md)), objects in S3 are already ciphertext. Server-side encryption is defence in depth. The project currently avoids discretionary spending.

Costs (AWS pricing, checked 2026-11-19):
- SSE-S3: free, and on by default for all new S3 objects.
- SSE-KMS with the AWS-managed `aws/s3` key: no key fee; $0.03 per 10,000 KMS requests after 20,000 free per month. No control over the key policy.
- SSE-KMS with a customer-managed key: $1/month per key plus the same request pricing. S3 Bucket Keys cut KMS requests by up to 99%.
- RDS encryption with the AWS-managed `aws/rds` key: free.

## Decision

- **S3:** use the default SSE-S3 encryption. No KMS key for now.
- **RDS:** enable encryption **at creation** with the AWS-managed key. This is free and can't be added in place later.
- **Upgrade path (when wanted):** create a customer-managed KMS key in Terraform, switch the bucket's default encryption to SSE-KMS with S3 Bucket Keys, and grant the API role `kms:GenerateDataKey`/`kms:Decrypt`. No application changes: S3 applies bucket default encryption without client headers. Existing objects keep SSE-S3 until copied in place (optional, since they're already end-to-end encrypted). Moving RDS to the new key needs a snapshot copy with the new key and a restore, so a short maintenance window.

## Alternatives considered

- **Customer-managed KMS key now (~$1/month)** — adds an access audit trail, a way to make all stored data unreadable at once by disabling the key, and KMS/IAM experience. Deferred for cost, not rejected; this is the planned upgrade.
- **SSE-KMS with the AWS-managed key** — near-free, but no control over the key policy, so most of the KMS benefit is missing.
- **SSE-C** — disabled by default on new S3 buckets since April 2026, and awkward with presigned URLs.

## Consequences

- Zero added cost for at-rest encryption.
- Switching later is a configuration change for S3 and a planned snapshot/restore for RDS.
- Open for spike 09: whether a KMS key in its 7–30 day deletion waiting period is billed, relevant to "`terraform destroy` leaves nothing billable" once a key exists.
