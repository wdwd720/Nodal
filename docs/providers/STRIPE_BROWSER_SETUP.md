# STRIPE BROWSER SETUP

Every browser action taken against Stripe, with its outcome. Goal Section 66.

**Session date:** 2026-09-08.
**Account:** `acct_1REGPQALyMyuBFc1` ("Actorvia"), US, live mode.
**Tool:** Claude in Chrome, against the signed-in Dashboard session.

---

## 1. Actions and outcomes

| Action | Status | Detail |
|---|---|---|
| Stripe Dashboard opened and account identified | **DONE** | One account under this login: Actorvia, `acct_1REGPQALyMyuBFc1` |
| Account capabilities inspected | **DONE** | See §2 |
| Past-due task investigated | **DONE** | See §3 |
| Live webhook destinations enumerated | **DONE** | See §4 |
| Connect state inspected | **DONE** | Present in the Dashboard, **not configured** |
| Stripe Identity availability checked | **DONE** | Not enabled on this account; both Identity URLs redirect to the dashboard home |
| Official documentation verified | **DONE** | Stablecoin payouts, Connect Accounts v2, restricted businesses, multiple accounts, organizations. See `STRIPE_CAPABILITY_MATRIX.md` for citations |
| Separate Nodal account created | **DONE, then superseded** | `acct_1UDZsoARym5YyR1Q`, US, unactivated, test mode only. Abandoned when the direction changed to using Actorvia. Left in place rather than deleted |
| Stripe CLI paired | **DONE** | Paired to `acct_1REGPQALyMyuBFc1`, context Actorvia · live. Verified from this session |
| Live account read through the API | **DONE** | See §8, which corrects two things the Dashboard implied |
| Live webhook endpoint for Nodal | **NEEDS USER** | See §6 |
| Connect platform profile | **NEEDS USER** | Declares the business model. Goal Section 67 stop |
| Stablecoin payout private-preview request | **NEEDS USER** | Requires contacting Stripe sales |
| Stablecoin payout due-diligence questionnaire | **NEEDS USER** | Legal attestation |
| Restricted-business review | **NEEDS USER** | Goal Section 67 stop |
| Production API keys in a secret store | **NEEDS USER** | Never handled in this session |
| Test-mode payment run | **NOT DONE** | Blocked on the CLI pairing or a key |

Nothing on Actorvia was modified. No setting was changed, no resource created,
no form submitted.

## 2. Capabilities, as the Dashboard reported them

**Active:** Payments, Payouts, ACH Direct Debit, Affirm, Afterpay Clearpay,
Amazon Pay, Bancontact, BLIK, Canadian pre-authorized debit, Cash App Pay, EPS,
Financial Addresses (Bank Accounts), Holds Currencies (USD), Klarna, Link,
MB WAY, Money Manager Business Storage Inbound USD, Money Manager Business
Storage Outbound USD, Pix, Satispay, and more behind a "View more" control.

**Paused:** Cartes Bancaires payments.

**Products present:** Treasury (Cross River Bank), Connect (unconfigured),
Payments, Billing, Reporting, Apps.

## 3. The past-due task

The owner asked specifically what it affects before anything changed.

> "Update your business information — Past due. Paused on Aug 14, 2026 •
> Impacts prepaid card FA Cross River Bank card issuing"

- **Paused capability:** Cartes Bancaires payments only.
- **Blocked by the task:** prepaid-card financial addresses and Cross River
  Bank card issuing.
- **Unaffected:** Payments and Payouts are both Active.

The Nodal Credit purchase path uses PaymentIntents and card payments. It
depends on neither Cartes Bancaires nor card issuing, so **this task does not
block the fiat-in integration**. It is Actorvia's, it concerns Actorvia's card
issuing programme, and it has not been submitted or modified.

## 4. Live webhook destinations

One, and it matters to the design.

| | |
|---|---|
| Name | `actorvia-site-live` |
| URL | `https://www.actorvia.xyz/api/billing/webhook` |
| Destination id | `we_1U1FDHALyMyuBFc1H4PhH0CO` |
| State | Active, 0% error rate |
| API version | `2026-07-29.dahlia` |
| Description | "Subscription state for www.actorvia.xyz (live mode)." |
| Events | `checkout.session.completed`, `customer.subscription.created`, `customer.subscription.deleted`, `customer.subscription.updated`, `invoice.paid`, `invoice.payment_failed` |

`checkout.session.completed` is why Nodal sells Credits through PaymentIntents
rather than Checkout. Stripe delivers each event to every endpoint subscribed
to it, so Checkout would post every Nodal purchase to Actorvia's billing
handler. See `STRIPE_CAPABILITY_MATRIX.md` §1.

## 5. Stripe CLI pairing — DONE

Paired by the account owner running `stripe login`. Credentials live in the
owner's own CLI configuration, never in this repository, and no key has entered
this session's transcript.

```
account_id   = acct_1REGPQALyMyuBFc1
display_name = Actorvia
context      = Actorvia · live
```

