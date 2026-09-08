# AUDIT FINDINGS

Every defect found while auditing the baseline and while building the migration, with what it was,
how it was found, and what was done. Ordered by when it was found.

Findings in **existing code** are marked `BASELINE`. Findings in code written during this migration
are marked `NEW` — they are recorded with the same weight, because a defect found in your own work an
hour after writing it is the same defect it would have been in six months.

One finding is **OPEN**: F-31, a concurrency failure seen once and not reproduced. It is in the table
with everything else rather than in a footnote, because a register that only records what was fixed
is a register that rewards not looking.

The audit narrative is in `INDEPENDENT_AUDIT.md`; this is the register.

---

| # | Severity | Origin | Status | Summary |
|---|---|---|---|---|
| F-01 | P2 | BASELINE | fixed | `make integration` ran 1 package while claiming to run 42 |
| F-02 | P2 | BASELINE | fixed | The cross-tenant cursor security test could not reach its own assertion |
| F-03 | P1 | NEW | fixed | Value-domain isolation was direction-blind, defeating `PAYOUT_RESERVE` |
| F-04 | P1 | NEW | fixed | `credit.Issue` double-credited balances when used inside a larger posting |
| F-05 | P2 | BASELINE | fixed | The 00603 state-binding could not guard a table keyed by anything but `id` |
| F-06 | P2 | BASELINE | fixed | The 00603 state-binding could not express two transitions in one transaction |
| F-07 | P2 | NEW | fixed | Two SQL statements were not provably constant, breaking a security control |
| F-08 | P2 | NEW | fixed | A test created a Credit asset per call where the schema permits one |
| F-09 | P3 | NEW | fixed | `internal/valuation`'s fixture predated required value domains |
| F-10 | P2 | NEW | fixed | Payout shortfalls were explained from the wrong set and said nothing useful |
| F-11 | P2 | NEW | fixed | Two states in the payout machine were unreachable, one of them needed |
| F-12 | P2 | NEW | fixed | `RailHostedPartner` reported itself implemented with no adapter behind it |
| F-13 | P3 | NEW | fixed | The market fill test asserted absolute balances on a shared account |
| F-14 | P1 | NEW | fixed | The readiness report claimed a `-race` run that cannot happen on this host |
| F-15 | P1 | NEW | fixed | Internal commerce shipped with no capability gate of its own |
| F-16 | P2 | BASELINE | fixed | `MARKETPLACE` was low risk while gating the minting of withdrawable provenance |
| F-17 | P2 | NEW | fixed | A gate being off was reported as a policy refusal, sending operators to the wrong fix |
| F-18 | P2 | NEW | fixed | The adversarial-coverage summary contradicted the evidence tables in its own document |
| F-19 | P3 | NEW | fixed | A Route field called `MaxAgentAuthority` held a minimum, inviting a backwards gate |
| F-20 | P2 | BASELINE | fixed | Any failure to read `/v1/me` told the customer they were signed out, including a rate limit |
| F-21 | P2 | NEW | fixed | A chaos negative control could not fail, so the guard it defended was unproven |
| F-22 | P3 | NEW | fixed | A load script reported idempotency conflicts as price changes |
| F-23 | P2 | NEW | fixed | Reconciling a finished payout asked the provider nothing and reported agreement it never obtained |
| F-24 | P2 | NEW | fixed | The deployment's legal policy never reached the trade-intent compiler, and no test could tell |
| F-25 | P3 | BASELINE | fixed | The readiness report named a test that does not exist, and 609 such names were hand-checked once |
| F-26 | P1 | BASELINE | fixed | Two required inputs were never supplied, so the internal economy was unreachable in every deployment |
| F-27 | P1 | BASELINE | fixed | A capability check inside a financial transaction read through the pool, deadlocking it under concurrency |
| F-28 | P1 | BASELINE | fixed | No deployment could launch a native market: the act every document called separate was never built |
| F-29 | P2 | BASELINE | fixed | A user could reserve their Credits in a payout request and had no way to release them |
| F-30 | P2 | BASELINE | fixed | A property test could fail on a no-op mutation, and its shrinker would mis-explain any real failure |
| F-31 | P3 | NEW | **OPEN, unreproduced** | Four of a hundred concurrent buyers failed once, on a loaded machine, and the test discarded the reason |
| F-32 | P2 | BASELINE | fixed | The browser test for the internal-economy pages could not tell a working page from a broken one |

---

## F-01 · `make integration` ran one package · BASELINE · P2 · FIXED

**Found by** running `make integration` and enumerating what it executed.

`./test/integration/...` contains exactly one package. The database-backed proof of the financial
core lives behind `//go:build integration` inside `internal/` and `cmd/` — 40 packages that the
target never ran. `make test-all` reported success having executed none of them.

CI had already grown an inline shell loop that enumerated the tag correctly, with a comment saying
the workaround would live there "until that target takes a package list". So CI was sound and the
local command was not, and two definitions of "the integration tests" were free to drift.

**Fix.** `scripts/inttest`: one program both call. It enumerates the build tag, gives each package
its own database (they cannot share one — `internal/event` truncates tables others count rows in),
and refuses to run at all if it finds fewer than two packages, so a broken enumeration fails loudly
instead of passing in a tenth of a second.

## F-02 · The cross-tenant cursor test could not reach its assertion · BASELINE · P2 · FIXED

**Found by** running `test/security` on a database other suites had already used.

`TestLedger_AForgedCursorCannotCrossTenants` selected the *first* customer-owned journal row and then
asserted it belonged to customer-a. Any other suite posting a row first broke the precondition — and
it broke **before** the cross-tenant assertion, so SEC-003 was unproven.

The API is not defective: it takes the owner as a bound parameter and the cursor only as a keyset
position. The defect is that the test could not demonstrate it. Note the asymmetry that made this
worth fixing rather than shrugging at: a security test that stops before its assertion *fails*, which
is survivable; the same test passing while proving nothing would not be.

**Fix.** Select the row by owner instead of asserting about whichever row sorted first. Verified on
three consecutive runs against the same database.

## F-03 · Isolation was direction-blind · NEW · P1 · FIXED

