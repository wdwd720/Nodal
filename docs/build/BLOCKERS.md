# BLOCKERS

Things that cannot be completed from inside this repository, and exactly what would unblock each.

The bar is the one `gola.md` PART IV sets. `BLOCKED_EXTERNAL` is only for facts that genuinely require
a human, a provider or counsel. Missing code, tests, migrations, runbooks, mocks, contract tests,
chaos tests and admin tools are **not** blockers — they are work, and they are tracked in
`MASTER_BUILD_STATE.md` instead.

Every entry states what the repository already does, so that when the external fact arrives the
remaining work is activation rather than construction.

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
hostname and an ACM certificate. A GitHub repository, which does not exist and which the deploy
role's OIDC trust names.

**Evidence required to close.** A named IAM role with a Terraform policy, a bootstrapped state
bucket, and `backend.hcl` written from the example.

**Capabilities currently disabled.** None directly. Everything, indirectly: nothing can serve a
webhook, so no Credit purchase can complete outside a local run.

---

## B-13 — No OIDC identity provider · **RESOLVED 2026-09-10**

**Decision needed.** Which identity provider authenticates users, and its issuer, client id and
client secret.

**Why external.** `CP_AUTH_MODE` is `oidc` in STAGING and PROD, and `dev` is refused there by
`config.Validate`. There is no fallback: a deployed environment with no issuer has no way for
anybody to log in.

**RESOLVED.** ZITADEL, and it is not a claim about configuration — the deployed service performs
the authorization request. LIVE_OBSERVED against `https://api-nodal.actorvia.xyz` on 2026-09-10:

```
GET /v1/auth/login  ->  302
Location: https://nodal-az1hxe.us1.zitadel.cloud/oauth/v2/authorize
          ?client_id=390076573774056913
          &code_challenge_method=S256&code_challenge=...&nonce=...
          &redirect_uri=https%3A%2F%2Fapi-nodal.actorvia.xyz%2Fv1%2Fauth%2Fcallback
          &response_type=code&scope=openid+email+profile&state=...
```

A real PKCE challenge, a real nonce and the deployment's own redirect URI, issued by the running
service. The issuer's discovery document answers 200. `render.yaml` carries the issuer, the client
id and `CP_AUTH_CLIENT_SECRET_REF`; the secret itself is a Render environment value and is not in
this repository.

Kept in this file rather than deleted, because what unblocked it is the record of how the next
environment gets an issuer.

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

---

## B-14 — Hostname and TLS certificate · **RESOLVED 2026-09-10**

**Decision needed.** The public hostname the API serves on, and approval to create the DNS record
and certificate for it.

**What is known.** `actorvia.xyz` is controlled: it is the Stripe account's declared URL, it serves
the live billing webhook, and DNS is managed at GoDaddy. The site sends
`Strict-Transport-Security: max-age=63072000; includeSubDomains; preload`, so any subdomain is
HTTPS-only in browsers from the first request and needs a valid certificate before it is usable.

**Recommended.** `api-nodal.actorvia.xyz`, a new record that touches neither the apex nor `www`, so
the Vercel site and the existing Stripe webhook are unaffected. See
`docs/operations/DEPLOYMENT_GAP_ANALYSIS.md` §6.

**RESOLVED.** `api-nodal.actorvia.xyz` is the hostname, behind Cloudflare, and the certificate
verifies. LIVE_OBSERVED on 2026-09-10: `curl` reports `ssl_verify_result=0` and the service answers
on it — `/v1/healthz`, `/v1/readyz` and `/v1/version` all 200. The recommendation above is what was
done.

**What it blocks.** The Stripe webhook endpoint, which is
`https://<hostname>/v1/webhooks/stripe_credit`, and with it every Credit purchase that is not run
locally.

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

**Checkpoint 2026-09-09 (second) — after clearing §4.** Unchanged. Nothing in
F-94 through F-99, and nothing in the §4 queue, produced a new external blocker.
`docs/audit/LAUNCH_GATE_MATRIX.md` is now the single place the seven external
items are listed with what the code already does for each, and it is what a
reader should look at before this file.

**Checkpoint 2026-09-09 — after reconciling the provider workstream.**
The external blockers are unchanged in kind and sharper in detail. Selling
Credits for real money needs, and cannot be given from inside this repository:

| Item | Why external |
|---|---|
| `CREDIT_PURCHASE` capability activation | Three distinct principals with recent step-up and four approval references. Not fabricable, and doing so would defeat the control. Note F-93: three principals is three `users.id` values, not three people. |
| Stripe production approval | A business relationship and a restricted-business/compliance review. Not an engineering artefact. |
| Legal review reference | Counsel. |
| Independent penetration test | A third party. This audit is not one and does not claim to be. |
| Risk and security approval references | People with the authority to sign them. |
| A real settlement mint | `CP_API_SETTLEMENT_MINT` is a Solana devnet USDC mint; the mainnet value is a fact about Solana, not a value to invent (F-93). |

Everything else the reconciliation found was work, and is either fixed
(F-83, F-86 to F-92) or recorded with its reason in F-93.

**Checkpoint 2026-09-10 — two blockers resolved by the deployment.**
B-13 (no OIDC identity provider) and B-14 (hostname and TLS certificate) are
**RESOLVED**, and were already resolved in fact before this checkpoint wrote it
down — which is its own small instance of the defect class this repository
keeps recording. Both were verified LIVE_OBSERVED against the running service
rather than read out of `render.yaml`: the deployment issues a real PKCE
authorization redirect to ZITADEL, and the hostname serves a certificate that
verifies.

**Twelve blockers remain, and B-12 is not one of the twelve that stops launch.**
`aws sts get-caller-identity` still fails (`Your session has expired`), but the
$0 launch tier does not run on AWS — it runs on Render free, Neon free and
Cloudflare free. B-12 blocks the scale-up path, not the launch.

**Checkpoint 2026-09-08.** The F-71..F-81 batch added **no** external blockers.
Every item it left undone is work, and each is named above or in
`MASTER_BUILD_STATE.md` §4 with the reason it was left. `CP_DATABASE_OPS_URL`
(D-045) is a new deployment input, not a blocker: it is a database role the
repository already creates, and the local default is in `.env.example`.

**Checkpoint 2026-09-10 (productization) — the provider boundary, sharpened by research.**
Public provider documentation was read for the product goal (Tilia/Thunes, Stripe
Connect, Stripe stablecoin payouts, Bridge, Persona, Veriff, Sumsub, Stripe
Identity, Plaid IDV, Circle, Onfido, Alloy, Coinbase); the synthesis and the
provider-abstract contract are in `docs/product/PROVIDER_BOUNDARY.md`. No account
was created, no terms accepted, no sales team contacted, and no provider approval
is claimed. What the research adds to the register above, all human actions:

| Item | Sharpens | Why external |
|---|---|---|
| Read Stripe's restricted-business list in a browser and settle whether virtual credits are *restricted* (extra diligence) or *prohibited* (cannot launch on Stripe) | B-09 | Two automated reads of the page disagreed. Everything downstream depends on which it is. |
| The Stripe platform profile must describe Nodal honestly, including withdrawal of earned value | B-11 | An attestation by a responsible human. |
| Sign up for the identity-verification vendor (Veriff or Persona), accept terms, sign a DPA, decide retention | B-06 | Account creation and a contract; Persona Essential is an annual commitment unless its Startup Program is granted. Stripe Identity alone does no AML or sanctions screening. |
| Open the Tilia/Thunes conversation: pricing, sandbox, a review of the closed-loop-until-withdrawal model | B-01, B-05 | Sales-led; its entity succession after the Thunes acquisition is UNVERIFIED and needs a human to confirm. |
| Stripe stablecoin payouts: request the private preview and complete its due-diligence questionnaire; or obtain a Bridge developer account by email | B-10 | Attestations and gated sandboxes. California is supported by Stripe's preview; New York and Hawaii are not. |
| Whether Nodal's own closed-loop Credit float is stored value requiring a licence, independent of any payout provider | B-02 | Counsel. Stripe Connect does not make Stripe the transmitter for value Nodal holds. |
| 1099 filing election and thresholds; business KYB on Nodal by the Role C provider | B-11 | A tax adviser and the provider. |

The contract Nodal builds against all of them has no primitive by which Nodal
itself converts Credits to money; it instructs a licensed provider. That is a
deliberate property of the architecture, not a gap.
## Verification and the conversion request — checkpoint 2026-09-10

The software behind goal §19–§25 is built: the financial verification state
machine, the sub-check evidence model, the versioned age/jurisdiction/sanctions
rule tables, the composite verification resolver, the per-origin withdrawal
eligibility explanation, payout destinations, the pre-commitment quote and the
provenance read model. It is exercised end to end on a sandbox tier against a
provider that decides nothing on its own. See
`docs/product/VERIFICATION_AND_WITHDRAWAL.md` §9 for the full table.

