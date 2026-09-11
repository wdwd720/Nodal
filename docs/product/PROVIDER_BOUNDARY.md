# PROVIDER BOUNDARY — where Nodal ends and a licensed provider begins

Status: product architecture, 2026-09-10. Derived from the product goal
(§19–§26, §59–§60) and from public provider documentation read for this goal
(`research: conversion providers`, summarised in §5 below). **Nothing in this
document is a provider's approval of Nodal's model, and nothing in it is legal
advice.** `LEGAL_APPROVED` is false. Counsel and the provider approve the exact
implementation; this document builds the boundary so that they can.

## 1. Three roles, three contracts, never one vendor

Nodal has three money-adjacent functions with different regulatory characters.
They are modelled as three provider roles and are not conflated in code, in
configuration or in vocabulary.

| Role | What happens | Character | Nodal's part |
|---|---|---|---|
| **A · Acquiring** | A customer pays a card and receives closed-loop, non-redeemable Credits | Card acceptance for a stored-value-adjacent product | `internal/credit` + `internal/provider/stripecredit`, capability-gated (`CREDIT_PURCHASE`), sandbox today |
| **B · Identity at withdrawal** | A customer who asks to withdraw is verified: identity, age, jurisdiction, sanctions | Customer identification; evidence for whoever is regulated | Nodal stores a **decision**, a provider reference and timestamps — never a document, an SSN or a full date of birth |
| **C · Conversion and payout** | Eligible value leaves Nodal as USD, USDC or another approved rail | **Money transmission. Someone must hold the licence.** | Nodal **instructs** a licensed provider. Nodal never converts. |

The load-bearing constraint: at $0 fixed cost Nodal is not, and cannot be, a
money transmitter. The Role C provider is the transmitter of record and Nodal
is its platform. Whether a given provider accepts that structure for a
closed-loop credit that becomes convertible only at withdrawal is a question
only that provider can answer (§6).

## 2. The boundary in the ledger

```
CLOSED_LOOP_CREDIT ──(trade, earn, spend)──► CLOSED_LOOP_CREDIT
        │
        │ customer clicks Withdraw
        ▼
   FinancialProfile ──(Role B decision)──► VERIFIED / RESTRICTED / REJECTED …
        │
        │ customer requests conversion of an amount
        ▼
   ConversionRequest ──(eligibility policy: provenance, holds, jurisdiction, limits)──► PAYOUT_ELIGIBLE_VALUE
        │
        │ Role C provider accepts, quotes, pays
        ▼
   ExternalSettlement (provider's money movement, observed by webhook + reconciliation)
```

Three rules the ledger keeps regardless of provider:

1. **Verification never mutates Credits.** A Role B decision changes the
   customer's financial profile; it does not touch a lot, a balance or a
   value domain. There is no `UPDATE credits SET redeemable = true` anywhere,
   and `test/security` will assert that no writer of the Credit tables reads
   the verification tables.
2. **Conversion is a request with its own state machine**, not a flag. It
   names the amount, the eligible source lots and their provenance, the quote
   (gross, fee, net, expiry), the provider, the destination, its evidence and
   an idempotency key Nodal owns. Its states follow the existing transition
   pattern (a transition row is the state change; the application cannot
   write the state column).
3. **Provenance survives trading.** Eligibility is computed from lots, not
   from a balance: purchased value, trading gains, promotional grants,
   creator earnings, refunds, disputed funds and adjustments each carry their
   origin through every trade, and the eligibility policy decides per origin.
   The default policy fails closed: no origin is eligible until counsel and
   the provider say which are. `internal/valuedomain.DefaultPolicy` already
   forbids every origin, and this goal keeps that default.

## 3. The `ConversionProvider` contract

Provider-abstract, derived from the union of what Tilia/Thunes, Stripe Connect,
Stripe stablecoin payouts, Bridge, Persona, Veriff and Sumsub actually require.
Every method exists because at least one viable provider demands it; none
leaks a single provider's vocabulary.

