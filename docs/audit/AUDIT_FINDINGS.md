# AUDIT FINDINGS

Every defect found while auditing the baseline and while building the migration, with what it was,
how it was found, and what was done. Ordered by when it was found.

Findings in **existing code** are marked `BASELINE`. Findings in code written during this migration
are marked `NEW` — they are recorded with the same weight, because a defect found in your own work an
hour after writing it is the same defect it would have been in six months.

Three findings are **OPEN**: F-31, a concurrency failure seen once and not reproduced; F-42, a
database control whose stated guarantee does not hold; and F-47, two statements about who may read
encrypted PII that contradict each other. All three are in the table with everything else rather
than in a footnote, because a register that only records what was fixed is a register that rewards
not looking.

**F-47 is also the one wrong call in this register.** It was raised as a privilege leak and a fix was
written and applied before the integration suite refused it. The entry keeps the whole sequence
rather than the conclusion, because "an auditor's framing was taken without checking whether the
repository stated a different contract" is the more useful thing to have written down.

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
| F-33 | P2 | BASELINE | fixed | No browser test completed a transaction, so nothing proved a customer could finish anything |
| F-34 | P1 | BASELINE | fixed | The whole risk kernel was unreachable: no deployment wrote a policy and nothing evaluated one |
| F-35 | P2 | BASELINE | fixed | `make lint` had never passed either: 70 findings, 56 of them a linter arbitrating British English |
| F-36 | P1 | BASELINE | fixed | A read permission authorized writes: one ADMIN session could trade, buy and reserve payouts out of any customer's balance |
| F-37 | P1 | BASELINE | fixed | `POST /v1/native-markets/{id}/orders` had never worked in any deployment — no caller set `effective_at` |
| F-38 | P1 | BASELINE | fixed | A policy DENY carrying the gate's reason code became a PERMITTED route |
| F-39 | P2 | BASELINE | fixed | The no-float linter never looked at the internal economy |
| F-40 | P2 | NEW | fixed | This register claimed a database guarantee that the trigger it named does not make |
| F-41 | P2 | BASELINE | fixed | Three by-id reads answered more than they should: a membership oracle and two unpublished records |
| F-42 | P2 | BASELINE | **OPEN** | The AU001 audit binding trusts a session variable any caller can set |
| F-43 | P1 | BASELINE | fixed | MARKETPLACE is high-risk in Go and was not in SQL, so the database evidence check never fired for it |
| F-44 | P1 | BASELINE | fixed | Every native-market price was displayed at the wrong scale, as a sixteen-digit number of Credits |
| F-45 | P2 | NEW | fixed | The web unit suite had been red on the purchase spec, which parsed money into doubles |
| F-46 | P2 | BASELINE | fixed | The nine-page browser check asserted absences before the page had loaded |
| F-47 | P2 | BASELINE | **OPEN** | Two deliberate statements about who may read encrypted PII contradict each other |
| F-48 | P2 | BASELINE | fixed | Five SECURITY DEFINER functions did not pin `pg_temp` |
| F-49 | P2 | BASELINE | fixed | An account chose its own exemption from the negative-balance guard |
| F-50 | P2 | BASELINE | fixed | An asset could be stored with no value domain: a CHECK that evaluates to NULL accepts |
| F-51 | P2 | BASELINE | fixed | Three operations documents described controls and wiring that do not exist, in both directions |
| F-52 | P1 | BASELINE | fixed | A deployment could block an account's new risk and had no wired path to unblock it |
| F-53 | P2 | BASELINE | fixed | Tradability and the economics freeze keyed on different columns, so a market could trade while editable |
| F-54 | P3 | BASELINE | fixed | Every derivable count in the readiness documents had gone stale, including a restore drill reported at 111 migrations old |
| F-55 | P1 | BASELINE | fixed | Every runbook described a system that predates the build: 140 PENDING markers, and every package, binary and route they named exists |
| F-56 | P2 | BASELINE | fixed | Provider webhook evidence was mutable by the application role, including the payload hash and the signature flag |
| F-57 | P1 | BASELINE | fixed | Five subsystems' integration tests skipped silently in CI, and fifteen VERIFIED rows cited them as evidence |
| F-58 | P2 | BASELINE | fixed | Four documented SQLSTATEs were never raised and five raised families were never handled in Go |
| F-59 | P1 | BASELINE | fixed | The prediction resolver applied no maximum price age, so a dead feed scored every prediction FLAT |
| F-60 | P2 | BASELINE | fixed | Calibration dropped unresolvable predictions silently and reported a denominator that hid it |
| F-61 | P2 | BASELINE | fixed | `position_lots` had no database invariant tying status to quantity, and open quantity could be raised |
| F-62 | P3 | BASELINE | fixed | `normalized_events` replaced a column that varied between copies of the same event |
| F-63 | P2 | NEW | fixed | F-55's own correction pass replaced three true PENDING markers with false claims that things were wired |
| F-64 | P1 | NEW | fixed | An approval could be repointed at another target after both signatures, and the dual-control gate trusted a column instead of the code |
| F-65 | P2 | NEW | part | Two kill-switch kinds an operator would reach for in an incident reach nothing, and four documents said they work |
| F-66 | P1 | NEW | fixed | An authentication time in the future satisfied every step-up window for the life of the session |
| F-67 | P1 | NEW | fixed | The signing recovery path signed the bytes it was handed rather than the bytes that were approved |
| F-68 | P2 | NEW | fixed | The production withdrawal velocity policy bounds nothing, under a comment saying it bounds everything |
| F-69 | P2 | NEW | open | Inventory: what six independent audits found and what has not been fixed, with the reason for each |

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
would then claim fewer units than the ledger held. It could not construct the
fault it was named after, the test passed with it active, and the guard was
therefore proving nothing.

**CORRECTION (F-40).** This paragraph used to say the reason was migration
00711's SQLSTATE CR004, "which refuses a lot EVENT whose journal transaction
never touched the account, so that drift is unrepresentable". That is false, and
it is exactly the kind of false statement this register exists to catch. CR004
is raised only by `cp_credit_lot_open`, the trigger on `credit_lots` INSERT.
`cp_credit_lot_apply_event` never reads `NEW.journal_transaction_id`, and the
column is nullable on `credit_lot_events` — a CONSUME event with no posting
behind it IS accepted by the database. Why the original control did not
construct the fault is recorded again below, correctly; the sentence claiming a
database guarantee was written from the wrong trigger and stood for a session.

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

**And then the type system was made to catch the class.** `validatedList` took
`unknown`, which threw away what the generated client already knew: it types
`/native-assets` as `{ items: NativeAsset[] } | undefined` and `/accounts` as
`Account[] | undefined`. Calling `validatedList` on a paged response therefore
compiled cleanly and threw on every page load.

The parameter is now `readonly unknown[] | undefined`. Putting the committed
version back produces a compile error on the exact line:

    Argument of type '{ items: {...}[] } | undefined' is not assignable to
    parameter of type 'readonly unknown[] | undefined'

So the failure moves from a customer's screen, past the browser test that could
not see it, to `tsc`. A browser test that catches this is worth having; a
compiler that makes it unwritable is worth more.

**A note on how the other endpoints were checked.** I wrote a script comparing
every client parse against the OpenAPI response shape. Its first version
reported `/assets` as a mismatch — a false positive: `/assets` answers an inline
`type: array` and the script had looked up the ITEM schema and found `items:`
inside it. Corrected, it then skipped most paths for a different regex reason.
Two wrong answers in opposite directions from the same script is a good argument
for the type change above and against the script, which is not kept.

## F-33 · No browser test finished anything · BASELINE · P2 · FIXED

**Found by** asking the F-26 question of the interface: 69 browser tests, and
does any of them prove a customer can COMPLETE something?

None did. The suite covered rendering, navigation, accessibility and the honesty
rules — every one of them a statement about a page at rest. The closest thing to
a transaction was a heading being visible.

That is the same gap that produced F-26, F-28 and F-29 on the backend, one layer
up: a path the tests never walk looks finished from inside the tests.

**Fix.** `a customer can buy something and their Credits fall by exactly the
price`. It reads the Credit balance the customer is shown, buys the cheapest
product on the Marketplace, and asserts the balance fell by exactly the price
and the purchase appears in their own list. It ran against a live API, a real
Postgres and a MARKETPLACE gate activated through the real three-principal
ceremony, and it moved 300 Credits per run.

**It found three defects in itself before it found anything else,** which is
worth recording because each is a pattern:

1. **It read one product's price and clicked another's button.** `.panel`
   matched the outer container, so `.field` with "PRICE" resolved to the FIRST
   product while `.last()` clicked the LAST. Each product is its own
   `<article class="panel-nested" aria-label={title}>`; locating the CARD and
   then its own price and its own button is what makes the assertion about one
   thing.
2. **It asserted a Purchases table was visible.** That table was already there
   from an earlier order, so the assertion passed while nothing had been
   bought. It now asserts the card's own "Bought." notice.
3. **Its refusal branch never matched, and its outcome check ran too early.**
   The branch looked for the problem TITLE and the backend renders the CODE, so
   with the gate off the test fell through to the success assertion and failed
   with "element not found" — a confusing way to be told the gate is closed.
   And `networkidle` returned before the mutation had re-rendered, so the branch
   read an empty card. It now waits for the card to show EITHER a success notice
   or an explanation before deciding which happened.

**Both branches were run against a real deployment.** With MARKETPLACE ACTIVE
the purchase completes and the balance falls. With the capability removed from
`CP_API_ENABLED_CAPABILITIES` the test takes the refusal branch, asserts the
refusal names `CAPABILITY_NOT_APPROVED` and `MARKETPLACE`, and checks the
balance did NOT move. Neither outcome is a green tick over an untested path.

**And it would have failed in CI.** The e2e job seeded users and assets but not
the internal economy, so `/marketplace` had nothing to buy. The new spec would
have found an empty catalogue on its first CI run — a test that needs data
nobody creates is another path only its author can walk. The job now runs
`seedeconomy` and activates MARKETPLACE through `gateceremony`, the same real
ceremony used locally, and starts the API with the development legal policy and
the capability enabled.

Point 3 is the third time in this session that a check ran before the thing it
was checking existed — F-32's first fix, this refusal branch, and this outcome
wait. The pattern is worth naming: **an assertion about an absence, or a branch
on a condition, needs a positive signal before it, or it is only measuring how
fast the test runs.**

## F-34 · The risk kernel was complete, tested, and unreachable · BASELINE · P1 · FIXED

**Found by** following the readiness report's own admission — *"The risk kernel
is not yet an input to the Domain A route. `Route.RequiresRiskEvaluation` is
determined and recorded; nothing consumes it for internal trades yet"* — and
asking the F-26 question of `internal/risk` rather than only of Domain A.

The answer was worse than the report said. It is not that one route fails to
consult the kernel. **Nothing does.** `risk.Store.RecordPolicy` and
`risk.Store.RecordDecision` had no caller outside a test; no migration, script
or endpoint ever wrote a `risk_policies` row; so `risk_policies` was empty in
every deployment, `EffectivePolicy` answered `ErrNoPolicy` everywhere, and
`risk_decisions` had never received a row. Nine hundred lines of limits, kill
switches, hash verification and fail-closed reasoning, none of it reachable.

The kernel's own documentation described the missing step:
`DefaultGlobalPolicyJSON` is commented as *"the compiled-in GLOBAL policy used
only to seed a fresh deployment"*, for a seeding step that did not exist.

That is F-26, F-28 and F-29 a fourth time, and it is the largest instance:
**a path only tests can walk looks finished from inside the tests.**

**Two of the limits did not exist either.** PART XXXII names what the kernel
must constrain "at minimum", and two entries are about this economy
specifically: native-market concentration and creator concentration. Neither was
in `Policy`. They cannot be expressed the way every other limit is, because
every other limit is denominated in USD and a Credit has no approved external
value (PART LIV) — so a USD limit on a native position would need an exchange
rate nobody set. Both are ratios instead, compared as `part * 10000 > whole *
limit` with no division and no float.

**Choosing the denominators was the hard part, and the first two choices were
both broken.** The obvious denominator for market concentration is the FLOAT,
which is also what `surveil` alerts on. It is unusable as a limit: the first
buyer in a new market holds all of the float, so a limit against it refuses the
opening trade of every market that will ever exist. Creator concentration
measured as one creator's share of the account's native SPEND has the identical
defect one level down — an account's first native purchase is necessarily 100%
of its native spend. Both were fixed by changing the denominator to something
that is not degenerate at zero history: total supply, which is fixed at mint and
which no participant can move, and the account's whole Credit position, spent
plus still spendable.

**A refusal that costs nothing to write is the wrong kind of refusal**, so both
denominators were chosen against the real curve rather than in the abstract.
Under the compiled-in defaults (2000 / 3000 bps) five of this package's
fifty-two integration tests began failing, and every one of them was a test
buying a genuinely concentrated position: a 10,000-Credit order that takes 22.5%
of an asset's entire supply, a hundred buyers each putting half their Credits
into one creator. The tests were changed to buy positions inside the limits, and
each carries a comment saying why — the alternative, a fixture policy that
relaxes the limits it is meant to prove, is the test equivalent of turning the
control off.

**Fix, in four parts.**

1. **The limits.** `max_native_market_concentration_bps` and
   `max_creator_concentration_bps` in `Policy`, in `Validate`, in
   `MissingLimits`, in `Compose` (so an ACCOUNT row can only tighten), and in
   `DefaultGlobalPolicyJSON`. `EvaluateNativeTrade` is a second entry point
   beside `Evaluate` that evaluates exactly these two and refuses
   `RISK_POLICY_MISSING` if either is absent, naming the absent ones.

2. **The caller.** `nativemarket.Execute` builds a snapshot — counts and
   Credits, never a price — evaluates it after pricing the fill and BEFORE
   posting anything, and refuses with `RISK_CONCENTRATION`. A SELL is never
   refused: both limits constrain holding too much, and refusing an exit would
   trap a holder in the position the limit exists to discourage.

3. **The seeding step.** `scripts/riskpolicy` records a GLOBAL policy through
   the real `Store.RecordPolicy`. With `-rules` it records an operator's
   document in any environment; with no `-rules` it records the compiled-in
   default and REFUSES outside LOCAL/DEV/TEST, because seeding production with
   starter limits nobody signed off would produce exactly what this audit is
   against: a control that looks decided and is not. `make seed-economy` and the
   CI e2e job run it.

4. **The evidence.** Every decision is persisted. An ALLOW is written inside the
   trade's own transaction, so a decision cannot commit without the trade it
   permitted. A REJECT cannot be — the transaction it refuses rolls back and
   would take the record with it — so the service hands the decision out with
   the error and `nativeMarketsAdapter` writes it in a new transaction once the
   failed one is finished. Not inside it: acquiring a second pool connection
   while holding the first is precisely F-27.

