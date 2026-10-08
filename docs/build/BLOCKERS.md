# BLOCKERS

Things that cannot be completed from inside this repository, and exactly what would unblock each.

The bar is the one `gola.md` PART IV sets. `BLOCKED_EXTERNAL` is only for facts that genuinely require
a human, a provider or counsel. Missing code, tests, migrations, runbooks, mocks, contract tests,
chaos tests and admin tools are **not** blockers — they are work, and they are tracked in
`MASTER_BUILD_STATE.md` instead.

Every entry states what the repository already does, so that when the external fact arrives the
remaining work is activation rather than construction.

The ids this file used before commit `088f8e6`, `EB-001` to `EB-017` and `SB-001` to `SB-009`, are
kept under "Earlier ids (EB-, SB-), kept so older documents still resolve" below, and must not be
reused.

---

## B-01 — No payout provider contract exists · BLOCKED_EXTERNAL

**Decision needed.** Which licensed provider will actually send value to a user, under what
commercial terms, for which destination types and currencies.

**Why external.** A provider contract is signed by people. No amount of engineering produces one, and
PART XVIII explicitly forbids implementing an adapter against an unverified API — building one from
guessed endpoint names would be worse than having none, because it would look finished.

**What the code already supports.**
- `internal/payout.Provider` — the interface, shaped around what any payout provider must do (accept
  a request under a key we chose, say later what happened to that key, describe what it supports)
  rather than around one vendor's request body.
- `internal/payout.Registry` — refuses a provider with no `ContractReference` unless built with
  sandboxes allowed, which production never is. Refuses any provider that cannot answer a lookup,
  because a timed-out submission to one would be permanently ambiguous.
- `internal/payout/payouttest.Sandbox` — a faithful fake, including the inconvenient behaviours: the
  same key is the same payout, and a submission can succeed and lose its response on demand.
- The full request lifecycle, reservation, provenance consumption, settlement, failure return,
  `PAYOUT_STATUS_UNKNOWN` and reconciliation are implemented and tested against that sandbox.

**Evidence required to close.** A signed agreement naming the provider, the destination types, the
currencies, the jurisdictions, and the provider's own statement of idempotency and lookup semantics.

**Capabilities currently disabled.** `PAYOUT_RESERVE`, `PAYOUT_SETTLE`.

---

## B-02 — Whether Credits or trading proceeds may be paid out at all · BLOCKED_EXTERNAL

**Decision needed.** For each `CreditOrigin`, in each jurisdiction: may value of that provenance
leave the system, and under what conditions.

**Why external.** This is a legal determination about the economic substance of the internal economy.
`gola.md` PART IX is explicit that the mapping is policy, that counsel may approve a different one,
and that the code must not hardcode a business truth.

**What the code already supports.**
- `valuedomain.Policy` — versioned, hashed, covering every origin explicitly. `Validate` refuses a
  policy that omits an origin, that permits payout without naming a capability gate, or that permits
  payout below `PAYOUT_KYC` verification.
- `valuedomain.DefaultPolicy` — every origin forbidden. A fresh deployment pays nobody out, and the
  integration tests assert that even a fully verified user with settled funds and every capability
  active is refused under it.
- `payout.Engine` — decides per unit and records the policy version and hash on the decision, so a
  decision made under one policy can still be explained after two more have replaced it.

**Evidence required to close.** A written determination per origin and jurisdiction, recorded as a
policy version with an approval reference.

---

## B-03 — Whether pre-KYC participation in native markets is permissible · BLOCKED_EXTERNAL

**Decision needed.** May a user trade Nodal-native assets before any financial identity verification,
and in which jurisdictions.

**Why external.** PART XIX states the target experience (KYC at exit) and immediately warns against
assuming it is available. It is a money-transmission and consumer-protection question.

**What the code already supports.** `legalrouter` keys a capability on jurisdiction, product, value
origin, agent authority and verification level together, so the answer can be as narrow as the
determination is. The shipped `ConservativePolicy` denies native-market trading everywhere and
permits only simulation, which is the correct starting position rather than a placeholder.

**Capabilities currently disabled.** `NATIVE_MARKET_TRADING`, `NATIVE_ASSET_CREATION`.

---

## B-04 — Credit purchase provider production credentials · BLOCKED_EXTERNAL

**Decision needed.** Live credentials and a production account with the payment provider used to sell
Credits.

**What the code already supports.** `internal/credit` implements the funding lifecycle — thirteen
states as of migration 00728 — mints on `CAPTURED → REVERSIBLE`, promotes to settled only when the
funding settles, and handles chargeback with a recorded `DEFICIT` rather than a negative balance.
`internal/provider/stripecredit` is the adapter behind `credit.PurchaseProvider`, and
`credit.PurchaseService` drives the lifecycle from verified provider events. Acceptance criteria
PAY-001 through PAY-006 are integration tests against a real database.

