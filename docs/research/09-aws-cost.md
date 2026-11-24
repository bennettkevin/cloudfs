# Spike 09: AWS cost model

- **Issue:** #9
- **Status:** Decided. See [ADR-0019](../adr/0019-phase-2-aws-architecture-and-cost-guardrails.md): all recommendations accepted on 2026-11-24. ALB, public IPv4 and KMS pending-deletion figures independently re-checked on 2026-11-24. Re-check prices and Free plan terms before the first `terraform apply`.
- **Question:** What does CloudFS cost per month on AWS in phase 2, idle and in light personal use? Which design choices drive that cost, what does the 2025 free tier change mean for a 2027 start, and what budget and guardrails should be in place before the first `terraform apply`?

Facts below were checked on 2026-11-24 unless marked *unverified*. Evidence types:
- **Price list:** the AWS public Price List API (bulk CSV offer files for us-east-1, no account needed). Each file's publication version is listed in References. These are the authoritative numbers in the tables.
- **Docs:** AWS documentation, pricing pages and What's New / blog announcements, fetched on 2026-11-24.
- **Estimate:** arithmetic on the above with assumptions stated next to it.

All prices are **us-east-1, on-demand, USD**, with a month taken as **730 hours**. Nothing was deployed and no AWS account was used.

**Region.** us-east-1 is kept. For the services here, us-east-2 and us-west-2 cost the same or within cents (*prior knowledge; other regions' price files were not downloaded*), so no region is clearly cheaper. Pick the US region closest to you for latency. One us-east-1 wrinkle: Fargate on ARM isn't offered in the `use1-az3` availability zone (*from an AWS docs search snippet, not fetched in full*), so let Terraform choose subnets by AZ ID or just avoid that AZ.

**Usage profiles used throughout:**
- **Idle:** the stack is deployed and running 24/7, the bucket is empty, no clients are connected. This is what you pay just for things existing.
- **Light use:** 1 user, 3 devices, ~50 GB stored (~56 GB after ADR-0010's ≤12% padding), ~5 GB/month of changed content uploaded (~5,000 chunks at ~1 MiB average), ~10 GB/month downloaded by devices.

---

## 0. Decisions this builds on

- Same code locally and on AWS; AWS is S3 + RDS + ECS in design goal 7, all built by Terraform, and `terraform destroy` must leave nothing billable (design §5).
- Clients move chunk bytes **directly** to S3 with 15-minute presigned URLs. The API never touches chunk bytes ([ADR-0015](../adr/0015-direct-presigned-chunk-transfer.md)).
- Commit HEADs each new chunk once; a weekly ListObjectsV2 sweep removes orphans ([ADR-0015](../adr/0015-direct-presigned-chunk-transfer.md), [ADR-0016](../adr/0016-upload-reservations-and-idempotent-commits.md)).
- ~131k objects for 50 GB / 100k files ([ADR-0010](../adr/0010-chunk-storage-and-manifests.md)).
- Live updates use a long-poll or WebSocket "come fetch" signal ([ADR-0003](../adr/0003-sync-model.md)).
- At rest: SSE-S3 and RDS with the AWS-managed key, both free. A customer-managed KMS key is the planned upgrade ([ADR-0008](../adr/0008-at-rest-encryption-aws.md)).
- Database restore regenerates the log `epoch`, which makes every device do a full reconcile ([ADR-0003](../adr/0003-sync-model.md)).

---

## 1. Price sheet (the raw numbers)

**Jargon used below:**
- **Fargate:** AWS runs your container; you pay per second for the vCPU and memory you reserve, no server to manage. **ECS** is the scheduler that tells Fargate what to run.
- **Graviton / ARM:** AWS's own ARM CPUs. Go cross-compiles to ARM with one env var (`GOARCH=arm64`), so this is a free choice for CloudFS.
- **Spot:** spare AWS capacity sold at a discount. AWS can take it back with a 2-minute warning.
- **ALB (Application Load Balancer):** a managed HTTPS front door that terminates TLS and forwards requests to your containers.
- **LCU (Load Balancer Capacity Unit):** the ALB's usage meter. Per hour, one LCU is 25 new connections/s, **or** 3,000 active connections/min, **or** 1 GB processed, **or** 1,000 rule evaluations/s; you pay for whichever dimension is highest (ELB pricing page).
- **NAT gateway:** a managed box that lets things in a *private* subnet (no public IP) reach the internet. Billed per hour **and** per GB.
- **VPC endpoint:** a private path from your VPC to an AWS service. **Gateway** endpoints (S3, DynamoDB only) are free. **Interface** endpoints (ECR, CloudWatch Logs, Secrets Manager, SSM, …) are billed per endpoint per AZ per hour.
- **ACU (Aurora Capacity Unit):** Aurora Serverless v2's unit of database size, roughly 2 GiB of memory plus matching CPU (*the 2 GiB figure is prior knowledge*).

### Compute

| Item | Price (price list) | 24/7 month |
|---|---|---|
| Fargate x86, per vCPU-hour / GB-hour | $0.04048 / $0.004445 | — |
| Fargate ARM, per vCPU-hour / GB-hour | $0.03238 / $0.00356 | — |
| **Smallest task (0.25 vCPU, 0.5 GB), x86** | | **$9.01** |
| **Smallest task (0.25 vCPU, 0.5 GB), ARM** | | **$7.21** |
| Same, Fargate Spot x86 ($0.014291 / $0.00156926)¹ | | $3.18 |
| Same, Fargate Spot ARM ($0.01143139 / $0.00125682)¹ | | **$2.54** |
| 0.5 vCPU / 1 GB ARM (if 0.5 GB is too small) | | $14.42 |
| EC2 t4g.nano / t4g.micro / t4g.small (Linux) | $0.0042 / $0.0084 / $0.0168 per h | $3.07 / $6.13 / $12.26 |
| EC2 t3.micro (x86, for comparison) | $0.0104 per h | $7.59 |
| EBS gp3 volume | $0.08 per GB-month | 20 GB: $1.60 |
| EBS snapshot | $0.05 per GB-month | — |
| Lambda ARM | $0.0000133334 per GB-second, $0.20 per million requests (*request price not re-checked*) | — |
| API Gateway HTTP API | $1.00 per million requests | — |
| API Gateway WebSocket | $1.00 per million messages, $0.25 per million connection-minutes | — |

¹ Fargate Spot prices change over time. These are from the feed behind the Fargate pricing page on 2026-11-24, about 65% below on-demand. AWS says "up to 70%". Fargate rounds to the second with a 1-minute minimum, and every task gets 20 GB of ephemeral storage at no charge (Fargate pricing page). Fargate Spot on ARM has been supported on ECS since September 2024.

### Database

| Item | Price (price list) | 24/7 month |
|---|---|---|
| RDS PostgreSQL db.t4g.micro, Single-AZ | $0.016 per h | **$11.68** |
| RDS PostgreSQL db.t4g.small, Single-AZ | $0.032 per h | $23.36 |
| RDS gp3 storage | $0.115 per GB-month | 20 GB minimum (*minimum is prior knowledge*): **$2.30** |
| RDS backup storage beyond the free allowance | $0.095 per GB-month | $0 while backups ≤ allocated storage (*free allowance = 100% of provisioned storage is prior knowledge, unverified*) |
| Aurora PostgreSQL Serverless v2 | $0.12 per ACU-hour | 0.5 ACU 24/7: **$43.80** |
| Aurora storage / I/O (Standard) | $0.10 per GB-month / $0.20 per million I/Os | ~1 GB metadata: ~$0.10 |

### Networking

| Item | Price (price list) | 24/7 month |
|---|---|---|
| ALB hour | $0.0225 per h | **$16.43** |
| ALB LCU | $0.008 per LCU-hour | Light use: cents (section 5) |
| Public IPv4 address, in use or idle | $0.005 per h | **$3.65 each** |
| NAT gateway hour + data | $0.045 per h + $0.045 per GB | **$32.85** + per GB, plus its own IPv4 ($3.65) |
| Interface VPC endpoint | $0.01 per endpoint per AZ per h + $0.01 per GB | $7.30 each per AZ |
| Gateway VPC endpoint (S3) | Free (VPC pricing page) | $0 |
| Data out to the internet | First 100 GB/month free, "aggregated globally"; then $0.09 per GB for the next 10 TB | Light use (10 GB): $0 |
| Data in from the internet; S3 → EC2/Fargate in the same region | Free | $0 |

The public IPv4 charge started on 2024-02-01. It applies to every public IPv4 address, including the ones an ALB or a Fargate task uses ("standard public IPv4 address charges for all the addresses you consume with load balancers", ELB pricing page). An internet-facing ALB spans at least two AZs (*prior knowledge*), so it holds at least **two** public IPv4 addresses: **$7.30/month** on top of its hourly charge.

### Storage, secrets, DNS, keys, monitoring

| Item | Price (price list / docs) | Light-use month |
|---|---|---|
| S3 Standard storage | $0.023 per GB-month (first 50 TB) | 56 GB: **$1.29** |
| S3 PUT / COPY / POST / LIST | $0.005 per 1,000 | ~5,000 PUTs + sweep: $0.03 |
| S3 GET / HEAD ("all other") | $0.0004 per 1,000 | ~15,000: $0.01 |
| S3 DELETE | Free (S3 pricing page) | $0 |
| CloudWatch Logs ingestion (Standard / Infrequent Access) | $0.50 / $0.25 per GB | $0 under the 5 GB free tier |
| CloudWatch Logs storage | $0.03 per GB-month | ~$0 with 14-day retention |
| CloudWatch custom metrics / alarms | $0.30 per metric-month / $0.10 per alarm-month | $0 under 10 metrics, 10 alarms free |
| Secrets Manager | $0.40 per secret-month + $0.05 per 10,000 API calls | $0.40 per secret |
| SSM Parameter Store, standard tier | No line item in the price list (only advanced parameters at $0.05/month) | **$0** |
| ECR private storage | $0.10 per GB-month | ~0.1 GB of images: $0.01 |
| Route 53 hosted zone | $0.50 per zone-month (first 25) | **$0.50** |
| Route 53 queries | $0.40 per million; alias queries to AWS resources free | ~$0 |
| ACM public certificate | Free when used with ALB/CloudFront (*prior knowledge*) | $0 |
| AWS Budgets | $0 per budget-day; action-enabled budgets: first 62 budget-days per month free, then $0.10 per budget-day; $0.01 per emailed report | $0 |
| KMS customer-managed key | $1 per key-month, **prorated hourly**; $0.03 per 10,000 requests after 20,000 free per month | $1.00 |

### KMS details (open question from ADR-0008)

From the KMS pricing page and developer guide, fetched 2026-11-24:
- **A key pending deletion is not billed.** The pricing page: "There is no charge for customer managed KMS keys that you manage and are scheduled for deletion." If you cancel the deletion, the key is charged "as though it was never scheduled for deletion". **Verified.**
- The waiting period is 7–30 days (default 30), and the actual deletion can be up to 24 hours later than scheduled. The key can't be used while pending deletion (KMS developer guide).
- Key cost is prorated by the hour, so a key that exists for 20 hours of a demo costs about $0.03.
- **Rotation adds cost.** "The first and second rotation of the key adds $1/month (prorated hourly)", capped after the second rotation. With yearly automatic rotation, one key is $1/month in year 1, $2 in year 2, $3 from year 3. Worth knowing before turning rotation on.
- With **S3 Bucket Keys**, request volume at CloudFS's scale stays far under 20,000/month. Request cost is $0.

So `terraform destroy` with a KMS key is compatible with "destroy leaves nothing billable". The catch is elsewhere: anything encrypted with that key (the bucket's new objects, an RDS snapshot you keep for the next apply) becomes unreadable once the key is gone. **If data outlives the stack, the key has to outlive it too** (section 4, setup C).

---

## 2. Compute options

| Option | Idle month | What it buys | What it costs you |
|---|---|---|---|
| **Fargate ARM, 0.25 vCPU / 0.5 GB, on-demand** | $7.21 + $3.65 public IP = **$10.86** | The production pattern: containers, ECS service, rolling deploys, task roles, autoscaling for phase 3 | 0.5 GB is tight but plausible for a small Go API with workers as goroutines (*to be measured in phase 1*) |
| Fargate x86, same size | $9.01 + $3.65 = $12.66 | Nothing over ARM | 25% more for the same thing |
| **Fargate Spot ARM** | $2.54 + $3.65 = **$6.19** | Same as Fargate at ~65% off | Can be interrupted (2-minute SIGTERM). A single-task service stays down until Spot capacity returns ("A service with only one task is interrupted until capacity is available", ECS docs). Clients retry, so for one user this is acceptable |
| **EC2 t4g.small running the Compose stack** | $12.26 + $1.60 EBS + $3.65 IP = **$17.51** (includes Postgres) | Cheapest way to run API **and** database; same Compose file as phase 1 | You patch the OS, run Postgres backups, renew TLS (Caddy does it). Less "cloud architecture" for the portfolio; no managed DB |
| EC2 t4g.micro (1 GB) | $6.13 + $1.60 + $3.65 = **$11.38** | Same, half the RAM | API + Postgres in 1 GB needs care (small `shared_buffers`, swap file) |
| **Lambda + API Gateway** | ~$0 compute when idle | True scale-to-zero for the API | See below. Doesn't remove the database cost, and the change signal gets expensive or awkward |

**Lambda and the change signal.** Design §10 keeps one long-poll or WebSocket per online device.
- **Long-poll on Lambda** holds an invocation open for the whole wait. Three devices always online, 128 MB ARM functions, 30 days: 7.8M seconds × 0.125 GB = 972,000 GB-seconds. After the 400,000 GB-second always-free allowance that's about **$7.63**, plus ~$0.31 in HTTP API requests (25-second polls). API Gateway HTTP APIs cap integration time at about 30 s (*prior knowledge, unverified*), so polls have to stay short. That's no cheaper than a Fargate task.
- **API Gateway WebSocket** is cheap: 3 devices × 43,200 connection-minutes = $0.03/month, and Lambda only runs on connect, disconnect and notify. But the commit path then has to push to connection IDs through the `@connections` API instead of a goroutine and an in-process channel or Postgres `LISTEN/NOTIFY`. That's a second notification design that the Compose stack doesn't have. Connections are also capped at 2 hours with a 10-minute idle timeout (*prior knowledge, unverified*), so clients reconnect.
- **Workers** (GC, trash purge, weekly sweep) would become scheduled Lambdas (EventBridge Scheduler), another AWS-only path.
- Lambda in a VPC needs no NAT if it only talks to RDS and S3 (gateway endpoint). But it still needs a database, so the realistic floor is **RDS ($14) or a paused Aurora (~$1–10)**.

**Leaning:** **Fargate ARM** for the portfolio setup, with **Fargate Spot** as a dial you can turn once deploys work. Run the background workers as goroutines in the API task, not as a second service (a second 0.25 vCPU task adds $7.21 + $3.65). Lambda saves ~$10 at best and breaks "same code in both places" for the notification path. Revisit it only if phase 3 wants a serverless comparison.

---

## 3. Database options

| Option | Idle month | Light-use month | Notes |
|---|---|---|---|
| **RDS PostgreSQL db.t4g.micro, Single-AZ, 20 GB gp3** | $11.68 + $2.30 = **$13.98** | **$13.98** (automated backups fit in the free allowance, *unverified*) | Managed backups and point-in-time restore, encryption at creation (ADR-0008). 1 GB RAM is plenty for one user's metadata. Single-AZ: an AZ outage takes it down, which is fine for one user. Can be **stopped** for up to 7 days at a time, after which AWS restarts it (*prior knowledge*), paying storage only while stopped |
| **Aurora Serverless v2, min 0 ACU (auto-pause)** | ~$0.10 storage + nothing for compute while paused | **~$5–45**, depending on how often it wakes (below) | Scale-to-zero is real: min capacity 0 ACU, pause after 300 s–86,400 s with no user connections, "You aren't charged for instance capacity while an instance is in the paused state". Needs Aurora PostgreSQL ≥ 16.3 / 15.7 / 14.12 / 13.15 |
| **Postgres in Compose on the EC2 box** | $0 extra (included in section 2's EC2 line) | $0 extra + EBS snapshots ~$0.25 | You own backups, upgrades and restore testing |

**Why Aurora's light-use range is so wide.**
- **Resume latency:** "approximately 15 seconds" normally, and "30 seconds or longer" if it has been paused for more than 24 hours (Aurora docs).
- **It only pauses with zero user connections.** Go's `pgx` pool keeps idle connections open by default, so the pool must be configured to drop to zero when idle. A long-poll that holds a DB connection, or a worker on a timer, keeps it awake forever.
- **After a wake for maintenance it stays up at least 20 minutes.** It resumes at a small capacity (the docs' example shows 2 ACU) and bills at least 0.5 ACU while awake.
- **The arithmetic:** three devices doing hourly rescans and pulls wake it dozens of times a day. Each wake costs at least the 5-minute idle timeout. Awake ~6 h/day at 0.5–1 ACU is roughly **$11–22/month**, and awake 24/7 is $43.80 at 0.5 ACU. On a pure demo account with no devices it's close to $0.
- **Pausing fights CloudFS's design.** The change-signal path and the hourly rescans in design §10 are exactly the always-touching pattern that stops pausing.

**Leaning:** **RDS db.t4g.micro**. It's predictable, the cheapest managed option for anything used daily, and it's what design goal 7 names. Aurora with auto-pause is attractive only for the on-demand / demo-only setup, and even there destroying RDS and restoring from a dump is cheaper (section 4C).

---

## 4. Networking: where the money actually goes

**Pattern that avoids NAT:** put the Fargate task (or EC2 box) in a **public subnet with a public IPv4 address**, and lock it down with a **security group** (a per-resource firewall) that allows inbound traffic only from the ALB. The task can then pull images from ECR, write CloudWatch Logs and read SSM parameters over the internet, encrypted and IAM-authenticated, with no NAT. RDS stays in private subnets with no public IP and accepts connections only from the task's security group. Add the **free S3 gateway endpoint** so S3 traffic (HEAD, LIST, DELETE from the API) stays on the AWS network. It costs nothing and allows a bucket policy condition on the VPC endpoint for server-side calls later.

| Way for a task to reach AWS APIs | Monthly | Verdict |
|---|---|---|
| Public subnet + task public IPv4 + security group | **$3.65** per task | **Use this** |
| Private subnet + NAT gateway (one AZ) | $32.85 + $3.65 NAT IP + $0.045/GB | Standard in production, **3× the cost of the whole compute layer** here. Avoid |
| Private subnet + interface endpoints (ECR API, ECR Docker, Logs, SSM = 4) | 4 × 2 AZ × $7.30 = **$58.40** (one AZ: $29.20) | Avoid; even worse than NAT at this scale |

**ALB or not.**
- An ALB is **$16.43 + $7.30 (two IPv4) ≈ $23.73/month**. That's about half of setup A's bill, for a box that serves one user. It does give you the standard pieces: TLS with a free ACM certificate, health checks, rolling deploys with zero downtime, and the target for phase 3 autoscaling.
- **Cutting the ALB's IPv4 cost:** since May 2024 an internet-facing ALB can use `dualstack-without-public-ipv4`, which gives it public IPv6 addresses only. That saves $7.30/month, but every client network must have working IPv6. Many home ISPs, university networks and cafés still don't, so this is a gamble for a sync client that roams. *Not recommended as the default; worth a phase 3 experiment.* Whether Fargate tasks can run in IPv6-only subnets (dropping the task's IPv4 too) was *not checked*.
- **No-ALB alternatives:**
  - EC2 with a fixed Elastic IP and Caddy for TLS (setup B).
  - A Fargate task with its own public IP and TLS in-process. The IP changes on every task restart, so you'd need an EventBridge rule to update Route 53. Fragile; not recommended.

---

## 5. Candidate setups

All three share: S3 bucket (SSE-S3), Route 53 hosted zone, ACM certificate, ECR repository with a lifecycle rule (keep the last 5 images), SSM Parameter Store for secrets, CloudWatch log group with **14-day retention**, Budgets, GitHub OIDC role for CI. No NAT, no interface endpoints. Light-use S3 is 56 GB, plus ~$0.04 in requests (section 7), plus 10 GB egress inside the free 100 GB.

### A. "Portfolio-realistic": Fargate + RDS + ALB, always on

| Line item | Idle | Light use |
|---|---|---|
| Fargate ARM task, 0.25 vCPU / 0.5 GB (API + workers) | $7.21 | $7.21 |
| Task public IPv4 | $3.65 | $3.65 |
| ALB hours | $16.43 | $16.43 |
| ALB public IPv4 × 2 | $7.30 | $7.30 |
| ALB LCUs | ~$0.05 | ~$0.10 |
| RDS db.t4g.micro Single-AZ | $11.68 | $11.68 |
| RDS 20 GB gp3 | $2.30 | $2.30 |
| S3 storage + requests | $0 | $1.33 |
| Egress (10 GB, inside free 100 GB) | $0 | $0 |
| Route 53 zone | $0.50 | $0.50 |
| ECR, CloudWatch Logs, SSM, ACM, Budgets | ~$0.02 | ~$0.02 |
| **Total** | **≈ $49** | **≈ $51** |
| *With Fargate Spot ARM instead* | *≈ $45* | *≈ $46* |
| *Optional: KMS customer-managed key* | *+$1* | *+$1* |

### B. "Budget": one EC2 t4g instance running Compose (API + Postgres + Caddy), chunks in S3

Keep chunks in **S3, not Garage on EBS**. EBS gp3 is $0.08/GB-month against S3's $0.023, so storing 56 GB on the instance's disk would cost $4.48 instead of $1.29, and S3 keeps the presigned-URL path identical to setup A.

| Line item | Idle (t4g.small) | Light (t4g.small) | Light (t4g.micro) |
|---|---|---|---|
| EC2 instance | $12.26 | $12.26 | $6.13 |
| EBS gp3 20 GB (OS + Postgres) | $1.60 | $1.60 | $1.60 |
| Public IPv4 (Elastic IP) | $3.65 | $3.65 | $3.65 |
| EBS snapshots (daily, keep 7; incremental, ~5 GB) | $0.25 | $0.25 | $0.25 |
| S3 storage + requests | $0 | $1.33 | $1.33 |
| Route 53 + ECR + misc. | $0.52 | $0.52 | $0.52 |
| **Total** | **≈ $18** | **≈ $20** | **≈ $13.50** |

What you give up compared with A: managed DB backups and point-in-time restore, zero-downtime deploys, ECS/ALB experience, and phase 3 "autoscaling" becomes an Auto Scaling group of one. What you gain: about $31/month, and the same Compose file as phase 1.

### C. "On-demand": setup A's architecture, created for demos and torn down with Terraform

Split Terraform into two root modules (separate state files):
- **`persistent`** (never destroyed): S3 bucket, Route 53 zone, ECR repo, SSM parameters, Budgets, the GitHub OIDC role, **and the KMS key if you add one** (section 1).
- **`ephemeral`** (apply / destroy): VPC, ALB, ECS service, RDS, security groups, log groups. Its inputs come from the persistent module's outputs.

**State that must survive a destroy:**
- **Chunks:** the S3 bucket, which lives in the persistent module.
- **Metadata:** a database dump. Options:
  - an RDS final snapshot (`skip_final_snapshot = false`), $0.095/GB-month once the instance is gone, roughly $0.10–1.90 depending on whether it bills used or allocated size (*unverified*);
  - or a `pg_dump` written to the S3 bucket at $0.023/GB, about $0.01. Cheaper, portable to the Compose stack, and it exercises the restore runbook.
- **Note on Route 53:** a hosted zone is billed for the whole month once it exists more than 12 hours (*prior knowledge, unverified*). Recreating it each demo would also change its name servers, so it must be persistent.

| Line item | Torn down all month | Light: 20 h up / month | Light: 60 h up / month |
|---|---|---|---|
| Ephemeral stack at $0.0634/h (task, task IP, RDS, ALB, ALB IPs) | $0 | $1.27 | $3.80 |
| RDS storage while up (prorated), LCUs | $0 | ~$0.10 | ~$0.25 |
| S3 storage (56 GB) + requests | $1.29 (if data kept) | $1.33 | $1.33 |
| DB dump in S3 | ~$0.01 | ~$0.01 | ~$0.01 |
| Route 53 + ECR + misc. | $0.52 | $0.52 | $0.52 |
| **Total** | **≈ $1.80** | **≈ $3.25** | **≈ $5.90** |
| *KMS key in persistent stack* | *+$1* | *+$1* | *+$1* |

**The catch: C isn't a sync service you can live in.** Devices can't sync while the stack is down. It suits "build and demo phase 2 on AWS, daily sync runs on the Compose stack at home". A restore also regenerates the log `epoch` per ADR-0003, so every device does a full reconcile after each apply. That's correct but slow, and see section 7 for a design tweak.

### What dominates, and what to design around

1. **ALB + its IPv4 addresses: ~$24/month**, about 47% of A. The one big fixed cost that exists only for TLS and routing.
2. **RDS hours + storage: ~$14/month**, about 28% of A.
3. **Public IPv4 overall: $11/month** in A (three addresses), $3.65 in B. Every resource with a public address costs $3.65/month, so count them in code review.
4. **NAT gateway: $36.50/month that A deliberately doesn't have.** The single easiest way to double the bill by following a generic "best practice" VPC module. Most Terraform VPC modules enable NAT by default (*prior knowledge*), so set `enable_nat_gateway = false` explicitly.
5. **S3 storage, requests, logs, egress: about $1.35 together at light use.** The data path is not where the money is. That is a direct result of ADR-0015's direct transfer.

---

## 6. Free tier for a 2027 start

### What changed on 2025-07-15

From the AWS Billing docs ("Choosing a plan"), the Free Tier FAQs and the 2025-07-16 What's New post:
- **Credits:** new customers get **USD $100 in credits at sign-up** and can earn **up to $100 more** by completing activities in the console's "Explore AWS" widget. That's the same on both plans.
- **Free plan:**
  - **No charges at all** while it lasts.
  - It ends after **6 months or when credits run out**, whichever is first.
  - It only covers "select AWS services". Excluded services are ones "that could possibly deplete your credits" (examples given: Savings Plans, Reserved Instances, some Marketplace offers).
  - When it ends, "your account closes automatically, and you lose access to your resources and data." AWS keeps the content for **90 days**, then permanently deletes the account and everything in it, unless you upgrade to the Paid plan within those 90 days. Leftover credits carry over when you upgrade.
  - It **automatically becomes a Paid plan** if you join AWS Organizations, set up Control Tower, and a few other triggers.
- **Paid plan:**
  - All services. The same credits apply first, then standard pay-as-you-go pricing.
  - The account doesn't close when credits run out.
  - Includes "short-term trials" for some services.
  - **Credits expire 12 months after account creation** per the Paid plan FAQ and the ELB pricing page. Another FAQ section says 6 months, so treat the expiry as **6–12 months (conflicting sources)**.
- **Always free** (both plans, no end date): "30+ services" with monthly limits. The ones that matter here:
  - 100 GB/month data transfer out, aggregated across services and regions (price list row `Global-DataTransfer-Out-Bytes`)
  - CloudWatch: 5 GB of logs (ingestion, archive and Logs Insights scanning), 10 custom metrics, 10 alarms, 3 dashboards, 1M API requests
  - Lambda: 1M requests and 400,000 GB-seconds (price list `Global-` rows)
  - KMS: 20,000 requests/month
  - Budgets aren't a free-tier item; they simply cost $0
- **Not free any more for new accounts:** the old 12-month allowances (750 h/month of a micro EC2 instance, 750 h of db.t*.micro RDS, 750 h of ALB + 15 LCU, 5 GB of S3, 750 h of public IPv4 for EC2) belong to the **legacy free tier**, which applies only to accounts created **before 2025-07-15**. New accounts get the credits instead. On the new plans, the free-tier EC2 instance types are `t3.micro`, `t3.small`, `t4g.micro`, `t4g.small` and a few others, but the "usage limit" is the credit balance, not free hours (EC2 docs).
- **Eligibility:** credits and the Free plan are "available only to new AWS customers". You're ineligible "if you have an existing AWS account or have previously had one" (Free Tier FAQs).

*Unverified:*
- **Which services the Free plan allows.** The list lives on aws.amazon.com/free and the fetched copy didn't enumerate it. Check that ECS/Fargate, RDS and ALB are on it **before** choosing the Free plan.
- The Free Tier FAQ refers to an "October 2026 sign-up change" without details, and a search found no announcement. Re-check the terms on the day you sign up.

### What it means for you

**Case 1: no AWS account yet, signing up in 2027.**
- $100–200 in credits. Setup A at ~$50/month uses that up in 2–4 months. Setup C at ~$2–6/month barely touches it. Setup B lasts 5–10 months.
- **Free plan:** the strongest guardrail available. You literally can't be charged. Its cliff is the problem: at 6 months or $0 credits, the account is suspended and erased 90 days later unless you upgrade. Phases 2–3 for a part-time student likely run longer than 6 months.
- **Paid plan:** no cliff, credits still apply first, but no hard cap. Budgets alert; they don't stop spending.
- **Suggested:** sign up on the **Free plan** if ECS, RDS and ALB are on its eligible list. Build phase 2 under a hard $0 ceiling, then **upgrade deliberately** (with Budgets already in place) before the 6-month mark or when credits drop below ~$25. If those services aren't eligible, use the Paid plan with section 8's guardrails from the first hour.

**Case 2: an account already exists.**
- **Created before 2025-07-15:** the legacy 12-month offers end 12 months after creation, so **by 2027 they're gone**. Only always-free remains. Not eligible for the new credits.
- **Created after 2025-07-15:**
  - On the **Free plan**, it has closed after 6 months, and if not upgraded within 90 days it has been erased. By 2027 it's likely gone; you can't get new credits by opening another.
  - On the **Paid plan**, credits have expired (6–12 months). List prices apply.
- **Either way, plan on list prices in 2027**, minus always-free. Don't open extra accounts to re-farm credits; the terms exclude anyone who "previously had one".

---

## 7. Design implications (quantified)

| Design element | Monthly cost at light use | Verdict |
|---|---|---|
| HEAD once per new chunk at commit (ADR-0015) | 5,000 × $0.0004/1,000 = **$0.002** | Negligible. Keep |
| Weekly ListObjectsV2 sweep (ADR-0016) | 131 pages × 4.3 weeks × $0.005/1,000 = **$0.003** | Negligible. Keep |
| Presigned PUTs (one per chunk) | 5,000 × $0.005/1,000 = **$0.025** | Negligible |
| Presigned GETs for 10 GB downloads | ~10,000 × $0.0004/1,000 = **$0.004** | Negligible |
| Long-poll through the ALB, 3 devices, ~30 s polls | Active connections 3 of 3,000 per LCU; new connections ~0.1/s of 25/s per LCU → < 0.01 LCU-hour/h ≈ **$0.05** | Negligible. **Set the poll duration below the ALB idle timeout** (default 60 s, configurable up to 4,000 s; *prior knowledge*), or the ALB cuts polls with 504s |
| Long-poll on Lambda | ≈ **$8** (section 2) | Avoid if you go serverless; use a WebSocket API |
| CloudWatch Logs | One ~300-byte line per request; 3 devices long-polling every 30 s ≈ 260k lines ≈ 80 MB, plus normal API traffic → **well under 5 GB free** | Free *if* per-chunk debug logging is off in production. A 131k-chunk initial sync logging 3 lines per chunk is ~120 MB once. Set retention (default is "never expire") |
| Container Insights, VPC Flow Logs, RDS Performance Insights / log exports | Container Insights bills custom metrics at $0.30 each | Off by default in Terraform; turn on deliberately in phase 3 |
| Initial download to each new device | 50 GB per device. Two new devices in the same month = 100 GB = the whole free allowance; a third = **$4.50** | One-off. Budget for it when onboarding devices |
| Secrets in Secrets Manager (e.g. RDS `manage_master_user_password`) | $0.40 per secret | Prefer SSM Parameter Store SecureString (standard tier, free) or RDS IAM auth. Note that RDS-managed master passwords create a Secrets Manager secret automatically |
| Second ECS service for workers | +$7.21 task + $3.65 IP | Keep workers as goroutines in the API task until load justifies splitting |
| Phase 3 load test | Egress at $0.09/GB if the load generator is outside AWS | Run the load generator on EC2 in the same region (S3 → EC2 in-region is free), or use mostly upload-heavy tests (ingress is free) |

**Design tweaks worth making** (for discussion):
1. **API database pool must be able to drop to zero idle connections** (`pgx` `MinConns=0` plus a short `MaxConnIdleTime`), and the change signal shouldn't hold a DB connection per waiting client. It's needed for Aurora auto-pause if ever used, and good hygiene with RDS's ~80-connection limit on a micro instance (*limit is prior knowledge*).
2. **Distinguish a clean-shutdown dump from a disaster restore.** ADR-0003 regenerates `epoch` on every restore so devices full-reconcile. In setup C, a dump taken *after* the API is stopped contains every committed change, so regenerating the epoch is unnecessary and costs a full reconcile per demo. A "clean shutdown" marker written into the dump would let restore keep the epoch. This needs its own ADR amendment if setup C is chosen; *not urgent*.
3. **Meter presigned GET bytes per device** (the API knows every chunk's padded length): a per-device daily cap protects against egress abuse (section 8).

### Version retention (open question in design §7)

At this scale it barely matters. With keyed chunk dedup, an old version costs only the chunks that differ from the current one.
- **5 GB/month of changed content kept for 30 days:** ≤ ~5 GB extra, about **$0.12/month**.
- **Kept for a year:** at most ~60 GB extra, about **$1.38/month**. In practice less, since edits overlap and deleted files already go through the 30-day trash.
- **Where it would matter:** large files rewritten wholesale often (VM images, database files, video exports), where each version is mostly new chunks.

Recommendation for that decision: pick retention on product grounds (e.g. 30–90 days), not cost. Revisit only if the bucket grows past a few hundred GB.

---

## 8. Budget and guardrails

**No AWS feature hard-caps spending on a Paid plan.** Budgets and alarms only notify (or run actions) after the fact, and billing data lags by hours (*refresh frequency is prior knowledge, unverified*). So guardrails are layered:

1. **AWS Budgets** (free) on day one, before the first `terraform apply`, managed in the `persistent` Terraform module:
   - Monthly cost budget at the target (section 9) with email alerts at **50% actual, 80% actual, 100% forecasted, 100% actual**.
   - A second, **usage-type budget on data transfer out** (e.g. alert at 80 GB/month), so egress abuse is noticed before the free 100 GB runs out.
   - Optional **budget action**: at 120% of target, attach an IAM deny policy to the CI deploy role. One action-enabled budget is ~30 budget-days per month, inside the 62 free.
   - **Cost Anomaly Detection** with a daily email (*free per AWS docs, prior knowledge, unverified*).
2. **Cost allocation tags:**
   - Terraform `default_tags` on the AWS provider: `project=cloudfs`, `env=dev|demo`, `stack=persistent|ephemeral`, `managed_by=terraform`.
   - **Activate** `project` and `stack` as cost allocation tags in the Billing console; they only apply from activation onward.
   - Use them for the phase 3 cost report.
3. **Teardown discipline:**
   - A `make destroy-check` script that lists anything still tagged `stack=ephemeral` via the Resource Groups Tagging API after `terraform destroy`.
   - Look for things destroy can miss: log groups AWS creates on its own (declare every log group in Terraform), RDS final snapshots, unattached Elastic IPs, and ECR images (lifecycle rule).
   - KMS keys pending deletion cost nothing (section 1).
4. **Least privilege as cost control:**
   - The GitHub OIDC deploy role gets only the services CloudFS uses, in one region (`aws:RequestedRegion` condition). A leaked CI credential then can't launch GPUs elsewhere.
   - Don't enable GuardDuty, Security Hub, Config, Inspector or Macie casually. They start free trials that turn into ongoing charges (*prior knowledge*).
5. **S3 egress abuse:**
   - **One leaked presigned GET:** one object (≤ ~4.6 MiB), downloadable repeatedly for 15 minutes. Abuse is bounded by bandwidth. At 1 Gbit/s sustained that's ~110 GB, about **$10** beyond the free tier. Real, but small.
   - **A leaked device token** is the bigger risk: it can mint URLs indefinitely.
   - Mitigations:
     - per-device rate limit and daily byte cap on URL issuance (section 7, tweak 3)
     - fast device revocation (spike 08)
     - the data-transfer usage budget above
     - a bucket policy denying non-TLS access (`aws:SecureTransport`)
     - a `s3:signatureAge` condition capping signatures at 15 minutes server-side, so a bug can't mint week-long URLs
   - Turning on S3 server access logs to investigate costs storage; leave it off unless investigating.
6. **Default-off expensive features:** NAT gateway, interface endpoints, Multi-AZ RDS, Container Insights, Performance Insights beyond the free retention, ALB access logs. Each one is a deliberate pull request with its monthly cost in the description.

---

## Recommendation (accepted 2026-11-24)

1. **Architecture for phase 2: setup A's shape** (Fargate ARM + ALB + RDS db.t4g.micro Single-AZ + S3, public subnets, no NAT, no interface endpoints, free S3 gateway endpoint). It's what design goal 7 describes and the most portfolio-relevant.
2. **Run it as setup C (on-demand) by default.** Two Terraform modules (`persistent` / `ephemeral`), with `pg_dump` to S3 on destroy and restore on apply. **≈ $2–6/month.** Daily personal sync stays on the Compose stack at home.
3. **Switch to always-on (≈ $45–51/month)** only for specific windows: a phase 3 load-test week, or a period when you want to actually live on it. Use Fargate Spot ARM when always-on.
4. **Keep setup B in your back pocket** if you later want an always-on AWS instance cheaply (≈ $13.50–20/month). It reuses the Compose file but gives up managed-service experience.
5. **KMS customer-managed key:** affordable whenever you want it ($1/month; nothing while pending deletion). It belongs in the `persistent` module because the bucket's objects and any kept snapshot depend on it. Leave automatic rotation off until you want to pay $2–3/month for it.
6. **Workers as goroutines** in the API task; **SSM Parameter Store** for secrets; **14-day log retention**; poll duration below the ALB idle timeout.
7. **Budget target: $10/month** during phase 2 (fits on-demand use plus a few always-on days), with the guardrails in section 8 in place before the first apply. Raise it explicitly to ~$60 for any always-on month.

---

## Decisions for the user

1. **Monthly budget target (phase 2).**
   - Options: $5; **$10**; $25; $60 (always-on setup A).
   - **Recommended: $10**, raised deliberately for an always-on window. It covers on-demand demos (~$2–6) with headroom for a few always-on days (~$1.65/day for A). $5 alerts constantly once data is stored; $60 normalises a bill you don't need yet. Alerts at 50% / 80% / 100% forecast / 100% actual, plus a data-transfer usage budget.
2. **Runtime mode.**
   - Options: A always-on; B always-on; **C on-demand with A's architecture**; B always-on plus A on-demand for demos.
   - **Recommended: C.** It's the cheapest way to build and show the real cloud architecture. Daily sync stays on Compose until you want to pay ~$45–50 a month for it. B-plus-A doubles the Terraform you maintain.
3. **Compute flavour.**
   - Options: Fargate x86; **Fargate ARM**; Fargate Spot ARM; EC2 t4g.
   - **Recommended: Fargate ARM**, with Spot ARM for always-on periods. ARM is 20% cheaper than x86 for the same Go binary; Spot adds ~65% off at the cost of rare restarts.
4. **Database.**
   - Options: **RDS db.t4g.micro Single-AZ**; Aurora Serverless v2 with auto-pause; Postgres on EC2.
   - **Recommended: RDS t4g.micro.** Predictable ~$14/month when up, managed backups, named in goal 7. Aurora's scale-to-zero fights the always-polling sync pattern and adds 15–30 s cold starts.
5. **ALB IPv4.**
   - Options: **dual-stack (IPv4 + IPv6)**; IPv6-only (`dualstack-without-public-ipv4`, −$7.30/month).
   - **Recommended: dual-stack** until you've confirmed every network your devices use has IPv6. A phase 3 experiment could measure it.
6. **Account plan (if creating a new account).**
   - Options: **Free plan, then upgrade deliberately**; Paid plan from day one.
   - **Recommended: Free plan**, *if* ECS, RDS and ALB are on its eligible-services list at sign-up. It's a true $0 ceiling while learning. Set a calendar reminder at month 5 and upgrade before the 6-month cutoff. Otherwise use the Paid plan with Budgets from minute one.
7. **Metadata persistence across destroy (setup C).**
   - Options: RDS final snapshot; **`pg_dump` to S3**; Aurora left paused instead of destroyed.
   - **Recommended: `pg_dump` to S3.** About $0.01/month, portable to Compose, and it rehearses the restore runbook. RDS snapshots are fine too but cost more once the instance is gone.
8. **Follow-ups to file** (not decided here):
   - per-device presigned-GET byte cap
   - clean-shutdown epoch handling (amends ADR-0003) if C is chosen
   - a pool configuration that lets idle connections drop to zero
   - the "destroy-check" script

**New user data:** none. This spike adds no data collection. The guardrails touch only AWS billing data in the owner's own account. Section 8's per-device byte metering, if adopted, would store per-device daily byte counts server-side. Flag it in that feature's plan; no privacy policy exists yet (design §11 already notes one may be needed for hosted deployments).

---

## References

**AWS Price List API** (bulk CSV offer files, us-east-1 unless noted; version = publication timestamp UTC). Offer index published 2026-11-24T16:00:01Z: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/index.json
- AmazonECS (Fargate), version 2026-11-11 23:46: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonECS/current/us-east-1/index.csv
- AmazonEC2 (t4g/t3 instances, EBS gp3 and snapshots, NAT gateway), version 2026-11-24 13:49: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/us-east-1/index.csv
- AmazonRDS (db.t4g.micro/small PostgreSQL, gp3, backup, Aurora Serverless v2 ACU-hour, Aurora storage/I/O), version 2026-11-20 22:46: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonRDS/current/us-east-1/index.csv
- AWSELB (ALB hour, LCU-hour), version 2026-11-19 22:27: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSELB/current/us-east-1/index.csv
- AmazonVPC (public IPv4 in-use/idle, interface endpoint hour/GB), version 2026-11-23 22:22: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonVPC/current/us-east-1/index.csv
- AmazonS3 (storage, Tier 1/Tier 2 requests), version 2026-11-21 17:29: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonS3/current/us-east-1/index.csv
- AWSDataTransfer (internet egress tiers, 100 GB global free row), version 2026-11-19 10:58: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSDataTransfer/current/us-east-1/index.csv
- AmazonCloudWatch (log ingestion Standard/IA, storage, metrics, alarms), version 2026-11-24 14:53: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonCloudWatch/current/us-east-1/index.csv
- AWSSecretsManager, version 2026-11-11 19:56: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSSecretsManager/current/us-east-1/index.csv
- AWSSystemsManager (Parameter Store advanced tier), version 2026-11-18 21:14: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSSystemsManager/current/us-east-1/index.csv
- AmazonECR, version 2026-11-13 16:39: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonECR/current/us-east-1/index.csv
- AmazonRoute53, `aws-other` (hosted zone, queries), version 2026-11-18 19:15: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonRoute53/current/aws-other/index.csv
- awskms (key-month, requests, global free requests), version 2026-11-08 17:23: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/awskms/current/us-east-1/index.csv
- AWSLambda (GB-second x86/ARM, free tier rows), version 2026-11-20 20:36: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSLambda/current/us-east-1/index.csv
- AmazonApiGateway (HTTP API, WebSocket messages and connection-minutes), version 2026-11-23 17:45: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonApiGateway/current/us-east-1/index.csv
- AWSBudgets, `aws-other`, version 2026-09-11 12:45: https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSBudgets/current/aws-other/index.csv
- Fargate Spot prices, from the JSON feed behind the Fargate pricing page (not part of the Price List API; marked "for use only on aws.amazon.com", values change over time), read 2026-11-24: https://dftu77xade0tc.cloudfront.net/fargate-spot-prices.json

**AWS docs and pricing pages** (fetched 2026-11-24):
- KMS pricing (prorated hourly; no charge while scheduled for deletion; rotation +$1 for first two rotations; 20,000 free requests): https://aws.amazon.com/kms/pricing/
- KMS developer guide, deleting keys (7–30 day waiting period, default 30, up to 24 h later; unusable while pending): https://docs.aws.amazon.com/kms/latest/developerguide/deleting-keys.html
- Aurora Serverless v2 auto-pause (0 ACU, 300–86,400 s timeout, ~15 s resume, 30 s+ after 24 h paused, not billed while paused, conditions that block pausing, 20-minute stay-awake after maintenance): https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/aurora-serverless-v2-auto-pause.html
- ECS Fargate capacity providers (Spot 2-minute warning, single-task service waits for capacity): https://docs.aws.amazon.com/AmazonECS/latest/developerguide/fargate-capacity-providers.html
- Fargate pricing (Spot up to 70% off, per-second with 1-minute minimum, 20 GB ephemeral storage, smallest 0.25 vCPU / 0.5 GB): https://aws.amazon.com/fargate/pricing/
- ELB pricing (LCU dimensions, IPv4 charges apply to load balancers, legacy 750 h ALB free tier, credits within 12 months): https://aws.amazon.com/elasticloadbalancing/pricing/
- VPC pricing (public IPv4 $0.005/h in use and idle, per-second billing with 60 s minimum, gateway endpoints free): https://aws.amazon.com/vpc/pricing/
- CloudWatch pricing (always-free: 5 GB logs, 10 metrics, 10 alarms, 3 dashboards, 1M API requests; Container Insights extra): https://aws.amazon.com/cloudwatch/pricing/
- Billing guide, Free Tier overview: https://docs.aws.amazon.com/awsaccountbilling/latest/aboutv2/free-tier.html
- Billing guide, choosing a plan (Free vs Paid, 6 months, closure, 90-day retention, automatic upgrade triggers): https://docs.aws.amazon.com/awsaccountbilling/latest/aboutv2/free-tier-plans.html
- AWS Free Tier page (up to $200 over 6 months, 30+ always-free services, plan comparison): https://aws.amazon.com/free/
- AWS Free Tier FAQs (credits for new customers only; ineligible if you previously had an account; 90-day upgrade window; conflicting 6 vs 12-month credit expiry; mention of an "October 2026 sign-up change"): https://aws.amazon.com/free/free-tier-faqs/
- Legacy Free Tier FAQs (accounts created before 2025-07-15; 12 months from activation; then pay-as-you-go): https://aws.amazon.com/free/legacy/free-tier-faqs
- EC2 free tier usage (eligible instance types on the new plans, credit-based limit, 6-month duration): https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-free-tier-usage.html

**Announcements** (dates as posted):
- AWS Free Tier credits and 6-month free plan, What's New, posted 2025-07-16: https://aws.amazon.com/about-aws/whats-new/2025/07/aws-free-tier-credits-month-free-plan
- AWS News Blog on the same change (body not retrievable by fetch; headline only): https://aws.amazon.com/blogs/aws/aws-free-tier-update-new-customers-can-get-started-and-explore-aws-with-up-to-200-in-credits
- Free tier adds 750 h/month of public IPv4 for EC2 (legacy 12-month offer), posted 2024-02-01 (*search summary, not fetched*): https://aws.amazon.com/about-aws/whats-new/2024/02/aws-free-tier-750-hours-free-public-ipv4-addresses
- Public IPv4 charge announcement, AWS News Blog, 2023 (*body not retrievable; effective date 2024-02-01 confirmed via the What's New above*): https://aws.amazon.com/blogs/aws/new-aws-public-ipv4-address-charge-public-ip-insights/
- ALB IPv6-only for internet clients (`dualstack-without-public-ipv4`), What's New, posted 2024-05-16 (*search summary*): https://aws.amazon.com/about-aws/whats-new/2024/05/application-load-balancer-ipv6-internet-clients
- ECS Fargate Spot on Graviton, What's New, September 2024 (*search summary*): https://aws.amazon.com/about-aws/whats-new/2024/09/amazon-ecs-graviton-based-spot-compute-fargate

**Not verified (prior knowledge or inference; check before relying on it):**
- RDS automated-backup free allowance equals provisioned storage; RDS gp3 20 GB minimum; RDS can be stopped for at most 7 days; ~80 max connections on db.t4g.micro; whether an RDS snapshot bills used or allocated size after the instance is deleted.
- An internet-facing ALB needs at least two AZs (so two public IPv4 addresses); ALB idle timeout default 60 s, max 4,000 s; ACM public certificates free.
- API Gateway HTTP API ~30 s integration timeout; WebSocket 2-hour connection limit and 10-minute idle timeout; Lambda $0.20 per million requests.
- Route 53 hosted zones deleted within 12 hours aren't billed (otherwise a full month).
- 1 ACU ≈ 2 GiB memory.
- Whether ECS/Fargate, RDS and ALB are on the Free plan's eligible-services list; the "October 2026 sign-up change" mentioned in the FAQ.
- Billing data refresh frequency; Cost Anomaly Detection being free; GuardDuty/Security Hub/Config trial behaviour.
- Fargate ARM not available in `use1-az3`; Fargate tasks in IPv6-only subnets.
- Popular Terraform VPC modules enabling NAT by default.
- Whether S3 bills failed (4xx) requests, carried over from spike 07.
