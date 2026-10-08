# ADVANCED SYSTEMS REVIEW

Independent adversarial reviews of the advanced capabilities (ADR-0020) and what became of every finding.
Each review was done by a read-only reviewer that had not written the code under review, against a named
commit, with a reproduction for every confirmed finding (Go integration tests compiled into the package
under review through `go test -overlay`, so nothing was written into the repository). A finding counts as
fixed only when its reproduction fails at the line that asserted the defect and a regression test in the
package proves the property it broke. The security red-team review of the integrated system is
`SECURITY_RED_TEAM.md`; what may be claimed from all of this is `PORTFOLIO_CLAIMS_REVIEW.md`.

Every value these subsystems move is value domain `SIMULATED`, which the database keeps from ever mixing
with real capital (VD002). Severity is stated as if the value were real, because the same controls are the
ones a live activation would rely on.

## Review 1 — kernel, transfers, sandbox faucet, margin

Scope: `internal/econ`, `internal/transfers`, `internal/sandboxfund`, `internal/margin`, at the head of
2026-10-07 after those subsystems merged. 13 findings; 161 integration tests of those packages passed at
review time.

| ID | Sev | Finding | Fix | Regression tests |
|---|---|---|---|---|
| F1 | High | A liquidation case could wedge in MANUAL_REVIEW forever: the step budget was checked before bankruptcy, the review had no exit, and interest kept accruing | `d91ead2` (00754): terminal accounting decided before the step budget; a review ends only with one operator's proposal and a different operator's approval; no interest while a person holds the case (MG007, MG009, MG010) | `TestIntegration_AManualReviewCaseResumesOnlyWithTwoOperators`, `TestIntegration_NoInterestAccruesWhileAPersonHoldsTheCase` |
| F2 | High | After the insurance fund covered a deficit, the account stayed active and its borrowed funds stayed spendable; the deficit could not be collected | buying power counts DEFICIT as a liability and blocks (`fd5d10f`); `d91ead2`: free wallet funds repay the loans before bankruptcy, a covered deficit restricts the account, and the deficit is repayable exactly once (MG011) | `TestIntegration_BorrowedCashInTheWalletIsSweptBeforeBankruptcy`, `TestIntegration_ACoveredDeficitRestrictsTheCustomerAccount`, `TestIntegration_ADeficitIsRepaidToTheFundExactlyOnce` |
| F3 | High | One operator could reverse a completed transfer alone | `a8f439f` (00758): a reversal is proposed by one operator and approved by a different one, bound in SQL (PT006-PT008) | `TestIntegration_AProposerCannotApproveTheirOwnReversal`, `TestIntegration_TwoOperatorsReverseExactlyOnceUnderConcurrency` |
| F4 | Med | Margin's routing accepted a route that needed confirmation | `d91ead2`: only a route that may execute now proceeds | `TestIntegration_ARouteThatNeedsConfirmationIsNeverExecuted` |
| F5 | Med | An agent requester could move a customer's wallet into margin collateral, with no intent | `d91ead2`: agents refused; posting collateral is a routed, risk-checked intent | `TestIntegration_AnAgentCannotPostCollateral` |
| F6 | Med | A liquidation step number replayed an old, completed case | `d91ead2`: a step replays only within the account's open case | `TestIntegration_AStepNumberReplaysOnlyWithinTheOpenCase` |
| F7 | Med | Nothing ran the advanced reconciliations on a schedule | `568264b`: `cmd/advanced-worker` runs every reconciler and time-driven job | `TestIntegration_InterestForADayIsChargedExactlyOnceAcrossThreeRuns`, `TestIntegration_AReservationPastItsTTLIsExpiredExactlyOnce` (`internal/advancedjobs`) |
| F8 | Low | The economic intent status machine was enforced only in Go; SQL let a finished intent reopen | `a330704` (00734, EI001) | `TestIntegration_TheDatabaseRefusesAnIllegalIntentEdge`, `TestIntegration_SQLAndGoAgreeOnIntentTransitions` |
| F9 | Low | The faucet ignored kill switches and any service identity could fund any account | `fd5d10f`: global kill refuses; a service needs `sandbox:fund` | `TestIntegration_OnlyTheOwnerAPermittedServiceOrTheSystemMayFund` |
| F10 | Low | Buying power V2 rounded margin loans in the customer's favour; the margin reader was not wired into the API | `fd5d10f` (liabilities round up); `8ea8194`/`9781267` (margin and derivatives combined, D-054) | buying power V2 unit tests in `internal/capital/buyingpower` |
| F11 | Low | Facility row locks were taken in different orders by liquidation and deficit coverage; the queue ran in one transaction | `d91ead2`: pool before fund everywhere; each account's step commits on its own | `TestIntegration_FacilityRowsAreLockedPoolFirstEverywhere`, `TestIntegration_TheQueueCommitsEachAccountInItsOwnTransaction` |
| F12 | Low | An APPLIED margin effect could name any unused journal | `d91ead2` (MG008): a commit-time binding to the journal its key posted | `TestIntegration_AMarginEffectAppliesOnlyTheJournalItsKeyPosted` |
| F13 | Low | The liquidation fee was booked against insurance-fund capital, with no revenue leg | `d91ead2`: booked as platform fee revenue | `TestIntegration_TheLiquidationFeeIsBookedAsPlatformFeeRevenue` |

