// Package advancedmetrics records the advanced capabilities' instruments
// (observability.AdvancedMetrics, prefix advanced_) from what the database
// holds, and the health of the advanced worker's jobs.
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
// delegated withdrawal (AUTO_APPROVE or HUMAN_REQUIRED). Intent rows carry
// their service's timestamp rather than the database's; the lag covers the
// skew between the two clocks.
//
// # Not recorded here, and why
//
//   - advanced_order_latency and advanced_cancel_latency: a latency is a
//     property of a request, not of a row; the request path records it or
//     nobody can.
//   - advanced_fill_rate_*, advanced_rejection_rate_* and
//     advanced_external_route_volume: derivable from book_orders and the paper
//     fills, but no external route exists in this build and the rate pairs
//     want the order path's own denominators; left to the execution path.
//   - advanced_circuit_breaker_events: no subsystem records a circuit breaker
//     trip as a row.
//   - advanced_authority_violations: delegation marks no refusal as one. A
//     capability refusal is a REJECTED agent intent with one of the package's
//     DELEGATION_* codes, which mix authority (scope, limits, expiry) with
//     timing (a lapsed quote, a cooling period), and a request refused before
//     its intent exists leaves no row; which codes count is the delegation
//     package's to define, as an exported set.
//   - advanced_sandbox_network_violations: the WASM sandbox gives a strategy
//     no network import at all, so there is nothing to violate and no code
//     that records it.
//   - advanced_provider_mismatches: no advanced reconciliation compares
//     against an external provider; every kind is internal or a devnet chain.
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
//     subsystem, kind, reason, stage, actor_type, outcome, stable_asset and
//     job, all small closed sets (stable_asset is one per issued asset).
//   - Use floating point: every value is an int64 (USD minor units, basis
//     points, counts, seconds, milliseconds).
package advancedmetrics
