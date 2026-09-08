# PRODUCTION READINESS REPORT

Against `gola.md`. Written at the point where the Nodal-native economy exists and the rest of the
final architecture does not yet.

`gola.md` PART XCVIII requires six readiness levels to be distinguished rather than collapsed. They
are not synonyms and this report does not treat them as such.

| Level | Status |
|---|---|
| **CODE READY** | **NO** — Stage 10 is externally blocked, Stage 21's committed write path is unmeasured, and Stage 24 is deliberately unwritten |
| **SANDBOX READY** | **PARTIAL** — the internal economy runs end to end against an in-house payout double, and five provider adapters pass contract suites against documented fixtures; no LIVE external sandbox is reachable (every one is application-gated or has no contract) |
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
| A cross-domain movement must declare which conversion it is | `TestIsolation_CrossDomainPostingMustDeclareItsConversion`, `TestIntegration_CrossDomainPostingMustDeclareItself` |
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

### The interface (PARTS LII, LIV)

| Property | Evidence |
|---|---|
| No code path produces a figure that sums the three kinds of value | there is no such hook in `apps/web/src/api/queries.ts`; the separation is structural rather than a layout convention |
| A page never shows a Credit figure and a currency figure together | `no page converts Credits into a currency` (source scan) and `no page puts a Credit figure and a currency figure together` (browser, reads rendered text) |
| Home names all three pots and says they are never added | `home names all three pots and adds none of them` |
| A page showing Credits says what Credits are | `a page showing Credits says what Credits are` |
| A page showing a user-created asset carries the risk statement | `a page showing a user-created asset carries the risk statement` |
| The four internal-economy pages render, have one `h1`, and are reachable from the navigation | `the internal economy is separate from the rest` |
| Every route passes the rendered-text honesty scan and the WCAG A/AA automated scan | `honesty.spec.ts`, `accessibility.spec.ts` — 13 routes each |
| A failure to reach the backend is never reported as being signed out | F-20; only a 401 renders the sign-in screen |
| A creator sees the exact immutable economics before publishing, and confirms those rather than the form | `CreateAsset` is two steps; the confirmation carries a key created at that moment |
| The order form never defaults the minimum output to the quote | it starts empty; a default equal to the quote is a zero slippage tolerance that would refuse every trade |
| Create Asset and the market pages pass the rendered-text honesty scan and the WCAG scan | both suites cover `/create-asset` |

Browser suite: **69 tests, consecutive clean runs**, against a production `vite build`
served by `vite preview`, proxying to the real `cmd/api` on a seeded database. Sign-in is the real
OIDC round trip. The run needs the transport rate limits raised
(`CP_API_RATE_LIMIT_*=100000/1m`) because a 66-test suite is not a person; the limiter correctly
refuses to be disabled in a production-like environment.

### Fault injection (Stage 20)

| Property | Evidence |
|---|---|
| A purchase killed mid-transaction leaves no order, no posting and no moved balance | `TestChaos_APurchaseDiesMidTransaction` — `pg_terminate_backend` on the exact backend, after every write and before COMMIT |
| A trade killed mid-transaction leaves the state version, the reserves and the constant product untouched | `TestChaos_ATradeDiesMidTransaction` |
| Eight concurrent buyers survive one of their backends being killed: the buyer pays for exactly the purchases that committed | `TestChaos_ConcurrentPurchasesSurviveABackendBeingKilled` |
| After every fault, provenance, commerce earnings and the market reserve control account still reconcile | `checkNoDrift` in all three |
| After every fault the same command retried still succeeds | asserted in all three; a guard that survives by staying broken has not survived |
| Both new guards have been OBSERVED failing | `CP_CHAOS_BREAK=commerce_partial_write` and `=native_trade_partial_write`, each producing the expected failure |
| The whole chaos suite runs with nothing skipped | 10/10 with `CP_TEST_REDPANDA_BROKERS` and `CP_TEST_ARCHIVE_ENDPOINT` set; it was 8 passing and 2 skipped |

