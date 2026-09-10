# Provider activation checkpoint

> **Independently re-audited 2026-09-09 (F-83 – F-93).** Six read-only
> audits ran over every surface below and each claim was re-verified against
> the source; the deployment was probed directly. Much of this document held
> — the signature verification, the database-enforced replay dedup, the
> exactly-once property across all four crash boundaries, and fail-closed under
> database unavailability are all as described. Eleven findings came out of it,
> four of them P1, and the sentences they contradict are corrected in place and
> marked **[corrected 2026-09-09]**. `docs/audit/AUDIT_FINDINGS.md` F-83 to
> F-93 is the record; F-93 is the inventory of what was verified and not fixed.
>
> The configuration hash below is superseded: F-88 adds
> `CP_HTTP_TRUSTED_PROXY_CIDRS` and F-90 adds `CP_CREDIT_SETTLEMENT_WINDOW`, so
> the next deploy reports a different hash. That is the mechanism working, not
> a drift.

Date: 2026-09-10
Deployment: `https://api-nodal.actorvia.xyz` (Render free web service, one instance)
Environment: `STAGING`
Configuration hash: `e1ad81b6917dcc72c2851646f388011b6e1170aca1f288f81b7860106c241a39`
Database: Neon Postgres 17, `neondb`, 18 MB of a 500 MB quota, schema at migration 730
Identity: Zitadel (`nodal-az1hxe.us1.zitadel.cloud`), OIDC with PKCE S256
Payments: Stripe sandbox `acct_1UDbrdAeDQ6sKD6H` ("NODAL Integration"), test mode only

Fixed monthly cost: **$0**. No payment method is on file at any provider.

---

## What this deployment is

It is a **rehearsal**, not a launch, and that is now stated rather than assumed.
It runs every production rule — no fake providers, no dev auth, no plain-text
secrets, TLS verification to the database, the same capability gates, the same
capacity ceilings — against Stripe's sandbox, so no real money can move through
it.

Going live is `CP_ENV` to `PROD`, every `CP_PROVIDER_*_MODE` to `live` with
live keys, and a real settlement mint. `internal/config` refuses any other
pairing of environment and provider mode, in both directions — for STAGING
and PROD.

**[corrected 2026-09-09]** Three things, not two. `CP_API_SETTLEMENT_MINT` is a
Solana **devnet** USDC mint and nothing refuses it in PROD, so flipping the other
two would settle against a worthless token. And "in both directions" holds only
between STAGING and PROD: `CP_ENV=DEV` with live modes and live keys passes
validation while dropping every production rule (F-93).

---

## Status by item

| # | Item | Status |
|---|---|---|
| 1 | Stripe sandbox webhook configured | VERIFIED PASS |
| 2 | Webhook signature validation end to end | VERIFIED PASS |
| 3 | Idempotency under duplicate and replayed delivery | VERIFIED PASS |
| 4 | Full sandbox credit purchase lifecycle | BLOCKED_EXTERNAL |
| 5 | Refunds, disputes and reversals | BLOCKED_EXTERNAL at the deployment; VERIFIED PASS against the code and a real database |
| 6 | Reconciliation against Stripe | VERIFIED PASS (both sides empty and agreeing) |
| 7 | Evidence persistence and immutability | VERIFIED PASS |
| 8 | Capacity guard against Neon, all five ceilings | VERIFIED PASS |
| 9 | Security, reachability, integration and production-config tests | VERIFIED PASS |
| 10 | Defects fixed rather than documented around | 6 fixed, listed below |

---

### 1. Webhook endpoint — VERIFIED PASS

`we_1UDwrTAeDQ6sKD6Hh0xTNND5`, enabled, `livemode: false`, delivering to
`https://api-nodal.actorvia.xyz/v1/webhooks/stripe_credit`.

It subscribes to exactly the twelve events the adapter declares and no others:
the seven `payment_intent.*`, `charge.refunded`, and the four
`charge.dispute.*`. An event the adapter cannot model would be signed, kept as
evidence and recorded `IGNORED` rather than guessed at.

### 2. Signature validation — VERIFIED PASS

Against the live endpoint, over the public internet, in `test/deployed`:

