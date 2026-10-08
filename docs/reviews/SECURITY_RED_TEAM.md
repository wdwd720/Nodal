# SECURITY RED TEAM

An adversarial attempt to break the integrated advanced capabilities (ADR-0020), as the goal's §41 asks:
agent authority, the user-code sandbox, signing, withdrawals, transfers, stable mint, margin limits, the
matching engine and the provider boundary, plus the HTTP surface. The reviewer was read-only and had not
written the code. Target: `main` at `a982e27`, after every subsystem, its API and the first two reviews'
fixes had merged. Every confirmed finding has a reproduction — a Go test compiled into the package under
review with `go test -overlay`, so nothing was written into the repository — and counts as fixed only when
the reproduction fails at the line that asserted the defect and a regression test in the package proves the
broken property.

Severity is stated as if the value were real, as the threat model does; every value involved is `SIMULATED`
(VD002 keeps it from ever mixing with real capital).

## Result

No High finding. One Medium and four Low findings, four of them confirmed by running a reproduction; four
observations with no concrete path in the reviewed build. Every finding and three of the observations are
fixed; the fourth observation is recorded as a residual risk of the sandbox.

| ID | Sev | Finding | Fix | Regression tests |
|---|---|---|---|---|
| F1 | Med | A delegated swap's action label (ACQUIRE_EXPOSURE / REDUCE_EXPOSURE) was the agent's choice and not tied to the direction of value, so labelling a purchase REDUCE passed a REDUCE-only capability, a RESTRICTED account's risk-reduction-only rule and the platform's single-trade cap | `a05a432` (00762, DG007): the action is derived from what the swap does (`delegation.SwapAction`) and every check — the capability, Risk V2's risk-increasing flag, the account status, the pre-signing re-check, the signer — uses it; a label that disagrees is refused (`DELEGATION_ACTION_MISMATCH`) and recorded; the database checks the stored label against the swap's assets at admission and again at signing | `TestIntegration_AReduceOnlyCapabilityNeverBuysExposure`, `TestIntegration_ARestrictedAccountsAgentOnlyReducesExposure`, `TestIntegration_ThePlatformSingleTradeCapBindsEveryPurchase`, `TestIntegration_TheDatabaseBindsASwapsActionToItsDirection`, `TestIntegration_SQLAndGoAgreeOnWhatASwapDoes`, `TestSwapActionIsTheDirectionOfValueNeverTheLabel` (`internal/delegation`) |
| F2 | Low | `STRATEGY_VERSION_DISABLE` on an agent's strategy version did not stop its delegated swaps and withdrawals (`AGENT_PAUSE` did) | `a05a432`: every kill-switch check of an agent — request, signing, the pre-submission gate, and a withdrawal's proposal, approval, signing and gate — names its current strategy version; Risk V2 refuses new risk under `STRATEGY_VERSION_DISABLE` | `TestIntegration_AStrategyVersionKillSwitchStopsItsAgentAtEveryStep`, `TestEveryKillSwitchCheckOfAnAgentNamesItsStrategyVersion` (`internal/delegation`); `TestAdvanced_StrategyVersionDisableStopsThatVersionsNewRisk` (`internal/risk`) |
| F3 | Low | One customer's withdrawals in their cooling period filled every slot of the operator's execution run, so other accounts' admitted swaps expired unexecuted | `a05a432`: an execution run takes only what can be signed now (never a cooling withdrawal, a swap past its quote or another network's subject), the accounts take turns, at most ten of one account's subjects per run, the soonest-expiring quote first (`delegation.Service.ExecutableSubjects`) | `TestIntegration_AnExecutionRunTakesWhatCanBeSignedNowEveryAccountInTurn` (`internal/delegation`); `TestIntegration_DelegationCoolingWithdrawalsCannotHoldAnotherAccountsSwapBack` (`internal/httpapi`) |
| F4 | Low | A strategy build within every bound (time, memory, module, log) wrote its bulk into the Go build cache the API shares, and nothing cleaned it: disk was not bounded | `1f1645e`: every build runs in a directory of its own — sources, its own Go cache, the toolchain's temporary files — bounded on disk while it runs (`MaxDiskBytes`, refused `BUILD_OUTPUT_LIMIT`) and removed when it ends, whatever the outcome; at most `MaxConcurrentBuilds` at once, so the work area is bounded; the shared directory holds only a read-only seed every build copies; a dead process's directory is swept by the next builder | `TestBuilder_TwoBuildsLeaveTheSharedCacheAndTheWorkAreaAsTheyFoundThem`, `TestBuilder_ABuildPastItsDiskBoundIsStoppedWhileItRuns`, `TestBuilder_EveryOutcomeLeavesTheWorkAreaAsItFoundIt`, `TestBuilder_BuildsBeyondTheSlotsWaitAndGiveUpAtCapacity`, `TestBuilder_ADirectoryWhoseProcessDiedIsRemovedByTheNextBuilder` (`internal/sandboxrt`) |
| F5 | Low | By reading: the marketplace follow, prediction markets and equities executed a route the legal router permits only after a person's confirmation (review-1 F4's class); no loadable policy asks for confirmation on simulation, so no live path | `1f1645e`: all three proceed only on a route that may execute now (`MayExecuteNow`); a route that needs a confirmation or a review is refused with `REQUIRES_USER_CONFIRMATION` or `REQUIRES_MANUAL_REVIEW` among its reasons | `TestIntegration_AFollowNeverDispatchesARouteThatNeedsAPerson` (`internal/marketplace`), `TestIntegration_ARouteThatNeedsAPersonIsNeverExecuted` (`internal/predmarket`), `TestIntegration_AnOrderWhoseRouteNeedsAPersonIsNeverExecuted` (`internal/equities`) |
| O1 | — | No database rule tied holdings of a platform stable asset or a bridged twin to their issuance (the faucet's Go API could grant one) | `1f1645e` (00763, SU001-SU003): such an asset is supply-controlled; at commit, every transaction that touches it changes its holdings by exactly what it changes its controller's outstanding (no trade, grant or paper fill can create or destroy a unit); only the controller books its issuance accounts; only the controllers' kinds and ordinary customer movements may move it; the faucet refuses it by name | `TestIntegration_AStableAssetsUnitsExistOnlyAsItsIssuerIssuesThem`, `TestIntegration_ABridgedTwinsUnitsExistOnlyAsItsBridgeReleasesThem`, `TestIntegration_TheFaucetNeverGrantsAnAssetWhoseSupplyAControllerOwns`, `TestIntegration_TheSupplyRuleCountsTheLedgersHoldingCodes`, `TestIntegration_AnAssetWithLedgerAccountsNeverBecomesSupplyControlled` (`internal/sandboxfund`) |
| O2 | — | A delegation venue could be backed by a treasury house book, so its swaps bypassed the treasury's actions and limits (operator + step-up) | `a05a432` (DG008): a venue's house book is its own — never a treasury book or another venue's — and a venue's book can never become a treasury book, in Go and in SQL | `TestIntegration_AVenueNeedsAHouseBookOfItsOwn` (`internal/delegation`); `TestIntegration_DelegationVenueOnATreasuryBookIsRefused` (`internal/httpapi`) |
| O3 | — | The order book's journal proof replayed commands and state hashes but did not compare the recorded trades and their postings | `1f1645e`: every replayed trade must equal its recorded trade (sequence, price, quantity, parties, schedule hashes, fees recomputed from the book's frozen schedules), and its SIM_TRADE journal must move exactly that value and those fees, account by account; a difference is listed and fails the proof | `TestIntegration_TheJournalProofComparesTheRecordedTradesAndTheirJournals` (`internal/orders`) |
| O4 | — | Fee reversals and corporate-action applications are single-operator actions on simulated value | recorded as a residual risk of the sandbox: both are reason-coded, audited and exactly once, and a live activation would put them under dual control like transfer reversals (PT006-PT008) and derivative overrides (DV008) | — |