**What changed on 2026-09-08.** The provider is chosen: Stripe, PaymentIntents, on the existing
Actorvia account. Credentials are still not configured — `stripe login` is pending — so this
blocker is narrower than it was but is not closed. See
`docs/providers/STRIPE_PRODUCTION_CHECKLIST.md` Stage 1.

**Capabilities currently disabled.** `CREDIT_PURCHASE`. Note that B-09 must close first: having
credentials is not having permission.

---

## B-05 — Hosted partner rail: no provider selected · BLOCKED_EXTERNAL

**Decision needed.** Which licensed partner carries hosted customer accounts.

**Why this is not merely unbuilt.** An adapter cannot be written against an unverified API, and the
partner's capability set (does it support hosted USD, hosted crypto, statements, tax artifacts,
webhooks) is a contractual fact rather than an engineering choice.

**What the code already supports.** `valuedomain.RailHostedPartner` is declared with its authority
model — the provider is authoritative for balances and Nodal holds a mirror — and the ledger's
value-domain isolation already refuses to let hosted value share a transaction with internal Credits.

**A defect this document found and closed.** `RailHostedPartner.Implemented()` returned `true` while
no adapter existed. Writing this entry is what surfaced it. It now returns `false`, so the Settlement
Compiler cannot route to a rail with nothing behind it — PART XXII's "do not implement live
unsupported products merely because an interface exists", enforced rather than intended.

---

## B-06 — Financial identity verification provider · BLOCKED_EXTERNAL

**Decision needed.** Who performs KYC, and whether their verification is accepted by the payout
provider (the two are not the same question).

**Answered for the Stripe rail, 2026-09-08.** Stripe performs it, and its own verification is by
definition accepted by itself. With a connected account configured `dashboard: express` — which the
stablecoin payout product requires anyway — `requirements_collector` computes to `stripe`, so Nodal
never collects a government identity document at the payout boundary. That is goal Section 16
satisfied by a required setting rather than by restraint. What remains open is reaching
`PAYOUT_KYC` at all, which needs B-10 and B-11.

**What the code already supports.** `valuedomain.VerificationLevel` separates Nodal identity from
financial identity from enhanced diligence. `payout.EligibilityInput` takes the level as an input and
`Decision.VerificationWouldSuffice` distinguishes "you cannot" from "you have not verified yet", so
the product has something useful to say the moment a provider exists.

**Current state.** `cmd/api` resolves the verification level Nodal can establish BY ITSELF and no
level above it: `NODAL_IDENTITY` when the identity provider asserted a verified email address
(persisted as `users.email_hash`), otherwise NONE. `PAYOUT_KYC` and `ENHANCED` are unreachable and
the payout engine refuses accordingly, which is what this blocker actually blocks.

It used to resolve NOTHING — every account `VerificationNone` — on the reasoning that a deployment
that cannot establish identity has not established it. That is right about a KYC provider and wrong
about this rung: `NODAL_IDENTITY` is "a verified email address and/or passkey", which this system
establishes at login. Every Domain A action requires exactly that level, so the over-wide reading
made the entire internal economy unreachable in every deployment (F-26). A blocker that blocks more
than it should is not conservative; it hides working software behind an external decision that has
nothing to do with it.

A passkey-only account with no email address still reports NONE, because only the email half is
recorded. That is under-reporting in the same direction and is named rather than hidden.

---

## B-07 — Age and jurisdiction matrix · BLOCKED_EXTERNAL

**Decision needed.** The per-jurisdiction age policy and the list of jurisdictions in which each
product may operate.

**What the code already supports.** `nativeasset.PolicyProfile.MinimumAge` defaults to 18 and is
frozen at activation; `legalrouter` carries jurisdiction as a first-class key dimension and matches
comma-separated sets, so a determination covering three states is one rule.

---

## B-08 — Independent security review and penetration test · BLOCKED_EXTERNAL

**Why external.** A review by the team that wrote the code is not an independent review.

**What the code already supports.** `docs/threat-model/THREAT_MODEL.md`, the `test/security` suite
(cross-tenant probes, forged sessions, SQL-source constancy analysis, agent escalation, webhook
forgery), `gosec`, `govulncheck`, `gitleaks` and a supply-chain script, all runnable today.

---

## B-09 — Stripe restricted-business review for stored value and marketplace · BLOCKED_EXTERNAL

**Decision needed.** Stripe's approval to sell Nodal Credits, and to settle creator sales, on
account `acct_1REGPQALyMyuBFc1`.

**Where requested.** Stripe Dashboard, account status / restricted business review, against the
business description in `docs/providers/STRIPE_BUSINESS_MODEL_REVIEW.md`.

**Current status.** Not requested. The description is drafted and has not been shown to Stripe.

**What it blocks.** `CREDIT_PURCHASE` and `MARKETPLACE`.

**Why it is a real category and not caution.** Stripe's published policy lists as restricted
"Preloaded payment cards, gift cards, virtual credits, or other products and services in which a
monetary value is stored", and "Payment facilitation and aggregation (including receiving settlement
proceeds for goods or services that you did not provide)". A Nodal Credit is the first sentence and
internal commerce is the second. There is no reading of the product under which those sentences are
about something else.

