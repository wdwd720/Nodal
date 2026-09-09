# STRIPE INTEGRATION STATE

What exists in the repository, what it does, and what is deliberately not done yet.

**Last updated:** 2026-09-08.

---

## 1. The three Stripe adapters, and why there are three

| Package | Product | Interface | What it is for |
|---|---|---|---|
| `internal/provider/stripe` | Crypto Onramp | `funding.FundingProvider` | Pre-existing. Domain C: a customer's own money becomes crypto in a wallet Nodal never holds |
| `internal/provider/stripecredit` | Payments (PaymentIntents) | `credit.PurchaseProvider` | **New.** Fiat becomes internal Credits |
| `internal/provider/stripepayout` | Stablecoin payouts for Connect | `payout.Provider` | **New.** Eligible internal value becomes USDC in the user's own wallet |

They are separate packages because they are separate products with separate
risk. The onramp never touches the Credit ledger; the credit adapter never
touches a wallet; the payout adapter never chooses a destination.

`internal/provider/stripesig` holds the one webhook signature verifier all of
them use. Two copies of an HMAC verifier is two chances to write one wrong,
and the wrong one believes forged events.

## 2. FOREIGN_EVENT_REJECTION

The single most important property of this integration, and the one created
entirely by the decision to share a Stripe account with another product.

Stripe delivers every event to every endpoint on the account subscribed to its
type. Nodal's endpoint therefore receives events belonging to the other
product. That is normal and is not an error.

Every event is classified before it is acted on:

| Case | Classification | What happens |
|---|---|---|
| Payment intent without `nodal_workstream=NODAL` | FOREIGN | Recorded, ignored |
| Payment intent whose `nodal_environment` is not this deployment's | FOREIGN | Recorded, ignored |
| Marker present, `nodal_credit_purchase_id` not a Nodal id | FOREIGN | Recorded, ignored |
| Charge or dispute naming a payment intent we do not have | FOREIGN, discovered at lookup | Recorded, ignored |
| Anything else Nodal-marked | OURS | Applied |

The environment check is the one that matters most. A staging deployment
pointed at the same live account would otherwise act on production purchases,
and every symptom of that bug looks like a system working correctly.

The reason an event was ignored is recorded rather than inferred. "We ignored
it" and "it never arrived" are identical in a ledger and completely different
in an incident.

**Tests:** `internal/provider/stripecredit/stripecredit_test.go`, and
`TestDispatch_ForeignEventIsIgnoredAndChangesNothing` in the integration suite,
which asserts a foreign event mints nothing.

## 3. Credit purchase, end to end

```
user asks to buy Credits for an AMOUNT
  -> CREDIT_PURCHASE gate checked (refuses before the provider is called)
  -> PricingPolicy derives the Credit quantity from the amount
  -> credit_fundings row + idempotency key persisted
  -> PaymentIntent created under that same key, with immutable metadata
  -> client secret returned to the browser; Stripe.js collects the card
  -> Stripe webhook -> signature verified -> inbox dedupes -> classified
  -> funding advances; CAPTURED mints exactly one lot, REVERSIBLE
  -> reversibility window closes -> SETTLED -> lot becomes payout-capable
```

What is deliberately absent: any request field carrying a Credit amount. The
only client input is money.

## 4. Statuses

Stripe's documented PaymentIntent statuses, mapped:

| Stripe | Nodal purchase status | Funding state |
|---|---|---|
| `requires_payment_method` | PAYMENT_METHOD_REQUIRED | AUTHORIZATION_PENDING |
| `requires_confirmation` | PAYMENT_METHOD_REQUIRED | AUTHORIZATION_PENDING |
| `requires_action` | AUTHENTICATION_REQUIRED | AUTHORIZATION_PENDING |
| `processing` | PROCESSING | CAPTURE_PENDING |
| `requires_capture` | AUTHORIZED | AUTHORIZED |
| `succeeded` | SUCCEEDED | CAPTURED (then mints to REVERSIBLE) |
| `canceled` | CANCELED | CANCELED |
| anything else | MANUAL_REVIEW | MANUAL_REVIEW |

Events, mapped:

