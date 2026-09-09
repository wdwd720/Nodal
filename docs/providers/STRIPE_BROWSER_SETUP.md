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
| Stripe CLI paired | **NEEDS USER** | See §5 |
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

## 5. Stripe CLI pairing — NEEDS USER

The CLI is installed (`stripe` 1.50.3, via winget) and not paired. Pairing was
attempted twice and could not be completed from this session: the device flow
requires typing a verification code into `access.stripe.com`, and the
permission classifier correctly refuses to let an agent type what looks like a
credential.

**What the user needs to do**, in a terminal:

```
stripe login
```

Then approve in the browser. That stores credentials under the user's own
config, not in this repository, and this session never sees a key.

Once paired, the following become possible without any secret entering the
transcript:

- enumerate the live account's real capability set through the API rather than
  the Dashboard's summary;
- create the Nodal webhook endpoint with `NODAL_` naming;
- run `stripe listen` to exercise the webhook path locally against real
  Stripe-signed deliveries;
- trigger test events for every modelled type.

## 6. The live webhook endpoint — NEEDS USER, and needs a URL

The Nodal endpoint is ready to be created and one input is missing: **the
public URL Nodal's API will serve `POST` webhooks on in production.** It does
not exist yet, and creating a Stripe endpoint that points nowhere would put a
failing destination on Actorvia's live account.

When it exists, the endpoint to create is:

| | |
|---|---|
| Name | `NODAL_credit_purchase_live` |
| URL | *(pending)* |
| Events | `payment_intent.created`, `payment_intent.requires_action`, `payment_intent.processing`, `payment_intent.amount_capturable_updated`, `payment_intent.succeeded`, `payment_intent.payment_failed`, `payment_intent.canceled`, `charge.refunded`, `charge.dispute.created`, `charge.dispute.closed`, `charge.dispute.funds_withdrawn`, `charge.dispute.funds_reinstated` |

That list is `stripecredit.ModelledEventTypes()`, and a test asserts every type
in it produces a recognized event — so a handler cannot ship without its
subscription, and a subscription cannot be added without a handler.

Note what happens the moment this endpoint exists: it will also receive
Actorvia's subscription `payment_intent.*` events, because they share the
account. That is expected and handled. See `STRIPE_INTEGRATION_STATE.md` §2.

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