**The account-level risk, stated once.** This review happens against the account that currently runs
live payments and a prepaid-card issuing programme with Cross River Bank. (An earlier draft said
Treasury; the live API reports Treasury is not onboarded.) A denial does not land on an empty
account.

**A second thing the review will see.** The account's declared
`business_profile.product_description` describes software tooling for game developers sold by
subscription. It says nothing about stored-value Credits, a user-created asset market, marketplace
settlement or crypto payouts. Processing Nodal's payments against that description is processing
outside the declared business, and correcting it is itself what triggers the review. The owner was told this before any configuration work began and confirmed the
direction; `docs/providers/STRIPE_ACCOUNT_STRUCTURE.md` records it.

**Evidence required to close.** A written Stripe approval naming the account and the activities.

---

## B-10 — Stripe stablecoin payout private preview · BLOCKED_EXTERNAL

**Decision needed.** Stripe granting this account access to stablecoin payouts for Connect.

**Where requested.** Four steps, in order: be a Connect platform; request private-preview access
through Stripe sales at https://stripe.com/use-cases/crypto#request-invite; request the feature at
https://dashboard.stripe.com/stablecoin-payouts/overview; complete the due-diligence questionnaire
on the account status page.

**Current status.** None of the four has been started. The account is not yet a Connect platform:
`controller.type` is `account` and `GET /v1/accounts` returns an empty list.

**Confirmed on the account, 2026-09-08.** The live payment method configuration reports
`"crypto": {"available": false}`. Stripe's own documentation names that flag as the way to verify
the product is granted, so this is account-specific evidence rather than an inference.

**A prerequisite question nobody has asked Stripe yet.** `business_type` on this account is
`individual`. Stablecoin payouts require a US Connect platform, and whether a sole-proprietor
standard account may become one is not answered by any public document.

**What it blocks.** `PAYOUT_RESERVE` and `PAYOUT_SETTLE`, and with them every crypto payout.

**What the code already supports.** `internal/provider/stripepayout` is written against the
documented product and reports `Availability: REQUIRES_APPLICATION`, so `Submit` refuses before
touching the network. `payout.Capabilities` now carries supported assets, supported networks,
recipient kinds, who performs KYC and who holds the destination, and the registry refuses a crypto
adapter that names no asset or no network.

**What is known about the product, and is not negotiable by us.** USDC only. Base and Polygon only —
**not Solana**. US platforms only. Individuals and sole proprietors only; companies and non-profits
are not supported. 67 recipient countries, excluding the US states of New York and Hawaii.

**Evidence required to close.** Stripe confirming the preview is granted, and **Crypto** showing as
active in the account's Connect payment method settings.

---

## B-11 — Stripe Connect platform profile · BLOCKED_EXTERNAL

**Decision needed.** Completing the Connect platform profile, which declares the business model to
Stripe on this account.

**Why external.** It is a business-model declaration with the same review consequences as B-09, and
goal Section 67 makes it a stop.

**What it blocks.** Every payout, because the recipient model requires connected accounts; and
`MARKETPLACE` settlement.

**What the code already supports.** The payout architecture assumes the Recipient configuration with
`dashboard: express`, which is what makes Stripe rather than Nodal responsible for collecting
identity requirements — the answer to goal Section 16, established from Stripe's own documented
rule rather than chosen.

**Evidence required to close.** A completed platform profile and Connect showing as configured.

---

## B-12 — No deployed environment, and no authenticated non-root AWS role · BLOCKED_EXTERNAL

**Decision needed.** An authenticated AWS session under a role Terraform may use, and the manual
state-bucket bootstrap that Terraform cannot do for itself.

**What is NOT missing.** An AWS account exists: `049286562577`. The Terraform is written, validates
for all three environments and is correctly formatted. Twelve modules cover network, RDS, Redis,
S3 with Object Lock, KMS, Secrets Manager, the ECS cluster and services, the ALB with WAF,
observability and the GitHub OIDC deploy role.

**What is missing.** An authenticated session (`aws sts get-caller-identity` fails). A role that is
not root — the configured identity is `arn:aws:iam::049286562577:root`, and running Terraform as
root is not something to do once, let alone routinely. The state bucket and its KMS key, which are
bootstrapped by hand because Terraform cannot create the bucket that holds its own state. A
hostname and an ACM certificate. The deploy role's GitHub OIDC trust, which names the repository.
(This entry said the GitHub repository did not exist. It already did: `origin` is
`https://github.com/wdwd720/Nodal.git`, pushed since 2026-09-06 — SB-004 under "Earlier ids" below —
and it answered `git ls-remote` on 2026-10-08.)

**Evidence required to close.** A named IAM role with a Terraform policy, a bootstrapped state
bucket, and `backend.hcl` written from the example.

