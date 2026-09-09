# DEPLOYMENT (AWS V1)

Status: Terraform under `infra/terraform` is written and `terraform validate` passes for every environment (re-verified 2026-09-09, and `terraform fmt -check -recursive` is clean). **No plan or apply has been run against AWS.** Nothing in this document claims a deployed environment. Section 13 states exactly what has been verified.

**Correction, 2026-09-09.** This line previously said "no account, credentials or OIDC trust exist yet". An AWS account **does** exist — `049286562577` — and the operator machine has the AWS CLI configured against it. What is missing is an authenticated session and, more importantly, a non-root role: the configured identity is `arn:aws:iam::049286562577:root`, and Terraform must not run as root. OIDC trust and the GitHub repository are still absent.

**Read `DEPLOYMENT_GAP_ANALYSIS.md` before planning an apply.** It records what the API binary actually depends on, which is materially less than this document provisions: `cmd/api` links no ClickHouse, Temporal or Redpanda client at all, and chooses an in-memory rate-limit store over Redis. Its runtime dependencies are PostgreSQL and, for the Stripe webhook, S3. That changes what a first deployment has to cost.

Companions: `docs/architecture/SYSTEM.md` section 2 (binaries and credential scopes), `docs/security/SECURITY.md` section 10, `docs/operations/BACKUP_RESTORE.md`, `docs/compliance-gates/PRODUCTION_GATES.md`, `.github/workflows/release.yml`.

The repository has **nine** commands. `SYSTEM.md` section 2 still lists eight: it predates `cmd/relay-worker` and is owned elsewhere. Where the two disagree about the binary set, this document and `infra/terraform/environments/*/variables.tf` (whose `service_sizing` validation enumerates them) are current.

## 1. Layout

```
infra/terraform/
  modules/
    kms symmetric keys (rds, s3/ecr, secrets, logs/sns) + ECC_NIST_P256 SIGN_VERIFY audit key
    network VPC, 3 AZ public/app/data subnets, NAT, VPC endpoints, security groups, flow logs
    s3-evidence raw-events, provider-evidence, audit-archive (Object Lock COMPLIANCE), access-log bucket
    rds PostgreSQL 16 Multi-AZ, PITR, rds.force_ssl, bootstrap/roles.sql (no null_resource)
    redis ElastiCache replication group, TLS + at-rest encryption, AUTH token
    secrets one Secrets Manager entry per aws-sm:// reference + the least-privilege reader matrix
    ecs-cluster Fargate cluster, immutable KMS-encrypted ECR repositories
    ecs-service one task role + execution role + hardened task definition per binary;
                   Application Auto Scaling for the api (and only the api)
    app-config CP_* environment map mirroring internal/config/load.go (no resources)
    waf-edge ALB (TLS 1.2/1.3), WAFv2 managed rules + rate limit, optional CloudFront;
                   readiness target group on /v1/readyz + liveness target group on /v1/healthz
    observability SNS SEV1/SEV2, CloudWatch alarms, dashboard skeleton
    iam-deploy GitHub OIDC provider + deploy role (ECR push, ECS deploy, run migrate)
  environments/{dev,staging,prod}/ main.tf, variables.tf, outputs.tf, backend.tf, backend.hcl.example,
                                     terraform.tfvars.example, versions.tf
```

Terraform `>= 1.9, < 2.0`; `hashicorp/aws ~> 6.0` (6.63.0 was current on the registry when pinned); `hashicorp/random ~> 3.6`.

Environment differences are deliberate and mostly fixed in code rather than variables:

| | dev | staging | prod |
|---|---|---|---|
| NAT | single | per AZ | per AZ |
| RDS Multi-AZ / deletion protection | variable (default off) | fixed on | fixed on |
| Fake provider modes | allowed (config accepts fake in DEV) | refused by `validation` | refused by `validation` |
| Audit Object Lock retention | 30 d default | 90 d default | `>= 2555 d` enforced |
| OTLP | ADOT sidecar, plaintext localhost | sidecar allowed | sidecar forbidden; TLS collector required (`NO_INSECURE_OTLP`) |
| Redis nodes | 1 | `>= 2` | `>= 2` (default 3) |
| `force_destroy`, `apply_immediately` | on | off | off |
| CloudFront | off | off | on by default |

## 2. Prerequisites

