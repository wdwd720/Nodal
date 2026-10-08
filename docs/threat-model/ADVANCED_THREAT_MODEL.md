# THREAT MODEL — ADVANCED FINANCIAL CAPABILITIES

Scope: the capabilities of ADR-0020 (stable assets, margin, derivatives, equities, internal matching,
transfers, principal trading, fees, the strategy marketplace and code sandbox, delegated agent signing and
withdrawals, cross-chain capital, prediction markets). Base threat model: `THREAT_MODEL.md`.

**The boundary every row below inherits.** In this build every advanced capability moves only value in
domain `SIMULATED`, which the database refuses to move to or from any other domain (VD002), and which every
advanced journal kind is confined to (SM001). The worst outcome of every attack below is therefore a wrong
*simulated* balance, never a loss of real customer value. That is a property of the schema, not of the
code under attack: it holds even if the attacked code is fully compromised, as long as the database role
and migrations are intact. Each row nevertheless states the control *as if* the value were real, because
the activation path (ADR-0020 "Consequences") turns these same controls into the real ones.

Format per threat: **asset** · **attacker** · **entry point** · **privilege needed** · **attack** ·
**controls** · **residual risk** · **tests that prove the controls**. Test names are real functions in the
packages named; `go run ./scripts/inttest -pkg '^\./internal/<pkg>$'` runs a package's integration suite.

## Index

| # | Threat | Subsystem | Status of this entry |
|---|---|---|---|
| 1 | Stable mint compromise | `internal/stableasset`, `internal/sandboxchain` | below |
| 2 | Reserve falsification | `internal/stableasset`, ledger | below |
| 3 | Margin bypass | `internal/margin` | below |
| 4 | Liquidation manipulation | `internal/margin` | below |
| 5 | Mark-price manipulation | valuation, `internal/margin`, `internal/derivatives` | below |
| 6 | Matching-engine race | `internal/matching`, `internal/orders` | below (engine and persistent book) |
| 7 | Transfer replay | `internal/transfers` | below |
| 8 | House/customer asset commingling | ledger, `internal/econ`, `internal/treasury` | below |
| 9 | Fee manipulation | `internal/fees`, `internal/feeledger` | below |
| 10 | Marketplace malicious strategy | `internal/marketplace` | below |
| 11 | Sandbox escape | `internal/sandboxrt` | below |
| 12 | Agent capability escalation | kernel, settlement compiler, `internal/delegation` | below |
| 13 | Signing transaction substitution | `internal/delegation` | below |
| 14 | Withdrawal address substitution | `internal/delegation` | below |
| 15 | Bridge double counting | `internal/bridge`, buying power V2 | below |
| 16 | Oracle manipulation | `internal/predmarket`, `internal/derivatives` | below |
| 17 | Prediction resolution corruption | `internal/predmarket` | below |

---

## 1. Stable mint compromise

- **Asset:** the platform stable asset's supply: every unit minted is a claim on the reserve.
- **Attacker:** a customer minting without paying, an insider or compromised process asking the issuer to sign
  a mint nobody booked, or a binary configured with the wrong keys.
- **Entry point:** the mint workflow (`stableasset.Service.Mint` / `PrepareMint`) and the issuer signer.
- **Privilege needed:** `sandbox:stable` for a customer mint; process access for the signer.
- **Attack:** mint more than the reserve backs or the cap allows; mint without the reserve payment; get the
  issuer to sign a transaction other than the planned one; submit twice; mint with keys the chain does not
  know.
