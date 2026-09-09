# WALLET INTEGRATION STATE

Where a payout actually lands, who is authoritative for that, and what Nodal
still owes the user.

**Last updated:** 2026-09-08.

---

## 1. The finding that changes the design

Goal Sections 20 to 23 assume Nodal collects a wallet address, proves the user
controls it, and hands it to a payout provider. That is the right design for a
provider that takes an address.

Stripe's stablecoin payout is not that provider.

> "After you opt in to stablecoin payouts and enable the Express Dashboard,
> your users can link a crypto wallet with their account and set their default
> currency to USDC."

The recipient links their own wallet inside **Stripe's** Express Dashboard.
Nodal creates a Transfer in USD to a connected account and Stripe resolves the
destination. There is no parameter on that call that could carry an address,
and `stripepayout` refuses one if you pass it.

So for this rail:

- **Stripe is authoritative for the destination.** A Nodal-side address record
  is a mirror, and treating it as the destination would create a second source
  of truth about where money goes.
- **Stripe verifies the wallet.** Its own documentation describes a one-time
  wallet verification taking up to 24 hours.
- Goal Section 24's prohibitions — never request a seed phrase, never store a
  private key, never sign a payout-wallet transaction, never move funds after
  payout — are satisfied by the product's shape rather than by Nodal
  restraining itself. `payout.Capabilities.DestinationHeldByProvider` says so
  in the type system so no caller has to remember it.

## 2. What Nodal still owes the user

Three things, none of which Stripe does.

### A. The network warning

Stripe pays USDC on **Base or Polygon**. Not Solana.

Phantom supports Solana, Ethereum, Base, Polygon, Bitcoin and Sui, so a Phantom
wallet is a valid destination — but the address the user must give Stripe is
their **EVM address**, not their Solana address. Those are two different
addresses in the same wallet, and a user who supplies the Solana one has
supplied an address on a chain that cannot receive the payout.

Nothing in Stripe's flow knows the user came from a Solana-first product. Nodal
has to say this, in words, before sending the user to link a wallet. This is a
product requirement, not an implementation detail.

### B. The destination-change hold

Goal Section 22 is about account takeover: attacker logs in, changes the wallet,
withdraws immediately. Stripe holds the address, so Nodal cannot gate the
change at the point of change — but it can observe it. A connected account
emits `account.updated`, and a payout hold after an observed destination change
is enforceable on Nodal's side.

This is **not built**. It is the most important unbuilt thing in this document,
and it is a prerequisite for activating `PAYOUT_SETTLE` regardless of what
Stripe approves.

### C. Eligibility Stripe will not check for us

Before sending a user into onboarding, Nodal can already refuse the cases that
will fail later: a country outside the published 67, a US address in New York
or Hawaii, a recipient that is a company rather than an individual or sole
proprietor. `stripepayout.SupportsCountry`, `SupportsUSState` and
`Capabilities.SupportsRecipientKind` exist for this. Wiring them into the
payout eligibility path is **not built**.

## 3. The existing wallet package is a different thing

`internal/wallet` models `EMBEDDED_DELEGATED` wallets — provider-managed,
delegated-signing wallets used by Domain C for on-chain execution. It is not
the payout destination model and must not be extended into one. Goal Section 25
is explicit that the self-custodial real-capital rail and the internal-Credit
payout are separate and their ledgers do not merge.

A payout destination record, when it is written, belongs beside
`payout.Destination`, which already exists and already stores a provider
reference rather than an account number, for exactly this reason:

> "Nodal stores the provider's reference and never the underlying account
> number or key … a bank account number here is a liability with no
> compensating benefit."

The same sentence covers a wallet address on this rail.

## 4. Goal Section 20's field list, mapped

| Field | Where it lives | Note |
|---|---|---|
| `wallet_id` | `payout.DestinationID` | exists |
| `user_id` | `payout.Destination.AccountID` | exists |
| `chain` | not stored | Stripe holds it; a mirror would drift |
| `address` | **not stored** | deliberately. Stripe is authoritative |
| `ownership_verified` | `payout.Destination.Status` | exists; set from Stripe's own state |
| `verification_method` | not stored | would be a constant: "stripe_express_wallet_link" |
| `verified_at` | `payout.Destination.VerifiedAt` | exists |
| `created_at` / `last_changed_at` | `payout.Destination.CreatedAt` / `UpdatedAt` | exists |
| `risk_state` | **not built** | the destination-change hold in §2B |
| `status` | `payout.DestinationStatus` | exists |

Two rows say "not stored" and that is the design, not an omission. Storing an
address Stripe owns would create a second answer to "where does the money go",
and the two would eventually disagree.

## 5. If the payout provider ever changes

If Stripe denies the application, or a different provider is chosen, the
provider may well be one that takes an address. Then everything in goal
Sections 21 and 22 becomes required as written: a signed ownership nonce
carrying domain, account, nonce, address, timestamp, expiry, environment and
purpose; single-use nonce; replay refused; recent-authentication and step-up on
change; cooling period; notification; immutable audit.

`payout.Capabilities.DestinationHeldByProvider` is the switch that says which
world we are in, so the branch is a capability question and not a rewrite.

## 6. State

| | |
|---|---|
| Nodal custodies payout crypto | **No**, and cannot |
| Nodal stores a private key or seed phrase | **No**, and nothing in the codebase can |
| Wallet ownership proven | **Yes, by Stripe**, on the only rail that exists |
| Phantom usable as destination | **Yes**, via its EVM address on Base or Polygon |
| Solana usable as destination | **No.** Stripe does not send USDC there |
| Destination-change hold | **NOT BUILT** — blocks `PAYOUT_SETTLE` |
| Pre-flight eligibility refusals | **NOT BUILT** |
| Network warning in the UI | **NOT BUILT** |
