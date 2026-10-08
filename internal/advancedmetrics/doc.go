// Package advancedmetrics records the advanced capabilities' instruments
// (observability.AdvancedMetrics, prefix advanced_): from what the database
// holds (the Collector, run by the advanced worker), from the order book's
// request path (OrderLatency, the pipeline's observer in the API), and the
// health of the advanced worker's jobs (JobHealth).
//
// # Gauges
//
// The Collector re-reads, every pass and read-only, what the rows say now:
//
//   - capital, in USD minor units: advanced_gross_assets is every SIMULATED
//     holding on the ledger -- every owner, every code the ledger classifies as
//     holding value (ledger.HoldingCodes: wallets, collateral, escrows, house
//     inventory, facility cash, collected fees) -- valued at par for a
//     registry-declared paper dollar and at the latest mark in the sandbox
//     dollar no older than the API accepts otherwise (transfers.RegistryValuer),
//     each holding rounded toward zero; advanced_customer_liabilities is the
//     customers' share of it, advanced_house_assets the house books'. A holding
//     that cannot be valued is left out of the sums and counted in
//     advanced_unvalued_holdings, never valued at zero or at a stale price.
//   - margin, through margin.Service.Aggregate, the engine's own evaluation:
//     advanced_margin_exposure, advanced_leverage (bps; not recorded while
//     aggregate equity is not positive, when it is undefined) and
//     advanced_liquidation_buffer.
//   - advanced_reserve_coverage per stable asset (attribute stable_asset),
//     through stableasset.Service.ReserveLedger, for the network the worker's
//     devnet host carries.
//   - reconciliation: advanced_unmatched_economic_effects per subsystem
//     (PENDING or UNKNOWN effects unchanged for longer than the horizon -- the
//     selection econ.EffectStore.ListUnresolved makes), and per kind the open
//     reconciliation records (advanced_open_reconciliation_records), of which
//     DEVNET_CHAIN and BRIDGE are advanced_chain_mismatches and STABLE_RESERVE
//     advanced_reserve_mismatches.
//
// # Counters, and why they are at most once per process
//
// A counter must count each event once, but the events are rows, and rows are
// what the collector reads. So it keeps a watermark in memory: a pass counts
// the rows created in (watermark, upper], where upper is the database's own
// clock minus a lag, and then moves the watermark to upper. The first pass
// after the process starts only sets the watermark -- a restarted collector
// does not re-count history, which would show every past event again as a
// burst -- and a pass that cannot read every counter records none of them and
// leaves the watermark, so its window is counted by the next pass. The lag is
// there because a row's created_at is its transaction's start: a transaction
// still open when the pass ran commits rows stamped inside the counted window,
// and the lag waits for it.
//
// Each row is therefore counted at most once by each process. Rows created
// while no collector runs (between a stop and the next start) are not counted
// at all, a transaction open for longer than the lag is missed, and two worker
// processes each count every row: run one collector per database, or
// aggregate the replicas with max rather than sum.
//
// Counted: advanced_risk_denies per reason code of a Risk Kernel V2 REJECT
// decision (one increment per code, so a decision with two reasons counts
// once under each), advanced_stale_oracle_events (REJECT decisions naming a
// stale or unavailable mark), advanced_margin_calls, advanced_liquidation_events
// (liquidation steps), advanced_sandbox_runs, advanced_sandbox_killed_runs per
// termination reason (every one but OK), advanced_sandbox_cpu_limit (TIMEOUT)
// and advanced_sandbox_memory_limit (MEMORY_LIMIT), and
// advanced_internal_match_volume -- the quote notional of internal book
// trades, valued like the holdings; trades whose quote asset cannot be valued
// are counted in advanced_unvalued_match_trades instead --
// advanced_intent_proposals per actor type (every economic intent recorded),
// advanced_denied_intents per stage (each REJECTED transition, attributed to
// the status the intent was refused from: RECEIVED is routing, ROUTED risk,
// and so on), advanced_signing_requests per verdict of the delegated signing
// provider, and advanced_withdrawal_proposals per evaluation outcome of a
// delegated withdrawal (AUTO_APPROVE or HUMAN_REQUIRED); and the execution,
// circuit-breaker, authority and network counters below. Intent rows carry
// their service's timestamp rather than the database's; the lag covers the
// skew between the two clocks.
//
// # Execution: fill and rejection rates, and the external route
//
// advanced_fill_rate_* and advanced_rejection_rate_* are counter pairs a
// dashboard divides, counted here from book_orders (attribute owner_type,
// CUSTOMER or HOUSE): both denominators are every book order recorded,
// counted when it is recorded; advanced_fill_rate_filled is every book
// order's first fill -- its first trade, as maker or taker -- counted in the
// window of that trade; advanced_rejection_rate_rejected is every book order
// that ended REJECTED, at acceptance, at a later stage or by the engine,
// counted when it was rejected (a rejected order never changes again, OB003).
// They are counted from the rows rather than on the request path because the
// request path does not see every fill -- a resting order fills inside
// another order's command or a replace, a house order is placed by the
// treasury, a submission a crash interrupted is finished by Pipeline.Resume or
// the upkeep sweep -- and a retried request would be counted twice; the rows
// count each order, each first fill and each rejection once. A request
// refused before any order was recorded (malformed, priced off the book's
// tick, sent to a book that is not open) is no order and is in neither pair;
// its latency is in advanced_order_latency with outcome REFUSED.
//
// advanced_external_route_volume is the notional the order book's paper venue
// filled (orders.PaperVenue, paper_fills). The paper venue is this build's
// stand-in for an external venue -- external simulated liquidity whose own
// side is not booked -- so what it fills is the volume that went to a venue
// other than the internal book. Nothing routes a resting or unmatched book
// order to it: it fills what is sent to it, which today is the strategy
// marketplace's paper follows. Each fill counts at its reference-mark notional
// (the SIM_TRADE's; the spread and the fees are disclosed charges beside it),
// valued as the internal match volume is; fills whose quote asset cannot be
// valued are counted in advanced_unvalued_route_fills instead. The equities
// paper broker and the treasury's house paper venue keep fills of their own
// and are not this venue.
//
// # Circuit breakers, authority violations, network violations
//
// advanced_circuit_breaker_events counts trips, attributes kind and reason: a
// stable asset paused by its breaker (kind STABLE_ASSET; the breaker writes
// exactly one tripping alert per pause, stable_peg_alerts.breaker_tripped,
// whose kind is the reason: PRICE_DEVIATION, RESERVE_SHORTFALL or
// SUPPLY_MISMATCH) and a bridge route paused by its breaker (kind
// BRIDGE_ROUTE, reason RECONCILIATION_MISMATCH: the route's ACTIVE to PAUSED
// transition by bridge.BreakerActorID). An operator's pause is not a trip.
// Breakers that are conditions rather than trips are not counted: an open
// reconciliation record that blocks new risk (the derivatives clearing
// breaker, CLEARING_UNRECONCILED, is one, as are the records the stable asset
// and the bridge name to Risk V2) is in advanced_open_reconciliation_records
// by kind, and the treasury's limits, evaluated per request, refuse as
// advanced_risk_denies with reason RISK_CIRCUIT_BREAKER.
//
// advanced_authority_violations counts the refused delegated requests whose
// refusal violated the owner's grant, once each, attributes kind (SWAP or
// WITHDRAWAL) and stage. Which refusals are violations is delegation's to
// say: delegation.AuthorityViolationCodes (scope, limits, expiry, revocation,
// the action label, a destination off an allow-list) and, for a capability
// check Risk V2 refused at admission, delegation.AuthorityViolationRiskReasons.
// They are counted from the rows delegation records for a refused request:
// stage ADMISSION is an agent intent rejected under a violation code before
// anything was planned for it, or one Risk V2 refused with an authority
// reason; stage EXECUTION is an admitted swap or withdrawal refused when it
// was checked again -- its intent rejected under a violation code by the
// check before signing (or the expiry sweep's), a signing decision that names
// one, or a signed subject stopped before its submission for one. A request
// refused before its intent exists (no such capability on the account, an
// inactive agent, a destination not on the account, no withdrawal policy)
// leaves no row and is not counted; and a withdrawal the account's policy
// refuses records only the first of the policy's codes, in sort order, as its
// intent's rejection code, so one whose first code is not a violation (its
// compliance state, its daily limit) is not counted even when a later one is.
//
// advanced_sandbox_network_violations counts the strategy versions the
// scanner refused for an import that would reach the network or another
// process. The runtime gives a module no network or process import, so such
// an attempt can only be refused at scan time: a version whose validation row
// says the scanner refused it with a forbidden import (FAILED, SCAN_REJECTED,
// IMPORT_FORBIDDEN) counts once when one of the imports refused would give
// the network or another process (sandboxrt.NetworkOrProcessAccess: net and
// everything under it, os/exec, os/signal, syscall). Which imports those were
// the collector asks the scanner itself, re-scanning the version's stored
// source -- immutable, and checked against the version's digest when stored.
//
// # Recorded on the request path: order and cancel latency
//
// advanced_order_latency and advanced_cancel_latency are not rows: a latency
// belongs to a request. OrderLatency is orders.Pipeline's observer
// (orders.WithObserver; cmd/api wires it), recording each customer Submit and
// Cancel the pipeline runs once, in the process that ran it, in milliseconds
// of monotonic time, attribute outcome (OK, REFUSED or FAILED, by the error's
// status class). A replace, a house order, a prediction market's bulk cancel
// at its close (Service.CancelResting, run by the advanced worker) and a paper
// venue fill are not timed.
//
// # Not recorded, and why
//
//   - advanced_provider_mismatches: no advanced reconciliation compares
//     against an external provider; every kind is internal or a devnet chain.
//     Nothing records it, and no dashboard widget or alarm reads it.
//
// # Job health
//
// JobHealth is an advancedjobs.Observer: after every run it records the job's
// last success (unix seconds), its consecutive failures and the run's duration
// (milliseconds), attribute job.
//
// # What this package must never do
//
//   - Write anything: every read is a read-only transaction.
//   - Value a holding it cannot price at zero, at par it was not declared to
//     have, or at a mark older than the API accepts.
//   - Count a row twice in one process, or count history again on restart.
//   - Put a high-cardinality attribute on an instrument: attributes are
//     subsystem, kind, reason, stage, actor_type, outcome, owner_type,
//     stable_asset and job, all small closed sets (stable_asset is one per
//     issued asset).
//   - Use floating point: every value is an int64 (USD minor units, basis
//     points, counts, seconds, milliseconds).
package advancedmetrics
