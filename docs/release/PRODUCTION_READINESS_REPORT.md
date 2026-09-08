# PRODUCTION READINESS REPORT

Against `gola.md`. Written at the point where the Nodal-native economy exists and the rest of the
final architecture does not yet.

`gola.md` PART XCVIII requires six readiness levels to be distinguished rather than collapsed. They
are not synonyms and this report does not treat them as such.

| Level | Status |
|---|---|
| **CODE READY** | **NO** — Stages 10, 15, 16 and 20–24 are not built |
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
| 100 concurrent spends of 1,000 against a 10,000 balance yield exactly 10 successes | `TestIntegration_HundredConcurrentSpendsAgainstInsufficientBalance` — asserted by OUTCOME (exactly ten commits), and separately under `-race` |
| 10 concurrent purchases against a balance that funds 2 yield exactly 2 | `TestIntegration_ConcurrentPurchasesCannotOverspend` |
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

### Internal commerce (the creator economy)

| Property | Evidence |
|---|---|
| A sale turns the buyer's `PURCHASED` Credits into the seller's `DATA_SALE_EARNING` — the only legitimate way an earning provenance comes into existence | `TestIntegration_ASaleMovesCreditsAndRecordsCreatorProvenance` |
| The product kind decides the provenance, for every declared kind, end to end | `TestIntegration_TheProductKindDecidesTheProvenance` — all eight kinds, each asserting the lot that lands |
| An account cannot buy from itself, in Go **and** at the database, which refuses it for the schema owner too | `TestIntegration_AnAccountCannotBuyFromItself` — CHECK `internal_commerce_orders_no_self_dealing` |
| Nor indirectly, by attributing a third party's earnings back to the buyer | `TestIntegration_SelfDealingThroughAPayoutAccountIsRefused` |
| A published product's price, fee, kind, version and publication instant are immutable | SQLSTATE IC002, five separate attempts through the migration role |
| An order that claims an amount its posting does not show is refused at COMMIT | SQLSTATE IC001, three forgeries refused and the genuine numbers accepted against the same posting |
| Commerce history cannot be edited or deleted | `TestIntegration_AnOrderIsImmutable` |
| A retried purchase charges once: one order, one posting, one earning lot | `TestIntegration_APurchaseIsIdempotent` |
| 10 concurrent purchases against a balance that funds 2 commit exactly twice | `TestIntegration_ConcurrentPurchasesCannotOverspend` |
| Value whose funding is disputed cannot buy anything | `TestIntegration_DisputedCreditsCannotBuyAnything` |
| A stale price is a refusal, never a surprise charge | `TestIntegration_APriceTheBuyerDidNotAgreeToIsRefused` |
| A suspended seller takes no orders, and WITHDRAWN is terminal | `TestIntegration_ASuspendedSellerTakesNoOrders`, `TestIntegration_OnlyAnActiveProductCanBeBought` |
| The platform's fee rounds DOWN, and the two halves always add back to the price | `TestSplit_AlwaysAddsBackToThePrice` over 400 prices × every fee step; `TestIntegration_TheFeeRoundsTowardTheCreator` on the ledger |
| A purchase commits with NO capability active, because nothing converts | `TestIntegration_APurchaseIsASingleDomainMovement` — every leg INTERNAL_CREDIT, conversion columns null |
| Go and SQL declare the same kinds, statuses and permitted origins | `TestMigration_DeclaresTheSameKindsAsGo` and siblings, which read the migration out of the embedded FS |
| Earnings paid reconcile against provenance issued | `VerifyEarnings`, asserted after every scenario |

### The Settlement Compiler's rail dispatch