**Found by** `TestIsolation_PayoutPathIsGatedAtBothSteps`, written before the design was believed.

The first draft of `CheckDomains` took only the *set* of domains a transaction touched and permitted
the pair if a conversion was declared in **either** direction. `PAYOUT_PENDING → INTERNAL_CREDIT` is
deliberately ungated so an in-flight payout can always be unwound, so a direction-blind check let
that ungated return path authorise the gated `INTERNAL_CREDIT → PAYOUT_PENDING` reserve step —
defeating `PAYOUT_RESERVE` for anyone able to post a two-domain transaction.

**Fix.** `CheckConversion(from, to, caps)` is directional and is the authority. `CheckPosting`
additionally requires a cross-domain transaction to *declare* which conversion it is, and the
declaration to match the domains present. Regression: `TestIsolation_DirectionIsLoadBearing`.

## F-04 · `credit.Issue` double-credited inside a larger posting · NEW · P1 · FIXED

**Found by** `TestIntegration_BuyMovesValueExactlyWhereItShould`, which asserted the creator's
balance and got twice the fee.

`Issue` did two things: post the movement and record the provenance. A native-market trade already
credits the seller and the creator as legs of a six-entry, two-asset posting, so calling `Issue`
there posted a *second* transaction and credited them again. Provenance and balance then disagreed,
which `VerifyProvenance` caught immediately afterwards.

**Fix.** Split into `Issue` (post + record) and `RecordLot` (record against a posting the caller
already made). The database already refused provenance for units no journal transaction moved, so the
second half of the bug was contained; the first half was real.

## F-05, F-06 · The state-change binding was narrower than its purpose · BASELINE · P2 · FIXED

**Found by** applying migration 00603's binding to `native_assets`.

`cp_require_transition` read `NEW.id`, because every table it guarded had an `id` primary key. A
native asset IS its registry entry and is keyed by `asset_id`; giving it a surrogate key so one
trigger could find it would be the tail wagging the dog.

Separately, the flag was a single value, so an entity could change state only **once** per
transaction: a second flag overwrote the first, and at commit the earlier update found the later
state and was refused. Creating and launching an asset does exactly that.

**Fix.** The function takes an optional id-column argument, and the flag accumulates with membership
checking. Both are widenings — every existing trigger passes one argument and is unaffected — and
membership is exactly as strong as equality was, because writing an immutable transition row is still
the only way to set the flag.

## F-07 · Two SQL statements were not provably constant · NEW · P2 · FIXED

**Found by** `test/security`'s SQL-source analysis in the full sweep. It proves every statement in the
repository is built from constants, so no request-derived string can be concatenated into one.

`internal/credit` assembled a `WHERE` clause from literals and used an `ORDER BY` computed from a Go
map at init. Both were safe and neither was *provably* safe.

**Fix.** Not weakening the check — weakening it for one package weakens it for every package. The
`ORDER BY` is a constant, both lot filters are parameters of one constant statement, and
`TestConsumptionOrderSQL_MatchesTheMap` asserts the constant still implements the Go rank map exactly,
so the map remains the authority and the constant cannot drift.

## F-08, F-09 · Two fixtures broke under the second run · NEW · P2/P3 · FIXED

`internal/ledger`'s value-domain tests created a Credit asset per call, and migration 00711 permits
exactly one — green on a fresh database, red on the second run of the same one. `internal/valuation`'s
asset fixture predated required value domains and had not been caught by the mechanical sweep because
it constructs its asset differently from the others.

Neither was reachable from any test run in isolation. They are the argument for `scripts/inttest`
existing, and for the standing rule that every suite is run twice.

## F-10 · Payout shortfalls were explained from the wrong set · NEW · P2 · FIXED

**Found by** `TestIntegration_ReversibleFundingIsNeverPayable`, which expected `FUNDING_NOT_FINAL`
among the reasons and got only `INSUFFICIENT_ELIGIBLE_VALUE`.

The shortfall explanation was computed from `EligibleLots`, which filters **out** exactly the lots
whose refusal is the answer. A user whose entire balance was ineligible was told "insufficient" and
never why — which is the least useful possible answer to "why can I not withdraw".

**Fix.** Explaining a refusal starts from the refused set. The engine now enumerates every lot the
account holds and reports the distinct reasons.

## F-11 · Two payout states were unreachable · NEW · P2 · FIXED

`VERIFICATION_REQUIRED` could not reach `VERIFIED` (a user may complete identity verification out of
band, with no pending provider flow to sit in), and `SUBMITTED` could not reach `MANUAL_REVIEW`,
which is where a settlement that cannot be recorded has to go.

Both were gaps in the transition table rather than deliberate restrictions.
`TestState_EveryTransitionTargetIsDeclared` now asserts every declared state is reachable, so a
decorative state cannot be added again without noticing.

## F-12 · A rail reported itself implemented with nothing behind it · NEW · P2 · FIXED

**Found by** writing the BLOCKERS entry for the hosted-partner rail and noticing that the sentence
"no adapter exists" contradicted `Implemented() == true`.

The Settlement Compiler refuses to route to an unimplemented rail; reporting `true` would have let it
route to nothing. PART XXII names this failure mode exactly: "do not implement live unsupported
products merely because an interface exists".

**Fix.** `RailHostedPartner.Implemented()` returns `false` until an adapter exists, with the test
asserting it.

## F-13 · A market test asserted absolute balances on a shared account · NEW · P3 · FIXED

Every market quotes against the same Credit asset, so the platform's `MARKET_RESERVE` and fee
accounts are shared by every market — and, in a test, by every test that ran before. Asserting an
absolute balance passed only on a virgin database. The test now asserts deltas, and the shared-account
property became the `VerifyReserves` control-account invariant rather than being worked around.

## F-14 · The readiness report claimed a race-detector run that cannot happen on this host · NEW · P1 · FIXED

**Found by** trying to run `go test -race` over `internal/commerce` and getting
`cgo: C compiler "gcc" not found`. There is no C compiler on this machine — `gcc`, `clang` and `cc`
are all absent — so `go test -race` cannot have succeeded here at any point.

