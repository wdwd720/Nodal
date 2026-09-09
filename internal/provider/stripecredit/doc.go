// Package stripecredit sells Nodal Credits for fiat through Stripe, behind
// credit.PurchaseProvider.
//
// It is a different product from internal/provider/stripe, which is the crypto
// onramp for Domain C. That one converts a customer's own money into crypto in
// the customer's own wallet and Nodal never holds it. This one takes a card
// payment and issues internal Credits against it, which is the reversible-card
// / irreversible-value problem the whole funding lifecycle exists to manage.
//
// # PaymentIntents, not Checkout Sessions
//
// This is the one design choice here that is forced from outside the code.
// Nodal shares a live Stripe account with another product (see
// docs/providers/STRIPE_ACCOUNT_STRUCTURE.md), and that account already has an
// active webhook destination subscribed to checkout.session.completed. Stripe
// delivers each event to EVERY endpoint subscribed to it. Selling Credits
// through Checkout would therefore deliver every Nodal purchase to the other
// product's billing handler, which knows nothing about Credits.
//
// PaymentIntents avoid the collision because the other product does not
// subscribe to payment_intent.*. Card details are still collected entirely by
// Stripe.js in the browser, so PCI scope is unchanged and no PAN reaches
// Nodal.
//
// # Foreign events
//
// The collision is removed in one direction only. The other product's
// subscription invoices also produce payment_intent.* events, so this adapter
// WILL receive events that are not Nodal's. That is normal on a shared account
// and it is not an error.
//
// Every event is therefore classified before it is acted on:
//
//   - a payment_intent.* event whose metadata does not carry
//     nodal_workstream=NODAL is FOREIGN and is ignored;
//   - a payment_intent.* event whose nodal_environment disagrees with this
//     deployment's is FOREIGN, which is what stops a staging deployment acting
//     on a production purchase that shares the account;
//   - a charge.* or charge.dispute.* event carries no Nodal metadata of its
//     own, so it is linked by the payment intent id it names and the service
//     decides foreignness by whether a funding with that provider reference
//     exists.
//
// The distinction matters operationally: "we ignored it because it was not
// ours" and "it never arrived" look identical in a ledger and completely
// different in an incident, so the reason is recorded rather than inferred.
//
// # The API surface actually used
//
//	POST /v1/payment_intents        create (form-encoded, Idempotency-Key)
//	GET  /v1/payment_intents/:id    retrieve
//
// No Stripe SDK. The wire types are hand-written from the official object
// reference and contain only documented fields. Amounts are integer minor
// units on the wire and stay integers here; nothing is parsed as a float.
//
// # Statuses
//
// The documented PaymentIntent statuses are requires_payment_method,
// requires_confirmation, requires_action, processing, requires_capture,
// canceled and succeeded. They are mapped onto credit.PurchaseStatus and the
// raw string is kept verbatim. Anything unrecognised maps to MANUAL_REVIEW so
// the funding stops rather than being pushed into whichever state looked
// closest.
//
// # This package must never
//
//   - treat a 2xx from the API, or the receipt of a webhook, as settlement:
//     it reports provider status, and only the funding service decides what
//     that means for Credits;
//   - accept a webhook without a verified v1 signature within tolerance;
//   - act on an event that does not carry this deployment's own metadata;
//   - decide how many Credits a payment buys -- that is credit.PricingPolicy,
//     server-side, and this package only records the answer;
//   - persist or log client_secret or the API key;
//   - retry a create without the Idempotency-Key header;
//   - run the fake in STAGING or PROD.
package stripecredit
