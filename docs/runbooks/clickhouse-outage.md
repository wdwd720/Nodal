# Runbook: ClickHouse (analytics store) outage

Severity: SEV3 by default; SEV2 if LIVE-mode strategies depend on ClickHouse-served data (PART 158) · Owner: OPERATIONS · Related: [redpanda-outage.md](./redpanda-outage.md), [stale-market-data.md](./stale-market-data.md), [model-malfunction.md](./model-malfunction.md), ADR-0006

## Trigger

- ClickHouse unreachable, TLS failure (`CLICKHOUSE_TLS` mandatory in STAGING/PROD), disk full, merge backlog; ingest consumer lag on market/decision topics.
- Analytics, backtests, performance pages and strategy decision analysis failing or stale.
- Strategy runs skipping with `MISSING_DEPENDENCY`/`STALE_DATA` for dependencies served from ClickHouse (`internal/agent`, `internal/reality`).
- BLOCKED_EXTERNAL: alarms (`infra/terraform/modules/observability`); the ingest worker and the ClickHouse client are wired (`cmd/market-ingest-worker`, `internal/reality`); the DDL is applied by `reality.ClickHouseStore.EnsureSchema` and mirrored in `docs/architecture/POINT_IN_TIME.md` Appendix A.

## Blast radius

PART 158: **live financial trading may continue if the required critical data remains available.** ClickHouse is never truth for any balance, eligibility or reservation (PART 118, SYSTEM.md §4).

- Not affected: intents, eligibility, risk (reads Postgres policies and the Postgres/Redis market snapshot), settlement compilation, quotes (from the execution provider), signing, submission, finality, reconciliation, ledger, positions, funding, gates, kill switches, audit.
- Degraded: backtests, performance snapshots, strategy analytics, telemetry datasets, the "decision analysis" views; anything the web app renders from analytics.
- Conditional: strategies whose dependencies (features, aggregates, history windows) are computed from ClickHouse cannot evaluate. By construction they skip rather than trade on stale or missing input (POINT_IN_TIME.md §4, §7), so the money-safety property holds without operator action.
- Ingestion continues to the raw archive (S3) and Redpanda; ClickHouse can be backfilled.

## Immediate actions (first 10 minutes)

1. Confirm the money path has no ClickHouse dependency in the current build: `SELECT max(posted_at) FROM journal_transactions;` advancing; risk decisions still being recorded (`SELECT max(decided_at) FROM risk_decisions;` — column per `internal/risk` store); `/readyz` does not consult ClickHouse.
2. Identify strategies at risk (`internal/agent`):
   ```sql
   SELECT a.id, a.mode, a.status FROM agents a WHERE a.status = 'ACTIVE' AND a.mode IN ('CANARY','LIMITED','LIVE');
   SELECT agent_id, skip_reason, count(*) FROM agent_runs WHERE created_at > now - interval '15 minutes' GROUP BY 1,2;
   ```
   A rising `MISSING_DEPENDENCY`/`STALE_DATA` skip count is the expected, safe outcome.
3. Decide on pausing: if any LIVE/LIMITED agent has open orders and a dependency on ClickHouse-served data for its *exit* logic, pause it with `AGENT_PAUSE(<agent_id>)` (STANDARD; `kill:activate` or `agent:pause`): `POST /admin/kill-switches {"kind":"AGENT_PAUSE","scope_id":"<agent_id>","action":"activate","reason":"<INC-id>: analytics dependency unavailable"}`. Pausing never cancels or closes positions by itself; `open_orders_policy` on `agent_pauses` governs cancellable orders only.
4. Check ClickHouse itself (managed console, BLOCKED_EXTERNAL EB-014; locally `docker compose ps clickhouse`, `make infra-logs`): disk, replication, merges, `system.errors`.
5. Announce: "analytics store outage; trading path unaffected; strategies depending on analytics are skipping/paused; backtests and performance views stale since <time>".

## Diagnosis

- Ingestion: `ingest_checkpoints` status (`ACTIVE | DISCONNECTED | REPLAYING | GAP | STOPPED`) per data source/stream and `stream_gaps` OPEN rows tell you whether the raw pipeline is still healthy (it should be; ClickHouse is downstream of the bus).
- Consumer lag on the ClickHouse ingestion consumer group vs. broker retention: if lag exceeds retention, backfill from the raw archive (`raw_archive_objects`) rather than the bus.
- Web/app errors: analytics endpoints should fail with a clear problem code, never fall back to Postgres for tick history and never render an unlabeled partial figure (PART 81: never conflate modes).
- Was anything financial reading ClickHouse? It must not (`cp_readonly` is the only path from Postgres to reporting; nothing reads the reverse). If you find such a read in the trading path, that is a design violation: open a BLOCKERS entry.

## Containment and recovery

1. Restore ClickHouse (managed incident or `docker compose up -d clickhouse`).
2. Let the ingestion consumer drain; if retention was exceeded, run the archive backfill for the missing window (PENDING: `reality.Ingestor` replay from `raw_archive_objects`), record a `stream_gaps` row of kind `REPLAY` with `ACKNOWLEDGED` resolution and the actor.
3. Label affected analytics: any backtest whose window overlaps the outage carries `impurity_reasons += DATA_GAP:<id>` and is `RESEARCH_ONLY_TEMPORALLY_IMPURE`; performance snapshots computed during the gap are recomputed after backfill.
4. Resume paused agents (`kill:release` + step-up for `AGENT_PAUSE`; or the owner's resume when paused by the owner) only after their dependency freshness checks pass in a dry run.
5. No financial repair is ever needed for a ClickHouse outage.

## What NOT to do

- Never route balance, buying-power, eligibility or reservation reads to ClickHouse, even temporarily.
- Never hand-load ClickHouse from Postgres to "catch up"; the ingestion path with checkpoints and gap records is the only way to keep point-in-time semantics honest.
- Never let a strategy evaluate on partial history to keep it "running"; `Skip{MISSING_DEPENDENCY}` is correct.
- Never present analytics computed over a gap without the impurity label.
- Never activate `GLOBAL_NEW_RISK_KILL` for an analytics outage; use `AGENT_PAUSE` per affected agent. **PENDING the bridge:** activating `AGENT_PAUSE` writes a `kill_switches` row, and the agent runtime reads `agent_pauses`. `agent.KillSwitchMirror` exists to keep the two together and is implemented by nothing, so today the switch records the decision and does not reach a running agent (F-65).

## Verification / exit criteria

- ClickHouse healthy; ingestion consumer lag at zero; `ingest_checkpoints.status = 'ACTIVE'` for every stream.
- Gap rows for the window resolved (`REPLAYED` or `UNRECOVERABLE` with rationale); affected backtests labelled.
- Agent skip counts back to baseline; paused agents resumed with evidence.
- Confirmed during the incident that no financial decision path touched ClickHouse.

## Post-incident

- Record the outage window and the list of impure analytics windows; archive the backfill report.
- If any agent was paused, archive the pause/resume audit rows (`agent_pauses`, `admin` stream).
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-118-1 and R-158-1; file the chaos test `test/chaos/clickhouse_down_trading_continues_test.go` as a BLOCKERS item if absent.