Integrating O1's rule showed that its database refusal was the only refusal on some paths: a platform stable asset
could be a margin account's numeraire (any USD-pegged SIMULATED asset can), and such an account's borrowing,
interest and liquidation postings would have been refused at commit — a liquidation that could never complete.
Fourteen entry points now refuse a supply-controlled asset up front, before anything is written, with
`ASSET_RESTRICTED` and the refusal `SUPPLY_CONTROLLED_ASSET` (`internal/econ/supply.go`): margin accounts, collateral
and leveraged buys; derivative positions and the clearing house's funding; equity listings, orders and corporate
actions; treasury funding and paper trades; the order book's paper venue; marketplace installs and follows; fee
periods; delegated withdrawals and swaps a venue could only fund by creating units. A table with one row per
advanced journal kind names each kind's guard or why it is unreachable, and is checked against the database's rule
(`TestSupplyKinds_EveryKindHasARow`, `TestSupplyKinds_EveryGuardRefusesSupplyControlledAssets`,
`TestIntegration_SupplyKindsAgreeWithTheDatabase`; `TestIntegration_AMarginAccountNeverBooksASupplyControlledAsset`).
Configurations made before the guards are not re-checked; none exists outside test databases.

## What was attacked and held

1. **Agent authority.** Cross-account use, capability self-expansion, use after expiry or revocation, an agent
   on any HTTP route, an agent executing or deciding: all refused — the capability is hashed over every term
   and frozen (DG002), its use is bound to its account (DG003), it is re-checked at request, signing and the
   pre-submission gate (DG005), and every HTTP route refuses an agent principal. F1 and F2 were the gaps.
