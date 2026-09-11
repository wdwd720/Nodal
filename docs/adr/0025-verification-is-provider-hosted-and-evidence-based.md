# ADR-0025 — Verification is provider-hosted, evidence-based, and never touches a Credit

Status: **Accepted** (2026-09-10)

Supersedes nothing. Constrains every path by which this system concludes that a
person has been identified. Answers §20 and §21 of the product goal: *"implement
a real state machine"*, *"prefer provider-hosted KYC"*, and *"do not fake this
with a checkbox"*.

## Context

`compliance_profiles` held a five-value `identity_state` enum with no transition
table. Three consequences, each worse than the last:

1. An application statement could move a profile from UNVERIFIED to VERIFIED.
   No edge was recorded, no actor, no provider reference, no reason. The trail a
   regulator would read was whatever the last writer said it was.
2. Age was one boolean and sanctions one enum, with no record of who decided,
   when, under which rule version, or whether the answer was real. §21 asks for
   age, country, state, jurisdiction, sanctions, risk and provider availability
   to each be able to refuse on their own; one boolean cannot express that.
3. `identity.NodalIdentityResolver` could not report anything above
   `NODAL_IDENTITY`, and said so honestly: PAYOUT_KYC and ENHANCED come from a
   provider, and there was no provider and no place to record what one had said.
   So the withdrawal journey stopped before it began, in every deployment.

The external half of (3) is genuinely blocked — signing up for Veriff, Persona
or Sumsub is a human action with an account, terms and a data-processing
agreement (BLOCKERS B-06). The internal half was not blocked; it was missing.

## Decision

### 1. The state machine is the schema's, not the application's (migration 00761)

`compliance_profiles.identity_state` grows to §20's ten states and gains F-42's
full treatment, in exactly the shape migration 00744 established for `accounts`:

- `compliance_profile_transitions` records every change, with the actor, the
  reason, the provider, the provider's reference and the session it came from.
- An AFTER INSERT trigger writes `identity_state`, `verified_at` and
  `expires_at` from the row; `cp_app` loses UPDATE on all three and keeps it on
  the attribute columns, which is also what permits the row lock a transition
  takes.
- The edge binding of 00731/00732 refuses a row that names an origin the
  profile was never in.
- A BEFORE INSERT trigger refuses any birth state but UNVERIFIED, because a row
  inserted VERIFIED never changes and none of the above would apply to it.

Migration 00796 extends the same treatment to `sanctions_state`, which 00761
granted back to `cp_app` one statement after revoking the state beside it. It is
a screening DECISION — `internal/eligibility` reads it as one of the allowlists
that decides whether a payout may proceed — so it rides on the same transition
row (`from_sanctions_state`, `to_sanctions_state`), is written by the same
trigger, and is out of the application's reach. A profile born already screened
records that through a trigger of its own, for the reason the birth control
above exists (F-168).

UNVERIFIED keeps its name rather than becoming §20's `NOT_STARTED`: rows carry
it and `eligibility.Policy` documents allowlist it by name (D-057).

The absence that matters: **nothing reaches VERIFIED except from PENDING, from
RESTRICTED or from SUSPENDED.** There is no edge by which a person becomes
verified without a provider session having decided so.

### 2. A level is EARNED from evidence, not asserted by a state (migration 00762)

`verification_sessions` is one attempt, with the provider-abstract status union
PROVIDER_BOUNDARY §3 derived from Persona, Veriff, Sumsub and Stripe Identity
together — so no one vendor's vocabulary becomes the schema's. It has its own
F-42 treatment, its own birth control, and a partial unique index permitting one
open session per person: two live attempts means two answers arriving in an
order nobody controls, and the later one winning is a race, not a decision.

`verification_checks` is the evidence: one append-only row per sub-check,
naming its kind (identity document, age, jurisdiction, sanctions, PEP), its
outcome, the provider, the provider's reference, the rule version that judged it
and whether it is a SANDBOX answer.

`verification.Resolver` then composes rather than replaces:

```
base NONE            -> NONE, whatever the profile says
state not VERIFIED   -> the base level
window elapsed       -> the base level, even before a sweep moves it to EXPIRED
evidence incomplete  -> the base level
evidence complete    -> PAYOUT_KYC, or ENHANCED with a political-exposure answer
```

A profile in VERIFIED with no sub-checks reports the base level. A state without
evidence is somebody's assertion; a state with evidence is a decision that can
be shown to a regulator.

