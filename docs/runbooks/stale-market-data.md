# Runbook: stale market data

Severity: SEV2 (sustained stale data) · Owner: OPERATIONS (RISK for policy) · Related: [helius-outage.md](./helius-outage.md), [redpanda-outage.md](./redpanda-outage.md), [clickhouse-outage.md](./clickhouse-outage.md), [model-malfunction.md](./model-malfunction.md), `docs/architecture/POINT_IN_TIME.md` §4, §7

## Trigger

- `RISK_STALE_DATA` / `RISK_QUOTE_AGE` rejections rising (`risk_rejections` by reason); `STALE_MARKET_DATA` problem responses from valuation (`valuation.PriceStore.Latest` judges staleness on `observed_at`, never `received_at`).
- `data_freshness` histogram (age of the freshest input used for a decision) above strategy `max_age_ms`; `agent_runs.skip_reason = STALE_DATA | MISSING_DEPENDENCY` climbing (`internal/agent`).
- `ingest_checkpoints.status` in `DISCONNECTED | GAP | STOPPED`, or `last_platform_received_at` older than the source's expected cadence; `stream_gaps` rows `OPEN` of kind `SILENCE`/`GAP` (`cmd/market-ingest-worker`).
- Market-data provider health `UNHEALTHY` (role `DATA`); `asset_prices.observed_at` not advancing for an active asset.
- BLOCKED_EXTERNAL: alarms; `internal/reality`, `cmd/market-ingest-worker`. Implemented: `valuation` staleness, risk kernel `RISK_STALE_DATA`, quote freshness (`internal/quote`), tables for checkpoints/gaps/health samples.

## Blast radius

Money safety holds without operator action: **stale data means no trade** (PART 79). The risk kernel refuses intents whose market snapshot is older than policy; quotes expire; strategies skip with `STALE_DATA`/`MISSING_DEPENDENCY` and never pretend continuity (POINT_IN_TIME.md §4). Provider clocks cannot make data look fresher (freshness = `T − min(source_event_at, platform_received_at)`).

- Affected: new manual trades on affected instruments (quote freshness check fails or `RISK_QUOTE_AGE`), agent evaluations depending on the stale streams, valuation/portfolio USD figures (shown with staleness, never silently), backtests over the gap (labelled impure).
- Not affected: execution of already-approved plans whose quote is fresh (the quote comes from the execution provider, not the market-data feed), finality observation, reconciliation, ledger, funding. Position quantities are exact regardless of prices; only USD valuation is stale.
- Risk-reducing exits for open positions are still possible when a fresh quote is obtainable; if the execution provider's quotes are also stale, see [jupiter-outage.md](./jupiter-outage.md).

## Immediate actions (first 10 minutes)

1. Confirm the safety property is holding, not just assumed (read-only):
   ```sql
   SELECT asset_id, source, max(observed_at) AS last_observed, max(received_at) AS last_received
     FROM asset_prices GROUP BY asset_id, source ORDER BY last_observed;
   SELECT data_source_id, stream, consumer, status, last_source_event_at, last_platform_received_at, disconnected_at
     FROM ingest_checkpoints ORDER BY last_platform_received_at NULLS FIRST LIMIT 20;
   SELECT kind, resolution, count(*), min(gap_start_at) FROM stream_gaps WHERE resolution = 'OPEN' GROUP BY 1,2;
   ```
   Then check that intents are being *rejected* for the affected instruments (`risk_decisions` reason codes contain `RISK_STALE_DATA`/`RISK_QUOTE_AGE`) rather than accepted. If an intent was accepted on stale data, that is a SEV1 kernel defect: activate `GLOBAL_NEW_RISK_KILL` ([global-kill-and-reenable.md](./global-kill-and-reenable.md)).