One operational note for anyone repeating this on Windows: Git Bash rewrites a
leading-slash argument into a Windows path, so `stripe get /v1/account` asks
Stripe for `/v1/C:/Program Files/Git/v1/account`. Prefix the command with
`MSYS_NO_PATHCONV=1`.

The CLI has a live context only. A sandbox context is what Stage 2 of the
production checklist needs and does not exist yet.

## 6. The live webhook endpoint — NEEDS USER, and needs a URL

The Nodal endpoint is ready to be created and one input is missing: **the
public URL Nodal's API will serve `POST` webhooks on in production.** It does
not exist yet, and creating a Stripe endpoint that points nowhere would put a
failing destination on Actorvia's live account.

When it exists, the endpoint to create is:

| | |
|---|---|
| Name | `NODAL_credit_purchase_live` |
| URL | *(pending)* — must end in `/v1/webhooks/stripe_credit`, which is the path the handler is mounted on |
| Events | `payment_intent.created`, `payment_intent.requires_action`, `payment_intent.processing`, `payment_intent.amount_capturable_updated`, `payment_intent.succeeded`, `payment_intent.payment_failed`, `payment_intent.canceled`, `charge.refunded`, `charge.dispute.created`, `charge.dispute.closed`, `charge.dispute.funds_withdrawn`, `charge.dispute.funds_reinstated` |

That list is `stripecredit.ModelledEventTypes()`, and a test asserts every type
in it produces a recognized event — so a handler cannot ship without its
subscription, and a subscription cannot be added without a handler.

Note what happens the moment this endpoint exists: it will also receive
Actorvia's subscription `payment_intent.*` events, because they share the
account. That is expected and handled. See `STRIPE_INTEGRATION_STATE.md` §2.

## 8. What the live API said, and what it corrects

The Dashboard is a summary. Two things it implied turned out to be wrong, and
both were corrected here rather than left standing.

**Treasury is not onboarded.** `GET /v1/treasury/financial_accounts` answers
"Have you onboarded to Treasury? ...", and no treasury capability appears on
the account object. The Dashboard shows a "Treasury overview" nav item and
lists Money Manager Business Storage Inbound/Outbound USD and Financial
Addresses as active, which read as a live Treasury programme. Earlier drafts of
these documents said "live Treasury with Cross River Bank" on that basis. That
overstated the blast radius of a restricted-business review and has been
corrected.

**Stripe Identity's API responds.** `GET /v1/identity/verification_sessions`
returns an empty list rather than an error, so the endpoint exists on this
account even though the Dashboard shows no Identity product and no session has
ever been created. Earlier drafts said "not enabled", which was a Dashboard
reading rather than a fact. Identity is still not needed: Connect Express
onboarding performs the payout KYC.

### The finding that settles the payout question empirically

Stripe's stablecoin payout documentation says to verify access by checking that
**Crypto** is active in the account's payment method settings. It is not:

```
"crypto": { "available": false }
```

That is account-specific confirmation, from the account itself rather than from
documentation, that the product has not been granted. It matches exactly what
the adapter reports without having been told: `AvailabilityRequiresApplication`.

### Other facts worth having on record

| | |
|---|---|
| Account type | `standard`, `controller.type: account` — a standalone account, **not a Connect platform** |
| Business type | **`individual`** |
| Country / default currency | US / USD |
| `charges_enabled` / `payouts_enabled` / `details_submitted` | true / true / true |
| `requirements.currently_due` and `past_due` | **both empty** at the account level |
| Connected accounts | none (`GET /v1/accounts` returns an empty list) |
| Transfers API | reachable, no transfers exist |
| Crypto Onramp API | **"Unrecognized request URL"** — not available on this account |
| Statement descriptor | static `ACTORVIA`, card prefix `ACTR` |
| MCC | 5734 |

Two of those need a decision rather than a note.

**`business_type` is `individual`.** Stablecoin payouts require the platform to
be a US Connect platform. Whether a sole-proprietor standard account can become
one is a question for Stripe, and carrying marketplace settlement and
stored-value issuance on an individual account is a materially different risk
posture from carrying them on a company.

**The declared product description does not describe Nodal.** The account's
`business_profile.product_description` describes software tooling for game
developers, sold by subscription or usage. It says nothing about stored-value
Credits, a user-created asset market, marketplace settlement or crypto payouts.
Running Nodal's payments through this account without updating that description
means processing outside the declared business. Goal Section 28 requires a
truthful description and Section 68 forbids working around the gap by leaving
words out. **This is a STOP: the description is the owner's to change, and
changing it is what triggers the review.**

## 7. What was never touched

Recorded because "we did not change it" is only credible if it is specific.

- No Actorvia webhook was created, edited, disabled or deleted.
- No Actorvia product, price, customer or subscription was touched.
- Connect was not enabled; the platform profile was not started.
- Treasury and Issuing were not opened beyond reading the account status page.
- The past-due task was opened to read it and not submitted.
- No terms were accepted, no attestation made, no business description
  submitted.
- No API key was read, copied, displayed or stored.
