# STRIPE ACCOUNT STRUCTURE

Which Stripe account Nodal uses, why, and what that choice costs.

**Decision date:** 2026-09-08. **Decided by:** the account owner, explicitly, after being shown the
alternative and its rationale.

---

## 1. The decision

Nodal runs on the **existing Actorvia live account**, `acct_1REGPQALyMyuBFc1`.

No separate Nodal Stripe account is used. No organization is created.

## 2. What the owner directed

Verbatim constraints, because they bound everything in this workstream:

- Work in LIVE / PRODUCTION mode wherever the integration requires real production resources.
- Preserve all existing Actorvia products, Treasury, card issuing, payments, webhooks, customers,
  balances, issuing configuration and production settings.
- Do not delete, overwrite, disable, rename or break existing Actorvia resources.
- Namespace every new Nodal resource clearly with `NODAL_*` or equivalent metadata.
- Keep Nodal's code and provider abstractions logically isolated so Nodal can later migrate to
  another Stripe account without an architectural rewrite.
- Do not send real customer charges, real payouts or real crypto transfers merely to test, without
  explicit per-transaction authorization.
- Stop before any restricted-business application, business-model attestation, legal certification,
  identity or tax or bank information, or acceptance of contracts and fees.
- Do not hide or mischaracterize Nodal's business model to Stripe.

## 3. What Stripe's documentation says about this

It says the opposite, and that has to be on the record.

> "You must use separate Stripe accounts for projects, websites, or businesses that operate
> independently from one another. When you activate a new account, it's subject to Stripe standard
> policies and pricing — it doesn't inherit any special status or other similar considerations that
> might apply to your existing account."
>
> — https://docs.stripe.com/get-started/account/multiple-accounts

An organization would not have helped either way. An organization "doesn't conduct its own business
through Stripe. It acts as a container structure to view and manage the operation of its separate
businesses" — it is consolidated reporting and SSO, not isolation
(https://docs.stripe.com/get-started/account/orgs/setup).

## 4. What Actorvia currently is

Recorded from the Dashboard on 2026-09-08, because the blast radius is the point.

| | |
|---|---|
| Account | `acct_1REGPQALyMyuBFc1`, "Actorvia", US |
| Active capabilities | Payments, Payouts, ACH Direct Debit, Affirm, Afterpay Clearpay, Amazon Pay, Bancontact, BLIK, Canadian pre-authorized debit, Cash App Pay, EPS, Financial Addresses (Bank Accounts), Holds Currencies (USD), Klarna, Link, MB WAY, Money Manager Business Storage Inbound USD, Money Manager Business Storage Outbound USD, Pix, Satispay, and more behind "View more" |
| Paused capability | Cartes Bancaires payments |
| Open task | "Update your business information" — **Past due**, paused 2026-08-14, "Impacts prepaid card FA Cross River Bank card issuing" |
| Live webhook | `actorvia-site-live` → `https://www.actorvia.xyz/api/billing/webhook`, active, API version `2026-07-29.dahlia`, 0% error rate |
| Webhook events | `checkout.session.completed`, `customer.subscription.created`, `customer.subscription.deleted`, `customer.subscription.updated`, `invoice.paid`, `invoice.payment_failed` |
| Connect | Present in the Dashboard, **not configured** |
| Treasury | **Not onboarded.** The Dashboard shows a Treasury nav item and Money Manager Business Storage capabilities, but `GET /v1/treasury/financial_accounts` answers "Have you onboarded to Treasury?" and no treasury capability is on the account |
| Card issuing | A prepaid-card programme with Cross River Bank exists and is what the past-due task blocks |
| Account type | `standard`, `controller.type: account`, `business_type: individual` — a standalone account, not a platform |
| Statement descriptor | static `ACTORVIA`, card prefix `ACTR` |
| Identity | Not shown in the Dashboard; the API endpoint responds and no session has ever been created |

### The past-due task, specifically

The owner asked what it affects before anything changes. The Dashboard is precise about it:

- **What is paused:** Cartes Bancaires payments only.
- **What the task blocks:** "prepaid card FA Cross River Bank card issuing".
- **What is unaffected:** Payments and Payouts are both Active. Credit purchase through
  PaymentIntents does not depend on Cartes Bancaires or on card issuing.

So the task does **not** block the Nodal fiat-in path. It is Actorvia's, it concerns Actorvia's card
issuing programme, and it has not been submitted or modified by this workstream.

## 5. The risk this creates, stated plainly

Nodal's business model touches several of Stripe's listed restricted categories
(`STRIPE_BUSINESS_MODEL_REVIEW.md`). Declaring it means a Stripe human reviews it **against this
account**. If that review goes badly, what is at stake is not a greenfield Nodal account with
nothing on it. It is the account currently running live payments and a prepaid-card issuing
programme with Cross River Bank. An earlier draft said "Treasury with Cross
River Bank"; the live API says Treasury is not onboarded, and that correction
narrows the blast radius without removing it.

This risk was stated to the owner before any configuration work began, and the direction was
confirmed. It is recorded here rather than argued again.

**Every point at which this risk actually bites is a STOP.** No restricted-business application,
platform-profile business declaration, attestation or capability request is submitted without
per-action approval. Preparation is done; submission is not.

## 6. What the code does about it

The architectural requirement — "Nodal can later migrate to another Stripe account without an
architectural rewrite" — is met by three properties, each of which is testable rather than
aspirational.

1. **No Stripe object is a Nodal identity.** Nodal's ids are primary; Stripe references are
   external, unique and indexed. That was already true of `credit_fundings` and `payout_requests`
   and is preserved.
2. **The account id is configuration.** `CP_PROVIDER_CREDIT_PURCHASE_ACCOUNT_REF` names the account
   this adapter expects, so moving accounts is a configuration change rather than a code change.
   (An earlier draft named a `CP_STRIPE_ACCOUNT_ID` variable that does not exist; the real one is
   per provider slot, which is also the right shape, because the payout provider need not live on
   the same account as the payment provider.)
3. **Foreign events are rejected, not tolerated.** Because the account is shared, Nodal's webhook
   endpoint receives Actorvia's events. Nodal ignores any event that does not carry Nodal's own
   metadata namespace. That is what makes the shared account survivable, and it is also exactly what
   makes a later migration a no-op: a Nodal-only account simply never delivers a foreign event.

## 7. Namespacing

Every Nodal-created live Stripe resource carries:

| Key | Value |
|---|---|
| `nodal_workstream` | `NODAL` |
| `nodal_environment` | `PROD` \| `STAGING` \| `LOCAL` |
| `nodal_credit_purchase_id` | Nodal's own id, on purchase objects |
| `nodal_user_id` | Nodal's account id |
| `nodal_pricing_version` | the pricing policy version that derived the Credit amount |
| `nodal_idempotency_key` | on payout transfers, so a timed-out submission can be found again |

Card charges additionally carry a statement descriptor suffix, because the account's own descriptor
is `ACTORVIA` and a cardholder who does not recognise a charge disputes it. See
`STRIPE_BROWSER_SETUP.md` §8.

Names of Dashboard-created resources are prefixed `NODAL_`. An object without
`nodal_workstream=NODAL` is not Nodal's and Nodal's code refuses to act on it.

## 8. The account created and then abandoned

Before the direction changed, a separate account was created: **Nodal**,
`acct_1UDZsoARym5YyR1Q`, US, unactivated, test mode only, no business information submitted, no
products, no keys in use.

It has been left alone rather than deleted, because deleting is destructive and it costs nothing to
leave an empty unactivated account in place. If the account structure is ever revisited, it is
already there. Say the word and it can be removed.
