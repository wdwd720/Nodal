# STRIPE CAPABILITY MATRIX

What Stripe's current official documentation says, per feature, for the exact Nodal use case.
Nothing here is inferred from marketing copy, from an SDK's surface, or from what a product was
capable of at some earlier date.

**Evidence date:** 2026-09-08.
**Account assessed:** `acct_1REGPQALyMyuBFc1` ("Actorvia"), US, live mode.
See `STRIPE_ACCOUNT_STRUCTURE.md` for why that is the account and what it costs.

**How to read STATUS.** `VERIFIED` means a primary Stripe document states it. `DASHBOARD` means
this account's own Dashboard showed it. `NOT_VERIFIED` means nobody has confirmed it and no code
may depend on it.

---

## 1. Buying Nodal Credits with fiat (goal Section 3.A)

| Field | Value |
|---|---|
| PRODUCT | Stripe Payments — PaymentIntents + Payment Element |
| API / DASHBOARD CAPABILITY | `POST /v1/payment_intents`; Stripe.js Payment Element |
| ACCOUNT ELIGIBILITY | Dashboard shows **Payments: Active**, **Payouts: Active** |
| REGION | US account, USD |
| STATUS | VERIFIED + DASHBOARD |
| PRODUCTION AVAILABILITY | Technically available today on this account |
| REQUIRES APPLICATION? | **Yes — restricted-business review.** Selling stored value is a listed restricted category. See `STRIPE_BUSINESS_MODEL_REVIEW.md` §B |
| REQUIRES CONNECT? | No |
| REQUIRES KYC? | No end-user KYC. The platform itself is already verified |
| SUPPORTED ASSET | n/a (fiat in) |
| SUPPORTED NETWORK | n/a |
| CAN PAY EXTERNAL WALLET? | n/a |
| CAN HANDLE PLATFORM RECIPIENTS? | n/a |
| BLOCKER | `STRIPE_RESTRICTED_BUSINESS_APPROVAL` |
| SOURCE | https://docs.stripe.com/payments/payment-intents ; https://stripe.com/legal/restricted-businesses |

### Why PaymentIntents and not Checkout Sessions

This is not a style preference. Actorvia's live account already has an active webhook destination,
`actorvia-site-live`, pointing at `https://www.actorvia.xyz/api/billing/webhook` and subscribed to
six event types, one of which is `checkout.session.completed`. Stripe delivers an event to **every**
endpoint on the account subscribed to it. If Nodal sold Credits through Checkout Sessions, every
Nodal Credit purchase would also be delivered to Actorvia's billing handler, which has no concept of
a Nodal Credit.

Nodal therefore sells Credits through PaymentIntents, an event family Actorvia does not subscribe
to. The collision is removed rather than handled.

The converse still has to be handled. Actorvia's subscription invoices also produce
`payment_intent.*` events, so Nodal's endpoint will receive events that are not Nodal's. That is the
`FOREIGN_EVENT_REJECTION` requirement in `STRIPE_INTEGRATION_STATE.md`, and on a shared account it
is a correctness requirement rather than a defensive nicety.

---

## 2. Verifying payout recipients (goal Section 3.B)

| Field | Value |
|---|---|
| PRODUCT | Stripe Connect — connected account with the **Recipient** configuration, `dashboard: express` |
| API / DASHBOARD CAPABILITY | Accounts v2 `POST /v2/core/accounts`; `configuration.recipient`; `dashboard` |
| ACCOUNT ELIGIBILITY | Requires this account to become a Connect platform. Dashboard: Connect present, **not configured** |
| REGION | US platform |
| STATUS | VERIFIED (docs) / NOT_VERIFIED (this account) |
| PRODUCTION AVAILABILITY | Requires completing the Connect platform profile |
| REQUIRES APPLICATION? | Yes — the platform profile declares the business model |
| REQUIRES CONNECT? | Yes, definitionally |
| REQUIRES KYC? | **Yes, and Stripe performs it** |
| SUPPORTED ASSET | n/a |
| SUPPORTED NETWORK | n/a |
| CAN PAY EXTERNAL WALLET? | n/a |
| CAN HANDLE PLATFORM RECIPIENTS? | Yes — this is the recipient model |
| BLOCKER | `STRIPE_CONNECT_APPROVAL` |
| SOURCE | https://docs.stripe.com/connect/accounts-v2/connected-account-configuration |

### The sentence that decides goal Section 16

> "Responsibility for collecting KYC requirements is based on the
> `defaults.responsibilities.losses_collector` and `dashboard` values. In most configurations,
> Stripe is responsible for collecting KYC requirements from your connected accounts. Your platform
> is responsible only when you set `defaults.responsibilities.losses_collector` to `application` and
> `dashboard` to `none`."

Nodal needs `dashboard: express` regardless, because stablecoin payouts require Express Dashboard
access. `express` is not `none`. Therefore **Stripe owns the payout KYC**, and Nodal never collects
a government ID, an SSN or a selfie at the payout boundary. That answers the goal's question 7, and
it follows from a setting the payout product forces rather than from a choice Nodal could later get
wrong.

---

## 3. Paying eligible users in USDC (goal Section 3.C and 3.D)