`docs/release/PRODUCTION_READINESS_REPORT.md` nonetheless carried a row reading

> | Race | `-race` over credit, nativemarket, payout | ok |

and a claim that the hundred-concurrent-spends test had run "also under `-race`". Neither was
executed. This is worse than an untested code path: the whole purpose of that document is that every
row has an executable command behind it, and a row that does not is a licence for the reader to stop
checking the others.

**Why it happened.** `-race` is in `make race` and in CI, both of which run on Linux where cgo works.
I recorded the *intent* of that pipeline as a *result* of this session.

**Fix, in four parts.**

1. The report was corrected to state that the race detector had not run here, name the reason, and
   mark the row as delegated to CI rather than as passed.
2. `internal/{credit,nativemarket,payout,commerce}` were added to the CI integration-race loop and to
   `make integration-race`. They were absent, so even in CI those four packages — the ones with the
   database-backed concurrency properties of the internal economy — had never been raced. Fixing the
   false claim without fixing that would have been cosmetic.
3. The concurrency properties that motivated the claim are also asserted without the race detector,
   by outcome rather than by instrumentation: 100 concurrent spends against a balance that funds 10
   yield exactly 10 successes, and 10 concurrent purchases against a balance that funds 2 yield
   exactly 2.
4. **A C compiler was then installed on the host and the run was actually executed.** It did not
   work immediately: the toolchain had been installed under a path containing a space
   (`C:\Users\Mihir Modi\...`), and the MinGW driver embeds its own unquoted path in a linker-script
   argument, so `ld` received `C:/Users/Mihir` and refused with "linker script file ... appears
   multiple times". A directory junction does not fix it — gcc canonicalises through it — so the
   toolchain was copied to a space-free path. `internal/{credit,nativemarket,payout,commerce}` and
   the financial core then passed under `-race` on this host, and the report's row says so with the
   command that produced it.

**Two lessons, not one.** The first is the obvious one: never record an intended command as an
executed one. The second is the reason the first is easy to get wrong — the fix looked impossible
("no compiler on this host"), and an obstacle that looks environmental is exactly the kind a report
quietly routes around instead of removing.

**The general lesson, recorded because it will recur.** A readiness document must distinguish "this
command was run and passed here" from "this command runs in CI" from "this command is intended".
Collapsing the three is how a launch package becomes decorative. Every row in
`PRODUCTION_READINESS_REPORT.md` §2 now carries which of the three it is.

## F-15 · Internal commerce shipped with no capability gate of its own · NEW · P1 · FIXED

**Found by** writing the Settlement Compiler's routing table and having to answer "which capability
gates PURCHASE_INTERNAL_SERVICE?" -- and finding that the honest answer was none.

Stage 8 reasoned that a purchase is a single-domain movement inside `INTERNAL_CREDIT`, so the
ledger's isolation check requires no capability for it. That reasoning is correct and it answered the
wrong question. The isolation check asks "may value cross this boundary"; nobody had asked "may this
deployment run a user-to-user marketplace at all". The result was that internal commerce -- the only
legitimate way `CREATOR_EARNING` / `DATA_SALE_EARNING` / `AGENT_SERVICE_EARNING` provenance comes
into existence -- would have been live on a fresh deployment with no gate, no policy and no decision.

Every other Domain A action had a gate. Commerce did not, precisely because its *isolation* story was
the simplest.

**Fix.** `internal/commerce` now takes a `CapabilityResolver` and refuses a purchase unless
`MARKETPLACE` is ACTIVE. It is checked in the DOMAIN SERVICE, not only in the compiler at the edge: a
gate that lives at the HTTP boundary is one a worker or a script walks around.
`TestIntegration_WithoutTheMarketplaceCapabilityNothingSells` proves the refusal for an unconfigured
resolver and for an explicitly inactive gate, and proves the same purchase commits once it is on --
so the refusal is about the gate and nothing else.
`TestIntegration_ARepeatedPurchaseStopsWorkingWhenTheGateIsPulled` proves the gate is checked before
the idempotency lookup: a replay is not a different act, and it must not keep working after the
marketplace is switched off.

**The general lesson.** "This movement needs no capability" and "this product needs no approval" are
different sentences. The first is about the ledger; the second is about the business. Answering the
first does not answer the second, and Domain A had exactly one place where the two came apart.

## F-16 · `MARKETPLACE` was low risk while gating the minting of withdrawable provenance · BASELINE · P2 · FIXED

`gates.IsHighRisk` classified `MARKETPLACE` as low risk. That was defensible when the capability was
a placeholder meaning "users can sell things some day". It is not defensible now that it gates
internal commerce, because exercising it does two things the function's own stated criterion covers:
it moves Credits between users, and it mints the creator-earning provenance a payout policy may one
day permit to be withdrawn.

Low risk means no legal-review, provider-contract, risk-approval or security-approval reference is
required to propose the gate. It still requires dual authorization -- so this was never a
one-person switch -- but it was a switch two people could throw with no evidence attached to the
only path by which withdrawable provenance is created.

**Fix.** `MARKETPLACE` is high risk, with the reason recorded next to it. The three tests that used
it as their low-risk exemplar now use `SOCIAL_DATA_PERSISTENCE`, which is genuinely low risk and
stays a real test of the low-risk path rather than an assertion that happens to pass.

## F-17 · A gate being off was reported as a policy refusal · NEW · P2 · FIXED

**Found by** an HTTP integration test asserting the status code of a refused purchase and getting
403 where 422 was expected.

The legal router correctly turns an ALLOW whose required gate is inactive into a DENY -- policy and
gate must agree. The first version of the compiler read that DENY as `LEGAL_ROUTER_DENIED`, so a
deployment whose policy already permitted internal commerce and had simply not activated the gate was
told "no approval on record permits this action". An operator reading that would go and edit a policy
that was already correct.

**Fix.** The compiler now distinguishes them: a router DENY whose reason code is
`CAPABILITY_NOT_ACTIVE` produces the capability reason only, with the capability named, because
activating it is the actual next step. A policy that denies on the merits still produces
`LEGAL_ROUTER_DENIED`, and a fresh deployment produces both. `TestCompile_AGateBeingOffIsNotAPolicyRefusal`
pins all three cases.

