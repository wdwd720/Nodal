# TECHNICAL EVIDENCE

**Platform status: NOT_READY. Capital authority: DISABLED.** Written against `80f616b` (main, 2026-10-08).

This document is the list of technical claims about this repository that can be defended, and for each one
what a skeptical reviewer needs to check it: the claim in one sentence that can be quoted, the code that
implements it, the tests that prove it (file and exact function), the mode it runs in, what it does **not**
mean, and one command that runs exactly those tests. A claim that is not in this document should not be made.
The last section lists the claims that must not be made at all.

The citations are checked rather than promised: this document is in the `inScope` list of
`test/docs/references_test.go`, so `go test ./test/docs/` fails if any test function named here stops
existing. When this document was written, every evidence command below was run once at `80f616b`, and every
test a claim cites was reported `PASS` (see "How this document was verified"). Claims revised since were re-run
when they changed: C3 and SEC2 at `e575c4b`, DA4 (the agent worker holding no key material) at `ba20d9c`,
the new test in M1's caveat at `da9f0c0`, OP2 (the restore drill) at `9db3f7c`.

## The one paragraph to read first

Every advanced capability in this repository (ADR-0020: stable asset, bridge and cross-chain capital, margin,
derivatives, equities, internal order books and matching, customer transfers, treasury, fee ledger,
performance fees, strategy marketplace, WASM strategy sandbox, delegated agent signing and withdrawals,
prediction markets, Buying Power V2, Risk Kernel V2) moves only value in domain `SIMULATED`, and the database
refuses to let that value mix with real capital (VD002), to let an advanced journal kind touch a non-simulated
account (SM001) or to let any party come out of such a journal unbalanced (SM002). `LIVE` is refused for every
advanced capability in code (`gates.DecideMode` answers `LIVE_NOT_IMPLEMENTED`) and cannot be written as an
economic intent's mode (a CHECK constraint). The V1 live gates (`LIVE_FUNDING`, `LIVE_MANUAL_TRADING`,
`LIVE_AGENT_TRADING`, `WITHDRAWALS`) are DISABLED, and activating one needs deployment configuration **and** a
persisted, approved, unexpired gate with evidence from distinct principals, so one environment variable is
never enough. Nothing in this repository has moved real money, real securities, real leverage or a real
prediction-market stake, and no transaction it built has been sent to a public blockchain: the V1 execution
worker refuses to start in every provider mode because no venue adapter is wired. "Devnet" in this document
means `internal/sandboxchain` and `internal/devnethost`: deterministic, in-process simulated chains whose keys
derive from one seed. Signatures on them are real Ed25519 over the exact message bytes; the chains are not a
public testnet.

## Modes

| Mode | What it means in this document |
|---|---|
| **Core** | V1 production-path code: ledger, capital, risk, settlement, reconciliation, audit. Exercised by tests against a real local PostgreSQL 16; never operated with customer funds; live capital DISABLED. |
| **Paper** | Simulated fills against a reference mark or the internal order book. Value domain `SIMULATED`. |
| **Sandbox** | A complete workflow against a deterministic provider inside the repository (the sandbox dollar, the sandbox oracle, the paper venue). Value domain `SIMULATED`. |
| **Simulated devnet** | `internal/sandboxchain` and `internal/devnethost`: in-process simulated chains with real Ed25519 signatures, blockhash windows, finality states, reorgs and injected faults. Not a public testnet. |
| **Local/test** | Static guards, property tests and tooling that run on a developer machine or in CI. Nothing deployed. |

## How to check any claim here

Prerequisites:

- Go 1.27 (`GOTOOLCHAIN=local`). The sandbox tests also need the `go` command on `PATH`, because they compile
  strategies to WASM.
- PostgreSQL 16 on 127.0.0.1:5433 with the roles in `docker/postgres/init/001_roles.sql`
  (`docker compose up -d --wait postgres`, or a local cluster carrying those roles). Nothing else: no claim
  here depends on Redis, Redpanda, ClickHouse, Temporal or MinIO. Tests that need those skip without them,
  and none of them is cited.
- `make` for `make advanced-demo`, and the PostgreSQL 16 client tools for the restore drill.

| To run | Command |
|---|---|
| A claim's database-backed tests | `go run ./scripts/inttest -pkg '^\./internal/<pkg>$' -run '^(<names>)$'` (a fresh, fully migrated database per package, dropped afterwards) |
| A claim's pure tests | `go test -count=1 -run '^(<names>)$' ./internal/<pkg>/` |
| Each test's own PASS or SKIP line | put `GOFLAGS=-v` in front of either command |
| The end-to-end scenarios | `make advanced-demo` (report in `dist/advanced-demo.json`) |
| The untagged security guards | `go test -count=1 ./test/security/...` (the HTTP probes: `go run ./scripts/inttest -pkg '^\./test/security$'`) |
| The performance targets | `make perf-advanced` (report in `dist/advanced-perf.md`; no claim here rests on it, see Benchmarks) |
| The declared-test census | `go run ./scripts/testcensus` and `go run ./scripts/testcensus -advanced` |
| This document's citations | `go test -count=1 ./test/docs/` |

Two details matter when reading an evidence command. `scripts/inttest` keeps a package only when one of its
integration-tagged tests matches `-run`, and then runs every test of that package, pure or database-backed,
that matches; a claim whose tests are all pure therefore uses `go test`. And nearly every `-run` expression
is an anchored list of the cited names, so the command runs exactly those tests; the few commands that run a
whole package or a family of tests (`^TestIDOR_`) run at least the ones their claim cites.

### How this document was verified

On 2026-10-08, on macOS arm64 (8 GB, shared with other work, load average between 3 and 28) against
PostgreSQL 16.15 on 127.0.0.1:5433, the evidence commands were run twice: first at `a05a432`, then at
`80f616b`, after the fee-ledger wiring, the delegated-authority hardening, the red-team fixes and demo
Scenarios E and F had merged. Each time every command ran once, one after another, with `GOFLAGS=-v`, and a
script compared the output with the claims: a claim held only if its command exited 0 and every test
function it cites printed `--- PASS`. No command printed `--- SKIP` for any test. At `80f616b` all 67 claims
held. The pass at `a05a432` held for 63 of the 66 claims then written, and the three that did not changed
this document:

- C3 cited `TestIntegration_V2ComposesEverySimulatedComponentAndIgnoresRealCapital`, which failed on a
  fixture that predated migration 00748 (a bridge journal no leg referenced, BR005). The fixture was repaired
  after `80f616b` and a real leg in flight now covers the bridge component
  (`TestIntegration_TheV2LoaderCountsALegInFlightOnlyAsItsHaircut`), so C3 cites both again.
- SEC2 cited the `internal/gates` database tests, which had stopped compiling (an import cycle through
  `internal/agent`). The cycle was removed after `80f616b` (`internal/agent/livegate`), and SEC2 cites them
  again.
