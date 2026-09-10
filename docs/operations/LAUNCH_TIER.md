# The $0-fixed-cost launch tier

A hard constraint, not a preference: the initial cohort must cost nothing fixed
per month beyond the domain, and no quota may be crossable in a way that starts
charging a card.

**Date:** 2026-09-09. Every free-tier claim below was read from the provider's
own documentation on that date, not from memory. Where a provider's behaviour on
overage decides the answer, that behaviour is quoted.

---

## 1. The test every candidate had to pass

Popularity was not a criterion. Five properties were, and the first eliminated
more candidates than the other four together.

1. **Crossing a quota must not charge.** It must suspend, reject, or fail. A
   provider that meters overage against a card on file fails this even if the
   free allowance is generous.
2. **No card required**, where a provider offers that. A card on file is the
   mechanism by which quota-crossing becomes a charge, so its absence is a
   control rather than a convenience.
3. **Postgres transaction semantics** the ledger actually uses: real
   `BEGIN`/`COMMIT`, `SELECT ... FOR UPDATE`, serialisable behaviour where the
   code asks for it. Not an eventually-consistent document store.
4. **Durable financial records with an export path.** A free tier that deletes
   data on expiry cannot hold a ledger.
5. **A migration path** to the AWS architecture that is a connection-string
   change rather than a rewrite.

## 2. What the research found

### Rejected, and why

| Candidate | Why not |
|---|---|
| **Render free Postgres** | "Free databases expire 30 days after creation", then a 14-day grace period, "after which it's permanently deleted." A ledger cannot live on a store with an expiry date |
| **Cloudflare R2** | The free 10 GB is real, but R2 cannot be enabled without a payment method on file, and users report an immediate charge on activation. Fails test 1 and 2 |
| **Auth0 free** | MFA is a paid feature. See §4 — this one is load-bearing |
| **Google Cloud Run** | The free allowance is the most generous available, but it requires a billing account and offers budget *alerts*, not hard caps. Crossing the allowance charges the card. Fails test 1 outright |
| **Oracle Cloud Always Free** | Genuinely free compute, but requires a card and reclaims idle instances. Fails test 2, and reclamation fails test 4 |
| **Supabase** (as the ledger) | 500 MB Postgres with no card, which passes tests 1–3. But free projects pause after a week of inactivity, and a paused ledger during a dispute window is the wrong failure |

### Chosen

| Layer | Provider | Free allowance | Behaviour at the limit |
|---|---|---|---|
| Ledger | **Neon** Postgres | 0.5 GB storage, 100 CU-hours, 5 GB egress per project | **Hard-stops.** "When you run out of CU-hours or public network transfer, your compute is suspended until the next billing period or until you upgrade." Storage overage fails the operation |
| Compute | **Render** web service | 750 instance-hours/month, custom domain, managed TLS | **Suspends.** Over the hours, "Render suspends all of your Free web services until the start of the next month." On bandwidth, it bills only "if you haven't added a payment method" — otherwise it suspends. So: no payment method, ever |
| Identity | **Zitadel** Cloud | 100 daily active users, all security features, $0 forever | Cohort cap is below the allowance |
| Evidence archive | **Postgres**, same instance | within the 0.5 GB | Covered by the capacity guard's database ceiling |
| Rate limiting | **in-process** | n/a | See §5 |
| Telemetry | **none exported** | n/a | See §6 |

Neon's hard-stop is the single most important property in this table. It is the
only one of the shortlisted database providers that documents suspension rather
than metering, and it is why the ledger lives there.

## 3. What was NOT compromised

The instruction was explicit: zero cost must not mean lower financial
integrity. Three things were considered and rejected as ways to save money.

**The ledger stays authoritative Postgres.** Double-entry, `FOR UPDATE`,
transactional issuance of Credit against captured funding. No key-value store,
no eventual consistency, no "we'll reconcile later".

**Evidence stays write-once.** The raw signed webhook body is still preserved
before it is parsed. It moves from S3 to a Postgres table, which is a change of
medium and not of guarantee: the application role gets `INSERT` and `SELECT` and
is denied `UPDATE` and `DELETE`, which is the same privilege-model technique the
rest of this codebase uses (D-016) and is enforced by the database rather than
by the code that writes it.

**Every gate, kill switch and dual-control path is untouched.** Nothing in the
launch tier relaxes a capability gate, a step-up check, or an approval
requirement.

## 4. Auth0 is out, and this is the reason

Auth0's free plan does not include MFA. The One-time Password factor is a
Professional feature, and the tenant created earlier is 22 days into a trial of
exactly that.

That is not a cost problem, it is a correctness problem. `HasStrongAMR` accepts
`mfa`, `otp`, `hwk`, `swk`, `pop`, `webauthn` or `passkey`, and nothing else.
`RequireStepUp` fails closed without one. Step-up gates break-glass elevation
and the dual-control approval path, and those are what activate a capability
gate. So an identity provider that stops emitting a strong `amr` does not
degrade gracefully — it makes `CREDIT_PURCHASE` **permanently impossible to
activate**. That is finding F-26's exact shape: a control no user can ever
satisfy is not a control.

**Zitadel** replaces it. It is OIDC-certified, its documented `amr` values
include `mfa` and `otp`, and its published claim set includes `acr`, `amr`,
`auth_time`, `email` and `email_verified` — requirements 6, 7 and 8, which are
the three that eliminate candidates. TOTP and WebAuthn are in the free tier's
"all security features". The free allowance of 100 daily active users sits above
the launch cohort cap of 50.

