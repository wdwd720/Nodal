# ADR-0026 — The conversion request is `payout_requests`, with a destination, a quote and its provenance

Status: **Accepted** (2026-09-10)

Supersedes nothing. Answers §22 of the product goal — *"THIS IS CRITICAL. Do not
implement: KYC complete → UPDATE credits SET redeemable=true. Instead create a
separate conversion process"* — by naming which existing thing IS that process,
and completing it.

## Context

PROVIDER_BOUNDARY §4 left one question open in writing:

> the exact mapping (which existing table is the ConversionRequest, which state
> machine carries verification) is recorded in `VERIFICATION_AND_WITHDRAWAL.md`
> once built.

The repository already had a conversion process and did not call it one.
`payout_requests` has thirteen states, a transition table, an edge binding, lot
allocations that record exactly which provenance units were reserved, a
provider idempotency key chosen before the provider is called, a
`PAYOUT_STATUS_UNKNOWN` state that holds the reservation while reconciliation
finds out, and three ledger movements that keep `INTERNAL_CREDIT`,
`PAYOUT_PENDING` and `EXTERNAL_SETTLED` apart.

Three things it did not have, and a customer needs all three before committing:

1. **Where.** `payout_destinations` existed, with a status column any
   application statement could write and no country, so
   `Capabilities.SupportedCountries` and `ExcludedRegions` had nothing to read.
2. **What it costs.** §19 and §22 want the fee and the net shown before the
   customer commits. Nothing computed one, so the number on the screen and the
   number charged would have been computed twice by two pieces of code.
3. **What is leaving.** §23's provenance was recorded in `payout_allocations`
   and shown nowhere. "500 Credits" answers "how much" and not "which of my
   money", and those are different questions when half a balance is a
   promotional grant.

## Decision

### The mapping, stated once

| Product-goal concept (§22) | This system |
|---|---|
| `ConversionRequest` | `payout_requests` (13 states, migration 00713) |
| eligible source lots / provenance | `payout_allocations`, reported by `payout.Provenance` |
| conversion rate | the Credit pricing policy VERSION on the quote; never a stored decimal |
| fees | `payout_quotes.fee_quantity` / `fee_amount_minor`, from the provider's `Capabilities` |
| provider | `payout_requests.provider` + `internal/payout.Registry` |
| destination | `payout_destinations` (migration 00713, lifecycle in 00763) |
| state | `payout_requests.state`, bound to `payout_request_transitions` |
| evidence | `payout_provider_events`, raw before interpretation |
| idempotency key | `payout_requests.idempotency_key`, Nodal's own, written before the provider is called |

There is no second conversion table and there will not be one. A parallel
concept would be a second answer to "did this money leave", and the whole point
of PART XXI's `PAYOUT_STATUS_UNKNOWN` is that there is exactly one.

### 1. A destination status is the provider's decision (migration 00763)

F-42's treatment, as on `compliance_profiles`: a transitions table, an edge
binding, a trigger that writes `status` and `verified_at`, no UPDATE for
`cp_app` beyond the label a person renames, and a birth control so a destination
cannot be INSERTed already usable.

The lifecycle keeps the four names it had rather than taking §25's sketch
(VERIFYING / ACTIVE / DISABLED), because working code and a registered enum
pairing use them; the mapping is D-060. REJECTED and DISABLED are terminal: a
destination never comes back, and re-adding one is a NEW row with its own
creation time — which is what lets §25's cooldown on a changed destination be a
fact about the data rather than a field somebody remembers to reset.

A person may disable their own destination and may not decide that it may
receive value. The second is the provider's.

**Nodal never accepts a bank account number, a card number, an IBAN, a routing
number, a private key or a seed phrase.** The request schema has no field for
one, `payout.ValidateDestinationToken` refuses an input that looks like one, and
the table stores a provider token and a mask. The heuristic is calibrated
towards accepting: it refuses a bare number, an IBAN shape, a Luhn-valid card
run and a seed phrase, and accepts a token that merely contains digits, because
a validator that rejects valid input at random is one somebody turns off.

### 2. A quote is what the customer was shown (migration 00764)

A separate pre-commitment call, as PROVIDER_BOUNDARY §3 requires. Both sides are
exact integers — Credits in base units, money in minor units — and there is no
rate column holding a decimal: the Credit price is a pricing-policy VERSION,
recorded, and the arithmetic is redone from it if anybody needs to check.

Three properties are load-bearing:

- **`minimum_ok` is judged NET of fees**, because sub-minimum dust is destroyed
  rather than returned. False is recorded rather than rounded away.
- **A provider with no published fee model produces no quote**, rather than a
  quote of zero. A zero fee that means "we do not know" is how a customer is
  promised a net amount nobody agreed to.
- **An expired quote refuses the request** rather than being silently re-priced.
  A person who saw a number and pressed the button a quarter of an hour later is
  told the number moved, not charged a different one.

It has no status column and needs none: created, expires at a written time,
consumed at most once. Those are facts about time, not states, and a trigger
refuses to un-consume it or to change anything else. Uniqueness is
`(account_id, idempotency_key)`, so another caller's identical key cannot
collide with somebody's quote at all.