| Property | Evidence |
|---|---|
| Every declared action type has exactly one routing profile, and its domain lives on the rail it routes to | `ValidateCompiler`, asserted by `TestValidateCompiler_TheRoutingTableIsTotal` and re-checked per action |
| No action can be routed to a rail this build cannot execute on, whatever the policy says | `TestCompile_NoActionRoutesToAnUnimplementedRail` — hosted trading is refused with every capability active and a policy that permits it |
| A fresh deployment permits exactly one thing: simulation | `TestCompile_AFreshDeploymentPermitsOnlySimulation` — every other action refused under the conservative policy with no gate active |
| An agent can never request a payout, at any authority level, with every capability active | `TestCompile_AnAgentCanNeverRequestAPayout` |
| An agent below its action's level is refused, and the same agent one level up is not | `TestCompile_AnAgentBelowItsActionsLevelIsRefused` |
| A declared capital domain that disagrees with the action is refused, over the whole action × domain space | `TestCompile_EveryDomainMismatchIsRefused` — exhaustive, not sampled |
| Policy permitting is not enough, and the gate being on is not enough | `TestCompile_ThePolicySayingYesIsNotEnough` |
| A gate being off is reported as a gate problem, not as a policy refusal | `TestCompile_AGateBeingOffIsNotAPolicyRefusal` (F-17) |
| Every legal-router outcome is handled, and an unhandled one denies | `TestCompile_EveryRouterOutcomeIsHandled` — over `AllOutcomes()` |
| A route awaiting a person's confirmation does not read as executable | `TestCompile_ARouteRequiringConfirmationIsNotPermissionToExecute`, `MayExecuteNow` |
| Compile is deterministic, and reports every problem at once | `TestCompile_IsDeterministic` (50 iterations), `TestCompile_ReportsEveryProblemAtOnce` |
| `Permitted` and `Reasons` can never disagree | `TestCompile_PermittedAndReasonsCanNeverDisagree` — over routers × capability sets × actions × verification levels |
| Every Domain A command passes through the compiler before its domain service | `TestIntegration_AFreshDeploymentSellsNothing`, `TestIntegration_TheGateAndThePolicyMustBothAgree` — over HTTP, with the real commerce service behind it |
| A refusal names the policy version and the rule index that produced it | same tests, asserted on the response body |

### Operator controls on the internal economy

| Property | Evidence |
|---|---|
| One operator can halt, close-only or freeze a market, and the market actually changes state | `TestIntegration_OneOperatorCanHaltAMarket`, `TestIntegration_EveryStoppingControlIsOneOperator` |
| A halted market refuses to quote | `TestIntegration_OneOperatorCanHaltAMarket` — asserted against the engine, not the status code |
| Resuming takes two distinct principals, and the proposer cannot approve their own resume even with a live break-glass elevation | `TestIntegration_ResumingAMarketTakesTwoPeople` |
| An approval does not override the market's own transition table: FROZEN → ACTIVE stays refused | `TestIntegration_ApprovalDoesNotOverrideTheMarketsOwnTransitionTable` |
| A moderation verdict is recorded and does not start or stop trading | `TestIntegration_AModerationVerdictIsRecordedAndDoesNotTrade` |
| An undeclared moderation state is refused and nothing is written | `TestIntegration_AnUnknownModerationStateIsRefused` |
| Suspending a seller stops new orders and leaves past earnings untouched | `TestIntegration_SuspendingASellerStopsNewOrdersAndKeepsPastEarnings` — provenance re-verified afterwards |
| A withdrawn product is terminal | `TestIntegration_WithdrawingAProductIsTerminal` |
| No executor runs outside `admin.Execute`; each reads its target from the stored action | `TestIntegration_AnExecutorRefusesOutsideExecute` — over every registered executor |
| A deployment without the internal economy registers no executors and answers UNSUPPORTED | `TestIntegration_ADeploymentWithoutTheInternalEconomyRegistersNoExecutors` |
| A stuck payout can be failed, rejected or retried — never declared settled | `TestIntegration_AManualReviewCanNeverBeDeclaredSettled` (five spellings refused) |
| Failing a manual review returns the exact reserved units to the exact lots | `TestIntegration_FailingAManualReviewReturnsTheExactUnits` |
| A payout that may already be at the provider can never be retried | `TestIntegration_APayoutThatMayHaveBeenSubmittedCannotBeRetried` |
| Every hand resolution carries a reason | `TestIntegration_AResolutionRequiresAReason` |
| Stopping is one signature and restarting is two, across the whole kind table | `TestKindTable_Golden` |

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

Every row states which of three things it is: **run here** (this host, this session), **runs in CI**
(and not here), or **intended**. Collapsing those three is what F-14 was about, so they are never
collapsed again. Everything below is *run here* unless it says otherwise.

