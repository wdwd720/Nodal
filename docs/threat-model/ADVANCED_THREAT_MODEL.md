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
| 1 | Stable mint compromise | `internal/stableasset` | written when the stable-asset subsystem merges |
| 2 | Reserve falsification | `internal/stableasset` | written when the stable-asset subsystem merges |
| 3 | Margin bypass | `internal/margin` | below |
| 4 | Liquidation manipulation | `internal/margin` | below |
| 5 | Mark-price manipulation | valuation, `internal/margin`, `internal/derivatives` | below |
| 6 | Matching-engine race | `internal/matching`, `internal/orders` | engine below; persistent book when `internal/orders` merges |
| 7 | Transfer replay | `internal/transfers` | below |
| 8 | House/customer asset commingling | ledger, `internal/econ` | below (treasury book when it merges) |
| 9 | Fee manipulation | `internal/fees`, fee ledger | written when the fee ledger merges |
| 10 | Marketplace malicious strategy | `internal/marketplace` | below |
| 11 | Sandbox escape | `internal/sandboxrt` | below |
| 12 | Agent capability escalation | kernel, settlement compiler, delegation | kernel below; delegation when it merges |
| 13 | Signing transaction substitution | delegation | written when delegated signing merges |
| 14 | Withdrawal address substitution | delegation | written when delegated withdrawals merge |
| 15 | Bridge double counting | `internal/bridge` | written when the bridge merges |
| 16 | Oracle manipulation | `internal/predmarket`, `internal/derivatives` | below |
| 17 | Prediction resolution corruption | `internal/predmarket` | below |

---

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
  mark/index divergence beyond policy refuses new risk; expiry settlement uses only a fresh mark at or before
  expiry.
- **Residual risk:** one source per mark in the sandbox — no median of independent feeds — and one operator can
  record a mark. A live activation needs multi-source aggregation and dual control on manual marks.
- **Tests:** `TestIntegration_StaleOraclePreventsARiskIncreasingOrderButNotClosing`,
  `TestIntegration_MarkIndexDivergenceRefusesNewRisk`, `TestSpec_PricesOffTheTickGridAreRefused`,
  `TestIntegration_SettlementPriceMustBeOnTheTickGrid` (`internal/derivatives`);
  `TestHaircut_StaleMissingFutureOrStuckIsWorthless`, `TestEvaluate_StaleMarkIsZeroCollateralAndCallsMargin`
  (`internal/margin`); `TestIntegration_AMarkValuedAssetFailsClosedWithoutAFreshMark` (`internal/transfers`).

## 6. Matching-engine race (engine)

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
- **Residual risk:** persistence, reservations and posting of fills belong to the persistent book
  (`internal/orders`), whose controls are added here when it merges.
- **Tests:** matching property and replay tests in `internal/matching`; `make perf-advanced` measures replay
  determinism (full replay and snapshot + tail reproduce the live state hash).

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
- **Residual risk:** the principal/treasury book (house trading) is added to this entry when it merges.
- **Tests:** `TestIntegration_HouseAndFacilityBooksCannotHoldRealValue`, `TestIntegration_FacilityBooksAreSegregatedByKind`,
  `TestIntegration_ASimulatedOnlyKindCannotTouchRealCapital`, `TestIntegration_ATransferMustBalancePerParty`
  (`internal/econ`); `TestProp_EveryTemplateBalancesPerPartyAndIsSimulatedOnly` (`internal/margin`);
  `TestIntegration_TheDatabaseRefusesAClearingFlowThatDoesNotBalancePerParty` (`internal/derivatives`);
  `TestIntegration_TheDatabaseRefusesAPredictionPostingThatDoesNotBalancePerParty` (`internal/predmarket`).

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

## 12. Agent capability escalation (kernel)

- **Asset:** customer funds and authority.
- **Attacker:** an agent, a strategy's output, or a compromised model.
- **Entry point:** agent intents, strategy proposals, the HTTP surface.
- **Privilege needed:** an agent bound to one account.
- **Attack:** move value between customers, withdraw, post collateral, call an operator action.
- **Controls:** agent principals never reach the HTTP surface; the settlement compiler forbids
  `SIMULATED_TRANSFER_VALUE` and `SIMULATED_WITHDRAW` to agents at every authority level; agents only create
  intents and never apply effects; the faucet refuses agents; strategy output becomes only risk-checked intents.
- **Residual risk:** review finding F5 (an agent requester could post a customer's collateral) is being fixed in
  the margin hardening work. Delegated signing and withdrawals are added here when they merge.
- **Tests:** `TestRoute_TheCompilerForbidsAgentTransfersAtEveryLevel`, `TestIntegration_AnAgentNeverTransfersAndTheCompilerAgrees`
  (`internal/transfers`); `TestNoOperationAdmitsAnAgent` (`internal/httpapi`); `TestIntegration_AnAgentCannotTouchTheMarketplace`
  (`internal/marketplace`); `TestProp_NoStrategyOutputBecomesATransferOrWithdrawal` (`internal/sandboxrt`).

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
  (`internal/predmarket`); `TestIntegration_SettlementPriceMustBeOnTheTickGrid` (`internal/derivatives`).

## 17. Prediction resolution corruption

- **Asset:** a market's escrow.
- **Attacker:** a market creator or an operator.
- **Entry point:** resolve, uphold, void, settle.
- **Privilege needed:** `sandbox:resolve` (held by RISK and COMPLIANCE, not by OPERATIONS which lists markets).
- **Attack:** resolve one's own market, rule on one's own dispute, settle before resolution, pay out more or
  less than the escrow.
- **Controls:** the creator can never resolve or void their market and a disputer never rules on their
  dispute; settlement needs a resolution and no open dispute; the settlement row checks its own conservation in
  SQL (escrow before = total paid + residual); positions are a view over the ledger; concurrent settlements
  settle once; escrow reconciliation detects divergence.
- **Residual risk:** one resolver (not two) decides a resolution; collusion between two roles is out of scope
  for the sandbox.
- **Tests:** `TestProp_SettlementPaysExactlyTheEscrow`, `TestProp_PayoutConservation`, `TestIntegration_NoSettlementBeforeResolution`,
  `TestIntegration_ConcurrentSettlementsSettleOnce`, `TestIntegration_ReconciliationDetectsAnEscrowDivergence`,
  `TestIntegration_ADisputeCanBeUpheldOrEndInAVoid` (`internal/predmarket`).
