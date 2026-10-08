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
// are counted in advanced_unvalued_match_trades instead.
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
//     subsystem, kind, reason, stable_asset and job, all small closed sets.
//   - Use floating point: every value is an int64 (USD minor units, basis
//     points, counts, seconds, milliseconds).
package advancedmetrics