**A wrong answer the control introduced, caught before it shipped.** An order
larger than the account's Credits makes the creator-concentration denominator
smaller than its numerator, so the ratio exceeds every limit and the kernel
answered `RISK_CONCENTRATION` — for what is really "you do not have that many
Credits". The trade was correctly refused either way, which is exactly why
nobody would have noticed: a refusal that is right about the outcome and wrong
about the reason sends somebody to read a concentration policy. The limits are
now not evaluated for a trade the account cannot pay for; the ledger's own
negative-balance guard produces that refusal, and
`TestIntegration_AnOrderBiggerThanTheBalanceSaysSo` was observed reporting
`RISK_CONCENTRATION` with the guard removed.

**What the refusal says.** Both limits have an obvious remedy — a smaller order
— and a customer shown `RISK_NATIVE_MARKET_CONCENTRATION` learns that something
was refused and nothing about what to do, which reads as a fault rather than a
limit. The detail names the limit in words and says a smaller order may be
within it; the reason codes stay on the problem for anything machine-read. The
one case that does NOT say "try a smaller order" is a refusal caused by the
deployment rather than the order — a policy missing its limits — because that
would send somebody to fix something that is not theirs.

A deployment with no risk policy at all answers `UNSUPPORTED` with "this
deployment has recorded no risk policy, so no internal trade can be evaluated",
not an opaque `INTERNAL`. It is true, and the fix is `go run
./scripts/riskpolicy` rather than a bug report.

**Observed failing before being believed.** All five new
`internal/nativemarket` integration tests fail with the `checkRisk` call
removed, `TestIntegration_ARefusedTradeStillRecordsItsRiskDecision` fails with
`no rows in result set` when the adapter's `recordRiskRefusal` call is removed,
and the overspend test fails with the affordability guard removed.

**What this did NOT fix.** `internal/risk` is still unreachable for everything
except native trades: no Domain B or C route consults it, and `RecordPolicy` has
no administrative endpoint — an operator changes limits by running a script
against the database rather than by two operators through the admin plane, which
is what PART 58's risk-desk sign-off deserves.

`internal/eligibility` is in the identical position, and for the same reason:
`Store.RecordPolicy` and `Store.RecordDecision` have no caller outside their own
tests, because the settlement PLANNER that would consume an
`eligibility.Decision` is part of the Domain B and C execution path this
deployment does not run. That is checkable rather than asserted — nothing
outside a `_test.go` file and `settlementtest` constructs a `settlement.Input`
at all, so the struct field that holds the decision has never been filled by a
deployment. That is recorded here rather than raised as a separate
finding, because it is the same fact the readiness levels already state — but it
should be read as "the eligibility kernel has never run either", not as
"eligibility is covered".

`internal/risk` is now in `test/reachability`'s package list, so the answer
cannot quietly go back to no.


## F-35 · `make lint` had never passed · BASELINE · P2 · FIXED

**Found by** running it, after F-34's work touched enough packages to make it
worth checking. It is the third target in this session found red on arrival, and
the same argument applies as to `make sast`: **a check that can only fail is a
check nobody runs.**

Three separate causes, and only the third was a real defect count:

**1. `fmt-check` was red on fifteen committed files.** Not files this session
wrote — `internal/agentauthority`, `internal/commerce`, `internal/credit`,
`internal/payout`, `scripts/seedeconomy` and others had been committed
unformatted. `gofumpt -w` on all fifteen; the diff is whitespace and struct
literal bracing.

**2. `misspell` was arbitrating dialect, and losing either way.** Configured
`locale: US`, it reported 56 findings across 27 files — `cancelling`,
`behaviour`, `catalogue`, `serialised`, `honour`, and `CHEQUE`, which is a
payout instrument kind whose spelling IS the name of the thing. Switching to
`locale: UK` produced 692, because the repository genuinely mixes: `sanitizes`,
`analyze` and `authorization` sit beside the British spellings.

Neither locale can pass without rewriting correct prose, so the locale is now
unset. misspell still catches actual typos, which is what it is for, and stops
adjudicating a house style nobody wrote down. Imposing one dialect across 27
files is a real change worth making on its own; making it as a side effect of a
linter setting, in the same commit as a risk kernel, is not.

**3. Fourteen genuine findings, all fixed.** The ones worth naming:

- **Six unchecked type assertions in the commerce integration test.**
  `created.raw()["product_id"].(string)` panics when the field is absent, which
  is exactly what a handler that started returning a Problem instead of a
  product looks like — so the failure mode was a panic inside a helper rather
  than an assertion naming the field. Replaced with a helper that requires the
  field and says which one is missing.
- **Three doc comments orphaned by insertions.** `GetBySpotPair`,
  `AllRequiredCapabilities` and `StoredQuote` each carried the FIRST LINE of
  another symbol's comment, because a new function had been inserted between a
  comment and the thing it documented. In each case the original symbol was left
  undocumented and the new one was introduced by a sentence about something
  else. Moved back.
- **A bidi control character in `nativeasset_test.go`.** The same Trojan Source
  issue `make sast` found in `moderation.go` earlier this session, in the test
  that proves bidi names are refused — and invisible to gosec, which builds only
  the default tag set. Written as `\u202e` now, like the source it tests.

**Trojan Source made mechanical.** That was the third occurrence in this
project, each found by a different tool, and each in a file the other tools do
not read: gosec builds only the default tag set so it never sees a file behind
`//go:build integration`; staticcheck reads Go and nothing else; neither reads
Markdown, and a sweep found the character in TWO readiness documents — including
the paragraph describing its removal from the code.

`test/source` reads every text file in the repository, whatever the extension
and whatever build tag guards it, and refuses the nine bidi embedding, override
and isolate codepoints. U+200E and U+200F are deliberately not among them: they
are ordinary characters in multilingual text and do not reorder a line. There is
no allowlist, because the one file that legitimately needs these codepoints
already carries them as escapes, which is strictly better than the literal. It
was observed failing on a planted override, and it names the file, the line and
the codepoint — the only three things that help, given the character itself
cannot be seen.
- **A negated parenthesised pair in the isolation check.**
  `if !((declared.From == a && declared.To == b) || ...)` — correct, and exactly
  the shape a reviewer misreads. F-03 was a direction-blind isolation check, so
  the condition is now named (`declaresThisPair`) rather than read through a
  `!`.

**And the two formatters disagreed.** `scripts/fmtcheck` ran plain `gofumpt -l`
while `.golangci.yml` sets `gofumpt.extra.group-params: true`, so `make fmt`
could produce a file `make lint` rejects — which is how two of the fourteen were
found. fmtcheck now passes `-extra`, and the 25 files that difference had been
hiding are formatted.

`make lint` exits 0.

## F-36 · A read permission authorized writes, and ADMIN holds the customer surface · BASELINE · P1 · FIXED

**Found by** asking an adversarial re-audit of the HTTP authorization surface
one question: for every handler that takes an id from the request, is ownership
actually checked? It is — by `security.RequireAccount`, which was the only
tenant check on the write routes as well as the read ones.

`RequireAccount` returns nil for any non-agent principal holding
`account:read_any`. That is correct for a read and is what the permission is
named for. The write routes used the same function.

**Why that is not merely untidy.** `RoleAdmin` is defined as
`except(allPermissions, dualControl ∪ agentOnly)` — every permission that is not
an approve-side or agent-intrinsic one. So an ADMIN holds `account:read_any`
AND `native_market:trade`, `commerce:buy`, `commerce:sell`, `payout:create`,
`withdrawal:create`, `funding:create` and `credit:purchase`. Those customer
permissions are there so an operator can use their own account. Combined with
the read override, one ADMIN session could do this:

    POST /v1/native-markets/{marketId}/orders
    {"account_id": "<any customer>", "side": "BUY", "amount": "1000000000", ...}

and spend that customer's Credits. Likewise `POST /v1/internal-products/{id}/orders`
(buys with their Credits), `POST /v1/payouts` (moves their Credits into
`PAYOUT_RESERVED`), `POST /v1/internal-sellers` (registers them as a seller).
No second signature, no step-up, no `admin_actions` row, no reason recorded —
the audit stream shows an operator making a customer's request, which is
precisely what it would look like if the customer had made it.

**Fourteen write routes were scoped this way.** Every account-scoped
`POST`/`PUT`/`PATCH`/`DELETE` in the API.

**Fix.** `security.RequireAccountOwner` — ownership only, no operator override —
and `accountScopeWrite` in the HTTP layer, on all fourteen. An operator who
needs to change a customer's position uses the admin plane, where the act needs
a reason, a second principal for anything consequential, and a permanent row
naming both. Reads are untouched: `account:read_any` still reads any account,
which is what it is for.

**Made mechanical.** `TestAccountScope_EveryWriteUsesOwnershipOnly` reads this
package's own source and fails on any mutating handler that scopes through the
read helper, naming it. Its negative control requires the write helper to be in
real use, so a green run cannot mean "the regexes stopped matching". Both were
observed failing.

**And proved over HTTP.**
`TestIntegration_AnOperatorCannotTradeOutOfACustomersAccount` puts a real ADMIN
principal on a real market with a real funded customer, asserts 403, asserts the
balance did not move — and then asserts the same operator can still READ that
account and still trade out of their OWN. A fix that broke either of those would
have passed a test that only checked the refusal.

**No test could have caught it.** Every principal in
`TestIntegration_CommerceRefusesCrossTenantRequests` is a customer; the
`TestIDOR_*` family uses customers and SUPPORT_READ_ONLY. Nothing anywhere put
an operator principal on a customer write route with a foreign `account_id` —
the combination was not modelled, so the suite agreed with the code.

## F-37 · The customer trade route had never worked, in any deployment · BASELINE · P1 · FIXED

**Found by** F-36's test failing for the wrong reason. Proving an operator is
refused needed a real request through the real port, and the first honest run of
that request — by the account's own owner — returned

    400 VALIDATION_FAILED: an order needs effective_at

`nativemarket.ExecuteRequest` requires `EffectiveAt`. The handler does not set
it, because a handler must not invent time. The adapter does not set it either
— and the payout adapter and the commerce adapter both do, from the deployment
clock, three files away. So **every** request to `POST
/v1/native-markets/{marketId}/orders` had been refused since the route was
written.

**Why nothing noticed, which is the whole lesson.** The path was covered
everywhere except where it mattered: `internal/nativemarket`'s integration tests
call `Execute` directly with an `EffectiveAt`; the chaos suite and the restore
drill do the same; the browser suite buys from the Marketplace, which goes
through `commerceAdapter`; and `test/load/internal_economy.js` says in its own
comment that it deliberately does not create native-market trades. Fifty-two
integration tests, seventy browser tests, a load script and a chaos suite, and
not one of them sent this request.

That is F-26, F-28, F-29 and F-34 again — **a path only tests can walk looks
finished from inside the tests** — with a new wrinkle: here the tests walked a
path *beside* the real one. The domain service was exercised exhaustively; the
route into it was not exercised at all.

**Fix.** `nativeMarketsAdapter.Execute` stamps `EffectiveAt` from
`deps.now()`, like its siblings. `TestIntegration_ACustomerCanTradeOverHTTP`
walks the route as a customer and asserts the Credits that left the account are
the Credits the fill reports; it was observed failing with the stamp removed.

## F-38 · A policy DENY carrying the gate's reason code became a permission · BASELINE · P1 · FIXED

**Found by** an adversarial re-audit of the settlement compiler, asking which
inputs produce a permissive `Route`.

F-17 established a real distinction: a DENY the router produced only because a
capability gate is off is not a policy refusal, and reporting it as
`LEGAL_ROUTER_DENIED` sends an operator to change a policy that is already
correct. The compiler drew that distinction by comparing the decision's
`ReasonCode` against the literal `"CAPABILITY_NOT_ACTIVE"`.

A reason code is a string a policy author writes. `Policy.Validate` required one
to be non-empty and nothing more — there was no reason-code vocabulary. So a
hand-authored rule:

    {Match: {Jurisdiction: "US-NY"}, Outcome: Deny, ReasonCode: "CAPABILITY_NOT_ACTIVE"}

validated, matched, and was then read by the compiler as a gate refusal. With no
`RequiredCapability` on the rule there was nothing for the gate check to catch
either, so the route came out with **no reasons at all** and `Permitted: true`.
The deployment's own policy said no and the compiler said yes.

**Two fixes, because one of them is the control and the other is the reason
nobody can reach it.**

1. `legalrouter.Decision.GateRefusal` — a field set at exactly one place, where
   the router converts an ALLOW whose gate is off into a DENY. The compiler asks
   that instead of the string. It cannot be forged, because a `Rule` has no such
   field.
2. `ReasonCapabilityNotActive` is reserved: `Policy.Validate` refuses a rule
   that writes it by hand, naming the rule. Without this, a policy could still
   make every refusal message lie about which of the policy and the gate
   refused.

A belt-and-braces line was added with them: a router `Deny` that produced no
reason at all now adds `LEGAL_ROUTER_DENIED` unconditionally. It cannot fire
today — "cannot happen" is what the string comparison assumed too, and the cost
of being wrong is a permissive route.

**Observed failing.** `TestRoute_GateRefusalIsSetOnlyWhereTheGateRefused` fails
with the field assignment removed, and `TestPolicy_TheGatesReasonCodeIsReserved`
fails with the reservation removed. The compiler-level test is labelled in its
own comment as a regression guard rather than a proof, because the exploit can
no longer be constructed through `legalrouter.New` and it therefore passes
against the old code as well.

## F-39 · The no-float linter never looked at the internal economy · BASELINE · P2 · FIXED

**Found by** an adversarial re-audit of the money spine sweeping for floats, and
then asking what stops one being added.

`scripts/lintfin` enforces "no float in a money path" over a hard-coded
directory list: `money, ledger, capital, risk, positions, valuation, quote,
settlement`. Every package of the internal economy is absent —
`internal/credit`, `internal/nativemarket`, `internal/commerce`,
`internal/payout`, `internal/valuedomain`. Domain A was built after that list
was written and nobody extended it.

There is no float in any of them today; the sweep confirmed it. What did not
exist was the control, and "there is no float" and "a float cannot be added
without the build failing" are different claims — the second is the one the goal
document asks for.

**Fix.** The five packages are in the list. The linter was run against them and
is clean, and a deliberately-planted `float64` in a Credit calculation was
observed failing it.

## F-41 · Three by-id reads answered more than they should · BASELINE · P2 · FIXED

**Found by** an adversarial re-audit of the HTTP authorization surface, asking
of every handler that takes an id: does it verify ownership, and is a refusal
for somebody else's record distinguishable from a refusal for one that does not
exist?

Three routes fetched by id and decided afterwards, and each leaked in the gap:

- **`GET /v1/payouts/{payoutId}`** — 403 for a payout belonging to somebody
  else, 404 for one that does not exist. That difference is a membership
  oracle: anybody could enumerate which payout ids are real. Its own sibling,
  `POST /v1/payouts/{payoutId}/cancel`, has answered NOT_FOUND to both since
  F-29 and says why in a comment two files away. The same resource, two answers.
- **`GET /v1/internal-products/{productId}`** — no ownership check and no status
  filter, while `ListActive` beside it shows only what is buyable. A DRAFT
  product of another seller, with its price and its fee split, was readable by
  anyone holding `commerce:read`, which is every customer.
- **`GET /v1/native-assets/{assetId}`** — the same shape, and the response
  carries `moderation_state` and `moderation_notes`. A stranger could read
  somebody's unpublished asset AND the moderator's private commentary on it.

