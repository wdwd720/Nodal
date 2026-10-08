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

Last updated: 2026-10-07 — foundation committed (`964eadb`); subsystem lanes in progress.

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
| 8 | Margin + leverage engine (§6) | IN_PROGRESS | PAPER/SANDBOX | B-15 |
| 9 | Derivatives: futures, perps, options (§7) | IN_PROGRESS | PAPER/SANDBOX | B-15, B-17 |
| 10 | Equities abstraction (§8) | IN_PROGRESS | PAPER/SANDBOX | B-15, B-17 |
| 11 | Internal matching engine (§9) | IN_PROGRESS | internal simulation | B-15 |
| 12 | Customer-to-customer transfers (§10) | IN_PROGRESS | SANDBOX | B-15 |
| 13 | Principal / treasury book (§11) | IN_PROGRESS | PAPER/SANDBOX | B-15 |
| 14 | Transparent spread + fee engine (§12) | IN_PROGRESS | PAPER/SANDBOX | B-15 |
| 15 | Performance fee engine (§13) | IN_PROGRESS | PAPER/SANDBOX | B-15 |
| 16 | Strategy marketplace (§14) | IN_PROGRESS | PAPER | B-15 |
| 17 | Sandboxed user code (§15) | IN_PROGRESS | SANDBOX | B-19 |
| 18 | Delegated agent signing (§16) | NOT_STARTED | SANDBOX/DEVNET | B-18 |
| 19 | Delegated agent withdrawals (§17) | NOT_STARTED | SANDBOX/DEVNET | B-18 |
| 20 | Cross-chain buying power (§18) | NOT_STARTED | SANDBOX/DEVNET | B-20 |
| 21 | Prediction markets (§19) | NOT_STARTED | PAPER/SANDBOX | B-15, B-17 |
| 22 | Universal buying power V2 (§20) | NOT_STARTED | — | — |
| 23 | Provider layer modes (§27) | IN_PROGRESS | fake/sandbox | B-17, B-20 |
| 24 | Observability (§29) | NOT_STARTED | — | — |
| 25 | API surface (§25) | NOT_STARTED | — | — |
| 26 | Web application (§26) | NOT_STARTED | — | — |
| 27 | Demo scenarios A–I (§34) | NOT_STARTED | — | — |

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