| Field | Value |
|---|---|
| PRODUCT | **Stablecoin payouts for Connect** |
| API / DASHBOARD CAPABILITY | `POST /v1/transfers` in USD to the connected account; Stripe converts to the recipient's preferred currency |
| ACCOUNT ELIGIBILITY | Platform must be a **US** Connect platform, opted in, and approved |
| REGION | US platforms only. Recipients in 67 listed countries, **excluding the US states of New York and Hawaii** |
| STATUS | VERIFIED (docs) / **NOT AVAILABLE, confirmed on this account**: the live payment method configuration reports `"crypto": {"available": false}`, which is the check Stripe's own documentation names |
| PRODUCTION AVAILABILITY | **Private preview.** Not generally available |
| REQUIRES APPLICATION? | **Yes, four steps.** Be a Connect platform; request private-preview access through Stripe sales; request the feature in the Dashboard; complete a due-diligence questionnaire on the Account status page |
| REQUIRES CONNECT? | Yes |
| REQUIRES KYC? | Yes, performed by Stripe |
| SUPPORTED ASSET | **USDC only** |
| SUPPORTED NETWORK | **Base and Polygon** |
| CAN PAY EXTERNAL WALLET? | **Yes.** The recipient links their own wallet address in the Express Dashboard |
| CAN HANDLE PLATFORM RECIPIENTS? | **Individuals and sole proprietors only.** Companies and non-profits are not supported |
| BLOCKER | `STRIPE_STABLECOIN_PAYOUT_ACCESS` |
| SOURCE | https://docs.stripe.com/connect/stablecoin-payouts ; https://support.stripe.com/express/questions/stablecoin-payouts |

### Solana is not a supported payout network

The goal document anticipated this exactly in Section 23.

> "Stripe Express currently processes USDC payouts over the Base and Polygon Networks, which are
> Ethereum-compatible crypto networks."

There is no Solana payout path in this product. A Phantom wallet is still a valid destination,
because Phantom supports Solana, Ethereum, Base, Polygon, Bitcoin and Sui. But the address the user
must supply is their **EVM address**, not their Solana address. Those are two different addresses in
the same wallet, and a user who supplies the Solana one has supplied an address on a chain that
cannot receive the payout.

That is a product-facing fact, not an implementation detail, and the withdrawal UI has to say it in
words.

### Who holds the destination address

Stripe does. The recipient links a wallet inside the Express Dashboard; Nodal never submits an
address on the Transfers call. Nodal's own wallet record is therefore an **advisory mirror** and must
never be treated as the destination — see `WALLET_INTEGRATION_STATE.md`, which explains what Nodal
still has to verify and why.

---

## 4. Goal Section 23's four-way gate

All four columns must pass before an asset/network pair is usable.

| PAYOUT_ASSET | PAYOUT_NETWORK | STRIPE_SUPPORTED | WALLET_SUPPORTED | NODAL_APPROVED | Usable |
|---|---|---|---|---|---|
| USDC | Base | preview, not granted | yes (Phantom, MetaMask, Coinbase Wallet) | **no** | **no** |
| USDC | Polygon | preview, not granted | yes (Phantom, MetaMask, Coinbase Wallet) | **no** | **no** |
| USDC | Solana | **no** | yes | no | **no** |
| USDC | Ethereum mainnet | **no** | yes | no | **no** |
| SOL | Solana | **no** | yes | no | **no** |
| BTC | Bitcoin | **no** | yes | no | **no** |

`NODAL_APPROVED` is false on every row and stays false until a legal determination exists
(`docs/build/BLOCKERS.md` B-02) and a capability gate is activated through the dual-control path.
Two independent columns are false on the Base and Polygon rows, which is why the payout capability
is disabled rather than merely unconfigured.

---

## 5. Products deliberately NOT used

Goal Section 3 says to prefer the minimum architecture. These were assessed and rejected, and the
reason is recorded so that nobody re-adds one later on a hunch.

| Product | Verdict | Why |
|---|---|---|
| Stripe Checkout | Rejected | `checkout.session.completed` collides with Actorvia's live endpoint. See §1 |
| Stripe Identity | Not needed | Connect Express onboarding already performs the payout KYC. Adding Identity would collect identity data twice, which goal Section 16 forbids. The API endpoint responds on this account and no session has ever been created |
| Stripe Treasury | Not used by Nodal, and not onboarded | The Dashboard implies otherwise; the API answers "Have you onboarded to Treasury?". Nodal holds no stored fiat balance for users either way |
| Stripe Crypto Onramp | Already integrated, different purpose | `internal/provider/stripe` implements it for Domain C, fiat to crypto direct to a self-custodial wallet. It is neither a Credit purchase path nor a payout path |
| Stripe Issuing | Not used | Actorvia's, and the subject of the past-due task |
| Global Payouts | Not a separate product here | `/crypto/crypto-payouts` resolves to the Connect stablecoin payouts page. There is one stablecoin payout product and it is Connect-based |

---

## 6. What no document could answer

| Question | Why it is open |
|---|---|
| Will Stripe approve Nodal's business model on this account? | A human reviews it. See `STRIPE_BUSINESS_MODEL_REVIEW.md` |
| Will the private preview be granted? | Stripe sales decides, "around two business days" after a request |
| Minimum and maximum stablecoin payout amounts | Not stated in the public documentation. The adapter reports them as unknown rather than guessing |
| Stablecoin payout fees | Not stated in the public documentation. The payout quote required by goal Section 35 cannot be completed until the preview is granted and real fee data exists |