- **Controls:** the mint authority key lives only in `IssuerSigner`, which signs exactly the hashed mint plan
  after `InspectMintTransaction` verifies the transaction is that plan (instruction, mint, recipient, amount,
  memo = plan hash, signer set, fee payer); admission is fully reserved under the asset's row lock and again in
  the posting transaction; the database refuses at COMMIT supply above the cap (SV003), an issuance without
  its full eligible cash-equivalent backing (SV004) and any illegal request transition (SV001); the reserve
  payment is a capital reservation consumed only by the posting; the posting happens only after finality;
  the submission is recorded before it happens, so a timeout is UNKNOWN and reconciled, never resubmitted;
  every key derives from one configured seed through `internal/sandboxchain`, and a binary with another seed
  fails closed when it opens the network (the host's genesis pins the attester). Out-of-band mints are caught
  by supply reconciliation, which trips the breaker.
- **Residual risk:** the issuer signer is in-process: a compromise of the process that holds the seed can sign
  planned-looking mints, which reconciliation detects after the fact rather than prevents (an HSM or a remote
  signer is the production answer, ADR-0020). Values are SIMULATED.
- **Tests:** `TestIssuerSigner_SignsExactlyThePlannedMint`, `TestInspection_RefusesEveryDeviationFromThePlan`,
  `TestSigners_NeverRevealTheirKeys`, `TestIntegration_SupplyCannotExceedThePolicyCap`,
  `TestIntegration_TheDatabaseRefusesUnderReservedIssuanceAndOverpayingRedemption`,
  `TestIntegration_ConcurrentMintsPreserveTheInvariants`, `TestIntegration_ADuplicateMintRequestIsIdempotent`,
  `TestIntegration_AnUnknownSubmissionIsReconciledNotRetried`, `TestIntegration_ReconciliationDetectsAnOutOfBandMint`
  (`internal/stableasset`); `TestIntegration_AWrongSeedFailsClosedOnAnExistingNetwork`,
  `TestDerive_TheDerivationIsPinned` (`internal/sandboxchain`); demo Scenarios A and I.

## 2. Reserve falsification

- **Asset:** the published claim that the stable asset is fully reserved.
- **Attacker:** an operator or process that wants the reserve to look larger, or the supply smaller, than it is.
- **Entry point:** the reserve ledger, the supply reconciliation and the signed reserve snapshots.
- **Privilege needed:** operator, or write access to the snapshot table.
- **Attack:** count a customer's pending payment as reserve as well as a receivable; value a cash equivalent
  above par; publish a snapshot that misstates supply or reserve; rewrite an old snapshot; hide units held
  outside customer wallets.
- **Controls:** the reserve ledger states every figure's origin and counts a pending mint's payment once (as a
  receivable, then as reserve after the posting); a cash equivalent is valued at the lower of its fresh price
  and par, times its eligibility factor, and an unpriced reserve counts zero; coverage rounds down, deviation
  rounds up; snapshots are canonical JSON, Ed25519-signed by the snapshot signer, hash-linked to their
  predecessor and append-only — the database refuses one that does not extend the chain (SV007) and any
  update; supply reconciliation compares outstanding supply with every holding of the asset on the ledger
  (`ledger.HoldingCodes`: wallets, collateral, bridge escrow, house inventory, collected fees) and with the
  chain's supply, and a mismatch opens a material STABLE_RESERVE record that blocks new risk and trips the
  breaker.
- **Residual risk:** the snapshot signer key has the same in-process exposure as the issuer key (threat 1);
  external attestation by an independent party does not exist in a sandbox.
- **Tests:** `TestIntegration_ReserveCannotBeDoubleCounted`, `TestCapAtPar_ACashEquivalentIsNeverWorthMoreThanADollar`,
  `TestRatios_CoverageRoundsDownAndDeviationRoundsUp`, `TestIntegration_SnapshotsAreSignedHashLinkedAndVerifiable`,
  `TestVerifySnapshotChain_DetectsEveryKindOfTampering`, `TestIntegration_AReserveShortfallFreezesIssuance`,
  `TestIntegration_SupplyReconcilesWhereverTheUnitsAreHeld`, `TestProp_SupplyNeverExceedsEligibleReserve`
  (`internal/stableasset`); `TestHoldingCodes_EveryCodeIsClassifiedOnPurpose` (`internal/ledger`); demo
  Scenarios A and G.

## 3. Margin bypass

- **Asset:** the sandbox lending pool's cash and the customer's collateral.
- **Attacker:** a customer, or a compromised client acting for one.
- **Entry point:** the margin operations (borrow, leveraged buy, release collateral), including many concurrent
  requests and stale marks.
- **Privilege needed:** an authenticated customer with `sandbox:trade` on their own account.
- **Attack:** borrow or buy beyond margin by racing requests that each pass a check the others invalidate;
  release collateral the open loan still needs; trade on a stale mark that overvalues collateral.
- **Controls:** Risk Kernel V2's margin section (equity after the action against the initial requirement,
  computed by the margin engine with haircuts); a capital reservation under the `(account, asset)` totals lock,
  which every spender of the wallet takes, so concurrent requests serialize at the books; a stale, missing or
  future mark makes the holding worth zero as collateral and refuses risk-increasing actions; a release is
  re-evaluated against maintenance; exactly-once effects per operation key; database constraints on margin
  state (MG001–MG006).
- **Residual risk:** one margin account per customer account; spot and derivative books are margined
  separately and summed (D-054), so no cross-book offset is ever credited — conservative, not exploitable.
- **Tests:** `TestIntegration_ConcurrentBorrowsCannotSpendTheSameMarginTwice`,
  `TestIntegration_ConcurrentLeveragedBuysCannotSpendTheSameMarginTwice`,
  `TestIntegration_ReleaseThatWouldBreachMaintenanceIsRefused`, `TestIntegration_StaleMarksBlockNewLeverage`,
  `TestProp_NoWithdrawalBreachesMaintenance`, `TestProp_MarginAvailableNeverExceedsCapacity`,
  `TestIntegration_BorrowAndLeveragedBuyAreExactlyOnce`, `TestIntegration_DatabaseRefusesInvalidMarginStates`
  (all `internal/margin`); `TestIntegration_V2CountsWhatIsOwedAgainstTheOwner` (`internal/capital/buyingpower`).

## 4. Liquidation manipulation

- **Asset:** the liquidated customer's collateral, the insurance fund, the lending pool.
- **Attacker:** a customer avoiding liquidation; anyone able to move the mark that triggers it; an operator
  liquidating an account that is not eligible.
- **Entry point:** mark recording, the liquidation operation and queue, replayed liquidation requests.
- **Privilege needed:** `sandbox:operate` to run liquidation; mark recording for the price route (see 5).
- **Attack:** liquidate on a stale or manipulated price; sell more than needed; sell at an unbounded slippage;
  replay a step to sell twice; liquidate a healthy account.
- **Controls:** liquidation refuses a stale price and waits rather than guessing; the plan is deterministic and
  reduce-only and the smallest sale that restores the account is chosen first; every step's slippage is capped
  and the cap and step budget are database constraints; a step is executed once (expected-step replay); a kill
  switch stops new risk but never liquidation; deficit cover by the insurance fund is exactly once.