The refusal precedence at the HTTP edge follows the same principle and is documented where it is
written: rail-not-implemented, then policy, then gate, then verification, then provider -- the order
in which the obstacles would actually have to be removed. Every reason is still in the response; the
ordering only decides which one leads.

## F-18 · The adversarial-coverage summary contradicted its own document · NEW · P2 · FIXED

**Found by** trying to write the per-item mapping and checking each claim instead of copying the
summary sentence.

`PRODUCTION_READINESS_REPORT.md` summarised PART LXXII's thirty required adversarial tests in one
sentence: "items 1–4, 7–11, 15–17, 20–22, 28 have named tests; items 5, 6, 12–14, 18, 19, 23–27,
29, 30 do not yet". Four of those placements were wrong, in both directions:

- Item 6 (provider timeout after success) was listed as missing. `TestIntegration_AProviderTimeoutDoesNotDuplicateThePayout`
  is cited by name **in the payout evidence table of the same document, sixty lines above**.
- Item 12 (hidden supply increase) was listed as missing.
  `TestIntegration_ACreatorCannotChangeEconomicsAfterLaunch` covers it, and is also cited above.
- Item 13 (self-trade) was listed as missing. The surveillance test raises `CREATOR_SELF_DEALING`.
- Items 26 and 27 were listed as missing and were covered by tests that predate this migration.

Nothing was untested that the summary said was tested — the error ran the safe way — but that is
luck, not a property of the process. The sentence was written by recalling what had been built rather
than by checking, which is the same failure mode as F-14 wearing different clothes.

**Fix.** The sentence is replaced by a thirty-row table in `PRODUCTION_READINESS_REPORT.md` §3a, one
row per required scenario, each naming the test. Three items (5, 14, 23) genuinely had no test and
were named, with what each would take. A summary a reader cannot check is worse than a table they
can. All three were closed afterwards — item 23 by way of F-23 — and the table is now thirty of
thirty.

**And then applied to the rest of the document.** Every "not built" claim was re-checked the same
way, which immediately caught another: the report said `apps/admin` had no pages driving the nine new
administrative actions. It has one. The console's propose form is generated from `authority.json`,
which now lists all nine, and carries the free-form params field the two parameterised kinds need, so
they are operable today. The corrected claim is narrower and true: there is no kind-SPECIFIC UI, so
an operator pastes a uuid where a picker belongs.

## F-19 · A `Route` field called `MaxAgentAuthority` held a minimum · NEW · P3 · FIXED

**Found by** re-reading `internal/settlement/compiler.go` looking for exactly this: a name that says
one thing while the value says another.

`Route.MaxAgentAuthority` was populated from `agentauthority.MinimumLevel(...)` — the LOWEST level at
which an action becomes available — and documented as "the highest authority level that may execute
this action". Nothing consumed it yet, which is the only reason it was harmless. The obvious way to
consume it would have been `if agent.Level <= route.MaxAgentAuthority`, which is the exact inversion
of the intended check and would have permitted only the agents that should have been refused.

**Fix.** Renamed to `MinAgentAuthority`, documented as a floor, and pinned by
`TestCompile_MinAgentAuthorityIsAFloorNotACeiling`: an agent at exactly that level may act, one below
may not, and a permanently forbidden action has no minimum at all because no level permits it.

**Why P3 and not P2.** No caller existed. Recorded anyway, because "nothing uses it yet" is a
statement about today and the name would have outlived the memory of what it meant.

## F-20 · A failure to ASK was reported as an answer about the session · BASELINE · P2 · FIXED

**Found by** the browser suite failing intermittently while building the Domain A pages — two or
three tests per run, never the same ones, always with the page rendering the sign-in screen. The
tests that failed were about pages I had not touched, which is what made it worth chasing rather
than working around.

`SessionProvider` computed `signedIn = me.isSuccess`, and `App` rendered `<SignIn />` whenever that
was false. So EVERY way of failing to read `/v1/me` — a dropped connection, a timeout, a 500, a 429 —
produced the sentence *"This session is not signed in."*

That sentence is a claim about the customer's session, and the application did not know it. The
session was fine. What had actually happened was:

```
code RATE_LIMITED · HTTP 429
```

The suite makes enough requests to trip the transport rate limiter, the limiter did exactly its job,
and the interface translated "I was told to slow down" into "you are logged out". A customer hitting
a rate limit would have been shown a sign-in screen and would reasonably have concluded their session
had been terminated.

It was also self-concealing: the same code path discarded the real error, so the 429 never reached
the screen. It took a fix to make the cause visible — the first run after the change failed with the
rate-limit message rendered in full, which is how the root cause was finally identified.

**Fix.**

- `SessionValue` gains `signedOut`, true only when the backend actually answered 401. `App` shows the
  sign-in screen for that and only that; every other failure renders the real problem with a retry
  and the line *"This is a failure to reach the backend, not a statement about your session."*
- `useMe` no longer refuses to retry everything. A 401 is an answer and is never retried; anything
  else gets one more attempt before the application concludes anything about the session.

**Verification.** Three consecutive full browser runs, 66/66, stable at ~52 s. Before the fix the
same suite failed 1–2 tests in three runs out of four.

**The harness half.** The e2e run also needs the transport limits raised, because a 66-test browser
suite is not a human being:

```
CP_API_RATE_LIMIT_GENERAL=100000/1m CP_API_RATE_LIMIT_COMMAND=100000/1m \
CP_API_RATE_LIMIT_QUOTE=100000/1m   CP_API_RATE_LIMIT_AUTH=100000/1m
```

That is a fact about the harness, not a defect: the limiter refuses to be switched off in a
production-like environment, which is correct, and the default budget is the right one for a person.

**The general lesson.** "The answer was no" and "I could not ask" are different facts, and an
interface that collapses them tells the customer something false at the moment they are least able
to check it. The same distinction is why the Settlement Compiler separates a policy denial from a
gate being off (F-17) — this is that principle at the other end of the system.

## F-21 · A chaos negative control could not fail · NEW · P2 · FIXED

