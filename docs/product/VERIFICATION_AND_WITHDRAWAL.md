# VERIFICATION AND WITHDRAWAL — the whole journey, and who decides what

Status: product architecture, 2026-09-10. Implements goal §19–§25 against the
boundary in [`PROVIDER_BOUNDARY.md`](PROVIDER_BOUNDARY.md). Decisions:
[ADR-0025](../adr/0025-verification-is-provider-hosted-and-evidence-based.md),
[ADR-0026](../adr/0026-the-conversion-request-is-payout-requests.md), D-057 to
D-062.

**Nothing in this document is a provider's approval, a legal opinion, or a
claim that anybody may withdraw anything today.** `LEGAL_APPROVED` is false and
`valuedomain.DefaultPolicy` still forbids every origin. What this document
describes is the software, which is finished, and the external items it is
waiting on, which are not.

---

## 1. The sentence the architecture has to protect

From goal §19, in as many words:

> Show: *"Verify your identity to enable withdrawal eligibility."*
> Do NOT say: *"Verify now to turn your Credits into cash."*
> **The architecture must preserve the distinction.**

It does, and not by convention. `internal/verification` imports
`internal/credit` and `internal/ledger` **nowhere**. There is no column a
verification decision writes that any Credit writer reads. An integration test
counts `credit_lots`, `credit_lot_events` and `journal_transactions` across a
complete verification and requires all three to be unchanged
(`TestIntegration_VerificationNeverTouchesACredit`).

Verification changes what a person may **ask for**. What they may actually
receive is four other decisions, each made by a different thing.

---

## 2. The journey, end to end

```
                      the person                       the platform
                      ──────────                       ────────────
  Withdraw  ─────────────────────────────────────────► GET /v1/me/eligibility
                                                        (per-origin buckets,
                                                         reasons, next steps)
                          │
       REQUIRES_VERIFICATION │ "verify your identity to enable
                             │  withdrawal eligibility"
                          ▼
  POST /v1/me/verification/sessions ────────────────►  verification_sessions
       (jurisdiction supplied, never inferred)          state CREATED
                          │                                 │
                          │◄──── hosted URL (single-use, stored nowhere)
                          │
             completes the provider's hosted flow
                          │
  GET /v1/me/verification/sessions/{id} ───────────►  poll the provider,
       (never trust the redirect)                     ingest the decision
                                                          │
                                                      verification_checks
                                                      (5 sub-checks, each with
                                                       provider, reference, rule
                                                       version, sandbox flag)
                                                          │
                                                      compliance_profiles
                                                      state → VERIFIED
                          │
  GET /v1/me/verification ──────────────────────────► the §24 profile area
                          │
  POST /v1/me/payout-destinations ─────────────────►  payout_destinations
       (a PROVIDER TOKEN, never an account number)     status UNVERIFIED
                          │                                 │
                          │                            provider accepts
                          │                            status → VERIFIED
                          ▼
  POST /v1/me/terms-acceptances ───────────────────►  terms_acceptances
       (WITHDRAWAL_DISCLOSURE, asked HERE and           version + content hash
        never at signup)                                of the bytes shown
                          │
                          ▼
  POST /v1/payouts/quote ──────────────────────────►  payout_quotes
       (gross, fee, net, minimum_ok NET of fees,       expires in 5 minutes
        expiry, what value would leave)
                          │
  POST /v1/payouts  (names the quote) ─────────────►  payout_requests
                                                      DRAFT → ELIGIBILITY_CHECK
                                                        → VERIFIED (reserved)
                                                        → SUBMITTED
                                                        → PROVIDER_PENDING
                                                        → SETTLED
                                                      INTERNAL_CREDIT
                                                        → PAYOUT_PENDING
                                                        → EXTERNAL_SETTLED
```

---

## 3. Who decides what

Four independent decisions stand between Credits and money. None of them can
substitute for another, and the product tells a person which one is refusing.