All three answer NOT_FOUND now. Owners and creators still read their own
unpublished work; a published product and a live asset are still public,
because a market nobody can look at is not a market.

**The tests needed the ports first.** The Domain A harness wired only the admin
plane, so a request to any customer route answered `UNSUPPORTED` — which reads
as a refusal and is not one. A test asserting "a stranger is refused" would have
passed against a deployment where the route did not exist. That is F-37's lesson
arriving twice in one session: the harness now wires the native-asset and payout
ports, and each of the three tests was observed failing with its fix removed.

## F-42 · The audit binding trusts a session variable any caller can set · BASELINE · P2 · OPEN

**Found by** an adversarial read of the migration surface.

Migration 00603 binds every state change of an audited entity to a transition
row written in the same transaction: an AFTER INSERT trigger on the transitions
table sets a transaction-local setting, and a DEFERRED constraint trigger on the
entity table raises `AU001` at COMMIT unless that setting matches the new state.
Seventeen tables depend on it.

Its header says: *"The application role cannot bypass this: it has no privilege
to drop triggers, and the flag can only be set by inserting an immutable
transition row."*

**The second clause is false.** `cp.transition.*` is a custom GUC, and
PostgreSQL lets any role set one. This repository proves the role can:
`internal/reconciliation/store.go:366` executes `SELECT set_config($1, '1',
true)` on a `cp.`-prefixed key as `cp_app`. So a caller that can execute SQL as
`cp_app` can satisfy `AU001` without writing a transition row, and change state
on any of the sixteen tables where `cp_app` holds unrestricted `UPDATE`.

`capability_gates` is the exception and shows the shape of a real fix: 00701
does `REVOKE UPDATE ON capability_gates FROM cp_app` and grants back only
`UPDATE (version)`, so a bare state update is impossible whatever the flag says.

**Why it is OPEN rather than fixed.** The obvious repair — have the deferred
trigger check that a transition row exists in the current transaction, instead
of trusting a setting — needs a way to say "in this transaction" that survives
savepoints. `xmin = pg_current_xact_id()::text::xid` was tried and rejected:
verified against this project's own PostgreSQL 16, a row inserted inside a
`SAVEPOINT` does NOT match the top-level xid, and the admin executor runs every
action inside one. A `created_at >= transaction_timestamp()` predicate was
rejected too — the test suites run on fake clocks, so a legitimately-written
transition row is routinely dated before the transaction that writes it.

What remains is the `capability_gates` treatment applied to the other sixteen
tables: revoke UPDATE on the state column and route state changes through
SECURITY DEFINER functions. That is a real piece of work and it is not being
started at the end of a session.

**Recorded now, with the exploit and the remedy, because the alternative is a
migration header that claims a guarantee the database does not make** — which
is exactly F-40, one file over.

### Update 2026-09-08: two more candidate fixes tried and rejected, with the evidence

Both are recorded so the next attempt does not repeat them.

**`pg_xact_status` does what `xmin = pg_current_xact_id()` could not.** Probed
against this project's own PostgreSQL 16, one row inserted at the top level of a
transaction and one inside a `SAVEPOINT`:

| row | `xmin` | `pg_current_xact_id()` | `xmin = pg_current_xact_id()::text::xid` | `pg_xact_status(xmin::text::xid8)` |
| --- | --- | --- | --- | --- |
| top level | 1911303 | 1911303 | **true** | in progress |
| in a savepoint | 1911304 | 1911303 | **false** | in progress |

So the earlier rejection was right about `xmin =` and the reason was the
subtransaction xid, exactly as recorded. `pg_xact_status` answers the question
correctly for both, and a row that is visible to us with an in-progress writer
can only be our own — no other transaction's uncommitted rows are visible at
any isolation level this system uses.

**It is still not safe to use.** `pg_xact_status` takes an `xid8`, and `xmin` is
a 32-bit `xid`. Casting it through text produces a full transaction id with no
epoch, which is correct until the counter wraps and then wrong for every row at
once. A control that works for years and then refuses every state change in the
system is worse than the forgery it prevents — the forgery needs the
application's database credential; the wraparound needs only time.

**Making the flag carry the transition row's id was also considered.** The
deferred trigger would look the row up rather than trusting a bare setting, so a
caller who sets the GUC without inserting a row fails. It does not close the
finding on its own: nothing stops a caller naming an OLD transition row with the
right `to_state`, replaying a state change that was once legitimate. Requiring
the row to be the entity's *latest* transition would close that — the latest
transition's `to_state` is by construction the current state, so a row naming a
different target cannot already be latest — but "latest" needs an ordering,
and the test suites run on fake clocks that make `occurred_at` non-monotonic.
That is the same rock the `created_at >= transaction_timestamp()` attempt hit.

**So the remedy is unchanged**: the `capability_gates` treatment — revoke
`UPDATE` on the state column, route state changes through SECURITY DEFINER
functions — applied to the other sixteen tables. It is privilege rather than
detection, which is why it works. It remains open because it is a change to the
mechanism seventeen state machines depend on, and the right way to do it is
deliberately, one table at a time with its own tests, rather than as a
sixteen-table migration written in one pass.

## F-43 · MARKETPLACE was high-risk in Go and not in SQL · BASELINE · P1 · FIXED

**Found by** an audit of the documents against the code, which found
`docs/compliance-gates/PRODUCTION_GATES.md` asserting the opposite of
`gates.IsHighRisk` — and asserting it as a *correction* to an earlier table,
which is the form a reader trusts most.

The document was wrong: F-16 had already moved `MARKETPLACE` to high risk in Go,
because it gates the minting of the only withdrawable creator-earning
provenance, so leaving it low risk would let one approver switch that on.

**What was true is worse than what the document claimed.** The database did not
agree with Go. `cp_gate_is_high_risk` listed seventeen capabilities;
`gates.IsHighRisk` returns true for eighteen; the one they disagreed about was
`MARKETPLACE`. Migration 00701 describes the SQL check as *"the line that holds
when the Go check is bypassed"* — `GT003` refuses to approve a high-risk gate
without four evidence references and three distinct principals. For MARKETPLACE
that line was absent.

Nothing was exploitable through the API, because `gates.Admin` asks Go first.
What was missing is precisely the thing the second copy exists for: what happens
when the API is not the caller.

**Fix.** Migration 00716 adds `MARKETPLACE` to the SQL list, and
`TestIntegration_GoAndSQLAgreeOnEveryCapabilitysRisk` drives
`cp_gate_is_high_risk` for every capability `AllCapabilities()` declares and
compares each answer with `IsHighRisk`. It was observed failing without the
migration, reporting `MARKETPLACE: Go says yes, SQL says no`. A second test
asserts every declared capability is nameable by the gate table's CHECK
constraint, because a membership test against a literal list treats an unknown
capability and a low-risk one identically.

The register already listed "the chart of accounts is duplicated between Go and
SQL" as a known, deliberately-unraised maintainability risk. This is that risk
arriving: a list duplicated in two languages diverged, and the half that
diverged was a security control.

## F-44 · Every native-market price was shown at the wrong scale · BASELINE · P1 · FIXED

**Found by** an audit of the browser interface against this project's own
honesty rules: a screen must never show a number the API did not return.

`NativeMarket.spot_price` and `NativeQuote.effective_price` are `Quantity` —
*"exact asset base units as an integer string"* — and both responses carry a
sibling `price_scale`. The web app declares `price_scale` in its own contract
and no component reads it. Both fields were rendered with `DecimalValue`, which
inserts thousands separators and nothing else.

`nativemarket.PriceScale` is **18**. So a price of a few thousandths of a Credit
was displayed as a sixteen-digit number of Credits, on the field labelled
"Price" and marked `emphasis`, and on the "Effective price" of a live quote —
the number a buyer commits against.

This is a worse failure than the class the honesty rules were written for. Those
forbid showing a zero for a figure the backend did not return. This showed a
figure the backend *did* return, in a unit the backend never used, with no
indication anything was wrong.

**Fix.** Both fields render with `Qty` at the scale the response carries. The
constant `CREDIT_DECIMALS = 6` now says in a comment that prices are not on that
scale and that the scale is a fact about the asset which the API states.

**Closed 2026-09-08.** See the update at the end of this entry.

**Was not fixed here, and recorded:** the same page scaled `circulating_supply`,
`asset_reserve`, the holder table and a quote's `expected_output` — all ASSET
units — by the same hardcoded Credit scale. They are accidentally right today
because the asset-creation screen also hardcodes six decimals and sends it, so
every asset made through the interface has six. An asset created through the API
with any other value renders wrong by a power of ten, and nothing would say so.

**Update, 2026-09-08 — the recorded remainder is closed.**

The asset quantities were left because they were accidentally right: the
creation screen also sends six decimals, so `CREDIT_DECIMALS` matched. They were
wrong for any creator who asked for anything else, by a factor of ten to the
difference, on exactly the supply and concentration figures a buyer judges a
market by.

The API could not have been used correctly, because it never stated the scale:
neither `NativeMarket` nor `NativeAsset` carried it, though
`CreateNativeAssetRequest` accepts it. So the fix is a fact, not a constant:
`asset_decimals` on `NativeMarket`, `NativeQuote` and `NativeFill`, read from
the asset registry in the same transaction the quote or fill was computed in.
`MarketView` gained the field and the port gained `NativeQuoteView` and
`NativeExecuteView` — the domain types stay free of a registry fact.

On the page, `circulating_supply`, `asset_reserve` and every holder's balance
now render at `asset_decimals`, and a BUY quote's `expected_output` does while a
SELL's stays in Credits, because that is which side the asset is on.
`CREDIT_DECIMALS` keeps its name and its comment now says the one thing it may
be used for.

**Evidence.** `TestIntegration_TheMarketResponseStatesItsAssetsScale` and
`TestIntegration_TheQuoteStatesTheScaleOfItsAssetSide` launch a market whose
asset has **nine** decimals — chosen so that a hardcoded six cannot pass —
and assert the response states nine, and that the asset scale and the price
scale are different numbers. The fixture checks the registry rather than what it
asked for.

## F-45 · The browser suite's own unit tests had been failing · NEW · P2 · FIXED

**Found by** running them.

`node --test src/lib/*.test.ts` is a source scan that enforces the money rules
on the app's own source, and it had been red since the purchase spec was written
— on that spec. Two lines converted a displayed Credit figure to a JavaScript
number, and the test's central assertion subtracted two of them and compared the
result to a third. IEEE-754 arithmetic on money, as the sole proof that money
moved, inside the test that exists to prove money moved.

The scan was right and the test I wrote was wrong. It parses exact base units
into `BigInt` now, and the `?? "0"` fallbacks are gone: a figure the page did not
show must fail the test, not become zero.

Fourth target found red on arrival in this session, after `make sast`, the
backup drill's Domain A coverage and `make lint`. The pattern is worth stating
once more: **a check that fails is a check nobody runs**, and every one of these
had been failing long enough that its failure had become the normal output.

## F-46 · The nine-page browser check asserted absences before the page loaded · BASELINE · P2 · FIXED

**Found by** the same audit, comparing the two page loops in
`critical-paths.spec.ts`.

F-32 established the rule: `toHaveCount(0)` passes the instant it is evaluated,
so asserting the absence of an error before the queries resolve proves nothing.
The fix was applied to the five internal-economy pages — `networkidle`, then no
`.loading`, then no "This response could not be trusted" — and the nine required
pages beside them kept the original shape: heading visible, one `h1`, no
`.malformed`, all evaluated immediately.

Home, Trade, Portfolio, Settings, Lab, Agents and Activity therefore passed with
every panel on them showing an error. All nine now settle first and assert the
same three things the other five do.

## F-47 · Two deliberate statements about who may read encrypted PII contradict each other · BASELINE · P2 · OPEN

**Found by** an adversarial read of the database as a security boundary,
enumerating what each role can actually do rather than what each migration says
it granted. **Then partly un-found by this project's own test suite, which is
the part worth recording.**

The observation is real. `cp_readonly` and `cp_ops` can SELECT `identity_pii`
(`email_encrypted`, `legal_name_encrypted`, `dob_encrypted`, `key_version`) and
`sessions` (`token_hash`, `roles`, `break_glass_until`, `ip`). Checked against a
live database: `has_table_privilege('cp_readonly','identity_pii','SELECT')` is
true.

They can because the role bootstrap says:

    ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public
        GRANT SELECT ON TABLES TO cp_readonly, cp_ops;

while migration 00010 grants those two roles SELECT on `users`, `accounts`,
`compliance_profiles`, the transitions and `security_events` — and deliberately
NOT on `identity_pii` or `sessions`. The migration's list has no effect while
that default exists.

**I revoked both, and the suite refused it.**
`test/integration/migrations/privileges_test.go` states the opposite contract in
a doc comment — *"cp_readonly and cp_ops can SELECT everything and write
nothing"* — and asserts it for every table in the schema. It failed. That is the
suite doing exactly its job, against a change made on an auditor's framing
without checking whether another part of the repository stated a different
contract.

And the revoke would have broken something real: the same file lists `sessions`
under `opsHousekeeping`, where *"cp_ops performs retention cleanup"*. A
`DELETE … WHERE expires_at < now()` needs SELECT on the columns it filters on,
so removing SELECT would have stopped session retention.

**So the finding is not "a privilege leak". It is that two deliberate statements
in this repository contradict each other, and the contradiction is invisible
because one of them silently wins.** Migration 00010's grant list was written by
somebody who meant those two tables to be withheld. The privileges test was
written by somebody who meant every table to be readable. Both are still in the
tree; only one has any effect.

**Why this stays OPEN rather than being resolved here.** Which statement is
right is a question about who may read encrypted personal data and session
material in this deployment. It has an operational constraint attached
(`cp_ops` needs `sessions`), it likely has a different answer for `cp_readonly`
than for `cp_ops`, and it is not a decision to take unilaterally inside a
migration at the end of a session. What is recorded here is the contradiction,
the evidence for both sides, and the constraint any resolution has to satisfy.

**What was left in place.** Nothing about privileges changed. The bootstrap
files carry a note saying the default is deliberate, and pointing at the
contradiction rather than resolving it.

### Update 2026-09-08: the table is empty, and there is now a fuse on it

Two facts that size the finding, both checkable rather than argued.

**Nothing reads or writes `identity_pii`.** `email_encrypted`,
`legal_name_encrypted` and `dob_encrypted` appear in exactly one place in the
repository outside documentation: the `CREATE TABLE` in migration 00010. No Go
file mentions any of them. So in every deployment that exists, the table the
contradiction is about holds no rows.

That is not a new discovery so much as a confirmation of what two other
documents already say honestly. `SECURITY.md` §167: *"Encryption of
`identity_pii` at the application layer: DESIGNED."* R-121-1 in the traceability
matrix, IN_PROGRESS: *"the column encryption is schema-shaped but has no Go
implementation, the identity_pii grant review has not been done, and there is no
test asserting the boundary."*

So an unresolved grant on an empty table costs nothing today, and the same grant
on a populated one is a standing exposure of every customer's name and date of
birth to two roles that were meant not to have it. The finding is not urgent; it
becomes urgent at an identifiable moment.