**Found by** running the negative control and watching the test PASS.

`test/chaos` holds itself to a rule: every guard carries a named control that
makes it violate the property it defends, so the assertion can be observed
firing. "A guard that has never been seen to fail is recorded as unproven."

The first `commerce_partial_write` control consumed the buyer's Credit lots in a
separate transaction with no posting behind them, on the theory that provenance
would then claim fewer units than the ledger held. It does not: migration 00711
(SQLSTATE CR004) refuses a lot event whose journal transaction never touched the
account, so that drift is **unrepresentable**. The control could not construct
the fault it was named after, the test passed with it active, and the guard was
therefore proving nothing.

This is the more dangerous shape of the problem the file's own header warns
about. A guard nobody exercised is an unknown. A guard whose control silently
passes is false confidence with a certificate attached.

**Fix.** The control is the mirror image: post the Credit movement and skip the
consumption, so the ledger says the buyer paid while provenance still claims
every unit. It now fires, with `VerifyProvenance` producing "buyer provenance
and ledger diverged after the fault". `native_trade_partial_write` was written
the same way and was checked the same way. Both are recorded in `breakNames`
with the reason the mirror image is not usable, so the next person does not
retry the dead end.

## F-22 · A load script reported idempotency conflicts as price changes · NEW · P3 · FIXED

The first run of `test/load/internal_economy.js` reported 16
`internal_purchases_refused_price_changed`. No price had changed. The script
counted refusals by HTTP status, and 409 is three different things on that
endpoint: the price moved (`CONFLICT`), the same key is still in flight
(`IDEMPOTENCY_IN_PROGRESS`), or the same key came back with a different body
(`INVALID_IDEMPOTENCY_REUSE`). What it had actually measured — the shared-key
iterations racing, and the idempotency store correctly refusing the second —
was the more interesting result and was the one it threw away.

**Fix.** Refusals are counted by the CODE the backend gave, with a separate
counter for each. The threshold moved too: it was `http_req_failed < 1%`, which
counts every expected refusal as a failure and would fail a run in which the
system behaved perfectly. It is now "never a 5xx" and "always problem+json",
which are the things that must hold whatever the business answer is. An operator
who has learned to ignore a red threshold has lost the threshold.

---

## F-23 · Reconciling a finished payout reported agreement it never obtained · NEW · P2 · FIXED

**Found by** writing PART LXXII item 23 — a compromised provider sending a
contradictory status — and discovering the scenario was not reachable, which is
a different thing from being handled.

`Service.Reconcile` opened with:

```go
if req.State.Terminal() {
    return req, nil
}
```

That guard is right about the state machine and wrong about the operator. Not
moving a settled or failed payout on the word of a provider is the whole point:
`FAILED` and `REJECTED` have no outgoing transitions and `SETTLED` leads only to
`REVERSED`, so a provider that changes its story cannot move Nodal's money by
lying twice. But the early return also meant the provider was never ASKED. An
operator who reconciled a settled payout got `err == nil` and the request back,
which reads as "the provider confirms it", and the provider had not been
contacted at all. A contradictory answer left no trace anywhere.

That is the same shape as F-14 and F-20: not a wrong action, a **false report**
of a check that did not happen.

**Fix.** A terminal request with a provider key is now looked up, the answer is
recorded as a `payout_provider_events` row whatever it says, and a definite
disagreement — settled-now-failed, failed-now-settled, or settled under a
different provider reference — is returned as `CodeReconciliationRequired` and
logged at ERROR. **No state moves**, because that is the control; what changed
is that the contradiction is now visible instead of discarded.

A provider that has gone back to `ACCEPTED` after settling is deliberately NOT
called a contradiction: read-after-write lag on the provider's side produces
exactly that, and an alarm a coincidence can trigger is one operators learn to
ignore. The same reasoning governs the wash-trade detector added for item 14.

Four tests, including the control that fails if every terminal reconcile is
called a contradiction:

- `TestIntegration_ACompromisedProviderCannotRewriteAFinishedPayout`
- `TestIntegration_AProviderClaimingItPaidAFailedPayoutIsNotBelieved`
- `TestIntegration_AProviderSwappingTheReferenceOfASettledPayoutContradictsItself`
- `TestIntegration_AProviderThatAgreesWithASettledPayoutIsNotAContradiction`

Reaching the scenario at all needed a provider double that can contradict
itself. `payouttest.Sandbox` is deliberately incapable of it, because a correct
provider is; the affordance is a single `Corrupt` method, named after what it
models, in the package the production registry refuses to load.

## F-24 · A deployment's legal policy never reached the intent compiler · NEW · P2 · FIXED

**Found by** reading `git status` and noticing a file that should have been
modified was not.

Putting Domain B and C trade intents through the settlement compiler needed two
halves: a `Ports.SettlementPolicy` the handler reads, and an assignment in
`Wire` that fills it from the deployment's `WireDeps.NativeEconomy`. A script
that applied several edits at once asserted its way out on an earlier step, and
the assignment never landed.

The result: every deployment's trade intents were compiled against the ZERO
VALUE — no legal policy on record, no capability active, no verification,
unknown jurisdiction. That is the conservative policy, so nothing became
permissive; but a deployment that had configured a real policy would have been
refused by a policy it never chose, which is a different wrong answer.

**Nothing caught it.** `TestIntegration_ARealCapitalIntentIsRefusedAtTheEdge`
passed with the assignment missing and passes with it present, because a
harness that configures no policy and a compiler that cannot see the policy
produce the identical refusal. This is F-21's shape again in a different place:
not a guard that failed, a guard that could not distinguish the two cases it
existed to distinguish.

**Fix.** The assignment, plus the two tests that can tell the difference:

- `TestWire_CarriesTheDeploymentsSettlementPolicy` asserts `Wire` carries the
  configured router through, and that an unconfigured deployment still gets the
  conservative policy rather than a nil one. **Observed failing** with the
  assignment removed.
- `TestIntegration_AConfiguredPolicyActuallyReachesTheIntentCompiler` wires a
  policy that PERMITS self-custodial trading and asserts the answer changes —
  the policy version in the refusal is the deployment's own, and the leading
  reason moves from the policy to the gate.