`POST /v1/payouts` names the quote, and `internal/payout.Create` consumes it
inside the transaction that reserves the value — so one quote funds exactly one
payout, and an expired or mismatched one refuses before anything is decided
about the money.

### 2a. The state, the money and the price are the transition row's (migrations 00807, 00808, 00810)

`payout_requests` is the ConversionRequest, and until this wave it was the one
state column in this area the application could still write. 00713 granted
`cp_app` table-wide UPDATE; 00733 narrowed it and left `state`,
`reserved_quantity`, `settled_quantity`, `reserved_at` and `settled_at` on the
grant. One transaction moved a REJECTED request to SETTLED with the whole amount
settled and a forged provider reference, and the holder was shown it (F-229).

It now has the treatment its three neighbours got: a legal-edge table populated
from `payout.StateEdges()`, a SECURITY DEFINER trigger that writes the state and
refuses an edge that is not in it, the money carried ON the transition row rather
than written beside it, and a column grant back for only what the application
owns. Every place this package wrote a quantity it was also moving the state, so
the two were always one event; writing them as two was what let a quantity be
rewritten alongside a lawful move (D-123).

Two more things the row now records, because the alternative was deriving them
later from something that may have moved:

- **the price the customer was shown** — the quote's gross, fee and net and the
  currency they were in (00808). The quote itself is REQUIRED now: it was
  optional, and the entire minimum-and-fee branch of `Create` sat inside the
  check for it, so a payout below the provider's published minimum was reserved
  and settled with the fee never taken (F-224, D-119).
- **whether it was a rehearsal** — `sandbox` and `environment` (00810), written
  at creation. The by-id read used to answer that question from today's provider
  mode and the other two surfaces did not answer it at all (F-232).

### 3. Provenance is a read model, and the order is not the intuitive one

`payout.Provenance` folds `payout_allocations` by origin and orders them by
`credit.ConsumptionRank`. Nothing is recomputed and no ledger semantics change.

The order deserves stating plainly because it is the opposite of §23's sketch:
consumption runs from the MOST restricted origin to the least — promotional,
competition reward, admin adjustment, refund, purchased, provider settlement,
then the earnings. `internal/credit` gives the reason: ordering by current
payout eligibility would make the same spend consume different lots before and
after a policy change, so a provenance question asked twice could get two
answers.

The consequence for a withdrawal: among the origins a policy PERMITS, the most
restricted permitted one leaves first — purchased value before earnings — and an
origin the policy forbids is never selected at all, so it can never appear in
what left. That is §23's real requirement ("do not allow: nonwithdrawable source
→ trade → magically payout-eligible balance") holding by construction rather
than by a check.

## Why this and not the alternatives

- *A new `conversion_requests` table.* Two tables answering "did this money
  leave" is the defect `PAYOUT_STATUS_UNKNOWN` exists to prevent.
- *Computing the fee at request time instead of quoting.* Then the number the
  customer saw is the one nobody kept, and the two computations diverge the
  first time a provider reprices.
- *A quote with a status column.* It would need a transition table and a trigger
  to express "later than a timestamp".
- *Storing the conversion rate as a decimal.* It would be the first float in
  this system's money, or a decimal string nobody could reproduce. A version
  identifier is reproducible.
- *Deleting a destination on request.* A destination value has left through is
  financial history, and a row that can be removed is a row an incident cannot
  be reconstructed from.

## Consequences

- `GET /v1/me/eligibility` explains withdrawal eligibility per origin bucket
  with reasons a person can act on, composing the payout policy, the capability
  gates, the verification level, the jurisdiction and the provider. It moves and
  reserves nothing.
- `test/reachability`'s exemption for `payout.CreateDestination` is dropped. Its
  stated reason was that collecting bank details with nowhere to send them is
  worse than not offering it, and the surface does not collect them.
- The fail-closed default is untouched: `valuedomain.DefaultPolicy` still
  forbids every origin, so on any deployment that is not a sandbox tier every
  bucket answers `ORIGIN_NOT_WITHDRAWABLE` and no quote leads anywhere.
- What remains BLOCKED_EXTERNAL is unchanged and is named in
  `VERIFICATION_AND_WITHDRAWAL.md`: a licensed conversion provider (B-01, B-05),
  the Stripe restricted-business determination (B-09), and the counsel decision
  about which origins may ever be withdrawn (B-02).

## Evidence

Migrations 00763, 00764; `internal/payout` (`destination.go`, `quote.go`,
`provenance.go`) and its unit and integration suites, including
`TestIntegration_ADestinationStatusIsNotTheApplicationsToWrite`,
`TestIntegration_AQuoteStandsAndIsSpentOnce`,
`TestIntegration_AnExpiredQuoteRefusesThePayout` and
`TestIntegration_APayoutSaysWhatValueIsLeaving`;
`internal/eligibility/withdrawal.go` and `TestExplainWithdrawal_TheSandboxPolicyPerOrigin`;
`internal/httpapi/handlers_payout_destinations.go`, `handlers_eligibility.go`;
`docs/product/VERIFICATION_AND_WITHDRAWAL.md`.