2. If only some instruments are affected and you want the state to be explicit for customers: `POST /admin/instruments/{instrumentId}/status {"to":"CLOSE_ONLY","reason":"<INC-id>: market data stale","policy_version":"<v>"}` (`instrument:status_write`; OPERATIONS/RISK/ADMIN) or `INSTRUMENT_CLOSE_ONLY(<instrument_id>)` (STANDARD switch). Close-only preserves `REDUCE_RISK`.
3. Identify the layer: provider (health samples role `DATA`), stream/bus ([redpanda-outage.md](./redpanda-outage.md)), ingest worker (checkpoints not advancing while the provider is healthy), or normaliser (raw archive advancing but `asset_prices` not).
4. Do not touch agents: they skip on their own; pause (`AGENT_PAUSE`) only an agent whose *exit* logic needs the stale stream and that has open positions you want held rather than evaluated.
5. Announce affected instruments/streams, since when, and that trading on them is refused by policy.

## Diagnosis

- Provider side: rate limit or plan gating (`429`), stream disconnects (`RECONNECT` gaps), a host move (`docs/api/providers/README.md` "hostname drift"); compare `last_source_event_at` vs `last_platform_received_at` to separate "provider silent" from "we stopped receiving".
- Bus side: unpublished market events are not outboxed (they are transport-only); consumer lag on market topics vs broker retention decides whether replay is possible.
- Ingest side: worker crash loops, checkpoint `version` conflicts (two consumers on one checkpoint), archive write failures (raw archive precedes normalisation; if the archive fails, nothing downstream advances by design).
- Policy side: `max_age_ms` per dependency is the persisted IR value (prompt defaults are suggestions); a global staleness spike right after a policy change points to the policy, not the feed.
- Clock skew: a provider clock running behind only makes data older (conservative); one running ahead is ignored for freshness. Never "fix" staleness by trusting provider timestamps.

## Containment and recovery

1. Restore the failing layer (provider ticket, bus, worker restart). Checkpoints resume from `last_sequence`/`last_source_offset`; `RECONNECT` gap rows are recorded automatically.
2. Replay the gap when `data_sources.supports_replay` (from the provider or the raw archive; `internal/reality` replay): the gap becomes `REPLAYED`; otherwise mark `UNRECOVERABLE` with rationale (actor recorded; `stream_gaps.resolved_by_*`).
3. Strategies resume automatically when the freshness check passes; backtests overlapping the gap carry `DATA_GAP:<id>` impurity forever.
4. Return instruments to `ACTIVE` (`instrument:status_write`) or release `INSTRUMENT_CLOSE_ONLY` (`kill:release` + step-up) once prices are fresh for a full policy window.
5. No financial repair: nothing traded on stale data, by construction. If something did (step 1 finding), the kernel defect is a release blocker and the affected orders are reviewed under [ledger-mismatch.md](./ledger-mismatch.md) for customer impact.

## What NOT to do

- Never widen `max_age_ms`, quote freshness, or `RISK_QUOTE_AGE` limits to let trading continue on old data.
- Never backfill `asset_prices` from ClickHouse or a spreadsheet; prices enter through the ingest path with provenance (`raw_ref`, `source`).
- Never mark a gap `REPLAYED` without an actual replay; never delete gap rows.
- Never let a strategy evaluate with "last known" values; `Skip{STALE_DATA}` is correct.
- Never disable the valuation staleness check to make portfolio pages render numbers.

## Verification / exit criteria

- `asset_prices.observed_at` advancing within cadence for every active asset; `ingest_checkpoints.status = 'ACTIVE'`; no `OPEN` gaps for the affected streams.
- `RISK_STALE_DATA`/`RISK_QUOTE_AGE` rejections back to baseline; `data_freshness` within policy.
- Instruments restored to `ACTIVE` with transition rows; switches released through their paths.
- Confirmed: zero intents accepted with a snapshot older than policy during the window.

## Post-incident

- Archive checkpoint/gap history, provider incident, replay report; label affected backtest and performance windows.
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-077-1, R-174-1, R-200-1; add the gap-detection table test and `test/chaos/market_data_stale_test.go` to BLOCKERS if absent.