| Method | Why it is there |
|---|---|
| `Capabilities(jurisdiction{country, subdivision})` | Rails, currencies **and chains**, per-rail minimums, recipient-type limits, minimum age and **sub-national exclusions** (Stripe stablecoin payouts exclude NY and HI; Bridge excludes NY by principal address). A country-level model is insufficient. |
| `StartVerification(subject_ref, jurisdiction, purpose, return_url, refresh_url)` → session `{hosted_url, expires_at, provider_ref, environment}` | Hosted, single-use URLs that expire in minutes; both a return and a refresh URL; prefill before the first link. Nodal never renders a document capture. |
| `GetVerification(session)` → `{status, failure_reason?, decisions}` | **Never trust the redirect.** Status union: created, pending_user_action, processing, requires_input, manual_review, approved, declined, cancelled, expired. Decisions, not PII: `age_verified`, `jurisdiction`, `sanctions_clear`, `verified_at`. |
| `ResumeVerification(session)` | A fresh hosted URL on the same session, preserving failed-attempt history. |
| `ParseVerificationWebhook(headers, body)` → verified event | Signature verification lives in the adapter (Persona HMAC over timestamp·body with rotation; Stripe's own scheme). Idempotent on receipt — Persona retries up to eight times. A webhook is never the sole source of truth; every adapter has a reconciliation poll. |
| `QuotePayout(request)` → `{gross, fee, net, fx_rate?, expires_at, minimum_ok}` | A separate pre-commitment call: the customer sees the fee and the net before committing, and `minimum_ok` is judged **net of fees** because sub-minimum dust is destroyed, not returned. |
| `RequestPayout(request{idempotency_key, amount minor units, asset+chain, rail, destination (tagged union), subject_ref, verification_session, jurisdiction})` → handle | The provider's idempotency window is short (Bridge: 24h, then 422). **Nodal's ConversionRequest is the durable dedupe authority**; a retry past the window is a new key under Nodal's lock, never a replay, and the body is hashed so a divergent retry is refused locally. |
| `GetPayout(handle)`, `ListPayouts(subject_ref, since)` | Status union: queued, pending_provider_review, processing, sent, settled, failed, returned, cancelled — plus per-rail `reversible` (ACH returns; on-chain USDC does not) and `settlement_delay`. |
| `ParsePayoutWebhook(headers, body)` | As above. |
| `RedactVerification(session)` → pending | Asynchronous (Stripe: up to four days). |

Cross-cutting: environment is a field, not a base URL; every response carries
`provider_name`, `provider_request_id`, `provider_raw_status`; errors are
classified (`retryable`, `terminal`, `requires_user_action`,
`requires_provider_action`, `jurisdiction_unsupported`, `below_minimum`,
`idempotency_conflict`) and the state machine branches on the class, never on
a provider string.

**What the contract deliberately cannot express:** a primitive by which Nodal
itself converts Credits to money. If Nodal's own code contained such a
function, the architecture would be arguing against Nodal in a regulatory
review. The contract instructs a licensed provider; that is all it does.

## 4. What exists today, and how it maps

The repository already has the Role A path (Stripe sandbox, sandbox-only, gated),
a payout engine with eligibility, provenance consumption, destinations, a
`PAYOUT_STATUS_UNKNOWN` state and reconciliation against a faithful sandbox
(`internal/payout`, `internal/eligibility`, `internal/valuedomain`,
`internal/legalrouter`), a compliance profile with identity states
(`compliance_profiles`), and the `WITHDRAWALS`, `PAYOUT_RESERVE`,
`PAYOUT_SETTLE` capabilities, all DISABLED. `PAYOUT_KYC` is not a capability: it
is the verification level `valuedomain.SandboxPolicy` requires. The productization
work builds the Role B and conversion-request surfaces onto these rather than
beside them; the exact mapping (which existing table is the ConversionRequest,
which state machine carries verification) is recorded in
`VERIFICATION_AND_WITHDRAWAL.md` once built.

## 5. Provider landscape, as read on 2026-09-10

Ranked by fit for the launch shape. Every entry below is from public
documentation; **UNVERIFIED** marks a claim that could not be confirmed.

| Rank | Provider | Role | Fit | What blocks it |
|---|---|---|---|---|
| 1 | **Tilia (Thunes)** | B + C | Its data model is Nodal's: standard token wallets vs *convertible* token wallets; hosted KYC; KYC-gated hosted payout UI; money-transmitter licences across 48 states/territories incl. a California DFPI registration | Sales-led, no public pricing, gated sandbox; post-acquisition entity churn (UNVERIFIED succession) |
| 2 | **Stripe Connect** (Accounts v2, `recipient` agreement, `transfers`) | C | Self-serve and genuinely $0 fixed: $2 per *active* account, 0.25% + 25¢ per payout; hosted onboarding; already in the stack | Stripe's restricted-business classification of virtual credits is **unresolved and must be read by a human**; Nodal carries loss liability; Connect does not make Stripe the transmitter for the Credit float Nodal holds — a counsel question |
| 3 | **Stripe stablecoin (USDC) payouts** | C rail | Incremental on Connect; California supported (NY, HI excluded); Stripe converts, Nodal holds no crypto | Private preview, US platforms only, individuals only, sales contact, a due-diligence questionnaire a human attests to |
| 4 | **Bridge** (a Stripe company) | C | Best-designed API to shape the contract against (KYC links, transfers, explicit minimums, exclusions, idempotency) | Sandbox gated behind an email; no public pricing; 24-hour idempotency window |
| 5 | **Veriff** | B | Cheapest credible floor with sanctions: $49/mo minimum, $0.80/verification, +$0.64 PEP/sanctions, free sandbox, self-serve | A monthly minimum — a fixed cost, so not before a launch decision |
| 5 | **Persona** | B | Best ergonomics: hosted one-time links, `reference-id`, `environment-id`, signed webhooks, 60-day sandbox trial | Essential is $250/mo on an annual contract unless the Startup Program is granted |
| 7 | **Sumsub** | B | Strongest declarative age/jurisdiction gating; bundled AML | $299/mo for the tier with AML; sandbox config mirrors production |
| 8 | **Stripe Identity** | B component | $1.50/verification, first 50 free, no minimum | **No AML or sanctions screening** — never the whole answer |
| — | Plaid IDV, Circle, Onfido/Entrust, Alloy, Coinbase | — | Kept warm / ruled out for launch (institutional-only, DFAL pending, sales-only, unverifiable) | — |

The launch shape this supports: Role A Stripe (with the restricted-business
question settled in writing first); Role B Veriff or Persona behind Nodal's
own contract so the choice is reversible; Role C Stripe Connect as the
buildable-today path with Tilia/Thunes pursued in parallel and USDC payouts as
a second rail once the preview is granted. **None of it is selected here.**
Selection is B-05/B-06 in `docs/build/BLOCKERS.md` and requires a human.

## 6. External blockers this boundary makes explicit

Every one is a human action. None was or could be performed by engineering.

1. Read Stripe's restricted-business list in a browser and settle whether
   Nodal's Credits are *restricted* (extra diligence) or *prohibited* (cannot
   launch on Stripe). Two automated reads disagreed; everything downstream
   depends on this.