| Delivery | Answer |
|---|---|
| No signature header | 400 |
| Signed with a different secret | 400 |
| Body changed after signing | 400 |
| Timestamp an hour old | 400 |
| Timestamp an hour ahead | 400 |
| Correctly signed | 200 |

A refused delivery leaves no `provider_events` row, so nothing can later claim
`signature_verified = true` about a signature that was never verified.

### 3. Idempotency — VERIFIED PASS

The same signed delivery twice: 200 then 200, one `provider_events` row, one
`inbox_messages` row, one evidence object, the same evidence reference, and the
same recorded disposition. An event id reused with different bytes is refused
with 400 rather than accepted as a repeat.

### 4 and 5. Purchase lifecycle, refunds, disputes, reversals — BLOCKED_EXTERNAL

**A credit purchase cannot be started against this deployment, and the reason
is a control working correctly.**

`PurchaseService.StartPurchase` calls `gates.RequireActive(CREDIT_PURCHASE)`.
The production `capability_gates` table has **zero rows**, so the verdict is
`ReasonNoGateRow` and the purchase is refused with `CAPABILITY_NOT_APPROVED`.

Activating that gate is not something this session can do, by design:

- `Admin.Propose` records a proposer and requires all four evidence references
  — legal review, provider contract, risk approval, security approval.
- `Admin.Approve` refuses the proposer: *"proposer cannot approve their own
  proposal"*.
- `Admin.Activate` refuses both of them: *"the approving principal cannot also
  activate; a distinct principal is required"*.

Three distinct principals, each with a step-up authentication, and four
external approval references that a person has to produce. That is what the gate
is for, and satisfying it by writing a row would be defeating it.

**[corrected 2026-09-09]** Two things this said were wrong. The window is not
`CP_AUTH_STEP_UP_MAX_AGE`: that variable was read by nothing, and every window
was a hard-coded constant — 15 minutes rather than the 5 the blueprint set
(F-89, now the tighter of the two). And "three distinct principals" is three
distinct `users.id` values: distinctness is a string comparison, there is no
person entity and no uniqueness on email, so three ZITADEL accounts under one
person's control satisfy the whole ceremony. It is a control against one careless
operator, not against one determined one (F-93).

What **is** verified, against the real Neon database, is every behaviour the
lifecycle consists of. `internal/credit`'s integration suite passes in full:

| Test | What it proves |
|---|---|
| `TestPAY001_OnePaymentCreatesExactlyOneFundingRecord` | one payment, one funding |
| `TestPAY002_CreditAmountIsDerivedServerSide` | the client cannot say how many Credits it bought |
| `TestPAY003_DuplicateWebhookCreatesNoDuplicateCredits` | minting is exactly once |
| `TestPAY004_RefundIsHandledSafely` | a refund unwinds the Credits |
| `TestPAY005_DisputeAffectsPayoutEligibilityImmediately` | disputed value stops being withdrawable |
| `TestPAY006_ChargebackCannotCreateFreeWithdrawableValue` | a chargeback after the Credits are spent |
| `TestIntegration_ChargebackAfterTheCreditsAreSpent` | the same, through the ledger |
| `TestReconcile_AdoptsTheProvidersView` | the provider's record wins |
| `TestSettleDue_PromotesTheLotAndNotJustTheRow` | settlement moves the lot, not only the funding |
| `TestSEC003_AProviderEventCannotCreditAnotherUsersAccount` | metadata cannot redirect Credits |
| `TestSEC_AnEventCannotRaiseTheAmountThatWasPaid` | the event cannot inflate what was paid |

The gap that remains is narrow and worth naming precisely: these run the real
state machine and the real SQL against the real database engine, with the
provider behind a test double. What has not been exercised is a real Stripe
PaymentIntent moving through the deployed service. That needs the gate.

### 6. Reconciliation — VERIFIED PASS

Performed against the Stripe sandbox with the authenticated Stripe CLI, and
compared with the deployment's own database:

| | Stripe | Nodal |
|---|---|---|
| Payment intents | 0 | — |
| Charges | 0 | — |
| Refunds | 0 | — |
| Disputes | 0 | — |
| `credit_fundings` | — | 0 |
| `credit_lots` | — | 0 |
| `provider_events` | — | 29, all `IGNORED` |
| `provider_evidence` | — | 35 |