**That moment now fails a test.**
`TestPII_NothingWritesPersonalDataWhileTheGrantIsUnresolved` scans `internal`,
`cmd` and `scripts` for a statement that writes `identity_pii`, and fails when
one appears, saying what has to be decided before it lands. Observed failing on
a planted writer, which it named by file. Its companion is the positive signal:
the same scan must find writers of `journal_transactions`, which certainly has
them, and the table must still be created by 00010 — so neither a broken
regex nor a rename can make the absence vacuous (F-32, F-46).

`sessions` is deliberately outside that check. `cp_ops` genuinely needs it for
retention cleanup, that need is recorded in `privileges_test.go`'s
`opsHousekeeping`, and it is written constantly. Its half of F-47 is a question
about column-level grants — `token_hash` and `break_glass_until` are not
`expires_at` — not about whether the table should have data.

**Still OPEN, and still for the same reason.** This is a fuse on a decision, not
the decision. Who may read encrypted personal data in a deployment is a policy
question with an operational constraint attached, and it is not one to settle
inside a migration.

## F-48 · Five SECURITY DEFINER functions did not pin `pg_temp` · BASELINE · P2 · FIXED

**Found by** the same audit, checking every SECURITY DEFINER body's
`search_path`.

Migration 00701 names this hazard precisely and pins `pg_catalog, public,
pg_temp` for `cp_gate_transition`. Five older functions were written with
`SET search_path = public` and never updated: `ledger_apply_entry`,
`cp_credit_lot_open`, `cp_credit_lot_apply_event`, `cp_native_market_open` and
`cp_native_market_apply_fill` — the triggers that enforce LG001, the credit-lot
invariants and the market's constant product.

`TEMP` on a database is granted to `PUBLIC` by default and is revoked nowhere in
this tree, so any caller may create temporary tables; with `pg_temp` unpinned it
is searched first. A temp relation shadowing `ledger_accounts` would be what
`ledger_apply_entry` reads, as the definer.

Not demonstrated as exploitable — the mechanism is confirmed, the exploit was
not built. Recorded at P2 and fixed anyway, because `ALTER FUNCTION … SET
search_path` changes nothing but the setting and the argument for pinning it was
already written down in this repository, one migration over.

## F-49 · An account chose its own exemption from the negative-balance guard · BASELINE · P2 · FIXED

**Found by** the same audit, asking what LG001 actually reads.

LG001 is the guard that stops a balance going below zero, and it asks the
account row:

    IF newbal < 0 AND NOT acct.allow_negative THEN RAISE ... LG001

`ledger_accounts.allow_negative` is `NOT NULL DEFAULT false` and `cp_app` holds
INSERT on the table. So a caller could create an account that says it may go
negative, and post it below zero for ever after. Migration 00604 restricts
UPDATE to `(status)` and explains that the app *"must never change an account's
negative-balance policy"* — which is true, and it was setting the policy at
creation instead.

The application itself is not the risk here: `internal/ledger` sets the column
from its chart of accounts and `checkDefinition` refuses an account whose stored
values disagree with its code. What was missing is the check where the guard
reads it.

**Fix.** Migration 00717 adds `cp_ledger_code_allows_negative` and
`cp_ledger_code_normal_side` and a CHECK constraint binding both columns to the
code. Existing rows are validated rather than grandfathered: a forged account
already in the table would be a finding, not an inconvenience.

**And the copy is compared, not trusted.** These functions are a second copy of
a Go table — the exact maintainability risk this register lists under "findings
deliberately NOT raised", and the one that arrived as F-43 in a security
control. `TestIntegration_GoAndSQLAgreeOnTheChartOfAccounts` drives both
functions for every code the Go registry declares, with negative controls for
"every code allows negative" and "none does", either of which would make the
comparison agree perfectly and prove nothing.

## F-50 · An asset could be stored with no value domain at all · BASELINE · P2 · FIXED

**Found by** the parity test written for F-49's sibling claim — a comment in
`internal/assets` promising that "a test asserts the two agree for every kind",
where no such test existed. It failed on its first run.

Migration 00710 introduces `assets_kind_domain_agree` with the sentence *"a kind
and a domain that disagree is a data-entry error that would silently reclassify
value"*. It does not catch the commonest form of that error.

    CHECK ( (kind = 'CREDIT'       AND value_domain = 'INTERNAL_CREDIT')
         OR (kind = 'NATIVE_ASSET' AND value_domain = 'INTERNAL_NATIVE_ASSET')
         OR (kind = 'FIAT'         AND value_domain IS NULL)
         OR (kind IN (...)         AND value_domain IN (...)) )

For a CREDIT row with `value_domain` NULL the first disjunct is `true AND NULL`
= NULL and the rest are `false`, so the whole expression is NULL — **and a CHECK
constraint fails only on FALSE.** NULL passes.

Demonstrated as `cp_app` against a migrated database: a CREDIT asset with no
value domain inserts cleanly. `internal/assets` refuses the same shape, so Go
and SQL disagreed on every kind except FIAT.

**How far it goes.** Not far, and worth saying so rather than overstating it:
`cp_ledger_account_domain` raises VD001 for an asset with no domain, so a
malformed asset cannot hold a ledger account and cannot be posted. What it can
do is exist, be listed, and be referenced by anything that never opens an
account.

**Fix.** `assets_value_domain_presence`, stated as an equivalence —
`(kind = 'FIAT') = (value_domain IS NULL)` — so both sides are non-null booleans
and the constraint can never evaluate to NULL. The parity test drives both
constraints, coalescing each the way a CHECK behaves, and is exhaustive over
every (kind, domain) pair including the absent domain.

**The general lesson, which is not about this constraint.** Any CHECK whose
expression can be NULL is a CHECK that accepts. This one had been read by
several people, cited in a doc comment as the thing Go mirrors, and was wrong in
a way that reading it in Go-shaped terms does not reveal.

## F-51 · Three operations documents described a system that does not exist · BASELINE · P2 · FIXED

**Found by** an audit of the documents against the code — the same question
`test/docs` asks mechanically for cited test names, asked by hand of every other
kind of claim.

- **`docs/architecture/EXECUTION.md`**: *"The signing package is importable only
  by `cmd/execution-worker` (depguard rule)"*. Both halves false. The only
  depguard rule naming `internal/signing` is `agent-authority`, which denies it
  to the agent trees — a different restriction, in the opposite direction. And
  `cmd/execution-worker` does not import it: `internal/signing` has no non-test
  importer at all.
- **`docs/operations/DEPLOYMENT.md`**: *"`gates.Bootstrap` persists a `DISABLED`
  row for every capability at first start"*. It has no caller in `cmd/`; only
  `scripts/gateceremony` invokes it. Migration 00701 and the readiness report
  both say a fresh deployment has no gate rows; the deployment runbook said the
  opposite. Still fail-closed — an absent row is INACTIVE — but an operator
  looking for those rows after a deploy will not find them.
- **`docs/operations/RECONCILIATION.md`**: *"The reconciliation engine is not yet
  implemented (Stage 7)"*. It is: the engine exists, a worker drives it on a
  ticker, and the admin read endpoints are wired. Stale in the PESSIMISTIC
  direction, which misleads just as effectively — a reader would not go looking
  for a subsystem that is running and raising records.

All three are corrected in place and labelled as corrections rather than
silently edited, for the reason F-40 gives: a document that quietly changes its
mind teaches nobody anything.

**What this says about the mechanical check.** `test/docs/references_test.go`
verifies that every TEST NAME cited in five documents exists — one narrow class
of claim, in five of the repository's fifty-odd documents. Nothing checks a
claim about a control, a wiring, or a count. These three were found by reading.

## F-52 · An account's new risk could be blocked with no wired way to unblock it · BASELINE · P1 · FIXED

**Found by** a worker-reachability sweep asking which exported functions have no
caller a deployment can run.

The two halves of reconciliation were wired asymmetrically:

- **Blocking was live.** `cmd/reconciliation-worker` runs `RunPeriodic` and
  `SweepEscalations` on tickers and raises records. `ReconciliationBlockReader`
  is wired into `cmd/api` and reads `reconciliation_records WHERE
  blocks_new_risk AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED')`,
  which removes the account's capacity to take new risk.
- **Resolution was not.** `cmd/api` set `Reconcile: nil`, so both admin
  endpoints answered 422 UNSUPPORTED and `Engine.ResolveManual` had no caller
  outside `internal/reconciliation`'s own tests.

A deployment could freeze somebody's account by automated check and had no
button to unfreeze it. The only recourse was a hand-written UPDATE against the
database — the thing the admin plane exists to replace.

**This is F-29 one level up.** That finding was *"a user could reserve their
Credits in a payout request and had no way to release them"*. This is capacity
reserved with no path to release, found by the same question.

**Fix.** `cmd/api` constructs a resolution-shaped `reconciliation.Engine` and
`httpapi.NewReconciliationPort` serves both routes.
`TestIntegration_AnOperatorCanClearAReconciliationBlock` raises a blocking
record, asserts the account IS blocked, resolves it over HTTP as an operator,
and asserts the block is gone and the record names who cleared it and why. It
was observed failing with the port unwired.

**Three things the domain refused, which the fix respects rather than works
around.**

1. **`INVESTIGATING` comes before `RESOLVED_MANUAL`,** and the state machine
   says why: somebody looked at the evidence before resolving. A worker-raised
   record is `MISMATCH`, or `ESCALATED` after the sweep — so a resolve-only
   endpoint could not clear a single record the worker actually produces, and
   this would have been fixed in name only. The adapter moves those two into
   `INVESTIGATING` first, carrying the operator's own reason.
2. **A record may change status at most once per transaction.** The repository
   refuses more than one, which is what keeps each transition bound to exactly
   one audit row. So the investigate commits first and the resolution follows in
   its own transaction. A failure between them leaves the record
   `INVESTIGATING`, which still blocks: nothing is lost and repeating the call
   finishes the job, because the first step is skipped for a record already
   there.
3. **An `OPEN` record is not operator-resolvable.** OPEN means the engine has
   not decided there is a difference at all, and its only transitions are the
   engine's own. The adapter does not force a path there;
   `TestIntegration_AnOpenRecordIsNotOperatorResolvable` asserts the refusal
   rather than working around it.

**What is deliberately NOT wired.** A compensating posting. It is the only way a
resolution changes financial state (PART 195), and building one from an API
request means choosing the posting's kind, its idempotency key and its
reference, and mapping each entry's account code onto an owner — decisions with
financial consequences that deserve their own design and review. The adapter
refuses one **by name**, and the engine it is given carries no ledger, so the
two agree even if somebody later changes only one of them.
`TestIntegration_AReconciliationResolutionRefusesToPost` asserts the refusal and
that the record is left exactly as it was.

## F-53 · A market could be tradable while its economics were still editable · BASELINE · P2 · FIXED

**Found by** an adversarial read of the migration surface, comparing which
column each guard actually reads.

Two freezes exist for PART XIII's *"do not allow a creator to silently change
economics after buyers enter"*:

    cp_native_market_curve_frozen     keys on native_markets.activated_at
    cp_native_asset_economics_frozen  keys on native_assets.economics_locked_at

and both `RETURN NEW` unconditionally when their column is NULL. What decides
whether a fill is accepted is a **different column**:
`cp_native_market_apply_fill` gates on `m.status IN ('ACTIVE','CLOSE_ONLY')`.

Nothing tied them together. A row with `status = 'ACTIVE'` and `activated_at`
NULL would trade normally while `virtual_credit_reserve`,
`initial_asset_reserve`, `platform_fee_bps` and `creator_fee_bps` stayed
mutable — so the constant product `k` and the NM005 supply ceiling could be
rewritten between fills, which is precisely the harm the freeze exists to
prevent.

The application never produces that row: `nativemarket.SetStatus` sets
`activated_at` in the same UPDATE as the status and `nativeasset.Activate` does
the same for its lock, each with a comment saying why. This is F-49's shape — an
invariant that holds because the service is careful, in a database that is
supposed to hold it whatever the caller is.

**Fix.** Two CHECK constraints in migration 00718, tying tradability to the
freeze on both tables. Written as `status NOT IN (...) OR <column> IS NOT NULL`
so the expression can never evaluate to NULL, which is F-50's lesson applied one
migration later.

**The test was written wrong twice, and both mistakes are the same mistake.**

1. It first tried to build the forbidden state by clearing `activated_at` from
   the fixture's live market. That is refused by `cp_native_market_curve_frozen`
   itself (NM003), a BEFORE trigger that runs ahead of the constraint — so the
   test would have passed on the trigger and proved nothing about the CHECK.
   The state has to be built from a market that has never been activated, where
   the freeze trigger returns early and the constraint is the only thing there.
2. Its positive control then tried `UPDATE … SET status = 'ACTIVE', activated_at
   = now()` by hand, and the AU001 audit binding refused it: a status change
   needs a transition row in the same transaction. The control now goes through
   the service, which is what a launch actually is.

Both are the same lesson as the finding itself: **a test that passes because a
different guard fired is a test that has not seen the guard it names.**

## F-54 · Every derivable count in the readiness documents had gone stale · BASELINE · P3 · FIXED

**Claim under test.** The numbers a reviewer reads in the readiness documents
describe the system as it is now.

**What was found.** Five of them did not.

| Document | Said | Actually |
| --- | --- | --- |
| `MASTER_BUILD_STATE.md` | capabilities: 20, of which **9** are high-risk | **18** |
| `MASTER_BUILD_STATE.md` | **59** permissions | **60** |
| `PRODUCTION_READINESS_REPORT.md` | browser suite: **69** tests | **70** |
| `PRODUCTION_READINESS_REPORT.md` | a **66**-test suite is not a person | same suite, same page |
| `BACKUP_RESTORE.md` | latest drill: **89** tables, version **604**, 7.8 s | **118** tables, version **718** |

Each was written accurately. The high-risk figure was right until F-16 moved
MARKETPLACE and the rest of the internal economy behind gates and did not
recount. The readiness report contradicted itself twice on one page because
three sentences were written on three different days. The restore drill's line
was the worst of them: a hundred and eleven migrations old, describing a run
that had never seen a Domain A table, the ledger's chart-parity CHECK, or the
frozen-economics constraints — which makes it not a weaker claim than it
appears but a claim about a different database.

**Severity.** P3. Nothing here is a control that fails; it is the evidence a
reviewer would use to decide whether the controls are there. A readiness
document whose numbers drift is exactly F-14 and F-18 again — a document
asserting something about the system by recalling it rather than checking —
in its slowest and least visible form. F-18 was caught because it was wrong in
four places at once. These were wrong in one place each, for months.

**Fix.** `TestDocs_CountsMatchTheCode` derives each number from the thing it
describes and asserts it against the sentence that carries it: capabilities and
high-risk capabilities from `gates.AllCapabilities`/`gates.IsHighRisk`,
permissions from `security.AllPermissions`, and the drill's schema version from
the newest migration in the tree. It sits beside the citation check this package
already ran, which is what caught the first attempt at this fix — the edit
cited `TestDocs_CountsMatchTheCode` in the build state before writing it, and
`TestDocs_EveryTestTheyNameExists` refused it. That is F-25's mistake, made
again, by the thing correcting F-25's class of mistake, and stopped by the
control built for it.