| Suite | Command | Result |
|---|---|---|
| Compile | `go build ./...` | exit 0 |
| Vet | `go vet ./...` and `go vet -tags=integration ./...` | exit 0 |
| Unit | `go test -short ./internal/... ./cmd/...` | all packages ok |
| Integration | `go run ./scripts/inttest` — 46 packages, one fresh database each | all ok |
| Race | `go run ./scripts/inttest -race -pkg '^\./internal/(credit\|nativemarket\|payout\|commerce)$'` and the same over `capital, ledger, execution, reconciliation, event, settlement, signing, httpapi` | all ok. This needed a C compiler on a space-free path first — see F-14 for why the obvious install did not work. |
| Fuzz | `FuzzCurve_NeverBreaksTheInvariant`, 45 s | 4.7M execs, no failures |
| Security | `test/security` (cross-tenant, forged sessions, SQL-source constancy, agent escalation, webhook forgery) | ok, and on three consecutive runs against the same database |

Every new suite was run **twice against the same database** before being believed. That rule caught
two defects in this migration alone (F-08, F-13) and had caught a real key-rotation defect in an
earlier one.

## 3. What is NOT ready, stated plainly

### Not built

- **Hosted partner rail** (Stage 10). Declared, `Implemented() == false`, no adapter.
- **No commerce moderation tooling** (part of Stage 17). `commerce:moderate` exists as a permission
  and `internal_sellers.status` and the product lifecycle support suspension and withdrawal, but no
  admin workflow drives them: an operator would have to run SQL. The controls exist; the operator
  interface to them does not.
- **Domain C is not yet expressed as a `FinancialIntent`** (the second half of Stage 12). Domain A
  now compiles through `settlement.Compile`, and external spot swaps still reach `V1Planner` through
  the older `IntentSnapshot` path. The compiler routes them correctly; what is missing is the single
  entry point, so `intent.TradeIntent` and `settlement.FinancialIntent` currently coexist.
- **The risk kernel is not yet an input to the Domain A route.** `Route.RequiresRiskEvaluation` is
  determined and recorded; nothing consumes it for internal trades yet.
- **No frontend for Domain A** (Stage 16). PART LII's rule — that Nodal Economy, Simulated and Real
  Capital are never summed — is enforced in the API's response shapes and is not yet enforced in a
  UI, because there is no UI.
- **No kind-SPECIFIC admin screens for Domain A** (part of Stage 16). The nine administrative
  actions are fully operable from the existing console today: its propose form is driven by the
  generated `authority.json`, which now lists all nine, and it carries a free-form params field that
  the two parameterised kinds need. What is missing is kind-specific UI — a market picker rather than
  a pasted uuid, a moderation-state dropdown rather than hand-written JSON. That is a usability gap,
  not a missing capability, and an operator pasting the wrong uuid into a freeze is the risk it
  leaves open.
- **Reality Engine and Prediction Ledger are not integrated with Domain A** (Stage 15).

### Not run

- Chaos and load testing of the new subsystems (Stages 20–21).
- The backup/restore drill against the new tables.
- `govulncheck`, `gosec`, `gitleaks`, `trivy`, SBOM in this session.
- Terraform validation in this session.
- The PART LXXII adversarial list: **27 of 30 covered**, mapped item by item in §6 below. Items 5,
  14 and 23 are not covered and are named there rather than left to inference.

### Externally blocked

Eight items, each with what would unblock it, in `docs/build/BLOCKERS.md`. In summary: no payout
provider contract, no determination of whether any Credit provenance may be withdrawn, no
determination on pre-KYC market participation, no live payment credentials, no hosted partner, no
identity verification provider, no jurisdiction matrix, no independent security review.

## 3a. PART LXXII adversarial coverage, item by item

Every row names the test. The previous version of this document summarised this list in one sentence
and got four items wrong in both directions — it claimed items that had no test and denied items
whose tests were already cited in the evidence tables above. That is F-18. A summary that cannot be
checked is worse than a table that can.

