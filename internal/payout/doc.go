// Package payout decides whether internal value may leave the system, reserves
// the exact units, and reconciles what an external provider did with them
// (gola.md PARTS XVIII–XXI).
//
// # Nodal does not send money
//
// PART XVIII is explicit that Nodal itself must not be the thing that sends
// arbitrary fiat or crypto. A Provider does. This package decides eligibility,
// reserves value, hands the provider a request under a key Nodal chose, and
// reconciles the answer. Every provider is behind an interface with a contract
// test and a sandbox fake; none is implemented against an unverified API, and
// live connectivity is BLOCKED_EXTERNAL until a real contract exists.
//
// # Eligibility is per-unit, not per-balance
//
// "Can this user withdraw 500 Credits?" has no answer. The answer depends on
// which 500: a promotional grant is never withdrawable, purchased Credits are
// not withdrawable by default, a creator's earnings might be if a policy and a
// capability both say so, and value backed by a card payment inside its dispute
// window is not withdrawable no matter what its origin is. Engine therefore
// works from internal/credit's provenance lots, and a payout reserves the
// specific lots it was approved against.
//
// # Fail closed, twice
//
// The default policy (valuedomain.DefaultPolicy) forbids every origin, so a
// fresh deployment can pay nobody out. On top of that, both ledger movements
// that a payout performs are capability-gated in the database:
// INTERNAL_CREDIT → PAYOUT_PENDING needs PAYOUT_RESERVE and
// PAYOUT_PENDING → EXTERNAL_SETTLED needs PAYOUT_SETTLE. Turning off
// PAYOUT_SETTLE stops value leaving even if every policy row says otherwise.
//
// The return path, PAYOUT_PENDING → INTERNAL_CREDIT, is deliberately ungated.
// If PAYOUT_SETTLE is revoked while payouts are in flight, the reserved value
// still has to be able to get back to the user; a capability check there would
// strand it (PART XXXII).
//
// # The uncertain answer
//
// A submission that times out may have succeeded. PART XXI forbids releasing
// the reservation and retrying blindly, so the request moves to
// PAYOUT_STATUS_UNKNOWN, the reservation stays, and reconciliation resolves it
// by asking the provider about the idempotency key. That key is persisted
// BEFORE the provider is called, so a crash in between still leaves something
// to reconcile against — which is the crash test of PART XXXVIII applied to
// this provider category.
//
// # The emergency controls reach it
//
// A conversion request is a WITHDRAW-class operation (POLICY_AUTHORITY §2), so
// Create, CompleteVerification and Submit each call KillSwitchChecker.Check
// inside the transaction that authorizes them, and each reads the account's
// status in the same transaction. WITHDRAWALS_DISABLE and GLOBAL_NEW_RISK_KILL
// stop every one of them; ACCOUNT_FREEZE stops the named account's; a FROZEN,
// RESTRICTED or CLOSED account cannot open or advance one. None of that was here
// until F-163: this package imported no kill switch at all, so the only
// withdrawal surface the product can actually reach was the one surface no
// emergency control touched.
//
// # What this package must never do
//
//   - Pay out value whose provenance the eligibility decision did not approve.
//     Consumption is restricted to exactly the approved origins.
//   - Release a reservation because a provider call failed in an ambiguous way.
//   - Return a bare quantity when a payout is cancelled. It returns the exact
//     units to the exact lots, or a user could launder a promotional grant into
//     an earning by requesting a payout and cancelling it.
//   - Treat a webhook as authority over a state it did not observe. Provider
//     events are recorded raw first and interpreted second.
//   - Decide a jurisdiction question. It consumes policy; it does not author it.
package payout