- **Residual risk:** review findings F1 (a step budget reached before bankruptcy could leave a case in manual
  review with no exit) and F6 (a step-1 request after a finished case replayed the old case) are being fixed in
  the margin hardening work (migration 00754); until then an operator can be needed to unwedge a case.
- **Tests:** `TestProp_LiquidationIsDeterministicReduceOnlyAndTerminates`,
  `TestIntegration_StalePriceLiquidationIsRefused`, `TestIntegration_PartialLiquidationRestoresTheAccount`,
  `TestPlan_SlippageIsCapped`, `TestApplySale_IsReduceOnly`, `TestIntegration_KillSwitchStopsNewRiskButNeverLiquidation`,
  `TestIntegration_LiquidationQueueTakesTheMostUnderMarginedFirst`, `TestIntegration_ScenarioB_FromFundingToACoveredDeficit`
  (`internal/margin`); demo scenario B (`make advanced-demo`).

## 5. Mark-price manipulation

- **Asset:** every valuation built on marks — buying power, margin and liquidation, derivative variation
  margin and funding, option analytics.
- **Attacker:** whoever can record a mark (an operator, an oracle feed) or replay an old one.
- **Entry point:** `valuation.PriceStore.RecordPrice`, `derivatives.Service.RecordMark`.
- **Privilege needed:** operator (`sandbox:operate`) or the service identity of a price feed.
- **Attack:** record a mark that inflates collateral or deflates a competitor's position; reuse an old mark;
  put a derivative mark off its tick grid or far from its index.
- **Controls:** a mark is an append-only observation identified by asset, quote, source and observation time;
  every consumer bounds its age and fails closed on a stale, missing or future mark (zero collateral value,
  refused new risk, liquidation waits); derivative marks must lie on the instrument's tick grid and a
  mark/index divergence beyond policy refuses new risk; a derivative mark cannot be backdated — it is refused
  in the future, older than the policy's maximum lag, or older than the latest mark, and the database keeps
  marks in order (DV005) — and variation margin, funding and expiry read only marks recorded by their cutoff;
  a close is priced only at a fresh mark; an expiry settles at the price its documented rule derives from
  the marks (EXPIRY_MARK), never at a caller's price, and an override needs two operators and stays within a
  bound of the rule's reference price, all re-derived in SQL (DV008-DV010).
- **Residual risk:** one source per mark in the sandbox — no median of independent feeds — and one operator can
  record a mark. A live activation needs multi-source aggregation and dual control on manual marks.
- **Tests:** `TestIntegration_StaleOraclePreventsARiskIncreasingOrderButNotAPricedClose`,
  `TestIntegration_MarkIndexDivergenceRefusesNewRisk`, `TestSpec_PricesOffTheTickGridAreRefused`,
  `TestIntegration_AMarkIsRecordedOnlyInOrderAndOnTime`, `TestIntegration_ASettlementReadsOnlyMarksRecordedByExpiry`,
  `TestIntegration_ASettlementIsPricedByTheRuleNeverByTheCaller`, `TestIntegration_AStaleExpiryWaitsForAnOverrideTwoOperatorsAgreeOn`,
  `TestIntegration_ACloseAtAStaleMarkIsRefusedAndRecorded` (`internal/derivatives`);
  `TestHaircut_StaleMissingFutureOrStuckIsWorthless`, `TestEvaluate_StaleMarkIsZeroCollateralAndCallsMargin`
  (`internal/margin`); `TestIntegration_AMarkValuedAssetFailsClosedWithoutAFreshMark` (`internal/transfers`).

## 6. Matching-engine race

- **Asset:** resting orders and the fills they produce.
- **Attacker:** a participant submitting, cancelling or replacing orders concurrently or replaying them.
- **Entry point:** the order book's commands.
- **Privilege needed:** a customer with `sandbox:trade`.
- **Attack:** fill more than was submitted; trade against oneself; reorder the queue by racing; resubmit a
  filled order.
- **Controls (pure engine, `internal/matching`):** a single-writer sequencer assigns a total order to every
  command; price-time priority through a FIFO per price level; every filled quantity is bounded by what was
  submitted; self-trade prevention; an order id is consumed by its first submission whatever the outcome, so a
  retry cannot create a second order; the journal replays to byte-identical events and the same state hash.
- **Controls (persistent book, `internal/orders`):** every command is journaled with its sequence and the
  state hash after it, and the database refuses a journal row that does not extend the book's cursor (OB001,
  OB002) or a snapshot whose hash is not the one the journal recorded (OB005); the order status machine is
  enforced in SQL with every change bound to a transition row (OB003, OB004), and the database refuses an
  overfill, a self-trade, an unruled house/customer crossing and a duplicate client order id; each order's
  funds are a capital reservation consumed only by its fills, so concurrent buys cannot overspend a wallet;
  a book's row lock serializes its commands across processes, and the in-memory engine is a cache checked
  against the journal on every command; a crash after execution is completed by the sweep; every fill posts
  one SIM_TRADE with an exactly-once effect and archived evidence.
- **Residual risk:** one book is one serialization point (throughput is bounded per instrument, measured in
  `make perf-advanced`); kill switches stop new orders but resting orders stay until cancelled.