**Capabilities currently disabled.** None directly. Everything, indirectly: nothing can serve a
webhook, so no Credit purchase can complete outside a local run.

**What changed on 2026-09-09 and 2026-09-10.** By the two records below, a deployed environment
exists outside AWS, and most of what this entry called missing on AWS exists too.
`docs/operations/PATH_B_STATUS.md` (2026-09-09) records a non-root deployment identity,
`nodal-terraform`, assumed with MFA; the state bucket `nodal-terraform-state-049286562577-us-east-2`;
an initialised backend, with `terraform plan` succeeding against real credentials (283 resources to
add); and an ACM certificate issued for `api-nodal.actorvia.xyz`. No apply is recorded; §4 and §5 of
that file list what one still waits on: a sizing decision, a TLS OTLP collector, a paging endpoint
and an alert address. `docs/operations/PROVIDER_ACTIVATION_CHECKPOINT.md` (2026-09-10) records a
`STAGING` deployment on Render, the $0 launch tier of `docs/operations/LAUNCH_TIER.md`, serving that
hostname against Stripe's sandbox with its webhook endpoint verified. So on that date a webhook
could be served, and what stopped a Credit purchase outside a local run was the `CREDIT_PURCHASE`
gate, which had no row in that deployment, and, for real money, B-04 and B-09. Neither record has
been re-checked against AWS or Render since.

---

## B-13 — No OIDC identity provider · BLOCKED_EXTERNAL

**Decision needed.** Which identity provider authenticates users, and its issuer, client id and
client secret.

**Why external.** `CP_AUTH_MODE` is `oidc` in STAGING and PROD, and `dev` is refused there by
`config.Validate`. There is no fallback: a deployed environment with no issuer has no way for
anybody to log in.

**What the code already supports.** `internal/auth/oidc` is implemented and tested, and
`internal/auth/devidp` serves LOCAL and TEST only.

**Answered on 2026-09-09: use Auth0.** `docs/operations/IDENTITY_PROVIDER.md` derives the
requirements from the code rather than from preference. Three of them eliminate candidates:
`email_verified`, without which every account sits at verification level NONE and the whole
internal economy is unreachable (F-26); `auth_time`, without which every step-up action is refused;
and an `amr` claim carrying one of `mfa`, `otp`, `hwk`, `swk`, `pop`, `webauthn` or `passkey`.

That last one decides it. Step-up is verified by `amr` and not `acr`, and step-up gates break-glass
elevation and the dual-control approval path — which are what activate a capability gate. An issuer
that never emits a strong `amr` makes `CREDIT_PURCHASE` permanently impossible to activate whatever
Stripe approves.

**AWS Cognito was the obvious choice and is rejected.** Its documented ID token payload carries no
`amr` claim at all. A pre-token-generation Lambda could add one, but the claim is an assertion about
how somebody authenticated, and synthesising it would defeat step-up everywhere it is used,
including the approval path for live money movement.

**Evidence required to close.** An Auth0 tenant, its issuer URL, a client id, and a client secret in
Secrets Manager. The tenant's discovery document should be checked first: `oidc.New` now refuses an
issuer that publishes a PKCE method list without S256, or an `acr_values_supported` list sharing
nothing with the configured step-up value.

**What changed on 2026-09-09 and 2026-09-10.** Auth0 was replaced the same day: its free plan does
not include MFA, and an issuer that stops emitting a strong `amr` when the trial ends makes
`CREDIT_PURCHASE` impossible to activate (`docs/operations/LAUNCH_TIER.md` §4). Zitadel was chosen
instead. `docs/operations/PROVIDER_ACTIVATION_CHECKPOINT.md` (2026-09-10) records the `STAGING`
deployment configured against the Zitadel issuer `https://nodal-az1hxe.us1.zitadel.cloud`, and
verifies that `/v1/auth/login` redirects to it with `code_challenge_method=S256`, a state, a nonce
and `response_type=code`. The issuer and client id are in `render.yaml`; the client secret is a
Render secret, not a Secrets Manager entry. No completed sign-in is recorded, so requirements 3, 5
and 9 of `IDENTITY_PROVIDER.md`, which only a real login proves, are not verified.

---

## B-14 — Hostname and TLS certificate · BLOCKED_EXTERNAL

**Decision needed.** The public hostname the API serves on, and approval to create the DNS record
and certificate for it.

**What is known.** `actorvia.xyz` is controlled: it is the Stripe account's declared URL, it serves
the live billing webhook, and DNS is managed at GoDaddy. The site sends
`Strict-Transport-Security: max-age=63072000; includeSubDomains; preload`, so any subdomain is
HTTPS-only in browsers from the first request and needs a valid certificate before it is usable.

**Recommended.** `api-nodal.actorvia.xyz`, a new record that touches neither the apex nor `www`, so
the Vercel site and the existing Stripe webhook are unaffected. See
`docs/operations/DEPLOYMENT_GAP_ANALYSIS.md` §6.

