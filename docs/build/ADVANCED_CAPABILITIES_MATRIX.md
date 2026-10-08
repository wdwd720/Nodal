# ADVANCED CAPABILITIES MATRIX

Source goal: `NODAL_MEGA_GOAL_ADVANCED_FINANCIAL_SYSTEMS.md` (universal financial systems expansion).
Decision: `docs/adr/0020-advanced-financial-capabilities-sandbox-first.md`. Contract:
`docs/architecture/ADVANCED_FINANCIAL_MODEL.md`. Test evidence: `ADVANCED_TEST_EVIDENCE.md`.

Status vocabulary (goal §1.1): `NOT_STARTED` · `IN_PROGRESS` · `IMPLEMENTED_UNVERIFIED` · `VERIFIED_LOCAL`
(implemented, automated tests green against a real local PostgreSQL) · `VERIFIED_SANDBOX` (green against
an external provider's sandbox) · `BLOCKED_EXTERNAL` · `LIVE_CAPABLE_BUT_DISABLED` · `LIVE_ENABLED`.

**No row is `LIVE_CAPABLE_BUT_DISABLED` or `LIVE_ENABLED`.** For every advanced capability live is refused in
code (`gates.DecideMode` → `LIVE_NOT_IMPLEMENTED`) and unrepresentable in the database (D-049). The live
column below says what activation would need, never that it is near.

Last updated: 2026-10-08 — foundation, kernel amendments (00733), transfers + faucet (00744), margin (00740), derivatives (00741), prediction markets (00749), strategy marketplace (00750), sandboxed runs (00751) the internal order book (00742), the stable asset (00747) and bridges (00748) merged; other subsystem lanes in progress.

## Summary

| # | Capability (goal §) | Status | Mode in this build | Live blocker |
|---|---|---|---|---|
| 1 | Canonical instrument model (§4) | VERIFIED_LOCAL | n/a (model) | — |
| 2 | Typed economic intents (§4) | VERIFIED_LOCAL | PAPER/SANDBOX/DEVNET | — |
| 3 | Exactly-once economic effects (§23) | VERIFIED_LOCAL | all | — |
| 4 | Ledger V2 subledgers (§22) | VERIFIED_LOCAL | SIMULATED domain | — |
| 5 | Capability gates + mode policy (§28) | VERIFIED_LOCAL | — | — |
| 6 | Risk Kernel V2 (§21) | VERIFIED_LOCAL | — | — |
| 7 | Stable asset subsystem (§5) | VERIFIED_LOCAL (API in progress) | SANDBOX/DEVNET | B-16 |
| 8 | Margin + leverage engine (§6) | VERIFIED_LOCAL (API in progress) | PAPER/SANDBOX | B-15 |
| 9 | Derivatives: futures, perps, options (§7) | VERIFIED_LOCAL (API in progress) | PAPER/SANDBOX | B-15, B-17 |
| 10 | Equities abstraction (§8) | IN_PROGRESS | PAPER/SANDBOX | B-15, B-17 |
| 11 | Internal matching engine (§9) | VERIFIED_LOCAL (API in progress) | internal simulation | B-15 |
| 12 | Customer-to-customer transfers (§10) | VERIFIED_LOCAL (API in progress) | SANDBOX | B-15 |
| 13 | Principal / treasury book (§11) | IN_PROGRESS | PAPER/SANDBOX | B-15 |
| 14 | Transparent spread + fee engine (§12) | IN_PROGRESS (pure engine VERIFIED_LOCAL; fee ledger pending) | PAPER/SANDBOX | B-15 |
| 15 | Performance fee engine (§13) | IN_PROGRESS (pure engine VERIFIED_LOCAL; persistence pending) | PAPER/SANDBOX | B-15 |
| 16 | Strategy marketplace (§14) | VERIFIED_LOCAL (API in progress) | PAPER | B-15 |
| 17 | Sandboxed user code (§15) | VERIFIED_LOCAL (API in progress) | SANDBOX | B-19 |
| 18 | Delegated agent signing (§16) | NOT_STARTED | SANDBOX/DEVNET | B-18 |
| 19 | Delegated agent withdrawals (§17) | NOT_STARTED | SANDBOX/DEVNET | B-18 |
| 20 | Cross-chain buying power (§18) | VERIFIED_LOCAL (bridge; V2 wiring and API in progress) | SANDBOX/DEVNET | B-20 |
| 21 | Prediction markets (§19) | VERIFIED_LOCAL (API in progress) | PAPER/SANDBOX | B-15, B-17 |
| 22 | Universal buying power V2 (§20) | VERIFIED_LOCAL (margin wired; derivatives, bridge pending) | SIMULATED domain | — |
| 23 | Provider layer modes (§27) | IN_PROGRESS | fake/sandbox | B-17, B-20 |
| 24 | Observability (§29) | NOT_STARTED | — | — |
| 25 | API surface (§25) | IN_PROGRESS (core reads VERIFIED_LOCAL; per-subsystem lanes running) | — | — |
| 26 | Web application (§26) | IN_PROGRESS (advanced area lane running) | — | — |
| 27 | Demo scenarios A–I (§34) | IN_PROGRESS (`make advanced-demo`: transfer prologue and B pass) | — | — |

## Foundation detail

### 1. Canonical instrument model
- **Implementation:** `internal/instruments/canonical.go` (types SPOT_PAIR, STABLE_VALUE, EQUITY, ETF, FUTURE, PERPETUAL, OPTION, EVENT_OUTCOME = prediction contract, SYNTHETIC; versioned `Terms`).
- **DB:** `00732_canonical_instruments.sql` (`instrument_terms`, `instrument_versions`; IN001 advanced instruments settle only in SIMULATED value; IN002 consecutive versions).
- **Tests:** `TestTerms_EveryKindHasAValidShape`, `TestTerms_KindRulesFailClosed`, `TestTerms_HashIsCanonical`, `TestIntegration_CanonicalInstrumentsAreVersionedAndSimulatedOnly`.
- **Security boundary:** no AGENT actor may define or amend terms (CHECK on `instrument_versions.actor_type`).
- **Limitations:** symbol changes are a terms amendment; corporate-action adjustment of the multiplier is driven by the equities subsystem.

### 2–3. Economic intents and effects
- **Implementation:** `internal/econ` (`intent.go`, `effects.go`, `route.go`, `authorize.go`, `facility.go`, `evidence.go`).
- **DB:** `00731` (`economic_intents` + transitions, `economic_effects` + transitions; EF001/EF002).
- **Tests:** `internal/econ` unit and integration suites (exactly-once under 24-way concurrency, request-hash reuse refusal, journal requirement, SQL status machine, LIVE unrepresentable, evidence write-once).

### 4. Ledger V2
- **Implementation:** `internal/ledger/advanced.go` (67 codes, 50 kinds, owner types HOUSE/FACILITY, per-party balance).
- **DB:** `00731` (SM001, SM002, FA001; chart, owner type, facility kind and simulated-only kind parity functions).
- **Tests:** `TestCode_Chart`, `TestKind`, `TestIntegration_GoAndSQLAgreeOnTheChartOfAccounts`, `TestIntegration_GoAndSQLAgreeOnOwnerTypeAndFacilityKind`, `TestIntegration_GoAndSQLAgreeOnSimulatedOnlyKinds`, econ raw-SQL bypass tests.

### 5. Capability gates
- **Implementation:** `internal/gates/capability.go` (15 new capabilities, all high risk), `internal/gates/advanced.go` (`AdvancedSpecs`, `DecideMode`), `internal/killswitch` `CAPABILITY_DISABLE`.
- **Tests:** `TestCapabilities_And_HighRisk`, `TestDecideMode_Table`, `TestProp_LiveIsNeverAllowed`, `TestAuthorizer_KillSwitchAndGate`, `TestIntegration_AuthorizerReadsRealGatesAndKillSwitches`, gate parity integration tests (SQL high-risk list).
- **Per-gate encoding (goal §28):** environment (ungated LOCAL/TEST/DEV; gated STAGING/PROD), modes, evidence requirements, approvers (≥ 2 distinct), expiry (30-day max validity), maximum capital (0), jurisdiction (none analysed ⇒ none permitted), provider readiness (stated per capability). One environment variable never activates anything: configuration is condition 1 of 5 and LIVE is unimplemented.

### 6. Risk Kernel V2
- **Implementation:** `internal/risk/advanced.go`.
- **DB:** `advanced_risk_decisions` (append-only; verdict/reasons CHECK).
- **Tests:** `internal/risk/advanced_test.go`, `advanced_integration_test.go`.

### 8. Margin, leverage, borrowing, liquidation
- **Implementation:** `internal/margin` (pure engine: `Evaluate`, `ComputeHaircut`, `PlanLiquidation`, `RunStress`; service: accounts, collateral, borrow/repay with interest-first repayment, accrual, leveraged paper spot buys, mark-to-market, liquidation queue, step-by-step liquidation with a slippage cap and step budget, insurance-fund deficit cover, stress runs); `BuyingPowerReader` feeds buying power V2 from the engine's own evaluation.
- **DB:** `00740_margin.sql` (MG001–MG006; append-only histories; step budget and slippage caps as constraints; closed-case rule).
- **Tests:** property tests (`TestProp_LiquidationIsDeterministicReduceOnlyAndTerminates`, `TestProp_NoWithdrawalBreachesMaintenance`, `TestProp_MarginAvailableNeverExceedsCapacity`, `TestProp_LosingLeveragedPositionNeverIncreasesWithdrawableBuyingPower`, `TestProp_StaleMarksBlockNewLeverage`, `TestProp_EquityIdentityHolds`, `TestProp_HaircutIsMonotoneInEachFactor`); integration (`ScenarioB_FromFundingToACoveredDeficit`, `ConcurrentBorrowsCannotSpendTheSameMarginTwice`, `ConcurrentLeveragedBuysCannotSpendTheSameMarginTwice`, `InterestAccrualReplayIsIdempotent`, `StalePriceLiquidationIsRefused`, `DatabaseRefusesInvalidMarginStates`, `BuyingPowerV2SeesTheMarginEngine`); demo scenario B.
- **Limitations:** one margin account per customer account (ISOLATED margins one position); the service lends only the numeraire; derivatives cases go to MANUAL_REVIEW; deficit cover refuses when the insurance fund is short.

### 12. Customer-to-customer transfers and the sandbox faucet
- **Implementation:** `internal/transfers` (send with recipient rules, sender modes, handles, compliance screen, velocity and threshold step-up, Risk Kernel V2, reservation, posting template, effect, evidence; operator reversal by compensation; database-only reconciliation), `internal/sandboxfund` (faucet with a rolling per-account cap); one sandbox dollar owned by `econ`.
- **DB:** `00744_transfers.sql` (PT001–PT005, SF001–SF002), `00733` (multi-step transition binding, reservation ↔ economic intent, transfer notification kinds).
- **Tests:** `TestProp_TransfersAndReversalsConserveCustomerWallets`, `TestIntegration_ConcurrentTransfersCannotDoubleSpend` (50 goroutines, exactly 10 succeed), `TestIntegration_HundredConcurrentTransfersPreserveInvariants`, `TestIntegration_ADuplicateTransferIsIdempotent`, `TestIntegration_AReversalCompensatesAndNeverEditsHistory`, `TestIntegration_AnAgentNeverTransfersAndTheCompilerAgrees`, bypass tests for AU001/SM002/PT001–PT005, faucet cap and concurrency tests; demo scenario 0.
- **Limitations:** recipients must be CUSTOMER accounts; the default screen blocks only a configured list (no sanctions feed: B-06); a reversal that cannot proceed answers CONFLICT and records nothing.

### 22. Universal buying power V2
- **Implementation:** `internal/capital/buyingpower/v2.go`, `v2_load.go` (ledger + reservations + marks, SIMULATED domain only), margin wired through `margin.BuyingPowerReader`; served at `GET /v1/advanced/buying-power`.
- **Tests:** `v2_test.go`, `v2_integration_test.go`, `TestIntegration_BuyingPowerV2SeesTheMarginEngine`, `TestIntegration_AdvancedReadsServeTheRealStores`.

### 21. Prediction markets
- **Implementation:** `internal/predmarket` (market lifecycle DRAFT → OPEN → CLOSED → RESOLUTION_PENDING → RESOLVED/DISPUTED → SETTLED or VOIDED; complete-set issuance and redemption against escrow; Ed25519-verified sandbox oracle reports; disputes; settlement that pays winning shares one unit and losing shares nothing; void refunds with a disclosed rounding residual; escrow reconciliation). Positions are a view over the ledger, so they cannot drift from it. The creator can never resolve or void their own market, and a disputer never rules on their dispute.
- **DB:** `00749_prediction_markets.sql` (PM001–PM005; settlement rows check their own conservation in SQL).
- **Tests:** `TestProp_SettlementPaysExactlyTheEscrow`, `TestProp_PayoutConservation`, `TestProp_RefundConservesAndBoundsTheResidual`, `TestIntegration_ConcurrentSettlementsSettleOnce`, `TestIntegration_ADisputedMarketCannotSettle`, `TestIntegration_NoSettlementBeforeResolution`, `TestIntegration_OracleReplayIsIdempotentAndAConflictingReportIsRefused`, `TestIntegration_ReconciliationDetectsAnEscrowDivergence`, `TestIntegration_ScenarioH_CreateTradeCloseResolveSettle`, `FuzzDecodeOracleReport`.
- **Limitations:** settlement is all-or-nothing (a holder Risk V2 refuses blocks it until resolved); shares outside customer wallets or reserved by open orders make settlement refuse with RECONCILIATION_REQUIRED.

### 9. Derivatives: futures, perpetuals, options
- **Implementation:** `internal/derivatives` (contract specs, positions with fills as the single trade path, variation margin runs once per interval, perpetual funding with a premium index, European/American options with exact payoff grids, exercise and FIFO assignment, expiry settlement, a SIMULATED clearing house that absorbs one-sided books and nets to zero, reference analytics -- price and Greeks from `internal/quant` -- that are labelled and never posted). `MarginReader` feeds buying power V2 through `buyingpower.CombinedMarginReader` (D-054).
- **DB:** `00741_derivatives.sql` (DV001–DV004; one settlement per instrument, one funding accrual per position and interval).
- **Tests:** `TestProp_RoundTripRealizedPnLIsExact`, `TestProp_FundingConservesOnABalancedBook`, `TestProp_OptionPayoffIsExactAndPutCallParityHolds`, `TestProp_AssignFIFOConserves`, `TestProp_AnalyticsGreeksAreSane`, `TestProp_EveryPostingTemplateBalancesPerParty`, integration (`FuturesPnLIsExactThroughTheLedger`, `AmericanExerciseAndFIFOAssignmentConserve`, `OptionExpirySettlementIsExact`, `ConcurrentSettlementPostsOnce`, `VariationMarginRunsOncePerInterval`, `StaleOraclePreventsARiskIncreasingOrderButNotClosing`, `ConcurrentOpensCannotOverspendMargin`, `TheClearingHouseNetsToZeroForAMatchedBook`).
- **Limitations:** only cash-settled, 24x7 instruments with a whole-unit tick value; funding rounds down once per contract per interval; variation margin settles at the mark current when the run happens; every flow-producing derivative operation serializes on one clearing-house lock.

### 16–17. Strategy marketplace and sandboxed user code
- **Implementation:** `internal/sandboxrt` (static scan of strategy source with forbidden imports refused, a builder that compiles Go to WASM with an embedded SDK and an environment carrying nothing from the parent, a wazero runner with wall-time, memory (metered allocator), output-size and no-network/no-filesystem limits, output parsing into proposals checked against a manifest, proposals converted to economic intents that then go through the normal route/gate/Risk V2 path — untrusted code can only ever produce risk-checked intents, never a transfer or withdrawal) and `internal/marketplace` (listing lifecycle with review by someone other than the creator, immutable manifests, PAPER-only installs, paper-follow producing intents and evidence, forks with provenance, evaluations and performance snapshots).
- **DB:** `00750_strategy_marketplace.sql` (MK001–MK010), `00751_sandbox_runs.sql` (SX001: a run's violation count matches its violation rows).
- **Tests:** sandbox escape suite (`TestSandbox_FilesystemEscapeAttemptFails`, `…NetworkAttemptFails`, `…ForkBombBlocked`, `…CPUInfiniteLoopKilled`, `…MemoryBombKilled`, `…OversizedOutputKilled`, `…EnvironmentSecretsAbsent`, `…StrategyCannotConstructALiveSigningCall`), `TestProp_NoStrategyOutputBecomesATransferOrWithdrawal`, `TestSandbox_SameArtifactSameInputIsByteIdentical`, `FuzzScan`, `TestIntegration_Prop_UntrustedOutputOnlyBecomesRiskCheckedIntents`, `TestIntegration_CreatorCannotApproveOwnListing`, `TestIntegration_InstallIsPaperOnly`, `TestIntegration_ConcurrentFollowsOfOneSnapshotDispatchOnce`.
- **Measured:** cold start (compile) 1.16 s on a quiet machine, per-run p50 7.6 ms, p95 9.2 ms for a 4.9 MB strategy module (lane measurement; the performance report re-measures).
- **Limitations:** under `-race` the Go-built module tests skip (each compile takes about two minutes); host-side concurrency is still raced with hand-assembled modules. Any violation in a run rejects every intent from that run.

### 11. Internal matching engine and order book
- **Implementation:** `internal/matching` (pure deterministic CLOB: price-time priority, LIMIT/MARKET, GTC/IOC/FOK, post-only, reduce-only, self-trade prevention, sequencer, snapshot, replay to an identical state hash) and `internal/orders` (the persistent book: a journal of every command and event in the database, snapshots that must agree with the journal, staged submission through intent → route → gate → Risk V2 → reservation → execution → posting, house orders with a crossing control that refuses house-versus-customer crossing by default, the paper venue for spot paper fills with tamper-evident disclosures, and an audit proof per trade).
- **DB:** `00742_order_book.sql` (OB001–OB009; overfill, self-trade and crossing-without-a-rule refused by CHECKs; duplicate client order ids by a unique index).
- **Tests:** `TestIntegration_DeterministicReplayYieldsByteEquivalentBookStateThroughTheDB`, `TestProp_NoCrossedRestingBookAfterEveryCommandThroughTheService`, `TestProp_QuantitiesAreConservedAndNoOrderOverfillsThroughTheService`, `TestIntegration_FIFOWithinAPriceLevel`, `TestIntegration_SelfTradePreventionNeverTradesAnOwnerWithItself`, `TestIntegration_CancelRaceHasExactlyOneOutcome`, `TestIntegration_ConcurrentSubmitsReplayToTheLiveStateHash`, `TestProp_CustomerWalletsChangeOnlyByFeesAcrossRandomTrading`, `TestIntegration_HouseCustomerCrossingIsRejectedByDefault`, `TestIntegration_ScenarioD_TwoSandboxUsersTradeWithAnAuditProof`, fuzzers for order conversion and command/event decoding.
- **Measured (lane, shared machine):** matching engine about 729k commands/s; persistent staged pipeline 96 orders/s on one book (p95 582 ms), bounded by the book row lock held through commit and the ledger's deferred triggers.
- **Limitations:** one book's throughput is capped by its row lock; StateHash is O(book size) per command; market orders need a protection price; house orders cannot be replaced.

### 7. Stable asset subsystem (and the simulated chains it issues on)
- **Implementation:** `internal/devnethost` (a persistent host for the simulated chains: every chain-changing call is one event in an append-only, gapless, hash-chained `devnet_events` log written in its own transaction; the network is rebuilt by replaying that log and refuses to run on a history that does not replay to the recorded state hashes; fault injection only in LOCAL/TEST/DEV) and `internal/stableasset` (registry with mint/burn/redemption authority modes, reserve assets, a reserve ledger, mint and burn as two-phase operations around a devnet submission whose UNKNOWN outcome is reconciled rather than retried, peg monitoring with alerts, hash-chained signed reserve snapshots — attestations — and supply reconciliation against the chain).
- **DB:** `00747_stable_assets.sql` (DH001; SV001–SV007: supply above the cap, issuance without full backing and redemptions that overpay are refused by the database).
- **Tests:** property and integration suites in both packages (reserve invariant, supply cap, no double counting, unknown submissions, snapshot chain verification, replay, append-only log).

### 20. Cross-chain capital (bridges)
- **Implementation:** `internal/bridge` (routes, legs with a state machine from AVAILABLE through BRIDGE_REQUESTED, SOURCE_FINALIZED, MESSAGE_IN_FLIGHT, DESTINATION_OBSERVED/FINALIZED to RECONCILED or FAILED/MANUAL_REVIEW; lock-and-release over the devnet with attested releases inspected before submission; refunds; route reconciliation of `BRIDGE_LOCKED = BRIDGE_OUTSTANDING + Σ BRIDGE_IN_FLIGHT`; a cross-chain buying power evaluation where in-flight value counts only at the policy factor).
- **DB:** `00748_bridges.sql` (BR001–BR005; a requested leg carries its lock).
- **Tests:** `TestProp_IntegrationNoDoubleCounting`, `TestProp_EachLegIsCountedOnceWhereItsValueIs`, `TestProp_PostingsConserveValueAcrossChains`, `TestIntegration_AFailedLockRestoresTheSourceState`, `TestIntegration_AFailedReleaseIsRefundedAndRestoresTheSourceState`, `TestIntegration_ADelayedBridgeCreatesNoPhantomCapital`, `TestIntegration_AReorgNeverRevertsFinalityOrDoubleCounts`, `TestIntegration_ADuplicateReleaseMintsNothingTwice`, `TestIntegration_ConcurrentDuplicatesSettleOnce`, `TestHaircut_InFlightValueCountsOnlyAtThePolicyFactor`, `TestIntegration_BridgeReconciliationDetectsAnOutOfBandLock`, `TestIntegration_TheDatabaseRefusesBridgeBookkeepingBypasses`, `TestIntegration_ARequestedLegCarriesItsLock`.
- **Limitations:** DEVNET legs and unknown submissions advance only when `ObservePending` runs (no scheduler yet); every bridge posting serializes on its route's row lock.
