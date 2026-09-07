# PRODUCTION READINESS REPORT

Against `gola.md`. Written at the point where the Nodal-native economy exists and the rest of the
final architecture does not yet.

`gola.md` PART XCVIII requires six readiness levels to be distinguished rather than collapsed. They
are not synonyms and this report does not treat them as such.

| Level | Status |
|---|---|
| **CODE READY** | **NO** — Stages 8, 10, 12, 15, 16, 17 and 20–24 are not built |
| **SANDBOX READY** | **PARTIAL** — the internal economy runs end to end against a sandbox payout provider; no external provider sandbox is integrated |
| **STAGING READY** | **NO** — no staging deployment of the new subsystems has been exercised |
| **PROVIDER READY** | **NO** — no payout or hosted provider contract exists (BLOCKERS B-01, B-05) |
| **LEGAL READY** | **NO** — no jurisdiction or payout-eligibility determination exists (B-02, B-03, B-07) |
| **LIVE CAPITAL READY** | **NO**, and it must remain NO |

`SoftwareComplete` (PART XCIX) is **FALSE**. `LiveReady` (PART C) is therefore **FALSE**, and would
remain false even if the software were complete, because every external condition is open.

---

## 1. What is genuinely proven

Each row is a property with an executable test behind it, not a claim.

### Value domains

| Property | Evidence |
|---|---|
| Internal Credits can never share a transaction with real capital — no capability, policy or approval permits it | `TestIsolation_CreditsCanNeverReachRealCapital`; and at the database layer, `TestIntegration_TheDatabaseRefusesCreditsReachingRealCapital` writes a properly balanced Credits-to-SOL swap by hand **through the migration role** and watches it refused six ways, including when disguised as a legitimate declared conversion |
| Simulated capital never mixes with anything | `TestIsolation_SimulatedNeverMixesWithAnything` |
| A cross-domain movement must declare which conversion it is | `TestIsolation_CrossDomainPostingMustDeclareItself`, `TestIntegration_CrossDomainPostingMustDeclareItself` |
| The rule holds over the entire ordered domain × domain × capability-subset space | `TestProp_IsolationIsExhaustiveOverEveryPairAndCapabilitySubset` — exhaustive, not sampled |
| Go and SQL agree on every one of the 64 ordered pairs | `TestIntegration_GoAndSQLAgreeOnEveryOrderedDomainPair` |
| Content hashes computed before the migration still verify | the conversion is omitted from the canonical form when absent; `TestCanonicalContent_Golden` unchanged |

### Credits and provenance

| Property | Evidence |
|---|---|
| 100 concurrent spends of 1,000 against a 10,000 balance yield exactly 10 successes | `TestIntegration_HundredConcurrentSpendsAgainstInsufficientBalance`, also under `-race` |
| Provenance and the ledger agree after every scenario | `VerifyProvenance`, asserted in every credit and market test |
| Chargeback after the Credits are spent produces a recorded DEFICIT, never a negative balance | `TestIntegration_ChargebackAfterTheCreditsAreSpent` — 10,000 bought, 7,000 spent, 3,000 destroyed, 7,000 owed |
| A duplicated provider webhook cannot mint twice | `TestIntegration_MintingIsExactlyOnceUnderWebhookReplay` |
| Captured card funding is spendable and never payout-eligible | `TestFinality_ReversibleValueIsSpendableButNeverPayable` |
| Reversed value never comes back to life | enforced in Go and by SQLSTATE CR003 |
| A lot cannot be over-consumed or over-restored | SQLSTATE CR001, tested through the migration role |

### The market engine

| Property | Evidence |
|---|---|
| The constant product never falls, over random trade sequences, with and without fees | `TestProp_RealReserveNeverGoesNegative`, `TestProp_InvariantAndConservationHoldWithFees` |
| No trade sequence creates Credits or asset units | value- and supply-conservation assertions in `simulate`; supply conservation re-checked after 30 mixed trades and 25 concurrent buyers |
| 2,000 tiny round trips cannot extract value from rounding | `TestProp_ManyTinyTradesCannotExtractValue` |
| Full liquidation leaves the reserve solvent | `TestProp_FullLiquidationLeavesTheReserveSolvent` |
| Fuzzing finds no invariant break | 4.7M executions in 45 s, `FuzzCurve_NeverBreaksTheInvariant` |
| A stale fill is refused by the database | SQLSTATE NM002, written by hand through the migration role |
| A fill below the constant product is refused by the database | SQLSTATE NM001, likewise |
| A creator cannot change supply, allocation, symbol, policy or fees after launch | SQLSTATE NM003, six separate attempts through the migration role |
| A retried order does not re-trade | `TestIntegration_ExecutionIsIdempotent` — version moves once |
| Close-only lets holders out and refuses entry | `TestIntegration_CloseOnlyLetsHoldersOutAndNobodyIn` |
| With `NATIVE_MARKET_TRADING` off, no trade commits | `TestIntegration_WithoutTheCapabilityNoTradeCommits` |

### Payouts