### Load (Stage 21) — the committed write path, measured

Run here against the real `cmd/api` binary on a freshly seeded local database,
with the MARKETPLACE gate ACTIVE through the real three-principal ceremony
(`scripts/gateceremony`, which drives `gates.Admin` rather than writing a row).

| Measure | Result |
|---|---|
| Requests | 2,864 at 95 req/s |
| Latency | p95 469 ms overall; **10.3 ms** on successful requests; median 4.6 ms |
| Purchases COMMITTED | **31** — the entire seeded balance, 31 × 800 Credits = 24,800 of 25,000 |
| Refused after that | 120 `LEDGER_NEGATIVE_BALANCE`, which is the correct answer to a buyer with 200 Credits and an 800-Credit product |
| Refused `IDEMPOTENCY_IN_PROGRESS` | 49 — the shared-key iterations racing, the idempotency store working |
| Server errors | none |
| Refusal shape | every refusal delivered as problem+json |
| Financial invariant | the buyer's Credits fell by exactly the 31 purchases that committed; fees + proceeds = price to the base unit |

**The run's real value was the two defects it found.** With the gate genuinely
active, the first run measured p95 **30.07 s**, five commits out of two hundred,
three 500s and 124 requests killed by the statement timeout. That is F-27: the
capability check inside a financial transaction read through the connection
POOL, so every write held one connection and reached for another, and a dozen
concurrent writes deadlocked the pool until the timeout. Reaching the run at all
needed F-26 fixed first — two required compiler inputs `cmd/api` never supplied,
which made the entire internal economy unreachable in every deployment.

**What still bounds the number.** Committed throughput here is capped by the
SEEDED BALANCE, not by the system: 25,000 Credits buys 31 of an 800-Credit
product, and the remaining 169 iterations are correctly refused. A capacity
number would need a larger seed, and it would still be a laptop number.

### This report's own citations

| Property | Evidence |
|---|---|
| Every test this report names exists | `TestDocs_EveryTestTheyNameExists` — it also covers `AUDIT_FINDINGS.md`, `INDEPENDENT_AUDIT.md`, `MASTER_BUILD_STATE.md` and `ADVERSARIAL_VALIDATION.md` |
| The check found a real broken citation on its first run | F-25: this report offered `TestIsolation_CrossDomainPostingMustDeclareItself`, which does not exist |

### Every rail compiles through one place (Stage 12, PART XXVI)

| Property | Evidence |
|---|---|
| A Domain B or C trade intent is compiled before the intent service is touched | `TestIntegration_ARealCapitalIntentIsRefusedAtTheEdge` — a refused intent never reaches `intent.Submit` |
| A fresh deployment refuses real capital at the edge, naming the policy that refused it | same test: `policy_version`, `policy_rule_index` and every reason on a 403 |
| A fresh deployment still permits simulation, so the refusal is about real capital and not a broken endpoint | same test: PAPER is accepted in the same run |
| The refusal is a recorded conclusion, so replaying the key reproduces it rather than re-asking the policy | same test: two deliveries, identical code, detail and fields |
| Whether capital is real is decided by the MODE alone, never by the instrument | `TestIntentRouting_TheModeDecidesWhetherCapitalIsReal` |
| Which real rail it travels is read from the instrument's base asset, never guessed | `TestIntentRouting_TheRailComesFromTheInstrument`, including a domain with no real rail |
| Every declared intent action has a direction | `TestIntentRouting_TheDirectionFollowsTheAction`, which fails when a new action is added |
| `TARGET_EXPOSURE` may be routed as a buy only while both sides of a rail route identically | `TestProfiles_BuyAndSellAgreeOnEveryExternalRail` in `internal/settlement` |
| A deployment that wired no policy refuses real capital rather than permitting it | `Ports.SettlementPolicy` is a struct whose ZERO VALUE is the conservative deployment |
| The deployment's OWN policy is the one that answers, not the default | `TestWire_CarriesTheDeploymentsSettlementPolicy` (observed failing without the wiring) and `TestIntegration_AConfiguredPolicyActuallyReachesTheIntentCompiler` — this is F-24 |

