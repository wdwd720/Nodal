# STRIPE BUSINESS MODEL REVIEW

Nodal's economic activities, each measured against Stripe's current published restricted-business
policy. Goal Section 4.

**Evidence date:** 2026-09-08.
**Source:** https://stripe.com/legal/restricted-businesses
**Account:** `acct_1REGPQALyMyuBFc1` ("Actorvia"). See `STRIPE_ACCOUNT_STRUCTURE.md`.

This document does not make legal conclusions. It reads a published policy against a described
product and reports where the two touch. Whether Nodal is a money transmitter, whether a native
asset is a security, and whether a competition reward is a prize are questions for counsel and are
tracked separately in `docs/build/BLOCKERS.md`.

Goal Section 68 governs what happens if Stripe says no: record the blocker. Do not rename Credits,
do not hide the native markets, do not remove words from an application, do not move to another
account, do not misclassify. The architecture may then need a different provider.

---

## The policy text that matters

Quoted so that no later reading has to go and find it again.

**Restricted (requires Stripe review and approval):**

- "Sale of stored value or credits maintained, accepted, and issued by anyone other than the seller"
- "Preloaded payment cards, gift cards, virtual credits, or other products and services in which a
  monetary value is stored"
- "Sale of in-game currency or game items, unless the business is the operator of the virtual world"
- "Investment and brokerage services, including real estate-based investments"
- "Money transmitters, remittances, currency exchange services, and other money service businesses"
- "Cryptocurrency (for example, Bitcoin, Ripple, Ethereum, Dogecoin, Cardano, etc.) exchanges and
  wallets"
- "Payment facilitation and aggregation (including receiving settlement proceeds for goods or
  services that you did not provide)"
- "First-party non-fungible tokens (NFTs) minting and sales, including marketplaces and SaaS
  platforms"

**Prohibited (not permitted at all):**

- "Games of chance including gambling, internet gambling, casino games, sweepstakes and contests,
  and fantasy sports"
- "Games of skill … with a monetary or material prize"
- "'Get rich quick' schemes, including investment opportunities or other services that promise high
  rewards"
- "Predatory investment opportunities with no or low money down"
- "Secondary NFT sales"
- "Cryptocurrency mining and staking"

---

## A. Nodal SaaS / AI / data payments

**Verdict: LIKELY ORDINARY.**

A customer pays money for AI inference, research tooling and compute. This is software sold for
money. No listed category reaches it.

The qualification is that if the same checkout also funds Credits, the transaction is not this
category. Nodal's pricing policy keeps them separate: a Credit purchase is a Credit purchase, and a
service subscription is a service subscription. They are different products and different
PaymentIntents.

## B. Purchase of internal Credits

**Verdict: REQUIRES STRIPE APPROVAL.**

Two categories touch it, and the second one lands squarely.

The first, "stored value or credits maintained, accepted, and issued by anyone other than the
seller", does **not** apply. Nodal issues its Credits, Nodal accepts them, Nodal maintains the
ledger. Nodal is the seller.

The second does apply on its face: "Preloaded payment cards, gift cards, **virtual credits**, or
other products and services in which a monetary value is stored". A Nodal Credit is a virtual credit
in which a monetary value is stored. There is no reading of the product under which that sentence is
not about it.

"Sale of in-game currency or game items, unless the business is the operator of the virtual world"
carries an operator exception Nodal arguably meets — Nodal operates the environment the Credits are
spent in — but Nodal is not obviously a "virtual world", and resting the classification on that
exception would be exactly the kind of convenient reading goal Section 68 forbids.

**What follows:** Credit purchase requires a restricted-business review. `CREDIT_PURCHASE` stays a
disabled capability gate until the review is passed and recorded.

## C. User-created native assets

**Verdict: UNCLEAR, leaning REQUIRES STRIPE APPROVAL.**

Users create Nodal-native assets. Nodal is the first-party issuer of the environment they exist in.

"First-party non-fungible tokens (NFTs) minting and sales, including marketplaces and SaaS
platforms" is restricted. Whether a Nodal-native asset is an NFT is a question of substance, not
vocabulary: these are ledger-native fungible assets in a constant-product market, not tokens on a
public chain, and no NFT standard is involved. That argues the category does not apply.

Against that, the family resemblance is close enough that a Stripe reviewer may treat it as covered,
and the honest answer is that no published sentence settles it.

**What follows:** disclose the mechanism plainly and let Stripe classify it. Do not pick the
favourable reading in the application.

## D. Internal secondary trading

**Verdict: REQUIRES STRIPE APPROVAL. This is the sharpest edge in the product.**

Users trade Credits against user-created native assets on an internal market whose price moves.
"Investment and brokerage services" is restricted. Whether an internal constant-product market for
platform-native assets is a brokerage service is not settled by the policy text.

Two prohibited categories sit nearby and must be actively kept away from:

- "'Get rich quick' schemes, including investment opportunities or other services that promise high
  rewards." Nodal must never present native markets as an expected-return opportunity. This is a
  marketing-copy constraint with a compliance consequence, and it belongs in the frontend honesty
  rules that `apps/web` already enforces by test.
- "Predatory investment opportunities with no or low money down."

**What follows:** `NATIVE_MARKET_TRADING` remains a disabled high-risk capability. The gate exists
in `internal/gates` already. Nothing about this workstream changes that.

## E. Creator service earnings

**Verdict: REQUIRES STRIPE APPROVAL — and this is the Connect question.**

A creator sells a dataset or an agent service to another user, and Nodal settles it internally.
"Payment facilitation and aggregation (including receiving settlement proceeds for goods or services
that you did not provide)" is restricted, and it describes what a marketplace does.

Stripe's answer to this category is Connect. Nodal needs Connect anyway for payouts, so this is not
an additional product — but it *is* an additional thing the platform profile has to declare
truthfully.

**What follows:** `MARKETPLACE` is already classified high-risk in `internal/gates` and requires the
full evidence set. That classification was made for internal reasons (it mints the only withdrawable
provenance) and turns out to be right for external reasons too.

## F. Market-creator fees

**Verdict: LIKELY ORDINARY, contingent on E.**

A fee taken by the creator of a market is revenue share on the activity in D and E. It carries no
category of its own. It inherits whatever verdict D and E receive.

## G. Trading proceeds

**Verdict: UNCLEAR — and the payout question, not the payment question.**

Buying Credits is category B. What makes trading proceeds different is that a user may want to
**withdraw** them, which converts speculative internal gains into real value leaving the system.

Two separate approvals are needed and they are not the same approval:

1. Stripe must accept that the platform pays out trading proceeds
   (`STRIPE_TRADING_PROCEEDS_PAYOUT`).
2. Counsel must determine whether doing so is lawful in each jurisdiction
   (`docs/build/BLOCKERS.md` B-02).

Goal Section 39 is explicit that these are independent, and `internal/gates` already requires both a
provider approval reference and a legal review reference on a high-risk capability.

**Default policy remains:** `MARKET_TRADING_PROCEEDS` is not payout-eligible.
`valuedomain.DefaultPolicy` forbids every origin, and this one stays forbidden until both
approvals exist.

## H. Crypto payout

**Verdict: REQUIRES STRIPE APPROVAL. Currently NOT AVAILABLE.**

"Cryptocurrency exchanges and wallets" is restricted, and "Money transmitters, remittances, currency
exchange services, and other money service businesses" is restricted. Paying a user in USDC for
value they accrued in Credits has the shape of both.

Stripe's own product for this is the Connect stablecoin payout, which is in **private preview** with
its own due-diligence questionnaire. That is Stripe applying its restricted-business process through
a dedicated door rather than the general one.

**What follows:** `STRIPE_STABLECOIN_PAYOUT_ACCESS` is a hard external blocker.
`PAYOUT_RESERVE` and `PAYOUT_SETTLE` stay disabled.

## I. Wallet payouts to a user-controlled external wallet

**Verdict: SUPPORTED IN PRINCIPLE, BLOCKED_EXTERNAL in practice.**

This is the one place the answer is unambiguously good. Stripe's stablecoin payout product sends
USDC to a wallet address the recipient links themselves, on Base or Polygon. Nodal never custodies
it and never submits it. Goal Sections 20 and 24 are satisfied by the product's own design rather
than by Nodal restraining itself.

The constraint is the network. Solana is not supported. See `STRIPE_CAPABILITY_MATRIX.md` §3.

---

## Summary

| Category | Verdict |
|---|---|
| A. Nodal SaaS / AI / data payments | LIKELY ORDINARY |
| B. Purchase of internal Credits | REQUIRES STRIPE APPROVAL |
| C. User-created native assets | UNCLEAR |
| D. Internal secondary trading | REQUIRES STRIPE APPROVAL |
| E. Creator service earnings | REQUIRES STRIPE APPROVAL |
| F. Market-creator fees | LIKELY ORDINARY (contingent on E) |
| G. Trading proceeds | UNCLEAR |
| H. Crypto payout | REQUIRES STRIPE APPROVAL |
| I. Wallet payouts | SUPPORTED IN PRINCIPLE, BLOCKED_EXTERNAL |

Nothing in column two is `NOT SUPPORTED` outright, and nothing is `LIKELY ORDINARY` in the parts
that matter. The product needs a conversation with Stripe, not a form.

## What must never be done to improve these verdicts

From goal Section 68, restated because it is the rule most likely to be violated under deadline
pressure:

- Do not rename Credits to something that sounds less like stored value.
- Do not omit the native markets from the description.
- Do not describe Nodal as "AI SaaS" when payment processing also funds internal Credits and
  user-created markets.
- Do not move the activity to another Stripe account after a denial.
- Do not classify a Credit purchase as a software subscription.

A denial is a recorded blocker and a signal that the architecture may need a different provider.
