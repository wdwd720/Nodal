# Launch gate matrix

What stands between this repository and taking real money, and who can move
each one. Every row is either something software can finish, in which case it is
work and is tracked, or something a person outside this repository has to
produce, in which case it is BLOCKED_EXTERNAL and no amount of engineering
changes it.

Last audited: **2026-09-09**, against commit `d1092d1` and the deployment at
`https://api-nodal.actorvia.xyz`.

## The five launch flags

| Flag | Value | What would change it |
|---|---|---|
| `SOFTWARE_COMPLETE` | **false** | Named work remains. See "What software still owes" below. |
| `STRIPE_PRODUCTION_APPROVED` | **false** | Stripe's own review of a real business. BLOCKED_EXTERNAL. |
| `LEGAL_APPROVED` | **false** | Counsel. BLOCKED_EXTERNAL. |
| `PENTEST_COMPLETE` | **false** | An independent third party. This audit is not one and does not claim to be. BLOCKED_EXTERNAL. |
| `LIVE_READY` | **false** | All four above, plus the capability gate activated by three people. |

`LIVE_READY` is the conjunction. It is false today for four independent
reasons, and three of them cannot be resolved from inside this repository at
all.

## The gate that actually holds the money path shut

`CREDIT_PURCHASE` is not activated. `capability_gates` has no row for it in the
deployed database, so `gates.Evaluate` returns `ReasonNoGateRow` and
`PurchaseService.StartPurchase` refuses with `CAPABILITY_NOT_APPROVED` before it
reaches pricing or the provider.

Activating it requires, in this order and by construction:

| Step | Requires | Enforced where |
|---|---|---|
| Propose | `gate:propose`, a recent step-up, and all four evidence references | `internal/gates.Admin.Propose` (step-up added by F-99) |
| Approve | `gate:approve`, a recent step-up, a principal **distinct from the proposer** | `internal/gates.Admin.Approve`, re-derived in SQL by `cp_gate_transition` |
| Activate | `gate:approve`, a recent step-up, a principal distinct from **both** | `internal/gates.Admin.Activate`, re-derived in SQL |

The four evidence references are legal review, provider contract, risk approval
and security approval. They are the rows below marked BLOCKED_EXTERNAL, which is
why this gate is where the external blockers meet the code.

**Stated plainly, because the provider checkpoint did not:** "three distinct
principals" means three distinct `users.id` values. Distinctness is a string
comparison; there is no person entity and no uniqueness on email. Three identity
provider accounts under one person's control satisfy the ceremony in both Go and
Postgres. It is a control against one careless operator, not against one
determined one (F-93).

## BLOCKED_EXTERNAL

| # | Item | Why software cannot resolve it | What the code already does |
|---|---|---|---|
| 1 | Stripe production approval | A business relationship and a restricted-business review by Stripe. | The adapter, the webhook path, the evidence store and the reconciliation are built and exercised against Stripe's sandbox. Going live is a configuration change, not construction. |
| 2 | Legal review reference | Counsel's opinion on selling Credits in the intended jurisdictions. | `internal/legalrouter` refuses by default and `CP_API_LEGAL_POLICY` is CONSERVATIVE unless set. |
| 3 | Provider contract reference | A signed contract. `PurchaseRegistry` refuses a provider with no `ContractReference` outside LOCAL/TEST. | The refusal is built and tested. |
| 4 | Risk approval reference | A person with the authority to sign it. | The gate demands it and re-checks it at activation. |
| 5 | Security approval reference | Same. | Same. |
| 6 | Independent penetration test | A third party. | The adversarial suites in `test/security` are not a substitute and are not offered as one. |
| 7 | A mainnet settlement mint | `CP_API_SETTLEMENT_MINT` is a Solana **devnet** USDC mint. The mainnet address is a fact about Solana, not a value to invent. | The pair is validated against the registered stablecoins in the database; nothing refuses a devnet mint in PROD, which is recorded in F-93. |

Nothing in this table moved as a result of this audit, and nothing in it can be
moved by more engineering.

## What software still owes

These are the reasons `SOFTWARE_COMPLETE` is false. Each is tracked with its
evidence in `docs/audit/AUDIT_FINDINGS.md`.

| Item | Finding | Shape |
|---|---|---|
| The request body is buffered before the rate limiter runs | F-85 | Middleware ordering on the money path; open by decision, not oversight |
| Request validation precedes authentication | F-84 | Authorising on the route pattern before the generated wrapper; wants its own design |
| `cmd/api` requires the schema-owner credential | F-93 | Make `CP_DATABASE_MIGRATE_URL` service-conditional; needs a blueprint change and a redeploy |
| A wrong Stripe account warns rather than refusing to start | F-93 | A real availability decision on a tier that cold-starts |
| Render replica count is an assumption, not an assertion | F-93 | A startup advisory lock would make it one |
| Neon pool has no idle or connect timeout | F-93 | Small change, needs measurements this repository does not have |
| Three retention classes are declared and unenforced | ADR-0020 | The ADR decides how; the migration and the partition manager remain |
| 121 enum CHECKs have no Go counterpart | F-95 | Most have no Go list to compare against, by their nature |
| Fifteen destination-only transition bindings | F-94 | **Done.** Listed for completeness |
| The AU001 binding trusts a transaction-local setting | F-42 | Privilege work on the state columns |
| Two statements about who may read encrypted PII contradict each other | F-47 | A policy decision |

## What this audit changed about the matrix

Before it, four of the rows in "what software still owes" were not known to
exist, and four defects that would have stopped the product were not either:

- a planted OIDC callback signed the victim in as the attacker (F-87);
- every unauthenticated rate-limit bucket was one shared counter (F-88);
- the money-at-risk ceiling could only rise, and would have refused every
  purchase forever at $2,000 of lifetime sales (F-90);
- the 50-account launch cohort was enforced nowhere (F-91);
- and the Stripe call inside the purchase transaction meant eight concurrent
  purchases could each pass one ceiling (F-96).

None of those was visible from the provider activation checkpoint, which
recorded the surfaces they live on as VERIFIED PASS.
