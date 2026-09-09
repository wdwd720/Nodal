# DEPLOYMENT GAP ANALYSIS

Exactly what stands between this repository and a real production HTTPS endpoint,
established by inspection rather than by assumption.

**Date:** 2026-09-09. **Assessed against:** `infra/terraform`, `cmd/api`, the live
domain, and the operator machine.

---

## 1. What is already true

Verified in this session, not quoted from a document.

| | |
|---|---|
| Terraform code | **Valid.** `terraform validate` passes for dev, staging and prod. `terraform fmt -check -recursive` is clean |
| Terraform version | 1.15.8 installed; the code requires `>= 1.9, < 2.0` |
| Modules | Twelve: kms, network, rds, redis, s3-evidence, secrets, ecs-cluster, ecs-service, app-config, waf-edge, observability, iam-deploy |
| Provider pinning | `hashicorp/aws` 6.63.0 and `hashicorp/random` 3.9.0, locked |
| AWS CLI | 2.36.21 installed |
| **AWS account** | **Exists: `049286562577`.** `DEPLOYMENT.md` says "no account, credentials or OIDC trust exist yet". The account exists; the session needs re-authentication |
| Domain | `actorvia.xyz` resolves, is served by **Vercel**, and DNS is managed at **GoDaddy** (`domaincontrol.com` nameservers) |
| Terraform state | **None.** No `.tfstate` anywhere. Nothing has ever been applied |

## 2. The finding that changes the shape of the problem

**`cmd/api` dials Postgres and nothing else.**

This was established by linking rather than by reading comments:

```
go list -deps ./cmd/api | grep -E "clickhouse|temporal|redpanda|franz|kgo"
→ no matches
```

Those clients are not in the binary at all. Redis is linked, through
`internal/ratelimit`, and `cmd/api/wire.go` chooses `ratelimit.NewMemoryStore()`
rather than the Redis store. Readiness is `db.Ping` and nothing more.

So the API's runtime dependencies are:

| Dependency | Needed by the API? | Why |
|---|---|---|
| **PostgreSQL** | **Yes** | Every read and write |
| **S3-compatible object storage** | **Yes, for the Stripe webhook** | The ingestion pipeline preserves the raw signed request before parsing it. `creditWebhookPort` refuses to build without an evidence bucket |
| Redis | No | The API uses an in-memory rate-limit store |
| Redpanda | No | Not linked |
| ClickHouse | No | Not linked |
| Temporal | No | Not linked |
| The other eight binaries | No | The Credit purchase path is API plus the reconciliation worker |

That matters because the prod Terraform provisions all of them, and the
prerequisites for the ones the API does not use are paid third-party
subscriptions.

**Closed on 2026-09-09.** `internal/config` used to require every dependency of
every binary, so an API-only deployment had to be given four endpoints it would
never contact. Requirements are now declared per service: `Load` takes a
`config.Service`, a variable tagged with a `Dependency` is required only of a
binary that declares it, and a malformed value still fails closed for everyone.
`cmd/api` starts in PROD with no Redis, Redpanda, ClickHouse or Temporal
configured at all, and the workers that use those still refuse to start
without them. See `internal/config/service.go` for the table and the audit
behind it.

## 3. What the prod stack actually requires before it can apply

From `infra/terraform/environments/prod/terraform.tfvars.example`, every value
below is required and none of them exists today.

### AWS-side

| Item | State |
|---|---|
| AWS account | **Exists** (`049286562577`) |
| Authenticated session | **Missing** — `aws sts get-caller-identity` fails, "reauthenticate with `aws login`" |
| An IAM role for Terraform | **Missing, and needed.** The configured identity is `arn:aws:iam::049286562577:root`. Terraform must not run as root |
| Terraform state bucket + KMS key | **Missing.** Bootstrapped by hand, once, before the first `init` |
| `backend.hcl` | **Missing.** Only `backend.hcl.example` exists |
| ACM certificate (regional, for the ALB) | **Missing.** Requires a hostname |
| Second ACM certificate in `us-east-1` | **Missing.** Only if CloudFront stays enabled, which it is by default in prod |
| Three IAM roles: auditor, kms-admin, secrets-admin | **Missing** |

### Third-party subscriptions the prod tfvars requires

| Item | State | Note |
|---|---|---|
| Redpanda Cloud | Missing | Not used by the API |
| ClickHouse Cloud | Missing | Not used by the API |
| Temporal Cloud | Missing | Not used by the API |
| An OIDC identity provider | **Missing, and the API does need it** | `auth.issuer`, `client_id`, `client_secret`. Without it, `CP_AUTH_MODE=oidc` has nothing to point at, and dev auth is refused in STAGING/PROD |
| OTLP TLS collector | Missing | PROD forbids the plaintext sidecar |
| PagerDuty (SEV1/SEV2) | Missing | |
| SES | Missing | Notification provider |
| Privy, Jupiter, Helius, Anthropic keys | Missing | Domain B/C providers, not the Credit path |

### Repository and CI