**What it blocks.** The Stripe webhook endpoint, which is
`https://<hostname>/v1/webhooks/stripe_credit`, and with it every Credit purchase that is not run
locally.

**What changed on 2026-09-09 and 2026-09-10.** Both exist. The recommended name was taken: ACM
issued a certificate for `api-nodal.actorvia.xyz` on 2026-09-09, kept for the AWS load balancer
(`docs/operations/DNS_RECORDS.md`), and on 2026-09-10 the name served the `STAGING` deployment on
Render with a valid certificate, TLS 1.2 or better and HSTS
(`docs/operations/PROVIDER_ACTIVATION_CHECKPOINT.md` item 9). Stripe's sandbox webhook endpoint
delivers to `https://api-nodal.actorvia.xyz/v1/webhooks/stripe_credit` (item 1), so this entry no
longer blocks the webhook; B-12 says what stopped a Credit purchase there instead.

---

# Advanced financial capabilities (ADR-0020)

Every advanced capability is built, tested and runnable in PAPER, SANDBOX or DEVNET mode. What follows
is what stands between each one and real customer value. None of these is software: in every case the
code path, the gate, the evidence fields and the refusal already exist, and `gates.AdvancedSpecs`
names the evidence a live activation would require. Live is refused in code (`LIVE_NOT_IMPLEMENTED`)
and unrepresentable in the database (no `LIVE` economic intent, no non-SIMULATED house or facility
account) until a change resolves the relevant blocker below.

## B-15 — Licences or exemptions for regulated advanced activities · BLOCKED_EXTERNAL

**Decision needed.** For each of margin lending and leverage, futures, perpetuals and options,
equities and ETFs, an internal order book, principal trading, performance-based fees, a strategy
marketplace, customer-to-customer value transfer and event contracts: which licence, registration or
exemption would permit offering it with real value, in which jurisdictions.

**Why external.** These are determinations by counsel and, in most cases, by a regulator (broker-dealer
or FCM registration, a designated contract market or swap execution facility, investment-adviser
registration, money-transmission licences). No code produces one.

**What the code already supports.** The full simulated implementation of each, its capability gate
(`MARGIN`, `LEVERAGE`, `DERIVATIVES`, `EQUITIES`, `INTERNAL_MATCHING`, `PRINCIPAL_TRADING`,
`PERFORMANCE_FEES`, `STRATEGY_MARKETPLACE`, `P2P_TRANSFERS`, `PREDICTION_MARKETS`), and a spec listing the
`REGULATORY_LICENSE_OR_EXEMPTION` evidence a live activation must attach.

**Evidence required to close.** A written legal determination per capability and jurisdiction, and the
licence or exemption reference itself.

**Capabilities currently live-disabled.** All ten above.

## B-16 — Stable-asset issuance: reserve custodian, attestation and issuer licensing · BLOCKED_EXTERNAL

**Decision needed.** Whether the platform may issue a redeemable value token at all (ADR-0018 says no
for real value), and if so the reserve custodian, the attestation firm and the licensing basis.

**What the code already supports.** `internal/stableasset`: reserve ledger, issuance cap, mint/burn
workflows with exactly-once semantics on a simulated chain, peg and reserve monitors with circuit
breakers, hash-linked reserve/supply snapshots. Capability `STABLE_ASSET_MINT` requires
`RESERVE_ATTESTATION` evidence for live.

**Evidence required to close.** Custody agreement, attestation engagement, legal opinion.

## B-17 — Execution and clearing partners for equities, derivatives and event contracts · BLOCKED_EXTERNAL

**Decision needed.** A broker-dealer with a production execution API for equities, a clearing member
or venue for listed derivatives, and a designated contract market for event contracts.

**What the code already supports.** `EquitiesExecutionAdapter` with a deterministic paper adapter and
recorded fixtures; the derivatives clearing house is a simulated facility; prediction markets run on
the internal book. Provider mode `live` is refused for every advanced adapter.

**Evidence required to close.** Signed partner agreements and verified sandbox credentials, followed by
contract tests against the partner's sandbox.

## B-18 — Delegated signing and withdrawal custody analysis · BLOCKED_EXTERNAL

**Decision needed.** Whether delegated, policy-scoped agent authority over a customer wallet is a
custody arrangement, and which wallet provider's policy engine is verified to enforce the scopes
(extends EB-002 and EB-005).

**What the code already supports.** Capability tokens with scope, limits, expiry, nonce and revocation;
an independent transaction inspector; bounded signing on the local simulated chain only; withdrawal
proposals with policy evaluation and optional human approval. Capabilities `AGENT_SIGNING` and
`AGENT_WITHDRAWALS` require `CUSTODY_ANALYSIS` evidence for live.

## B-19 — Independent review of the strategy sandbox · BLOCKED_EXTERNAL

**Decision needed.** An independent security review (and ideally a sandbox-escape engagement) of the
WASM runtime, its host configuration and the build worker before any user code runs in a deployed
environment.

