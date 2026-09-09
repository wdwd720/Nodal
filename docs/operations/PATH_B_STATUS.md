# Path B deployment — status and the decision before apply

**Date:** 2026-09-09. Everything below was verified against the live AWS
account, the live DNS zone and the live Auth0 tenant, not inferred.

---

## 1. Done

| | |
|---|---|
| AWS deployment identity | `nodal-terraform`, assumed with MFA, no access key anywhere |
| Terraform state | `s3://nodal-terraform-state-049286562577-us-east-2`, versioned, encrypted, private |
| Backend | initialised; `terraform plan` runs against real credentials |
| TLS certificate | **ISSUED** for `api-nodal.actorvia.xyz`, valid to 2027-03-25 |
| DNS validation | CNAME added at GoDaddy; ACM validated it in under three minutes |
| Auth0 tenant | `dev-wlk8r3o1qrt5g15y`, region `us` |
| Auth0 application | `Nodal Production API`, Regular Web Application |
| Auth0 MFA | One-time Password factor enabled |

**The certificate exists before the hostname resolves**, which is the ordering
`actorvia.xyz`'s HSTS `includeSubDomains; preload` forces. The record that makes
the name live is added after the load balancer exists.

`api.actorvia.xyz` already points at the other project's Lightsail instance.
`api-nodal` is a different name and nothing about the apex, `www`, `api` or
`releases` was touched.

## 2. The Auth0 configuration, checked against the eleven requirements

`docs/operations/IDENTITY_PROVIDER.md` lists what this codebase demands. Read
from the live discovery document:

| # | Requirement | State |
|---|---|---|
| 1 | Discovery echoes the issuer exactly | `https://dev-wlk8r3o1qrt5g15y.us.auth0.com/` — **with the trailing slash**, which the config must match byte for byte |
| 2 | PKCE S256 | advertised |
| 4 | Asymmetric signing | RS256 and PS256 advertised |
| 6 | `email` + `email_verified` | both in `claims_supported` |
| 7 | `amr` with a strong method | OTP factor enabled, so `amr` carries `mfa` |
| 8 | `auth_time` | in `claims_supported` |
| 10 | `acr_values` overlap | Auth0 publishes no `acr_values_supported`, and the check only fires on a published list, so it passes |
| 11 | HTTPS issuer | yes |

Configured on the application:

```
Allowed Callback URLs   https://api-nodal.actorvia.xyz/v1/auth/callback
Allowed Logout URLs     https://api-nodal.actorvia.xyz
Allowed Web Origins     https://api-nodal.actorvia.xyz
```

The callback path is not a guess: `cmd/api/devlogin.go` defines
`defaultCallbackPath = "/v1/auth/callback"`.

**One thing to know about the plan.** The One-time Password factor is badged
`PRO` in the dashboard, and this tenant is 22 days into a trial of features
outside the free plan. If MFA stops working when the trial ends, `amr` loses its
strong method, `RequireStepUp` fails closed, and `CREDIT_PURCHASE` becomes
impossible to activate — the exact shape of finding F-26. This is a billing
decision, not a technical one, and it is written down rather than discovered
later.

Requirements 3, 5 and 9 are behavioural and are proved by a real login, not by
configuration. That happens once the API is reachable.

## 3. Two Terraform defects, found and fixed

Neither could have been found without running a plan against real credentials.
Both are correct Terraform against infrastructure that already exists, and
neither survives a **first** apply.

The Redis alarms used `for_each` over node ids derived from the replication
group's name, which does not exist yet. `for_each` over unknown strings cannot
be planned at all. The set became a map keyed by position, which is known.

The liveness alarm's `count` tested a target group ARN suffix produced by the
same apply. `count` may not depend on an unknown value, so whether the alarm
exists is now a separate, statically known variable.

Before this, `terraform plan` failed on every environment before printing a
single resource.

## 4. The plan, and the decision it forces

`terraform plan` now succeeds: **283 resources to add**, with three services
(`api`, `reconciliation-worker`, `migrate`) rather than nine.

But Path B inherits `environments/prod`'s **sizing**, and that is a full
production footprint:

| | Planned | Monthly, us-east-2, on-demand |
|---|---|---|
| RDS `db.r6g.xlarge`, Multi-AZ | 1 | ~$590 |
| RDS read replica, same class | 1 | ~$295 |
| RDS storage, 200 GB | | ~$60 |
| ElastiCache `cache.r7g.large` | 3 nodes | ~$440 |
| NAT gateways | 3 | ~$100 |
| Fargate tasks | 5 | ~$180 |
| ALB, Secrets Manager, KMS, alarms | | ~$35 |
| **Total** | | **~$1,700** |

That is Path A money for a Path B deployment, and it is not what Path B was
approved as. The gap analysis put Path B well below this precisely because the
point of Path B is a small foundation the rest is added to.

### The same 283 resources, sized for Path B

Every value below is an existing variable. No code changes, no weakened
controls:

```hcl
rds_instance_class       = "db.t4g.medium"   # was db.r6g.xlarge
rds_allocated_storage    = 50                # was 200
rds_read_replica_count   = 0                 # was 1
redis_node_type          = "cache.t4g.micro" # was cache.r7g.large
redis_num_cache_clusters = 2                 # was 3
```

**~$450/month**, most of which is the three NAT gateways and Fargate.

What stays untouched, because these are durability and safety controls rather
than sizing: RDS Multi-AZ, deletion protection, 35-day backup retention,
per-AZ NAT redundancy, and every alarm.

Dropping to a single NAT gateway would save another ~$66 and is the only
remaining lever, but it is hardcoded `false` in the prod environment on purpose
and changing it is a real availability decision.

## 5. Still blocked, and on whom

| Item | Blocked on | Why it cannot be invented |
|---|---|---|
| `otlp_endpoint` | an account | PROD forbids the plaintext sidecar and requires a TLS OTLP collector. A fake host means every trace and metric is silently dropped |
| `sev1_https_endpoints` | an account | PROD requires at least one paging endpoint. A fake URL means SEV1 alarms fire into nothing, which is worse than no alarm |
| Container images | nothing external | ECR repositories are created by this apply, so images are built and pushed after it, and the services scale up on a second apply |
| Alert email | a decision | Any address here receives an SNS confirmation mail that must be clicked |

The first two have free tiers that satisfy them honestly — Grafana Cloud or
Honeycomb for OTLP, and any pager or webhook receiver for SEV1. Both need a
sign-up, which is why they are here rather than done.

## 6. What happens after those three values arrive

1. `terraform.tfvars` is written from the values, no placeholders.
2. `nodal-deploy.ps1 -Apply` creates the 283 resources, ECR included.
3. Images are built from `build/Dockerfile` and pushed to the new repositories.
4. The Auth0 client secret is placed in Secrets Manager. It is never printed.
5. `api` and `reconciliation-worker` scale up and pass health checks.
6. The `api-nodal` CNAME is added at GoDaddy, pointing at the load balancer.
7. `https://api-nodal.actorvia.xyz/v1/healthz` answers over TLS.
8. The Stripe webhook is pointed at `/v1/webhooks/stripe_credit`.
9. Sandbox end-to-end: signature verification, idempotency, credit issuance,
   refund, dispute, reversal, reconciliation. No real money moves.