`Ports.SettlementPolicy` is a struct rather than an interface for the same
reason the finding is only P2: its zero value is the conservative deployment,
so the failure mode of forgetting it is refusal rather than permission.

## F-25 · A readiness claim pointed at a test that does not exist · BASELINE · P3 · FIXED

**Found by** asking, mechanically, whether every test name the documents cite
resolves to a declaration.

`docs/release/PRODUCTION_READINESS_REPORT.md` offered
`TestIsolation_CrossDomainPostingMustDeclareItself` as the evidence that a
cross-domain movement must declare its conversion. No such function exists. The
test is `TestIsolation_CrossDomainPostingMustDeclareItsConversion`, and the
property IS proven — so the defect is in the citation and not in the system,
which is exactly why it survived: everything a reader could check by hand said
the right thing, and the one thing they would have had to grep for did not.

The wider problem is the habit rather than the row.
`REQUIREMENTS_TRACEABILITY.md` states that "all 609 Go test-function references
resolve to a `func Test`/`func Fuzz` that exists", hand-verified on one
afternoon. A hand-verified claim about 609 things is one that goes false
without anybody knowing which week it happened. It is F-14 and F-18's shape
again: a document asserting something about the test suite, written by
recalling rather than by checking.

**Fix.** The name is corrected, and `test/docs/references_test.go` now enforces
the property for the five documents a reviewer would actually use to decide
readiness. It is deliberately NOT applied to the whole `docs/` tree:
`THREAT_MODEL.md` and `SECURITY.md` legitimately quote the names of tests they
assert do NOT exist, and a check that could not tell an assertion from a
quotation would force those documents to lie to satisfy it.

A cited name resolves if a declaration matches it exactly, or if it is a family
prefix at an underscore boundary — prose that says "the `TestProp_` suite" is
naming a group, not claiming one function.

**Still open, and named rather than fixed:** `THREAT_MODEL.md` and
`SECURITY.md` carry a 2026-09-05 inventory that lists as absent a dozen
packages that now exist, including a `TestAgentImportBoundary` whose property
is in fact covered three times over (`TestAgentTreesNeverImportAuthority`,
`TestAgentBoundaryNeverImportsSigning`, `TestNoAgentPathImportsWithdrawal`).
That staleness is in the pessimistic direction and was already recorded in the
readiness report; re-scoring those two documents is work, not a claim.

## F-26 · The internal economy was unreachable in every deployment · BASELINE · P1 · FIXED

**Found by** trying to run the Stage 21 load script against a marketplace whose
gate had actually been activated, and watching every purchase refused anyway.

Two inputs the settlement compiler requires were never supplied by `cmd/api`,
and each alone was enough to make every Domain A action impossible.

**The verification level.** `NativeEconomyDeps.Verification` was `nil`, so every
account resolved to `VerificationNone`. Every Domain A profile requires
`NODAL_IDENTITY`. The reasoning recorded in BLOCKERS B-06 — "a deployment that
cannot establish identity has not established it" — is right about a KYC
provider and wrong about this rung of the ladder. `NODAL_IDENTITY` is "a
verified email address and/or passkey: a person can be reached and can log in",
which this system establishes at login and persists as `users.email_hash`.
Reporting NONE for it was not conservatism, it was under-reporting.

**The capability.** `gateCapabilityResolver` answers only about the keys in
`conversionCapabilities`, and a key it does not answer about is reported
inactive. `MARKETPLACE` was not in that list. So the gate could be ACTIVE in the
database, three principals could have approved it with evidence, an operator
could have enabled it in configuration, and every purchase would still be
refused `CAPABILITY_NOT_ACTIVE` — indistinguishable from a gate nobody had ever
approved.

Both are fail-closed, which is why they were invisible: refusals looked exactly
like the refusals a correct fresh deployment produces. But a control that no
configuration can ever satisfy is not a control. It is dead code with a reason
attached, and the whole Domain A surface — the marketplace, native asset
creation, native market trading — sat behind it.

**Fix.**

- `internal/identity.NodalIdentityResolver` reports `NODAL_IDENTITY` when the
  owning user is ACTIVE and the identity provider asserted a verified email
  address, and NOTHING above it. `PAYOUT_KYC` and `ENHANCED` remain unreachable
  and B-06 is narrowed to what it actually blocks: payouts. A passkey-only
  account with no email still reports NONE, which is under-reporting again, in
  the same direction, and is named rather than hidden.
- `internal/identity` now fills `users.email_hash` on a later login when it is
  absent, because it used to be written only at user creation: an account
  created before its email was verified could never reach NODAL_IDENTITY
  afterwards however many times the provider asserted it. It fills an absence
  and never overwrites.
- `MARKETPLACE` and `LIVE_MANUAL_TRADING` are added to the resolver's list, and
  `settlement.AllRequiredCapabilities()` is exported so the list can be CHECKED
  rather than remembered: `TestCapabilities_ResolverAnswersEverythingTheCompilerCanRequire`
  fails on any capability a compiler profile can require that the resolver never
  answers about. **Observed failing** with MARKETPLACE removed again, naming it.

After the fix a purchase committed over HTTP for the first time.

## F-27 · A capability check inside a transaction deadlocked the connection pool · BASELINE · P1 · FIXED

**Found by** running 50 concurrent purchases once F-26 made purchases possible,
and finding p95 latency of **30.07 seconds**, five commits out of two hundred,
three 500s and 124 requests killed by the statement timeout.

`pg_stat_activity` said it plainly: nine of ten pool connections `idle in
transaction`, each waiting on `ClientRead` with `begin isolation level read
committed` as its last statement. The database was not busy. It was holding
transactions open for clients that had gone away to do something else.

The something else was the capability check. `ledger.Service.Post` runs
`checkDomainIsolation` inside the caller's transaction, and
`commerce.Service.Purchase` runs `requireMarketplace` inside its own; both asked
the capability resolver, and the production resolver held a `db.Querier` that
was **the connection pool**. So every financial write, while holding one pooled
connection for its transaction, reached for a second one — twelve times, once
per capability.