| Question | Decided by | Refusal a person sees |
|---|---|---|
| Who is this person? | a licensed identity provider, through `verification.Provider` | `REQUIRES_VERIFICATION` |
| May value of this PROVENANCE ever leave? | `valuedomain.Policy` — counsel's and the provider's decision, persisted as a version | `ORIGIN_NOT_WITHDRAWABLE` |
| Is this deployment permitted to do it at all? | `capability_gates` (`PAYOUT_RESERVE`, `PAYOUT_SETTLE`) and `legalrouter` | `CAPABILITY_INACTIVE` |
| Will a provider actually send it? | `payout.Provider.Capabilities` — rails, currencies, countries, minimums, fees | `PROVIDER_UNAVAILABLE`, `MINIMUM_NOT_MET` |

Plus three facts about the person that are not about the money:
`JURISDICTION_RESTRICTED` (the versioned rule table), `ACCOUNT_RESTRICTED`, and
`TERMS_NOT_ACCEPTED` — the withdrawal disclosure, below.

`ACCOUNT_RESTRICTED` is a freeze, a sanctions screen that is not clear, or a
restriction recorded against the compliance profile. Two of those three are
written by something today: the account status is written by the account state
machine, and `compliance_profiles.sanctions_state` is derived from the provider's
sanctions and political-exposure checks. `compliance_profiles.restrictions` is
read by both surfaces and **written by nothing** — the operator action that would
write one is a dual-controlled admin kind with an approval, an executor and a
transition row, and it does not exist yet (F-234). The clause is kept because the
column is read and the refusal is composed from it the moment anything writes
one.

`GET /v1/me/eligibility` composes all seven and reports them per origin bucket,
because eligibility is decided per unit of provenance and not per balance. Two
people holding "18,450 Credits" can have entirely different withdrawable
amounts, and that is correct.

The conversion request composes the same seven, from the same reader
(`httpapi.WithdrawalDeps.complianceFacts`), inside the transaction that reserves
the value. Until F-226 it composed four: an open sanctions review, a restriction
and an unsupported jurisdiction stopped this page and stopped nothing on the path
that actually moves money.

### The withdrawal disclosure (§48, D-084)

`WITHDRAWAL_DISCLOSURE` is the one document in `internal/terms` whose
`Requirement` is `BEFORE_WITHDRAWAL` rather than `AT_ONBOARDING`, because asking
for it at signup is exactly the frontloading §6 forbids: a person buying Credits
to spend inside the product has not asked to take anything out and should not be
made to read a document about it.

It is required at the two points where somebody does ask:

| Surface | What happens without it |
|---|---|
| `GET /v1/me/eligibility` | reason `TERMS_NOT_ACCEPTED`, `eligible: false`, and **the withdrawable figures stay true** |
| `POST /v1/payouts/quote` | `422 TERMS_ACCEPTANCE_REQUIRED`, before the quote is priced or stored |
| `POST /v1/payouts` | `422 TERMS_ACCEPTANCE_REQUIRED`, before any request row or reservation exists |

Three properties of that are deliberate.

**The refusal is in the domain service, not the handler.** `payout.Service.Create`
and `payout.Service.Quote` take `DisclosureAccepted` and refuse without it.
`internal/payout` has more than one way in — a person pressing a button, an
operator resolving a stuck payout, a retry, a worker — and a check that lived
only in the HTTP layer would apply to one of them. The field has no "unknown"
value: `false` refuses, so a caller that forgets to supply it stops a payout
rather than permitting one.

**The eligibility page still shows the real numbers.** An unsigned disclosure is
the normal state of everybody who has never withdrawn. Zeroing the buckets would
tell those people their money is stuck when an unread document is the whole of
it, so `TERMS_NOT_ACCEPTED` lowers the verdict and changes no figure — the same
shape `MINIMUM_NOT_MET` already had. It is a next step, like
`REQUIRES_VERIFICATION`, and the product presents it as one.