### One randomised test found on this commit (F-30)

`TestProp_ByteFlipsNeverApprove` failed on a commit that had just passed the
full integration sweep — randomised tests do that, because the seed changes
every run. It had drawn two flips of the same bit, which cancel, so the
"mutated" transaction was byte-identical to the golden one and the inspector
approved it correctly.

The false alarm was the small half. rapid shrinks toward small values, so
`flips=2, pos=0, xor=1` is where the shrinker goes from ANY failure — a real
defect would have been shrunk into "identical bytes approved" and reported with
an explanation that had nothing to do with it. The case is now discarded, and
both controls were run: swallowing every case leaves the test passing while
proving nothing, and an inspector that approves everything still fails it.

### Security and supply-chain scans, run on this commit

| Scan | Result |
|---|---|
| `govulncheck ./...` | no vulnerability this code calls; 1 in a required module nothing reaches |
| `gosec` (`make sast`) | **clean** — and it had never been clean: seven findings, every one a suppression that did not satisfy `-nosec-require-justification`, so the target failed for anyone who ran it |
| `gitleaks` (`make secrets`) | **clean** — one finding, in `internal/gen/api/api.gen.go`, which embeds the OpenAPI document as base64'd gzip; adding two endpoints was enough to trip the entropy rule |
| `trivy config` (`make iac-scan`) | 0 HIGH/CRITICAL across every terraform module and environment |
| SBOM (`make sbom`) | generated, 6.1 MB SPDX |
| `terraform validate` | dev, staging and prod all valid; `terraform fmt -check -recursive` clean |

Two of those are worth more than a tick.

**`make sast` had never passed.** Its flags require every `#nosec` to name the
rule it silences AND state the invariant that makes it safe, and seven
suppressions predating this session said only `#nosec G101` or nothing at all. A
scan target that fails is a scan nobody runs. The seventh was the best of them:
`internal/nativeasset/moderation.go` was flagged for Trojan Source because it
contains bidirectional control characters — it is the code that REFUSES them in
user-supplied asset names. They are now written as `'‪'` escapes, which
removes the characters from the source and reads better anyway: a reviewer can
see which codepoint each one is instead of an invisible glyph.

**`make secrets` was red for a generated file.** `internal/gen` is allowlisted
by path with the reason, which is the same argument `make sast` already makes
with `-exclude-generated`: nothing there is hand-written, `make gen` overwrites
any annotation, and the SOURCES those files are generated from are scanned
normally — which is where a real secret would have to be introduced first.

### Backup and restore, over Domain A data

| Property | Evidence |
|---|---|
| A backup of a database holding a live internal economy restores complete | `make restore-drill`: 118 tables, row counts match, journal hashes match on both sides, zero balance drift |
| The restored copy carries the Domain A tables with DATA in them | 1 native-market fill, 1 commerce order, 4 credit lots, 2 lot events, 2 published prices, 1 market, 1 instrument, 3 audit events |
| The fixture is made the way the application makes it | `scripts/restoredrill/domaina.go` drives the REAL services, so the restored rows satisfy the deferred balance triggers, CR004, AU001, IC001 and the terms-frozen guard by having been written through them |
| Migration state survives | source and restored both at version 715, checksums verified on the restored copy |

Until this session the drill's fixture was users, accounts, one asset and 25
journal transactions. Every Domain A table restored EMPTY, so "row counts match"
compared zero with zero. A backup proven only on tables nobody uses is not a
proven backup.

### Every method that moves money can be reached (F-29)

| Property | Evidence |
|---|---|
| A user can cancel their own pending payout and get the exact lots back | `TestIntegration_AUserCanCancelTheirOwnPayoutAndNobodyElses` |
| A stranger cannot, and cannot learn the payout exists | same test: NOT_FOUND, and the request is unchanged |
| Every financial mutator has a caller a deployment can run | `TestReachability_EveryFinancialMutatorHasADeploymentCaller` — a caller in `test/` does not count |
| The eleven exemptions each name a real external blocker | `TestReachability_EveryExemptionNamesABlocker` — checked against `BLOCKERS.md` |
| The check was observed failing on both defects it was written for | `payout.Cancel` and `nativeasset.Activate`, with their callers removed |