- **Tests:** matching property and replay tests in `internal/matching`;
  `TestIntegration_TheDatabaseRefusesABrokenJournal`, `TestIntegration_TheDatabaseRefusesOverfillAndIllegalOrderStates`,
  `TestIntegration_TheDatabaseRefusesSelfTradesUnruledCrossingAndDuplicateClientIDs`,
  `TestIntegration_ConcurrentBuysCannotOverspendTheWallet`, `TestIntegration_CancelRaceHasExactlyOneOutcome`,
  `TestIntegration_ConcurrentSubmitsReplayToTheLiveStateHash`, `TestIntegration_ACrashAfterExecuteIsCompletedBySweep`,
  `TestProp_QuantitiesAreConservedAndNoOrderOverfillsThroughTheService`,
  `TestProp_NoCrossedRestingBookAfterEveryCommandThroughTheService` (`internal/orders`); `make perf-advanced`
  measures replay determinism; demo Scenarios D and H.

## 7. Transfer replay

- **Asset:** the sender's simulated funds.
- **Attacker:** a network replayer, a retrying client, or a sender submitting the same transfer twice.
- **Entry point:** the transfer send operation.
- **Privilege needed:** none to replay captured traffic (the session cookie is still required); the sender for a
  double submission.
- **Attack:** move the same value twice.
- **Controls:** the HTTP command layer is idempotent per actor, operation and `Idempotency-Key`; the transfer is
  idempotent on (sender account, key) with the request content-hashed, so a different request under the same
  key is `INVALID_IDEMPOTENCY_REUSE`; the economic effect's key is unique and a replay returns the first
  outcome; the ledger posting has its own idempotency key; the reservation is keyed by the transfer.
- **Residual risk:** review finding F3 — one operator can reverse a completed transfer without a second
  approver or a recent step-up — is open and tracked; reversal is a compensation, never an edit of history.
- **Tests:** `TestIntegration_ADuplicateTransferIsIdempotent`, `TestIntegration_ConcurrentTransfersCannotDoubleSpend`,
  `TestIntegration_TransfersAndOtherSpendersShareOneBudget`, `TestIntegration_AReversalCompensatesAndNeverEditsHistory`
  (`internal/transfers`); `TestIntegration_EffectIsClaimedExactlyOnceUnderConcurrency`,
  `TestIntegration_SameKeyDifferentRequestIsRefused` (`internal/econ`).

## 8. House/customer asset commingling

- **Asset:** the separation between what customers own and what house books and facilities hold.
- **Attacker:** buggy or malicious posting code; an application-role writer bypassing Go.
- **Entry point:** any journal posting.
- **Privilege needed:** the application database role.
- **Attack:** move customer value into a house or facility book (or the reverse) without a balanced
  counter-movement; hold real value in a house book; post a facility code to a facility of the wrong kind.
- **Controls:** every advanced journal kind is simulated-only and may touch only SIMULATED accounts (SM001) and
  must balance per party (SM002), so no party gains what another did not give; house and facility books may
  hold only SIMULATED value (segregated-books constraint); a facility code must match its facility's kind
  (FA001); value domains never mix (VD002).
- **Controls (principal/treasury book, `internal/treasury`):** the house trades only from its own HOUSE
  segment — treasury postings stay in it (TY004) and no customer fund is reachable by a house strategy; only
  the house service or an operator acts for the house (TY002); every house order is evaluated against the book's
  loss, inventory and exposure limits, including concurrently; a house fill against a customer needs a disclosed
  sandbox crossing rule (TY006); the kill switch stops new house orders. Every house order on the book is the
  result of an applied treasury action, so none reaches the book around the treasury's limits (TY007, 00759). A
  delegated-swap venue is backed by a house book of its own: never a treasury book or another venue's, and a
  venue's book can never become a treasury book (DG008, 00762), so delegated swaps cannot move the treasury's
  inventory outside its actions and limits.
- **Residual risk:** none known beyond the sandbox's single-operator actions listed in `SECURITY_RED_TEAM.md`.
- **Tests:** `TestIntegration_HouseAndCustomerLedgersAreSegregated`, `TestIntegration_NoCustomerFundsAreAvailableToTheHouseStrategy`,
  `TestIntegration_OnlyTheHouseServiceOrAnOperatorActsForTheHouse`, `TestIntegration_ConcurrentHouseOrdersCannotBreachALimitTogether`,
  `TestIntegration_HouseCustomerCrossingNeedsADisclosedSandboxRule`, `TestIntegration_TheDatabaseRefusesInvalidTreasuryStates`
  (`internal/treasury`); `TestIntegration_HouseOrdersExistOnlyThroughTheTreasury` (`internal/httpapi`);
  `TestIntegration_AVenueNeedsAHouseBookOfItsOwn` (`internal/delegation`); `TestIntegration_HouseAndFacilityBooksCannotHoldRealValue`, `TestIntegration_FacilityBooksAreSegregatedByKind`,
  `TestIntegration_ASimulatedOnlyKindCannotTouchRealCapital`, `TestIntegration_ATransferMustBalancePerParty`
  (`internal/econ`); `TestProp_EveryTemplateBalancesPerPartyAndIsSimulatedOnly` (`internal/margin`);
  `TestIntegration_TheDatabaseRefusesAClearingFlowThatDoesNotBalancePerParty` (`internal/derivatives`);
  `TestIntegration_TheDatabaseRefusesAPredictionPostingThatDoesNotBalancePerParty` (`internal/predmarket`).

## 9. Fee manipulation