| # | Scenario | Test |
|---|---|---|
| 1 | 100 concurrent Credit spends against insufficient balance | `TestIntegration_HundredConcurrentSpendsAgainstInsufficientBalance` |
| 2 | 100 concurrent native-asset buys | `TestIntegration_ConcurrentBuyersSerialiseWithoutBreakingAnything` |
| 3 | duplicate order request | `TestIntegration_ExecutionIsIdempotent`, `TestIntegration_APurchaseIsIdempotent` |
| 4 | duplicate webhook | `TestProp_DuplicateWebhookOneEffect`, `TestContract_DuplicateWebhook` |
| 5 | out-of-order webhook | **none** |
| 6 | provider timeout after success | `TestIntegration_AProviderTimeoutDoesNotDuplicateThePayout` |
| 7 | process crash after provider success | `TestIntegration_Part49_CrashRecovery`, `TestExecutor_ResumeAfterCrash_SubmitRunningNeverResubmits` |
| 8 | chargeback after Credits spent | `TestIntegration_ChargebackAfterTheCreditsAreSpent` |
| 9 | payout request during pending chargeback | `TestIntegration_APayoutIsRefusedWhileTheFundingIsDisputed` |
| 10 | payout using promotional Credits | `TestIntegration_PromotionalCreditsCannotCashOut` |
| 11 | payout using ineligible trading proceeds | `TestIntegration_PayoutEnforcesProvenance` |
| 12 | market creator attempting hidden supply increase | `TestIntegration_ACreatorCannotChangeEconomicsAfterLaunch` — "raise max supply", through the migration role |
| 13 | self-trade | `TestIntegration_SurveillanceRaisesAlertsWithoutBlocking` — `CREATOR_SELF_DEALING` raised, trade not blocked |
| 14 | two accounts under one controlled actor wash-trading | **none** — the `WASH_TRADE` alert kind exists and nothing proves it fires |
| 15 | stale quote | `TestContract_Order_StaleQuote`, `TestIntegration_QuotesAreRecordedAndExpire` |
| 16 | market state changes between quote and execution | `TestIntegration_AStaleFillIsRefusedByTheDatabase` (SQLSTATE NM002) |
| 17 | integer overflow | `TestCurve_ExtremeSizesDoNotOverflow`, and `money` is integer-only by construction |
| 18 | rounding edge at minimum unit | `TestSplit_RoundsTowardTheCreator` (price 1, 3 and 4 at the 30% cap), `TestProp_ManyTinyTradesCannotExtractValue` |
| 19 | malicious negative quantity | `TestCompile_MaliciousQuantitiesAreRefused` |
| 20 | malicious huge quantity | same — and it asserts the compiler does NOT cap: affordability is the ledger's question |
| 21 | replay old request | `TestIntegration_IdempotentReplayIsPersisted`, `TestRecordedRejectionIsReplayedNotReExecuted` |
| 22 | modify idempotency payload | `TestIntegration_IdempotencyKeyReuseWithADifferentBodyConflicts` |
| 23 | compromised provider sends contradictory status | **none** |
| 24 | agent tries to withdraw | `TestCompile_AnAgentCanNeverRequestAPayout` — every level, every capability active |
| 25 | agent attempts prohibited asset | `TestCompile_AnAgentIsRefusedAProhibitedAsset` — with a human and a different asset as controls |
| 26 | agent attempts limit escalation | `TestPermits_ForbiddenActionsAreRefusedAtEveryLevelWithEveryCapability` (`CHANGE_OWN_LIMITS`) |
| 27 | admin attempts unaudited balance mutation | `TestNoBalanceEditKindExists` — no admin action kind can edit a balance; repair is a compensating journal transaction |
| 28 | mock provider used in production config | `TestRegistry_ProductionRefusesAProviderWithNoContract`, `TestFake_RejectsProductionLike`, `TestLocalSigner_RefusedInProductionLikeEnvironments` |
| 29 | legal capability missing | `TestCompile_AFreshDeploymentPermitsOnlySimulation`, `TestIntegration_TheGateAndThePolicyMustBothAgree` |
| 30 | jurisdiction turns blocked mid-session | `TestIntegration_JurisdictionTurningBlockedMidSessionStopsTheNextPurchase` — same session, same cookie, next command refused |

The three gaps, stated as work rather than as risk acceptance:

- **5 — out-of-order webhook.** Duplicate delivery is covered; delivery in the wrong ORDER is not.
  The event store is append-only and consumers are idempotent, so the likely outcome is correct; that
  is a prediction, not a test.
- **14 — wash trading across two accounts under one actor.** The alert kind exists. Detecting shared
  control is the hard half and nothing implements it, so the test would currently assert nothing.
- **23 — contradictory provider status.** A provider that says SETTLED and then FAILED. `Reconcile`
  returns early on a terminal state, so the contradiction is not reachable through it today; proving
  the system refuses to act on one needs a provider double that can contradict itself.

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