- SB4's budget-concurrency test failed on lock timeouts under load and passed when re-run, and again at
  `80f616b` (SB4's caveat).

At `80f616b`, `make advanced-demo` reported 10 scenarios and 116 checks, all holding, and the restore drill
passed at schema version 763 (OP2). `make perf-advanced` was not run for this document: no claim rests on it
and the machine was not idle. The commit that adds this document sits on `6ad5297`, which differs from
`80f616b` only by `docs/reviews/SECURITY_RED_TEAM.md` and its line in `test/docs/references_test.go`; no code
or test changed between them.

## Claims by area

### 1. Ledger and accounting

#### L1 · The database enforces double entry

- **Claim:** Every journal transaction must balance per asset at COMMIT, enforced by a deferred PostgreSQL trigger that refuses an unbalanced or entry-less transaction even when it is inserted with raw SQL around the Go service, and posted rows can be neither updated nor deleted: a correction is a new compensating transaction that leaves the original byte for byte intact.
- **Implementation:** `migrations/00101_ledger.sql`, `internal/ledger/service.go`, `internal/ledger/validate.go`
- **Tests:** `internal/ledger/integration_test.go`: `TestIntegration_UnbalancedRejectedAtCommit`, `TestIntegration_NegativeBalanceRejected`, `TestLedger_UpdateDeleteForbidden`, `TestLedger_CorrectionIsNewTransaction`, `TestIntegration_AppRoleGrantsMatchFinancialModel`; `internal/ledger/property_test.go`: `TestProp_PostingBalanced`
- **Mode:** Core.
- **Caveat:** The refusals are measured against the application role `cp_app`. The table owner can disable a trigger (the tamper tests do exactly that); catching that is L3's detection, not prevention. Exercised on a local PostgreSQL 16, not under production load.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/ledger$' -run '^(TestIntegration_UnbalancedRejectedAtCommit|TestIntegration_NegativeBalanceRejected|TestLedger_UpdateDeleteForbidden|TestLedger_CorrectionIsNewTransaction|TestIntegration_AppRoleGrantsMatchFinancialModel|TestProp_PostingBalanced)$'`

#### L2 · Posting is idempotent

- **Claim:** A posting is idempotent on its key: replaying the same key and content returns the original transaction, the same key with different content is refused with `INVALID_IDEMPOTENCY_REUSE`, and twelve concurrent identical postings write exactly one transaction.
- **Implementation:** `internal/ledger/service.go`, `internal/ledger/hash.go`
- **Tests:** `internal/ledger/integration_test.go`: `TestIntegration_Idempotent`; `internal/ledger/property_test.go`: `TestProp_ContentHashOrderInvariant`
- **Mode:** Core.
- **Caveat:** It protects a retry of one business event. It relies on the caller deriving the key from that event (every subsystem does) and does not merge two different keys for the same event.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/ledger$' -run '^(TestIntegration_Idempotent|TestProp_ContentHashOrderInvariant)$'`

#### L3 · Concurrent postings stay exact, and a tampered balance is found

- **Claim:** Fifty goroutines posting randomly ordered transfers across four overlapping wallets produce exactly the expected balances with no drift, and `VerifyBalances` reports a balance row edited by the table owner at exactly that account, with the stored and the recomputed figures.
- **Implementation:** `internal/ledger/service.go`, `internal/ledger/verify.go`
- **Tests:** `internal/ledger/integration_test.go`: `TestIntegration_ConcurrentPostingsOverlappingAccounts`, `TestIntegration_VerifyBalances_DetectsTamper`
- **Mode:** Core.
- **Caveat:** Detection after the fact, not prevention. Fifty-way concurrency on one machine shows the locking is correct; it is not a throughput figure.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/ledger$' -run '^(TestIntegration_ConcurrentPostingsOverlappingAccounts|TestIntegration_VerifyBalances_DetectsTamper)$'`

#### L4 · Simulated value cannot reach real capital

- **Claim:** Independently of any Go code, the database refuses a simulated-only journal kind that touches a real asset (SM001), a simulated-only journal that balances per asset but not per party (SM002), a house or facility book holding a real asset, and a facility account on the wrong kind of facility (FA001), and SQL and Go agree for all 64 ordered pairs of value domains on which pairs may never move together, `SIMULATED` with every other domain among them (VD002).
- **Implementation:** `migrations/00731_advanced_financial_foundation.sql`, `migrations/00710_value_domains.sql`, `internal/valuedomain/isolation.go`, `internal/ledger/advanced.go`
- **Tests:** `internal/econ/integration_test.go`: `TestIntegration_ASimulatedOnlyKindCannotTouchRealCapital`, `TestIntegration_ATransferMustBalancePerParty`, `TestIntegration_HouseAndFacilityBooksCannotHoldRealValue`, `TestIntegration_FacilityBooksAreSegregatedByKind`; `internal/ledger/valuedomain_integration_test.go`: `TestIntegration_GoAndSQLAgreeOnEveryOrderedDomainPair`; `internal/ledger/valuedomainerrors_integration_test.go`: `TestIntegration_ValueDomainRefusalsAreClassified`; `internal/valuedomain/valuedomain_test.go`: `TestIsolation_SimulatedNeverMixesWithAnything`
- **Mode:** Core (the boundary every Paper, Sandbox and Simulated devnet path inherits).
- **Caveat:** For `SIMULATED`, VD002 is shown through the SQL function the trigger calls (`cp_domains_structurally_forbidden`, compared with Go over all 64 pairs), and the trigger itself is shown firing on raw SQL for another forbidden pair; the raw-SQL refusals specific to simulated value are SM001 and SM002. Every refusal is measured as `cp_app`; a database superuser can disable triggers.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(econ|ledger)$' -run '^(TestIntegration_ASimulatedOnlyKindCannotTouchRealCapital|TestIntegration_ATransferMustBalancePerParty|TestIntegration_HouseAndFacilityBooksCannotHoldRealValue|TestIntegration_FacilityBooksAreSegregatedByKind|TestIntegration_GoAndSQLAgreeOnEveryOrderedDomainPair|TestIntegration_ValueDomainRefusalsAreClassified)$' && go test -count=1 -run '^TestIsolation_SimulatedNeverMixesWithAnything$' ./internal/valuedomain/`

#### L5 · No binary floating point in a money path

- **Claim:** `scripts/lintfin` fails on any float type, float literal or float conversion in the production code of 34 financial packages (the core money path, the internal economy and 21 advanced packages), its own test shows it reporting planted findings, and the money types are integers: an asset quantity is an arbitrary-precision count of base units, a USD amount is integer cents and a price is an integer mantissa with a decimal scale.
- **Implementation:** `scripts/lintfin/main.go`, `internal/money/quantity.go`, `internal/money/usd.go`
- **Tests:** `scripts/lintfin/main_test.go`: `TestRun`; `internal/ledger/nofloat_test.go`: `TestNoFloatingPointInSource`; `internal/advancedjobs/nofloat_test.go`: `TestNoFloatingPointInSource`; `internal/advancedmetrics/nofloat_test.go`: `TestNoFloatingPointInSource`; `internal/sandboxchain/nofloat_test.go`: `TestNoFloatingPointInSource`; `internal/money/fuzz_test.go`: `FuzzParseQuantity`, `FuzzParseUSD`
- **Mode:** Local/test.
- **Caveat:** A static check of non-test production files; one line can be exempted with `// lintfin:allow <reason>`. `internal/advancedjobs`, `internal/advancedmetrics` and `internal/sandboxchain` are outside lintfin's list and carry their own source scans (cited). The fuzz targets run their seed corpus here, not a fuzzing campaign.
- **Evidence:** `go run ./scripts/lintfin && go test -count=1 ./scripts/lintfin/ && go test -count=1 -run '^(TestNoFloatingPointInSource|FuzzParseQuantity|FuzzParseUSD)$' ./internal/ledger/ ./internal/money/ ./internal/advancedjobs/ ./internal/advancedmetrics/ ./internal/sandboxchain/`

#### L6 · Every list kept twice is compared

- **Claim:** Where the chart of accounts, the owner types, the facility kinds, the simulated-only journal kinds and the enum CHECK constraints exist in both Go and SQL, parity tests fail as soon as the two copies differ, a new enum CHECK cannot appear unnoticed, and an account row cannot exempt itself from the negative-balance guard.
- **Implementation:** `internal/ledger/types.go`, `internal/ledger/advanced.go`, `migrations/00731_advanced_financial_foundation.sql`
- **Tests:** `internal/ledger/chartparity_integration_test.go`: `TestIntegration_GoAndSQLAgreeOnTheChartOfAccounts`, `TestIntegration_GoAndSQLAgreeOnOwnerTypeAndFacilityKind`, `TestIntegration_GoAndSQLAgreeOnSimulatedOnlyKinds`, `TestIntegration_AnAccountCannotExemptItselfFromTheNegativeBalanceGuard`; `test/integration/enums/enums_test.go`: `TestIntegration_EveryDeclaredEnumMatchesItsCheck`, `TestIntegration_NoEnumCheckAppearsUnnoticed`
- **Mode:** Local/test.
- **Caveat:** `TestIntegration_NoEnumCheckAppearsUnnoticed` still carries an explicit list of older enum CHECKs that have no Go twin; it fails on a new unpaired CHECK, not on the ones already listed.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./(internal/ledger|test/integration/enums)$' -run '^(TestIntegration_GoAndSQLAgreeOnTheChartOfAccounts|TestIntegration_GoAndSQLAgreeOnOwnerTypeAndFacilityKind|TestIntegration_GoAndSQLAgreeOnSimulatedOnlyKinds|TestIntegration_AnAccountCannotExemptItselfFromTheNegativeBalanceGuard|TestIntegration_EveryDeclaredEnumMatchesItsCheck|TestIntegration_NoEnumCheckAppearsUnnoticed)$'`

### 2. Capital and buying power

#### C1 · Reservations cannot oversubscribe

- **Claim:** One hundred concurrent $500 reservations against $10,000 succeed exactly 20 times and fail exactly 80 times with `INSUFFICIENT_BUYING_POWER` in every iteration, under read-committed row locks and under SERIALIZABLE, against a wallet and against an envelope, with no duplicate row, nothing negative and no drift; mixed reserve, consume, release and expire operations under contention conserve capital.
- **Implementation:** `internal/capital/service.go`, `internal/capital/envelope.go`, `migrations/00102_capital.sql`
- **Tests:** `internal/capital/torture_test.go`: `TestTorture_ReservationsNeverOversubscribe`, `TestTorture_MixedOperationsConserveCapital`; `internal/capital/property_test.go`: `TestProp_ReservationsNeverOversubscribe`, `TestProp_CapitalConserved`
- **Mode:** Core (the same reservation service holds every advanced capability's `SIMULATED` funds).
- **Caveat:** 25 iterations per mode by default (`CP_TORTURE_ITERATIONS` raises it; the CI target is 1,000), on one machine and one PostgreSQL. It proves the invariant under contention, not production throughput.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/capital$' -run '^(TestTorture_ReservationsNeverOversubscribe|TestTorture_MixedOperationsConserveCapital|TestProp_ReservationsNeverOversubscribe|TestProp_CapitalConserved)$'`

#### C2 · V1 buying power is bounded, and a haircut never adds to it

- **Claim:** For random snapshots under random policies, buying power never exceeds portfolio value, available never exceeds buying power, withdrawable never exceeds available or settled funding net of withdrawals, no figure is negative, reservations and holds never drive a figure below zero, a degraded stablecoin is marked to market rather than at par, and a haircut never increases buying power.
- **Implementation:** `internal/capital/buyingpower/engine.go`
- **Tests:** `internal/capital/buyingpower/engine_test.go`: `TestProp_BuyingPowerBounds`, `TestEvaluate_WithdrawableIsBoundedByFundingAndWithdrawals`, `TestEvaluate_ReservedAndHoldsNeverGoNegative`, `TestEvaluate_StablecoinDegradedMarksToMarket`
- **Mode:** Core.
- **Caveat:** Properties of the pure evaluator over generated inputs: the figures are only as good as the marks and the policy it is given.
- **Evidence:** `go test -count=1 -run '^(TestProp_BuyingPowerBounds|TestEvaluate_WithdrawableIsBoundedByFundingAndWithdrawals|TestEvaluate_ReservedAndHoldsNeverGoNegative|TestEvaluate_StablecoinDegradedMarksToMarket)$' ./internal/capital/buyingpower/`

#### C3 · Buying Power V2 is conservative

- **Claim:** Buying Power V2 counts against the owner: what is owed rounds up and what is held rounds down, so a sub-cent never shows as buying power; a deficit left by a bankrupt margin account blocks new risk and withdrawals until it is repaid; margin is read through the margin engine's own evaluation, and spot and derivatives margin are summed at the strictest rate; more reservations or more debt never raise buying power, in-flight bridge value counts zero under the default policy, unsettled funds can trade but are never withdrawable, a blocked account has no buying power, and no withdrawable amount leaves equity below maintenance.
- **Implementation:** `internal/capital/buyingpower/v2.go`, `internal/capital/buyingpower/v2_load.go`, `internal/capital/buyingpower/v2_combine.go`, `internal/margin/buyingpower.go`
- **Tests:** `internal/capital/buyingpower/v2_integration_test.go`: `TestIntegration_V2ComposesEverySimulatedComponentAndIgnoresRealCapital`, `TestIntegration_V2CountsWhatIsOwedAgainstTheOwner`; `internal/bridge/v2loader_integration_test.go`: `TestIntegration_TheV2LoaderCountsALegInFlightOnlyAsItsHaircut`; `internal/capital/buyingpower/v2_test.go`: `TestProp_V2NoWithdrawalBreachesMaintenance`, `TestProp_V2ReservationsAndDebtNeverIncreaseBuyingPower`, `TestProp_V2InFlightBridgeNeverCountsAtDefaultPolicy`, `TestV2_UnsettledFundsTradeButNeverWithdraw`, `TestV2_BlockedAccountHasNoBuyingPower`; `internal/capital/buyingpower/v2_combine_test.go`: `TestCombinedMarginReader_SumsAndKeepsTheStrictestRate`; `internal/margin/integration_controls_test.go`: `TestIntegration_BuyingPowerV2SeesTheMarginEngine`
- **Mode:** Paper and Sandbox.
- **Caveat:** The loader values only what it can mark: a holding without a fresh mark contributes nothing rather than a guess, so V2 can understate. Real capital in the same account is left out of every figure (the composition test seeds a real wallet beside the simulated components). Spot and derivatives margin are never offset against each other (D-054): conservative, not capital-efficient.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(capital/buyingpower|margin|bridge)$' -run '^(TestIntegration_V2ComposesEverySimulatedComponentAndIgnoresRealCapital|TestIntegration_TheV2LoaderCountsALegInFlightOnlyAsItsHaircut|TestIntegration_V2CountsWhatIsOwedAgainstTheOwner|TestProp_V2NoWithdrawalBreachesMaintenance|TestProp_V2ReservationsAndDebtNeverIncreaseBuyingPower|TestProp_V2InFlightBridgeNeverCountsAtDefaultPolicy|TestV2_UnsettledFundsTradeButNeverWithdraw|TestV2_BlockedAccountHasNoBuyingPower|TestCombinedMarginReader_SumsAndKeepsTheStrictestRate|TestIntegration_BuyingPowerV2SeesTheMarginEngine)$'`

### 3. Risk

#### R1 · The V1 risk kernel is a deterministic function

- **Claim:** The V1 risk kernel is a pure function: each case of the 87-case golden corpus gives its expected verdict and sorted reason codes, re-evaluating every case 1,000 times gives the same hash and byte-identical canonical JSON, concurrent evaluation shares no state, and composing policies never yields anything looser than the strictest input.
- **Implementation:** `internal/risk/evaluate.go`, `internal/risk/policy.go`, `internal/risk/testdata/cases`
- **Tests:** `internal/risk/golden_test.go`: `TestGolden_Risk`, `TestGolden_Determinism1000`, `TestGolden_DeterminismAcrossGoroutines`; `internal/risk/property_test.go`: `TestProp_ComposeStrictest`, `TestProp_ConstraintsNeverLooserThanPolicy`
- **Mode:** Core.
- **Caveat:** It proves what the function does, not that a given path consults it: F-34 records a period when no deployment did. A deployment needs a seeded policy (`scripts/riskpolicy`).
- **Evidence:** `go test -count=1 -run '^(TestGolden_Risk|TestGolden_Determinism1000|TestGolden_DeterminismAcrossGoroutines|TestProp_ComposeStrictest|TestProp_ConstraintsNeverLooserThanPolicy)$' ./internal/risk/`

#### R2 · Risk Kernel V2 fails closed and never blocks risk reduction

- **Claim:** Risk Kernel V2 runs 21 composable checks, exactly one per category: a stale, missing or future-dated mark refuses every risk-increasing intent but never a risk-reducing close, a missing required input section is a rejection rather than a pass, an ALLOW for a risk-increasing intent never leaves equity below maintenance, a disabled strategy version stops that version's new risk only, and every decision is byte-for-byte deterministic and stored append-only with its hash.
- **Implementation:** `internal/risk/advanced.go`, `internal/risk/store.go`
- **Tests:** `internal/risk/advanced_test.go`: `TestAdvanced_EveryCategoryHasExactlyOneCheck`, `TestAdvanced_StaleMarkBlocksNewRiskButNotReduction`, `TestAdvanced_MissingRequiredSectionsFailClosed`, `TestAdvanced_StrategyVersionDisableStopsThatVersionsNewRisk`, `TestProp_StaleOrMissingOracleNeverAllowsNewRisk`, `TestProp_RiskIncreasingNeverLeavesEquityBelowMaintenance`, `TestProp_AdvancedDeterminism`; `internal/risk/advanced_integration_test.go`: `TestIntegration_AdvancedDecisionsArePersistedImmutablyAndUntampered`
- **Mode:** Paper, Sandbox and Simulated devnet (every advanced subsystem calls it).
- **Caveat:** Each subsystem assembles the kernel's input; the decision is only as good as that snapshot, and the sandbox has one mark source.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/risk$' -run '^(TestAdvanced_EveryCategoryHasExactlyOneCheck|TestAdvanced_StaleMarkBlocksNewRiskButNotReduction|TestAdvanced_MissingRequiredSectionsFailClosed|TestAdvanced_StrategyVersionDisableStopsThatVersionsNewRisk|TestProp_StaleOrMissingOracleNeverAllowsNewRisk|TestProp_RiskIncreasingNeverLeavesEquityBelowMaintenance|TestProp_AdvancedDeterminism|TestIntegration_AdvancedDecisionsArePersistedImmutablyAndUntampered)$'`

#### R3 · Kill switches stop new risk without stopping liquidation, reconciliation or settlement

- **Claim:** Kill switches are a tested matrix, not a blanket: under a global kill no pre-trade or final new-risk decision is allowed, every matching switch kind refuses new risk, and risk reduction is refused only by the kinds that make a trade impossible (an instrument halt, a disabled chain or provider, an unknown kind) or where the policy says so; a switch raised between reservation and submission stops the order unsubmitted and releases its funds; and whatever the switches say, margin liquidation proceeds, reconciliation completes (under a kill and an account freeze) and an already submitted transaction settles.
- **Implementation:** `internal/killswitch/matrix.go`, `internal/risk/evaluate.go`, `internal/settlement/executor.go`
- **Tests:** `internal/risk/killswitch_test.go`: `TestKillSwitchMatrix`; `internal/risk/property_test.go`: `TestProp_NewRiskNeverAllowedUnderGlobalKill`; `internal/settlement/executor_test.go`: `TestExecutor_KillSwitchNeverBlocksPostSubmission`, `TestExecutor_KillSwitchActivatedBetweenReserveAndSubmit`; `internal/margin/integration_controls_test.go`: `TestIntegration_KillSwitchStopsNewRiskButNeverLiquidation`; `internal/reconciliation/engine_integration_test.go`: `TestIntegration_KillSwitchNeverStopsReconciliation`
- **Mode:** Core and Paper.
- **Caveat:** For delegated signing, a kill switch raised between the last pre-submit check and the simulated chain accepting the transaction is not caught (threat model 13). The settlement tests use in-memory doubles (X1).
- **Evidence:** `go test -count=1 -run '^(TestKillSwitchMatrix|TestProp_NewRiskNeverAllowedUnderGlobalKill|TestExecutor_KillSwitchNeverBlocksPostSubmission|TestExecutor_KillSwitchActivatedBetweenReserveAndSubmit)$' ./internal/risk/ ./internal/settlement/ && go run ./scripts/inttest -pkg '^\./internal/(margin|reconciliation)$' -run '^(TestIntegration_KillSwitchStopsNewRiskButNeverLiquidation|TestIntegration_KillSwitchNeverStopsReconciliation)$'`

#### R4 · A route that needs a person is never executed by automation

- **Claim:** When the legal router permits an action only after a person confirms it or an operator reviews it, the route is permitted but not executable now, and margin, equity orders, marketplace follows and prediction-market issuance each refuse it at the route, naming the step that was not taken: the intent is rejected and never routed, the risk kernel is never asked, and nothing is reserved, ordered, dispatched or moved.
- **Implementation:** `internal/settlement/compiler.go`, `internal/margin/ops.go`, `internal/equities/orders.go`, `internal/marketplace/follow.go`, `internal/predmarket/pipeline.go`
- **Tests:** `internal/margin/integration_hardening_test.go`: `TestIntegration_ARouteThatNeedsConfirmationIsNeverExecuted`; `internal/equities/integration_confirmation_test.go`: `TestIntegration_AnOrderWhoseRouteNeedsAPersonIsNeverExecuted`; `internal/marketplace/follow_confirmation_integration_test.go`: `TestIntegration_AFollowNeverDispatchesARouteThatNeedsAPerson`; `internal/marketplace/follow_route_test.go`: `TestRouteRefusalReasons_NameTheSkippedStep`; `internal/predmarket/route_confirmation_integration_test.go`: `TestIntegration_ARouteThatNeedsAPersonIsNeverExecuted`
- **Mode:** Paper and Sandbox.
- **Caveat:** Shown with a legal policy that demands confirmation or review; these subsystems have no confirmation flow, so such a route is refused rather than queued for a person.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(margin|equities|marketplace|predmarket)$' -run '^(TestIntegration_ARouteThatNeedsConfirmationIsNeverExecuted|TestIntegration_AnOrderWhoseRouteNeedsAPersonIsNeverExecuted|TestIntegration_AFollowNeverDispatchesARouteThatNeedsAPerson|TestRouteRefusalReasons_NameTheSkippedStep|TestIntegration_ARouteThatNeedsAPersonIsNeverExecuted)$'`

### 4. Exactly-once effects

#### X1 · The V1 settlement executor resumes from any crash without a second submission

- **Claim:** The 17-step settlement executor, crashed before, during and after each step (51 fault-injected runs) and restarted over the same durable state, completes with exactly one landed transaction, one submission, one fill, one ledger posting, one position update and a consumed reservation; a submission whose outcome is unknown is investigated and never resubmitted, and a new attempt is built only after the old one is proven absent from the chain.
- **Implementation:** `internal/settlement/executor.go`, `internal/settlement/executor_steps.go`, `internal/settlement/steps.go`
- **Tests:** `internal/settlement/executor_test.go`: `TestExecutor_ResumeAfterCrash_EveryStepBoundary`, `TestExecutor_ResumeAfterCrash_SubmitRunningNeverResubmits`, `TestExecutor_SubmitTimeoutLost_ProvenAbsent_NewAttemptOnce`, `TestExecutor_SubmitTimeout_Unresolved_ReconciliationRequired`, `TestExecutor_SubmitRejectedButLanded_IsUnknownNotFailure`; `internal/settlement/integration_test.go`: `TestIntegration_ExecutorOnRepositories`; `cmd/execution-worker/main_test.go`: `TestWire_RefusesWithoutAProviderBinding`
- **Mode:** Core code, exercised with test doubles.
- **Caveat:** The crash tests run against in-memory doubles of the repositories, the venue and the chain (`internal/settlement/settlementtest`); `TestIntegration_ExecutorOnRepositories` runs the executor on the real PostgreSQL repositories with a fake venue. No venue adapter is wired: `cmd/execution-worker` refuses to start in every provider mode (`TestWire_RefusesWithoutAProviderBinding`), so this executor has never submitted a transaction to Solana.
- **Evidence:** `go test -count=1 -run '^(TestExecutor_ResumeAfterCrash_EveryStepBoundary|TestExecutor_ResumeAfterCrash_SubmitRunningNeverResubmits|TestExecutor_SubmitTimeoutLost_ProvenAbsent_NewAttemptOnce|TestExecutor_SubmitTimeout_Unresolved_ReconciliationRequired|TestExecutor_SubmitRejectedButLanded_IsUnknownNotFailure|TestWire_RefusesWithoutAProviderBinding)$' ./internal/settlement/ ./cmd/execution-worker/ && go run ./scripts/inttest -pkg '^\./internal/settlement$' -run '^TestIntegration_ExecutorOnRepositories$'`

#### X2 · Every advanced effect is claimed exactly once

- **Claim:** An advanced economic effect is claimed exactly once under its key: 24 concurrent claims of one key resolve to one effect, a different request under the same key is refused, an effect that posts cannot be APPLIED without its journal and a journal backs at most one effect, an UNKNOWN outcome is resolved by observation and never retried, and the effect and intent status machines are enforced in SQL (EF001, EI001), where SQL and Go agree on every edge.
- **Implementation:** `internal/econ/effects.go`, `internal/econ/intent.go`, `migrations/00731_advanced_financial_foundation.sql`, `migrations/00734_economic_intent_status_machine.sql`
- **Tests:** `internal/econ/integration_test.go`: `TestIntegration_EffectIsClaimedExactlyOnceUnderConcurrency`, `TestIntegration_SameKeyDifferentRequestIsRefused`, `TestIntegration_ALedgerEffectIsNotAppliedWithoutItsJournal`, `TestIntegration_UnknownIsResolvedNotRetried`, `TestIntegration_TheEffectStatusMachineHoldsInSQL`, `TestIntegration_TheDatabaseRefusesAnIllegalIntentEdge`, `TestIntegration_SQLAndGoAgreeOnEffectTransitions`, `TestIntegration_SQLAndGoAgreeOnIntentTransitions`
- **Mode:** Paper, Sandbox and Simulated devnet.
- **Caveat:** The edge rules (EF001, EI001) and the journal constraints hold for any caller. The separate rule that every status change has a transition row (AU001) trusts a transaction-local flag that code holding the application credential can set (F-42, OPEN), so it stops mistakes, not a compromised application.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/econ$' -run '^(TestIntegration_EffectIsClaimedExactlyOnceUnderConcurrency|TestIntegration_SameKeyDifferentRequestIsRefused|TestIntegration_ALedgerEffectIsNotAppliedWithoutItsJournal|TestIntegration_UnknownIsResolvedNotRetried|TestIntegration_TheEffectStatusMachineHoldsInSQL|TestIntegration_TheDatabaseRefusesAnIllegalIntentEdge|TestIntegration_SQLAndGoAgreeOnEffectTransitions|TestIntegration_SQLAndGoAgreeOnIntentTransitions)$'`

#### X3 · An unknown chain outcome is reconciled, never resubmitted

- **Claim:** When a submission to a simulated chain times out, the stable-asset mint, the bridge lock, the delegated swap and the delegated withdrawal each record the outcome as unknown and settle it exactly once by observing the chain, never by sending it again: a transaction that landed completes, one that can still land is waited for, and one that never lands expires and releases its funds; while a mint's or a swap's outcome is unknown, that account takes no new risk of the same kind.
- **Implementation:** `internal/stableasset/observe.go`, `internal/bridge/flow.go`, `internal/delegation/reconcile.go`, `internal/devnet/faults.go`
- **Tests:** `internal/stableasset/mint_integration_test.go`: `TestIntegration_AnUnknownSubmissionIsReconciledNotRetried`, `TestIntegration_ADroppedSubmissionExpiresWithoutResubmission`, `TestIntegration_ATimedOutSubmissionThatNeverLandedFailsAfterExpiry`; `internal/bridge/bridge_integration_test.go`: `TestIntegration_AnUnknownLockSubmissionIsReconciledNotRetried`; `internal/delegation/controls_integration_test.go`: `TestIntegration_UnknownSubmissionIsReconciledNotRetried`; `internal/delegation/withdrawal_integration_test.go`: `TestIntegration_UnknownWithdrawalSubmissionIsReconciled`
- **Mode:** Simulated devnet.
- **Caveat:** The faults (timeout-but-land, drop, delay, reorg) are injected by the in-process chain, which allows injection only in LOCAL, TEST and DEV; a real network's behaviour beyond those faults is not modelled. Demo Scenario I runs the same path end to end (RE5).
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(stableasset|bridge|delegation)$' -run '^(TestIntegration_AnUnknownSubmissionIsReconciledNotRetried|TestIntegration_ADroppedSubmissionExpiresWithoutResubmission|TestIntegration_ATimedOutSubmissionThatNeverLandedFailsAfterExpiry|TestIntegration_AnUnknownLockSubmissionIsReconciledNotRetried|TestIntegration_UnknownSubmissionIsReconciledNotRetried|TestIntegration_UnknownWithdrawalSubmissionIsReconciled)$'`

#### X4 · A provider webhook has exactly one effect

- **Claim:** A provider webhook is verified before anything is stored or dispatched (a forged signature or a stale timestamp is refused, recorded as a security event and archives nothing), and any number of deliveries of one event, sequential or twelve at once, produce exactly one economic effect; a handler failure leaves the event reprocessable.
- **Implementation:** `internal/webhook/handler.go`, `internal/webhook/event.go`
- **Tests:** `internal/webhook/integration_test.go`: `TestProp_DuplicateWebhookOneEffect`, `TestIntegration_ConcurrentDuplicatesOneEffect`, `TestIntegration_ForgedSignature`, `TestIntegration_StaleTimestamp`, `TestIntegration_HandlerErrorIsFailedAndReprocessable`
- **Mode:** Core.
- **Caveat:** Signed with test secrets over recorded provider-format payloads. No live provider has delivered a webhook to this system: the Stripe onramp is application-gated (EB-003).
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/webhook$' -run '^(TestProp_DuplicateWebhookOneEffect|TestIntegration_ConcurrentDuplicatesOneEffect|TestIntegration_ForgedSignature|TestIntegration_StaleTimestamp|TestIntegration_HandlerErrorIsFailedAndReprocessable)$'`

### 5. Reconciliation and evidence

#### RE1 · V1 reconciliation recovers a crash after submission

- **Claim:** Reconciliation recovers a crash between a successful submission and its local record (restart, `SUBMISSION_UNKNOWN`, both chain observers agree on the transaction, then exactly one fill, one position change, the correct ledger entries and the unused reservation released) without resubmitting; replays converge without double-posting; an agent can never resolve a record, and a material resolution needs a real approval.
- **Implementation:** `internal/reconciliation/engine.go`, `internal/reconciliation/recovery.go`, `internal/reconciliation/resolver.go`
- **Tests:** `internal/reconciliation/crash_part49_test.go`: `TestIntegration_Part49_CrashRecovery`; `internal/reconciliation/property_integration_test.go`: `TestProp_ReplayConvergesAndNeverDoublePosts`; `internal/reconciliation/engine_integration_test.go`: `TestIntegration_AgentCanNeverResolve`, `TestIntegration_MaterialResolutionNeedsARealApproval`, `TestIntegration_ObserverDisagreementBlocks`, `TestIntegration_InternalDriftIsASEV1LedgerIntegrityViolation`
- **Mode:** Core.
- **Caveat:** The chain is a test double (`internal/chain/chaintest`) and the venue a fake; the Helius and RPC observers production would use are contract-tested against recorded fixtures only, never against a live node.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/reconciliation$' -run '^(TestIntegration_Part49_CrashRecovery|TestProp_ReplayConvergesAndNeverDoublePosts|TestIntegration_AgentCanNeverResolve|TestIntegration_MaterialResolutionNeedsARealApproval|TestIntegration_ObserverDisagreementBlocks|TestIntegration_InternalDriftIsASEV1LedgerIntegrityViolation)$'`

#### RE2 · Tamper-evident audit history with signed Merkle checkpoints

- **Claim:** Audit events form a per-stream SHA-256 hash chain (one hundred concurrent appends to one stream stay contiguous, and rows are immutable), and signed RFC 6962 Merkle checkpoints over it detect eleven kinds of tampering (an edited or deleted row, a truncated stream, a bit flip in or loss of the archived checkpoint, an altered root or coverage, a forged signature, rows re-hashed consistently) each under its own reason, while a key rotation is not reported as tampering.
- **Implementation:** `internal/audit/writer.go`, `internal/audit/verify.go`, `internal/proof/checkpoint.go`, `internal/proof/merkle.go`, `internal/proof/verify.go`, `internal/proof/keyset.go`
- **Tests:** `internal/audit/integration_test.go`: `TestIntegration_TamperIsDetectedAtExactSeq`, `TestIntegration_ConcurrentAppendsSameStream`, `TestIntegration_RowsAreImmutable`; `internal/proof/integration_test.go`: `TestIntegration_TamperingIsDetected`, `TestIntegration_KeyRotation`, `TestIntegration_CheckpointsChainAndCoverContiguously`; `internal/proof/merkle_test.go`: `TestMerkle_RFC6962KnownAnswers`, `TestMerkle_AdversarialProofsAreRejected`
- **Mode:** Core.
- **Caveat:** Tested with a local ECDSA P-256 signer and the PostgreSQL write-once archive. The KMS signer is tested only against a fake KMS client, and the S3 Object Lock test needs MinIO, which skips here and is not cited; neither has run against AWS. Tamper evidence detects; it does not prevent.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(audit|proof)$' -run '^(TestIntegration_TamperIsDetectedAtExactSeq|TestIntegration_ConcurrentAppendsSameStream|TestIntegration_RowsAreImmutable|TestIntegration_TamperingIsDetected|TestIntegration_KeyRotation|TestIntegration_CheckpointsChainAndCoverContiguously|TestMerkle_RFC6962KnownAnswers|TestMerkle_AdversarialProofsAreRejected)$'`

#### RE3 · Every simulated facility book reconciles

- **Claim:** Each simulated facility book has its own reconciliation, which `cmd/advanced-worker` runs on a schedule, and each one detects an out-of-band change (a stable-asset mint the books do not explain, a bridge lock, a prediction-market escrow divergence, a transfer journal no transfer explains, a tampered order-book journal) while counting supply wherever the ledger holds it; a periodic pass leaves one MATCHED record, not one per run.
- **Implementation:** `internal/stableasset/reconcile.go`, `internal/bridge/reconcile.go`, `internal/predmarket/reconcile.go`, `internal/transfers/reconcile.go`, `internal/advancedjobs/catalog.go`, `cmd/advanced-worker/main.go`
- **Tests:** `internal/stableasset/burn_integration_test.go`: `TestIntegration_ReconciliationDetectsAnOutOfBandMint`; `internal/stableasset/holdings_integration_test.go`: `TestIntegration_SupplyReconcilesWhereverTheUnitsAreHeld`; `internal/stableasset/upkeep_integration_test.go`: `TestIntegration_PeriodicSupplyReconciliationsLeaveOneMatchedRecord`; `internal/bridge/recon_integration_test.go`: `TestIntegration_BridgeReconciliationDetectsAnOutOfBandLock`; `internal/predmarket/db_integration_test.go`: `TestIntegration_ReconciliationDetectsAnEscrowDivergence`; `internal/transfers/bypass_integration_test.go`: `TestIntegration_ReconcileDetectsAJournalNoTransferExplains`; `internal/advancedjobs/markets_integration_test.go`: `TestIntegration_ATamperedBookJournalProducesAReconciliationRecord`, `TestIntegration_TransfersReconciliationRecordsADiscrepancyOnce`
- **Mode:** Paper, Sandbox and Simulated devnet.
- **Caveat:** A detected difference opens a reconciliation record (for the stable asset it also trips the breaker); resolving it is a person's decision. Detection is after the fact, on the worker's schedule.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(stableasset|bridge|predmarket|transfers|advancedjobs)$' -run '^(TestIntegration_ReconciliationDetectsAnOutOfBandMint|TestIntegration_SupplyReconcilesWhereverTheUnitsAreHeld|TestIntegration_PeriodicSupplyReconciliationsLeaveOneMatchedRecord|TestIntegration_BridgeReconciliationDetectsAnOutOfBandLock|TestIntegration_ReconciliationDetectsAnEscrowDivergence|TestIntegration_ReconcileDetectsAJournalNoTransferExplains|TestIntegration_ATamperedBookJournalProducesAReconciliationRecord|TestIntegration_TransfersReconciliationRecordsADiscrepancyOnce)$'`

#### RE4 · Evidence is written once, in the transaction it describes

- **Claim:** Every advanced economic effect writes canonical-JSON evidence into a write-once archive in the same transaction: replaying the same document yields the same digest, a different document under the same key is refused, the archive returns exactly the bytes it was given and refuses to hand back altered evidence, and its guard trigger refuses a rewrite or an early deletion even by the owning role.
- **Implementation:** `internal/econ/evidence.go`, `internal/proof/pgarchive.go`, `migrations/00730_the_evidence_archive_moves_into_the_database.sql`
- **Tests:** `internal/econ/integration_test.go`: `TestIntegration_EvidenceIsWriteOnceAndReplayable`; `internal/proof/pgarchive_test.go`: `TestIntegration_PgArchiveReturnsExactlyTheBytesItWasGiven`, `TestIntegration_PgArchiveRefusesToHandBackAlteredEvidence`, `TestIntegration_TheApplicationRoleCannotRewriteOrDeleteArchivedEvidence`, `TestIntegration_TheGuardTriggerRefusesTheOwnerToo`
- **Mode:** Core (the archive), used by every advanced mode.
- **Caveat:** The archive is a PostgreSQL table that emulates Object Lock retention; a database superuser can still drop the trigger. S3 Object Lock is designed for production (Terraform) and not exercised here.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(econ|proof)$' -run '^(TestIntegration_EvidenceIsWriteOnceAndReplayable|TestIntegration_PgArchiveReturnsExactlyTheBytesItWasGiven|TestIntegration_PgArchiveRefusesToHandBackAlteredEvidence|TestIntegration_TheApplicationRoleCannotRewriteOrDeleteArchivedEvidence|TestIntegration_TheGuardTriggerRefusesTheOwnerToo)$'`

#### RE5 · Ten end-to-end scenarios, 116 exact checks

- **Claim:** `make advanced-demo` provisions a fresh, fully migrated local database and runs ten deterministic scenarios through the real services (a transfer, stable-asset issuance and redemption, margin through to a covered deficit, an option to expiry, a matched trade with its audit proof, a strategy built to WASM and run in the sandbox to a paper fill, an agent's delegated swap approved, signed and settled, a bridge, a prediction market, an unknown submission), checking 116 exact assertions about the books; one failed check fails the run.
- **Implementation:** `scripts/advanceddemo/main.go`, `scripts/internal/sandboxworld/world.go`, `scripts/internal/sandboxworld/strategies.go`, `scripts/internal/sandboxworld/delegation.go`
- **Tests:** the checks of the scenario files `scripts/advanceddemo/scenario_0_transfer.go`, `scripts/advanceddemo/scenario_a_stable.go`, `scripts/advanceddemo/scenario_b_margin.go`, `scripts/advanceddemo/scenario_c_option.go`, `scripts/advanceddemo/scenario_d_matching.go`, `scripts/advanceddemo/scenario_e_strategy.go`, `scripts/advanceddemo/scenario_f_delegation.go`, `scripts/advanceddemo/scenario_g_bridge.go`, `scripts/advanceddemo/scenario_h_prediction.go`, `scripts/advanceddemo/scenario_i_failure.go` (not Go test functions)
- **Mode:** Paper, Sandbox and Simulated devnet.
- **Caveat:** A fixed fake clock and fixed identifiers make every run identical, so this shows the paths compose; it is not a randomized test. It refuses any database host but 127.0.0.1 or localhost.
- **Evidence:** `make advanced-demo`

### 6. Matching engine

#### MA1 · A deterministic price-time-priority matching engine

- **Claim:** `internal/matching` is a deterministic central limit order book (price-time priority, LIMIT and MARKET, GTC, IOC and FOK, post-only, reduce-only, self-trade prevention, cancel and replace) whose state after every command of a random sequence of up to 400 commands is uncrossed, FIFO within each price level and never overfilled, and is reproduced byte for byte (events, snapshot and state hash) by replaying the journal from empty or from any snapshot; a 120,000-command stream replays to the same state hash.
- **Implementation:** `internal/matching/book.go`, `internal/matching/sequencer.go`, `internal/matching/journal.go`, `internal/matching/snapshot.go`
- **Tests:** `internal/matching/property_test.go`: `TestProp_InvariantsAndReplay`, `TestProp_LongRunsReplayIncrementally`, `TestProp_SnapshotRestoreIsExact`; `internal/matching/load_test.go`: `TestLoad_100kOrders`; `internal/matching/book_test.go`: `TestPriceTimePriority`, `TestSelfTradePrevention`, `TestFOK`; `internal/matching/sequencer_test.go`: `TestSequencer_ConcurrentSubmitDeterminism`; `internal/matching/fuzz_test.go`: `FuzzBook`
- **Mode:** Local/test (the pure engine); it is the engine of the paper order book (MA2).
- **Caveat:** Determinism of the engine given its journal; the engine is single-writer behind a sequencer. `TestLoad_100kOrders` logs a commands-per-second figure for the machine it runs on, which is not a claim (Benchmarks). `FuzzBook` runs its seed corpus here.
- **Evidence:** `go test -count=1 -run '^(TestProp_InvariantsAndReplay|TestProp_LongRunsReplayIncrementally|TestProp_SnapshotRestoreIsExact|TestLoad_100kOrders|TestPriceTimePriority|TestSelfTradePrevention|TestFOK|TestSequencer_ConcurrentSubmitDeterminism|FuzzBook)$' ./internal/matching/`

#### MA2 · The persistent order book replays to its own state hash

- **Claim:** The persistent order book journals every command in PostgreSQL with the state hash after it: twelve goroutines submitting in their own transactions serialize into one journal that replays, from empty and from a snapshot, to exactly the live state hash; the journal proof also compares every replayed trade with its recorded row (sequence, price, quantity, parties, value, fees) and with the journal that row names, and names the field when the database owner has rewritten one; a cancel racing a fill has exactly one outcome and the ledger agrees with it; concurrent buys never reserve more than the wallet holds; across random trading customer wallets change only by the disclosed fees; and an engine that lost its memory, or ran ahead of a rolled-back transaction, rebuilds from the journal.
- **Implementation:** `internal/orders/service.go`, `internal/orders/engine.go`, `internal/orders/query.go`, `migrations/00742_order_book.sql`
- **Tests:** `internal/orders/book_integration_test.go`: `TestIntegration_DeterministicReplayYieldsByteEquivalentBookStateThroughTheDB`, `TestIntegration_FIFOWithinAPriceLevel`, `TestIntegration_ReplayOfASubmissionProducesNoSecondEffect`; `internal/orders/journal_trades_integration_test.go`: `TestIntegration_TheJournalProofComparesTheRecordedTradesAndTheirJournals`; `internal/orders/concurrency_integration_test.go`: `TestIntegration_ConcurrentSubmitsReplayToTheLiveStateHash`, `TestIntegration_CancelRaceHasExactlyOneOutcome`, `TestIntegration_ConcurrentBuysCannotOverspendTheWallet`; `internal/orders/property_integration_test.go`: `TestProp_NoCrossedRestingBookAfterEveryCommandThroughTheService`, `TestProp_QuantitiesAreConservedAndNoOrderOverfillsThroughTheService`, `TestProp_CustomerWalletsChangeOnlyByFeesAcrossRandomTrading`
- **Mode:** Paper.
- **Caveat:** The journal proof detects a rewritten book after the fact; it does not stop the database owner from rewriting one. One book is one serialization point (its row lock), so throughput per instrument is bounded (Benchmarks).
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/orders$' -run '^(TestIntegration_DeterministicReplayYieldsByteEquivalentBookStateThroughTheDB|TestIntegration_FIFOWithinAPriceLevel|TestIntegration_ReplayOfASubmissionProducesNoSecondEffect|TestIntegration_TheJournalProofComparesTheRecordedTradesAndTheirJournals|TestIntegration_ConcurrentSubmitsReplayToTheLiveStateHash|TestIntegration_CancelRaceHasExactlyOneOutcome|TestIntegration_ConcurrentBuysCannotOverspendTheWallet|TestProp_NoCrossedRestingBookAfterEveryCommandThroughTheService|TestProp_QuantitiesAreConservedAndNoOrderOverfillsThroughTheService|TestProp_CustomerWalletsChangeOnlyByFeesAcrossRandomTrading)$'`

#### MA3 · The database refuses an inconsistent book

- **Claim:** Around the Go service, the database refuses a command that is not the next link of the book's hash chain (OB001), a cursor that moves without its command (OB002), a snapshot of a state the journal did not record (OB005), any edit of journal history, an overfill, an illegal order transition, a self-trade, a house/customer crossing with no disclosed rule and a duplicate client order id; a crash after execution is completed by the sweep.
- **Implementation:** `migrations/00742_order_book.sql`, `internal/orders/crossing.go`, `internal/orders/upkeep.go`
- **Tests:** `internal/orders/bypass_integration_test.go`: `TestIntegration_TheDatabaseRefusesABrokenJournal`, `TestIntegration_TheDatabaseRefusesOverfillAndIllegalOrderStates`, `TestIntegration_TheDatabaseRefusesSelfTradesUnruledCrossingAndDuplicateClientIDs`, `TestIntegration_SQLAndGoAgreeOnTransitions`; `internal/orders/house_integration_test.go`: `TestIntegration_HouseCustomerCrossingIsRejectedByDefault`; `internal/orders/recovery_integration_test.go`: `TestIntegration_ACrashAfterExecuteIsCompletedBySweep`
- **Mode:** Paper.
- **Caveat:** Measured as the application role; the order status binding to its transition row has the F-42 limit described in X2.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/orders$' -run '^(TestIntegration_TheDatabaseRefusesABrokenJournal|TestIntegration_TheDatabaseRefusesOverfillAndIllegalOrderStates|TestIntegration_TheDatabaseRefusesSelfTradesUnruledCrossingAndDuplicateClientIDs|TestIntegration_SQLAndGoAgreeOnTransitions|TestIntegration_HouseCustomerCrossingIsRejectedByDefault|TestIntegration_ACrashAfterExecuteIsCompletedBySweep)$'`

### 7. Margin and liquidation

#### M1 · Liquidation is deterministic, reduce-only and bounded

- **Claim:** For random accounts, fill rates, venue availability and slippage, a liquidation plan is deterministic (the same input gives the same plan hash), only ever sells what the account holds, keeps every step's slippage under the cap, splits proceeds exactly into fee, repayment and excess, and ends within its step budget; the smallest sale that restores the account is chosen, the most under-margined account is liquidated first, a stale price makes liquidation wait rather than guess, and a kill switch never stops it.
- **Implementation:** `internal/margin/liquidation.go`, `internal/margin/ops_liquidation.go`, `internal/margin/ops_step.go`, `migrations/00740_margin.sql`, `migrations/00754_margin_hardening.sql`
- **Tests:** `internal/margin/property_test.go`: `TestProp_LiquidationIsDeterministicReduceOnlyAndTerminates`; `internal/margin/liquidation_test.go`: `TestPlan_SlippageIsCapped`, `TestApplySale_IsReduceOnly`, `TestPlan_PartialLiquidationIsTheSmallestThatRestores`, `TestPlan_TerminalAccountingIsDecidedBeforeTheStepBudget`; `internal/margin/integration_scenario_test.go`: `TestIntegration_StalePriceLiquidationIsRefused`, `TestIntegration_PartialLiquidationRestoresTheAccount`, `TestIntegration_LiquidationQueueTakesTheMostUnderMarginedFirst`; `internal/margin/integration_controls_test.go`: `TestIntegration_KillSwitchStopsNewRiskButNeverLiquidation`
- **Mode:** Paper and Sandbox.
- **Caveat:** Simulated leverage against a sandbox lending pool, tested with the sandbox dollar as the numeraire. Marks have one source and one operator can record a mark (threat model 5). A liquidation sale fills at the paper price, not against real market depth. A platform stable asset or a bridged twin can be neither a margin numeraire nor margin collateral: the supply rule of migration 00763 refuses the borrowing, interest and liquidation postings such an account would need, so opening the account or posting the collateral is refused up front with a coded reason (`TestIntegration_AMarginAccountNeverBooksASupplyControlledAsset`), and no liquidation can be left unable to complete.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/margin$' -run '^(TestProp_LiquidationIsDeterministicReduceOnlyAndTerminates|TestPlan_SlippageIsCapped|TestApplySale_IsReduceOnly|TestPlan_PartialLiquidationIsTheSmallestThatRestores|TestPlan_TerminalAccountingIsDecidedBeforeTheStepBudget|TestIntegration_StalePriceLiquidationIsRefused|TestIntegration_PartialLiquidationRestoresTheAccount|TestIntegration_LiquidationQueueTakesTheMostUnderMarginedFirst|TestIntegration_KillSwitchStopsNewRiskButNeverLiquidation)$'`

#### M2 · The same margin cannot be spent twice

- **Claim:** Concurrent borrows and concurrent leveraged buys cannot spend the same margin: sixteen concurrent borrows of 150 against 1,000 of collateral admit exactly six, and the pool's receivable equals what was lent; borrowing and leveraged buying are exactly once; a collateral release that would breach maintenance is refused; stale marks block new leverage; the equity identity always holds; and no withdrawal ever leaves equity below maintenance.
- **Implementation:** `internal/margin/ops_borrow.go`, `internal/margin/ops_spot.go`, `internal/margin/ops_collateral.go`, `internal/margin/evaluate.go`
- **Tests:** `internal/margin/integration_controls_test.go`: `TestIntegration_ConcurrentBorrowsCannotSpendTheSameMarginTwice`, `TestIntegration_ConcurrentLeveragedBuysCannotSpendTheSameMarginTwice`, `TestIntegration_BorrowAndLeveragedBuyAreExactlyOnce`, `TestIntegration_ReleaseThatWouldBreachMaintenanceIsRefused`, `TestIntegration_StaleMarksBlockNewLeverage`; `internal/margin/property_test.go`: `TestProp_NoWithdrawalBreachesMaintenance`, `TestProp_MarginAvailableNeverExceedsCapacity`, `TestProp_EquityIdentityHolds`
- **Mode:** Paper and Sandbox.
- **Caveat:** One margin account per customer account, and the pool lends only the numeraire. Spot and derivatives margin are kept apart, never netted (D-054).
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/margin$' -run '^(TestIntegration_ConcurrentBorrowsCannotSpendTheSameMarginTwice|TestIntegration_ConcurrentLeveragedBuysCannotSpendTheSameMarginTwice|TestIntegration_BorrowAndLeveragedBuyAreExactlyOnce|TestIntegration_ReleaseThatWouldBreachMaintenanceIsRefused|TestIntegration_StaleMarksBlockNewLeverage|TestProp_NoWithdrawalBreachesMaintenance|TestProp_MarginAvailableNeverExceedsCapacity|TestProp_EquityIdentityHolds)$'`

#### M3 · From funding to a covered deficit, with two people over the exceptions

- **Claim:** A whole margin life cycle (funding, leveraged buy, falling mark, margin call, liquidation, bankruptcy) ends with the insurance fund covering the deficit exactly once; free wallet cash is swept to the loans before bankruptcy, the covered account is restricted and its deficit repayable exactly once, the liquidation fee is platform revenue booked through the fee ledger (the database refuses an unbooked one), and a case in manual review leaves it only on one operator's proposal and a different operator's approval.
- **Implementation:** `internal/margin/ops_deficit.go`, `internal/margin/resolution.go`, `internal/margin/feebooking.go`, `migrations/00761_fee_bookings_wired.sql`
- **Tests:** `internal/margin/integration_scenario_test.go`: `TestIntegration_ScenarioB_FromFundingToACoveredDeficit`; `internal/margin/integration_hardening_test.go`: `TestIntegration_BorrowedCashInTheWalletIsSweptBeforeBankruptcy`, `TestIntegration_ACoveredDeficitRestrictsTheCustomerAccount`, `TestIntegration_ADeficitIsRepaidToTheFundExactlyOnce`, `TestIntegration_TheLiquidationFeeIsBookedAsPlatformFeeRevenue`; `internal/margin/integration_review_test.go`: `TestIntegration_AManualReviewCaseResumesOnlyWithTwoOperators`, `TestIntegration_TheDatabaseRefusesAManualReviewExitWithoutTwoOperators`; `internal/margin/integration_feeledger_test.go`: `TestIntegration_TheLiquidationFeeIsBookedThroughTheFeeLedger`, `TestIntegration_TheDatabaseRefusesAnUnbookedLiquidationFee`
- **Mode:** Paper and Sandbox.
- **Caveat:** The insurance fund and the lending pool are `SIMULATED` facility books, and deficit cover refuses when the fund is short. Two operators in the tests are two operator identities. Demo Scenario B runs the same path (RE5).
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/margin$' -run '^(TestIntegration_ScenarioB_FromFundingToACoveredDeficit|TestIntegration_BorrowedCashInTheWalletIsSweptBeforeBankruptcy|TestIntegration_ACoveredDeficitRestrictsTheCustomerAccount|TestIntegration_ADeficitIsRepaidToTheFundExactlyOnce|TestIntegration_TheLiquidationFeeIsBookedAsPlatformFeeRevenue|TestIntegration_AManualReviewCaseResumesOnlyWithTwoOperators|TestIntegration_TheDatabaseRefusesAManualReviewExitWithoutTwoOperators|TestIntegration_TheLiquidationFeeIsBookedThroughTheFeeLedger|TestIntegration_TheDatabaseRefusesAnUnbookedLiquidationFee)$'`

### 8. Derivatives

#### D1 · Futures and perpetual cash flows are exact and conserve

- **Claim:** Round-trip realized PnL is exact; funding on a balanced book pays exactly what it receives and a one-sided book is absorbed by the simulated clearing house; an internally matched book nets the clearing house to zero; variation margin runs once per interval and concurrent settlement posts once; and the database refuses any clearing flow that does not balance per party.
- **Implementation:** `internal/derivatives/pnl.go`, `internal/derivatives/clearing.go`, `internal/derivatives/runs.go`, `internal/derivatives/settle.go`, `migrations/00741_derivatives.sql`
- **Tests:** `internal/derivatives/pnl_test.go`: `TestProp_RoundTripRealizedPnLIsExact`, `TestProp_FundingConservesOnABalancedBook`; `internal/derivatives/integration_futures_test.go`: `TestIntegration_FuturesPnLIsExactThroughTheLedger`, `TestIntegration_VariationMarginRunsOncePerInterval`, `TestIntegration_ConcurrentSettlementPostsOnce`; `internal/derivatives/integration_clearing_test.go`: `TestIntegration_TheClearingHouseNetsToZeroForAMatchedBook`; `internal/derivatives/integration_funding_test.go`: `TestIntegration_FundingConservesOnABalancedBook`, `TestIntegration_FundingOnAOneSidedBookIsAbsorbedByTheClearingHouse`; `internal/derivatives/integration_db_test.go`: `TestIntegration_TheDatabaseRefusesAClearingFlowThatDoesNotBalancePerParty`
- **Mode:** Paper and Sandbox.
- **Caveat:** Cash-settled, 24x7 instruments with a whole-unit tick value only; funding rounds down once per contract per interval; every flow-producing operation serializes on one clearing-house lock.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/derivatives$' -run '^(TestProp_RoundTripRealizedPnLIsExact|TestProp_FundingConservesOnABalancedBook|TestIntegration_FuturesPnLIsExactThroughTheLedger|TestIntegration_VariationMarginRunsOncePerInterval|TestIntegration_ConcurrentSettlementPostsOnce|TestIntegration_TheClearingHouseNetsToZeroForAMatchedBook|TestIntegration_FundingConservesOnABalancedBook|TestIntegration_FundingOnAOneSidedBookIsAbsorbedByTheClearingHouse|TestIntegration_TheDatabaseRefusesAClearingFlowThatDoesNotBalancePerParty)$'`

#### D2 · Options settle exactly, at a price no caller chooses

- **Claim:** Option payoffs are exact integers that satisfy C − P = S − K at every grid point, FIFO assignment conserves contracts, and American exercise and expiry settle exactly; the expiry price comes from a documented rule over marks recorded by the expiry (marks only in order and on time), an override needs two operators and stays within a bound, terms are frozen while positions are open, and a close at a stale mark is refused and recorded.
- **Implementation:** `internal/derivatives/options.go`, `internal/derivatives/exercise.go`, `internal/derivatives/marks.go`, `internal/derivatives/override.go`, `migrations/00755_derivatives_hardening.sql`
- **Tests:** `internal/derivatives/options_test.go`: `TestProp_OptionPayoffIsExactAndPutCallParityHolds`, `TestProp_AssignFIFOConserves`; `internal/derivatives/integration_options_test.go`: `TestIntegration_OptionExpirySettlementIsExact`, `TestIntegration_AmericanExerciseAndFIFOAssignmentConserve`; `internal/derivatives/integration_hardening_test.go`: `TestIntegration_ASettlementIsPricedByTheRuleNeverByTheCaller`, `TestIntegration_AStaleExpiryWaitsForAnOverrideTwoOperatorsAgreeOn`, `TestIntegration_AMarkIsRecordedOnlyInOrderAndOnTime`, `TestIntegration_ASettlementReadsOnlyMarksRecordedByExpiry`, `TestIntegration_ACloseAtAStaleMarkIsRefusedAndRecorded`, `TestIntegration_TermsAreFrozenWhileTheContractHasOpenPositions`
- **Mode:** Paper and Sandbox.
- **Caveat:** Put–call parity here is the payoff identity at expiry, not a pricing claim. Marks come from one source per instrument in the sandbox.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/derivatives$' -run '^(TestProp_OptionPayoffIsExactAndPutCallParityHolds|TestProp_AssignFIFOConserves|TestIntegration_OptionExpirySettlementIsExact|TestIntegration_AmericanExerciseAndFIFOAssignmentConserve|TestIntegration_ASettlementIsPricedByTheRuleNeverByTheCaller|TestIntegration_AStaleExpiryWaitsForAnOverrideTwoOperatorsAgreeOn|TestIntegration_AMarkIsRecordedOnlyInOrderAndOnTime|TestIntegration_ASettlementReadsOnlyMarksRecordedByExpiry|TestIntegration_ACloseAtAStaleMarkIsRefusedAndRecorded|TestIntegration_TermsAreFrozenWhileTheContractHasOpenPositions)$'`

#### D3 · Option analytics are exact fixed point, checked against an oracle

- **Claim:** Black–Scholes–Merton prices, Greeks and implied volatility are computed in exact 40-digit fixed point with no floating point and are deterministic; every output is compared with an independent 320-bit `big.Float` oracle over random markets; and the analytics are labelled reference values that are never posted to the ledger.
- **Implementation:** `internal/quant/bsm.go`, `internal/quant/funcs.go`, `internal/quant/iv.go`, `internal/derivatives/analytics.go`
- **Tests:** `internal/quant/property_test.go`: `TestProp_BlackScholesVsOracle`, `TestProp_PutCallParity`, `TestProp_ImpliedVolRoundTrip`; `internal/quant/determinism_test.go`: `TestDeterminism`; `internal/quant/iv_test.go`: `TestImpliedVolRecoversGoldenVols`; `internal/quant/nofloat_test.go`: `TestNoFloatingPointInSource`, `TestImportsAreRestricted`; `internal/derivatives/integration_options_test.go`: `TestIntegration_AnalyticsAreReferenceValuesAtTheBoundary`
- **Mode:** Local/test (the pure package) and Paper.
- **Caveat:** A model value, not a market price: the analytics never price a trade or a settlement. The oracle uses `big.Float` inside the test only.
- **Evidence:** `go test -count=1 -run '^(TestProp_BlackScholesVsOracle|TestProp_PutCallParity|TestProp_ImpliedVolRoundTrip|TestDeterminism|TestImpliedVolRecoversGoldenVols|TestNoFloatingPointInSource|TestImportsAreRestricted)$' ./internal/quant/ && go run ./scripts/inttest -pkg '^\./internal/derivatives$' -run '^TestIntegration_AnalyticsAreReferenceValuesAtTheBoundary$'`

### 9. Equities

#### E1 · Corporate actions preserve value exactly and apply once

- **Claim:** Splits and reverse splits preserve each holder's economic value under the stated rounding policy, with cash in lieu; cash dividends pay holders of record only, exactly; a corporate action applies exactly once however often it is replayed; and an intent recorded before a split is never executed in the old units.
- **Implementation:** `internal/equities/corporate.go`, `internal/equities/corporate_service.go`, `migrations/00743_equities.sql`
- **Tests:** `internal/equities/corporate_test.go`: `TestProp_SplitPreservesEconomicValueUnderRoundingPolicy`, `TestProp_DividendIsExactAndNeverOverpays`; `internal/equities/integration_corporate_test.go`: `TestIntegration_SplitAndReverseSplitPreserveValueWithCashInLieu`, `TestIntegration_DividendPaysHoldersOfRecordOnlyExactAmounts`, `TestIntegration_CorporateActionReplayIsIdempotent`, `TestIntegration_AnIntentRecordedBeforeASplitIsNotExecutedInOldUnits`
- **Mode:** Paper.
- **Caveat:** Simulated equity instruments priced by sandbox reference marks: no broker, exchange or market-data feed, and nothing here is a security.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/equities$' -run '^(TestProp_SplitPreservesEconomicValueUnderRoundingPolicy|TestProp_DividendIsExactAndNeverOverpays|TestIntegration_SplitAndReverseSplitPreserveValueWithCashInLieu|TestIntegration_DividendPaysHoldersOfRecordOnlyExactAmounts|TestIntegration_CorporateActionReplayIsIdempotent|TestIntegration_AnIntentRecordedBeforeASplitIsNotExecutedInOldUnits)$'`

#### E2 · Paper equity orders follow session, settlement and disclosure rules

- **Claim:** Paper equity orders respect exchange sessions, holidays and halts; twelve clients submitting the same request produce one intent, one order, one reservation, one risk decision, one venue submission and one trade; concurrent sells cannot sell the same shares twice; sale proceeds are not withdrawable until T+1 settlement; every charged spread re-verifies from the stored disclosure and is booked through the fee ledger against the fill's own journal, so the spread audit lists it and the database refuses a fill without its booking (F2, FW001); a SPREAD_ENGINE kill switch refuses new orders whose fills would be charged and holds resting buys, never a resting sell; and the live adapter refuses unconditionally.
- **Implementation:** `internal/equities/orders.go`, `internal/equities/fills.go`, `internal/equities/feebooking.go`, `internal/equities/calendar.go`, `internal/equities/paper.go`, `internal/equities/adapter.go`, `migrations/00764_equity_fee_bookings.sql`
- **Tests:** `internal/equities/integration_orders_test.go`: `TestIntegration_SessionRulesAreEnforced`, `TestIntegration_TradingHaltBlocksNewOrders`, `TestIntegration_UnsettledProceedsAreNotWithdrawable`, `TestIntegration_AllChargedSpreadIsDiscoverableFromTheStoredDisclosure`, `TestIntegration_LiveCannotBeRequested`; `internal/equities/integration_concurrency_test.go`: `TestIntegration_OrderPlacementIsExactlyOnceUnderConcurrency`, `TestIntegration_ConcurrentSellsCannotSellTheSameSharesTwice`; `internal/equities/feeledger_integration_test.go`: `TestIntegration_EveryEquityFillIsBookedThroughTheFeeLedger`, `TestIntegration_TheSpreadAuditFindsEveryEquitySpread`, `TestIntegration_TheDatabaseRefusesAnUnbookedEquityFee`, `TestIntegration_ASpreadEngineKillSwitchStopsEquityBuysNotSells`; `internal/equities/paper_test.go`: `TestProviderMode_LiveIsRefusedUnconditionally`; `internal/equities/replay_test.go`: `TestContract_PaperAndReplayAdaptersAgree`
- **Mode:** Paper.
- **Caveat:** DAY orders only, one fill per order, base-tier fees, full-day holidays only, sells only from settled shares. The replay adapter replays a recorded conversation; it is not a broker.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/equities$' -run '^(TestIntegration_SessionRulesAreEnforced|TestIntegration_TradingHaltBlocksNewOrders|TestIntegration_UnsettledProceedsAreNotWithdrawable|TestIntegration_AllChargedSpreadIsDiscoverableFromTheStoredDisclosure|TestIntegration_LiveCannotBeRequested|TestIntegration_OrderPlacementIsExactlyOnceUnderConcurrency|TestIntegration_ConcurrentSellsCannotSellTheSameSharesTwice|TestIntegration_EveryEquityFillIsBookedThroughTheFeeLedger|TestIntegration_TheSpreadAuditFindsEveryEquitySpread|TestIntegration_TheDatabaseRefusesAnUnbookedEquityFee|TestIntegration_ASpreadEngineKillSwitchStopsEquityBuysNotSells|TestProviderMode_LiveIsRefusedUnconditionally|TestContract_PaperAndReplayAdaptersAgree)$'`

### 10. Stable asset

#### S1 · Never issued beyond its reserve or its cap

- **Claim:** The sandbox stable asset cannot be issued beyond its eligible reserve or its policy cap: the database refuses at COMMIT an issuance without full backing (SV004), supply above the cap (SV003) and an overpaying redemption (SV005); a reserve payment cannot be counted twice, concurrent mints keep every invariant, a reserve shortfall freezes issuance, and a redemption is the exact inverse of issuance; and its units exist only as its issuer issues them: written with plain SQL past every service, a faucet grant, a house book's funding, a withdrawal, a paper fill against no one and an issuance that credits more than it issues are each refused, and the holdings always equal what was issued.
- **Implementation:** `internal/stableasset/admission.go`, `internal/stableasset/reserve.go`, `internal/stableasset/mint.go`, `internal/stableasset/burn.go`, `migrations/00747_stable_assets.sql`, `migrations/00763_supply_controlled_assets.sql`
- **Tests:** `internal/stableasset/mint_integration_test.go`: `TestIntegration_TheDatabaseRefusesUnderReservedIssuanceAndOverpayingRedemption`, `TestIntegration_SupplyCannotExceedThePolicyCap`, `TestIntegration_ReserveCannotBeDoubleCounted`; `internal/stableasset/burn_integration_test.go`: `TestIntegration_ConcurrentMintsPreserveTheInvariants`, `TestIntegration_AReserveShortfallFreezesIssuance`, `TestIntegration_ARedemptionIsTheExactInverseOfIssuance`; `internal/stableasset/admission_prop_test.go`: `TestProp_SupplyNeverExceedsEligibleReserve`; `internal/stableasset/prop_integration_test.go`: `TestProp_IntegrationSupplyNeverExceedsEligibleReserve`; `internal/sandboxfund/supply_integration_test.go`: `TestIntegration_AStableAssetsUnitsExistOnlyAsItsIssuerIssuesThem`
- **Mode:** Sandbox and Simulated devnet.
- **Caveat:** The reserve is `SIMULATED` sandbox cash: there is no custodian, bank account or attestor behind it. This is not a stablecoin and not a claim on any dollar.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(stableasset|sandboxfund)$' -run '^(TestIntegration_TheDatabaseRefusesUnderReservedIssuanceAndOverpayingRedemption|TestIntegration_SupplyCannotExceedThePolicyCap|TestIntegration_ReserveCannotBeDoubleCounted|TestIntegration_ConcurrentMintsPreserveTheInvariants|TestIntegration_AReserveShortfallFreezesIssuance|TestIntegration_ARedemptionIsTheExactInverseOfIssuance|TestProp_SupplyNeverExceedsEligibleReserve|TestProp_IntegrationSupplyNeverExceedsEligibleReserve|TestIntegration_AStableAssetsUnitsExistOnlyAsItsIssuerIssuesThem)$'`

#### S2 · The issuer signs only the planned mint

- **Claim:** The mint key lives only in the issuer signer, which signs exactly the hashed mint plan after an inspector verifies the transaction is that plan (instruction, mint, recipient, amount, memo, signer set, fee payer), refuses every deviation and never reveals a key; every chain key derives from one configured seed, and a binary with another seed fails closed on an existing network.
- **Implementation:** `internal/stableasset/signer.go`, `internal/stableasset/inspect.go`, `internal/sandboxchain/sandboxchain.go`
- **Tests:** `internal/stableasset/signer_test.go`: `TestIssuerSigner_SignsExactlyThePlannedMint`, `TestInspection_RefusesEveryDeviationFromThePlan`, `TestSigners_NeverRevealTheirKeys`; `internal/stableasset/types_test.go`: `FuzzInspectMintTransaction`; `internal/sandboxchain/sandboxchain_test.go`: `TestDerive_TheDerivationIsPinned`, `TestDerive_EveryPurposeHasItsOwnKey`; `internal/sandboxchain/integration_test.go`: `TestIntegration_AWrongSeedFailsClosedOnAnExistingNetwork`, `TestIntegration_ASecondBinaryWithTheSameSeedSeesTheSameChain`
- **Mode:** Simulated devnet.
- **Caveat:** The signer is in-process and the process holds the seed: a compromise of that process could sign planned-looking mints, which supply reconciliation detects afterwards rather than prevents (threat model 1). An HSM or a remote signer is the production answer and is not built.
- **Evidence:** `go test -count=1 -run '^(TestIssuerSigner_SignsExactlyThePlannedMint|TestInspection_RefusesEveryDeviationFromThePlan|TestSigners_NeverRevealTheirKeys|FuzzInspectMintTransaction|TestDerive_TheDerivationIsPinned|TestDerive_EveryPurposeHasItsOwnKey)$' ./internal/stableasset/ ./internal/sandboxchain/ && go run ./scripts/inttest -pkg '^\./internal/sandboxchain$' -run '^(TestIntegration_AWrongSeedFailsClosedOnAnExistingNetwork|TestIntegration_ASecondBinaryWithTheSameSeedSeesTheSameChain)$'`

#### S3 · Signed, hash-linked reserve snapshots and a peg breaker

- **Claim:** Reserve snapshots are canonical JSON, Ed25519-signed, linked to their predecessor and append-only, and verification detects every kind of tampering; supply is reconciled against the chain and against every ledger code classified as a holding, so units posted as collateral still count; a cash equivalent is never valued above par, coverage rounds down and deviation rounds up; and a peg deviation beyond the threshold trips the breaker and pauses issuance, while a stale price only warns.
- **Implementation:** `internal/stableasset/snapshot.go`, `internal/stableasset/monitor.go`, `internal/stableasset/reconcile.go`, `internal/stableasset/value.go`, `internal/ledger/holding.go`
- **Tests:** `internal/stableasset/burn_integration_test.go`: `TestIntegration_SnapshotsAreSignedHashLinkedAndVerifiable`, `TestIntegration_APegDeviationTripsTheBreaker`; `internal/stableasset/snapshot_test.go`: `TestVerifySnapshotChain_DetectsEveryKindOfTampering`, `FuzzVerifySnapshot`; `internal/stableasset/holdings_integration_test.go`: `TestIntegration_SupplyReconcilesWhereverTheUnitsAreHeld`; `internal/stableasset/value_test.go`: `TestCapAtPar_ACashEquivalentIsNeverWorthMoreThanADollar`, `TestRatios_CoverageRoundsDownAndDeviationRoundsUp`; `internal/ledger/holding_test.go`: `TestHoldingCodes_EveryCodeIsClassifiedOnPurpose`
- **Mode:** Sandbox and Simulated devnet.
- **Caveat:** The snapshot signer has the issuer key's in-process exposure, and no independent party attests. A signed snapshot proves who published it and that its history was not rewritten, not that a reserve exists outside this database.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/stableasset$' -run '^(TestIntegration_SnapshotsAreSignedHashLinkedAndVerifiable|TestIntegration_APegDeviationTripsTheBreaker|TestVerifySnapshotChain_DetectsEveryKindOfTampering|FuzzVerifySnapshot|TestIntegration_SupplyReconcilesWhereverTheUnitsAreHeld|TestCapAtPar_ACashEquivalentIsNeverWorthMoreThanADollar|TestRatios_CoverageRoundsDownAndDeviationRoundsUp)$' && go test -count=1 -run '^TestHoldingCodes_EveryCodeIsClassifiedOnPurpose$' ./internal/ledger/`

### 11. Cross-chain

#### CC1 · The simulated chains are deterministic and tamper-evident

- **Claim:** The simulated devnet host records every chain-changing call as one event in an append-only, gapless, hash-chained PostgreSQL log written in its own transaction: replaying the log from the database reproduces the recorded state hash, an event inserted around Go with a valid hash chain is refused on replay, the database refuses a gap, concurrent submissions are journaled exactly once, two hosts on one history converge, and fault injection is refused outside LOCAL, TEST and DEV; keys are Ed25519 (checked against the RFC 8032 vector), tokens are conserved, and a finalized transaction never reverts under an injected reorg.
- **Implementation:** `internal/devnethost/host.go`, `internal/devnethost/store.go`, `internal/devnet/chain.go`, `internal/devnet/faults.go`, `internal/devnet/keys.go`, `migrations/00747_stable_assets.sql`
- **Tests:** `internal/devnethost/integration_test.go`: `TestIntegration_ReplayFromTheDatabaseReproducesTheStateHash`, `TestIntegration_AForgedEventIsRefusedOnReplay`, `TestIntegration_TheDatabaseRefusesAGapInTheHistory`, `TestIntegration_TheEventLogIsAppendOnly`, `TestIntegration_ConcurrentSubmissionsAreJournaledExactlyOnce`, `TestIntegration_TwoHostsShareOneChainHistory`; `internal/devnethost/host_test.go`: `TestHost_FaultInjectionIsForbiddenOutsideLocalTestDev`; `internal/devnet/keys_test.go`: `TestKeypair_RFC8032Vector`; `internal/devnet/faults_test.go`: `TestFault_FinalizedNeverReverts`; `internal/devnet/property_test.go`: `TestProp_TokenConservation`
- **Mode:** Simulated devnet.
- **Caveat:** A simulation of a chain's observable behaviour (blockhash windows, finality, reorgs, injected faults), not a consensus network and not a public testnet; nothing is broadcast anywhere.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/devnethost$' -run '^(TestIntegration_ReplayFromTheDatabaseReproducesTheStateHash|TestIntegration_AForgedEventIsRefusedOnReplay|TestIntegration_TheDatabaseRefusesAGapInTheHistory|TestIntegration_TheEventLogIsAppendOnly|TestIntegration_ConcurrentSubmissionsAreJournaledExactlyOnce|TestIntegration_TwoHostsShareOneChainHistory|TestHost_FaultInjectionIsForbiddenOutsideLocalTestDev)$' && go test -count=1 -run '^(TestKeypair_RFC8032Vector|TestFault_FinalizedNeverReverts|TestProp_TokenConservation)$' ./internal/devnet/`

#### CC2 · A bridge moves value exactly once across two simulated chains

- **Claim:** A bridge leg moves value once across two simulated chains and is never counted twice: the attester signs exactly the planned release, a duplicate release mints nothing twice, concurrent duplicates settle once, a reorg never reverts finality or double-counts, a failed lock or release restores the source, a delay creates no phantom capital, the database refuses bridge bookkeeping that breaks locked = outstanding + in flight, a twin's units exist only as its bridge releases them (a paper fill against no one is refused at COMMIT, SU002, and the bridge's minting accounts booked outside a release, SU001), and in-flight value counts toward buying power only at the policy haircut (zero by default).
- **Implementation:** `internal/bridge/flow.go`, `internal/bridge/leg.go`, `internal/bridge/attester.go`, `internal/bridge/buyingpower.go`, `migrations/00748_bridges.sql`, `migrations/00763_supply_controlled_assets.sql`
- **Tests:** `internal/bridge/bridge_integration_test.go`: `TestIntegration_ABridgeMovesValueOnceAndReconciles`, `TestIntegration_ADuplicateReleaseMintsNothingTwice`, `TestIntegration_AFailedLockRestoresTheSourceState`, `TestIntegration_AFailedReleaseIsRefundedAndRestoresTheSourceState`; `internal/bridge/reorg_integration_test.go`: `TestIntegration_AReorgNeverRevertsFinalityOrDoubleCounts`, `TestIntegration_ADelayedBridgeCreatesNoPhantomCapital`; `internal/bridge/recon_integration_test.go`: `TestIntegration_ConcurrentDuplicatesSettleOnce`, `TestIntegration_TheDatabaseRefusesBridgeBookkeepingBypasses`, `TestProp_IntegrationNoDoubleCounting`; `internal/bridge/attester_test.go`: `TestAttester_SignsExactlyTheExpectedRelease`; `internal/bridge/buyingpower_test.go`: `TestHaircut_InFlightValueCountsOnlyAtThePolicyFactor`, `TestProp_EachLegIsCountedOnceWhereItsValueIs`; `internal/sandboxfund/supply_integration_test.go`: `TestIntegration_ABridgedTwinsUnitsExistOnlyAsItsBridgeReleasesThem`
- **Mode:** Simulated devnet.
- **Caveat:** One attester key (no multi-party attestation), and a release stuck beyond its attempt budget goes to manual review. Two simulated chains, not real ones; no third-party bridge protocol is involved.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(bridge|sandboxfund)$' -run '^(TestIntegration_ABridgedTwinsUnitsExistOnlyAsItsBridgeReleasesThem|TestIntegration_ABridgeMovesValueOnceAndReconciles|TestIntegration_ADuplicateReleaseMintsNothingTwice|TestIntegration_AFailedLockRestoresTheSourceState|TestIntegration_AFailedReleaseIsRefundedAndRestoresTheSourceState|TestIntegration_AReorgNeverRevertsFinalityOrDoubleCounts|TestIntegration_ADelayedBridgeCreatesNoPhantomCapital|TestIntegration_ConcurrentDuplicatesSettleOnce|TestIntegration_TheDatabaseRefusesBridgeBookkeepingBypasses|TestProp_IntegrationNoDoubleCounting|TestAttester_SignsExactlyTheExpectedRelease|TestHaircut_InFlightValueCountsOnlyAtThePolicyFactor|TestProp_EachLegIsCountedOnceWhereItsValueIs)$'`

### 12. Transfers

#### T1 · Customer transfers cannot double-spend and conserve value

- **Claim:** Fifty concurrent transfers, each a tenth of the sender's balance, complete exactly ten times and leave the sum of all wallets unchanged; a hundred concurrent transfers across accounts keep every invariant; transfers and other spenders draw on one budget; a duplicate is idempotent and the sender cannot go negative; random sequences of transfers and reversals conserve customer wallets; and the database refuses a transfer journal that does not balance per party.
- **Implementation:** `internal/transfers/service.go`, `internal/transfers/postings.go`, `migrations/00744_transfers.sql`
- **Tests:** `internal/transfers/concurrency_integration_test.go`: `TestIntegration_ConcurrentTransfersCannotDoubleSpend`, `TestIntegration_HundredConcurrentTransfersPreserveInvariants`, `TestIntegration_TransfersAndOtherSpendersShareOneBudget`; `internal/transfers/transfer_integration_test.go`: `TestIntegration_ADuplicateTransferIsIdempotent`, `TestIntegration_TheSenderCannotGoNegative`; `internal/transfers/property_integration_test.go`: `TestProp_TransfersAndReversalsConserveCustomerWallets`; `internal/transfers/bypass_integration_test.go`: `TestIntegration_AP2PJournalThatDoesNotBalancePerPartyIsRefused`
- **Mode:** Sandbox (the `SIMULATED` sandbox dollar only).
- **Caveat:** Recipients must be customer accounts, and the compliance screen blocks only a configured list; no sanctions-screening provider is integrated. No money is transmitted.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/transfers$' -run '^(TestIntegration_ConcurrentTransfersCannotDoubleSpend|TestIntegration_HundredConcurrentTransfersPreserveInvariants|TestIntegration_TransfersAndOtherSpendersShareOneBudget|TestIntegration_ADuplicateTransferIsIdempotent|TestIntegration_TheSenderCannotGoNegative|TestProp_TransfersAndReversalsConserveCustomerWallets|TestIntegration_AP2PJournalThatDoesNotBalancePerPartyIsRefused)$'`

#### T2 · A reversal is a two-operator compensation

- **Claim:** A completed transfer is reversed only by a compensating journal under dual control: a proposer cannot approve their own reversal, twelve concurrent approvals execute it exactly once, reversing needs its permission and a recent step-up, the database refuses a self-approved reversal or one without an approved two-operator proposal, a reversal fails closed when the funds are gone, and history is never edited.
- **Implementation:** `internal/transfers/reversal.go`, `migrations/00758_transfer_reversal_dual_control.sql`
- **Tests:** `internal/transfers/dualcontrol_integration_test.go`: `TestIntegration_AProposerCannotApproveTheirOwnReversal`, `TestIntegration_TwoOperatorsReverseExactlyOnceUnderConcurrency`, `TestIntegration_ReversalNeedsPermissionAndStepUp`; `internal/transfers/dualcontrol_bypass_integration_test.go`: `TestIntegration_TheDatabaseRefusesASelfApprovedReversal`, `TestIntegration_TheDatabaseRefusesAReversalWithoutAnApprovedTwoOperatorProposal`, `TestIntegration_ReversalHistoryIsAppendOnly`; `internal/transfers/reversal_integration_test.go`: `TestIntegration_AReversalCompensatesAndNeverEditsHistory`, `TestIntegration_AReversalFailsClosedWhenTheFundsAreGone`
- **Mode:** Sandbox.
- **Caveat:** Two operators in the tests are two operator identities; separating duties between real people is an operating control, not code.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/transfers$' -run '^(TestIntegration_AProposerCannotApproveTheirOwnReversal|TestIntegration_TwoOperatorsReverseExactlyOnceUnderConcurrency|TestIntegration_ReversalNeedsPermissionAndStepUp|TestIntegration_TheDatabaseRefusesASelfApprovedReversal|TestIntegration_TheDatabaseRefusesAReversalWithoutAnApprovedTwoOperatorProposal|TestIntegration_ReversalHistoryIsAppendOnly|TestIntegration_AReversalCompensatesAndNeverEditsHistory|TestIntegration_AReversalFailsClosedWhenTheFundsAreGone)$'`

#### T3 · Sandbox funding is capped, idempotent and simulated-only

- **Claim:** The sandbox faucet funds only simulated assets; it is on in TEST and off in STAGING and PROD unless deployment configuration enables it; it funds only for the account's owner, a service holding `sandbox:fund` or the system; concurrent grants cannot exceed its rolling cap; a grant is idempotent; the global kill switch stops it; it refuses prediction-market shares; and it never grants an asset whose supply a controller owns (a platform stable asset, a bridged twin), refusing it by name before anything is written, while around the faucet the database refuses the grant's journal (SU001).
- **Implementation:** `internal/sandboxfund/faucet.go`, `internal/sandboxfund/policy.go`, `internal/econ/sandboxdollar.go`, `migrations/00763_supply_controlled_assets.sql`
- **Tests:** `internal/sandboxfund/integration_test.go`: `TestIntegration_ConcurrentGrantsCannotExceedTheCap`, `TestIntegration_TheFaucetEnforcesItsRollingCap`, `TestIntegration_TheFaucetRefusesNonSimulatedAssets`, `TestIntegration_TheFaucetIsGatedOnTheEnvironment`, `TestIntegration_OnlyTheOwnerAPermittedServiceOrTheSystemMayFund`, `TestIntegration_TheFaucetIsIdempotent`, `TestIntegration_TheGlobalKillSwitchStopsTheFaucet`; `internal/sandboxfund/predshare_integration_test.go`: `TestIntegration_TheFaucetRefusesAPredictionMarketShare`; `internal/sandboxfund/supply_integration_test.go`: `TestIntegration_TheFaucetNeverGrantsAnAssetWhoseSupplyAControllerOwns`; `internal/sandboxfund/policy_test.go`: `TestProp_GrantsNeverExceedTheCap`
- **Mode:** Sandbox.
- **Caveat:** The faucet mints play money by construction; its caps protect the sandbox books' meaning, not anyone's funds.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/sandboxfund$' -run '^(TestIntegration_ConcurrentGrantsCannotExceedTheCap|TestIntegration_TheFaucetEnforcesItsRollingCap|TestIntegration_TheFaucetRefusesNonSimulatedAssets|TestIntegration_TheFaucetIsGatedOnTheEnvironment|TestIntegration_OnlyTheOwnerAPermittedServiceOrTheSystemMayFund|TestIntegration_TheFaucetIsIdempotent|TestIntegration_TheGlobalKillSwitchStopsTheFaucet|TestIntegration_TheFaucetRefusesAPredictionMarketShare|TestIntegration_TheFaucetNeverGrantsAnAssetWhoseSupplyAControllerOwns|TestProp_GrantsNeverExceedTheCap)$'`

### 13. Treasury, fees and performance fees

#### F1 · Every fee is disclosed, capped and recomputable

- **Claim:** Fee quotes are deterministic and conserve every unit; each component (spread, platform, strategy, venue, network) is its own line; default rounding never charges more than the exact amount and platform-favourable rounding needs an explicit opt-in and is always disclosed; no fee exceeds its cap or goes negative; and changing any amount of a stored quote by one base unit makes its verification fail.
- **Implementation:** `internal/fees/quote.go`, `internal/fees/schedule.go`, `internal/fees/verify.go`, `internal/fees/disclosure.go`
- **Tests:** `internal/fees/quote_prop_test.go`: `TestProp_QuoteConservesEveryUnit`, `TestProp_QuoteIsDeterministicAndItsRecordVerifies`, `TestProp_DefaultRoundingNeverChargesMoreThanExact`, `TestProp_PlatformFavourableModesAreAlwaysDisclosed`, `TestProp_FeesNeverExceedTheirCaps`, `TestProp_AnyOneUnitTamperIsDetected`; `internal/fees/verify_test.go`: `TestVerifyQuote_DetectsTampering`; `internal/fees/schedule_test.go`: `TestSchedule_PlatformFavourableRoundingNeedsExplicitOptIn`; `internal/fees/revenue_test.go`: `TestRevenueSplit_BooksEachKindSeparately`
- **Mode:** Local/test (the pure engine that Paper and Sandbox charges use).
- **Caveat:** Properties of the engine over generated schedules and orders; what a deployment charges is the schedule it publishes (F2).
- **Evidence:** `go test -count=1 -run '^(TestProp_QuoteConservesEveryUnit|TestProp_QuoteIsDeterministicAndItsRecordVerifies|TestProp_DefaultRoundingNeverChargesMoreThanExact|TestProp_PlatformFavourableModesAreAlwaysDisclosed|TestProp_FeesNeverExceedTheirCaps|TestProp_AnyOneUnitTamperIsDetected|TestVerifyQuote_DetectsTampering|TestSchedule_PlatformFavourableRoundingNeedsExplicitOptIn|TestRevenueSplit_BooksEachKindSeparately)$' ./internal/fees/`

#### F2 · Every fee the subsystems charge is booked in the fee ledger, or the transaction fails

- **Claim:** Every fee and spread charged by an internal-book trade, a paper-venue fill, a margin liquidation step or an equity fill is booked through the fee ledger in the charging transaction, component by component against the fill's own journal and effect and priced by a stored disclosure that re-verifies against the schedule version in force -- for an equity fill, byte for byte the disclosure the fill stores and the customer sees -- and the database refuses at COMMIT a trade, fill or step that charges a fee the fee ledger did not book (FW001); a replayed fill books its fees once, a refused or rolled-back fill books nothing, schedules are versioned, immutable and hashed, every charged spread is discoverable from the audit record, and the house is never charged.
- **Implementation:** `internal/feeledger/booking.go`, `internal/orders/feebooking.go`, `internal/margin/feebooking.go`, `internal/equities/feebooking.go`, `migrations/00746_fee_ledger.sql`, `migrations/00761_fee_bookings_wired.sql`, `migrations/00764_equity_fee_bookings.sql`
- **Tests:** `internal/orders/feeledger_integration_test.go`: `TestIntegration_EveryInternalBookFeeIsBookedThroughTheFeeLedger`, `TestIntegration_AReplayedFillBooksItsFeesOnce`, `TestIntegration_TheDatabaseRefusesAnUnbookedFee`; `internal/orders/paper_feeledger_integration_test.go`: `TestIntegration_EveryPaperFillIsBookedThroughTheFeeLedger`; `internal/margin/integration_feeledger_test.go`: `TestIntegration_TheLiquidationFeeIsBookedThroughTheFeeLedger`, `TestIntegration_TheDatabaseRefusesAnUnbookedLiquidationFee`; `internal/feeledger/fees_integration_test.go`: `TestIntegration_ChargesAreBookedComponentByComponentExactlyOnce`, `TestIntegration_SchedulesAreVersionedImmutableAndHashed`, `TestIntegration_QuoteDisclosuresAreStoredAndVerifiable`, `TestIntegration_EveryChargedSpreadIsDiscoverableFromTheAuditRecord`; `internal/treasury/feeledger_integration_test.go`: `TestIntegration_TheHouseIsChargedNothingAndItsCustomerCounterpartyIsBooked`; `internal/equities/feeledger_integration_test.go`: `TestIntegration_EveryEquityFillIsBookedThroughTheFeeLedger`, `TestIntegration_AReplayedEquityFillBooksItsChargesOnce`, `TestIntegration_ARefusedOrRolledBackEquityFillBooksNothing`, `TestIntegration_TheDatabaseRefusesAnUnbookedEquityFee`, `TestIntegration_TheSpreadAuditFindsEveryEquitySpread`
- **Mode:** Paper and Sandbox.
- **Caveat:** FW001 binds exactly those four charge points (book trades, paper fills, liquidation steps and equity fills). An equity schedule is fixed by the deployment's configuration and published under a key naming its settlement asset and hash; another version an operator publishes under that key refuses the fill instead of repricing it. Rows written before migrations 00761 and 00764 are not re-checked.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(orders|margin|feeledger|treasury|equities)$' -run '^(TestIntegration_EveryInternalBookFeeIsBookedThroughTheFeeLedger|TestIntegration_AReplayedFillBooksItsFeesOnce|TestIntegration_TheDatabaseRefusesAnUnbookedFee|TestIntegration_EveryPaperFillIsBookedThroughTheFeeLedger|TestIntegration_TheLiquidationFeeIsBookedThroughTheFeeLedger|TestIntegration_TheDatabaseRefusesAnUnbookedLiquidationFee|TestIntegration_ChargesAreBookedComponentByComponentExactlyOnce|TestIntegration_SchedulesAreVersionedImmutableAndHashed|TestIntegration_QuoteDisclosuresAreStoredAndVerifiable|TestIntegration_EveryChargedSpreadIsDiscoverableFromTheAuditRecord|TestIntegration_TheHouseIsChargedNothingAndItsCustomerCounterpartyIsBooked|TestIntegration_EveryEquityFillIsBookedThroughTheFeeLedger|TestIntegration_AReplayedEquityFillBooksItsChargesOnce|TestIntegration_ARefusedOrRolledBackEquityFillBooksNothing|TestIntegration_TheDatabaseRefusesAnUnbookedEquityFee|TestIntegration_TheSpreadAuditFindsEveryEquitySpread)$'`

#### F3 · Performance fees only on gains above the high-water mark

- **Claim:** A performance fee crystallizes only on a gain above the high-water mark: a losing period charges nothing, a gain below the mark charges nothing, deposits generate no performance, a withdrawal does not erase the accrual, the mark chains across periods, rounding is exact, a crystallization is reversed only by compensation, and concurrent accruals and crystallizations happen exactly once.
- **Implementation:** `internal/fees/performance.go`, `internal/feeledger/performance.go`, `migrations/00746_fee_ledger.sql`
- **Tests:** `internal/feeledger/performance_integration_test.go`: `TestIntegration_ALosingPeriodChargesNoFee`, `TestIntegration_AGainBelowTheHighWaterMarkChargesNoFee`, `TestIntegration_DepositsDoNotGeneratePerformance`, `TestIntegration_AWithdrawalDoesNotEraseTheAccrual`, `TestIntegration_MultiPeriodCrystallizationChainsTheHighWaterMark`, `TestIntegration_PerformanceFeeRoundsExactly`, `TestIntegration_ACrystallizationIsReversedByCompensation`, `TestIntegration_ConcurrentAccrualsAndCrystallizationsAreExactlyOnce`; `internal/fees/performance_prop_test.go`: `TestProp_FlowsNeverCreatePerformance`, `TestProp_PerformanceFeeBounds`, `TestProp_AttributionSumsExactlyToTheFee`
- **Mode:** Paper and Sandbox.
- **Caveat:** Period NAVs are supplied by the caller (threat model 9): the engine guarantees the arithmetic and the period chain, not where a NAV came from.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/feeledger$' -run '^(TestIntegration_ALosingPeriodChargesNoFee|TestIntegration_AGainBelowTheHighWaterMarkChargesNoFee|TestIntegration_DepositsDoNotGeneratePerformance|TestIntegration_AWithdrawalDoesNotEraseTheAccrual|TestIntegration_MultiPeriodCrystallizationChainsTheHighWaterMark|TestIntegration_PerformanceFeeRoundsExactly|TestIntegration_ACrystallizationIsReversedByCompensation|TestIntegration_ConcurrentAccrualsAndCrystallizationsAreExactlyOnce)$' && go test -count=1 -run '^(TestProp_FlowsNeverCreatePerformance|TestProp_PerformanceFeeBounds|TestProp_AttributionSumsExactlyToTheFee)$' ./internal/fees/`

#### F4 · The house trades only its own segregated simulated book

- **Claim:** The treasury's house books hold only `SIMULATED` value in their own HOUSE segment: no customer fund is reachable by a house strategy (even around the treasury, the database refuses a house order that is not an applied treasury action, TY007), only the house service or an operator acts for the house, eight concurrent house buys against a cap with room for five admit exactly five, a loss limit stops new risk, a house/customer crossing needs a disclosed sandbox rule, and house actions are exactly once.
- **Implementation:** `internal/treasury/service.go`, `internal/treasury/limits.go`, `internal/treasury/crossing.go`, `migrations/00745_treasury.sql`, `migrations/00759_house_orders_through_treasury.sql`
- **Tests:** `internal/treasury/limits_integration_test.go`: `TestIntegration_NoCustomerFundsAreAvailableToTheHouseStrategy`, `TestIntegration_ConcurrentHouseOrdersCannotBreachALimitTogether`, `TestIntegration_TreasuryLossLimitStopsNewRisk`, `TestIntegration_KillSwitchPreventsNewHouseOrders`; `internal/treasury/controls_integration_test.go`: `TestIntegration_HouseAndCustomerLedgersAreSegregated`, `TestIntegration_OnlyTheHouseServiceOrAnOperatorActsForTheHouse`, `TestIntegration_HouseActionsAreExactlyOnce`; `internal/treasury/activities_integration_test.go`: `TestIntegration_HouseCustomerCrossingNeedsADisclosedSandboxRule`
- **Mode:** Paper and Sandbox.
- **Caveat:** Principal trading against real customers stays forbidden for real value (ADR-0019, amended for simulated modes only by ADR-0020); this is the simulated equivalent.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/treasury$' -run '^(TestIntegration_NoCustomerFundsAreAvailableToTheHouseStrategy|TestIntegration_ConcurrentHouseOrdersCannotBreachALimitTogether|TestIntegration_TreasuryLossLimitStopsNewRisk|TestIntegration_KillSwitchPreventsNewHouseOrders|TestIntegration_HouseAndCustomerLedgersAreSegregated|TestIntegration_OnlyTheHouseServiceOrAnOperatorActsForTheHouse|TestIntegration_HouseActionsAreExactlyOnce|TestIntegration_HouseCustomerCrossingNeedsADisclosedSandboxRule)$'`

### 14. Strategy marketplace and WASM sandbox

#### SB1 · Untrusted strategy code cannot escape the WASM sandbox

- **Claim:** A strategy is scanned, compiled to WASM in an environment that carries nothing from the parent, and run in wazero with no filesystem, network, processes or environment: compiled Go attack fixtures that try eight filesystem escapes, six network operations, process creation, a goroutine bomb, an infinite loop, a memory bomb and oversized stdout and stderr are each refused or killed, a host secret planted in the test process is invisible, a module whose digest does not match is refused, and a build is stopped at its time, memory and disk bounds, every outcome of a build leaving its work area as it found it.
- **Implementation:** `internal/sandboxrt/runtime.go`, `internal/sandboxrt/builder.go`, `internal/sandboxrt/workarea.go`, `internal/sandboxrt/scan.go`, `internal/sandboxrt/limits.go`
- **Tests:** `internal/sandboxrt/runtime_test.go`: `TestSandbox_FilesystemEscapeAttemptFails`, `TestSandbox_NetworkAttemptFails`, `TestSandbox_ForkBombBlocked`, `TestSandbox_CPUInfiniteLoopKilled`, `TestSandbox_MemoryBombKilled`, `TestSandbox_OversizedOutputKilled`, `TestSandbox_EnvironmentSecretsAbsent`, `TestSandbox_DigestMismatchIsRefused`; `internal/sandboxrt/builder_test.go`: `TestBuilder_EnvironmentCarriesNothingFromTheParent`, `TestBuilder_TimeoutKillsTheBuild`, `TestBuilder_ABuildBeyondItsMemoryBoundIsStoppedAndRefused`; `internal/sandboxrt/workarea_test.go`: `TestBuilder_ABuildPastItsDiskBoundIsStoppedWhileItRuns`, `TestBuilder_ThePaddedStrategyPastTheDiskBoundIsRefused`, `TestBuilder_EveryOutcomeLeavesTheWorkAreaAsItFoundIt`; `internal/sandboxrt/scan_test.go`: `TestScan_ForbiddenImportBlockedByScanner`, `TestScan_EveryDirectiveIsRefused`, `FuzzScan`
- **Mode:** Sandbox (strategies emit PAPER proposals only).
- **Caveat:** The residual risk is a vulnerability in wazero or in the Go compiler, which compiles untrusted source on the host (after the scan, in an empty environment, under time, memory and disk bounds); a live activation would need an isolated builder. The Go-built fixtures need the Go toolchain and skip under `-race` (one compile takes minutes there); this command runs them without `-race`.
- **Evidence:** `go test -count=1 -run '^(TestSandbox_FilesystemEscapeAttemptFails|TestSandbox_NetworkAttemptFails|TestSandbox_ForkBombBlocked|TestSandbox_CPUInfiniteLoopKilled|TestSandbox_MemoryBombKilled|TestSandbox_OversizedOutputKilled|TestSandbox_EnvironmentSecretsAbsent|TestSandbox_DigestMismatchIsRefused|TestBuilder_EnvironmentCarriesNothingFromTheParent|TestBuilder_TimeoutKillsTheBuild|TestBuilder_ABuildBeyondItsMemoryBoundIsStoppedAndRefused|TestBuilder_ABuildPastItsDiskBoundIsStoppedWhileItRuns|TestBuilder_ThePaddedStrategyPastTheDiskBoundIsRefused|TestBuilder_EveryOutcomeLeavesTheWorkAreaAsItFoundIt|TestScan_ForbiddenImportBlockedByScanner|TestScan_EveryDirectiveIsRefused|FuzzScan)$' ./internal/sandboxrt/`

#### SB2 · A run is deterministic and reproducible

- **Claim:** The same artifact with the same input produces byte-identical output, the clock, randomness and sleep inside the sandbox are virtual and deterministic, a recorded run reproduces exactly, and the database refuses an artifact whose digest is not its WASM, a run whose digest does not follow from its artifact, input and output, and any edit of either.
- **Implementation:** `internal/sandboxrt/runtime.go`, `internal/sandboxrt/service.go`, `migrations/00751_sandbox_runs.sql`
- **Tests:** `internal/sandboxrt/runtime_test.go`: `TestSandbox_SameArtifactSameInputIsByteIdentical`, `TestSandbox_ClockAndRandomnessAreDeterministic`, `TestSandbox_SleepIsVirtual`; `internal/sandboxrt/integration_test.go`: `TestIntegration_ARecordedRunIsReproducible`, `TestIntegration_TheDatabaseRefusesATamperedArtifactOrRun`
- **Mode:** Sandbox.
- **Caveat:** Determinism of the runtime given an artifact. Building the artifact is reproducible with the same Go toolchain (`TestBuilder_RealBuildsAreReproducibleAndRefuseBrokenCode`), not across toolchain versions.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/sandboxrt$' -run '^(TestSandbox_SameArtifactSameInputIsByteIdentical|TestSandbox_ClockAndRandomnessAreDeterministic|TestSandbox_SleepIsVirtual|TestIntegration_ARecordedRunIsReproducible|TestIntegration_TheDatabaseRefusesATamperedArtifactOrRun)$'`

#### SB3 · Strategy output can only become a risk-checked paper intent

- **Claim:** Whatever bytes a strategy writes, the only thing that comes out is a service intent of a proposable kind inside the version's manifest (never a transfer, a withdrawal or a signing call), valued at the reference price rather than the strategy's own limit; proposals beyond the manifest are dropped and counted, untrusted output becomes only risk-checked intents, and a dispatch without an ALLOW decision on record is refused by the database.
- **Implementation:** `internal/sandboxrt/manifest.go`, `internal/sandboxrt/output.go`, `internal/sandboxrt/intent.go`, `internal/marketplace/follow.go`
- **Tests:** `internal/sandboxrt/manifest_test.go`: `TestProp_NoStrategyOutputBecomesATransferOrWithdrawal`, `TestProposableKinds_NeverIncludeValueMovement`, `TestProp_ManifestFilterNeverAcceptsBeyondTheManifest`, `TestManifest_ANotionalIsComputedAtTheReferencePriceNeverTheStrategysLimit`; `internal/sandboxrt/runtime_test.go`: `TestSandbox_StrategyCannotConstructALiveSigningCall`; `internal/sandboxrt/output_test.go`: `FuzzParseOutput`; `internal/marketplace/follow_integration_test.go`: `TestIntegration_Prop_UntrustedOutputOnlyBecomesRiskCheckedIntents`, `TestIntegration_ProposalsBeyondManifestDroppedAndCounted`, `TestIntegration_ADispatchWithoutAnAllowDecisionIsRefusedByTheDatabase`
- **Mode:** Sandbox and Paper.
- **Caveat:** A paper result can still mislead a follower about future performance; every surface labels it simulated.
- **Evidence:** `go test -count=1 -run '^(TestProp_NoStrategyOutputBecomesATransferOrWithdrawal|TestProposableKinds_NeverIncludeValueMovement|TestProp_ManifestFilterNeverAcceptsBeyondTheManifest|TestManifest_ANotionalIsComputedAtTheReferencePriceNeverTheStrategysLimit|TestSandbox_StrategyCannotConstructALiveSigningCall|FuzzParseOutput)$' ./internal/sandboxrt/ && go run ./scripts/inttest -pkg '^\./internal/marketplace$' -run '^(TestIntegration_Prop_UntrustedOutputOnlyBecomesRiskCheckedIntents|TestIntegration_ProposalsBeyondManifestDroppedAndCounted|TestIntegration_ADispatchWithoutAnAllowDecisionIsRefusedByTheDatabase)$'`

#### SB4 · Marketplace governance: paper only, reviewed by someone else, results from the books

- **Claim:** A listing is approved only by an operator other than its creator (refused in Go and by the database), an operator cannot re-clear their own version, a version's manifest is immutable, installs are PAPER only (enforced before the database and by it), an agent cannot touch the marketplace, an oversized build is a recorded rejection rather than a wedged submission, concurrent runs cannot overspend a sandbox budget, concurrent follows of one instant dispatch once, and published paper performance is recomputed from fills and marks, never taken from a caller.
- **Implementation:** `internal/marketplace/lifecycle.go`, `internal/marketplace/install.go`, `internal/marketplace/performance.go`, `internal/sandboxrt/budget.go`, `migrations/00750_strategy_marketplace.sql`, `migrations/00757_marketplace_hardening.sql`
- **Tests:** `internal/marketplace/lifecycle_integration_test.go`: `TestIntegration_CreatorCannotApproveOwnListing`, `TestIntegration_AnOperatorCannotReclearTheirOwnVersion`, `TestIntegration_ManifestIsImmutable`, `TestIntegration_InstallIsPaperOnly`, `TestIntegration_AnAgentCannotTouchTheMarketplace`, `TestIntegration_AnOversizedBuildIsARecordedRejectionNeverAWedgedSubmission`; `internal/marketplace/follow_integration_test.go`: `TestIntegration_PaperPerformanceComesFromTheBooks`, `TestIntegration_PaperPerformanceNeverComesFromACallersFigures`, `TestIntegration_ConcurrentFollowsOfOneInstantDispatchOnce`; `internal/sandboxrt/integration_test.go`: `TestIntegration_ConcurrentRunsCannotOverspendTheBudget`
- **Mode:** Paper.
- **Caveat:** "Another operator" is another operator identity. A marketplace over real capital or real followers' money is out of scope (ADR-0011 stands for every live path). `TestIntegration_ConcurrentRunsCannotOverspendTheBudget` is timing-sensitive: a run holds its account's budget lock while the module executes, so on a heavily loaded machine the waiting calls reach the 5 s lock timeout and fail as INTERNAL instead of being refused as rate-limited. It failed that way once while this document was checked (load average above 15) and passed in its two other runs; in no run did the budget overspend.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(marketplace|sandboxrt)$' -run '^(TestIntegration_CreatorCannotApproveOwnListing|TestIntegration_AnOperatorCannotReclearTheirOwnVersion|TestIntegration_ManifestIsImmutable|TestIntegration_InstallIsPaperOnly|TestIntegration_AnAgentCannotTouchTheMarketplace|TestIntegration_AnOversizedBuildIsARecordedRejectionNeverAWedgedSubmission|TestIntegration_PaperPerformanceComesFromTheBooks|TestIntegration_PaperPerformanceNeverComesFromACallersFigures|TestIntegration_ConcurrentFollowsOfOneInstantDispatchOnce|TestIntegration_ConcurrentRunsCannotOverspendTheBudget)$'`

### 15. Delegated agent authority

#### DA1 · A delegated swap is signed exactly as approved

- **Claim:** An agent's delegated swap compiles to one delivery-versus-payment transaction whose plan hash covers every field; the inspector accepts exactly that plan, refuses every mutation and checks Ed25519 signatures over the exact bytes; the venue co-signs only the planned transaction; the human approval binds the plan hash; ten executors racing on one swap produce one signature, one transaction and one posting; a modified, replayed, expired or revoked request is refused before anything is signed; and the database refuses a signing no approved decision licenses.
- **Implementation:** `internal/delegation/plan.go`, `internal/delegation/inspect.go`, `internal/delegation/signer.go`, `internal/delegation/swap.go`, `migrations/00752_delegated_signing.sql`
- **Tests:** `internal/delegation/inspect_test.go`: `TestTheInspectorAcceptsExactlyThePlannedSwap`, `TestTheInspectorRefusesEveryModifiedSwap`, `TestTheInspectorChecksSignaturesOverTheExactBytes`; `internal/delegation/property_test.go`: `TestProp_TheInspectorRefusesEveryMutation`; `internal/delegation/signer_test.go`: `TestTheVenueCoSignsOnlyThePlannedTransaction`, `TestSignersNeverFormatTheirSecrets`; `internal/delegation/swap_integration_test.go`: `TestIntegration_ADelegatedSwapSettlesThroughEveryStep`, `TestIntegration_ModifiedTransactionRejected`, `TestIntegration_ReplayRejected`, `TestIntegration_ExpiredTokenRejected`, `TestIntegration_RevokedTokenRejected`; `internal/delegation/controls_integration_test.go`: `TestIntegration_HumanApprovalBindsThePlan`, `TestIntegration_ExactlyOneSignaturePerSwap`; `internal/delegation/bypass_integration_test.go`: `TestIntegration_TheDatabaseRefusesUnlicensedSwapStates`
- **Mode:** Simulated devnet.
- **Caveat:** Keys are derived inside an in-process signer from a seed held in a closure; there is no HSM. A kill switch raised between the last pre-submit check and the simulated chain accepting the transaction is not caught. The signatures are real Ed25519, on simulated chains only.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/delegation$' -run '^(TestTheInspectorAcceptsExactlyThePlannedSwap|TestTheInspectorRefusesEveryModifiedSwap|TestTheInspectorChecksSignaturesOverTheExactBytes|TestProp_TheInspectorRefusesEveryMutation|TestTheVenueCoSignsOnlyThePlannedTransaction|TestSignersNeverFormatTheirSecrets|TestIntegration_ADelegatedSwapSettlesThroughEveryStep|TestIntegration_ModifiedTransactionRejected|TestIntegration_ReplayRejected|TestIntegration_ExpiredTokenRejected|TestIntegration_RevokedTokenRejected|TestIntegration_HumanApprovalBindsThePlan|TestIntegration_ExactlyOneSignaturePerSwap|TestIntegration_TheDatabaseRefusesUnlicensedSwapStates)$'`

#### DA2 · An agent's authority cannot grow

- **Claim:** An agent acts only under a capability its customer granted, scoped to instrument and action classes, chains, limits and a lifetime and hashed over every term; the capability can neither expand itself nor cross an account boundary, the actors of a grant are never agents, and every use outside the grant is refused with its own code; twelve concurrent $1.00 signings against a daily limit with room for five admit exactly five; a swap's action is the direction of its value, never its label, in Go and in the database (DG007), so a reduce-only capability never buys exposure; the platform's single-trade cap binds every purchase; and a strategy-version kill switch stops the agent's swap and withdrawal at every step.
- **Implementation:** `internal/delegation/capability.go`, `internal/delegation/capabilities.go`, `internal/delegation/swap.go`, `migrations/00762_delegation_hardening.sql`
- **Tests:** `internal/delegation/controls_integration_test.go`: `TestIntegration_StrategyCannotSelfExpandCapability`, `TestIntegration_CapabilityCannotCrossAccountBoundary`, `TestIntegration_ConcurrentSigningsCannotExceedDailyNotional`, `TestIntegration_KillSwitchesStopDelegatedSigning`; `internal/delegation/capability_test.go`: `TestTheActorsOfAGrantAreNeverAgents`, `TestCapabilityRefusesEveryUseOutsideItsGrant`, `TestCapabilityHashCoversEveryTermAndTheNonce`; `internal/delegation/property_test.go`: `TestProp_CapabilityLimitsNeverOverspend`; `internal/delegation/hardening_test.go`: `TestSwapActionIsTheDirectionOfValueNeverTheLabel`; `internal/delegation/hardening_integration_test.go`: `TestIntegration_AReduceOnlyCapabilityNeverBuysExposure`, `TestIntegration_TheDatabaseBindsASwapsActionToItsDirection`, `TestIntegration_SQLAndGoAgreeOnWhatASwapDoes`, `TestIntegration_ThePlatformSingleTradeCapBindsEveryPurchase`, `TestIntegration_AStrategyVersionKillSwitchStopsItsAgentAtEveryStep`
- **Mode:** Simulated devnet.
- **Caveat:** A capability is a hashed database record checked by the service, the risk kernel and the database, not a cryptographic bearer token held by the agent. Its limits are the ones the customer sets, under the platform's caps. A kill-switch check uses the agent's current strategy version, not the version that made the request (threat model 12).
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/delegation$' -run '^(TestIntegration_StrategyCannotSelfExpandCapability|TestIntegration_CapabilityCannotCrossAccountBoundary|TestIntegration_ConcurrentSigningsCannotExceedDailyNotional|TestIntegration_KillSwitchesStopDelegatedSigning|TestTheActorsOfAGrantAreNeverAgents|TestCapabilityRefusesEveryUseOutsideItsGrant|TestCapabilityHashCoversEveryTermAndTheNonce|TestProp_CapabilityLimitsNeverOverspend|TestSwapActionIsTheDirectionOfValueNeverTheLabel|TestIntegration_AReduceOnlyCapabilityNeverBuysExposure|TestIntegration_TheDatabaseBindsASwapsActionToItsDirection|TestIntegration_SQLAndGoAgreeOnWhatASwapDoes|TestIntegration_ThePlatformSingleTradeCapBindsEveryPurchase|TestIntegration_AStrategyVersionKillSwitchStopsItsAgentAtEveryStep)$'`

#### DA3 · Agents propose withdrawals; they never withdraw

- **Claim:** An agent can never withdraw directly (the settlement compiler refuses it at every authority level, even in simulation); a delegated withdrawal is an agent's proposal that a person decides, except a low-risk one auto-approved in a test environment, and the database refuses a policy approval of one that needed a person (DW003). It goes only to an allow-listed destination, with destination and amount frozen in the approved plan's hash, within a daily limit that holds under concurrency, after a cooling period, and a frozen account stops it at every step.
- **Implementation:** `internal/delegation/withdrawal.go`, `internal/delegation/policy.go`, `internal/settlement/compiler.go`, `migrations/00753_delegated_withdrawals.sql`
- **Tests:** `internal/delegation/controls_integration_test.go`: `TestIntegration_AnAgentCanNeverWithdrawDirectly`; `internal/delegation/withdrawal_integration_test.go`: `TestIntegration_WithdrawalAllowlistIsEnforced`, `TestIntegration_NoRawAddressSubstitutionAfterApproval`, `TestIntegration_NoAmountMutationAfterApproval`, `TestIntegration_WithdrawalDailyLimitHoldsUnderConcurrency`, `TestIntegration_AHumanDecidesEverythingElse`, `TestIntegration_AnAutoApprovedWithdrawalSettlesInTestMode`, `TestIntegration_TheCoolingPeriodDelaysExecution`, `TestIntegration_AFrozenAccountStopsWithdrawalsAtEveryStep`, `TestIntegration_TheDatabaseRefusesUnlicensedWithdrawalStates`; `internal/settlement/compiler_test.go`: `TestCompile_AnAgentCanNeverTransferOrWithdrawEvenInSimulation`
- **Mode:** Simulated devnet.
- **Caveat:** A "withdrawal" here moves `SIMULATED` value to an address on a simulated chain; nothing leaves the system. The customer must set a withdrawal policy explicitly (there is no default).
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/delegation$' -run '^(TestIntegration_AnAgentCanNeverWithdrawDirectly|TestIntegration_WithdrawalAllowlistIsEnforced|TestIntegration_NoRawAddressSubstitutionAfterApproval|TestIntegration_NoAmountMutationAfterApproval|TestIntegration_WithdrawalDailyLimitHoldsUnderConcurrency|TestIntegration_AHumanDecidesEverythingElse|TestIntegration_AnAutoApprovedWithdrawalSettlesInTestMode|TestIntegration_TheCoolingPeriodDelaysExecution|TestIntegration_AFrozenAccountStopsWithdrawalsAtEveryStep|TestIntegration_TheDatabaseRefusesUnlicensedWithdrawalStates)$' && go test -count=1 -run '^TestCompile_AnAgentCanNeverTransferOrWithdrawEvenInSimulation$' ./internal/settlement/`

#### DA4 · The agent runtime can only ask

- **Claim:** The agent runtime requests delegated swaps and proposes delegated withdrawals in-process through a facade whose whole surface is two admissions and a capability read; the runtime's source names no delegation service, signer or signing method; requests the runtime refuses never reach delegation; an agent run that requests both leaves no signature, effect, posting, transfer or on-chain movement until the account's owner approves, and the capability bounds every request; agent principals are refused by every HTTP operation; the agent trees import no signing, wallet, admin, gate, withdrawal or kill-switch package; and the process that runs agents holds no private key material at all: the facade is built with no seed, key or signer and plans from the wallet public key the owner's grant registers in the API process, and the worker never reads the sandbox seed.
- **Implementation:** `internal/delegation/admission.go`, `internal/agent/delegated.go`, `internal/agent/delegated_run.go`, `cmd/agent-worker/delegation.go`
- **Tests:** `internal/delegation/admission_test.go`: `TestAgentAdmissionExposesOnlyAdmissionAndTheCapabilityRead`, `TestTheAgentAdmissionHoldsNoKeyMaterial`, `TestNewAgentAdmissionTakesNoSeedAndNoSigner`; `cmd/agent-worker/main_test.go`: `TestTheWorkerTakesNoSeed`, `TestTheWorkersConfigurationHoldsNoSeed`; `internal/agent/delegated_test.go`: `TestTheRuntimeHoldsNoDelegationServiceOrSigner`, `TestDelegatedRequestsTheRuntimeRefusesNeverReachDelegation`; `internal/agent/delegated_integration_test.go`: `TestIntegration_AnAgentNeverProducesATransferAWithdrawalOrASignature`, `TestIntegration_AnAgentRunRequestsADelegatedSwapThatSettlesAfterTheOwnerApproves`, `TestIntegration_TheCapabilityBoundsTheAgentsRequests`; `internal/httpapi/authz_test.go`: `TestNoOperationAdmitsAnAgent`, `TestAgentPrincipalsAreRefusedEverywhere`; `test/security/authority_boundary_test.go`: `TestAgentTreesNeverImportAuthority`
- **Mode:** Simulated devnet; agent runs are PAPER only.
- **Caveat:** Agents are untrusted proposal generators and stay off HTTP entirely; nothing here measures the quality of any model's proposals.
- **Evidence:** `go test -count=1 -run '^(TestAgentAdmissionExposesOnlyAdmissionAndTheCapabilityRead|TestTheAgentAdmissionHoldsNoKeyMaterial|TestNewAgentAdmissionTakesNoSeedAndNoSigner|TestTheWorkerTakesNoSeed|TestTheWorkersConfigurationHoldsNoSeed|TestTheRuntimeHoldsNoDelegationServiceOrSigner|TestDelegatedRequestsTheRuntimeRefusesNeverReachDelegation|TestNoOperationAdmitsAnAgent|TestAgentPrincipalsAreRefusedEverywhere|TestAgentTreesNeverImportAuthority)$' ./internal/delegation/ ./cmd/agent-worker/ ./internal/agent/ ./internal/httpapi/ ./test/security/ && go run ./scripts/inttest -pkg '^\./internal/agent$' -run '^(TestIntegration_AnAgentNeverProducesATransferAWithdrawalOrASignature|TestIntegration_AnAgentRunRequestsADelegatedSwapThatSettlesAfterTheOwnerApproves|TestIntegration_TheCapabilityBoundsTheAgentsRequests)$'`

### 16. Prediction markets

#### PM1 · Escrow conserves exactly, and settlement pays once

- **Claim:** Complete sets are issued and redeemed against escrow; settlement pays each winning share one unit of collateral and each losing share nothing, and over random sequences of issuance, redemption and trading across two to four outcomes it pays out exactly the escrow; eight concurrent settlements settle once; concurrent issuances cannot overspend collateral; a void refunds exactly and discloses any rounding residual; a share exists only as its market issued it; and a frozen holder cannot block settlement and claims once eligible.
- **Implementation:** `internal/predmarket/settle.go`, `internal/predmarket/math.go`, `internal/predmarket/held.go`, `migrations/00749_prediction_markets.sql`, `migrations/00756_predmarket_hardening.sql`
- **Tests:** `internal/predmarket/settle_integration_test.go`: `TestProp_SettlementPaysExactlyTheEscrow`, `TestIntegration_ConcurrentSettlementsSettleOnce`, `TestIntegration_ConcurrentIssuancesCannotOverspendCollateral`, `TestIntegration_AWinningSharePaysOneUnitAndALosingSharePaysNothing`, `TestIntegration_VoidRefundsAMultiOutcomeMarketAndDisclosesTheResidual`, `TestIntegration_ScenarioH_CreateTradeCloseResolveSettle`; `internal/predmarket/math_test.go`: `TestProp_PayoutConservation`, `TestProp_RefundConservesAndBoundsTheResidual`; `internal/predmarket/hardening_integration_test.go`: `TestIntegration_AnOutcomeShareExistsOnlyAsItsMarketIssuedIt`, `TestIntegration_AFrozenHolderCannotBlockSettlementAndClaimsOnceEligible`, `TestProp_HeldPayoutsConserveTheEscrowExactly`
- **Mode:** Paper and Sandbox.
- **Caveat:** No real stakes: the collateral is the sandbox dollar and operators define the markets.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/predmarket$' -run '^(TestProp_SettlementPaysExactlyTheEscrow|TestIntegration_ConcurrentSettlementsSettleOnce|TestIntegration_ConcurrentIssuancesCannotOverspendCollateral|TestIntegration_AWinningSharePaysOneUnitAndALosingSharePaysNothing|TestIntegration_VoidRefundsAMultiOutcomeMarketAndDisclosesTheResidual|TestIntegration_ScenarioH_CreateTradeCloseResolveSettle|TestProp_PayoutConservation|TestProp_RefundConservesAndBoundsTheResidual|TestIntegration_AnOutcomeShareExistsOnlyAsItsMarketIssuedIt|TestIntegration_AFrozenHolderCannotBlockSettlementAndClaimsOnceEligible|TestProp_HeldPayoutsConserveTheEscrowExactly)$'`

#### PM2 · Nobody rules on their own question

- **Claim:** The operator who recorded the oracle observation cannot resolve the market, a resolver cannot rule on the dispute of their own resolution, a void needs a proposal and an approval by two distinct operators (bound in the database), oracle reports are Ed25519-verified, a replayed report is idempotent and a conflicting one refused, and a market cannot settle before resolution or while disputed.
- **Implementation:** `internal/predmarket/lifecycle.go`, `internal/predmarket/oracle.go`, `internal/predmarket/observations.go`
- **Tests:** `internal/predmarket/hardening_integration_test.go`: `TestIntegration_TheObservationRecorderCannotResolveTheMarket`, `TestIntegration_TheResolverCannotRuleOnTheDisputeOfTheirOwnResolution`, `TestIntegration_AVoidNeedsTwoDistinctOperators`, `TestIntegration_TheDatabaseBindsAVoidToAnApprovedProposalByTwoOperators`; `internal/predmarket/lifecycle_integration_test.go`: `TestIntegration_OracleReplayIsIdempotentAndAConflictingReportIsRefused`, `TestIntegration_AnUnverifiableOracleReportIsRefused`, `TestIntegration_ADisputedMarketCannotSettle`, `TestIntegration_NoSettlementBeforeResolution`; `internal/predmarket/oracle_test.go`: `TestVerifyReport_RefusesEveryTampering`, `FuzzDecodeOracleReport`
- **Mode:** Paper and Sandbox.
- **Caveat:** One sandbox oracle key whose observation an operator enters, so the oracle is only as independent as the separation between recorder and resolver; one resolver decides a resolution (two are needed to void); collusion between roles is out of scope.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/predmarket$' -run '^(TestIntegration_TheObservationRecorderCannotResolveTheMarket|TestIntegration_TheResolverCannotRuleOnTheDisputeOfTheirOwnResolution|TestIntegration_AVoidNeedsTwoDistinctOperators|TestIntegration_TheDatabaseBindsAVoidToAnApprovedProposalByTwoOperators|TestIntegration_OracleReplayIsIdempotentAndAConflictingReportIsRefused|TestIntegration_AnUnverifiableOracleReportIsRefused|TestIntegration_ADisputedMarketCannotSettle|TestIntegration_NoSettlementBeforeResolution|TestVerifyReport_RefusesEveryTampering|FuzzDecodeOracleReport)$'`

### 17. Security program

#### SEC1 · Live capital is unrepresentable for every advanced capability

- **Claim:** For each of the 16 advanced capabilities, `LIVE` is refused in every environment, gate state and kill-switch state (a property over all of them); each capability is high risk, with a zero live-capital ceiling, at least two approvers and a legal review among its live evidence; an economic intent in mode `LIVE` is refused by Go and by a database CHECK; the authorizer refuses `LIVE` against the real gate tables; and the equities live adapter refuses unconditionally.
- **Implementation:** `internal/gates/advanced.go`, `internal/econ/authorize.go`, `internal/econ/intent.go`, `migrations/00731_advanced_financial_foundation.sql`
- **Tests:** `internal/econ/econ_test.go`: `TestProp_LiveIsNeverAllowed`, `TestAdvancedSpecs_CoverEveryAdvancedCapabilityOnceAndNoneIsLive`, `TestDecideMode_Table`, `TestIntent_ValidateRefusesLiveAndEveryMalformedShape`; `internal/econ/integration_test.go`: `TestIntegration_ALiveIntentCannotBeWrittenDown`, `TestIntegration_AuthorizerReadsRealGatesAndKillSwitches`; `internal/equities/paper_test.go`: `TestProviderMode_LiveIsRefusedUnconditionally`
- **Mode:** Local/test, covering every mode.
- **Caveat:** "Unrepresentable" holds for this schema and build: enabling any of it is a code change (`LiveImplemented`), a migration and a gate ceremony. That is the design, and it also means no live path for these capabilities is available to test.
- **Evidence:** `go test -count=1 -run '^TestProviderMode_LiveIsRefusedUnconditionally$' ./internal/equities/ && go run ./scripts/inttest -pkg '^\./internal/econ$' -run '^(TestProp_LiveIsNeverAllowed|TestAdvancedSpecs_CoverEveryAdvancedCapabilityOnceAndNoneIsLive|TestDecideMode_Table|TestIntent_ValidateRefusesLiveAndEveryMalformedShape|TestIntegration_ALiveIntentCannotBeWrittenDown|TestIntegration_AuthorizerReadsRealGatesAndKillSwitches)$'`

#### SEC2 · A V1 live gate needs five conditions; configuration alone never activates one

- **Claim:** A V1 capability gate is active only when deployment configuration enables it and its persisted row is ACTIVE, unrevoked, inside its validity window, carries legal, provider, risk and security evidence references, and was proposed, approved and activated by distinct principals, each failing condition with its own reason, so configuration alone never activates one; every live-money capability is high risk; and in the database a gate can only be born DISABLED and its state moves only through a SECURITY DEFINER function the application cannot own, the application role holding no UPDATE on any deciding column and no INSERT into gate history.
- **Implementation:** `internal/gates/gate.go`, `internal/gates/checker.go`, `internal/gates/admin.go`, `migrations/00701_capability_gate_state_authority.sql`
- **Tests:** `internal/gates/gates_test.go`: `TestEvaluate_ConfigAloneNeverActivates`, `TestEvaluate_EachConditionFailsWithDistinctReason`, `TestRules_SamePrincipalCannotApproveAndActivate`, `TestChecker_NilEnabledFailsClosed`, `TestCapabilities_And_HighRisk`; `internal/gates/integration_test.go`: `TestIntegration_DatabaseRefusesForgedActivation`, `TestIntegration_Lifecycle_ThreePrincipals`, `TestIntegration_Bootstrap_Idempotent`; `internal/gates/evidence_integration_test.go`: `TestIntegration_DatabaseRefusesActivationWithoutEvidence`; `internal/gates/parity_integration_test.go`: `TestIntegration_GoAndSQLAgreeOnEveryCapabilitysRisk`; `test/integration/migrations/privileges_test.go`: `TestIntegration_CapabilityGateStateAuthority`
- **Mode:** Core.
- **Caveat:** The gates are built and every live one is DISABLED; none has ever been activated for real capital, and activation would also need provider contracts and legal reviews this project has not obtained (`docs/build/BLOCKERS.md`). "Distinct principals" in the tests are distinct identities, not distinct people.
- **Evidence:** `go test -count=1 -run '^(TestEvaluate_ConfigAloneNeverActivates|TestEvaluate_EachConditionFailsWithDistinctReason|TestRules_SamePrincipalCannotApproveAndActivate|TestChecker_NilEnabledFailsClosed|TestCapabilities_And_HighRisk)$' ./internal/gates/ && go run ./scripts/inttest -pkg '^\./(internal/gates|test/integration/migrations)$' -run '^(TestIntegration_DatabaseRefusesForgedActivation|TestIntegration_Lifecycle_ThreePrincipals|TestIntegration_Bootstrap_Idempotent|TestIntegration_DatabaseRefusesActivationWithoutEvidence|TestIntegration_GoAndSQLAgreeOnEveryCapabilitysRisk|TestIntegration_CapabilityGateStateAuthority)$'`

#### SEC3 · Production refuses fake providers, seed data and debug authentication

- **Claim:** A STAGING or PROD configuration that names a fake provider, enables seed data or enables debug authentication is refused, and the LOCAL defaults are never valid for production.
- **Implementation:** `internal/config/validate.go`, `internal/config/environment.go`
- **Tests:** `test/security/production_config_test.go`: `TestProductionRefusesFakeProviders`, `TestProductionRefusesSeedAndDebugAuth`, `TestLocalDefaultsAreNotProductionValid`
- **Mode:** Local/test.
- **Caveat:** It validates configuration; no production deployment exists to apply it to (Terraform validates but has never been planned or applied against AWS, EB-012).
- **Evidence:** `go test -count=1 -run '^(TestProductionRefusesFakeProviders|TestProductionRefusesSeedAndDebugAuth|TestLocalDefaultsAreNotProductionValid)$' ./test/security/`

#### SEC4 · Authority boundaries are structural

- **Claim:** The source trees that run agent or strategy logic import no signing, wallet, admin, gate, withdrawal or kill-switch package; only the execution boundary imports the signing service; the agent principal's permission set is closed; every SQL statement on a request path is derived from constants (a constant-derivation analysis, not a grep); untrusted text never enters a model's instruction channel; and every declared negative control is read by a test.
- **Implementation:** `test/security/authority_boundary_test.go`, `test/security/sqlsource_test.go`, `test/security/prompt_boundary_test.go`, `test/security/break_test.go`
- **Tests:** `test/security/authority_boundary_test.go`: `TestAgentTreesNeverImportAuthority`, `TestSigningImportedOnlyByExecutionBoundary`, `TestAgentPrincipalPermissionSetIsClosed`; `test/security/sqlsource_test.go`: `TestSQLInjection_EveryStatementIsBuiltFromConstants`, `TestSQLInjection_ExcludedTreesAreOffTheRequestPath`; `test/security/prompt_boundary_test.go`: `TestPromptInjection_UntrustedTextNeverEntersTheInstructionChannel`; `test/security/break_test.go`: `TestSecurityBreaksAreDeclared`
- **Mode:** Local/test.
- **Caveat:** Static analysis of the source: it constrains what the code can reach, not what an attacker who can execute code can do. `TestSecurityBreaksAreDeclared` proves each of the 16 negative controls in `test/security/break_test.go` is declared and used; running the controls to watch each guard fail is a separate exercise, which MASTER_BUILD_STATE records as done for all 16 (Stage 18) and which this document did not repeat.
- **Evidence:** `go test -count=1 ./test/security/...`

#### SEC5 · Deny-by-default HTTP authorization and tenant isolation

- **Claim:** Every generated OpenAPI operation has an explicit authorization policy (an operation without one fails this test and is refused at runtime), an unknown operation fails closed, every route refuses a principal without permissions, and a customer is refused another account's data on every account-scoped route, core and advanced, including with case-varied ids and duplicated query parameters; a new account-scoped path in the OpenAPI document fails the suite until it has a tenant probe.
- **Implementation:** `internal/httpapi/authz.go`, `openapi/openapi.yaml`
- **Tests:** `internal/httpapi/authz_test.go`: `TestEveryGeneratedOperationHasAnExplicitPolicy`, `TestEveryRouteRefusesAPrincipalWithoutPermissions`, `TestAuthorizeFailsClosedForAnUnknownOperation`, `TestOperationPolicyCountMatchesRouteCount`; `test/security/idor_test.go`: `TestIDOR_CrossTenantAccountReadsAreRefused`, `TestIDOR_QueryParameterSmuggling`, `TestIDOR_UUIDCaseVariationIsNotABypass`, `TestIDOR_CustomerCannotReachAdminSurface`, `TestIDOR_ForeignAndAbsentAccountsAreIndistinguishable`, `TestIDOR_DEFECT_RecordLookupDistinguishesForeignFromAbsent`; `test/security/fixtures_test.go`: `TestIDOR_CoversEveryAccountScopedRoute`
- **Mode:** Local/test (the real router and handlers over a real database, in-process).
- **Caveat:** One open defect is pinned by `TestIDOR_DEFECT_RecordLookupDistinguishesForeignFromAbsent`: four by-id V1 routes (`GET /v1/intents/{id}` and three like it) answer 403 for another customer's record and 404 for a missing one, so a customer holding an id can learn whether it names a real record (no data is disclosed). The coverage check reads the `/accounts/{accountId}` paths of the OpenAPI document; the advanced list routes are listed in the probe by hand.
- **Evidence:** `go test -count=1 -run '^(TestEveryGeneratedOperationHasAnExplicitPolicy|TestEveryRouteRefusesAPrincipalWithoutPermissions|TestAuthorizeFailsClosedForAnUnknownOperation|TestOperationPolicyCountMatchesRouteCount)$' ./internal/httpapi/ && go run ./scripts/inttest -pkg '^\./test/security$' -run '^TestIDOR_'`

#### SEC6 · Transactions are inspected before signing (V1 Solana)

- **Claim:** Every Solana transaction is decoded (legacy and v0, with address lookup tables) and checked against its approved plan before a bounded signing request: each of 57 mutation cases (an extra transfer, a foreign destination, a delegate approval and the rest) is refused with a named check, random byte flips are never approved, the inspection is deterministic, and the Jupiter v6 account layouts, route arguments and swap-variant payloads are checked against the program's published IDL.
- **Implementation:** `internal/signing/inspect/inspect.go`, `internal/signing/inspect/decode.go`, `internal/signing/inspect/jupiter.go`, `internal/signing/inspect/testdata/jupiter_v6_idl.json`
- **Tests:** `internal/signing/inspect/inspect_test.go`: `TestRejections`, `TestGolden_Approved`, `TestInspect_Deterministic`, `TestCheckNames_MatchExecutionSpec`; `internal/signing/inspect/property_test.go`: `TestProp_ByteFlipsNeverApprove`; `internal/signing/inspect/jupiter_idl_test.go`: `TestIDL_AccountLayoutsMatchTheInspector`, `TestIDL_RouteArgsMatchTheInspector`, `TestIDL_SwapVariantPayloadsMatchTheTable`, `TestIDL_VariantsBeyondThePublishedIDLStayUnverified`; `internal/signing/inspect/fuzz_test.go`: `FuzzDecode`, `FuzzInspect`
- **Mode:** Core code (the inspector a live path would use).
- **Caveat:** It has never inspected a transaction that went to a real chain: no venue adapter is wired (X1). The IDL is a published file pinned at a commit, not read from the chain, and swap variants newer than it stay unverified.
- **Evidence:** `go test -count=1 -run '^(TestRejections|TestGolden_Approved|TestInspect_Deterministic|TestCheckNames_MatchExecutionSpec|TestProp_ByteFlipsNeverApprove|TestIDL_AccountLayoutsMatchTheInspector|TestIDL_RouteArgsMatchTheInspector|TestIDL_SwapVariantPayloadsMatchTheTable|TestIDL_VariantsBeyondThePublishedIDLStayUnverified|FuzzDecode|FuzzInspect)$' ./internal/signing/inspect/`

#### SEC7 · Adversarial reviews found 27 defects; each is fixed and pinned by a test

- **Claim:** Two adversarial reviews of the advanced subsystems, by read-only reviewers that had not written the code under review, found 27 defects (7 high, 12 medium, 8 low); all 27 are fixed, the register names a regression test for each, and the seven high-severity fixes are pinned by the tests below.
- **Implementation:** `docs/reviews/ADVANCED_SYSTEMS_REVIEW.md` (the register)
- **Tests:** `internal/margin/integration_review_test.go`: `TestIntegration_AManualReviewCaseResumesOnlyWithTwoOperators`, `TestIntegration_NoInterestAccruesWhileAPersonHoldsTheCase`; `internal/margin/integration_hardening_test.go`: `TestIntegration_BorrowedCashInTheWalletIsSweptBeforeBankruptcy`, `TestIntegration_ACoveredDeficitRestrictsTheCustomerAccount`, `TestIntegration_ADeficitIsRepaidToTheFundExactlyOnce`; `internal/transfers/dualcontrol_integration_test.go`: `TestIntegration_AProposerCannotApproveTheirOwnReversal`, `TestIntegration_TwoOperatorsReverseExactlyOnceUnderConcurrency`; `internal/predmarket/hardening_integration_test.go`: `TestIntegration_AnOutcomeShareExistsOnlyAsItsMarketIssuedIt`, `TestIntegration_AVoidNeedsTwoDistinctOperators`; `internal/sandboxfund/predshare_integration_test.go`: `TestIntegration_TheFaucetRefusesAPredictionMarketShare`; `internal/derivatives/integration_hardening_test.go`: `TestIntegration_ASettlementIsPricedByTheRuleNeverByTheCaller`, `TestIntegration_AStaleExpiryWaitsForAnOverrideTwoOperatorsAgreeOn`, `TestIntegration_AMarkIsRecordedOnlyInOrderAndOnTime`, `TestIntegration_FundingIgnoresMarksRecordedAfterTheInterval`
- **Mode:** Local/test.
- **Caveat:** The reviewers were separate read-only agent sessions working from the code (ADVANCED_SYSTEMS_REVIEW.md); these are internal reviews, not an external audit. Severity is stated as if the value were real; every value involved is `SIMULATED`. A third review, the security red team of the integrated system (`docs/reviews/SECURITY_RED_TEAM.md`), reports one Medium and four Low findings and four observations; DA2, R2, SB1, R4, MA2, S1, T3 and CC2 cite tests that pin those fixes, for every finding except its F3 (fairness of a delegated execution run), which this document does not claim.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/(margin|transfers|predmarket|sandboxfund|derivatives)$' -run '^(TestIntegration_AManualReviewCaseResumesOnlyWithTwoOperators|TestIntegration_NoInterestAccruesWhileAPersonHoldsTheCase|TestIntegration_BorrowedCashInTheWalletIsSweptBeforeBankruptcy|TestIntegration_ACoveredDeficitRestrictsTheCustomerAccount|TestIntegration_ADeficitIsRepaidToTheFundExactlyOnce|TestIntegration_AProposerCannotApproveTheirOwnReversal|TestIntegration_TwoOperatorsReverseExactlyOnceUnderConcurrency|TestIntegration_AnOutcomeShareExistsOnlyAsItsMarketIssuedIt|TestIntegration_AVoidNeedsTwoDistinctOperators|TestIntegration_TheFaucetRefusesAPredictionMarketShare|TestIntegration_ASettlementIsPricedByTheRuleNeverByTheCaller|TestIntegration_AStaleExpiryWaitsForAnOverrideTwoOperatorsAgreeOn|TestIntegration_AMarkIsRecordedOnlyInOrderAndOnTime|TestIntegration_FundingIgnoresMarksRecordedAfterTheInterval)$'`

### 18. Operations

#### OP1 · Migrations are checksum-verified, role-separated and bind state changes

- **Claim:** The embedded migrations apply cleanly to an empty schema and re-run as a no-op, a tampered or missing checksum is detected, a rollback below the protected version is guarded, the application role has DML without DELETE and no DDL, the capability-gate state moves only through a SECURITY DEFINER function the application cannot own, every SQLSTATE a migration documents is actually raised, and a guarded table's state cannot change without a matching transition row in the same transaction (AU001).
- **Implementation:** `internal/db/migrate`, `migrations/00603_state_change_binding.sql`, `migrations/00701_capability_gate_state_authority.sql`
- **Tests:** `test/integration/migrations/migrations_test.go`: `TestIntegration_Migrations`; `test/integration/migrations/privileges_test.go`: `TestIntegration_ApplicationRolePrivileges`, `TestIntegration_CapabilityGateStateAuthority`; `test/integration/migrations/transition_binding_test.go`: `TestIntegration_StateChangeRequiresTransitionRow`; `test/integration/migrations/sqlstates_test.go`: `TestIntegration_EveryDocumentedSQLStateIsRaised`
- **Mode:** Core.
- **Caveat:** Two audit findings are OPEN. F-42: the AU001 binding trusts a transaction-local flag that code holding the application credential can set directly, so on a table where `cp_app` keeps UPDATE it stops mistakes, not a compromised application (the finding names sixteen such tables; the advanced status bindings of migration 00733 use the same kind of flag; capability gates are the exception, SEC2). F-47: two statements about which roles may read encrypted personal data disagree.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./test/integration/migrations$'`

#### OP2 · A backup restores to an identical ledger

- **Claim:** The restore drill seeds a database through the real services, dumps it with `pg_dump`, restores it into a fresh database with `pg_restore`, verifies the migration checksums and version, and compares every table's row count, every ledger balance against its entries and the journal hashes on both sides; any mismatch fails it.
- **Implementation:** `scripts/restoredrill/main.go`, `scripts/restoredrill/domaina.go`, `scripts/restoredrill/advanced.go`, `scripts/restoredrill/areas.go`, `scripts/internal/sandboxworld/activity.go`, `docs/operations/BACKUP_RESTORE.md`
- **Tests:** the drill is the check (report in `dist/restore-drill.json`); there is no Go test function for it.
- **Mode:** Local/test.
- **Caveat:** After `80f616b` the fixture gained the advanced books, seeded through the services the end-to-end demo drives (`scripts/internal/sandboxworld`): faucet funding of two customers, a transfer between them, two limit orders that trade on an internal book, a margin account with posted collateral, a stable asset issued on the simulated chain, and complete sets of a prediction market. The drill now reports every advanced area's rows on both sides and fails when an area it seeds holds no row in the source, so it shows those books surviving a restore. It shows nothing for the eight advanced areas it does not seed (derivatives, equities, treasury, bridges, the strategy marketplace, the strategy sandbox, delegation and scheduled upkeep), which still compare zero with zero, nor for what that opening activity does not write: no liquidation, redemption, resolution or settlement is seeded. The production procedure (RDS point-in-time restore) is documented and has never been exercised (EB-012). At `80f616b`, before the advanced books were seeded, the drill passed at schema version 763: 269 tables, row counts equal, no balance drift, journal hashes equal. With them it passes at the same version, and its 12 seeded advanced areas hold 145 rows in the source, every one restored with its count.
- **Evidence:** `go run ./scripts/restoredrill -pg-bin "$PG_BIN"`, where `PG_BIN` is the directory holding the PostgreSQL 16 `pg_dump` and `pg_restore` (on a host with Docker: `make restore-drill`)

#### OP3 · Scheduled upkeep runs each job once and isolates bad items

- **Claim:** `cmd/advanced-worker` runs every scheduled job and reconciler of the advanced books: a job runs exactly when it is due and never overlaps itself, a panicking job is a recorded failure that stops no other job, interest for a day is charged once across three runs, a reservation past its TTL expires once, an equity fill settles at T+1 and not before, an unknown equity placement is polled and never placed again, and one item that cannot be processed never stops the items behind it.
- **Implementation:** `cmd/advanced-worker/main.go`, `cmd/advanced-worker/wire.go`, `internal/advancedjobs/scheduler.go`, `internal/advancedjobs/catalog.go`, `migrations/00760_upkeep_robustness.sql`
- **Tests:** `internal/advancedjobs/scheduler_test.go`: `TestProp_SchedulerRunsAJobExactlyWhenDue`, `TestScheduler_NeverOverlapsAJobWithItself`, `TestScheduler_APanicIsAFailureAndTheOtherJobsRun`; `internal/advancedjobs/capital_margin_integration_test.go`: `TestIntegration_InterestForADayIsChargedExactlyOnceAcrossThreeRuns`, `TestIntegration_AReservationPastItsTTLIsExpiredExactlyOnce`; `internal/advancedjobs/equities_integration_test.go`: `TestIntegration_AnEquityFillSettlesAtTPlusOneAndNotBefore`, `TestIntegration_AnUnknownEquityPlacementIsPolledNeverPlacedAgain`; `internal/advancedjobs/upkeep_integration_test.go`: `TestIntegration_AnExpiryRunReportsTheReservationItCannotExpireAndExpiresTheRest`; `internal/capital/upkeep_integration_test.go`: `TestIntegration_AReservationThatCannotExpireNeverStopsTheOthers`; `cmd/advanced-worker/main_integration_test.go`: `TestIntegration_OnceRunsEveryJobThroughTheWiredServices`
- **Mode:** Paper, Sandbox and Simulated devnet.
- **Caveat:** Run against a local PostgreSQL; the worker has never run in a deployed environment.
- **Evidence:** `go run ./scripts/inttest -pkg '^\./(internal/advancedjobs|internal/capital|cmd/advanced-worker)$' -run '^(TestProp_SchedulerRunsAJobExactlyWhenDue|TestScheduler_NeverOverlapsAJobWithItself|TestScheduler_APanicIsAFailureAndTheOtherJobsRun|TestIntegration_InterestForADayIsChargedExactlyOnceAcrossThreeRuns|TestIntegration_AReservationPastItsTTLIsExpiredExactlyOnce|TestIntegration_AnEquityFillSettlesAtTPlusOneAndNotBefore|TestIntegration_AnUnknownEquityPlacementIsPolledNeverPlacedAgain|TestIntegration_AnExpiryRunReportsTheReservationItCannotExpireAndExpiresTheRest|TestIntegration_AReservationThatCannotExpireNeverStopsTheOthers|TestIntegration_OnceRunsEveryJobThroughTheWiredServices)$'`

#### OP4 · Metrics that count what the rows say, and runbooks that name real routes

- **Claim:** The advanced metrics collector's gauges move by exactly what the rows added between two passes (customer liabilities at par or at the mark, the house book apart, unvalued assets counted rather than valued at zero, open reconciliation records by kind, reserve coverage from the reserve ledger), counters count each new row once per process, every runbook route is served by the API, and nothing a runbook marks PENDING is already built.
- **Implementation:** `internal/advancedmetrics/collector.go`, `internal/advancedmetrics/instruments.go`, `docs/runbooks/advanced-sandbox-reconciliation.md`
- **Tests:** `internal/advancedmetrics/collector_integration_test.go`: `TestIntegration_GaugesEqualTheRowDerivedTruth`, `TestIntegration_CountersCountEachNewRowOncePerProcess`; `internal/advancedmetrics/instruments_test.go`: `TestInstruments_AreNamespacedAndDescribed`; `test/docs/runbooks_test.go`: `TestDocs_EveryRunbookRouteIsServed`, `TestDocs_NothingMarkedPendingAlreadyExists`
- **Mode:** Local/test.
- **Caveat:** Metrics, alarms and 20 runbooks are written; no alarm has fired in a deployment and no runbook has been used in an incident. One declared instrument, provider mismatches, is recorded by nothing, since no advanced reconciliation compares against an external provider (capabilities matrix, row 24).
- **Evidence:** `go run ./scripts/inttest -pkg '^\./internal/advancedmetrics$' -run '^(TestIntegration_GaugesEqualTheRowDerivedTruth|TestIntegration_CountersCountEachNewRowOncePerProcess|TestInstruments_AreNamespacedAndDescribed)$' && go test -count=1 -run '^(TestDocs_EveryRunbookRouteIsServed|TestDocs_NothingMarkedPendingAlreadyExists)$' ./test/docs/`

#### OP5 · Guards for reachable money paths, live citations and hidden characters

- **Claim:** Every exported financial mutator that takes a transaction has a caller a deployment can reach (a test caller does not count, and every exemption names an external blocker); every test that a reviewer-facing document cites, this one included, is declared; no file contains a bidirectional control character; and the reachability and character guards each carry a test showing they can fail.
- **Implementation:** `test/reachability/reachability_test.go`, `test/reachability/driver_test.go`, `test/docs/references_test.go`, `test/source/trojansource_test.go`
- **Tests:** `test/reachability/reachability_test.go`: `TestReachability_EveryFinancialMutatorHasADeploymentCaller`, `TestReachability_EveryExemptionNamesABlocker`; `test/reachability/driver_test.go`: `TestReachability_EveryDriverClaimIsTrue`, `TestReachability_TheDriverCheckCanFail`; `test/docs/references_test.go`: `TestDocs_EveryTestTheyNameExists`; `test/source/trojansource_test.go`: `TestSource_NoBidirectionalControlCharacters`, `TestSource_TheCheckCanFail`
- **Mode:** Local/test.
- **Caveat:** Reachability is a static question (does a deployed binary call it), not proof that a path is exercised in production. The citation check proves a cited test is declared, not that it asserts what a document says; that is why each claim here names the file and the function. It has no in-suite negative control; when this document was written it was seen failing on a planted citation and passing once the plant was removed.
- **Evidence:** `go test -count=1 ./test/reachability/ ./test/docs/ ./test/source/`

## Benchmarks

No number in this section is a claim yet. The measurements come from `make perf-advanced`
(`go run ./scripts/advperf`, report in `dist/advanced-perf.md` with a `.json` beside it), which records the
host, CPU, Go and PostgreSQL versions and the load average before and after, and reports a missed target as
missed. The one recorded run is in `docs/build/ADVANCED_TEST_EVIDENCE.md`:

> 2026-10-07, `c4defed`, `make perf-advanced` (smoke, shared machine): every measured target met; a recorded
> run on an idle machine is pending.

That run recorded no figures in the evidence file, so none is quoted here. The table is for the integrator to
fill from one run on an otherwise idle machine, with the commit, the date and the `environment` block of the
report. A figure from a development cluster says nothing about production hardware: the local cluster this
document was checked on runs with `fsync=off`, which flatters every commit latency.

| Measurement (`scripts/advperf/main.go`) | Target | Result | Commit, date, machine and load |
|---|---|---|---|
| Risk Kernel V2 decision (21 checks, in-process) | p95 < 10 ms | _pending_ | _pending_ |
| Buying power V2 evaluation (10 holdings, in-process) | p95 < 25 ms | _pending_ | _pending_ |
| Buying power V2 from the ledger (10 assets, 9 marked) | p95 < 25 ms | _pending_ | _pending_ |
| Ledger journal creation, 2 legs (sequential, committed) | p95 < 25 ms excluding contention | _pending_ | _pending_ |
| Ledger journal creation, 4 legs, 2 parties (P2P template) | p95 < 25 ms excluding contention | _pending_ | _pending_ |
| Matching engine throughput (simple GTC limit orders) | > 25,000 orders/s | _pending_ | _pending_ |
| Order-book replay determinism | replay deterministic | _pending_ | _pending_ |
| Option price + Greeks (Black–Scholes–Merton, exact fixed point) | measured (no target) | _pending_ | _pending_ |
| Concurrent reservation attempts on one wallet | 1,000+ concurrent attempts preserve invariants | _pending_ | _pending_ |
| Concurrent customer-to-customer transfers | 100 concurrent transfer attempts preserve invariants | _pending_ | _pending_ |
| Exactly-once effect claim under concurrency | retries yield no second effect | _pending_ | _pending_ |

## Test counts

`go run ./scripts/testcensus` and `go run ./scripts/testcensus -advanced`, at `80f616b` on 2026-10-08. The
census counts declarations and runs nothing; whether tests pass is what the commands above report. Subtests
and rapid cases are not counted separately, and "integration" includes the chaos- and e2e-tagged tests.

| Scope | Unit | Property | Fuzz | Integration | Total |
|---|---:|---:|---:|---:|---:|
| Whole repository (133 Go packages) | 2,766 | 203 | 79 | 1,146 | **4,194** |
| Advanced capabilities (28 packages) | 682 | 130 | 35 | 549 | **1,396** |

The web application declares 277 unit tests and 162 Playwright tests (same census). In the advanced figure,
`internal/ledger` and `internal/risk` include the core kernel's own tests, and the HTTP handler tests of the
advanced routes are counted under `internal/httpapi`, outside it.

## What this repository does not claim

Each item below is a claim a reviewer should reject if they see it made about this repository.

- **Real money.** No real funds have moved through it, no customer has been onboarded, and no balance in it is
  anyone's money.
- **Live trading.** No live trade has been placed, not even a canary. The V1 execution worker refuses to start
  in every provider mode (X1), and the Jupiter client has never been wrapped into a venue adapter.
- **A public chain.** Nothing has been broadcast to Solana mainnet, devnet or testnet, or to any other public
  network. "Devnet" here is the in-process simulated chain (CC1).
- **Real securities, leverage, derivatives, stablecoins, bridges or prediction-market stakes.** Each of these
  is a simulation in value domain `SIMULATED` (the one paragraph at the top).
- **An audit or a certification.** No external security audit, third-party penetration test, SOC 2 report or
  other attestation. The reviews in `docs/reviews/` and `docs/audit/` are internal (SEC7).
- **Regulatory status.** No licence, registration, exemption or legal opinion covers any capability here
  (money transmission, broker-dealer, derivatives, prediction markets, stable-value issuance).
- **Production readiness of the advanced capabilities.** The platform is NOT_READY and capital authority is
  DISABLED (`docs/PRODUCTION_READINESS_REPORT.md`).
- **Performance on production hardware or under production load.** No measurement in this repository was made
  on production-like infrastructure; see Benchmarks.
- **A running deployment.** Terraform validates in three environments but has never been planned or applied
  against AWS (EB-012), and the release workflow has never run.
- **Hardware-backed custody.** Every signer named here is in-process; the KMS signer has run only against a
  fake client.
- **Live provider integrations.** Stripe, Privy, Helius and Jupiter adapters are contract-tested against
  recorded fixtures; none is verified against the provider's live or sandbox service (MASTER_BUILD_STATE §11).
- **Model quality.** Nothing here measures how good an agent's or a model's proposals are; agents are
  untrusted proposal generators by design.
- **Whole-suite results at one commit.** The integration suite of every package in one run, `-race` across
  every advanced package, a bounded fuzzing campaign and the whole Playwright suite against one live API have
  not been recorded as single runs (`docs/build/ADVANCED_TEST_EVIDENCE.md`, "Not yet run as a whole"). At
  `80f616b` two integration packages failed as a whole (`internal/capital/buyingpower` and `internal/gates`),
  found by writing this document and repaired after it. Every claim above names the tests it rests on, and
  those were run.