| Stripe event | Nodal purchase status |
|---|---|
| `charge.refunded`, fully | REFUNDED |
| `charge.refunded`, partially | MANUAL_REVIEW |
| `charge.dispute.created` | DISPUTED |
| `charge.dispute.funds_withdrawn` | CHARGEBACK |
| `charge.dispute.funds_reinstated` | DISPUTE_WON |
| `charge.dispute.closed` lost / won / warning_closed | CHARGEBACK / DISPUTE_WON / DISPUTE_WON |

`funds_withdrawn` beats the dispute's own status string: Stripe can withdraw
funds while a dispute is still under review, and the funding must follow the
money rather than the paperwork.

### The known gap: partial refunds

A partial refund has no representation in the funding lifecycle. REFUNDED is
terminal and would destroy every Credit the purchase issued; ignoring it leaves
the platform out of pocket with the Credits still spendable. Neither is
defensible for a quarter refund, so it parks in MANUAL_REVIEW and a person
decides. This is named as a gap rather than hidden as an edge case, and closing
it means deciding the economics first, not writing more code.

## 5. Payout

`internal/provider/stripepayout` is written and **cannot submit anything**.
`Capabilities().Availability` is `REQUIRES_APPLICATION`, and `Submit` refuses
before touching the network. That refusal is the honest state of the
integration.

What it reports, from Stripe's own documentation and nothing else: USDC; Base
and Polygon; individuals and sole proprietors; 67 countries less New York and
Hawaii; Connect required; KYC required and performed by Stripe; the destination
held by Stripe; no published minimum or maximum.

Two error mappings are load-bearing:

- A timeout is `SUBMISSION_STATE_UNKNOWN`, never a failure. A caller told "it
  failed" is a caller free to submit again, which is how a payout gets sent
  twice.
- `Lookup` uses `GET /v1/transfers/search`, not a replayed create. Discovering
  whether a write happened must not be done by sending a write.

## 6. What is NOT done

Named rather than implied.

| Item | State | Why |
|---|---|---|
| HTTP endpoints for buying Credits | **done** | `GET /v1/credits/pricing`, `POST /v1/payments`, `GET /v1/payments/{paymentId}` |
| Composition-root wiring in `cmd/api` | **done** | All-or-nothing; every refusal logs why |
| Webhook ingestion wired | **done for this provider** | See below |
| Frontend funding page | not written | Goal Sections 55 and 57 |
| Frontend payout page | not written | Goal Sections 56 and 57 |
| Connect connected-account creation | not written | Blocked on the platform profile, which is a business-model declaration requiring approval |
| Payout wallet model | not written | See `WALLET_INTEGRATION_STATE.md`; the design question is settled and the code is not |
| Stripe sandbox integration run | not done | No key is configured. See `STRIPE_BROWSER_SETUP.md` |
| Reconciliation sweep scheduling | partial | `Reconcile` and `SettleDue` exist; nothing calls them on a timer |
| Destination-change payout hold | not built | Blocks `PAYOUT_SETTLE`; see `WALLET_INTEGRATION_STATE.md` §2B |

### A gap this workstream found in the existing system

`internal/webhook` is a complete and tested ingestion pipeline, and `cmd/api`
never populated `Ports.Webhooks` at all. `POST /v1/webhooks/{provider}` has
existed, with a public authorization policy, answering nothing for every
provider. **No provider webhook had ever been ingested in this binary.**

The Credit purchase pipeline is now wired. The funding onramp's is deliberately
not: it is a different product, it is blocked externally with no credentials,
and changing its behaviour is not this workstream's to do. The gap is named
here rather than left as an absence.

## 7. Classification (goal Section 70)

| | |
|---|---|
| SOFTWARE_COMPLETE | **FALSE**, and closer. Credit purchase is complete from HTTP through the ledger, wired, and tested. What remains for this half is the frontend, the reconciliation schedule, and the payout side |
| STRIPE_SANDBOX_COMPLETE | **FALSE.** No sandbox run has happened |
| STRIPE_ACCOUNT_CONFIG_COMPLETE | **FALSE.** No Nodal resource exists on the account yet |
| STRIPE_PRODUCTION_APPROVED | **FALSE.** No restricted-business review has been requested |
| LEGAL_APPROVED | **FALSE.** See `docs/build/BLOCKERS.md` B-02, B-03, B-07 |
| PENTEST_COMPLETE | **FALSE.** B-08 |
| LIVE_READY | **FALSE** |