- **Asset:** the customer's simulated value, through what is charged as a fee, a spread or a performance fee.
- **Attacker:** an operator or process that wants to charge more than disclosed, hide a spread, or charge a
  performance fee on gains that are not gains.
- **Entry point:** fee schedules, quotes, bookings and performance-fee periods (`internal/feeledger`), and the
  pure fee engines (`internal/fees`).
- **Privilege needed:** operator to publish a schedule; process access to book.
- **Attack:** change a schedule after a quote; charge a component not in the schedule; round against the
  customer silently; book a fee twice; charge a performance fee on a loss, below the high-water mark, or on a
  deposit; erase an accrual through a withdrawal.
- **Controls:** schedules are versioned, immutable and hashed (FE001, FE002), and a quote names the version
  and hash it was priced from; every quote is a stored disclosure that can be re-verified (FE003); every charge
  is booked component by component against its journal and effect exactly once (FE004), and a refund is a
  compensating reversal (FE005); a fee is never negative and never above the schedule's maximum; rounding is in
  the customer's favour unless the schedule allows otherwise and says so; every spread charged is discoverable
  from the audit record; performance fees crystallize only above the high-water mark, deposits generate no
  performance, a withdrawal does not erase the accrual, and the period, flow and line chains are bound in SQL
  (FE006-FE008). Every fee an internal-book trade, a paper-venue fill or a margin liquidation step charges is
  booked through the fee ledger in the transaction that charges it, against that row's own journal, under the
  schedule the row names and with a disclosure identical to the row's own; the database refuses the row
  without its booking (FW001, 00761), so no subsystem can charge a fee the fee ledger and the spread audit never see.
- **Residual risk:** performance-fee NAVs are supplied by the caller. A book whose fee schedule key is given a
  different version by an operator stops charging until fixed (fail closed, not silently repriced).
- **Tests:** `TestProp_FeeConservation`, `TestProp_NoNegativeFeeAndNoFeeAboveTheConfiguredMax`,
  `TestRounding_IsInTheCustomersFavourUnlessAllowedAndDisclosed`, `TestIntegration_SchedulesAreVersionedImmutableAndHashed`,
  `TestIntegration_QuoteDisclosuresAreStoredAndVerifiable`, `TestIntegration_ChargesAreBookedComponentByComponentExactlyOnce`,
  `TestIntegration_EveryChargedSpreadIsDiscoverableFromTheAuditRecord`, `TestIntegration_TheDatabaseRefusesInvalidFeeStates`,
  `TestIntegration_ALosingPeriodChargesNoFee`, `TestIntegration_AGainBelowTheHighWaterMarkChargesNoFee`,
  `TestIntegration_DepositsDoNotGeneratePerformance`, `TestIntegration_AWithdrawalDoesNotEraseTheAccrual`,
  `TestIntegration_ConcurrentAccrualsAndCrystallizationsAreExactlyOnce` (`internal/feeledger`);
  `TestIntegration_EveryInternalBookFeeIsBookedThroughTheFeeLedger`, `TestIntegration_AReplayedFillBooksItsFeesOnce`,
  `TestIntegration_TheDatabaseRefusesAnUnbookedFee`, `TestIntegration_EveryPaperFillIsBookedThroughTheFeeLedger`
  (`internal/orders`); `TestIntegration_TheLiquidationFeeIsBookedThroughTheFeeLedger`,
  `TestIntegration_TheDatabaseRefusesAnUnbookedLiquidationFee` (`internal/margin`); demo Scenario D (the fees of a
  trade are exactly the disclosed schedule).

## 10. Marketplace malicious strategy

- **Asset:** followers' paper capital and the platform's reputation.
- **Attacker:** a strategy author.
- **Entry point:** listing and version submission, paper-follow.
- **Privilege needed:** a customer with `sandbox:strategy`.
- **Attack:** publish a strategy that proposes more than its manifest allows, moves value, follows into live
  capital, approves itself, or changes after approval.
- **Controls:** a version's manifest is immutable and every proposal outside it is dropped and counted; a
  proposal can only become an economic intent, which then goes through route, capability gate and Risk Kernel
  V2 like any other — never a transfer or a withdrawal; installs are PAPER only (enforced before the database
  and by it); the creator can never approve their own listing; any violation in a run rejects every intent from
  that run; a dispatch needs an ALLOW decision on record.
- **Residual risk:** paper results can still mislead a follower; they are labelled simulated everywhere.
- **Tests:** `TestIntegration_ProposalsBeyondManifestDroppedAndCounted`, `TestIntegration_InstallIsPaperOnly`,
  `TestIntegration_CreatorCannotApproveOwnListing`, `TestIntegration_ManifestIsImmutable`,
  `TestIntegration_Prop_UntrustedOutputOnlyBecomesRiskCheckedIntents`,
  `TestIntegration_ADispatchWithoutAnAllowDecisionIsRefusedByTheDatabase`, `TestIntegration_AnAgentCannotTouchTheMarketplace`
  (`internal/marketplace`); `TestProp_NoStrategyOutputBecomesATransferOrWithdrawal` (`internal/sandboxrt`).

## 11. Sandbox escape

- **Asset:** the host: its filesystem, network, process table, environment and secrets.
- **Attacker:** a strategy author.
- **Entry point:** strategy source → scan → build → run.
- **Privilege needed:** a customer with `sandbox:strategy`.
- **Attack:** read files or secrets, open a socket, fork, exhaust CPU or memory, flood output, construct a
  signing call, behave non-deterministically.