The migration-version claim deliberately fails on staleness rather than on
typos: it compares the cited version against the tree, so landing a migration
without re-running `make restore-drill` breaks the build. It broke immediately
— the drill was three migrations behind at the time of writing. The fix was
to re-run it, not to edit the number: version 718 on both sides, 118 tables, row
counts identical, zero balance drift, journal hashes equal, 10.6 s. That run is
also the first to carry 00716–00718 through a real dump and restore.

The browser count is left out on purpose. Deriving it means expanding the
Playwright loops in Go, and a derivation that is wrong while asserting against a
number that is right would be worse than the staleness it replaced.

**Evidence.** `go test ./test/docs/` — observed failing on the stale drill
version before the drill was re-run, then passing.
`TestDocs_CountsCheckIsNotVacuous` is the positive signal: it fails if the
derivations collapse to zero or to each other, so agreement means something.

## F-55 · Every runbook described a system that predates the build · BASELINE · P1 · FIXED

**Claim under test.** An operator following a runbook during an incident is
told what this system can actually do.

**What was found.** 140 `PENDING` markers across 21 runbooks and the runbook
index. Every one that named a Go package, a binary or a make target named
something that is on disk:

| The marker said | On disk |
| --- | --- |
| `cmd/api` — "no serving binary, no handler wiring" | serving the whole API, admin routes permission-gated and step-up-protected |
| `internal/reconciliation`, `cmd/reconciliation-worker` | both built; the worker runs on a ticker |
| `cmd/audit-worker` | built; `make verify-audit` drives it |
| `internal/agent`, `internal/model`, `internal/strategy`, `internal/reality`, `cmd/agent-worker`, `cmd/market-ingest-worker` | all six built and integration-tested |
| the executor/recoverer | `cmd/execution-worker` |
| `internal/provider/privy`, `signing.Service` | both exist |
| production broker/Temporal client wiring "in the `cmd/*` composition roots" | `cmd/relay-worker`, `cmd/workflow-worker`, `cmd/market-ingest-worker` |

Fifteen HTTP routes the runbooks tell an operator to call were checked against
`openapi/openapi.yaml`. **All fifteen exist**, and twelve of them carried a
marker saying they did not. The index's closing paragraph read: "Until
`cmd/api` lands, the only way to activate a kill switch is to call
`killswitch.Controller.Activate` from a Go program run by an operator against
the database; there is no CLI."

**Severity.** P1, and the reasoning is worth stating because no production code
is wrong here. A runbook is read once, under time pressure, by somebody who was
not there when it was written. `database-corruption.md` step 5 told a responder
that the reconciliation engine did not exist — during a corruption
incident, that is the responder not running the one tool that says what the
restored copy is missing. `global-kill-and-reenable.md` told them to write a Go
program to stop the platform. The cost of a stale runbook is paid in the
minutes an incident is made of.

**The sharpest instance** was not a stale marker but a positively wrong query.
`funding-provider-compromise.md` told an incident responder to detect webhook
forgery by looking for `provider_events.signature_verified = false` rows, and
said the `webhook_signature_failed` emitter was PENDING. Both halves are wrong
in the dangerous direction: `internal/webhook` rejects an event whose signature
does not verify *before* writing any `provider_events` row, so that column is
`true` on every row that exists and the query cannot return one however bad the
incident is — while the emitter it says is missing writes a HIGH-severity
`security_events` row on every failure. A detection query that cannot fire is
worse than no query, because it answers.

**Fix.** Every false marker corrected against the tree. The vocabulary is now
two words that mean different things: `PENDING` for what is missing from this
repository, `BLOCKED_EXTERNAL` for what needs a cloud account, a credential or a
funded wallet. Filing "we have no AWS account" under the same word as "this
package does not exist" is what made the second invisible.

Both claims are now checked, in `test/docs`:

- `TestDocs_NothingMarkedPendingAlreadyExists` fails when a marker names a
  package, command or make target that is on disk. This forces a marker to name
  what is actually absent: "PENDING: `cmd/api` route" is the shape that rotted,
  because `cmd/api` existed and the route was the pending part, so nothing
  distinguished the sentence from a true one once the route landed.
- `TestDocs_EveryRunbookRouteIsServed` holds every `GET`/`POST` path in these
  documents against the spec. A stale marker wastes a responder's time; a route
  that does not exist wastes it and leaves them with no containment action.

**Evidence.** Both observed failing on planted defects — a PENDING naming
`internal/ledger` and `make unit`, and a call to
`POST /admin/definitely-not-a-route` — and passing after. Three markers
survive, and all three are true: `internal/backtest` and `internal/performance`
do not exist, and there is no `make reconcile-full` target.

**Related.** This is F-51 at the scale of the whole runbook set. F-51 fixed
three documents by hand; the rest went on rotting, which is the argument for
the mechanical check rather than a second careful read.

## F-56 · Provider webhook evidence was mutable by the application role · BASELINE · P2 · FIXED

**Claim under test.** `provider_events` is the evidence record. Migration 00107
says so in its header comment, and its `Down` section says "provider evidence is
never dropped".

**What was found.** Neither statement was enforced. The table had no trigger of
any kind — alone among the evidence tables in this schema, where
`audit_events`, `raw_archive_objects` and `payout_provider_events` all carry
`forbid_mutation` — and `cp_app` held table-wide `UPDATE`. So the columns
the evidence consists of were writable by anything holding the application role:
`payload_hash` (what the provider actually sent), `signature_verified` (that we
checked it was them), `raw_ref` (where the untouched body is archived),
`provider_event_id`, `received_at`.

Nothing does write them. `internal/webhook` touches four lifecycle columns in
two places and is careful about it. That is exactly F-49's shape: an invariant
that holds because the one caller is well behaved, in a table whose entire
purpose is to be arguable against a provider who disputes what they sent.
Evidence the disputing party's own software could rewrite is not evidence.

**Fix.** Migration 00719 states it twice, following the house pattern for a
table that has lifecycle columns and so cannot simply be `forbid_mutation`'d
(`fills_guard`, `execution_plans_guard`, `notifications_guard`):

1. the `UPDATE` grant is narrowed to the five columns that legitimately move,
   so the privilege system refuses the rest before a trigger is reached;
2. `provider_events_guard` refuses them again with SQLSTATE `LG003`, which also
   covers the owning role and any future grant somebody widens without reading
   the migration.

`canonical_event_id` is in the mutable set deliberately — nothing writes it
today, but it exists to be filled in when an event becomes a canonical one,
which is the lifecycle assignment the guard permits. `signature_verified` and
`verification_error` are frozen at their inserted values, and the reason is
recorded in the migration: a `provider_events` row exists only for an event that
verified, so the column records a fact rather than holding a field to revise.

**Evidence.** `TestIntegration_ProviderEvidenceCannotBeRewritten` drives six
evidence columns twice — once as the owner, which every grant is irrelevant
to, so a refusal is the trigger and nothing else; once as `cp_app`, which is the
role a defect or an intruder in this codebase would hold. Observed failing with
the trigger dropped and the table-wide grant restored: **every one of the twelve
mutations succeeded**, including `cp_app` rewriting `payload_hash` and setting
`signature_verified = false` on a verified event.
`TestIntegration_TheLifecycleColumnsStillMove` is the positive control — a
guard that refused everything would pass all twelve assertions and break the
pipeline that writes the evidence, which would look like an empty table rather
than an error.

## F-57 · Five subsystems' tests skipped silently in CI, and fifteen VERIFIED rows cited them · BASELINE · P1 · FIXED

**Claim under test.** The `integration` job runs the integration tests.

**What was found.** It enumerated all of them and ran most of them. Five
packages need something a Go test binary cannot start, and the job started
Postgres alone:

| Package | Needs | Guard |
| --- | --- | --- |
| `internal/reality` | ClickHouse | `CP_TEST_CLICKHOUSE_ADDR` |
| `internal/reality/redpandabus` | Redpanda | `CP_TEST_REDPANDA_BROKERS` |
| `internal/archive` | MinIO | `CP_TEST_ARCHIVE_ENDPOINT` |
| `internal/workflows` | Temporal | a health check |
| `internal/ratelimit` | Redis | `CP_TEST_REDIS_URL` |

Each skipped, and a skip is a pass. **Fifteen rows of
`REQUIREMENTS_TRACEABILITY.md` marked VERIFIED cite those exact tests as their
evidence**, including R-S11-1, whose stage exit is entirely met by
`TestIntegration_ClickHouse_LookAheadLeakage` and
`TestProp_ClickHouse_SnapshotNeverReturnsFutureKnowledge`.

`internal/ratelimit` was worse than skipped: its Redis test sat in an untagged
file, so `make unit` invoked it without Redis and the integration job's
enumeration — which selects packages by the build tag — never reached
the package at all. It could not run anywhere.

And `make property`, cited as the operational evidence for R-150-1, runs
`go test -run 'Prop|Property' ./internal/...` with no build tag. It cannot
compile the database-backed property files, so it silently ran none of the ten
packages that hold them — capital conservation, reservation
oversubscription, reconciliation convergence, balance convergence among them.

**The repository already knew.** The chaos job carries this comment: "Without
these two, broker_stall_test and archive_refused_test t.Skip() silently and the
job finishes in under half a second while reporting success." That warning was
written for one job and never applied to the other, and
`PRODUCTION_READINESS_REPORT.md` action item 13 asked for exactly this
— "Export ... in the chaos **and integration** jobs" — and only the
chaos half was done.

**Severity.** P1. This is the recurring class in its quietest form: *a check
that can only skip is a check nobody runs*. It is worse than a check that can
only fail, because a failing check gets noticed.

**Fix.**

- `internal/testkit/deps` decides whether a missing dependency is a skip or a
  failure. A job that promises the stack sets `CP_TEST_REQUIRE_EXTERNAL_DEPS`;
  in that job a missing dependency is a failure. Anywhere else it stays a skip,
  which is what makes this safe to apply everywhere — a fast unit job that
  never starts Redis is not lying when it skips a Redis test, because it never
  claimed to have Redis. Only a job that claims it can be caught out.
- The `integration` job now runs `make infra-up` rather than starting Postgres
  alone, exports the five addresses, and sets the promise. A following step
  re-runs the five packages verbosely and fails on any dependency skip.
- The Redis test moved to `internal/ratelimit/redis_integration_test.go` behind
  the build tag, which is what puts it in the enumeration.
- `scripts/inttest` gained `-run`, filtering packages by whether they declare a
  matching test so a database is not provisioned for a package with nothing to
  run, and `make property` now invokes it. A target cited as proof has to
  execute the thing.

**Evidence.** `deps.Need` observed in both directions: with
`CP_TEST_REQUIRE_EXTERNAL_DEPS=1` and no endpoint,
`TestIntegration_S3_PutGetHeadListRoundTrip` **fails** naming the variable;
with the endpoint, it passes. All five packages then run green against the live
stack — `internal/reality` 36 passes and zero skips, with the two
look-ahead-leakage tests among them.

## F-58 · Documented SQLSTATEs that nothing raised, and raised ones nothing handled · BASELINE · P2 · FIXED

**Claim under test.** Each migration's `-- Custom SQLSTATEs:` header names the
codes its triggers raise, and the application classifies them.

**What was found.** Every custom code in the tree was traced from its
documentation to its raise site to its Go handler. Two gaps, in opposite
directions.

**Documented, never raised (4).** Each labels an invariant that *is* enforced,
by a different mechanism carrying a different code — so the property holds
and the documentation is fiction:

| Code | Says | Actually enforced by | Actual SQLSTATE |
| --- | --- | --- | --- |
| `CR002` | immutable row | `forbid_mutation()` on the three credit tables | `P0001` |
| `CR005` | more than one Credit asset | `assets_single_credit_asset` unique index | `23505` |
| `PO002` | illegal state transition | the `state` CHECK plus the `AU001` transition binding | `23514` |
| `PO004` | amount mismatch | `settled <= reserved <= requested` CHECKs | `23514` |

The cost is not a missing control; it is that the application cannot tell these
apart from any other unique or check violation, so a second Credit asset and a
duplicate idempotency key surface identically.

`PO003` is a third kind of drift: documented as "provenance mismatch", and all
three of its raise sites emit `PAYOUT_ALLOCATION_IMMUTABLE`. The Go handler
agrees with the SQL and contradicts the header.

**Raised, never handled (13).** `VD001`–`VD005` — the entire
value-domain family, thirteen raise sites — appear in no Go file at all,
test or otherwise: `internal/valuedomain` has no `SQLState` or `pgconn`
reference, so every one of them reaches a caller as an unclassified
`INTERNAL`. `BT001` likewise. `GT001`, `GT002`, `GT004`, `GT005` and `AU002`
are asserted only inside integration tests, never mapped in production code.

**The sharpest of these is `GT003`.** It has five raise sites, and migration
00716 describes it as "the line that holds when the Go check is bypassed" —
the database backstop for capability-gate evidence and windows. **No Go code
asserts on it, in production or in a test.** A backstop with no evidence that it
fires is the F-26 shape: a control believed because it was written.

**Fixed so far.**

- **GT003 now has evidence.** `TestIntegration_DatabaseRefusesActivationWithoutEvidence`
  drives all four evidence references and a whitespace-only one, and
  `TestIntegration_DatabaseRefusesActivationOutsideTheWindow` drives the expired
  and revoked cases. Seven assertions of `GT003` where there were none.

  Writing it turned up something worth keeping: blanking an evidence column
  **as `cp_app` is refused outright**, because 00701 grants the application only
  `UPDATE (version)` on `capability_gates`. The privilege layer already covers
  the attacker-with-the-application-credential case. GT003 covers what is left
  — a defect in the Go layer, or anything running as the owner — so
  the test drives the owning connection. Written against `cp_app` it would have
  passed on SQLSTATE 42501 and never reached the guard it names, which is F-53's
  mistake again.

  The window half also had to be restructured: pushing a gate's state back from
  ACTIVE to APPROVED to reuse it is refused by the AU001 audit binding. Each
  case now gets its own gate, driven to APPROVED through the real proposal and
  approval path.

- **The value-domain family is classified.** `valuedomain.MapError` maps all
  five codes, `ledger.MapError` consults it, and
  `TestIntegration_ValueDomainRefusalsAreClassified` asserts what a caller
  receives rather than what the database said. Observed failing with the
  mapping removed: the refusal for moving Credits into real capital came back
  `INTERNAL` rather than `VALIDATION_FAILED`.

  Why the existing tests could not see it: every value-domain test in
  `internal/ledger` asserts on the *text* of the database's message, which is
  there with or without a mapping. Nothing asserted on the classification, which
  is what the customer and the on-call engineer actually see.

**The four fictions are withdrawn, not implemented, and that is a decision
rather than a shrug.**

Migration 00722 withdraws `CR002`, `CR005`, `PO002` and `PO004` and records what
actually enforces each invariant — as a `COMMENT ON` the schema object that
holds it, so a `\d+` or a schema dump carries the correction rather than a
sentence in a file nobody greps.

