// Package stripepayout pays eligible Nodal value out in USDC through Stripe,
// behind payout.Provider.
//
// # What this product actually is
//
// Stripe's stablecoin payout is not a "send crypto to an address" API. It is a
// property of a Connect connected account:
//
//  1. Nodal is a Connect platform, based in the US.
//  2. Each payout-eligible user is a connected account with the Recipient
//     configuration and Express Dashboard access.
//  3. Stripe collects that user's KYC. With dashboard set to express,
//     requirements_collector computes to stripe, so Nodal never holds a
//     government identity document.
//  4. The USER links their own crypto wallet in the Express Dashboard and sets
//     their default currency to USDC. Nodal never submits an address.
//  5. Nodal creates a Transfer in USD to the connected account. Stripe
//     converts and pays out to the linked wallet.
//
// Three consequences follow, and all three are load-bearing.
//
// Stripe owns the payout KYC. That is not a convenience; it is what the
// product's own required setting produces, and it satisfies the goal
// document's Section 16 by construction rather than by restraint.
//
// Stripe holds the destination. A Nodal-side wallet record for this rail is a
// mirror, and treating it as the destination would be a second source of truth
// about where money goes. DestinationHeldByProvider says so in the capability
// set so that no caller has to remember it.
//
// USDC on Base or Polygon. Not Solana. Stripe Express processes USDC payouts
// over the Base and Polygon networks; there is no Solana path in this product.
// A Phantom wallet is still a valid destination because Phantom supports Base
// and Polygon, but the address is the user's EVM address, not their Solana
// one. Those are different addresses in the same wallet.
//
// # Availability
//
// The product is in private preview and this account has not been granted it.
// Capabilities therefore reports AvailabilityRequiresApplication, and Submit
// refuses before touching the network. That refusal is the honest state of the
// integration, not a bug: the goal document's Section 70 exists so that
// SOFTWARE_COMPLETE and STRIPE_PRODUCTION_APPROVED cannot be confused, and an
// adapter that would happily attempt a payout it has no permission to make is
// exactly that confusion in code.
//
// # This package must never
//
//   - report a supported asset, network, region or recipient kind that
//     Stripe's own documentation does not state;
//   - submit a payout while Availability is not usable;
//   - accept a wallet address as an argument -- it does not choose the
//     destination and cannot;
//   - treat a 2xx on a Transfer as settlement: a transfer moves money into the
//     connected account's balance, and the payout to the wallet is a later,
//     separate event;
//   - re-submit after a timeout. Lookup answers what happened, and
//     PAYOUT_STATUS_UNKNOWN exists so that a submission whose outcome is
//     unknown keeps its reservation instead of being retried.
package stripepayout