### The registry and the venue cannot disagree

| Property | Evidence |
|---|---|
| Halting, closing, freezing or resuming a market moves its instrument with it | `TestIntegration_HaltingAMarketHaltsItsInstrument` — CLOSE_ONLY, HALTED, ACTIVE and FROZEN in one sequence |
| FROZEN maps to HALTED, because the registry has no word for "no exits either" | same test; the mapping loses detail only in the direction of permitting less |
| A market with no instrument still halts | `mirrorInstrumentStatus` treats a missing row as nothing to do: stopping must never be the harder path |
| A registry transition the registry itself refuses is reported, not routed around | the error names both ids so the two can be reconciled |

This closes a gap the previous version of this report named: the registry could
say an asset was ACTIVE while its market was halted. Two sources for "what may
be traded" eventually disagree, and the disagreement is discovered by something
moving that should not have.

### A native market can be launched (F-28)

| Property | Evidence |
|---|---|
| A creator can submit their own DRAFT for review | `POST /native-assets/{assetId}/submit`; a stranger gets NOT_FOUND, not FORBIDDEN |
| Launching a market takes two people, and then the market trades | `TestIntegration_LaunchingAMarketTakesTwoPeopleAndThenItTrades` — the whole chain through the real surfaces, ending in a buy |
| A moderation verdict does not start trading | same test: the asset is still PENDING_REVIEW after APPROVED |
| A DRAFT cannot be launched | `TestIntegration_ADraftCannotBeLaunched` — submitting is what freezes the economics |
| A launch with no opening price is refused | `TestIntegration_ALaunchWithNoOpeningPriceIsRefused` — missing, zero and unparseable |
| One approval mints one supply | the mint is keyed by the approval id |
| The market opens with the economics that were APPROVED | asserted against the params hash `Execute` re-verifies |

### Reality, Prediction and Proof over Domain A (Stage 15)

| Property | Evidence |
|---|---|
| Every fill publishes the market's post-trade spot price, in the trade's transaction | `TestIntegration_ATradePublishesAPriceWhoseKnowledgeTimeIsTheTrade` |
| A market publishes an opening price at creation, so a prediction made before the first trade is resolvable | same test — without it the resolver refuses, correctly, rather than inventing a price |
| `observed_at` equals `received_at`, because Nodal is the venue and has no provider clock to lag behind | same test, asserted rather than assumed |
| Every price names the fill that set it | `raw_ref = native_market_fill:<id>` |
| Two prices from one market never share an instant, on an append-only table with no UPDATE grant | same test — the stamp is nudged forward by a microsecond, never backward |
| A native market is registered as a `SPOT_PAIR` instrument, so predictions can name it | `TestIntegration_ANativeMarketIsRegisteredAsAnInstrument` |
| A prediction on a Domain A market resolves through the REAL ledger and resolver, with no Domain A special case | `TestIntegration_APredictionOnANativeMarketResolvesFromNodalNativePrices` |
| **No lookahead**: a price received after the horizon end cannot change the outcome | same test, with a control asserting the price really did move afterwards |
| A native trade is in the trader's audit stream and that stream VERIFIES | `TestIntegration_ATradeIsInTheAccountsVerifiableAuditStream` (`audit.Verifier`) |
| An audit event cannot commit without its trade | same test: a rolled-back trade leaves the event count unchanged |
| A purchase is in both parties' audit streams | `internal_commerce.purchase` and `internal_commerce.sale`, written in the purchase's transaction |

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

### Open findings