**The code is its own.** `TERMS_ACCEPTANCE_REQUIRED` (422) rather than
`VERIFICATION_REQUIRED`, which would send somebody into an identity flow they
may already have completed, or `FORBIDDEN`, which says the account may not do
this at all. The problem carries `documents`, so a client can present exactly
the ones outstanding and retry.

An acceptance counts only when the version AND the content hash match the bytes
served now (D-053), so amending the disclosure without bumping its version is a
document nobody has agreed to — and everybody is asked again.

---

## 4. The verification state machine (§20)

Ten states on `compliance_profiles.identity_state`. `UNVERIFIED` is §20's
`NOT_STARTED` under the name the schema already used (D-057).

```
  UNVERIFIED ──► REQUIRED ──► STARTED ──► PENDING ──► VERIFIED
       │            ▲            │  ▲         │  │        │
       │            │            ▼  │         │  └──► RESTRICTED
       │            └──── NEEDS_INFORMATION   │            │
       │                         │            ▼            ▼
       └──────────────────────── └──────► REJECTED      EXPIRED
                                                │           │
                                                └──► REQUIRED ◄┘

  SUSPENDED is reachable from every state and returns to
  VERIFIED / RESTRICTED / REJECTED / REQUIRED.
```

**The absence that matters:** nothing reaches VERIFIED except from PENDING
(a provider decided), from RESTRICTED (a limitation was lifted) or from
SUSPENDED (an operator restored a standing that was already decided). There is
no edge from UNVERIFIED, REQUIRED, STARTED, NEEDS_INFORMATION, EXPIRED or
REJECTED. A person cannot become verified without a provider session having
decided so, and that is a property of the transition table rather than of the
code that reads it.

**The state is not the application's to write.** Migration 00761 gives it F-42's
full treatment: a transitions table, an edge binding, a trigger that writes
`identity_state`, `verified_at` and `expires_at` from the row, no UPDATE for
`cp_app` on any of the three, and a birth control so a profile cannot be
INSERTed already verified.

**Neither is the sanctions screen.** 00761 left `sanctions_state` in the
attribute grant beside the state it had just protected, so one UPDATE cleared a
screening HIT with no edge, no actor and no evidence — and that column is one of
the allowlists `internal/eligibility` reads before a payout may proceed (F-168).
Migration 00796 carries the screen on the same transition row
(`from_sanctions_state`, `to_sanctions_state`, both NULL when the row says
nothing about it), has the same trigger write it, and takes the column out of
`cp_app`'s reach. A profile that is BORN holding a screen records that too,
through a trigger, because an INSERT is not a change.

**A level is earned from evidence.** A profile in VERIFIED with no sub-checks
reports only what Nodal establishes by itself. PAYOUT_KYC needs the four §21
checks to have PASSED — identity document, age, jurisdiction, sanctions — and
ENHANCED needs a political-exposure answer on top. A decision whose window has
elapsed reports the lower level immediately, before any sweep moves the state
(D-061).

**What runs on a timer, and what it is not for.** Two passes run in the API
process, because this deployment has no worker tier (render.yaml):

- `verification.Service.ExpireOverdue`, every five minutes, moves a VERIFIED
  profile past its validity window to EXPIRED (D-084). It decides nothing: the
  resolver above already reports that person at their base level and did so
  before any sweep ran. What it buys is that the row stops disagreeing with the
  level, which is what makes §20's EXPIRED — whose next step the profile view
  renders as REVERIFY — a state a deployment can reach.
- `verification.Service.ExpireOverdueSessions`, on the same ticker, closes an
  ATTEMPT that can no longer be decided: a hosted link past the expiry the
  provider gave it, and a session still in CREATED that the provider was never
  told about (a crash between writing the row and making the call), after
  `UnstartedSessionGrace`. This one has somebody waiting on it. Migration 00762
  permits exactly one OPEN session per person, so until it ran, a link that
  expired left its owner unable to start verification again at all — and "your
  link expired, start again" is what §20 says happens next (F-170).