**What the code already supports.** The runtime's own escape, resource-exhaustion and secret-absence
tests. `USER_CODE_SANDBOX` requires `SANDBOX_ISOLATION_AUDIT` evidence for live, and ADR-0011 still
forbids user code in any live path.

## B-20 — A bridge provider and public-testnet access · BLOCKED_EXTERNAL

**Decision needed.** Which bridge provider, with which documented finality and failure semantics; and
whether CI may reach public testnets.

**What the code already supports.** `BridgeProvider` with a deterministic simulated bridge over two
simulated chains (`internal/devnet`), including delayed, failed, reorged and duplicated messages.
Public-testnet adapters are not built: the deterministic local chain replaces them, which is the goal's
stated fallback ("use deterministic local providers when external credentials are unavailable").

---

## Earlier ids (EB-, SB-), kept so older documents still resolve

Until commit `088f8e6` (2026-09-07) this file numbered external blockers `EB-001` to `EB-017` and
software blockers `SB-001` to `SB-009`. The rewrite dropped them, and documents written before it
still cite them. Each row gives what the id meant and its status when this file last recorded it
(the file was last updated 2026-09-06, the SB-007 row on 2026-09-07), then where the fact stands
now: the entry above that tracks it, the evidence that resolved it, or the date it was last
assessed. These rows are a record, not a tracker: they are not updated, and a new blocker takes a
new `B-` id. The SB- ids were software blockers, which this file no longer lists (see the
introduction), so none of them has a `B-` successor.

Most of the ADRs that cite these ids, and `DECISION_REGISTER.md` D-004 and D-006, use them with
meanings this table does not have: EB-008 for managed Temporal, Redpanda and ClickHouse, which this
file called EB-014; EB-005 for Helius, which it called EB-010; SB-005 for a host without a C
compiler, which it called SB-006. Where a citing document and this table disagree, the document's
own words say what it meant.