With a pool of ten and a dozen concurrent writes, every connection is held by a
transaction whose owner is waiting for a connection that will never be released.
Nothing recovers until the 30-second statement timeout, and then it all fails at
once.

**Why nothing caught it.** Every test supplies a MAP as the capability resolver,
so only the production resolver touches the database and only production could
deadlock. And the one place it would have shown — concurrent purchases through
the API — could not commit a single purchase because of F-26. Three defects
stacked so that the third was unobservable until the first two were fixed.

**Fix.** Both resolver interfaces take the caller's `db.Querier`, which is the
transaction the posting is being written in, so no financial write ever reaches
for a second connection while holding its first. `gates.Checker.ActiveSet`
answers every capability in ONE query instead of one per capability, so the
round trips inside a transaction go from twelve to one. The configuration check
still runs first and per capability, so no database read can be what enables a
capability.

**Measured, same script and host, before and after:**

| | p95 latency | committed | 5xx | timed out |
|---|---|---|---|---|
| before | 30.07 s | 5 of 200 | 3 | 124 |
| after | 469 ms (10.3 ms on successful requests) | 31 of 200, the entire seeded balance | 0 | 0 |

`TestIntegration_ConcurrentPurchasesDoNotStarveTheConnectionPool` holds the line
with a pool of TWO, which makes the failure certain rather than probable.
**Observed failing** — hanging until its own deadline — with the resolver put
back on the pool.

## F-28 · No deployment could launch a native market · BASELINE · P1 · FIXED

**Found by** continuing the re-audit the same way F-26 and F-27 were found:
driving the other half of Domain A through a live API instead of reading it.

`POST /v1/native-assets` creates an asset in DRAFT. Nothing could move it after
that. `nativeasset.SetStatus`, `nativeasset.Activate` and `nativemarket.Create`
had no caller outside tests — no HTTP route, no administrative action kind, no
executor. So a creator could publish an asset and no operator in any deployment
could ever open its market.

The moderation-verdict executor even says so in its own comment: "Recording
APPROVED does not start trading. Activating a market is a separate act behind
its own capability." The separation is right. The separate act did not exist.

Every existing test built its market by calling the services directly, which is
exactly why nobody noticed that no operator could. This is F-26's shape a second
time: a path that only tests can walk looks finished from inside the tests.

**Fix — the two halves of the chain, kept apart on purpose.**

- **The creator's half.** `POST /v1/native-assets/{assetId}/submit` moves the
  creator's own DRAFT to PENDING_REVIEW. Only the creator, and only their own:
  a stranger gets NOT_FOUND rather than FORBIDDEN, because telling somebody an
  asset exists but is not theirs is a membership oracle. Submitting is what
  freezes the economics for review, so an operator cannot launch a draft its
  creator is still editing.
- **The operator's half.** `NATIVE_MARKET_LAUNCH` is a new administrative
  action kind: **dual control**, a five-minute step-up window and a one-hour
  expiry. It is the only action in the internal economy that MINTS — every unit
  that will ever exist is created by the posting it triggers, and the economics
  lock behind it — so it is proposed by whoever may halt a market and approved
  by whoever may resume one, the same pairing that guards every other addition
  of risk. An approval to launch is an approval of THESE economics at THIS
  moment, which is what the short expiry is for.