Neither touches a Credit, a lot or a balance. A verification decision changes
what a person may ASK for; it never changes what their Credits ARE.

---

## 5. What Nodal stores, and what it refuses to store

§20 lists what a provider owns and what Nodal keeps. This is that list, and
nothing beyond it.

**Stored:** a provider identifier; a provider reference; a state; timestamps; a
safe reason code; the rule version that judged each check; whether the answer is
a sandbox one; the jurisdiction; a boolean for "age verified" and the threshold
it was judged against.

**Never stored:** an identity document or its identifier. A social security
number. A date of birth. A full name obtained from a document. A bank account
number, routing number, IBAN, card number, private key or seed phrase. **The
hosted session URL** — those links are single-use credentials for resuming
somebody else's identity check, so one is handed to the browser that asked for
it and written down nowhere.

"Nowhere" includes the idempotency record. `POST /v1/me/verification/sessions`
is a command route, so its whole response used to be written to
`idempotency_keys.response_body` — a row `cp_readonly` and `cp_ops` may SELECT
and `cp_app` may not DELETE — and sat there for the key's whole lifetime
(F-231). The record now keeps the response minus the fields this list names, and
a replay of the same key answers with the session, no link, and a `resume`
sentence telling the client to start another (D-125).

A payout destination is a provider TOKEN plus a mask ("••••4242"). The request
schema has no field for an account number, `payout.ValidateDestinationToken`
refuses an input that looks like one (a bare number, an IBAN shape, a Luhn-valid
card run, a seed phrase), and the response never echoes the token back.

---

## 6. The conversion model (§22), and why there is no second table

`payout_requests` **is** the ConversionRequest. The mapping is in ADR-0026 §"The
mapping, stated once"; the short form:

- thirteen states, a transition table, an edge binding, and
  `PAYOUT_STATUS_UNKNOWN` — the state PART XXI insists on, where the reservation
  STAYS while reconciliation finds out whether the money moved;
- `payout_allocations` records exactly which provenance lots were reserved, so
  cancelling returns exactly what it took;
- three ledger movements keep the value domains apart:
  `INTERNAL_CREDIT → PAYOUT_PENDING → EXTERNAL_SETTLED`, the middle one gated on
  `PAYOUT_RESERVE`, the last on `PAYOUT_SETTLE`, and the return path
  deliberately ungated so a revoked capability cannot strand somebody's money;
- Nodal's idempotency key is written before the provider is called.

What this work added: a destination whose usability is a recorded decision, a
quote that stands until it expires, and a provenance read model.

**There is no `UPDATE credits SET redeemable = true` anywhere, and there is no
primitive by which Nodal itself converts Credits to money.** The contract
instructs a licensed provider; that is all it does.

---

## 7. Provenance through trading (§23), and the order that surprises people

A payout does not take "500 Credits". It takes specific units from specific
lots, and `GET /v1/payouts/{id}` reports which, in the order they leave.

**The reservation takes exactly the lots the decision evaluated.** That is a
fact about the code and not a phrase: `payout.Engine.Evaluate` records the lots
it approved in `Decision.Lots`, and `payout.Service.reserve` passes their IDS to
`credit.Consume` as `ConsumeRequest.LotIDs`, with the payout-grade finality
filter (UNFUNDED and SETTLED only — `RequireSpendableFinality` is for SPENDING,
and it admits REVERSIBLE). It used to pass the set of ORIGINS those lots carried,
which is a coarser thing than a lot in two ways that both leaked: a decision
approving a SETTLED purchase was filled from a REVERSIBLE one of the same origin,
and a decision approving trading proceeds out of a settled purchase was filled
from proceeds out of a promotional grant (D-136, F-270).

`payout_allocations` records the lot, the origin, the ORIGIN FLOOR and the
quantity, so a settled payout can be told afterwards from one drawn on value a
grant funded, and the provenance breakdown reports the two separately rather than
summing them into one line of `MARKET_TRADING_PROCEEDS`.