| ID | Meaning, as this file stated it | Status when last recorded | Where it stands now |
|---|---|---|---|
| EB-001 | LEGAL. U.S. and California licensing or exemption determination. Blocked every `LIVE_*` capability. | Open. Software required first: a capability gate with a `legal_review_ref` field, and no live gate ACTIVE. | No current entry for the `LIVE_*` gates; B-15 asks the same kind of question only for the advanced capabilities. Not re-assessed since 2026-09-06. |
| EB-002 | LEGAL. Delegated-signing custody analysis (embedded-wallet control semantics). Blocked all live trading. | Open. Software required first: the signing boundary implemented and tested, and production startup failing closed if signing semantics are unverified. | Partly B-18, which says it extends EB-002: the same custody question, asked for `AGENT_SIGNING` and `AGENT_WITHDRAWALS`. For the live trading EB-002 blocked (`LIVE_MANUAL_TRADING`, `LIVE_AGENT_TRADING`) there is no current entry; not re-assessed since 2026-09-06. |
| EB-003 | PROVIDER. Stripe fiat-to-crypto onramp: commercial approval and production credentials; the sandbox is application-gated too. Blocked `LIVE_FUNDING`. | Open; software state met 2026-09-06 (adapter CODE_COMPLETE + CONTRACT_TESTED in `internal/provider/stripe` and `test/contract/stripe`, the webhook pipeline, the gate), so genuinely BLOCKED_EXTERNAL. | No current entry: B-04 and B-09 are Stripe blockers for selling Credits and settling creator sales, not for the onramp. Not re-assessed since 2026-09-06. |
| EB-004 | BUSINESS. Stripe fraud and dispute responsibility allocation, which sets the funding-reversibility policy values. | Open. Software required first: reversal handling with a configurable hold policy. | No current entry. Not re-assessed since 2026-09-06. |
| EB-005 | PROVIDER. Wallet provider (Privy) production credentials and verified signing semantics: idempotent replay of `signTransaction` is documented but unverified, and the policy engine cannot resolve lookup-table accounts. Blocked `LIVE_MANUAL_TRADING` and `LIVE_AGENT_TRADING`. | Open; software state met 2026-09-06 (`internal/wallet`, `internal/provider/privy`, `internal/signing`, `test/contract/privy`); production startup fails closed without `delegation_verified_at`. | Partly B-18, which says it extends EB-005: a wallet provider whose policy engine is verified to enforce the scopes, for `AGENT_SIGNING` and `AGENT_WITHDRAWALS`. Privy production credentials, the `signTransaction` replay question and the two `LIVE_*` gates have no current entry; not re-assessed since 2026-09-06. |
| EB-006 | LEGAL. Permitted asset universe decision. Blocked instrument activation. | Open. Software required first: an asset registry with status and policy reference, unlisted assets failing closed. | No current entry. Not re-assessed since 2026-09-06. |
| EB-007 | LEGAL. Strategy, adviser and CTA regulatory implications. Blocked `LIVE_AGENT_TRADING` and any marketplace. | Open. Software required first: a gate, and no personalised recommendations implemented. | Partly B-15, whose activities include a strategy marketplace (`STRATEGY_MARKETPLACE`) and performance-based fees, and whose determinations include investment-adviser registration. The adviser and CTA question for `LIVE_AGENT_TRADING` has no current entry; not re-assessed since 2026-09-06. |
| EB-008 | LEGAL. Provider data-retention and redistribution rights (social and market-data licensing). Blocked `SOCIAL_DATA_PERSISTENCE` and any long-term raw archive of licensed feeds. | Open. Software required first: retention classes and a capability gate. | No current entry. Not re-assessed since 2026-09-06. |
| EB-009 | BUSINESS. Final brand clearance (`PUBLIC_PRODUCT_NAME`, trademark). Blocked the user-facing product name. | Open. Software required first: `PUBLIC_PRODUCT_NAME` configuration, with the codename used only internally. | No current entry. Not re-assessed since 2026-09-06. |
| EB-010 | PROVIDER. Helius production credentials and plan. Blocked live chain observation. | Open; software state met 2026-09-06 (`internal/provider/helius`, `internal/provider/solanarpc`, the `internal/chain` agreement policy, their contract tests), so genuinely BLOCKED_EXTERNAL. | No current entry. Not re-assessed since 2026-09-06. |
| EB-011 | PROVIDER. Jupiter API key and commercial terms. Blocked live execution. | Open; client software state met 2026-09-06 (`internal/provider/jupiter`, `test/contract/jupiter`), but wrapping it into `execution.ExecutionAdapter` and wiring the executor were still to do, so not yet BLOCKED_EXTERNAL end to end. | No current entry: B-17 is about partners for equities, derivatives and event contracts, not Solana execution. Not re-assessed since 2026-09-06. |
| EB-012 | PROVIDER / INFRA. AWS production account, IAM bootstrap, GitHub OIDC trust. Blocked staging and production deployment, the WORM audit archive, and CI/CD to AWS. | Open; software state met 2026-09-06: `infra/terraform` validates in dev, staging and prod, trivy clean, `DEPLOYMENT.md` written, a local MinIO archive, an OIDC deploy-role module; `plan` and `apply` impossible without the account. | B-12, which records that an AWS account now exists. |
| EB-013 | PROVIDER. Model provider (Anthropic) production API key and usage terms. Blocked natural-language strategy compilation in production. | Open. Software required first: a `ModelProvider` adapter and model budgets. | No current entry. Not re-assessed since 2026-09-06. |
| EB-014 | PROVIDER / INFRA. Managed Temporal, Redpanda and ClickHouse accounts. Blocked staging and production. | Open. Software required first: local containers and config-driven endpoints. | No current entry. Not re-assessed since 2026-09-06. |
| EB-015 | PROVIDER / LEGAL. Fiat off-ramp or withdrawal partner, and the custody path. Blocked `WITHDRAWALS`. | Open. Software required first: the withdrawal domain, state machine and gate implemented, with the capability DISABLED. | No current entry names `WITHDRAWALS`. B-01 (a payout provider, for the `PAYOUT_*` gates) and B-18 (custody analysis, for `AGENT_WITHDRAWALS`) ask related questions for other gates. Not re-assessed since 2026-09-06. |
| EB-016 | PROVIDER. Tax-reporting partner. Blocked any tax filing claim. | Open. Software required first: lot-level records preserved, and no filing claims made. | No current entry. Not re-assessed since 2026-09-06. |
| EB-017 | BUSINESS / PROVIDER. Identity provider selection (the OIDC issuer) and a production tenant. Blocked production authentication and the passkey experience. | Open. Software required first: an OIDC-generic `IdentityProvider` adapter, with the dev IdP rejected in STAGING and PROD. | B-13. |
| SB-001 | Go, make and terraform not installed on the build host at session start, so the core could not be built. | RESOLVED for Go 2026-09-05; make and terraform pending, queued behind a winget install stuck on UAC. | Go resolved: 1.27.0 installed from a SHA-256-verified zip. make and terraform not re-assessed since 2026-09-06, though the EB-012 row records `terraform validate` passing that day. |
| SB-002 | Docker daemon and the local compose stack not running at session start: no local Postgres, Redis, Redpanda, ClickHouse, Temporal or MinIO. | RESOLVED 2026-09-05. | Resolved: Docker Desktop launched and `docker compose up -d --wait` healthy with seven services, on offset ports because the host also ran unrelated containers on 127.0.0.1:8788 and 8789. |
| SB-003 | sqlc, buf, golangci-lint, staticcheck, govulncheck, gosec, k6, gitleaks, trivy and syft not installed, blocking the lint, scan and generate targets. | PARTIAL 2026-09-05: staticcheck, govulncheck, gofumpt, goimports, buf, oapi-codegen, gosec and golangci-lint in `./bin`; sqlc, gitleaks, trivy, syft and k6 via `scripts/tool install` (prebuilt, checksum-verified). | Not re-assessed since 2026-09-06. The SB-008 and EB-012 rows record gitleaks and trivy running that day. |
| SB-004 | No git remote or GitHub repository: CI workflows could not run, and there was no OIDC deploy identity. | RESOLVED 2026-09-06. | Resolved: the operator supplied `https://github.com/wdwd720/Nodal.git`, `main` was pushed (`673d9bf`), and CI went fully green (run 34062522222, commit `5143d0e`, all 18 jobs). Not covered by that: `release.yml`, which triggers on tags and had never run, and the OIDC deploy identity, which was EB-012 and is now B-12. B-12 says a GitHub repository does not exist; this remote answered `git ls-remote` on 2026-10-08. |
| SB-005 | Library selection for Solana transaction decoding and Token-2022 extension inspection, needed by the transaction inspector: `gagliardetto/solana-go` or a hand-written decoder, the decision to be recorded in `DECISION_REGISTER.md`. | OPEN. | Resolved, though this row never said so: `DECISION_REGISTER.md` D-019 pins `github.com/gagliardetto/solana-go v1.23.0` and records SB-005 resolved, and already did when this row last said OPEN. |
| SB-006 | No gcc on the build host: no cgo, so neither `go test -race` nor cgo-dependent tools. | RESOLVED 2026-09-05. | Resolved: WinLibs GCC 16.2 (SHA-256-verified) installed and `-race` verified on money, id, clock and errs. Production builds stay `CGO_ENABLED=0`. |
| SB-007 | The Jupiter v6 on-chain instruction layout in `internal/signing/inspect/jupiter.go` was reproduced from memory, so the inspector could reject valid swaps or misparse a variant. | LARGELY RESOLVED 2026-09-07, narrowed to an on-chain confirmation. | Resolved against the published IDL (`jup-ag/jupiter-cpi` at `12bc5f67`, committed as `internal/signing/inspect/testdata/jupiter_v6_idl.json`): everything the inspector relies on matched, including all 39 Swap variants, and `jupiter_idl_test.go` derives its expectations from that file, so the guard is no longer circular; both negative controls make it fail. Still open, with no current entry: proof that the deployed program matches the IDL (the on-chain IDL account or a decoded mainnet transaction), which a canary trade requires; Swap ordinals 39 and above remain from memory. Not re-assessed since 2026-09-07. |
| SB-008 | The entire build was uncommitted (one commit, 1,300+ untracked files, no remote): a disk failure or an errant delete would lose it, with no bisect, blame or per-stage diff. | RESOLVED 2026-09-06. | Resolved: 1,340 files committed as `673d9bf` and pushed to `origin/main`. Before the push `gitleaks detect` reported no leaks, a negative control confirmed realistic secrets in production paths are still caught, and no `node_modules`, `.terraform`, binaries or `.env` are tracked. |
| SB-009 | Production role bootstrap must never re-grant `cp_app` table-wide `UPDATE` on `capability_gates` or `INSERT` on `capability_gate_transitions`. Migration 00701 revokes both, and a re-grant would silently let anything holding the application credential activate a live-money capability. | OPEN: operator awareness, no action needed (verified 2026-09-06). | No current entry; it is a standing caution, not an external fact. As verified 2026-09-06, every `GRANT ALL` in the role bootstraps targets `cp_migrate`, and `TestIntegration_CapabilityGateStateAuthority` in `test/integration/migrations` pins the privileges, so a re-grant fails CI. Not re-assessed since 2026-09-06. |