## Review 2 — derivatives, prediction markets, strategy marketplace, sandbox runtime

Scope: `internal/derivatives`, `internal/predmarket`, `internal/marketplace`, `internal/sandboxrt`, at
`4bcbecc`. 14 findings. Verified as holding: the sandbox escape suite (scanner-bypass attempts through
`/*line*/`, `+build`, name constraints, cgo, embed, `go:debug`, go.mod/go.work/GOFLAGS/GOENV redirection —
none worked), the prediction markets' conservation, concurrency and oracle checks, the derivatives' position,
fill and cash-flow bindings, and by reading: clearing cash and wallets cannot go negative.

| ID | Sev | Finding | Fix | Regression tests |
|---|---|---|---|---|
| F1 | High | A faucet-minted or house-held outcome share froze its market forever (the escrow identity could never hold again) | `84b6403` (00756, PM008): a share exists only as its market issued it; the faucet refuses shares; no house or facility can hold one | `TestIntegration_AnOutcomeShareExistsOnlyAsItsMarketIssuedIt`, `TestIntegration_TheFaucetRefusesAPredictionMarketShare` |
| F2 | High | An expiry settled at any on-grid price the caller passed, with no actor, approval or bound | `cc5af7f` (00755, DV008-DV010): the price comes from a documented rule over recorded marks; an override needs two operators and stays within a bound, re-derived in SQL | `TestIntegration_ASettlementIsPricedByTheRuleNeverByTheCaller`, `TestIntegration_AStaleExpiryWaitsForAnOverrideTwoOperatorsAgreeOn` |
| F3 | High | A backdated mark chose an expiry or funding price with hindsight | `cc5af7f` (DV005): marks only in order and on time; runs read only marks recorded by their cutoff | `TestIntegration_AMarkIsRecordedOnlyInOrderAndOnTime`, `TestIntegration_FundingIgnoresMarksRecordedAfterTheInterval` |
| F4 | High | One operator, even the resolver, could void a RESOLVED market inside its dispute window | `84b6403` (PM007): a void is proposed and approved by two distinct operators, neither the creator nor the resolver; inside the window only through a dispute | `TestIntegration_AVoidNeedsTwoDistinctOperators` |
| F5 | Med | One frozen or unverified holder, or one resting order, blocked settlement or void for everyone | `84b6403` (PM009): the payout is held in escrow and claimed once eligible; working orders on the outcome books are cancelled at close, void and settlement | `TestIntegration_AFrozenHolderCannotBlockSettlementAndClaimsOnceEligible`, `TestIntegration_TheOrderBookCancelsRestingOrdersAtCloseVoidAndSettlement` |
| F6 | Med | A fill accepted any risk decision id, even a REJECT for another account | `cc5af7f` (DV006): an ALLOW of the same account, intent and instrument, used once | `TestIntegration_AFillNeedsAnAllowForItsOwnAccountIntentAndInstrument` |
| F7 | Med | A close was priced at the latest mark however old | `cc5af7f`: a close needs a fresh mark; otherwise refused and recorded | `TestIntegration_ACloseAtAStaleMarkIsRefusedAndRecorded` |
| F8 | Med | Losses were collected from the raw wallet, ignoring reservations and holds | `cc5af7f` (DV011): collections go through a reservation of available funds | `TestIntegration_CollectionsNeverSpendFundsReservedForSomethingElse` |
| F9 | Med | An instrument's terms could change under open positions, moving funds at an unchanged price | `cc5af7f` (DV007): terms frozen while positions are open | `TestIntegration_TermsAreFrozenWhileTheContractHasOpenPositions` |
| F10 | Med | Paper performance came from a caller-written snapshot; the actor was not bound to the account | `55f7581` (00757, MK012-MK013): the follow builds its own input from the paper book and fresh prices; the database recomputes performance from fills and marks | `TestIntegration_PaperPerformanceComesFromTheBooks`, `TestIntegration_PaperPerformanceNeverComesFromACallersFigures` |
| F11 | Med | The manifest's notional cap and Risk V2 used the strategy's own limit price | `55f7581` (MK014): notionals at the reference price; no fresh price, refused and recorded | `TestManifest_ANotionalIsComputedAtTheReferencePriceNeverTheStrategysLimit` |
| F12 | Med | Builds ran inside a database transaction with unbounded output and memory; an oversized build wedged a version in SUBMITTED | `55f7581` (MK015): builds bounded and run between transactions under one claim; an overrun is a recorded rejection | `TestValidate_ABuildNeverRunsInsideATransaction`, `TestIntegration_AnOversizedBuildIsARecordedRejectionNeverAWedgedSubmission` |
| F13 | Low | No overall sandbox budget: one user could run a thousand simulations in one transaction | `55f7581` (SX002, SX003): every run is charged to an account within per-call and per-hour limits | `TestIntegration_ConcurrentRunsCannotOverspendTheBudget` |
| F14 | Low | A resolver could uphold a dispute of their own resolution; an operator who created a version could re-clear it | `84b6403` (PM006), `55f7581` (MK011) | `TestIntegration_TheResolverCannotRuleOnTheDisputeOfTheirOwnResolution`, `TestIntegration_AnOperatorCannotReclearTheirOwnVersion` |

## Findings of integration

Defects found while integrating the subsystems with each other rather than by a reviewer, each fixed with
a regression test:

- **Supply reconciliation counted only customer wallets** (`f46d780`): once a holder bridged the stable asset
  or posted it as collateral, the reconciliation reported a material mismatch and tripped the breaker. Found by
  demo Scenario G. The ledger now classifies every code as holding value or not (`ledger.HoldingCodes`).
  Tests: `TestIntegration_SupplyReconcilesWhereverTheUnitsAreHeld`, `TestHoldingCodes_EveryCodeIsClassifiedOnPurpose`.
- **A house order could reach the book around the treasury's limits** (`d6c713f`, 00759, TY007): every
  house order must be the result of an applied treasury action. Test: `TestIntegration_HouseOrdersExistOnlyThroughTheTreasury`.
- **Dynamically built SQL** in the stable asset and delegated authority (`7ef8595`, `060fcf2`): rewritten as
  constant statements so `test/security`'s provenance check proves them.

## Status

All 27 findings of reviews 1 and 2 are fixed; none is open. Robustness defects the advanced worker found in
batch upkeep (a poison item blocking later runs, network filtering, reconciliation record growth) are being
fixed (00760). The red-team review of the integrated system is the next review.