The order is `credit.ConsumptionRank`, which runs from the **most restricted**
origin to the least:

```
promotional → competition reward → admin adjustment → refund → purchased
  → provider settlement → market proceeds → market creator fee
  → agent service → data sale → creator earning
```

That is a structural property of what the value IS, not of what a policy
currently permits — `internal/credit`'s own comment gives the reason: ordering
by current payout eligibility would make the same spend consume different lots
before and after a policy change, so a provenance question asked twice could get
two answers.

The consequence for a withdrawal, stated plainly because it is the opposite of
the intuition: among the origins a policy PERMITS, the most restricted permitted
one leaves first — purchased value before earnings. An origin the policy forbids
is **never selected at all**, so it can never appear in what left.

### The half lot selection does not answer (D-131)

This section used to end by saying that §23's requirement — *do not allow:
nonwithdrawable source → trade → magically payout-eligible balance* — "holds by
construction rather than by a check somebody could forget", on the strength of
the SELECTION argument above. It did not. Selection decides which of the lots an
account HOLDS may leave; §23 is about a TRANSFORMATION that produces a new lot of
a different origin. A trader holding only `PROMOTIONAL` Credits buys into a
market and sells back out, and what they hold afterwards is
`MARKET_TRADING_PROCEEDS` — an origin the sandbox policy permits — which lot
selection then correctly selects (F-261).

What makes §23 hold is a lot's PROVENANCE, recorded in two forms beside its
finality and maintained by a trigger the application may only read from:

* `credit_lot_state.root_origins` — every origin the lot's provenance bottoms
  out in. `valuedomain.Policy.Permits` releases a lot only when the policy
  releases the lot's own origin AND every root. A grant round-tripped through a
  market is still a grant, and a lot half funded by a purchase is still half a
  purchase under a policy that refuses purchases.
* `credit_lot_state.origin_floor` — the single most restricted of those roots,
  which is what a screen shows and what an ordering sorts by.
  `cp_credit_origin_floor_rank` and `valuedomain.CreditOrigin.Restriction()` are
  held identical by `TestIntegration_GoAndSQLAgreeOnHowRestrictedEveryOriginIs`.

The floor alone was the whole permission answer until D-138, and it could not be.
"Most restricted" is ranked across the two policies THIS BUILD ships, with the
origin's own name breaking a tie — so a lot funded by a purchase and by trading
gains records `MARKET_TRADING_PROCEEDS`, and a policy that releases trading gains
and closes the purchased float releases it. That policy is one of the answers
B-02 can come back with. A rank cannot be conservative for a policy it was not
computed from; a set can, because every root is asked (F-275).

Both are computed from the SAME recursive walk to the provenance roots, on every
`credit_lot_parents` row, and a parent row may not be written for a lot that is
already somebody else's parent — so a lot's provenance is closed before anything
derives from it, and a floor cannot depend on the order the rows were inserted in
(D-137, F-271). The refusal is `CREDIT_PARENT_AFTER_DESCENDANT`.

`GET /v1/me/eligibility` reports the floor on the bucket as `origin_floor`
whenever it differs from the origin, the roots as `root_origins`, and the first
root the live policy refuses as `refused_root` — because
`ORIGIN_NOT_PAYOUT_ELIGIBLE` on a bucket of trading proceeds is an answer nobody
can act on: what a person needs to read is that the value came from a promotional
grant. `verification_would_suffice` is never set on such a bucket — verifying will
not release it, and saying otherwise is the refusal §19 forbids, dressed as
encouragement.