No requirement was weakened to fit a free plan. The provider changed because the
requirement did not.

## 5. Rate limiting: one process, honestly

The old rule refused process-local rate-limit counters in STAGING and PROD. The
launch tier runs one instance, and on one instance process-local counters
enforce exactly the limit they were given — so the old rule would have forced a
Redis dependency that bought nothing.

The temptation was to add an exception for the free tier. That would have been a
weakening. Instead the rule now tests the thing the invariant actually depends
on: `CP_HTTP_REPLICAS`. Process-local counters are refused whenever more than
one process serves HTTP, **in every environment** — which is strictly stronger
than the rule it replaced, because it also catches the three-replica DEV
deployment the old rule waved through.

A free Redis was considered and rejected on a specific ground. Upstash's free
tier is a daily command allowance, and this limiter fails **closed** on a store
error. Exhausting the allowance would therefore refuse every request to the API
— an outage caused by adding a dependency that a single instance did not need.

## 6. Telemetry: nothing exported, and that is stated

`CP_TELEMETRY_OTLP_ENDPOINT` is optional in this codebase: empty installs no-op
tracer and meter providers. The launch tier leaves it empty and relies on
structured logs, which the platform retains.

This is the one place the launch tier is genuinely thinner than the AWS
architecture, and it is written down rather than glossed. There are free OTLP
collectors; none was adopted because each is another account, another quota and
another thing that can fail, for a cohort of 50 where the logs are readable by
one person. It is the first thing to add when the tier grows.

Alerting is not thinner, and it was until F-118. Every reconciliation alert —
a ledger-integrity violation above all — is POSTed by `internal/alert` to
`CP_ALERT_WEBHOOK_URL`, in Slack's, Discord's or ntfy's own shape when the host
is one of those and as JSON otherwise. A Slack or Discord incoming webhook or
an ntfy topic costs nothing and needs no collector. The API runs the internal
verification pass itself every five minutes (`cmd/api/reconverify.go`),
because this tier has no worker to run it, and STAGING refuses to start
without a destination: an alert nobody receives is not an alert.

## 7. Failing closed before the quota

The hard rule was that the system must fail closed before crossing a free-tier
limit or degrading a financial invariant. `internal/capacity` implements it, and
it runs **inside the transaction of the financial action, before the provider is
called**.

| Ceiling | Launch value | What it protects |
|---|---|---|
| Accounts | 50 | The cohort the tier was sized for |
| Purchases per rolling 24h | 200 | Webhook volume and database growth |
| Money at risk | $2,000 | What a failure can cost, not what it can consume |
| Database bytes | 67% of 500 MB | Room to reconcile and export while things go wrong |

Four properties make it a guard rather than a gesture:

- **A measurement that fails is a refusal.** A guard that admits an action
  because it could not measure the thing it guards is not a guard, and that is
  the entire requirement.
- **The amount is included.** One unit below the money-at-risk ceiling, checking
  current exposure alone admits any purchase at all and the tier ends up past
  its cap. The check is against what the deployment would owe *after* the
  action.
- **A rolling 24 hours, not a calendar day.** A calendar day resets at an hour
  nobody chose and lets twice the ceiling through across midnight.
- **An unknown action is refused.** A new financial action cannot inherit "no
  ceilings apply".

It is deliberately separate from `internal/gates`. A gate says whether this
deployment is *approved* to sell Credits, which people decide under dual
control. This says whether there is *room*, which is measured. Conflating them
would report a full tier as a revoked approval, and raising a ceiling would look
like granting one.

## 8. When to migrate, and why each threshold

Not a user count. Each threshold is the point at which a free-tier property
stops being true.

| Threshold | Why it is the right moment |
|---|---|
| Money at risk approaches $2,000 | The cap binds and starts refusing real purchases. This is a revenue signal, and it is the intended trigger |
| Database above 335 MB | The capacity guard stops new financial actions. Migration must complete before it does |
| Neon above 80 of 100 CU-hours in a month | The database is awake most of the time, so scale-to-zero has stopped being free |
| Render above 600 of 750 instance-hours | The API is awake most of the time. The same signal from the compute side |
| More than one replica needed | Either for availability or for load. The moment this is true, the free tier's single instance is the constraint and Redis-backed rate limiting becomes mandatory |
| Cold starts affecting webhook delivery | Render's free tier takes about a minute to wake. Stripe retries for three days so nothing is lost, but sustained retries mean the tier is undersized |
| Zitadel above 80 daily active users | Below the 100 allowance, with room to migrate |
| Any real-money volume beyond a pilot | Durability, backup retention and the audit archive's WORM guarantee are all thinner here than on AWS. This is a judgement call, not a metric, and it should be made early rather than late |

The destination already exists. The AWS architecture is applied and unchanged:
the deployment identity, the state bucket, the 283-resource plan, the TLS
certificate and the Terraform are all preserved. Migration is a database dump
and restore, a DNS change, and `nodal-deploy.ps1 -Apply`.

## 9. What the domain does

`api-nodal.actorvia.xyz` stays the public API hostname. Render supports custom
domains with managed TLS on the free tier, so the name and the public API
contract are unchanged between tiers — only the CNAME target differs.

The ACM certificate already issued is not wasted. It stays valid until March
2027 and is what the AWS load balancer will use on migration. The GoDaddy
validation record stays in place so ACM can renew.
