# ADR-0019: Phase 2 AWS architecture, on-demand by default, with a $10/month budget

- **Status:** Accepted
- **Date:** 2026-11-24
- **Research:** [Spike 09](../research/09-aws-cost.md) (#9)

## Context

Phase 2 deploys CloudFS to AWS with Terraform. The point is to learn and show industry cloud infrastructure: managed containers, a managed database, load balancing, IAM, infrastructure as code. CloudFS itself doesn't need AWS. Everything through phase 1 runs free on the local Compose stack (API, Postgres, Garage), and the tests use gofakes3 and Garage ([ADR-0011](0011-local-object-storage.md)).

The owner is a student and wants to avoid spending until phase 2, then keep it small. Spike 09 priced the options from the AWS public price list (us-east-1, files published 2026-11-08 to 2026-11-24):

| Setup | Idle | Light use |
|---|---|---|
| A. Fargate ARM + ALB + RDS, always on | ≈ $49/month | ≈ $51/month |
| B. One EC2 t4g running Compose, chunks in S3 | ≈ $13.50–18/month | ≈ $13.50–20/month |
| C. A's architecture, created for demos and destroyed after | ≈ $1.80/month | ≈ $3–6/month |

The fixed costs dominate: the ALB (Application Load Balancer) and its two public IPv4 addresses, about $24/month, and RDS, about $14/month. A NAT gateway would add $36.50/month and interface VPC endpoints $29–58/month. Storage, requests, logs and data transfer together are about $1.35/month at light use, thanks to direct transfer ([ADR-0015](0015-direct-presigned-chunk-transfer.md)).

## Decision

**Architecture (setup A's shape):**
- **Compute:** ECS on Fargate, ARM (Graviton), 0.25 vCPU / 0.5 GB to start. Background workers (GC, trash purge, orphan sweep) run as goroutines in the API task until load justifies a second service. Use Fargate Spot during always-on stretches.
- **Database:** RDS PostgreSQL `db.t4g.micro`, Single-AZ, 20 GB gp3, encrypted at creation ([ADR-0008](0008-at-rest-encryption-aws.md)). Not Aurora Serverless v2: it only pauses with zero connections, which polling devices prevent, and it takes 15–30 s to resume.
- **Network:** public subnets with security groups, **no NAT gateway and no interface endpoints**, plus the free S3 gateway endpoint. The ALB is dual-stack (IPv4 + IPv6). IPv6-only is a later experiment once every device network is known to support it.
- **Secrets:** SSM Parameter Store SecureString (standard tier, free), not Secrets Manager.
- **Logs:** CloudWatch Logs with 14-day retention; per-chunk debug logging off in production.
- **Change signal:** long-poll duration stays below the ALB idle timeout.

**Run it on demand by default (setup C).**
- Terraform is split into two root modules with separate state.
  - **`persistent`** (never destroyed): the S3 bucket, Route 53 zone, ECR repository, SSM parameters, AWS Budgets, the GitHub OIDC deploy role, and the KMS key if one is added.
  - **`ephemeral`** (applied for demos and work sessions, then destroyed): VPC, ALB, ECS service, RDS, security groups, log groups.
- Metadata survives a destroy as a `pg_dump` written to the S3 bucket, restored on the next apply.
- Daily sync stays on the local Compose stack. Always-on periods, such as a phase 3 load-test week or living on it, are deliberate and come with a raised budget.

**Budget: $10/month** during phase 2, raised explicitly (to about $60) for any always-on month. Guardrails go in before the first `terraform apply`:
- AWS Budgets, managed in the `persistent` module: email alerts at 50% and 80% actual, 100% forecast, 100% actual, plus a separate usage budget on data transfer out.
- Cost allocation tags via Terraform `default_tags`: `project`, `env`, `stack`, `managed_by`. Activate `project` and `stack` in the Billing console.
- A destroy-check script that lists anything still tagged `stack=ephemeral` after `terraform destroy`.
- Expensive features off by default, each turned on only in its own PR with the monthly cost in the description: NAT gateway, interface endpoints, Multi-AZ RDS, Container Insights, ALB access logs, GuardDuty/Security Hub/Config/Inspector/Macie.
- The CI deploy role is limited to the services CloudFS uses, in one region.
- Bucket policy: deny non-TLS requests, and cap signature age at 15 minutes (`s3:signatureAge`).

**KMS customer-managed key:** optional, at $1/month. A key pending deletion is not billed (verified on the KMS pricing page, 2026-11-24). If added, it lives in the `persistent` module, because the data it protects outlives the ephemeral stack. Automatic rotation stays off until wanted (+$1/month for each of the first two rotations).

**AWS account plan:** for a new account, start on the Free plan and upgrade to the Paid plan deliberately, with Budgets already in place, before the 6-month cutoff or when credits run low. This only applies if ECS/Fargate, RDS and ALB are on the Free plan's eligible list at sign-up. Otherwise use the Paid plan with the guardrails from the first hour.

## Alternatives considered

- **Setup A always on** — the most realistic, but ≈ $49–51/month to keep a demo running between uses.
- **Setup B (single EC2 with Compose)** — ≈ $13.50–20/month always on, but little managed-service experience: no ECS, ALB or RDS, and "autoscaling" becomes a group of one. Kept as a fallback if a cheap always-on AWS instance is ever wanted.
- **B always-on plus A on demand** — two Terraform setups to maintain.
- **Aurora Serverless v2 with auto-pause** — pausing fights the polling sync pattern; cold starts of 15–30 s.
- **Lambda for the API** — long-polling on Lambda costs about $8/month for 3 devices; a WebSocket API would mean a second, AWS-only notification design.
- **NAT gateway / interface endpoints** — the generic VPC "best practice" that would double the bill. Not needed with public subnets and an S3 gateway endpoint.
- **RDS final snapshot instead of `pg_dump`** — works, but costs more once the instance is gone and isn't portable to the Compose stack.
- **Budget of $5** (alerts constantly once data is stored) or **$60** (normalizes a bill not needed yet).

## Consequences

- No spending until phase 2 starts. Phase 0 and 1 need no AWS account.
- Phase 2 Terraform has two root modules, and every resource is tagged. "Destroy leaves nothing billable" except the persistent module (≈ $1.80/month with data stored).
- In setup C, devices can't sync while the stack is down. Each restore regenerates the log epoch ([ADR-0003](0003-sync-model.md)), so devices do a full reconcile after each apply. A clean-shutdown marker could avoid that (#25).
- Prices and free-tier terms change. Re-check them before the first apply: Free plan eligible services, credit expiry (sources disagree, 6 vs 12 months), and Fargate Spot prices.
- Version retention for live files costs about $0.12/month at 30 days and at most $1.38/month at a year at this scale, so that open question is a product choice, not a cost one.
- Follow-up issues: a per-device byte cap on presigned GET issuance (#24); clean-shutdown epoch handling (#25); a DB pool that can drop to zero idle connections (#26); the destroy-check script (#27).