1. **Accounts.** Three AWS accounts (dev, staging, prod). Staging is never the production database with test users (PART 144): separate account, database, secrets, provider configuration and capability state. Each environment's provider block carries `allowed_account_ids = [var.aws_account_id]`, so a stack cannot run against the wrong account.
2. **Terraform state bootstrap (manual, once per account).** Terraform cannot create the bucket that stores its own state. Create by hand, in this order: a KMS key for state; an S3 bucket `cp-<env>-terraform-state-<account>-<region>` with versioning, SSE-KMS with that key, public access blocked and a TLS-only bucket policy; an IAM policy limiting bucket access to the operators' role. Locking uses Terraform's native S3 lock file (`use_lockfile = true`), so no DynamoDB table is needed; if you migrate an older state that used one, add `dynamodb_table` to `backend.hcl` temporarily. Backend blocks cannot use variables, so copy `backend.hcl.example` to `backend.hcl` and run `terraform init -backend-config=backend.hcl`.
3. **Certificates.** A regional ACM certificate for the ALB; for prod with CloudFront, a second certificate in `us-east-1` and a DNS name (`cloudfront_origin_domain_name`) that resolves to the ALB and is covered by the ALB certificate.
4. **GitHub OIDC.** `github_org`/`github_repo` in tfvars. The `iam-deploy` module creates the provider (`token.actions.githubusercontent.com`) and a role trusting `repo:<org>/<repo>:ref:refs/heads/main` and `refs/tags/v*` only. Its ARN becomes the `AWS_RELEASE_ROLE_ARN` repository variable that `release.yml`'s commented ECR block expects. There are no static AWS keys anywhere (PART 99/143). The repository itself does not exist yet (SB-004).
5. **Managed services.** Redpanda, ClickHouse and Temporal are external (EB-014); their TLS endpoints go into tfvars, their credentials into Secrets Manager.
6. **Tooling on the operator machine.** Terraform 1.15+, trivy (`bin/trivy.exe`), the AWS CLI with the operator role.

## 3. First-time bootstrap order

Terraform resolves the graph, but the first apply of an environment is best done in stages so failures are local and the role bootstrap can run between them:

1. `terraform apply -target=module.kms` (all keys; the audit signing key's `kms:Sign` statement is empty until the audit-worker role exists and is completed in step 5).
2. State bucket already exists (section 2). Nothing to apply.
3. `-target=module.network` (VPC, endpoints, security groups, flow logs).
4. `-target=module.rds -target=module.redis -target=module.s3_evidence -target=module.ecs_cluster` (RDS master password is generated and rotated by RDS: `manage_master_user_password`, never in state; the Redis AUTH token is generated by Terraform and published to `cp/<env>/redis/url`).
5. Full `terraform apply` (secrets containers with resource policies, task roles, task definitions, services, ALB/WAF, alarms, deploy role). Services will start crash-looping until steps 6-8 are complete; that is expected and the circuit breaker keeps them at the previous (non-existent) revision.
6. **Roles bootstrap job.** Populate the four role passwords (`cp/<env>/database/roles/cp_{migrate,app,readonly,ops}-password`) as the secrets admin, then run the one-shot task: `aws ecs run-task --cluster cp-<env> --task-definition cp-<env>-db-bootstrap --launch-type FARGATE --network-configuration "awsvpcConfiguration={subnets=[<private app subnets>],securityGroups=[<worker sg>],assignPublicIp=DISABLED}"`. It runs `modules/rds/bootstrap/roles.sql` with `psql` as the RDS master user (credentials injected from the RDS-managed secret via the execution role) and creates `cp_migrate`, `cp_app`, `cp_readonly`, `cp_ops` with the D-016 policy: `cp_app` has **no** default table privileges, only sequence usage; every migration grants per table. Idempotent; re-run to rotate role passwords.
7. **Secrets population.** Write the remaining values as the secrets admin: the three database URLs (`postgres://cp_app:...@<rds address>:5432/controlplane?sslmode=verify-full&sslrootcert=...`), Redpanda SASL, ClickHouse, the OIDC client secret, provider API keys and webhook secrets. Each secret's resource policy allows `GetSecretValue` only to the task roles in the module's matrix and denies everyone else except `secret_admin_principal_arns`, so a wrongly targeted secret fails closed at runtime. `terraform.tfvars.example` files carry no secret values; `cloudfront_origin_secret` is passed via `TF_VAR_`.
8. Migration (section 4), then services become healthy.

## 4. Migrations

Schema changes run **before** the new api/workers, as a one-shot task under the migration role only (`cmd/migrate` holds `database/migrate-url` and nothing else):

```
aws ecs run-task --cluster cp-<env> --task-definition cp-<env>-migrate:<rev> --launch-type FARGATE \
  --network-configuration "awsvpcConfiguration={subnets=[...],securityGroups=[<worker sg>],assignPublicIp=DISABLED}"
aws ecs wait tasks-stopped... && aws ecs describe-tasks... --query 'tasks[0].containers[0].exitCode' # must be 0
```

The task definition's command is `up`; `verify` (embedded checksums vs applied rows) and `status` are run the same way with `--overrides`. Migrations are forward-only above the protected version; a checksum mismatch aborts the rollout. The deploy role may `RunTask` only this family on this cluster and read only its log group.

## 5. Rollout

1. `release.yml` builds one image per `cmd/<name>`, scans (trivy, HIGH/CRITICAL), pushes, signs (cosign keyless) and attests; the git SHA is the only tag (`image_tag`).
2. `terraform plan -var image_tag=<sha>` in the environment directory; review that only task definition revisions change.
3. `terraform apply`. The migrate task definition revision is registered; run section 4 with it.
4. Services roll with `deployment_minimum_healthy_percent = 100`, `maximum = 200`. The api is health-checked by the ALB on **`/v1/readyz`** and, on a second target group reached by a listener rule, on **`/v1/healthz`**. The prefix matters: `internal/httpapi` mounts every route under `BasePath = "/v1"`, so an unprefixed path answers 404 and no target ever enters service. Readiness fails when Postgres or Redis is unreachable, which is the correct response (stop taking traffic); liveness is answered by the process alone, so it fails only when the process is wedged. They are separate alarms because they call for separate actions. Workers have no container health command (distroless images ship no shell); their signal is `RunningTaskCount` and their application metrics.
5. The api is the only autoscaled service (`api_autoscaling`): two target-tracking policies, average CPU and `ALBRequestCountPerTarget`, scaling out on a 60 s cooldown and in on 300 s. Once `max_capacity` is set, Application Auto Scaling owns the count and the service ignores `desired_count`, so `service_sizing.api.desired_count` is only the count the first apply creates; the floor afterwards is `api_autoscaling.min_capacity`. Workers are not autoscaled: they claim their own work from Postgres, so their throughput is a sizing decision rather than a load signal, and `market-ingest-worker` and `audit-worker` are deliberately single-task.
6. Order within the apply is not controllable; if a release changes a message contract, deploy in two steps (consumers first with `-target=module.services["<worker>"]`).
7. Canary (PART 206): keep `desired_count` small on the first prod apply of a new capability and watch the dashboard for one reconciliation cycle before scaling.
8. **If a release changes the shape of a command's response, in-flight idempotency records outlive it.** `runCommand` stores the command's value as JSON and reads it back into whatever type the new build asks for, and records live `DefaultIdempotencyTTL` = 24 h. A record written by the old build therefore unmarshals into the new type with every added field zero — and a zero-filled response is one a client would act on. The quote route carries an explicit guard for exactly this (a replayed quote with no id is answered `CONFLICT`, telling the caller to retry with a new key) because its response gained `asset_decimals`. Any future change of this kind needs the same guard or a 24-hour quiet period; a shape change with neither is a silent wrong answer, not a failure anybody would see.

## 6. Rollback

- Task-level: the deployment circuit breaker rolls a failing service back to the last steady revision automatically.
- Release-level: re-apply with the previous `image_tag`. Images are immutable and retained (30 per repository), so any prior SHA can be redeployed.
- Schema: migrations above the protected version are forward-only; roll forward with a corrective migration. Data-level recovery is `BACKUP_RESTORE.md` section 3 (restore to a **new** instance, never overwrite).
- Never roll back by editing the database, disabling a gate row, or bypassing the migrate role.

## 7. Configuration and validation at start

`modules/app-config` assembles every `CP_*` variable from typed inputs; secrets are `aws-sm://cp/<env>/<name>` references resolved at runtime with the task role. Values that `config.Validate` requires in STAGING/PROD are fixed in the module, not variables: `CP_DATABASE_REQUIRE_TLS`, `CP_REDIS_REQUIRE_TLS`, `CP_REDPANDA_REQUIRE_TLS`, `CP_CLICKHOUSE_REQUIRE_TLS`, `CP_TEMPORAL_REQUIRE_TLS`, `CP_ARCHIVE_OBJECT_LOCK_REQUIRED`, `CP_AUTH_COOKIE_SECURE` are `true`; `CP_AUTH_MODE=oidc`; `CP_AUTH_DEBUG_ENABLED` and `CP_SEED_ENABLED` are `false`; `CP_KMS_AUDIT_SIGNING_KEY_ID` is the audit key ARN; `CP_ARCHIVE_ENDPOINT` is the regional S3 endpoint (an empty required variable is a startup error, `load.go`). Prod variables additionally refuse fake provider modes, insecure OTLP, non-https URLs, CORS wildcards, retention below seven years and an unpinned bootstrap image. A binary that fails `config.Validate` exits before opening any connection, and the circuit breaker keeps the previous revision.

Telemetry: dev/staging run the ADOT sidecar on `127.0.0.1:4317` (plaintext, accepted outside PROD). In PROD `NO_INSECURE_OTLP` rejects that, so `otlp_endpoint` must be a TLS collector; the sidecar flag is validated to `false`.

## 8. Capability gates stay DISABLED after deploy (PART 244)

Nothing in this stack can activate a capability. **CORRECTION (F-51):** `gates.Bootstrap` does NOT run at first start — it has no caller in `cmd/`, and the only thing that invokes it is `scripts/gateceremony`. A fresh deployment has no gate rows at all. That is still fail-closed, because an absent row is INACTIVE and migration 00701 refuses a gate born in any state but DISABLED, but a reader looking for those rows after a deploy will not find them. `migrations/00701` and the readiness report both say so; this runbook said the opposite; activation requires the deployment configuration **and** a persisted, dual-approved, in-window gate row with evidence hashes (PRODUCTION_GATES.md section 1). Provider modes in tfvars only decide which adapters are wired; `LIVE_FUNDING`, `LIVE_MANUAL_TRADING`, `LIVE_AGENT_TRADING` and `WITHDRAWALS` remain DISABLED until the gate procedure completes. Terraform grants no principal `gate:approve`; that is an application permission under dual control.

## 9. Credential scopes encoded in IAM (SYSTEM.md section 2)

| Role | Secrets it may resolve | Other |
|---|---|---|
| api | db app URL, Redis, bus, ClickHouse, OIDC client secret, funding key + webhook, wallet webhook, notification | evidence bucket RW; **no** `kms:Sign`, **no** audit bucket |
| relay-worker | db app URL, bus SASL | **nothing**: no bucket, no key, no provider credential |
| execution-worker | db app, Redis, bus, execution, **signing**, wallet, chain observers | evidence RW |
| reconciliation-worker | db app, Redis, bus, execution, chain observers | evidence RW |
| market-ingest-worker | db app, bus, ClickHouse, market-data | raw-events RW |
| agent-worker | db app, Redis, bus, model | evidence RW; nothing else |
| workflow-worker | db app, Redis, bus, funding, wallet, chain observer, notification | evidence RW |
| audit-worker | db **readonly** | audit-archive write, `kms:Sign` on the audit key (only principal with Sign) |
| migrate | db migrate URL | nothing |
| db-bootstrap (exec role) | RDS master secret, four role passwords | nothing |

The matrix is data in `modules/secrets/main.tf` guarded by `check` assertions (api never signing/wallet; agent-worker only db/redis/bus/model; signing only execution-worker; relay-worker only db app + bus; migrate only its own URL). The audit key policy grants `kms:Sign` to the audit-worker task role alone and `kms:Verify` to `auditor_principal_arns`; the account root has administration only, so no identity policy can add signers.

Two mechanisms, not one. The **identity** side is the task role's own policy, which lists nothing beyond that binary's needs (`task_policies` in each environment's `main.tf`; `relay-worker` and `migrate` are empty). The **resource** side is each secret's policy, which allows only the roles in the matrix and carries an explicit `Deny` to every other principal, so an over-broad identity policy still cannot read it. A binary that reaches for a credential outside its row gets `AccessDeniedException` the first time the code needs the value, which is the intended failure: the boundary the Go packages enforce is the same boundary IAM enforces.

**No secret value is ever in a task definition.** Task environments carry `aws-sm://cp/<env>/<name>` references only; `internal/config` resolves them at runtime with the task role, so no value is in the repository, the image, the task definition JSON, `describe-task-definition` output or a log line. The one place ECS itself injects values (`secrets`/`valueFrom`, execution role) is the `db-bootstrap` one-shot task, which needs `psql` environment variables and holds nothing but the RDS master secret and the four role passwords.

**Every worker needs its subcommand.** The runtime entrypoint is the bare binary; `relay-worker` with no arguments prints usage and exits 2, which the circuit breaker reads as a crash loop. `service_command` in each environment supplies `run` for all seven workers and `up` for `migrate`; `cmd/api` is the only binary that takes none.

## 10. Alerting: what pages, and what the page means

Two SNS topics, SEV1 and SEV2, with email and HTTPS (pager) subscriptions per environment. An alarm exists only if an engineer would do something different because it fired; an alarm nobody acts on trains people to ignore the pager, so the list below is deliberately short and every row names the action.

**Financial correctness**

| Alarm | Sev | Signal | What it means when it fires |
|---|---|---|---|
| `ledger-posting-errors` | 1 | `ledger_posting_errors` >= 1 | A posting was rejected or failed. Candidate ledger-integrity incident: `docs/runbooks/ledger-mismatch.md`, do not retry by hand. |
| `reconciliation-mismatches` | 1 | `reconciliation_mismatches` >= 1 | External truth disagrees with our books. New risk is already blocked by the reconciliation gate. |
| `oldest-unresolved-mismatch-sev2` / `-sev1` | 2 / 1 | `oldest_unresolved_mismatch` > 900 s / 3600 s | A mismatch has been open that long. The SEV1 threshold is the point at which "still investigating" stops being an acceptable answer while new risk stays blocked. |
| `unknown-submissions-elevated` | 2 | `unknown_submissions` >= 3 / 5 min | Submissions whose outcome we do not know. Each is money in an undetermined state; `docs/runbooks/submission-unknown.md`. |
| `unknown-submission-rate-high` | 1 | `unknown/submissions` > 5% over 5 min, min 20 submissions | Not an isolated timeout but a systematic loss of outcome knowledge: the provider or the chain observer has stopped answering. |

**Relay (transactional outbox)**

Relay lag is the number that says whether the rest of the platform is looking at the present. Events are committed into `outbox_events` with the state change they describe; until `cmd/relay-worker` publishes them, the SSE stream, the execution worker's wake-ups and every event-derived read model are that far behind. Money stays correct — that is what the outbox is for — but decisions taken on stale reads are not.

| Alarm | Sev | Signal | What it means when it fires |
|---|---|---|---|
| `relay-lag-sev2` | 2 | `outbox_oldest_unpublished_age` > `relay_lag_thresholds.sev2_seconds` for 3 min | Read models are behind by at least that much. Check `RunningTaskCount` for `relay-worker` and the publish-failure alarm before touching the outbox. `treat_missing_data = breaching`: no sample at all means no relay is running. |
| `relay-lag-sev1` | 1 | same gauge over `sev1_seconds` | The event stream has effectively stopped. Treat every event-derived read as stale until it drains. |
| `relay-publish-failures` | 2 | `outbox_publish_failures` >= 20 / 5 min | The bus is rejecting publishes. Rows stay unpublished and retryable, so nothing is lost. Usual causes: Redpanda unreachable, SASL credentials rotated without a redeploy, a topic that no longer accepts the payload. `docs/runbooks/redpanda-outage.md`. |
| `relay-blocked-partitions` | 2 | `outbox_blocked_partitions` >= 1 for 10 min | The outbox's dead-letter signal. Per-partition order is preserved, so a partition whose head row keeps failing publishes nothing behind it. `relay-worker status -json` names the partitions. |
| `relay-unregistered-topics` | 2 | `outbox_unregistered_topic_events` >= 1 | A producer is ahead of the event registry, or a rolling deploy is mid-flight with an incompatible contract. The event is still published, never dropped. |

**Providers and dependencies**

`internal/provider` tracks HEALTHY/DEGRADED/UNHEALTHY per adapter and serves it on `/v1/admin/providers`, but publishes no metric for the state itself, so degradation is alarmed on its consequences rather than on a number that does not exist yet.

| Alarm | Sev | Signal | What it means when it fires |
|---|---|---|---|
| `execution-failure-rate-high` | 2 | `100 * failures / attempts` > 25% over 5 min, min 20 attempts | Read as provider degradation until proven otherwise. Open `/v1/admin/providers`, find the adapter that went DEGRADED, decide whether its kill switch should be pulled. The minimum-attempts guard is what stops two failures out of three overnight attempts from paging anyone. |
| `alb-target-5xx`, `alb-elb-5xx`, `alb-p99-latency` | 2 | ALB metrics | The api itself is failing or slow. |
| `alb-unhealthy-targets` | 2 | readiness group `UnHealthyHostCount` | Targets are failing `/v1/readyz`: a dependency (Postgres, Redis) is unreachable. |
| `api-liveness-unhealthy` | 1 | liveness group `UnHealthyHostCount` | Targets are failing `/v1/healthz`, which the process answers without touching any dependency. The container is wedged or dying — a different problem, and a different fix, from the row above. |

**Capacity**

| Alarm | Sev | Signal | What it means when it fires |
|---|---|---|---|
| `rds-connections-high` | 2 | `DatabaseConnections` >= 60% of the `cp_app` CONNECTION LIMIT | The binding constraint is the role's limit, not the instance's `max_connections`. The ceiling is `CP_DATABASE_MAX_CONNS` x task count, so a service scaling out, or a pool leak, gets here first. |
| `rds-connections-critical` | 1 | >= 85% of the same limit | New tasks cannot open a pool and the migrate job cannot run — which presents as a deploy failure, not a database one. Recovery is to scale the noisy service in, not to raise the limit under pressure. |
| `rds-free-storage-low` | 1 | free storage below threshold | Writes will stop. |
| `rds-cpu-high`, `rds-freeable-memory-low`, `rds-replica-lag-*`, `redis-*`, `ecs-*-cpu-high`, `ecs-*-memory-high` | 2 | infrastructure | Ordinary saturation signals. |
| `ecs-<service>-no-running-tasks` | 2 | `RunningTaskCount` < 1 | A service has no task: crash loop, or the circuit breaker rolled it back. Read `/ecs/cp-<env>/<binary>`. |

**Control actions**

| Alarm | Sev | Signal | What it means when it fires |
|---|---|---|---|
| `kill-switch-changed` | 1 | log metric filter on the api access log: `POST /v1/admin/kill-switches` with a 2xx | A kill switch was activated or released. Activation means new risk of that kind has stopped and someone must decide what happens to the work already in flight; release means it has been allowed again, and the reason is already in the audit record. The authoritative record is the audit event in Postgres (PART 52/93); this filter exists so on-call learns in seconds instead of finding it in a query later. |

Alarm names are `cp-<env>-<name>`. `aws cloudwatch describe-alarms --alarm-name-prefix cp-<env>- --state-value ALARM` lists what is currently firing. Thresholds are variables in `modules/observability/variables.tf` and are set per environment; dev is deliberately looser than prod (`relay_lag_thresholds` in `terraform.tfvars.example`).

Two alarms depend on data this stack does not produce and will sit in `INSUFFICIENT_DATA` until the collector is publishing: every application-metric alarm needs the OTLP exporter's namespace and dimensions to match `custom_metric_namespace` / `custom_metric_dimensions` exactly, which cannot be checked without a running collector (section 13).

## 11. External managed services

Redpanda, ClickHouse and Temporal are managed services outside this Terraform (PART 137, BLOCKERS EB-014). Terraform holds their endpoints, and Secrets Manager holds their credentials; nothing here can create or configure them, so the configuration each one needs is written down rather than implied.

**Redpanda (event bus).** One cluster per environment, never shared with another environment (PART 144). TLS is mandatory: `CP_REDPANDA_REQUIRE_TLS` is fixed `true` in `modules/app-config` and `config.Validate` refuses plaintext outside LOCAL. SASL/SCRAM user per environment; `SCRAM-SHA-256` in dev, `SCRAM-SHA-512` in prod (`redpanda.sasl_mechanism`). Credentials go into `cp/<env>/redpanda/sasl-{username,password}`, readable by every binary that touches the bus and by no one else. The producer identity needs write on the domain topics and the consumer identity needs read plus consumer-group management; the relay worker is a producer only. Retention must exceed the longest consumer outage the platform is expected to survive — a topic that expires an event the platform has not consumed turns a recoverable outage into a reconciliation problem. Network: the workers reach it over the internet through the NAT gateways, so the cluster must allow the environment's NAT egress IPs. `docs/runbooks/redpanda-outage.md` is the failure procedure; `docs/adr/0005-redpanda-event-stream.md` is why.

**ClickHouse (analytics).** One database per environment, `CP_CLICKHOUSE_DATABASE` (default `controlplane`), TLS mandatory, credentials in `cp/<env>/clickhouse/{username,password}`. Only `cmd/market-ingest-worker` writes and only `cmd/api` reads: the matrix grants those two and nobody else. The DDL is applied by the binary itself (`market-ingest-worker schema`), not by Terraform. ClickHouse is never authoritative for a balance, an available amount or an eligibility decision (PART 118, ADR-0006); if it is unavailable, analytics degrade and money paths continue, which is what `docs/runbooks/clickhouse-outage.md` describes. Do not grant the ClickHouse user access to anything in Postgres.

**Temporal (workflow orchestration).** One namespace per environment, `CP_TEMPORAL_NAMESPACE`, task queues prefixed `CP_TEMPORAL_TASK_QUEUE_PREFIX`. TLS mandatory. Only `cmd/workflow-worker` connects; `workflow-worker check` validates configuration and connectivity without hosting anything and is the right smoke test after a namespace change. Workflow state is never financial truth (PART 116, ADR-0004): Postgres records balances, deposits and reconciliation records, and a workflow only decides what to do next, which is why the worker can be restarted, redeployed or lost without anyone's money changing. Namespace retention should cover at least the longest funding or escalation workflow. `docs/runbooks/temporal-outage.md`.

**Certificates and DNS.** ACM for the ALB (regional) and, for prod with CloudFront, a second certificate in `us-east-1`; `cloudfront_origin_domain_name` must resolve to the ALB and be covered by the ALB certificate. Neither the DNS zone nor the certificate validation records are in this stack.

**Identity provider.** OIDC issuer, client id and redirect URL in tfvars; the client secret in `cp/<env>/auth/oidc-client-secret`, readable by the api alone. `CP_AUTH_MODE` is fixed to `oidc` and `CP_AUTH_DEBUG_ENABLED` to `false` on AWS: the development identity picker cannot be mounted outside LOCAL/TEST/DEV, and `config.Validate` refuses the attempt.

**Pager.** SEV1 and SEV2 SNS topics take HTTPS endpoints (PagerDuty/Opsgenie integration URLs) in `sev1_https_endpoints` / `sev2_https_endpoints`. An environment with no subscription has alarms that fire into nothing; check `aws sns list-subscriptions-by-topic` after the first apply.

## 12. Day-2 operations

**Verifying a rollout.** After `apply`:

```
aws ecs describe-services --cluster cp-<env> \
  --services $(terraform output -json service_names | jq -r '.[]' | tr '\n' ' ') \
  --query 'services[].{name:serviceName,running:runningCount,desired:desiredCount,rollout:deployments[0].rolloutState}'
aws cloudwatch describe-alarms --alarm-name-prefix cp-<env>- --state-value ALARM
curl -fsS https://<public_base_url>/v1/healthz # the process is up
curl -fsS https://<public_base_url>/v1/readyz # its dependencies are reachable
```

A service whose `rolloutState` is `FAILED` was rolled back by the circuit breaker; read `/ecs/cp-<env>/<binary>` in CloudWatch Logs. The usual signatures: `config:... NO_FAKE_PROVIDERS` or another `Rule` name (a tfvars value violates `config.Validate`; fix the variable, not the code); `AccessDeniedException... secretsmanager:GetSecretValue` (the binary resolved a secret outside its matrix row; that is the intended fail-closed behaviour, so check which reference it needed and whether the matrix in `modules/secrets/main.tf` should change under review); ALB targets `unhealthy` with the api logging nothing (`/v1/readyz` depends on database and Redis reachability: check the `db` and `redis` security groups and that `sslmode=verify-full` can find its CA); a worker task that starts and exits 2 at once was given no subcommand (`service_command`).

**Rotation.**

- RDS master password: rotated by RDS (`manage_master_user_password`); nothing to do. The db-bootstrap task always reads the current version.
- Application role passwords: write new values to `cp/<env>/database/roles/*-password`, re-run the db-bootstrap task (idempotent `ALTER ROLE... PASSWORD`), then update the three URL secrets; services pick the new URL up on their next start, so roll them (`terraform apply` with a no-op change or `aws ecs update-service --force-new-deployment`).
- Redis AUTH token: taint `module.redis.random_password.auth_token` and apply; ElastiCache applies it with `ROTATE` (old and new tokens both valid until the next `SET`), and Terraform rewrites `redis/url`.
- Provider keys, OIDC secret, webhook secrets: `secret_rotation` in tfvars attaches a rotation Lambda per secret name (`aws_secretsmanager_secret_rotation`); the Lambda role must be listed in `secret_admin_principal_arns` or the resource policy denies it. No rotation Lambdas are written yet.
- Audit signing key: asymmetric keys cannot auto-rotate. Create a new key, switch `CP_KMS_AUDIT_SIGNING_KEY_ID`, keep the old key enabled for verification for the whole retention period; checkpoints record the key id.

**Retention and Object Lock.** The audit archive is COMPLIANCE-locked: no principal, root included, can delete a locked version or shorten the retention, and the bucket cannot be deleted while it holds locked objects. Dev uses 30 days so a torn-down environment frees itself within a month; prod uses 2555 days (seven years) and the bucket policy additionally denies `PutBucketObjectLockConfiguration`, `PutBucketVersioning`, `BypassGovernanceRetention` and `DeleteBucket` to everyone. Raw events expire per `CP_RETENTION_RAW_MARKET_DATA_DAYS` after a Glacier-IR transition; provider evidence transitions to Glacier at 90 days and expires per `CP_RETENTION_FINANCIAL_RECORD_DAYS`. CloudWatch log retention follows `CP_RETENTION_OPERATIONAL_LOG_DAYS`. Changing a retention variable changes lifecycle rules only; it never removes a lock.

**Scaling.** `service_sizing` per binary, plus `api_autoscaling` for the api alone (section 5). Running more than one `execution-worker` or `workflow-worker` is safe only because settlement and reservations serialize on Postgres row locks and idempotency keys (SYSTEM.md section 7); `market-ingest-worker` and `audit-worker` are single-task by design (checkpoints, chain heads).

`relay-worker` runs several replicas deliberately, and prod validates a floor of two. They coordinate entirely through Postgres with no lease, heartbeat or election: `SELECT... FOR UPDATE SKIP LOCKED` gives each row to exactly one instance, and since D-036 every claimed row -- not only each partition's oldest -- is checked against the rows another instance holds, so per-partition order survives a partition being split across instances mid-drain. `CP_RELAY_WORKER_EXCLUSIVE` (the advisory-lock single publisher) is off by default and stays off: since D-037 it protects nothing, caps drain rate at one instance, and fails over only when Postgres reaps the dead holder's connection, which is minutes of growing lag. It is an operator switch for a deliberate single publisher, not a safety control, and the `relay_worker` variable validates it to `false` so enabling it needs a decision-register entry first. One relay replica is a single point of staleness for every read model in the platform.

Never use FARGATE_SPOT for money-path workers; the cluster's capacity provider strategy is FARGATE only.

**State.** One state file per environment in its own account's bucket. `terraform state` surgery is a dual-controlled operation: record it in the incident log like a manual database change.

## 13. What has been validated (honest section)

Verified on 2026-09-06 on the development host, without AWS credentials. Literal output:

```
$ terraform version
Terraform v1.15.8
on windows_amd64

$ terraform fmt -check -recursive # in infra/terraform
exit=0

$ (cd environments/dev && terraform validate) Success! The configuration is valid. exit=0
$ (cd environments/staging && terraform validate) Success! The configuration is valid. exit=0
$ (cd environments/prod && terraform validate) Success! The configuration is valid. exit=0

$ trivy config --severity HIGH,CRITICAL --exit-code 1 infra/ # make iac-scan, trivy 0.74.0
 terraform/environments/dev terraform 0
 terraform/environments/prod terraform 0
 terraform/environments/staging terraform 0
 terraform/modules/rds/main.tf terraform 0
 terraform/modules/waf-edge/main.tf terraform 0
exit=0
```

Providers `hashicorp/aws ~> 6.0` (6.63.0) and `hashicorp/random ~> 3.6`, pinned in each environment's `.terraform.lock.hcl`. Three inline `#trivy:ignore` suppressions, each with its reason in the source: `AVD-AWS-0053` (the ALB is intentionally public), `AVD-AWS-0132` and `AVD-AWS-0089` (ELB access-log delivery supports SSE-S3 only, and the log bucket does not log itself). Suppressions written for `AVD-AWS-0104/0107` (tcp/443 egress and ALB ingress) and `AVD-AWS-0057` (actions without resource-level permissions) document intent but were not needed at HIGH/CRITICAL.

**Not verified, because EB-012 blocks it — no `terraform plan` or `apply` has ever run against AWS.** `validate` checks syntax, types, references and variable validation rules; it does not talk to an API. Specifically unverified:

- Everything the AWS API decides at plan/apply time: parameter names accepted by RDS and ElastiCache, IAM policy size limits, WAF managed rule group availability in the region, ALB access-log delivery in the region, the ADOT image tag `v0.42.0`, and the ECS `secrets` JSON-key syntax against the RDS-managed secret.
- The `roles.sql` bootstrap against a real RDS master user, and the `sslrootcert` path the database URLs must reference inside the distroless image.
- **Whether the application metric alarms ever leave `INSUFFICIENT_DATA`.** They are written against `custom_metric_namespace` and `custom_metric_dimensions`, which must match the OTLP collector's output exactly; that can only be confirmed with a collector running. This applies to every relay alarm, both rate-ratio alarms and the four financial-signal alarms.
- **Whether the kill-switch log metric filter matches.** The pattern is built from the JSON access log that `internal/httpapi/middleware.go` emits (`msg`, `method`, `route`, `status`) and the chi route pattern `/v1/admin/kill-switches` that `internal/gen/api` registers, both read from the source; it has not been run against a real log stream.
- **Whether the two-target-group health checks behave as intended.** `/v1/readyz` (readiness, decides traffic) and `/v1/healthz` (liveness, on a second target group reached by listener rule priority 10) were read from `internal/httpapi` and `openapi/openapi.yaml`. The previous default was an unprefixed `/readyz`, which would have 404'd and left every target permanently unhealthy; the prefix is now correct, but only an apply proves the rest.
- The autoscaling split. `aws_ecs_service.scaled` exists only so `ignore_changes = [desired_count]` applies to the api and not to the workers (a `lifecycle` block cannot be conditional). Moving the api between the two resources is a destroy-and-create of the service, so if the api is ever taken off autoscaling, do it in a maintenance window.
- Cross-region backup copies (ADR-0017) are not in this stack. RTO/RPO are not claimed (PART 205; `BACKUP_RESTORE.md`).

**CI/CD is unverified in a different way: the repository has no git remote (BLOCKERS SB-004), so `ci.yml` and `release.yml` have never executed.** Their YAML parses and every `make` target and script path they name exists, checked against the working tree. What cannot be checked without a remote: that the pinned actions resolve at the versions named (`actions/checkout@v7`, `actions/setup-go@v7`, `actions/cache@v6`, `actions/upload-artifact@v7`, `pnpm/action-setup@v6`, `actions/setup-node@v7`, `gitleaks/gitleaks-action@v3`, `sigstore/cosign-installer@v4.1.2`, `docker/login-action@v4`, `docker/setup-buildx-action@v4`, `aws-actions/*`, `actions/attest-build-provenance@v4`); that GHCR accepts the push and cosign keyless signing succeeds with the workflow's OIDC identity; that `docker compose up -d --wait postgres` comes up inside the runner's time budget; that `gitleaks` does not need the organisation licence; and that the whole set finishes inside its timeouts. `release.yml`'s ECR path activates only when the repository variables `AWS_RELEASE_ROLE_ARN` and `AWS_REGION` exist, which needs both the remote and an account.

Known residual: the Redis AUTH token generated by `random_password` is present in Terraform state (sensitive); mitigated by the KMS-encrypted, access-restricted state bucket and `auth_token_update_strategy = ROTATE`.

Known gaps outside this stack, recorded here because they affect deployment:

- `internal/provider` has no metric for a provider's health state, so provider degradation is alarmed on its consequences (section 10). A `provider_health_state` gauge, labelled by adapter, would let the alarm say which provider rather than which symptom.
- There is no kill-switch metric either; the alarm reads the access log. An audit-side counter would be a better source and would work for activations that do not come through the api.
- `docs/architecture/SYSTEM.md` section 2 lists eight binaries and predates `cmd/relay-worker`.