- **Controls:** a static scan refuses forbidden imports and every compiler directive before the toolchain runs;
  the build environment carries nothing from the parent; strategies run as WASI modules in wazero with no
  preopened directories, no network and no host functions beyond WASI; wall-time, memory (a metered allocator)
  and output-size limits; modules are digest-checked; clock and randomness are virtual and deterministic.
- **Residual risk:** a vulnerability in the WASM runtime or in the Go compiler (the build compiles untrusted
  source on the host); a live activation would build in an isolated builder.
- **Tests:** `TestSandbox_FilesystemEscapeAttemptFails`, `TestSandbox_NetworkAttemptFails`, `TestSandbox_ForkBombBlocked`,
  `TestSandbox_CPUInfiniteLoopKilled`, `TestSandbox_MemoryBombKilled`, `TestSandbox_OversizedOutputKilled`,
  `TestSandbox_EnvironmentSecretsAbsent`, `TestBuilder_EnvironmentCarriesNothingFromTheParent`,
  `TestScan_ForbiddenImportBlockedByScanner`, `TestScan_EveryDirectiveIsRefused`, `FuzzScan`,
  `TestSandbox_StrategyCannotConstructALiveSigningCall`, `TestSandbox_SameArtifactSameInputIsByteIdentical`,
  `TestSandbox_DigestMismatchIsRefused` (`internal/sandboxrt`).

## 12. Agent capability escalation

- **Asset:** customer funds and authority.
- **Attacker:** an agent, a strategy's output, or a compromised model.
- **Entry point:** agent intents, strategy proposals, the HTTP surface.
- **Privilege needed:** an agent bound to one account.
- **Attack:** move value between customers, withdraw, post collateral, call an operator action.
- **Controls:** agent principals never reach the HTTP surface; the settlement compiler forbids
  `SIMULATED_TRANSFER_VALUE` and `SIMULATED_WITHDRAW` to agents at every authority level; agents only create
  intents and never apply effects; the faucet refuses agents; strategy output becomes only risk-checked intents.
- **Controls (delegated authority, `internal/delegation`):** an agent acts only under a capability its customer
  granted — scoped to instrument and action classes, chains, limits and a lifetime, hashed over every term — and
  a capability never expands itself and never crosses an account boundary (DG003); the actors of a grant are
  never agents; an expired or revoked capability refuses every use. A swap is checked under the action its
  direction of value makes it — never the label the agent sent — so a purchase labelled as a reduction is
  refused (`DELEGATION_ACTION_MISMATCH`) and can pass neither a reduce-only capability, nor a restricted
  account's reduce-only rule, nor the platform's single-trade cap; the database checks the stored label against
  the swap's assets at admission and again at signing (DG007). Every kill-switch check of an agent names its
  current strategy version, so `STRATEGY_VERSION_DISABLE` stops its swaps and withdrawals at every step.
- **Controls (the agent runtime):** agents never reach the HTTP surface. The runtime acts in-process through
  `delegation.AgentAdmission`, which can request a swap, propose a withdrawal and read the agent's own
  capabilities, and nothing else — it holds no service, signer or seed — and a delegated tool is offered only
  under an active, unexpired, intact capability in a PAPER run.