**F-31 — four of a hundred concurrent buyers failed once, and nobody can say why.**
The run took 24.2s against 16–20s for every subsequent run, on a machine
simultaneously serving a load test; it did not recur in eight full-suite runs or
six runs of the test alone. Every financial invariant in that run passed — the
state version moved once per trade, the curve invariant held, supply reconciled
— so the four buys that failed left nothing behind, which is what a refused
transaction looks like rather than a partial one.

It is P3 and open. The test discarded its errors, so the run that failed
produced a number with no cause attached; it now keeps the first error. What
would close this is the error text from a recurrence, not another clean run.

### Not built

- **Hosted partner rail** (Stage 10). Declared, `Implemented() == false`, no adapter.
- **No commerce moderation tooling** (part of Stage 17). `commerce:moderate` exists as a permission
  and `internal_sellers.status` and the product lifecycle support suspension and withdrawal, but no
  admin workflow drives them: an operator would have to run SQL. The controls exist; the operator
  interface to them does not.
- **`intent.TradeIntent` and `settlement.FinancialIntent` still coexist.** Every trade intent is now
  COMPILED as a `FinancialIntent` before `intent.Submit` is reached, so the permission decision has
  one home. What has not been unified is the record: an accepted intent is still persisted and
  planned as a `TradeIntent` through `V1Planner`. That is a representation question, not a control
  question, and collapsing the two types is work rather than risk.
- **The risk kernel is not yet an input to the Domain A route.** `Route.RequiresRiskEvaluation` is
  determined and recorded; nothing consumes it for internal trades yet.
- **No kind-SPECIFIC admin screens for Domain A** (part of Stage 16). The nine administrative
  actions are fully operable from the existing console today: its propose form is driven by the
  generated `authority.json`, which now lists all nine, and it carries a free-form params field that
  the two parameterised kinds need. What is missing is kind-specific UI — a market picker rather than
  a pasted uuid, a moderation-state dropdown rather than hand-written JSON. That is a usability gap,
  not a missing capability, and an operator pasting the wrong uuid into a freeze is the risk it
  leaves open.

### Not run