| Property | Evidence |
|---|---|
| A payout reserves exactly once and the reserved value is unspendable | `TestIntegration_PayoutReservesExactlyOnce` |
| Promotional Credits can never cash out | `TestIntegration_PromotionalCreditsCannotCashOut` |
| An account with 30,250 Credits of four provenances can withdraw exactly the 250 the policy permits | `TestIntegration_PayoutEnforcesProvenance` |
| A payout approved for creator earnings cannot sweep up a promotional grant | same test — allocations are asserted, not just totals |
| A provider timeout does not duplicate the payout | `TestIntegration_AProviderTimeoutDoesNotDuplicateThePayout` — provider records one payout, response is lost, resubmission is absorbed, reconciliation resolves it |
| An unreachable provider does not release the reservation | `TestIntegration_AnUnreachableProviderLeavesTheReservationAlone` |
| A failed payout returns the exact units to the exact lots | `TestIntegration_AFailedPayoutReturnsTheExactUnits` |
| A submitted payout cannot be cancelled | `TestIntegration_ASubmittedPayoutCannotBeCancelled` |
| With every payout capability revoked, reserved value is still returnable | `TestIntegration_TheUnwindPathIsNeverGated` |
| Production cannot load a provider with no contract, or one that cannot answer a lookup | `TestRegistry_ProductionRefusesAProviderWithNoContract`, `TestRegistry_RefusesAProviderThatCannotAnswerLookups` |

### Policy and authority

| Property | Evidence |
|---|---|
| A fresh deployment can pay nobody out | `TestPolicy_DefaultPermitsNothingEvenForAPerfectUser` |
| An agent can never withdraw, transfer, raise its own limits or bypass risk, at any level with every capability active | `TestPermits_ForbiddenActionsAreRefusedAtEveryLevelWithEveryCapability` |
| Agent levels 4–6 are refused even with their capability on | `TestPermits_DisabledLevelsRefuseEvenWithTheirCapabilityOn` |
| A capability policy cannot fall through to permission | `TestPolicy_MustEndWithACatchAllDeny` |
| A policy row cannot be the only thing between a user and value | `TestPolicy_AnAllowMustRequireACapability` |
| Policy says yes + gate off = no | `TestRoute_PolicySaysYesAndTheGateSaysNo` |

## 2. Suite results

Executed at the commit this report accompanies:

| Suite | Command | Result |
|---|---|---|
| Compile | `go build ./...` | exit 0 |
| Vet | `go vet ./...` and `go vet -tags=integration ./...` | exit 0 |
| Unit | `go test -short ./internal/... ./cmd/...` | all packages ok |
| Integration | `go run ./scripts/inttest` — 45 packages, one fresh database each | all ok |
| Race | `-race` over credit, nativemarket, payout | ok |
| Fuzz | `FuzzCurve_NeverBreaksTheInvariant`, 45 s | 4.7M execs, no failures |
| Security | `test/security` (cross-tenant, forged sessions, SQL-source constancy, agent escalation, webhook forgery) | ok, and on three consecutive runs against the same database |

Every new suite was run **twice against the same database** before being believed. That rule caught
two defects in this migration alone (F-08, F-13) and had caught a real key-rotation defect in an
earlier one.

## 3. What is NOT ready, stated plainly

### Not built

- **Internal commerce / creator economy** (Stage 8). JOURNEY C cannot be walked. The provenance
  origins it would produce exist and are already handled differently from speculative proceeds by the
  payout engine, so this is construction rather than design.
- **Hosted partner rail** (Stage 10). Declared, `Implemented() == false`, no adapter.
- **Domain A is not routed through the Settlement Compiler** (Stage 12). This is the largest
  architectural gap: `internal/settlement` is the declared moat and Domain A commands currently
  bypass it. Until that is done, the risk kernel, eligibility engine and compliance decision do not
  sit in front of a native-market trade the way they sit in front of an external one.
- **No frontend for Domain A** (Stage 16). PART LII's rule — that Nodal Economy, Simulated and Real
  Capital are never summed — is enforced in the API's response shapes and is not yet enforced in a
  UI, because there is no UI.
- **No admin workflows for Domain A** (Stage 17): freeze market, close-only, disable asset,
  moderation verdict, payout manual review.
- **Reality Engine and Prediction Ledger are not integrated with Domain A** (Stage 15).

### Not run

- Chaos and load testing of the new subsystems (Stages 20–21).
- The backup/restore drill against the new tables.
- `govulncheck`, `gosec`, `gitleaks`, `trivy`, SBOM in this session.
- Terraform validation in this session.
- The PART LXXII adversarial list is partially covered: items 1–4, 7–11, 15–17, 20–22, 28 have named
  tests; items 5, 6, 12–14, 18, 19, 23–27, 29, 30 do not yet.

### Externally blocked

Eight items, each with what would unblock it, in `docs/build/BLOCKERS.md`. In summary: no payout
provider contract, no determination of whether any Credit provenance may be withdrawn, no
determination on pre-KYC market participation, no live payment credentials, no hosted partner, no
identity verification provider, no jurisdiction matrix, no independent security review.

## 4. The capability state of a fresh deployment

Twenty capabilities are declared. **A fresh deployment has no gate rows, and a capability with no row
is INACTIVE.** Nothing in the internal economy can move value until three distinct principals have
proposed, approved and activated a gate with its evidence attached, and migration 00701 refuses to
let a gate row be born in any state but DISABLED.

The default payout policy forbids every origin. The default legal-router policy permits simulation
and denies everything else. Neither is a placeholder: they are the absence of a decision, which is
what PART LXIII requires a fresh production deployment to start from.

## 5. Why the rest of the PART XCVIII package is not here

PART XCVIII lists nine release documents. Producing `LAUNCH_GATE.md`, `RECOVERY_EVIDENCE.md` and
`PROVIDER_READINESS.md` now would mean writing nine documents whose honest content is "not yet", and
a launch package that says "not yet" nine times is a launch package that teaches its readers not to
read it.

The two that carry real content today — the findings and the blockers — exist as
`docs/audit/AUDIT_FINDINGS.md` and `docs/build/BLOCKERS.md`. The rest become worth writing when
`SoftwareComplete` is within reach, which is after Stages 8, 12, 16, 17 and 20–21.