2. Complete the Stripe platform profile honestly, including that Nodal sells
   internal Credits and permits withdrawal of earned value.
3. Sign up for the chosen identity-verification vendor (account, terms,
   billing) and sign a data-processing agreement with it.
4. Open the Tilia/Thunes conversation (pricing, sandbox, business-model
   review).
5. For USDC: request Stripe's stablecoin-payouts preview and complete its
   due-diligence questionnaire; or obtain a Bridge developer account.
6. Decide 1099 filing (a tax adviser's decision; Stripe declines to advise).
7. Business KYB on Nodal itself by whichever Role C provider is chosen.

And the questions only counsel can answer, kept in the research report and in
`BLOCKERS.md`: whether Nodal's closed-loop Credit float is itself stored value
requiring a licence; whether trading gains may become payout-eligible and
under what provenance rule; what age and jurisdiction matrix applies; what
the terms, risk and Credits disclosures must say.

## 7. What Nodal will and will not say

- Nodal says: *"Credits are internal platform value and aren't directly
  withdrawable."* and *"Withdrawal eligibility requires identity verification
  and an approved payout method."*
- Nodal does not say that verification turns Credits into cash, that a
  withdrawal is approved, that any provider has approved Nodal, or that the
  model is legal. It builds the boundary and leaves the approval to the people
  who can give it.