| Item | State |
|---|---|
| GitHub repository | **Does not exist.** `git remote` points at one, `iam-deploy` expects `github_org`/`github_repo`, and the release workflow's ECR push block is commented out |
| Container images in ECR | **None.** `image_tag` expects a signed, attested release SHA |
| GitHub OIDC trust | **Missing**, created by `iam-deploy` on first apply |

## 4. The cost and time this implies

Stated plainly because it is a decision and not a detail.

The prod stack as written provisions, at minimum: a three-AZ VPC with a NAT
gateway per AZ, RDS PostgreSQL Multi-AZ with deletion protection, an
ElastiCache replication group of three nodes, an ALB, WAFv2 with managed rule
groups, CloudFront, ECR repositories, and Fargate services for nine binaries
with the API autoscaled from three tasks.

That is an enterprise production footprint. Three NAT gateways alone are around
100 USD a month before traffic; Multi-AZ RDS and a three-node Redis cluster are
each in the same range; nine Fargate services at the sizes in the tfvars are
more. Add Redpanda, ClickHouse and Temporal Cloud subscriptions and the monthly
figure is comfortably four digits.

**None of it is required to receive a Stripe webhook.**

## 5. Two honest paths

### Path A — the full stack, as the repository intends

Correct, matches the architecture, and is what STAGING and PROD were designed
for. It needs everything in section 3, including four paid subscriptions and a
GitHub repository, and it is days of work plus a significant recurring bill.

### Path B — the API path only, on the same AWS account

Provision what the Credit purchase path actually uses: VPC, RDS PostgreSQL, the
S3 evidence bucket, one Fargate service for `cmd/api`, one for
`cmd/reconciliation-worker`, an ALB with an ACM certificate, and Secrets
Manager. Skip Redis, Redpanda, ClickHouse, Temporal, CloudFront and the other
seven binaries.

This is not a toy. It is the same code, the same images, real HTTPS, real
Secrets Manager, real RDS with TLS — a production-style endpoint that Stripe can
post to, and a foundation the rest of the stack is added to later rather than
thrown away.

It still needs: an authenticated non-root AWS role, the state bucket bootstrap,
an OIDC identity provider for authentication, a hostname, and a certificate.

**Path B is the recommendation**, and it is a recommendation rather than a
decision because it commits real money and diverges from `environments/prod`
until the rest is added.

## 6. Hostname

`actorvia.xyz` is controlled: it is the Stripe account's declared URL, it serves
the live billing webhook with a 0% error rate, and its DNS is managed at GoDaddy
where a record can be added.

One property of the existing site constrains this:

```
Strict-Transport-Security: max-age=63072000; includeSubDomains; preload
```

`includeSubDomains` with `preload` means every subdomain of `actorvia.xyz` is
HTTPS-only in browsers from the first request, with no HTTP fallback and no
click-through on a bad certificate. A subdomain must therefore have a valid
certificate before it is useful. That is a constraint, not a problem: the ALB
gets an ACM certificate anyway.

**Recommended: `api-nodal.actorvia.xyz`.**

- It is a new record. It does not touch `actorvia.xyz` or `www`, so the Vercel
  site and the live Stripe webhook are untouched.
- It names the product and the environment without claiming to be Actorvia's
  API.
- It is a CNAME or ALIAS to the ALB, so it can be pointed elsewhere or deleted
  without affecting anything else.

The Stripe webhook would then be:

```
https://api-nodal.actorvia.xyz/v1/webhooks/stripe_credit
```

**This needs approval before any DNS record or certificate is created.**

## 7. What is missing, as a single list

Answering the question in the order it was asked.

| Item | State |
|---|---|
| AWS account | Exists (`049286562577`) |
| AWS credentials | **Needs `aws login`, and a non-root role for Terraform** |
| Terraform variables | Example only; a real `terraform.tfvars` does not exist |
| VPC / networking | Written, never applied |
| ECS / Fargate | Written, never applied. Images do not exist |
| RDS / Postgres | Written, never applied |
| S3 | Written, never applied. Required for webhook evidence |
| Secrets Manager | Written, never applied. No secret values placed |
| TLS certificate | **Missing.** Blocked on the hostname decision |
| DNS | **Missing.** GoDaddy, one record, blocked on approval |
| Load balancer | Written, never applied |
| Production environment variables | Derivable except the external endpoints and the OIDC issuer |
| Monitoring | Written (CloudWatch alarms, SNS, dashboard). Needs PagerDuty endpoints |
| Terraform state backend | **Missing.** Manual bootstrap, once |
| GitHub repository and OIDC | **Missing** |
| OIDC identity provider for user authentication | **Missing, and on the critical path** |

## 8. What can proceed without any of the above

- Everything in `STRIPE_PRODUCTION_CHECKLIST.md` Stage 2 that runs locally,
  once a sandbox key is placed. `stripe listen` forwards Stripe-signed
  deliveries to a local API, which exercises the real signature path without
  any deployment at all.
- The remaining code in that checklist's Stage 6.

A local `stripe listen` run is not a production endpoint and does not close the
webhook item. It does prove the integration against real Stripe deliveries,
which is the part a deployment cannot tell you anything more about.