Implementing them would cost more than the fiction does. Making `CR002` real
means replacing `forbid_mutation` on three live tables with a credit-specific
function, changing what `db.IsImmutableRow` sees for those tables, to buy a more
specific name for a refusal that is already correct and already tested. Making
`PO002` real means a second copy of a state machine the CHECK and the AU001
transition binding already enforce — and a second copy is a second thing to
drift. The cost of the fiction was never a missing control; it was that an
application cannot tell these refusals apart from any other unique or check
violation.

What makes it a correction rather than an excuse is the pair of controls:

- `TestIntegration_EveryDocumentedSQLStateIsRaised` fails on any code a
  migration header documents that nothing raises and no later migration
  withdraws. Observed failing with 00722's withdrawal line removed, naming
  exactly those four and no others; passing with it back.
- `TestIntegration_TheWithdrawnInvariantsAreStillEnforced` drives all four
  invariants and watches each refusal: a second Credit asset (23505), a payout
  state outside the set (23514), a legal state change with no transition row
  (AU001), settling more than was reserved and reserving more than was requested
  (23514). A withdrawal that was really an abandonment fails here.

`CR002` is the one honest exception in that test, and it is stated as such: it
asserts that the three credit tables carry the `forbid_mutation` trigger, with
the correct BEFORE/ROW/UPDATE/DELETE bits, because driving a real credit lot
needs a journal transaction with balanced entries — a fixture belonging to
`internal/credit`, not to a migration test. The function's refusal is already
driven directly by `TestIntegration_Migrations`. Two halves, stated as two
halves.

The census caught a fifth case immediately: migration 00720, written in the same
session, documented a `PL002` that is a CHECK raising 23514. The header was
corrected before it landed.

**Still open, and smaller.** `PO003` is documented as "provenance mismatch" and
all three of its raise sites emit `PAYOUT_ALLOCATION_IMMUTABLE`; the Go handler
agrees with the SQL and contradicts the header, so the behaviour is right and
the name is wrong. `BT001` and the test-only `GT001`/`GT002`/`GT004`/`GT005`/
`AU002` are asserted somewhere, just not in production code.

## F-59 · The prediction resolver applied no maximum price age · BASELINE · P1 · FIXED

**Claim under test.** A prediction is scored against the market.

**What was found.** `Resolver.Resolve` reads two prices, at `CommittedAt` and at
the horizon end, through `PriceAsOf`, whose SQL is:

```sql
 WHERE i.id = $1 AND p.received_at <= $2
 ORDER BY p.received_at DESC, p.observed_at DESC
 LIMIT 1
```

`received_at <= $2` is an upper bound — a look-ahead guard, which is the
property the doc comment claims and which holds. There is no lower bound. No
max-age parameter, no interval, and `observed_at` is selected and scanned but
never compared to anything. The `Resolver` struct carries no age field.

So when the price feed for an instrument stops, both calls return the same row:
the newest price that exists, however old. `returnInBPS(start, finish)` is then
zero, `realized` is `DirectionFlat`, drawdown is zero, and `Validate` passes
because both price refs are non-empty. **Every open prediction on that
instrument is durably scored as a miss, and `prediction_outcomes` carries
`forbid_mutation`, so the score cannot be corrected.**

The platform has this concept and the resolver does not use it:
`asset_policies.max_price_age_ms` is `NOT NULL CHECK (> 0)` and is honoured by
`valuation.PriceStore.Latest`, which bounds both ends. `internal/prediction`
reads `asset_prices` directly and never consults `asset_policies`.

**Coverage.** `TestResolverRefusesWhenThereIsNoPrice` covers **zero** price rows
and asserts `CodeStaleMarketData` — a code used here for absence, never for
staleness. `TestResolveScoresFromPointInTimePrices` writes its prices one second
before commit and exactly at the cut-off, and its one "must not be used" case is
a price from the *future*. No test writes an old price. That is the shape of the
gap: the look-ahead direction was tested thoroughly and the look-behind
direction was not tested at all.

**Fix.** `Resolver.usable` refuses a pair of prices that cannot measure the
window they are meant to measure. Two rules, and only the second has a number
in it:

1. **Unconditional:** the two endpoints must be different observations. If
   `PriceAsOf` returned one row for both cut-offs, no price arrived between the
   commitment and the horizon, and the computed return is zero by construction
   rather than by measurement. FLAT would be a statement about our data.
2. **Bounded:** each price must be within `maxPriceAge` of its own cut-off,
   measured on `ReceivedAt` — per `PricePoint`'s own comment, only what we
   had received decides what was knowable when.

`NewResolver` takes the age as a required argument and refuses a non-positive
one, naming the consequence. A default inside the constructor is how this comes
back: silently, in whichever deployment forgot to set it. `cmd/agent-worker`
sets fifteen minutes, with the reason written where the number is.

**Evidence.** `TestIntegration_ResolverRefusesADeadFeed`,
`TestIntegration_ResolverRefusesOneObservationSpanningTheWindow`,
`TestIntegration_ResolverRefusesAStaleStart` and
`TestResolverRequiresAMaximumPriceAge`. The dead-feed test also asserts that
**no outcome row was written** — the table is append-only, so a
half-resolution would be as permanent as a wrong one. The one-observation test
carries the positive control: adding a second price inside the window makes the
same prediction resolve, and resolve UP.

**Left open deliberately, and recorded here so it is a decision.** A refused
resolution stays unresolved and is retried, which for a permanently dead feed
means retried forever. That is the right behaviour for a resolver — it must
not invent a score — but it needs the accounting in F-60 to be visible
rather than merely absent.

## F-60 · Calibration dropped predictions silently, and its denominator hid it · BASELINE · P2 · FIXED

**Claim under test.** A calibration snapshot describes how a strategy version
performed over a window.

**What was found.** `calibrationSourceSQL` joins `predictions` to
`prediction_outcomes` with an **inner** join, so every prediction in the window
without an outcome — unresolved, or repeatedly unresolvable through F-59
— is dropped before Go sees a row. `PGCalibrator` holds only a clock: no
logger, no counter, no metric. The excluded rows never cross the database
boundary and nothing anywhere compares the window's prediction count against the
outcomes found.

`CalibrationRow.NPredictions` is `len(bucket)` — the count that *survived*.
There is no field for the window's population, the excluded count, or the
unresolved count, so a reader cannot tell 6 of 6 from 6 of 600. Worse, that one
figure sits beside four statistics computed over four different divisors:
`RealizedFrequency` over `hitCount`, `BrierMean` over `brierCount`,
`LogLossMean` over `logCount`, the return means over `len(bucket)`. An unset
statistic persists as SQL `NULL`, not as a visible gap.

These snapshots are load-bearing:
`agent_lifecycle_transitions.calibration_snapshot_id` is a foreign key onto this
table, so **a promotion decision cites a snapshot whose sample loss is
unrecorded.**

The existing test pins the behaviour rather than questioning it:
`TestCalibrationIgnoresUnresolvedPredictions` builds one open prediction and
asserts `err == nil` with zero rows. It is a test *for* the silent drop. No test
covers the mixed case — some resolved, some not — and the only
aggregation test writes an outcome for all six of its predictions, so the
`hasHit` / `brier != nil` / `logLoss != nil` branches are never exercised in the
false direction.

The empty-bucket skip is documented as deliberate, and the reasoning is right:
"an empty bucket is omitted rather than reported as zero, because 'no evidence'
and 'perfectly calibrated' are not the same statement." That reasoning simply
was not carried through to the predictions the query dropped.

**Fix.** Migration 00721 adds `window_predictions` and `window_scored` to
`calibration_snapshots`, and `Compute` counts the window's population with a
second query that deliberately does **not** carry the outcome join. The regime
filter is left off that count too: a regime is a property of the outcome, so an
unresolved prediction has none and filtering on it would re-introduce the very
exclusion the count exists to expose.

Every row of one computation carries the same two figures, which looks redundant
and is not: a promotion cites **one** row by foreign key, so that row has to be
self-describing.

The columns are NULLable, and that is the honest choice rather than a
convenience. A row written before this migration was computed without the
figure; a default of zero would assert that nothing was dropped, which is
precisely the false statement at issue. New rows always carry both, which two
CHECK constraints require, and `Persist` refuses a row that does not so the
message names the snapshot rather than the constraint.

**Evidence.** `TestIntegration_CalibrationStatesWhatItCouldNotScore` builds six
predictions and resolves two, then asserts the snapshot says **6 and 2** and
that the buckets partition exactly the two -- a number the old code could not
produce, since it only ever counted survivors. It reads the figures back out of
the persisted row, which is what a promotion decision reads.
`TestIntegration_CalibrationSampleIsNotVacuous` is the positive signal: with
nothing dropped the two numbers must agree, so a `WindowPredictions` that had
silently collapsed onto `WindowScored` fails one test or the other.

**Not fixed, and recorded.** The per-statistic divergence remains: `NPredictions`
sits beside four means computed over four different divisors (`hitCount`,
`brierCount`, `logCount`, `len(bucket)`), and an unset statistic still persists
as NULL. That is a narrower problem than the sample loss and it needs a decision
about what a partially-scored bucket should report, not just a column.

## F-61 · `position_lots` had no database invariant beyond per-row bounds · BASELINE · P2 · FIXED

**Claim under test.** Provenance lots are held to their invariants by the
database, as the credit lots beside them are.

**What was found.** The schema is a strong guard on per-row arithmetic and no
guard at all on anything relational:

| Invariant | SQL | Go |
| --- | --- | --- |
| `quantity_original > 0`, `0 <= quantity_open <= quantity_original` | CHECK | yes |
| at acquisition `quantity_open = quantity_original` | **no** | yes (one placeholder used twice) |
| `status = 'CLOSED'` iff `quantity_open = 0` | **no** | yes (a CASE in the UPDATE) |
| open quantity only ever decreases | **no** | yes (`OpenAfter = Open - min(Open, remaining)`) |
| cannot dispose more than the open quantity | **no** | yes |
| Σ open lots per (account, asset) = WALLET balance | **no** — detected, never enforced | reported as `Drift` |

`cp_app` holds table-wide `UPDATE`, and `position_lots` is deliberately absent
from `appendOnlyTables` (correctly — it has lifecycle columns). So
`UPDATE position_lots SET quantity_open = quantity_original` satisfies every
constraint and every trigger, refilling a consumed lot.

The asymmetry is the finding: `credit_lots`, doing the same job for Credits,
**does** get a SECURITY DEFINER trigger raising `CR001 CREDIT_LOT_OVERCONSUMED`.
The two lot tables were written to different standards.

No test writes an invalid lot directly through SQL. Every `position_lots`
reference in a test is a `SELECT`, and the concurrency test drives
`e.Dispose`, so it proves the Go compare-and-set rather than a database
constraint.

**Fix.** Migration 00720, in the shape `credit_lots` already had:

- `position_lots_status_matches_quantity` states `(status = 'CLOSED') =
  (quantity_open = 0)` as a CHECK. Both operands are NOT NULL, so the expression
  can never be NULL, which is F-50's lesson.
- `cp_position_lot_guard` raises `PL001` on an open quantity that rises, and on
  any change to `quantity_original`, `acquired_at` or `cost_basis_usd_minor` --
  the columns a FIFO ordering and a realized gain are computed from. Moving
  `acquired_at` reorders every disposal that has not happened yet.
- The UPDATE grant narrows to `quantity_open` and `status`, so the application
  is refused by privilege before a trigger is reached.

`updated_at` is deliberately not granted: the `set_updated_at` BEFORE trigger
assigns it and PostgreSQL checks column privileges against the statement's SET
list rather than what a trigger writes. That was verified by running the
disposal path, not reasoned about, and
`TestIntegration_DisposalStillWorksUnderTheGuard` asserts `updated_at >
created_at` so a wrong answer surfaces as a failure rather than as a stopped
clock.

The header of 00720 originally documented a second code, `PL002`, for the CHECK
-- which raises 23514 and not a custom code at all. Writing F-58 in the same
session is what caught it. A header that names a code has to be a header whose
migration raises it.

**Evidence.** Five tests, observed failing with the trigger dropped, the CHECK
dropped and the table-wide grant restored: the consumed lot was refilled, the
cost basis was edited, the acquisition time was moved back a day, the status was
decoupled from the quantity, and `cp_app` did all of it.
`TestIntegration_DisposalStillWorksUnderTheGuard` is the positive control, and
it is the reason the grant names exactly two columns -- one too few would pass
every refusal above and silently break the only path that writes these rows.

## F-62 · `normalized_events` replaced a column that varied between copies · BASELINE · P3 · FIXED

**Claim under test.** Re-ingesting an event is idempotent, as
`reality.Pipeline`'s doc comment says: "Every step is idempotent downstream
(archive dedup key, bus dedup id, ReplacingMergeTree, checkpoint version), so a
crash between steps replays safely."

**What was found.** The ClickHouse table is
`ReplacingMergeTree(platform_received_at)`, `ORDER BY (source, event_type,
dedup_id)`, `PARTITION BY toYYYYMM(decision_available_at)`.

On a re-ingest the archive returns the **original** meta, so
`platform_received_at` — the version column — is frozen; but
`Normalize` is handed a fresh `clk.Now()`, so `normalized_at` and therefore
`decision_available_at` are strictly later. **The version does not move and the
replaced column does.** Two rows with the same sort key and the same version
carry different `decision_available_at`, and which survives is decided at merge
time rather than by the data. Because the partition key is derived from the
column that varies, a re-ingest landing in a different month produces two rows
that `FINAL` cannot collapse.

There is also a semantic disagreement: `reality.Dedup` keeps the **first**
occurrence, and its comment says that is "what ClickHouse's ReplacingMergeTree
does at merge time". ReplacingMergeTree keeps the highest version or the last
inserted. When copies differ, a bus consumer and a ClickHouse reader resolve to
opposite values.

**Severity P3, and only because of what does not read it yet.** Both readers of
`decision_available_at` (`QueryNormalized`, `QueryMarketPrices`) have no
non-test caller: `internal/backtest` and `internal/performance` do not exist,
and `reality.Snapshotter`, which POINT_IN_TIME.md describes as the consumer,
returns zero hits. The Postgres lineage of the same column name, on
`predictions` and `tool_invocations`, is independent and unaffected. This is a
defect in the substrate of an unbuilt subsystem — recorded now because it
will be invisible once something reads it.

**The test masks it.** `TestIntegration_ClickHouse_NormalizedRoundTripAndReplacingDedup`
builds its duplicate by shifting `PlatformReceivedAt` **and**
`DecisionAvailableAt` together, so the version advances and the assertion "the
latest platform_received_at wins" holds deterministically. The pipeline does
the opposite. The pipeline-level test does exercise the real path but runs on a
fake clock that is never advanced between the delivery and the redelivery, so
both normalisations produce identical timestamps and the divergence cannot
appear. Two tests, neither able to see it: the F-26 shape again.

**Fix, and why it is in Go rather than the DDL.** The obvious repairs are to
the table — put `decision_available_at` in the sort key, or partition on the
version column instead. Neither can be deployed: `EnsureSchema` applies the DDL
with `CREATE TABLE IF NOT EXISTS`, so a new key never reaches a table that
already exists. A fix that cannot reach a running deployment is not a fix.