**No new blocker was created.** What follows narrows two existing ones and adds
one standing operational obligation.

| Item | Sharpens | Why external |
|---|---|---|
| B-06 narrows to "no contracted identity vendor", not "no code". `verification.Provider` is the contract, `verification_sessions` and `verification_checks` are the store, and `verification.Resolver` reaches PAYOUT_KYC and ENHANCED from a provider decision plus the sub-checks that justify it. What is missing is an account, accepted terms, a data-processing agreement and a retention decision with Veriff, Persona or Sumsub. | B-06 | Account creation and a contract. A human action. |
| B-01/B-05 narrows the same way for the conversion side. The registry refuses a provider with no contract reference, and a quote refuses a provider with no published fee model — so the honest state of a deployment with no conversion contract is `PROVIDER_UNAVAILABLE` on every eligibility bucket, which is what it reports. | B-01, B-05 | A licensed provider must accept the closed-loop-until-withdrawal model. Only that provider can say. |
| **Keeping the sanctions denylist current is an operational obligation, not a property of the code.** `internal/verification/rules` holds the comprehensively-sanctioned jurisdictions as of this date, versioned as `verification-rules-v1-us-only`. OFAC's list changes; nothing in this repository watches it. A person or a scheduled review has to. | B-02 | The authoritative list is OFAC's, and reading it is somebody's job. |
| Which United States subdivisions restrict a closed-loop credit that becomes convertible at withdrawal is still unanswered, so the restricted-regions map is EMPTY and says so in a comment. A payout rail's own exclusions (Stripe stablecoin: NY, HI; Bridge: NY) are applied separately from the provider's capabilities, because they are facts about a rail rather than about a person. | B-02 | Counsel. |
| The self-custody USDC destination flow of §25 — wallet connect, ownership proof, a signed nonce, a verified address, a cooldown — is **not started**, deliberately. `DestinationCryptoWallet` exists as a kind and `Capabilities.SupportsExternalWallet` and `SupportedNetworks` exist as the questions an adapter must answer, so the seam is open. Building the challenge-and-signature flow with no contracted crypto rail to send to would be code completion standing in for an external dependency, which the goal forbids. | B-05, B-10 | No crypto payout rail is contracted. |

Two things that must not be described as blocked, because they are built,
tested against a real database and reachable end to end on a sandbox tier: the
verification state machine, and the conversion-request surface. BLOCKED_EXTERNAL
is for software that is complete and an external item that is genuinely
unavailable (goal PART 167); it is never for missing code.
## Agents (productization wave 1, 2026-09-10)

**The strategy compiler has no backend in this build, and that is a decision
rather than an omission (D-074, ADR-0029).** Both halves must arrive before a
strategy can be compiled at all, and each is genuinely external to this wave:

| Item | What it is | Why it is not built here |
|---|---|---|
| A model provider credential on the `model` slot (`CP_PROVIDERS_MODEL_MODE`, `..._API_KEY`) | The configuration already exists; the secret does not. EB-013 in the table above is the same blocker seen from the compiler's side. | An account and a paid key. No production Anthropic key has been used anywhere in this repository, and no test contacts the network. |
| A validation registry for the compiler's TYPE and RISK_COMPAT stages: instruments, venues, tools and the composed risk policy | `strategy.ValidationRefs`, loaded through the `agents.RefsLoader` interface. Nothing loads it today. | It is a build, not a credential — but building it with the registry empty would be worse than not building it: every instrument a user named would fail the TYPE stage and the API would blame the user for a deployment's missing data. |

Until both exist, `POST /v1/strategies/{id}/compile` records each attempt with
outcome `MODEL_UNAVAILABLE` and failure code `COMPILER_UNAVAILABLE`, produces no
IR, and says so in words; `compiler_configured` on the strategy read model lets a
client say so before a user writes a description. No agent can be created,
because an agent is created only from a compiled strategy version.

**Not blocked and deliberately not built:** the agent runtime stays inert. There
is no evaluator, no deployed worker, and no production caller for the runtime's
own services, which is the premise F-65's deferral of the `AGENT_PAUSE` /
`MODEL_DISABLE` bridge rests on and which `test/security` watches. Nothing in
this wave crosses it.

**Not built, and owed to §17 rather than to a provider:** a read route for an
agent's decision history (the immutable `agent_lifecycle_transitions` rows exist
and nothing returns them over HTTP), and agent performance (`internal/backtest`
and `internal/performance` do not exist at all).