**The buckets are provenance buckets, not origin buckets.** One entry per
(origin, floor, root set, finality), so every unit in a bucket gets one answer
from the policy and `withdrawable_now` is the sum of the buckets. It was one
bucket per ORIGIN, folded to the least final finality and the most restricted
floor in it, and one refused lot then zeroed every other lot of its origin: the
page reported `eligible: false` and `withdrawable_now: 0` beside a positive
`payout_eligible`, and the conversion request it said was impossible succeeded
(F-272). The two figures are now equal by construction under no account-level
block, and `eligibility.ExplainWithdrawal` refuses the request rather than
rendering a response that contradicts itself.

The pooled reserve of a native market is drawn down WORST FIRST (D-132) for the
same reason: a pool is fungible, so "whose Credits left" is a choice, and any
choice but the worst hands a seller better provenance than the pool actually
holds.

---

## 8. What is sandbox-only

Everything below exists **only** on a deployment that declared itself a sandbox
tier (`CP_API_LEGAL_POLICY=SANDBOX`, refused in PROD by `config.Validate` —
ADR-0023). Each is labelled a rehearsal everywhere it is stored and shown.

| Affordance | What it is | How PROD is protected |
|---|---|---|
| `internal/provider/verifysandbox` | an identity provider that decides nothing on its own | refuses to be constructed in PROD; no contract reference, so a registry that does not allow sandboxes refuses it |
| `POST /v1/me/verification/sandbox-outcome` | choose what a rehearsal verification decides | the handler refuses when not a sandbox tier; the service refuses again; the CHECK refuses the row in PROD |
| `verification_sessions.sandbox`, `verification_checks.sandbox` | the label | `CHECK (NOT sandbox OR environment <> 'PROD')` |
| `internal/provider/payoutsandbox` fee model | placeholder fee figures for the quote | the version string reads `SANDBOX-PLACEHOLDER-NOT-A-PRICE`; refuses PROD |
| `payout_quotes.sandbox`, `payout_destinations.sandbox` | the label | `CHECK (NOT sandbox OR environment <> 'PROD')` on the quote |
| the sandbox destination "tokenisation" | the sandbox provider accepts a handle with no real numbers | only reachable when the configured provider reports `SANDBOX_ONLY` |

**There is no default outcome anywhere in the sandbox path.** A rehearsal
session nobody has answered stays PENDING_USER_ACTION forever. "Approved unless
told otherwise" is a fabricated approval with extra steps, and the goal forbids
one.

The five outcomes a sandbox operator may choose are `VERIFIED`,
`NEEDS_INFORMATION`, `REJECTED`, `UNDERAGE` and `SANCTIONED`. The last two are
separate from `REJECTED` because §21 requires an age failure and a sanctions
failure to be separately expressible, and both pass the document check on the
way — which is the whole point of five sub-checks rather than one boolean.

---

## 9. What remains BLOCKED_EXTERNAL

Every item below is a human action. None was or could be performed by
engineering, and the software behind each is complete.

| Blocker | What is missing | What is waiting on it |
|---|---|---|
| B-06 | an identity-verification vendor: an account, terms, a data-processing agreement, a retention decision | a real `verification.Provider`. The contract, the state machine, the evidence model and the resolver exist; nothing is selected. |
| B-01, B-05 | a licensed conversion provider, and whether one will accept a closed-loop credit that becomes convertible only at withdrawal | a real `payout.Provider` with a contract reference. The registry refuses one without. |
| B-09 | Stripe's restricted-business determination, read in a browser by a person | whether the Role A path can be the launch path at all |
| B-02 | counsel: is Nodal's closed-loop Credit float itself stored value requiring a licence; may trading gains ever be withdrawn; what jurisdiction matrix applies | every `PayoutAllowed: true` rule in a persisted `valuedomain.Policy`. The default still forbids every origin. |
| B-11 | the 1099 filing election; business KYB on Nodal by the Role C provider | a live payout |

Two things that are **not** blocked and should not be described as such: the
verification state machine, and the conversion-request surface. Both are built,
tested against a real database, and reachable end to end on a sandbox tier.
BLOCKED_EXTERNAL is for software that is complete and an external item that is
genuinely unavailable (goal PART 167); it is never for missing code.