The two sides agree: no money has moved anywhere, no Credits exist, and every
event the deployment recorded was correctly classified as another product's.
The 35 evidence objects against 29 events are the six deliveries refused at the
inbox after their bytes were archived — evidence is preserved before anything
decides what to do with it, which is the design.

`test/deployed/reconcile_test.go` runs the same comparison in both directions
whenever `NODAL_STRIPE_API_KEY` is supplied: no charge carrying our metadata may
be unknown here, no funding here may be unknown there, and the money-at-risk
ceiling may never count more than the provider says exists.

### 7. Evidence — VERIFIED PASS

Against the deployed database, with the deployment's own credentials:

- archived bytes are byte-for-byte the delivered bytes;
- the stored digest is the digest of the stored bytes, and `byte_len` agrees;
- `cp_app` holds no `UPDATE`, `DELETE` or `TRUNCATE` on `provider_evidence`,
  and does hold `INSERT`;
- `cp_ops` attempting an `UPDATE` is refused by privilege (`42501`);
- `cp_migrate`, which **owns** the table and is unconstrained by privilege, is
  refused by the guard trigger (`LG003`) on both `UPDATE` and `DELETE`;
- `cp_app` cannot update `provider_events.payload_hash`, `raw_ref`,
  `signature_verified` or `provider_event_id`.

Both statements of the rule were exercised separately, because they fail
differently and a test that drove only one would pass while the other was
missing.

### 8. Capacity guard — VERIFIED PASS

All seven `internal/capacity` integration tests passed against Neon.

**[corrected 2026-09-09]** Three subtests fail against a freshly migrated
database: they set each ceiling from a live measurement, and a measurement of
zero makes the assertion vacuous, so they passed because Neon already had rows
(F-92, now seeded). There are **four** ceilings, not five — the fifth row
below is an error path. The launch-cohort ceiling was enforced nowhere at all
(F-91), and the money-at-risk ceiling could not drain on this topology, which
would have refused every purchase forever at $2,000 of lifetime sales (F-90).
All three are fixed. The four ceilings:

| Ceiling | Behaviour |
|---|---|
| Launch cohort (50 accounts) | refuses at the measured value, admits one below |
| Rolling 24-hour purchases (200) | refuses at the measured value, admits one below |
| Money at risk (200,000 minor) | refuses at the measured value; an amount cannot vault over it |
| Database storage (500 MB × 0.67) | refuses before the quota, leaving room to reconcile and export |
| Measurement failure | `ErrUnmeasurable` wrapped in `AT_CAPACITY` — a refusal, never a pass |

### 9. Security, reachability and production config — VERIFIED PASS

Against the live deployment: valid certificate for the name, TLS 1.2 or better,
expiry beyond a week; HSTS with `includeSubDomains` and a one-year max-age
surviving the CDN; `nosniff`, `DENY`, `strict-origin-when-cross-origin`,
`frame-ancestors 'none'`, `same-origin`, `no-store`; no `/debug/pprof`,
`/debug/vars`, `/metrics`, `/.env` or `/v1/config`; an unconfigured `Origin`
neither echoed nor wildcarded; `/v1/payments`, `/v1/credits/balance` and `/v1/payouts` refuse without a
session — **[corrected 2026-09-09]** with 401 once their parameters
validate, and with 400 before that: the generated request validator runs ahead of
the authorization middleware, so an unauthenticated caller reaches it (F-84); `/v1/auth/login` redirecting to the
configured Zitadel issuer with `code_challenge_method=S256`, a state, a nonce
and `response_type=code`; liveness and readiness answering separately.

The configuration hash the running process reports equals the hash computed
from `render.yaml`, which is the check that catches a value edited in a
dashboard and never written back.

Repository suites: `go test ./...` green; `test/security` and
`test/reachability` green, and `test/security` green again with the integration
tag against an isolated database; `internal/credit`, `internal/webhook`,
`internal/proof` and `internal/capacity` integration suites green against Neon.

---

## Defects found and fixed

Each was invisible to every test in the repository before it was found.