The root cause is in the pipeline anyway. `Ingest` handed `Normalize` a fresh
`clk.Now()` on every pass while `PutMeta` returned the original archived meta,
so the version column was frozen and the replaced column was not. It now derives
`normalized_at` from the archived `platform_received_at`, which makes every pass
produce byte-identical timestamps — and that is what makes the replacement
well-defined, whatever the sort key is.

It is also the more honest reading of the policy. `AvailabilityPolicy` already
adds `FeatureLatency` and `PipelineLatency` as *configured* allowances for
processing time, so a wall-clock `normalized_at` was charging real latency on
top of the modelled latency — inconsistently, and only on the first pass.

`reality.Dedup`'s comment, which claimed to match ReplacingMergeTree, is
corrected: it keeps the first occurrence, the engine keeps the last. The two
agree now because the copies are identical; before, a bus consumer and a
ClickHouse reader could resolve the same event to opposite values with neither
of them wrong.

**Evidence.** `TestIntegration_ReplayingAnEventProducesTheSameTimestamps`
re-ingests one event twice with an **advancing** clock — five minutes, then
forty days — and asserts every timestamp and the monthly partition are
unchanged. Observed failing with the wall clock restored:
`decision_available_at` moved from `12:00:00.755` to `12:05:00.755`, and on the
second replay to **October**, a different partition that `FINAL` cannot collapse
across, while `platform_received_at` stayed exactly where it was.

`TestIntegration_ADifferentEventStillGetsItsOwnTimestamps` is the positive
control: a pipeline that had simply frozen every timestamp would pass every
assertion above and record a stream in which nothing ever happens at a different
time. `TestReplayDeterminismIsAPropertyOfTheDerivation` states the rule without
a database and asserts that a later `normalizedAt` *would* have moved the
result — so the determinism test is not passing on a derivation that ignores
its input.

**What is still true.** There are still no production readers:
`internal/backtest` and `internal/performance` do not exist, and both readers of
`decision_available_at` have only test callers. This was fixed now because the
defect becomes invisible the moment something reads it.

## F-63 · The correction pass made the mistake it was correcting · NEW · P2 · FIXED

**Found by** an independent read of `internal/auth` after F-55 had been
committed, which reported that `auth.Manager.RevokeAllForSubject` has no caller
outside its own package. The runbook step that names it had been edited by F-55
to say it was served.

**What was wrong.** F-55 removed 140 `PENDING` markers that named packages and
binaries which exist. Three of those removals were wrong, because the marker was
about a *capability* and the pass judged the *package*:

| F-55 wrote | The truth |
| --- | --- |
| "`auth.Manager.RevokeAllForSubject(subject)` ..., served by `cmd/api`" | No route exists. `SessionsPort` exposes `ListForSubject` and `Revoke` by session id, and nothing outside `internal/auth` calls `RevokeAllForSubject`. The original marker said "PENDING: `cmd/api` route" and was right. |
| "executor and recoverer wired in `cmd/execution-worker`" (five places) | `bindProviders` returns an error in every production build — *"the venue adapter, chain observer, inspector, signing client and recoverer are not wired in this build"* — and is replaced only in tests. The worker cannot start. |
| "the agreement policy ... wired into `cmd/market-ingest-worker`" | `openProvider` returns a single `chain.SolanaDataProvider`. `chain.MultiObserver` and the two-observer agreement run in tests only. |

The reasoning that produced all three was the same, and it is exactly the
reasoning F-55 exists to name: *the directory is there, so the thing is done*. It
was committed in the act of correcting 137 instances of the same error.

**Why the controls F-55 shipped did not catch it.**
`TestDocs_EveryRunbookRouteIsServed` looks for HTTP paths, and the session step
names none. `TestDocs_NothingMarkedPendingAlreadyExists` looks at markers, and
the pass had removed the marker. Both check that a *negative* claim is honest;
neither checks a *positive* one. That was the missing third direction.

**A fourth thing the same read exposed**, and the reason the session step now
says more than "there is no route": **nothing revokes a session when a role is
revoked.** Roles are frozen into the session row at login, `operator_roles` is
read nowhere else, and `Manager.Rotate` — whose doc comment promises
"rotation on privilege change ... revokes the old session and issues a new
token" — also has no caller outside its package. So during an
admin-compromise incident, clearing `revoked_at` on the suspect's roles leaves
their ADMIN live until the session's own 12-hour expiry. The runbook now says to
revoke the sessions, not the roles.

**Fix.** All three claims corrected, and reworded so the marker names the thing
that is actually missing — `bindProviders`, `chain.MultiObserver` —
rather than a package that exists. That is the rule
`TestDocs_NothingMarkedPendingAlreadyExists` enforces, and it refused two of the
first rewrites for naming `cmd/execution-worker`, which was the check working on
the person who wrote it.

Writing those rewrites also exposed a flaw in that check: its clause boundary
knew about `". "` and not about `".**"`, so a marker in bold read on into the
next sentence and blamed it for packages that sentence merely mentioned. Fixed
with a `sentenceEnd` that understands markup.

**The new control.** `TestDocs_SessionRevocationIsStillUnreachable` asserts both
halves: that `RevokeAllForSubject` still has no caller outside its package, and
that the runbook step naming it still carries a marker. It fails in both
directions — if the function becomes reachable it says to delete the marker
and the control, and if the marker disappears while the function is still
unreachable it says the runbook has gone back to claiming a route. Observed
failing on the second, by restoring the exact sentence F-55 wrote.

It is deliberately one named claim rather than a general scan. The general form
was written first and abandoned: across every runbook there is exactly **one**
function written in call form, so a scan would guard a corpus of one while
carrying a name-based call index that cannot tell a package's own internals from
an external caller — which it got wrong on its first run, reporting
`RevokeAllForSubject` reachable because `auth.Manager` calls its own store method
of the same name. A curated claim that is honest about being curated is worth
more than a general check that is wrong.

**What this says about F-55.** The other 137 removals were checked against the
OpenAPI spec or against a package's existence where existence was the whole
claim, and `TestDocs_EveryRunbookRouteIsServed` covers the largest group of them
mechanically. These three were the ones where the marker's subject was narrower
than the package it named, and nothing distinguished them at the time. The
lesson is not "check more carefully" — it is that a positive claim about
wiring needs a check of its own, which it now has for the case that produced it.

## F-64 · An approval did not stay attached to what was approved · NEW · P1 · FIXED

**Found by** an independent adversarial read of `internal/admin` and
`internal/adminplane`, verified against the schema before acting.

**What was found.** Three columns decide what dual control means, and nothing
protected any of them:

| Column | Decides |
| --- | --- |
| `requires_dual` | whether a second signature is needed at all |
| `kind` | what the approval authorises |
| `target_id` | what it authorises it against |

All three sat on a table `cp_app` held **table-wide `UPDATE`** on, with no CHECK,
no trigger, and no coverage by `params_hash` — which hashes `params` alone
(`ParamsHash: hashBytes(params)`).

**Two consequences.**

*An approval could be repointed.* `VerifyApproved(ctx, q, approvalID, kind,
targetID)` compares the caller's target against the stored one, and nothing bound
the stored one to what was signed for. Two principals approve the release of one
kill switch; a single `UPDATE admin_actions SET target_id = ...` makes the same
approval verify for a different one. `KILL_SWITCH_RELEASE` is the sharp case
because its whole subject lives in `target_id` and its params are empty —
the hash protects nothing there.

*The dual-control gate read the row.* `VerifyApproved` used `a.RequiresDual`
while `Approve` and `executable` used `spec.RequiresDual` from the code's
`KindSpec`. It was the **only** consumer that trusted the column, and it is the
gate `killswitch`, `agent` and `reconciliation` call before acting. A row set to
`requires_dual = false` satisfied it with no approver at all.

**Severity P1** because it is the authority mechanism itself: dual control is the
control that makes every other administrative action safe to have. It needs the
application's database credential, which is the same bar as F-42 — and the
same answer, that privilege is the thing that holds when detection does not.

**Fix, in two independent halves.**

Migration 00723 freezes a proposal's identity with a guard trigger raising
`AD001`, and narrows the grant to exactly the columns `updateStatus` writes —
read off its five call sites, not guessed. `VerifyApproved` now derives dual
control from the `KindSpec` like every other caller.

Two things about the trigger's scope are worth recording, because both were got
wrong first.

*`params` is deliberately not frozen; `params_hash` is.* Freezing `params` made
the `ErrParamsTampered` check in `Execute` unreachable — a working, tested
control turned into one only a test could walk, which is the F-26 class this
session has spent its time removing. Freezing the hash instead is what makes the
pair sound: change `params` and the hash no longer matches; change the hash and
the trigger refuses; change both and the trigger refuses. `params` is outside the
grant regardless, so the application cannot rewrite it either way.

*The behavioural test for the dual-control half is deliberately not written.*
Reaching that branch needs an APPROVED dual-control row with no approver, which
needs a hand-written `admin_action_transitions` row to satisfy AU001 — which
is exactly the forgery F-42 records, performed by a test. The first attempt did
it, and the package's own `TestIntegration_AdminStreamVerifiesAfterEverything`
caught it immediately: one transition, no audit event, invariant broken. A
fixture that has to commit the exploit to reach the code is a fixture that should
not exist, so that half is asserted against the package's own source instead, in
the shape `accountscope_test.go` already uses.

**Evidence.** `TestIntegration_AnApprovalCannotBeRepointedAtAnotherTarget` drives
five identity columns as the owner (so a refusal is the trigger) and four as
`cp_app` (so a refusal is the grant), then re-verifies the approval still holds
for its own target and refuses the other one. Observed failing with the trigger
dropped and the table-wide grant restored: **the repoint succeeded.**
`TestIntegration_TheLifecycleStillMoves` is the positive control — a column
list one entry short would pass every refusal above and break
propose/approve/execute, and the failure would look like actions never completing
rather than like a permission error anybody was watching for.

`TestIntegration_TamperedParamsAreRefused` was updated rather than left: it used
to plant its tamper as `cp_app`, which the narrowed grant now refuses. It asserts
that refusal as the first line and plants the tamper as the owner to keep driving
the hash check as the second.

**Still open from the same read, and recorded rather than fixed:** two identities
for one human defeats approver ≠ proposer entirely, because `users` is
unique on `(idp_issuer, idp_subject)` and there is no person entity — a
human with two IdP subjects can propose from one and approve from the other.
`internal/security/roles.go` says "individually identified (no shared accounts)",
which is a comment rather than a mechanism. That is an identity-model decision,
not a code fix.

## F-65 · Two kill switches that reach nothing, documented as working · NEW · P2 · PART

**Found by** an independent adversarial read of `internal/killswitch`, verified
against the matrix, the agent runtime and the import boundary before acting.

**`MODEL_DISABLE` can never match.** `Matches` requires the action to carry a
model id:

```go
case ModelDisable:
    return a.ModelID != "" && (sw.ScopeID == GlobalScope || a.ModelID == sw.ScopeID)
```

No non-test code sets `ModelID` on a `killswitch.Action` — every
`ModelID:` assignment in the tree is on an audit or event struct. And it cannot
be fixed at the call site, because the model-call path is architecturally
forbidden from consulting a kill switch at all: `internal/killswitch` is in
`forbiddenForAgents`, and `internal/agent` and `internal/model` are agent trees.
`test/security/authority_boundary_test.go` enforces that.

**`AGENT_PAUSE` writes a table the agent runtime does not read.** Activating it
through `POST /admin/kill-switches` writes a `kill_switches` row. Every
agent-runtime pause check reads `agent_pauses` — the dispatcher, the broker,
the model call, the emitter and the runner, five independent points, all correct
and all looking at the other table. `agent.KillSwitchMirror` exists precisely to
keep the two together, and is implemented by nothing; its own doc comment says
the mirror is optional because "the pause itself is authoritative in
`agent_pauses`", which is true of `Lifecycle.Pause` writing *outwards* and says
nothing about an operator writing *inwards*.

**Four documents said otherwise**, and one runbook is the place an operator
looks during exactly this incident:

- `model-malfunction.md` step 2: *"The ToolBroker refuses `CALL_MODEL` under
  `MODEL_DISABLE`"*
- `AGENT_RUNTIME.md` §: `KILL_SWITCH_ACTIVE` listed for `MODEL_DISABLE`
- `SECURITY.md`: the ToolBroker row lists both switches among its checks
- `clickhouse-outage.md`: *"use `AGENT_PAUSE` per affected agent"*

`REQUIREMENTS_TRACEABILITY.md` marked R-053-12 (`MODEL_DISABLE`) **VERIFIED**,
citing a test that asserts the kind's severity and scope-string rules — which
is true of the kind and says nothing about whether anything consults it. That is
F-58's shape in the traceability matrix: a citation that is accurate about
something other than the claim.

**Severity P2 rather than P1**, and the reason is worth being precise about. The
agent runtime is inert: `agent.NewLifecycle` and `agent.NewEmitter` have no
production callers, `cmd/agent-worker` wires no evaluator and its `EmitterFor`
returns `UNSUPPORTED`. So there is no running agent for either switch to fail to
stop. The defect is that the documents promise a control that would not work the
day the subsystem is turned on — and the day it is turned on is exactly when
nobody re-reads the runbook.

**Fixed here: the documents.** All four now say what actually happens, and
`model-malfunction.md` points the operator at the mechanism that does work
(pause the agents; the broker checks `agent_pauses` on every call). R-053-12 is
downgraded to IN_PROGRESS with the reason.

**Not fixed here, deliberately: the bridge.** Implementing `KillSwitchMirror`, or
teaching the operator path to write `agent_pauses`, would wire a control into a
subsystem that does not run — producing exactly the thing this session has
spent its time removing, a control only a test can reach. It belongs with the
work that makes `internal/agent` reachable, and the runbooks now say so where an
operator will read it.

## F-66 · A future authentication time satisfied every step-up window · NEW · P1 · FIXED

**Found by** an independent adversarial read of `internal/auth` and
`internal/identity`, tracing `AuthTime` from where it is set to where it is
checked. Verified against the source and the generated decision vectors before
acting.

**What was found.** `security.RequireStepUp` clamped a negative age to zero
without a bound:

```go
age := now().Sub(p.AuthTime)
if age < 0 {
    age = 0
}
```

`auth_time` is copied verbatim out of the OIDC ID token and validated nowhere.
`checkClaims` verifies `azp`, `nbf`, `iat`, `nonce` and `sub`; `Identity.Validate`
checks only that a subject is present; `Session.Validate` does not mention it;
the column carries no CHECK.

So an `auth_time` any distance in the future — an hour, a year — made
the age zero, which satisfies **every** step-up window in the system: the
15-minute HTTP surface, and the 5-minute windows on the tightest admin kinds
including `LEDGER_CORRECTION`, `WITHDRAWAL_APPROVE` and `BREAK_GLASS_GRANT`. Not
once, but for the whole 12-hour life of the session, because step-up freshness is
the only thing those windows measure.

`adminplane.Actor.steppedUp` and the console's TypeScript port
(`apps/admin/src/decide.ts`) carried the same unbounded clamp, so the affordance
layer agreed with the enforcement layer — both wrong in the same direction.