Two sweeps run in the API process and neither is load-bearing for the table
above. `ExpireOverdue` moves a profile past its window to EXPIRED so the row
stops disagreeing with the level the resolver already reports (D-084).
`ExpireOverdueSessions` closes an attempt that can no longer be decided — a
hosted link past its expiry, or a session the provider was never told about —
which matters because 00762 permits one open session per person, so a link
nothing closes is that person's verification blocked for good (F-170).

### 3. Provider-hosted, and what is therefore not stored

`verification.Provider` is the contract PROVIDER_BOUNDARY §3 derived. Nodal
never renders a document capture — a provider that does not host the flow is
refused at registration. What is stored is §20's list and nothing else: a
provider identifier, a state, timestamps, safe reason codes, eligibility
metadata.

The hosted URL is **not** stored. Those links are single-use, expire in minutes,
and are a credential for resuming somebody else's identity check; one is handed
to the browser that asked for it and written down nowhere.

"Nowhere" has to include the idempotency record, and for a while it did not.
`POST /v1/me/verification/sessions` is a command route, so `runCommand`
marshalled its whole response into `idempotency_keys.response_body` — a row
`cp_readonly` and `cp_ops` may SELECT and `cp_app` may not DELETE — where it sat
for the key's lifetime, for every session anybody started (F-231). The record now
keeps the response minus the fields the product documents as never stored, and a
replay of the same key answers with the session, no link, and a sentence saying
to start another (D-125). The sentence matters: an omission would leave a browser
waiting for a field that is never coming.

### 3a. The state machines are edge sets the DATABASE reads

This ADR and `VERIFICATION_AND_WITHDRAWAL.md` §4 both said that "nothing reaches
VERIFIED except from PENDING, RESTRICTED or SUSPENDED" is a property of the
transition table rather than of the code that reads it. Until migration 00806 it
was a property of `internal/verification.CanTransition` and of nothing else: the
edge bindings of 00731 and 00741 ask whether a transition row names the state the
entity is really in, and never whether the edge it describes exists. One INSERT
as `cp_app` reached VERIFIED with a CLEAR sanctions screen, and one more moved a
session the provider had never been called for to APPROVED (F-227).

`compliance_profile_state_edges` and `verification_session_status_edges` are the
edge sets, populated from the Go tables and held identical to them by
`test/integration/enums`; both apply functions refuse an edge that is not there,
and nothing but `cp_migrate` may write the tables. Evidence gets the matching
rule: a `verification_checks` row must name its session's own provider, and the
session must be in a status only a provider ANSWER produces. The residual —
further check rows on a session that HAS been answered — is recorded in D-121
with the privilege change that would close it.

00806 left one exemption: a transition row whose endpoints are the SAME state
skips the edge check, because 00796's birth screen writes exactly one. What the
exemption actually bought was a row that changes everything EXCEPT the state, and
00731's binding cannot see it — it compares `old_val` to `new_val` and returns
NULL when they are not distinct. A `VERIFIED -> VERIFIED` row carrying
`expires_at` therefore renewed, for as long as the writer liked, a validity
window a provider decided once and that `ExpireOverdue` and `Resolver` both read
(F-265). Migration 00815 narrows it to the one row it was for: a same-state row
must carry a sanctions screen and must not carry `verified_at`, `expires_at`, a
provider, a provider reference or a session, and the apply function leaves both
timestamps alone on such a row rather than trusting the refusal to have found
them NULL.

### 3b. A poll is a call to a provider (migration 00818)

`GET /v1/me/verification/sessions/{id}` calls `Provider.Get` on every request
whose session is not terminal, and it is a GET, so the transport budget that
applies is the General class — 600 a minute per principal. One signed-in person
could therefore make this deployment call an identity provider six hundred times
a minute, against pricing and rate limits that are the provider's.

`verification_sessions.provider_polled_at` records when the provider was last
asked, and a poll inside `verification.PollMinimumInterval` (ten seconds) answers
from the recorded status and calls nobody. It is an interval on the SESSION
rather than a tighter rate-limit class because a budget is per principal — two
tabs are two budgets and the provider sees the sum — and because a poll inside
the interval does not need to be REFUSED: the answer is already on the row. The
write happens before the call, in its own committed transaction, so a crash
between the two cannot leave a provider that was asked and a record that says it
was not (D-133).

