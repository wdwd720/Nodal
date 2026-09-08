# AUDIT FINDINGS

Every defect found while auditing the baseline and while building the migration, with what it was,
how it was found, and what was done. Ordered by when it was found.

Findings in **existing code** are marked `BASELINE`. Findings in code written during this migration
are marked `NEW` — they are recorded with the same weight, because a defect found in your own work an
hour after writing it is the same defect it would have been in six months.

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
row per required scenario, each naming the test. Three items (5, 14, 23) genuinely have no test and
are named, with what each would take. A summary a reader cannot check is worse than a table they can.

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