**Severity P1.** Step-up is the freshness check standing between a stolen
session and every dual-controlled action. The controlling party is whatever mints
the ID token, so this is not reachable by an HTTP caller — `httpmw` reads the
cookie and nothing else, which was checked — but a misconfigured or
compromised identity provider, or a badly-set clock on one, turns it into a
permanent step-up bypass. There was **no test anywhere** for a future
`auth_time`; the OIDC fixture uses a value in the past.

**Fix.** A negative age is treated as "now" only within the tolerance the token
verifier already applies to `exp`, `nbf` and `iat` — `oidc.DefaultClockSkew`,
two minutes. Beyond it the age is not fresher, it is unknown, and an unknown age
fails closed with a message that says so rather than reporting a stale
authentication and sending a reader to the wrong place. The same bound is applied
in `adminplane` and in the console port, because an affordance more permissive
than the server is a live button the server refuses — which
`adminplane`'s own doc forbids.

`security.MaxAuthTimeSkew` and `oidc.DefaultClockSkew` are two numbers that must
agree and live in packages that do not import each other, so
`TestStepUpSkewMatchesTheTokenVerifier` pins them together.

**Evidence.** `TestRequireStepUp_AFutureAuthTimeBeyondSkewIsRefused` drives
inside the tolerance (0, 1 s, exactly the skew — all still pass), beyond it
(skew + 1 s, an hour, a year — all refused), and both ordinary cases as
positive controls: a recent authentication passes and a stale one still fails for
the ordinary reason. Observed failing with the bound removed: an auth time 2m1s
in the future was accepted as a fresh step-up.

The generated decision vectors moved, and the movement is the finding stated as
data: **139 cases changed, all of one principal** — `other_admin_elevated_late`,
whose `auth_time` sits 48 hours after the instant those cases evaluate at. **65
went from ALLOWED to STEP_UP_REQUIRED.** The same principal's cases at the later
instant, where it is genuinely one minute old, are unchanged, so the fixture
keeps the purpose it was written for.

## F-67 · The signing recovery path signed bytes nobody approved · NEW · P1 · FIXED

**Found by** an independent adversarial read of `internal/signing`, verified
line by line before acting.

**What was found.** `Sign` computes the request's hash first:

```go
txHash := sha256.Sum256(req.UnsignedTx)

// Fast replay path without chain I/O.
if existing, err := s.repo.findDecision(...); ... else if existing != nil {
    return s.replay(ctx, req, existing, log)
}
```

and on the replay path never used it. When a decision exists and is APPROVED but
no `signing_results` row does, `replayLoaded` finishes the job:

```go
// Approved but never signed (provider failure or crash): finish signing.
l, err := s.loadWalletAndAccount(ctx, req)
...
return s.signApproved(ctx, req, d, l.wallet, ...)
```

and `signApproved` hands the provider `req.UnsignedTx` — **this call's
bytes**. Nothing compared them against the decision's `InspectedTxHash`, which is
recorded on the row and was simply unused. `Request.Validate` checks only that
`ExpectedTxHash` is 32 bytes; it never checks that it is the hash of
`UnsignedTx`, and both come from the same caller anyway.

So for any attempt in the state *approved, not yet signed*, a second call with
the same `attempt_id` and arbitrary transaction bytes signed those bytes with the
wallet key. None of the sixteen inspector checks runs again on that path, and
neither does `validateLinkage` — wallet status, delegation, plan approval,
risk ALLOW, reservation and quote expiry are all re-read on the first path and
none of them on this one. The hash was the whole of the binding, and it was not
being applied.

**The window is not exotic.** The decision commits in one transaction and the
provider is called in a second. Every provider failure or crash between them
produces exactly this state, and the code documents it as normal recovery —
`TestIntegration_ProviderFailureThenRecovery` exercises it deliberately.

**Latent, not live.** `internal/signing` has no non-test importer:
`cmd/execution-worker`'s `bindProviders` returns an error in every production
build (F-63). This is a code defect on the most security-critical path in the
system, found before that path was turned on.

**Fix.** The replay path refuses unless the offered bytes hash to what the
decision inspected. The concurrent-winner path needed the same treatment and did
not obviously look like it: when a parallel `Sign` wins the attempt lock, the
decision loaded under that lock belongs to *its* request, so its hash travels out
of the transaction with it rather than being assumed equal to this call's.

**Evidence.** `TestIntegration_RecoveryRefusesDifferentBytes` leaves an attempt
approved-but-unsigned, then calls again with one byte flipped and an
internally-consistent `ExpectedTxHash` — which is what the old
`Request.Validate` was checking, and why it was no defence. Observed failing with
the binding removed: execution continued past the check and reached the signer.

Two positive controls, and both are load-bearing. The genuine recovery must still
complete — a binding that refused everything would make every provider
failure permanently unrecoverable, which is worse than the defect. And
`TestIntegration_ReplayOfASignedAttemptStillReturnsTheStoredBytes` pins the path
that must **not** gain a binding: once a result exists the replay returns the
stored bytes without reaching the signer, so a refusal there would break
idempotency instead.

**Related, from the same read and not fixed here:** `signing_decisions` has only
a plain index on `attempt_id`, not a unique constraint — "one decision per
attempt" is held by a `SELECT ... FOR UPDATE` on the attempt plus a re-check, and
`findDecision` concedes it with `ORDER BY created_at DESC ... LIMIT 1`. That is
the F-49 shape and it belongs with the work that makes this package reachable.

## F-68 · The production velocity policy bounds nothing, and said the opposite · NEW · P2 · FIXED

`cmd/api` wires `withdrawal.VelocityPolicy{}` under this comment:

> the bounds stay at zero (no rolling allowance) until the gate is approved

"Bounds at zero" reads as a refusal. A zero policy refuses nothing:
`MaxPerRequest` is not positive so the per-request branch is skipped, and
`!MaxPerWindow.IsPositive() && MaxCountPerWindow == 0` returns nil before
anything is counted. A request of 2¹²⁸−1 base units passes.

The semantics are deliberate and tested — `withdrawal_test.go` asserts "zero
policy is unlimited" — so the defect is not the policy type; it is that the
one place that wires it believed the opposite, in a comment, on a money bound.

**What actually refuses a withdrawal today** is the WITHDRAWALS capability gate,
DISABLED in every environment and checked several steps earlier. That is the
load-bearing control and the velocity policy is not a second one.

**Fix.** The value has a name that says what it does —
`withdrawal.UnboundedVelocity()` — with `PermitsEverything()` beside it, and
the composition root's comment now states the truth. Naming it is the point: a
reviewer reading the composition root sees a value that announces it bounds
nothing, rather than an empty struct that looks like a default somebody chose.

**Evidence.** `TestUnboundedVelocityBoundsNothing` drives the wired policy with
2¹²⁸−1 and asserts it passes, so the fact is recorded rather
than believed — and the day the gate is approved, that test is what says the
velocity policy is still a no-op. `TestARealPolicyActuallyBounds` is the positive
control: without it the first test would also pass against a `Check` that had
stopped refusing anything at all.

## F-69 · Inventory: six independent audits, and what was not fixed · NEW · P2 · OPEN

Six read-only audits ran over the subsystems this session had not touched:
auth/identity, killswitch/eligibility, capital/withdrawal, signing/execution,
admin dual control, and agent authority. Each was asked for the specific defect
classes this codebase keeps producing rather than for a general review.

**What came out of them and was fixed:** F-64 (dual control), F-65 (two dead
kill switches), F-66 (the step-up bypass), F-67 (the signing recovery path),
F-68 (the velocity comment), and the half of F-63 about session revocation.

**One audit claim was wrong and is recorded as such.** It reported that migration
00717 revokes `cp_readonly`/`cp_ops` SELECT on `identity_pii` and `sessions`.
00717 contains no REVOKE; it contains a comment *describing* the revoke that F-47
took back out. That is the second time an agent has misread that comment as the
code, which is why every claim in this session was verified before action.

**Verified and NOT fixed.** Each of these was checked against the source; the
reason for leaving it is given, because "not fixed" without a reason is
indistinguishable from "not noticed".

| What | Verified | Why not fixed here |
| --- | --- | --- |
| A withdrawal is checked against no balance, settlement state or hold — in Go **or** SQL | `internal/withdrawal` imports neither `capital` nor `buyingpower`; `Repository.Transition` has zero non-test callers and there is no approve route, so the "consulted at approval time" the package doc promises has no approval time to happen at | This is Stage-not-built, not a defect in built code. The WITHDRAWALS gate is DISABLED and is the control. Building the approval path is the work, not patching around its absence. |
| `EnvelopeService` is entirely unreachable, so `ApplyRealizedPnL` never runs and no envelope can be exhausted by losses | `NewEnvelopeService` has only test callers; no `EnvelopeAdmin` port, no OpenAPI path, no executor for `ENVELOPE_AUTHORITY_CHANGE` | Same shape. Wiring a limit into a subsystem nothing calls produces a control only a test can reach. |
| `kill_switches_require_transition` fires `AFTER UPDATE OF active`, not on INSERT, so a first activation needs no transition row | Migration 00603; `Controller.Activate` creates via `insertActive` | Real, and narrow: the row still carries its actor and reason, and `Release` recomputes severity from the kind. Worth a migration; not one to write at the end of a batch without its own tests. |
| `internal/eligibility` has zero non-test callers for every function that refuses anything | Only non-test use is a struct field consumed inside the unreachable planner | Already recorded under F-34. Repeating it here so the inventory is complete. |
| `agents.stage` and `agents.mode` are not bound to a transition row, so a PAUSED agent can be moved to stage LIVE by direct UPDATE and then Resumed with only `agent:pause` | 00690 binds `state` only; `cp_require_transition` returns early when the bound column did not change; the `agents` CHECK exempts side states | The sharpest of the agent findings and additive to F-42 — F-42's remedy (revoke UPDATE on the state column) would leave `stage` and `mode` writable. Belongs with F-42's privilege work, deliberately not started piecemeal. |
| `agent.NewEmitter` does not require an `EnvelopeReader`; a nil one silently drops the instrument allow-list and the per-trade cap at any stage | `emitter.go` guards the whole envelope block on `deps.Envelope != nil`, while `NewBroker` refuses six nil dependencies | A one-line fail-closed fix in an inert package. Grouped with the agent-runtime work rather than committed alone, because its test needs the runtime the finding is about. |
| The runner never reads the agent's lifecycle state, so REVOKED, FAILED and SUPERSEDED agents keep executing runs already open | `prepare` uses `Stage` and `RiskPolicyVersion` only; the run-selection query has no agent-state predicate | Same package, same reason. `Pause` does reach an in-flight run, at five independent points, so the mechanism exists and one state is missing from it. |
| `Store.LiveTradingEnabled` scans `effective_at` and `expires_at` and discards them, so an expired LIVE gate reads as active to the agent worker | `store.go`; `gates.Evaluate` checks both | Real divergence between two readers of the same gate. Fixing it means deciding which reader is authoritative, which is a design call in an inert path. |
| Four admin action kinds have no test at all: `ENVELOPE_AUTHORITY_CHANGE`, `WITHDRAWAL_APPROVE`, `NATIVE_ASSET_DELIST`, `PAYOUT_MANUAL_REVIEW_RESOLVE` | Grepped for both the constant and the literal across every `*_test.go` | `PAYOUT_MANUAL_REVIEW_RESOLVE` and `NATIVE_ASSET_DELIST` have live executors and are the two worth writing first. Recorded rather than written because a test per kind is a body of work, not a patch. |
| `ApproverIsNotTarget` is never evaluated true in any test or generated vector, and the console cannot express it | Every fixture uses a `target_id` that is not a user id, so the preceding case always catches first; `ActionKind` has no such field, so `authority.json` cannot carry it | The server refuses correctly (proven end to end), so this is a dead button rather than an escalation — but it violates `adminplane`'s own stated contract not to be more permissive than the enforcement layer. |
| `admin.refuseAgent` permits the **absent** principal, so `VerifyApproved` runs fully unauthenticated | `if p, ok := PrincipalFrom(ctx); ok && p.ActorType == ActorAgent` | Deliberate per its doc ("it needs no principal but refuses agents") and every caller holds a `db.Querier` already. Recorded because the reasoning deserves to be revisited, not because it is wrong today. |
| Two IdP identities for one human defeat approver ≠ proposer entirely | `users` is unique on `(idp_issuer, idp_subject)`; there is no person entity; `RoleAdmin` holds both halves of break-glass | An identity-model decision, not a code fix. Already noted under F-64. |
| `login_attempts` accumulates plaintext `nonce` and `code_verifier` indefinitely | 00641 says the ops role purges it; nothing does, and `PurgeExpired` has no callers of any kind | A retention job, which is operations work with no home in this tree yet. |
| `internal/auth/httpmw`'s `RequireAuth`, `RequireRole`, `RequirePermission` and `RequireStepUp` all have zero non-test callers | The API enforces through `httpapi/authz.go` instead; `httpmw/doc.go` still advertises the dead set | Not a hole — the live path is the correct one — but a doc that names the wrong enforcement layer, and five exported guards that look live. |
| `signing_decisions` has a plain index on `attempt_id`, not a unique constraint | 00300; `findDecision` concedes it with `ORDER BY created_at DESC ... LIMIT 1` | Recorded under F-67. |

**Two things the audits confirmed are genuinely well covered**, recorded because
a clean answer is worth as much as a finding: the kill-switch never-blocked
classes are proven by exhaustive property tests over all 2¹² kind
subsets with a nil `Querier`, so a database read would panic rather than pass;
and `agent.Authority` immutability is asserted by running the full adversarial
prompt corpus through quarantine and re-checking the fingerprint, effect set,
tool set, budgets, stage and mode byte for byte.

## Findings deliberately NOT raised

Stated so their absence is a decision rather than an oversight:

- ~~**The chart of accounts is duplicated between Go and SQL.**~~ **RAISED, twice.** This entry said
  the duplication was "a maintainability risk, not a defect". It became both: F-43, where MARKETPLACE
  was high-risk in Go and not in SQL so the database evidence check never fired for it, and F-49,
  where the negative-balance policy was in Go and unenforced in SQL. Both halves are now compared by
  a test that drives each language's copy — `TestIntegration_GoAndSQLAgreeOnEveryCapabilitysRisk` and
  `TestIntegration_GoAndSQLAgreeOnTheChartOfAccounts`. The account-code and value-domain lists were
  already covered by `TestIntegration_GoAndSQLAgreeOnEveryOrderedDomainPair` and
  `TestIntegration_AccountDomainDerivationAgreesWithGo`; what remains uncovered is the transaction
  kinds, the credit origins and the finality transition table, and two of those carry comments
  claiming a test that does not exist.
- **Capability activation is not enforced in the database.** Migration 00710 says so explicitly and
  gives the reason: a gate is keyed by environment, and a connection carries no environment the
  application could not simply assert. Adding a session GUC would look like a control and be none.
- **`internal/compliance` is 248 lines with one test.** It is the thinnest financial package in the
  tree and was already flagged in the KEEP/MODIFY matrix as needing a real `ComplianceDecision`. It
  is under-built rather than wrong.
- **`internal/instruments` has 593 lines and one test function** for a package with status authority
  over what may be traded. Under-tested for its authority; not observed to be incorrect.