- A committed-throughput number not bounded by seed data (part of Stage 21) — see the table above.

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
| 5 | out-of-order webhook | `TestIntegration_OutOfOrderWebhooksNeverMoveADepositBackwards` — the newest event delivered first, then both older ones; plus `TestIntegration_ARejectionArrivingAfterConfirmationEscalates` |
| 6 | provider timeout after success | `TestIntegration_AProviderTimeoutDoesNotDuplicateThePayout` |
| 7 | process crash after provider success | `TestIntegration_Part49_CrashRecovery`, `TestExecutor_ResumeAfterCrash_SubmitRunningNeverResubmits` |
| 8 | chargeback after Credits spent | `TestIntegration_ChargebackAfterTheCreditsAreSpent` |
| 9 | payout request during pending chargeback | `TestIntegration_APayoutIsRefusedWhileTheFundingIsDisputed` |
| 10 | payout using promotional Credits | `TestIntegration_PromotionalCreditsCannotCashOut` |
| 11 | payout using ineligible trading proceeds | `TestIntegration_PayoutEnforcesProvenance` |
| 12 | market creator attempting hidden supply increase | `TestIntegration_ACreatorCannotChangeEconomicsAfterLaunch` — "raise max supply", through the migration role |
| 13 | self-trade | `TestIntegration_SurveillanceRaisesAlertsWithoutBlocking` — `CREATOR_SELF_DEALING` raised, trade not blocked |
| 14 | two accounts under one controlled actor wash-trading | `TestIntegration_WashTradingAcrossTwoAccountsOfOneUserIsDetected`, with `TestIntegration_TwoUnrelatedAccountsTradingIsNotWashTrading` as the control |
| 15 | stale quote | `TestContract_Order_StaleQuote`, `TestIntegration_QuotesAreRecordedAndExpire` |
| 16 | market state changes between quote and execution | `TestIntegration_AStaleFillIsRefusedByTheDatabase` (SQLSTATE NM002) |
| 17 | integer overflow | `TestCurve_ExtremeSizesDoNotOverflow`, and `money` is integer-only by construction |
| 18 | rounding edge at minimum unit | `TestSplit_RoundsTowardTheCreator` (price 1, 3 and 4 at the 30% cap), `TestProp_ManyTinyTradesCannotExtractValue` |
| 19 | malicious negative quantity | `TestCompile_MaliciousQuantitiesAreRefused` |
| 20 | malicious huge quantity | same — and it asserts the compiler does NOT cap: affordability is the ledger's question |
| 21 | replay old request | `TestIntegration_IdempotentReplayIsPersisted`, `TestRecordedRejectionIsReplayedNotReExecuted` |
| 22 | modify idempotency payload | `TestIntegration_IdempotencyKeyReuseWithADifferentBodyConflicts` |
| 23 | compromised provider sends contradictory status | `TestIntegration_ACompromisedProviderCannotRewriteAFinishedPayout` and three more, including the agreeing-provider control (F-23) |
| 24 | agent tries to withdraw | `TestCompile_AnAgentCanNeverRequestAPayout` — every level, every capability active |
| 25 | agent attempts prohibited asset | `TestCompile_AnAgentIsRefusedAProhibitedAsset` — with a human and a different asset as controls |
| 26 | agent attempts limit escalation | `TestPermits_ForbiddenActionsAreRefusedAtEveryLevelWithEveryCapability` (`CHANGE_OWN_LIMITS`) |
| 27 | admin attempts unaudited balance mutation | `TestNoBalanceEditKindExists` — no admin action kind can edit a balance; repair is a compensating journal transaction |
| 28 | mock provider used in production config | `TestRegistry_ProductionRefusesAProviderWithNoContract`, `TestFake_RejectsProductionLike`, `TestLocalSigner_RefusedInProductionLikeEnvironments` |
| 29 | legal capability missing | `TestCompile_AFreshDeploymentPermitsOnlySimulation`, `TestIntegration_TheGateAndThePolicyMustBothAgree` |
| 30 | jurisdiction turns blocked mid-session | `TestIntegration_JurisdictionTurningBlockedMidSessionStopsTheNextPurchase` — same session, same cookie, next command refused |

**30 of 30.** The three that were open in the previous version of this document are now closed, and
none of them closed by writing a test around what the code already did:

- **5 — out-of-order webhook.** The property proved is not "events arrive in order", which no test
  can make true. It is that a deposit's status is a function of the furthest point the provider has
  ever reported, never of the last packet received: the confirmation is delivered first, the two
  earlier events after it, and the states the deposit never entered are left unstamped and
  untransitioned. A straggler arriving after the money is AVAILABLE posts nothing. The one
  out-of-order case that must not be a silent no-op — a rejection after a confirmation — escalates
  to REVIEW_REQUIRED, because one of those two provider statements is false and nothing here can
  tell which.
- **14 — wash trading across two accounts under one actor.** A `WASH_TRADE` alert now fires when a
  DIFFERENT account with the SAME `owner_user_id` took the opposite side of the same market inside
  the round-trip window. Shared ownership is a recorded fact and is the only form of common control
  this system can prove; the alert's detail carries `detection_basis` and, deliberately,
  `what_this_cannot_see` — coordination between accounts owned by different users is invisible here
  and the alert does not imply otherwise. It is CRITICAL and does not block, because blocking on a
  surveillance heuristic is a denial-of-service vector against creators.
- **23 — contradictory provider status.** Reaching the scenario at all exposed F-23: `Reconcile`
  returned early on a terminal payout without asking the provider anything, so an operator got a
  `nil` error that read as confirmation of a check that never happened. The provider is now asked,
  its answer is recorded as a provider event whatever it says, and a definite contradiction is
  surfaced as an error. **No state moves** — that is the control, and it is what stops whoever
  controls the provider's responses from moving Nodal's money by lying twice.

Each of the three carries a control that fails if the detector is trivial: a stranger trading the
same shape is not wash trading, and a provider that agrees with a settled payout is not a
contradiction.

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