---

## Not blockers

Recorded because their absence might otherwise look like one:

| Item | Status | Where it is tracked |
|---|---|---|
| Internal commerce / creator economy | not built | `MASTER_BUILD_STATE.md` Stage 8 |
| Hosted partner rail adapter | not built (and see B-05) | Stage 10 |
| Rails unified behind FinancialIntent | not built | Stage 12 |
| Frontend for Domain A | not built | Stage 16 |
| Admin tooling for Domain A | not built | Stage 17 |
| Chaos, load, restore drill for the new subsystems | not run | Stages 20–21 |
| Terraform for the new tables | not needed (schema is migration-managed) | — |
| Retention of SOCIAL_DATA, MODEL_IO and OPERATIONAL_LOG | declared in config, enforced by nothing | `AUDIT_FINDINGS.md` F-79; `MASTER_BUILD_STATE.md` §4 |
| The ten transition bindings that still compare the destination only | `agents` is fixed (D-044); the rest are not | `AUDIT_FINDINGS.md` F-78; `MASTER_BUILD_STATE.md` §4 |
| 131 enum CHECK constraints with no Go list compared against them | named individually by `test/integration/enums` | `AUDIT_FINDINGS.md` F-74 |

**Checkpoint 2026-09-08.** The F-71..F-81 batch added **no** external blockers.
Every item it left undone is work, and each is named above or in
`MASTER_BUILD_STATE.md` §4 with the reason it was left. `CP_DATABASE_OPS_URL`
(D-045) is a new deployment input, not a blocker: it is a database role the
repository already creates, and the local default is in `.env.example`.