### 4. Age, jurisdiction and sanctions are versioned rule tables in code

`internal/verification/rules` holds them, as data with a version string that is
written onto every check it judged. They are code rather than rows because a
rule that can be changed by an UPDATE is a rule nobody reviews. The launch shape
is the United States alone, matching the only payout provider's
`SupportedCountries`, plus an explicit sanctions denylist that must survive the
allowlist widening (D-059). Every threshold has a decision record (D-058).

### 5. The sandbox provider, and the absence of a default

`internal/provider/verifysandbox` refuses PROD on its own account, has no
contract reference (so any registry that does not allow sandboxes refuses it),
and **decides nothing on its own**. A session it has issued sits in
PENDING_USER_ACTION until somebody chooses an outcome explicitly through
`POST /v1/me/verification/sandbox-outcome`. There is no timer that approves and
no default that approves: "approved unless told otherwise" is a fabricated
approval with extra steps.

Its hosted URL is a `sandbox:` reference rather than a page, so nothing mistakes
it for a real identity flow, and the API returns the sandbox control path
alongside it.

Three independent refusals stand between a sandbox outcome and a real
deployment: the handler refuses when `cfg.SandboxTier()` is false, the service
refuses again before anything is written, and a CHECK refuses a sandbox row in
PROD at all.

## What this deliberately does not do

**It does not touch a Credit.** `internal/verification` imports
`internal/credit` and `internal/ledger` nowhere, and an integration test counts
`credit_lots`, `credit_lot_events` and `journal_transactions` across a full
verification to prove it. PROVIDER_BOUNDARY §2 rule 1 states the property; this
is the implementation that keeps it true:

> Verification never mutates Credits. A Role B decision changes the customer's
> financial profile; it does not touch a lot, a balance or a value domain.

What verification changes is what a person may ASK for. What they may actually
receive is the value-domain policy's answer, the capability gates', the legal
router's and the provider's — four independent decisions, and ADR-0026 is about
the second half of them.

## Why this and not the alternatives

- *A boolean `kyc_complete`.* The goal's own anti-pattern list names it. It
  cannot express "verified, and sanctions is under review", which is the state a
  real screening produces most often after "clear".
- *Trusting the state without the evidence.* Cheaper, and it makes every future
  operator tool a way to mint a verification. Requiring the sub-checks means the
  only way to reach PAYOUT_KYC is through something that recorded why.
- *Storing the hosted URL so a resume is cheap.* It is a credential. Resuming
  costs one provider call and keeps nothing that can be stolen.
- *Inferring the jurisdiction from the caller's address.* A geolocated IP is a
  legal determination wearing a network header's clothes. The person says where
  they are, and the rule table judges it.
- *Letting the sandbox provider approve after a delay, like the sandbox payout
  provider settles after ten seconds.* A payout that settles itself moves
  nothing; a verification that approves itself is an approval nobody gave.

## Consequences

- `internal/compliance` keeps the attribute half of the profile and loses the
  state half. `Upsert` ignores `IdentityState`, `VerifiedAt` and `ExpiresAt` and
  returns what the row actually says.
- A deployment with no identity vendor answers UNSUPPORTED on every verification
  route, which is the honest state of a system that cannot verify anybody and is
  different from reporting that somebody failed a check.
- `cmd/api` wires the composite resolver in place of the NODAL_IDENTITY cap, so
  a sandbox tier can reach PAYOUT_KYC and the legal router's payout rule becomes
  reachable for the first time.
- BLOCKERS B-06 narrows: what is blocked is a contracted identity vendor, not
  the code that would use one.

## Evidence

Migrations 00761, 00762; `internal/verification` (state, session, evidence,
rules, provider, service, resolver, snapshot, sandbox) and its unit and
integration suites, including `TestIntegration_VerificationNeverTouchesACredit`,
`TestIntegration_TheVerificationStateIsNotTheApplicationsToWrite`,
`TestIntegration_ASandboxOutcomeCannotExistInProd` and
`TestIntegration_TheSandboxControlIsRefusedOutsideASandboxTier`;
`internal/provider/verifysandbox` and `TestGet_ThereIsNoDefaultOutcome`;
`test/integration/enums` (`TestVerificationStatesAgreeAcrossPackages`);
`docs/product/VERIFICATION_AND_WITHDRAWAL.md`.