The executor refuses an asset moderation has not approved, refuses a DRAFT with
the reason, refuses a missing or non-positive opening price (there is no default
worth guessing — a default would price somebody's asset for them), and is keyed
by the APPROVAL id, so one approval mints one supply however many times it is
executed.

Three tests drive the whole chain through the real surfaces — creator submits,
compliance approves the content, two operators launch, and then somebody buys —
because a launch that produced an unusable market would satisfy every assertion
about states.

## F-29 · A payout could be requested and never withdrawn · BASELINE · P2 · FIXED

**Found by** the same question as F-26 and F-28, asked systematically instead of
one path at a time: which methods that change money have no caller a deployment
can reach?

`payout.Cancel` was one of them. It is well tested — three service-level tests
cover it — and nothing outside the test suite called it. So a user could request
a payout, watch their Credits leave their spendable balance into
PAYOUT_RESERVED, and have no way to get them back. Only an operator could, and
only by running their own tool against the database.

Reserving somebody's money with no path to release it is not a conservative
control. It is a trap, and it is the kind that looks fine in review because the
reservation logic is correct.

**Fix.** `POST /v1/payouts/{payoutId}/cancel`. It returns the reserved Credits
to the exact lots they came from with their provenance intact, and it refuses a
SUBMITTED request — that one may already have been paid, and the only honest way
out of it is reconciliation, which `payout.Cancel` already said.

The endpoint introduces a question the service never had to answer — WHO may
cancel — so that is what
`TestIntegration_AUserCanCancelTheirOwnPayoutAndNobodyElses` tests: a stranger
gets NOT_FOUND rather than FORBIDDEN, because a distinguishable refusal is a
membership oracle, and the refused attempt changes nothing.

## The class, made mechanical

F-26, F-28 and F-29 are one defect three times: **a path only tests can walk
looks finished from inside the tests.** Every one was correct, covered, and
unreachable.

`test/reachability` is that question turned into a check. Every exported method
on a financial service that takes a transaction — every method that changes
money — must have a caller in `internal/`, `cmd/` or `scripts/`, or an entry
saying in words why not. A caller in `test/` does not count; that is the point.

Eleven methods are exempt today and every reason is external: the Credit funding
lifecycle needs a payment provider (B-04), payout destinations need a payout
provider to verify them and somewhere to send the money (B-01, B-06), and
PAYOUT_KYC is by definition a provider's determination. A second test refuses
any exemption whose reason does not name a blocker `BLOCKERS.md` actually
defines — an exemption without an external cause is a defect wearing a comment.

**Its first version could not fail.** It matched `.Method(ctx`, and `Cancel`,
`Create`, `Execute` and `SetStatus` are names four of the five packages share,
so every method looked reachable from everywhere. Two changes fixed it: a call
must pass a transaction as its second argument, and the calling file must import
the declaring package. It was then observed failing on both defects it was
written for — `payout.Cancel` with the endpoint removed, and
`nativeasset.Activate` with the launch executor removed.

## F-30 · A property test's shrinker would have mis-explained a real defect · BASELINE · P2 · FIXED

**Found by** `go test ./...` failing on a commit that had just passed the full
integration sweep. Randomised tests do that: the seed changes every run.

`TestProp_ByteFlipsNeverApprove` asserts that flipping bytes of a golden
transaction never yields an approval from the signing inspector. It drew 2
flips, both at position 0, both XOR 1 — and two flips of the same bit cancel.
The "mutated" message was byte-identical to the golden one, the inspector
approved it, correctly, and the test called that a security failure.

The false alarm is the small half. The dangerous half is what rapid does next:
**it shrinks toward small values**, so `flips=2, pos=0, xor=1` is where the
shrinker goes from ANY failure it finds. A real defect — an inspector that
approved some genuinely mutated transaction — would have been shrunk into
"identical bytes approved" and reported with an explanation that had nothing to
do with it. The engineer who chased it would have found a test bug, fixed the
test, and never seen the defect.

That is the same shape as F-21 pointing the other way. F-21 was a control that
could not fail; this is a control that fails for the wrong reason and hides the
right one.

**Fix.** The case is discarded when the mutation cancels out, with the reasoning
in the code, because the property is about MUTATED transactions and a
bitwise-identical one is not mutated. Discarding it also keeps the shrinker away
from it.

**Both controls were run.** Making the guard swallow every case leaves the test
passing while proving nothing — which is why the guard is `bytes.Equal` and not
anything broader. Making the inspector approve everything makes the test fail,
so the property still catches what it is for. The property was then run six
times at 3,000 checks each, and the whole property tier three times over.

The stale `testdata/rapid/...fail` file rapid wrote is deleted: left in place it
pins every future run to the degenerate case.

## F-31 · Four concurrent buyers failed once and nobody can say why · NEW · P3 · OPEN

**Status: open and unreproduced.** Recorded rather than closed, because "it
passed the next eight times" is not a diagnosis.

`TestIntegration_ConcurrentBuyersSerialiseWithoutBreakingAnything` runs 100
concurrent buys against one market. On one run it reported `every funded buyer
should succeed; 4 failed`, and passed on the immediately following run against
the same database.

**Nobody can say why, and that is the finding.** The test counted failures and
threw the errors away, so a failure produced a number with no cause attached.
That is now fixed — it keeps the first error and reports it — but the run that
failed is gone and the message with it.

What is known:

- The failing run took 24.2s against 16–20s for every subsequent run, on a
  machine that was simultaneously serving a k6 load test and another suite.
  A timeout under contention (`lock_timeout` is 5s, `statement_timeout` 30s)
  is the most likely explanation.
- It did not recur in eight consecutive full-suite runs or six runs of the test
  alone, against the same database.
- The run's OTHER assertions are the ones that would matter: the state version
  moved once per trade, the curve invariant held, and supply reconciled. Those
  are not reported as failing, which means the four buys that failed left
  nothing behind — consistent with a refused transaction rather than a partial
  one.

**Why it is P3 and not higher.** Every financial invariant in that test passed.
A buy that fails is a buy that did not happen, which is the correct outcome of a
timeout; the money property this test exists to defend was not violated. What is
unproven is whether the cause was environmental.

**What would close it.** The error text from a recurrence. The test now captures
it, and CI runs this suite on every commit.

## F-32 · A browser test that could not tell a working page from a broken one · BASELINE · P2 · FIXED

**Found by** noticing an uncommitted change in the working tree — somebody
else's, mid-fix — that made two list queries unwrap `{items: [...]}`, and asking
why 69 browser tests had not caught what it was fixing.

Verified independently of that change: `openapi.yaml` declares
`NativeAssetPage` and `InternalProductPage` as objects with an `items` array,
and the committed client called `validatedList`, which throws
`ContractViolation("expected an array")`. So at HEAD the Native Markets asset
list and the Marketplace product list rendered "This response could not be
trusted" on every load.

**The test that should have caught it asserted only this:**

```
await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
await expect(page.locator("h1")).toHaveCount(1);
await expect(page.locator(".malformed")).toHaveCount(0);
```

The heading renders before any request is made. Every one of those passes on a
page whose every panel is an error. The test proved the route existed.

**And the first fix did not work either.** Adding
`expect(page.getByText("This response could not be trusted")).toHaveCount(0)`
still passed against the broken client — because Playwright retries an
assertion until it passes, and `toHaveCount(0)` passes the instant it is
evaluated, which was before the query had resolved. A negative assertion with
no positive signal before it proves nothing at all. It now waits for
`networkidle` and for every spinner to clear first.

**Observed failing.** With the client's paged-response handling put back to the
committed version, `Native Markets renders` fails. With it restored, all 17
render tests pass against a live API and a real Postgres.

**What is NOT asserted, deliberately.** That no refusal appeared. This
deployment refuses plenty and demanding otherwise would be demanding the
interface lie. The assertion is narrower and is exactly the thing a heading
cannot tell you: that the data loaded at all.

The client-side fix itself is somebody's uncommitted work and is left where it
was found; this finding is about the test that let it through.

## Findings deliberately NOT raised

Stated so their absence is a decision rather than an oversight:

- **The chart of accounts is duplicated between Go and SQL.** Both were extended together and a test
  pins the Go side's shape, but nothing mechanically proves the two lists are identical. Worth a
  generator or a cross-check test; it is a maintainability risk, not a defect.
- **Capability activation is not enforced in the database.** Migration 00710 says so explicitly and
  gives the reason: a gate is keyed by environment, and a connection carries no environment the
  application could not simply assert. Adding a session GUC would look like a control and be none.
- **`internal/compliance` is 248 lines with one test.** It is the thinnest financial package in the
  tree and was already flagged in the KEEP/MODIFY matrix as needing a real `ComplianceDecision`. It
  is under-built rather than wrong.
- **`internal/instruments` has 593 lines and one test function** for a package with status authority
  over what may be traded. Under-tested for its authority; not observed to be incorrect.