- **Residual risk:** review-1 finding F5 (an agent requester could post a customer's collateral) and red-team
  findings F1 (the swap's label) and F2 (the strategy-version kill switch) are fixed (`SECURITY_RED_TEAM.md`). A
  kill-switch check uses the agent's current strategy version, not the version that made the request.
- **Tests:** `TestIntegration_StrategyCannotSelfExpandCapability`, `TestIntegration_CapabilityCannotCrossAccountBoundary`,
  `TestTheActorsOfAGrantAreNeverAgents`, `TestIntegration_ExpiredTokenRejected`, `TestIntegration_RevokedTokenRejected`,
  `TestProp_CapabilityLimitsNeverOverspend`, `TestIntegration_AReduceOnlyCapabilityNeverBuysExposure`,
  `TestIntegration_ARestrictedAccountsAgentOnlyReducesExposure`, `TestIntegration_ThePlatformSingleTradeCapBindsEveryPurchase`,
  `TestIntegration_TheDatabaseBindsASwapsActionToItsDirection`, `TestIntegration_AStrategyVersionKillSwitchStopsItsAgentAtEveryStep`,
  `TestAgentAdmissionExposesOnlyAdmissionAndTheCapabilityRead` (`internal/delegation`);
  `TestTheRuntimeHoldsNoDelegationServiceOrSigner`, `TestDelegatedToolsAreOfferedOnlyUnderAUsableCapabilityInAPaperRun`
  (`internal/agent`); `TestIntegration_AnAgentCannotPostCollateral`
  (`internal/margin`); `TestRoute_TheCompilerForbidsAgentTransfersAtEveryLevel`, `TestIntegration_AnAgentNeverTransfersAndTheCompilerAgrees`
  (`internal/transfers`); `TestNoOperationAdmitsAnAgent` (`internal/httpapi`); `TestIntegration_AnAgentCannotTouchTheMarketplace`
  (`internal/marketplace`); `TestProp_NoStrategyOutputBecomesATransferOrWithdrawal` (`internal/sandboxrt`).

## 13. Signing transaction substitution

- **Asset:** the delegated wallet's signature: whatever it signs moves the customer's simulated value.
- **Attacker:** an agent, a compromised relay or venue, or code between approval and signing that swaps the
  transaction for another.
- **Entry point:** the delegated signing workflow (`internal/delegation`: request, decide, sign, gate, submit).
- **Privilege needed:** an agent holding a capability; process access between steps.
- **Attack:** get a different transaction signed than the plan the capability and the human approved; change
  the amount, the venue or the recipient after approval; replay a signature; sign beyond the capability.
- **Controls:** agents never hold or read keys — keys are derived inside the signer from a seed held in a
  closure, and only public keys are stored; a swap compiles to one delivery-versus-payment transaction whose
  plan hash covers every field; the inspector accepts exactly the planned transaction and refuses every
  mutation, checking signatures over the exact bytes; the human approval binds the plan hash; the venue
  co-signs only the planned transaction; one signature per swap, and the database refuses a signing that is
  not licensed by an approved decision under a live capability (DG005); usage against the capability's limits
  is consumed under concurrency control (DG004); an unknown submission is reconciled, never resubmitted.
- **Residual risk:** a kill switch activated between the final pre-submit check and the chain accepting the
  transaction is not caught; the signer is in-process (see threat 1).
- **Tests:** `TestTheInspectorRefusesEveryModifiedSwap`, `TestProp_TheInspectorRefusesEveryMutation`,
  `TestTheInspectorChecksSignaturesOverTheExactBytes`, `TestTheVenueCoSignsOnlyThePlannedTransaction`,
  `TestIntegration_ModifiedTransactionRejected`, `TestIntegration_HumanApprovalBindsThePlan`,
  `TestIntegration_ExactlyOneSignaturePerSwap`, `TestIntegration_ReplayRejected`,
  `TestIntegration_ConcurrentSigningsCannotExceedDailyNotional`, `TestIntegration_TheDatabaseRefusesUnlicensedSwapStates`,
  `TestSignersNeverFormatTheirSecrets` (`internal/delegation`).

## 14. Withdrawal address substitution

- **Asset:** the customer's simulated value leaving through a delegated withdrawal.
- **Attacker:** an agent proposing a withdrawal to its own address, or code that changes the destination or
  amount after approval.
- **Entry point:** the delegated withdrawal workflow (`internal/delegation`: destinations, policy, propose,
  decide, sign, execute).
- **Privilege needed:** an agent holding a capability that includes withdrawals.
- **Attack:** withdraw to an address that is not allow-listed; substitute the address or the amount after the
  human approved; exceed the daily limit by concurrent proposals; withdraw while the account is frozen; have an
  agent withdraw without a person.
- **Controls:** a withdrawal can go only to a destination on the customer's allow list, under the customer's
  explicit policy; the plan hash covers the destination and the amount and the inspector accepts exactly that
  one transfer; a human decides every withdrawal, except a low-risk one auto-approved in a test environment —
  a rule the database also holds (`delegated_withdrawals_auto_only_in_test`); an agent can never withdraw
  directly; the daily limit holds under concurrency; a frozen account or a kill switch stops a withdrawal at
  every step (DW005); a cooling period delays execution; the database refuses an unlicensed signing (DW006) and
  a settlement that does not match (DW007).
- **Residual risk:** the customer must set a policy explicitly (there is no default); a signed but never
  submitted withdrawal is resolved only by the reconciler after its quote lifetime.
- **Tests:** `TestIntegration_WithdrawalAllowlistIsEnforced`, `TestIntegration_NoRawAddressSubstitutionAfterApproval`,
  `TestIntegration_NoAmountMutationAfterApproval`, `TestIntegration_AnAgentCanNeverWithdrawDirectly`,
  `TestIntegration_AHumanDecidesEverythingElse`, `TestIntegration_AnAutoApprovedWithdrawalSettlesInTestMode`,
  `TestIntegration_WithdrawalDailyLimitHoldsUnderConcurrency`, `TestIntegration_AFrozenAccountStopsWithdrawalsAtEveryStep`,
  `TestIntegration_TheCoolingPeriodDelaysExecution`, `TestIntegration_TheDatabaseRefusesUnlicensedWithdrawalStates`
  (`internal/delegation`).

## 15. Bridge double counting

- **Asset:** cross-chain buying power and the bridged twin's supply.
- **Attacker:** a customer who wants the same value counted on both chains, an attester or relayer that
  releases twice, or a reorg that undoes a lock after its release.
- **Entry point:** the bridge workflow (`bridge.Service.Bridge`, release attempts, refunds) and buying power V2.
- **Privilege needed:** `sandbox:bridge` for a customer; process access for the attester.
- **Attack:** spend source value that is locked and in flight; get a release minted twice or after a refund;
  count in-flight value as final collateral; release on a lock a reorg removed.
- **Controls:** the leg moves AVAILABLE → RESERVED → BRIDGE_REQUESTED → SOURCE_FINALIZED → MESSAGE_IN_FLIGHT →
  DESTINATION_FINALIZED → RECONCILED along a status machine the database enforces (BR001) with frozen
  identity (BR002); every bridge journal is bound to exactly one leg (BR005) and preserves the route's
  conservation identity (BR003): locked on the source = twins outstanding on the destination; the attester
  signs exactly the release its plan names, and a release is minted at most once; a release is attempted only
  after the lock is final, and finality never reverts; in-flight value is BRIDGE_IN_FLIGHT, which buying power
  counts only at the policy's haircut (zero by default) and which `ledger.HoldingCodes` treats as a mirror of
  the bridge's BRIDGE_LOCKED, so supply reconciliation counts the units once; route reconciliation compares the
  ledger with both chains and pauses the route on a mismatch.
- **Residual risk:** the attester is one key (no multi-party attestation in the sandbox); a release stuck
  beyond its attempt budget goes to manual review.
- **Tests:** `TestIntegration_ABridgeMovesValueOnceAndReconciles`, `TestIntegration_ADuplicateReleaseMintsNothingTwice`,
  `TestIntegration_ConcurrentDuplicatesSettleOnce`, `TestIntegration_AReorgNeverRevertsFinalityOrDoubleCounts`,
  `TestIntegration_ADelayedBridgeCreatesNoPhantomCapital`, `TestProp_IntegrationNoDoubleCounting`,
  `TestProp_EachLegIsCountedOnceWhereItsValueIs`, `TestIntegration_TheDatabaseRefusesBridgeBookkeepingBypasses`,
  `TestIntegration_BridgeReconciliationDetectsAnOutOfBandLock`, `TestAttester_SignsExactlyTheExpectedRelease`
  (`internal/bridge`); `TestIntegration_SupplyReconcilesWhereverTheUnitsAreHeld` (`internal/stableasset`); demo
  Scenario G.

## 16. Oracle manipulation (prediction markets and derivatives)

- **Asset:** the outcome a market settles on; derivative settlement prices.
- **Attacker:** whoever holds or impersonates the oracle; a replayer.
- **Entry point:** oracle reports, marks at expiry.
- **Privilege needed:** the oracle key, or network position to replay.
- **Attack:** forge, replay, or contradict a report; report before close; settle a disputed market.
- **Controls:** reports are Ed25519-signed and verified before use; a replayed report is idempotent and a
  conflicting one refused; a report must observe the event after close; a disputed market cannot settle;
  decoding is strict and fuzzed.
- **Residual risk:** a single sandbox oracle key; a live activation needs an external oracle with its own
  attestation and multiple attesters.
- **Tests:** `TestVerifyReport_RefusesEveryTampering`, `TestIntegration_OracleReplayIsIdempotentAndAConflictingReportIsRefused`,
  `TestIntegration_AnUnverifiableOracleReportIsRefused`, `FuzzDecodeOracleReport`, `TestIntegration_ADisputedMarketCannotSettle`
  (`internal/predmarket`); `TestIntegration_ASettlementIsPricedByTheRuleNeverByTheCaller`,
  `TestIntegration_FundingIgnoresMarksRecordedAfterTheInterval` (`internal/derivatives`).

## 17. Prediction resolution corruption

- **Asset:** a market's escrow.
- **Attacker:** a market creator or an operator.
- **Entry point:** resolve, uphold, void, settle.
- **Privilege needed:** `sandbox:resolve` (held by RISK and COMPLIANCE, not by OPERATIONS which lists markets).
- **Attack:** resolve one's own market, rule on one's own dispute, settle before resolution, pay out more or
  less than the escrow.
- **Controls:** the creator can never resolve or void their market and a disputer never rules on their
  dispute; the author of a disputed resolution can neither uphold nor overturn it, and the operator who recorded
  the sandbox oracle's observation cannot resolve the market (PM006); a void is proposed by one operator and
  approved by a different one, neither the creator nor the author of the resolution in force, and a RESOLVED
  market inside its dispute window is voided only through a dispute (PM007); oracle observations are stored,
  signed, append-only and audited, so every replica resolves from the same verified report (PM010);
  settlement needs a resolution and no open dispute; the settlement row checks its own conservation in SQL
  (escrow before = total paid + total held + residual); one frozen or unverified holder cannot block a
  settlement — their payout is held in escrow and claimed once eligible, exactly once (PM009); an outcome share
  exists only as its market issued it — no faucet grant, house or facility holding, or foreign journal kind can
  move one (PM008); working orders on the outcome books are cancelled at close, void and settlement;
  concurrent settlements settle once; escrow reconciliation detects divergence.
- **Residual risk:** one resolver decides a resolution (two operators are required to void, not to resolve);
  the sandbox oracle's observation is entered by an operator, so the oracle is only as independent as the
  separation between the recorder and the resolver; collusion between two roles is out of scope for the
  sandbox.
- **Tests:** `TestIntegration_AVoidNeedsTwoDistinctOperators`, `TestIntegration_TheResolverCannotRuleOnTheDisputeOfTheirOwnResolution`,
  `TestIntegration_TheObservationRecorderCannotResolveTheMarket`, `TestIntegration_SandboxObservationsAreDurableAuditedAndSharedAcrossReplicas`,
  `TestIntegration_AFrozenHolderCannotBlockSettlementAndClaimsOnceEligible`, `TestIntegration_AnOutcomeShareExistsOnlyAsItsMarketIssuedIt`,
  `TestIntegration_TheOrderBookCancelsRestingOrdersAtCloseVoidAndSettlement`, `TestProp_SettlementPaysExactlyTheEscrow`, `TestProp_PayoutConservation`, `TestIntegration_NoSettlementBeforeResolution`,
  `TestIntegration_ConcurrentSettlementsSettleOnce`, `TestIntegration_ReconciliationDetectsAnEscrowDivergence`,
  `TestIntegration_ADisputeCanBeUpheldOrEndInAVoid` (`internal/predmarket`).