**1. The money-at-risk ceiling could not see captured money.** The query summed
four funding states out of thirteen and omitted `CAPTURED` — money already
taken from the payer, one transition from minting Credit. The ceiling measured
less exposure than existed and admitted purchases past the cap. The list is now
every non-terminal, non-settled state, and `internal/credit` fails if a state is
added without being classified.

**2. There was no environment in which production rules applied and the
provider ran against its own sandbox.** The deployment said `PROD` and every
provider said `sandbox`; the adapter refused to build, and the refusal was a
warning nobody reads plus a silently disabled capability on a service answering
200 on every health check. The only visible symptom was a 404 on the webhook
route. `STAGING` now means production minus real money, enforced in both
directions: `PROD` is live only, `STAGING` is sandbox only.

**3. The Stripe account the credentials must belong to was never stated.** The
adapter compares the account its key answers for against the configured one and
refuses when there is nothing to compare against — correctly. Naming it also
makes a key rotated to the wrong account visible rather than a silence in which
charges succeed under somebody else's business.

**[corrected 2026-09-09]** It is not a refusal to start. `wireCreditPurchase`
logs a warning and returns an empty wiring, so the webhook route answers 404
while `/v1/healthz` answers 200 — the same shape as this document's own
defect #2. Recorded in F-93.

**4. A provider doing exactly what its delivery contract says was told to retry
forever.** Evidence is archived before the inbox deduplicates, and the archive
key is the event id plus the payload hash — so every retry lands on the same key
with the same bytes. The write-once archive refused, the pipeline read that as
"archive unavailable" and answered 503, which is a request for another retry.
Proved against the live deployment: the second delivery of one signed event came
back 503. The archive now distinguishes an identical replay from an attempted
rewrite, in the error rather than only in its message, and nothing is ever
overwritten.

The reason no test caught it is the more useful half: the webhook test double
overwrote whatever was under the key and never failed, so the duplicate-delivery
tests — including a property test firing up to eight deliveries — ran against an
archive with no write-once behaviour to get wrong.

**5. Ten variables were outside the table that proves what is running.** Four
transport rate limits, the enabled-capability list, the legal policy, two
funding-quote variables and two API timeouts were read straight from the
environment by `cmd/api`. Each was therefore undocumented, invisible to
`scripts/configcheck`, and absent from the configuration hash — so a transport
budget or the capability list could be changed in a dashboard while
`/v1/version` reported the same hash that exists to detect exactly that.

**6. And the test that stops the next one.** `test/infra` now reads every `CP_*`
name the binaries mention and requires it to be in the table. `cmd/api` is held
to zero. The seven worker binaries carry forty-eight more, frozen as a written
backlog so the debt is countable and cannot grow.

---

## What has to happen before real money

1. **Activate the `CREDIT_PURCHASE` capability gate.** Three distinct
   principals, each with a recent step-up, and four approval references. This
   is the control that decides whether the deployment may sell Credits.
2. **Switch to live Stripe.** `CP_ENV` to `PROD`, every `CP_PROVIDER_*_MODE` to
   `live`, and live keys in `NODAL_STRIPE_API_KEY` and
   `NODAL_STRIPE_WEBHOOK_SECRET`. A live Stripe webhook endpoint has to be
   created and its own signing secret installed; the sandbox endpoint's secret
   will not verify live deliveries.
3. **Re-run `test/deployed`** against the live deployment. Every test in it is
   safe to run against a deployment carrying real money: the deliveries it sends
   are foreign payments the pipeline ignores, and nothing it does writes to the
   ledger.
4. **Watch the ceilings.** The launch tier admits 50 accounts, 200 purchases a
   day and $2,000 at risk. All three are stated in `render.yaml` and refuse
   rather than degrade. See `LAUNCH_TIER.md` for the migration thresholds.

## What is still disabled, and correctly

- **Funding (the crypto onramp)** — the slot names an adapter but has no API
  key, so it refuses to build and the funding endpoints are absent. The
  blueprint says so: only `credit_purchase` is exercised by this tier.
- **Payout** — same, and additionally gated on `PAYOUT_RESERVE` and
  `PAYOUT_SETTLE`.
- **Every other capability** — `CP_API_ENABLED_CAPABILITIES` names
  `CREDIT_PURCHASE` alone, and a capability absent from that list is inactive
  whatever its gate row says.