2. **Signing.** No path signed anything but the persisted plan: the inspector compares byte-exact recompiled
   messages (plan hash in the memo, signer set, fee payer and fee, venue legs), the decision is bound to the
   message's hash, the venue co-signs only the recompiled plan, the stable issuer and custodial signers
   inspect against their plan, and the bridge attester signs only the expected release.
3. **Withdrawals.** Destination and amount substitution, the daily, velocity and per-withdrawal limits, a frozen
   account and the cooling period all held, in Go and in SQL (DW004-DW007); auto-approval exists only in
   LOCAL, TEST and DEV, in the service and in the database.
4. **Transfers.** A reversal needs two distinct operators (PT006-PT008) and takes the recipient's funds only
   through a reservation; idempotency per sender and key with the request hashed.
5. **Stable mint.** Admission under the asset's row lock against the cap and the full reserve (receivables and
   pending payouts included), again at commit (SV003, SV004); reserve prices only from the oracle source and
   capped at par; snapshots signed and hash-chained (SV007). O1 was the missing binding of holdings to issuance.
6. **Margin limits.** Collateral leaves the wallet for MARGIN_COLLATERAL, so no other spender can take it;
   equity excludes wallet funds, so withdrawing borrowed funds cannot breach maintenance.
7. **Matching engine.** Ownership on cancel and replace, self-trade prevention in Go and SQL, house/customer
   crossing only under a rule, every house order bound to an applied treasury action (TY007), and an
   append-only journal with its cursor and hash chain enforced (OB001, OB002, OB005). O3 was the proof's gap.
8. **Sandbox.** The scanner (import allow-list, every `//go:` and `//line` directive, build constraints,
   file names, encoding), a build environment assembled from nothing with no module proxy and a local
   toolchain, and a WASI-only runtime with no filesystem, environment, sockets or real clock, deterministic
   randomness and metered memory all held; strategy output is two record types with exact keys and becomes
   only risk-checked intents. F4 was the one missing bound.
9. **Provider boundary.** LIVE is refused for every capability; simulated modes need an active gate in staging
   and production, where the development legal policy and plain secrets are also refused; the sandbox oracle
   with its published key exists only where fake providers are allowed; the sandbox seed is only ever used to
   derive keys and is never formatted or logged.
10. **HTTP surface.** Every account-scoped write requires ownership; a record read by id answers NOT_FOUND across
    tenants; decisions bind the plan hash; idempotency keys are scoped per actor and operation with the request
    hashed; bodies are capped at 1 MiB; amounts are pattern-checked and range-validated.

## Method

Reproductions and their logs are kept outside the repository by design, in the scratchpad of the session that
ran the review; each was re-run against its fix and now fails at the line that asserted the defect. The regression
tests that replace them live in the packages named above. The earlier reviews are `ADVANCED_SYSTEMS_REVIEW.md`;
what may be claimed from all of this is `PORTFOLIO_CLAIMS_REVIEW.md`.
