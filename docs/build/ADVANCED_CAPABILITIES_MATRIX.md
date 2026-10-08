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

Last updated: 2026-10-08 — foundation, kernel amendments (00733), transfers + faucet (00744), margin (00740) and prediction markets (00749) merged; other subsystem lanes in progress.

## Summary

| # | Capability (goal §) | Status | Mode in this build | Live blocker |
|---|---|---|---|---|
| 1 | Canonical instrument model (§4) | VERIFIED_LOCAL | n/a (model) | — |
| 2 | Typed economic intents (§4) | VERIFIED_LOCAL | PAPER/SANDBOX/DEVNET | — |
| 3 | Exactly-once economic effects (§23) | VERIFIED_LOCAL | all | — |
| 4 | Ledger V2 subledgers (§22) | VERIFIED_LOCAL | SIMULATED domain | — |
| 5 | Capability gates + mode policy (§28) | VERIFIED_LOCAL | — | — |
| 6 | Risk Kernel V2 (§21) | VERIFIED_LOCAL | — | — |
| 7 | Stable asset subsystem (§5) | IN_PROGRESS | SANDBOX/DEVNET | B-16 |
| 8 | Margin + leverage engine (§6) | VERIFIED_LOCAL (API in progress) | PAPER/SANDBOX | B-15 |
| 9 | Derivatives: futures, perps, options (§7) | IN_PROGRESS | PAPER/SANDBOX | B-15, B-17 |
| 10 | Equities abstraction (§8) | IN_PROGRESS | PAPER/SANDBOX | B-15, B-17 |
| 11 | Internal matching engine (§9) | IN_PROGRESS (pure engine VERIFIED_LOCAL; persistent book in progress) | internal simulation | B-15 |
| 12 | Customer-to-customer transfers (§10) | VERIFIED_LOCAL (API in progress) | SANDBOX | B-15 |
| 13 | Principal / treasury book (§11) | IN_PROGRESS | PAPER/SANDBOX | B-15 |
| 14 | Transparent spread + fee engine (§12) | IN_PROGRESS (pure engine VERIFIED_LOCAL; fee ledger pending) | PAPER/SANDBOX | B-15 |
| 15 | Performance fee engine (§13) | IN_PROGRESS (pure engine VERIFIED_LOCAL; persistence pending) | PAPER/SANDBOX | B-15 |
| 16 | Strategy marketplace (§14) | IN_PROGRESS | PAPER | B-15 |
| 17 | Sandboxed user code (§15) | IN_PROGRESS | SANDBOX | B-19 |
| 18 | Delegated agent signing (§16) | NOT_STARTED | SANDBOX/DEVNET | B-18 |
| 19 | Delegated agent withdrawals (§17) | NOT_STARTED | SANDBOX/DEVNET | B-18 |
| 20 | Cross-chain buying power (§18) | NOT_STARTED | SANDBOX/DEVNET | B-20 |
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