**"Reachable end to end on a sandbox tier" was false when it was written, and is
true now.** Every earning this system minted — trading proceeds, a creator
earning, marketplace proceeds and the fees on them — was created at REVERSIBLE
unconditionally, and the only writer that promotes a lot out of REVERSIBLE keys
on a `credit_fundings` row an earning never has. So five of the six origins the
sandbox payout policy marks withdrawable could not be withdrawn on any
deployment, and this page's `FUNDING_NOT_SETTLED` — which `internal/eligibility`
documents as a reason waiting fixes — was shown on value whose finality nothing
could move (F-230). D-124 makes a derived lot record the lots that funded it and
take the least final finality among them, and `credit.Service.SettleDerived`
promotes it when they settle.

**And it is reachable for the right value, which is not the same claim.** The
sentence that stood here said "an earning funded by a sandbox tier's UNFUNDED
grants is payout-eligible at birth". It was true and it was the defect: a grant
is an origin no policy in this build releases, so an earning funded by one is
§23's forbidden round trip and D-131 closes it by giving every lot an origin
floor (§7, F-261). What is reachable end to end on a sandbox tier is the journey
that starts with a PURCHASE: a person buys Credits, the payment clears, they sell
something or trade, and the proceeds carry a `PURCHASED` floor that
`SandboxPolicy` releases at PAYOUT_KYC. That is the journey the browser suite
drives and the one the sandbox fixtures fund.

An earning funded by a card payment inside its dispute window still waits for
that payment, which is what `FUNDING_NOT_SETTLED` has always claimed; and a
seller drawn against another trader's reversible contribution waits for THAT
payment (D-132), which is the same reason one person further away.

**What is still genuinely blocked here.** Nothing in this section: the
provenance model and the draw-down order are code, and they are built. B-02
remains what decides whether any `PayoutAllowed: true` rule exists in a persisted
policy at all, and until counsel answers it `DefaultPolicy` forbids every
origin — which the provenance model does not change, because a root set can only
ever refuse more than the origin alone would.

**And B-02's answer is now safe to persist either way round.** The question is
written as two: is the closed-loop float itself stored value, and may trading
gains ever be withdrawn. One of its answers — the float is stored value, the gains
are not — is a policy that releases `MARKET_TRADING_PROCEEDS` and closes
`PURCHASED`, which is not a superset of anything this build ships. A model that
recorded one ranked origin per lot would have released value a purchase half
funded under exactly that policy; the root SET is judged against whatever policy
is in force, so a later policy is conservative for every shape of answer and not
only for the permissive one (D-138, F-275).

**A refused payout says why.** A reserved request whose destination stopped being
usable stays VERIFIED with its value reserved — failing it would return the
reservation on the strength of a fact the person can undo — and it now carries
`blocked_reason`, which the Withdraw page renders beside the Cancel control.
There is no route that re-points a payout at a different destination: the
destination is what the quote, the fee and the provider idempotency key were all
computed against, so a new destination is a new request (D-139, F-277).

---

## 10. What the product says, and does not say

From `PROVIDER_BOUNDARY.md` §7, restated because this is the document a
copywriter will read:

Nodal says:

- *"Credits are internal platform value and aren't directly withdrawable."*
- *"Withdrawal eligibility requires identity verification and an approved payout
  method."*
- *"Verify your identity to enable withdrawal eligibility."*

Nodal does not say:

- that verification turns Credits into cash;
- that a withdrawal is approved, before a provider has said so;
- that any provider has approved Nodal;
- that the model is legal.

The API enforces the first list and cannot express the second. `REQUIRES_VERIFICATION`
is a **next step** and is reported as one, with an action a person can take;
`ORIGIN_NOT_WITHDRAWABLE` is a policy decision about what the value is, and
verifying does not change it — so the eligibility response reports
`verification_would_suffice` per bucket, and it is false for a promotional grant
however verified the person becomes.
