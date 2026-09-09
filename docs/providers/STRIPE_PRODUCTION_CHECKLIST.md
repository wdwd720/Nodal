# STRIPE PRODUCTION CHECKLIST

Everything between here and a real user buying Credits with a real card, in
order, with who has to do each item.

**Last updated:** 2026-09-08.
**Account:** `acct_1REGPQALyMyuBFc1` ("Actorvia").

Legend: **[eng]** engineering, **[user]** the account owner, **[stripe]** Stripe,
**[legal]** counsel.

---

## Stage 1 — make the integration runnable

| # | Item | Who | State |
|---|---|---|---|
| 1.1 | Pair the Stripe CLI (`stripe login`) | [user] | **pending** |
| 1.2 | Decide the production webhook URL for Nodal's API | [user] | **pending** |
| 1.3 | Wire the adapters in `cmd/api` | [eng] | **done** |
| 1.4 | Expose HTTP endpoints for starting a purchase and reading funding state | [eng] | **done** |
| 1.5 | Mount the webhook handler on the Nodal endpoint path | [eng] | **done** — `POST /v1/webhooks/stripe_credit` |
| 1.6 | Store `STRIPE_SECRET_KEY` and `STRIPE_WEBHOOK_SECRET` in the existing secret store; never in the repo | [user] | not started |
| 1.7 | Set `CP_PROVIDER_CREDIT_PURCHASE_ACCOUNT_REF=acct_1REGPQALyMyuBFc1` and `..._SHARED_ACCOUNT=true` | [eng] | config exists |

Nothing in Stage 1 moves money or tells Stripe anything about the business.

## Stage 2 — prove it against Stripe without moving value

| # | Item | Who | State |
|---|---|---|---|
| 2.1 | `stripe listen` against a local API; exercise every modelled event type | [eng] | blocked on 1.1 |
| 2.2 | Test-mode purchase with `4242 4242 4242 4242`; assert exactly one lot | [eng] | blocked on 1.1 |
| 2.3 | Declined card (`4000 0000 0000 0002`) leaves no Credits | [eng] | blocked on 1.1 |
| 2.4 | 3-D Secure card (`4000 0027 6000 3184`) reaches AUTHENTICATION_REQUIRED | [eng] | blocked on 1.1 |
| 2.5 | Refund in test mode drives the funding to REFUNDED and conserves the ledger | [eng] | blocked on 1.1 |
| 2.6 | Dispute simulation (`4000 0000 0000 0259`) freezes the lot | [eng] | blocked on 1.1 |
| 2.7 | Duplicate webhook delivery mints once | [eng] | **done in the integration suite**; repeat against real Stripe |
| 2.8 | An Actorvia-shaped `payment_intent.*` event is ignored | [eng] | **done in unit tests**; repeat against real Stripe |

Only official Stripe test cards. No real card in test mode, ever.

## Stage 3 — the things that need the owner, in escalating consequence

Each of these is a goal Section 67 stop. Preparation is done; submission is not.

| # | Item | Who | Consequence if it goes badly |
|---|---|---|---|
| 3.1 | Decide whether Nodal really should share Actorvia's account | [user] | Recorded as decided; see `STRIPE_ACCOUNT_STRUCTURE.md` |
| 3.2 | Review and approve the business description | [user] | It is what Stripe classifies the business on |
| 3.3 | Restricted-business review for stored value and marketplace | [user] → [stripe] | **A denial can affect Actorvia's existing capabilities** |
| 3.4 | Connect platform profile | [user] → [stripe] | Declares the marketplace model on this account |
| 3.5 | Stablecoin payout private-preview request via Stripe sales | [user] → [stripe] | ~2 business days for a first answer |
| 3.6 | Stablecoin payout due-diligence questionnaire | [user] → [stripe] | A legal attestation |
| 3.7 | Tax and bank information wherever Stripe asks | [user] | Never handled by an agent |

3.3 is the item with the largest blast radius, and it is not reversible by
withdrawing the application. It should not be submitted until 3.2 is settled
and counsel has seen it.

## Stage 4 — legal, independent of Stripe

Stripe approval is not legal approval. Goal Section 39.

| # | Item | Who | State |
|---|---|---|---|
| 4.1 | Money transmission analysis for selling stored-value Credits | [legal] | B-02 |
| 4.2 | Whether native-market trading proceeds may be paid out, per jurisdiction | [legal] | B-02 |
| 4.3 | Whether pre-KYC native-market participation is permissible | [legal] | B-03 |
| 4.4 | Age and jurisdiction matrix | [legal] | B-07 |
| 4.5 | Whether a Nodal-native asset is a security | [legal] | not raised in BLOCKERS yet |

## Stage 5 — gates

No capability is activated by writing code. Each needs its evidence set and two
approvers.

| Capability | Requires | State |
|---|---|---|
| `CREDIT_PURCHASE` | 3.3 approved, 4.1 answered | **disabled** |
| `MARKETPLACE` | 3.4 approved, 4.1 answered | **disabled** |
| `NATIVE_MARKET_TRADING` | 4.3 answered | **disabled** |
| `PAYOUT_RESERVE` | 3.5 and 3.6 approved, 4.2 answered | **disabled** |
| `PAYOUT_SETTLE` | all of the above, plus the destination-change hold built | **disabled** |

The default payout policy forbids every origin. Activating a gate does not
change that; a new policy version does, through its own approval path.

## Stage 6 — before real value

| # | Item | Who | State |
|---|---|---|---|
| 6.1 | External penetration test | [user] | B-08 |
| 6.2 | Destination-change payout hold | [eng] | not built; see `WALLET_INTEGRATION_STATE.md` §2B |
| 6.3 | Pre-flight payout eligibility refusals (country, US state, recipient kind) | [eng] | **decided and tested** in `payout.Capabilities.CanPayRecipient`; not yet called from the payout eligibility path, which needs the Connect recipient model first |
| 6.4 | The network warning in the withdrawal UI: Base or Polygon, not Solana | [eng] | not built |
| 6.5 | Reconciliation sweep on a schedule | [eng] | functions exist; nothing calls them |
| 6.6 | Partial-refund economics decided, then implemented | [user] + [eng] | parks in MANUAL_REVIEW today |

## The one-line answer

Stage 1 is complete except for the two items that need the owner: a CLI
pairing and a URL. Software can reach Stage 2 on those alone. Everything past Stage 2 needs a human decision that an agent
must not make, and the largest of those decisions puts an existing live account
at risk.
