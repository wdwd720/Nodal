# POINT-IN-TIME REALITY, BACKTESTING AND PERFORMANCE

Status: design fixed 2026-09-05; implementation tracked in `docs/build/REQUIREMENTS_TRACEABILITY.md`. Contract for Stages 11–12. Covers PARTS 74–86, 118, 120, 122, 124, 127, 159, 174, 175, 199, 200, 224–226.

Every datum the platform holds carries knowledge-time semantics. A decision — live or simulated — may only see what was knowable at the moment it was taken. This is the property that makes a backtest honest and a live agent auditable; the same evaluator, the same risk semantics and the same instrument definitions serve both (PART 225).

## 1. Knowledge-time semantics (PART 74, 175)

| Timestamp | Set by | Trust | Meaning |
|---|---|---|---|
| `source_event_at` | provider / chain | untrusted | when the source says the event happened (block time, post time, trade time) |
| `provider_published_at` | provider | untrusted, nullable | when the provider says it emitted the event |
| `platform_received_at` | our ingest process via `clock.Clock` | trusted | first byte received by us |
| `normalized_at` | normalizer | trusted | canonical event produced |
| `feature_available_at` | feature/tool layer | trusted | derived feature (window, score, model output) became computable |
| `decision_available_at` | reality engine | trusted | earliest instant a decision process could have used the datum: `max(platform_received_at, normalized_at, feature_available_at) + tools.pipeline_latency_ms` |

Rules: a decision at time `T` may use an observation iff `decision_available_at ≤ T` (live: `T` = run decision time; backtest: `T` = simulated time). Freshness is `age = T − min(source_event_at, platform_received_at)`, so a provider clock running ahead can never make data look fresher, and a provider clock running behind only makes it look older (conservative). Provider timestamps are always stored next to ours and never drive knowledge-time logic.

## 2. Raw archive (PART 75, 124)

Every raw external payload — webhook body, stream message, RPC response, social page, model prompt and response — is written to object storage before it is interpreted, and `raw_archive_objects` indexes it: `object_uri`, `object_hash` (sha256), `provider`, `event_type`, `source_event_id`, `dedup_key`, `schema_version`, `ingested_at`, the timestamps known at ingest, `size_bytes`, `content_type`, `retention_class`, `retention_until`, `partition_key`, stream position. Layout: `raw/<provider>/<event_type>/v<schema_version>/YYYY/MM/DD/HH/<platform_received_at_ns>-<dedup_key_prefix>.<ext>`; object metadata (`provider`, `event-type`, `source-event-id`, `schema-version`, `platform-received-at`, `sha256`) allows provenance reconstruction from the bucket alone. Buckets use Object Lock and KMS per retention class. Nothing is transformed away: normalized events carry `raw_object_id` and `raw_object_hash`, and `reality.Archive.Verify` re-hashes objects on a schedule.

## 3. Normalized events and the truth boundary (PART 76, 118)

| Store | Holds | Never holds |
|---|---|---|
| Postgres | financial command truth (ledger, reservations, envelopes, intents, orders, fills, predictions, runs), archive index, checkpoints, gaps, data sources, health samples, backtest manifests and results, performance snapshots, cost rows | tick-level history |
| ClickHouse | normalized events, market history, strategy decision traces, backtest trade logs, analytics (Appendix A) | balances, reservations, anything a money decision reads as authoritative |
| Object storage | raw payloads, prompts/responses, dataset manifests, evidence | — |
| Redpanda | transport of normalized events, replayable within retention | truth |

No authority path (risk, buying power, settlement, reconciliation, ledger) queries ClickHouse. In live mode `reality.Snapshotter` serves the evaluator from the stream-fed feature cache with ClickHouse for windows; whatever it returns is agent input, and the agent's output is then judged by Postgres-backed authority. In backtest mode the snapshotter reads ClickHouse only, under the same `decision_available_at ≤ T` predicate.

## 4. Event stream (PART 77, 199, 200)

`reality.NormalizedEvent` envelope: `event_id` (UUIDv7), `dedup_id`, `schema_version`, `source` (data source code), `event_type`, `sequence` (nullable; provider sequence where supported — `slot × 2^20 + tx_index` for chain events), `source_partition`, `source_offset`, the six timestamps, entity references (`instrument_id`, `asset_id`, `wallet`), `raw_object_id`, `raw_object_hash`, `payload` (canonical JSON of typed fields).

Dedup (PART 199): `dedup_id` = provider event id when `data_sources.dedup_strategy = PROVIDER_ID`, else `sha256(source ‖ event_type ‖ canonical(identifying fields))` defined per source in its normalizer (`COMPOSITE_HASH`). Redpanda key = `dedup_id`; ClickHouse `ReplacingMergeTree` keyed `(source, event_type, dedup_id)`; Postgres consumers go through `event.Inbox`. The raw object stays archived whether or not the event is a duplicate.

Detection (PART 77, 200): `ingest_checkpoints` per `(data_source, stream, partition, consumer)` holds `last_sequence`, `last_source_offset`, `last_source_event_at`, `last_platform_received_at`, `last_raw_object_id`, `status`. Detectors write `stream_gaps` rows:

| Kind | Condition | Resolution path |
|---|---|---|
| `GAP` | observed sequence > expected + 1 | `REPLAYED` when `data_sources.supports_replay` and the range was backfilled; else `UNRECOVERABLE` |
| `SILENCE` | no event for `heartbeat_timeout_ms` while connected | closes on next event; window recorded |
| `RECONNECT` | connection lost and re-established | always recorded; a following GAP row references it |
| `REPLAY` | checkpoint deliberately moved backwards for recovery | `ACKNOWLEDGED` with actor |
| `ORDERING_ANOMALY` | sequence < `last_sequence` without a REPLAY marker, or `source_event_at` decreasing beyond tolerance | `ACKNOWLEDGED`; affected events kept, flagged |

A strategy whose dependency window overlaps an `OPEN` or `UNRECOVERABLE` gap gets `Skip{MISSING_DEPENDENCY}` — no pretend continuity. A backtest window overlapping such a gap is labelled `impurity_reasons += DATA_GAP:<id>`.

## 5. Solana data provider and provider health (PART 78, 79)

Helius is one implementation of `execution/observe.SolanaDataProvider` (EXECUTION.md §6): wallet/program event streaming plus transaction observation. `reality` wires a primary `SolanaDataProvider` and an independent fallback `ChainObserver` operated by a different vendor; chain events may be ingested from either and carry `source` accordingly. Neither observation alone finalizes anything: `FINALIZED` requires agreement per the `AgreementPolicy`, disagreement blocks dependent activity. Health is sampled into `provider_health_samples` (`HEALTHY/DEGRADED/UNHEALTHY/DISABLED`, error rate bps, p50/p99 latency, staleness, window, sorted reason codes, evaluator version) per provider and role (`DATA`, `OBSERVATION`, `EXECUTION`, `FUNDING`, `WALLET`, `MODEL`). Consumers: risk (`RISK_PROVIDER_HEALTH`), settlement (`PROVIDER_DEGRADED`), the snapshotter (observations from an `UNHEALTHY` source are unusable ⇒ stale-data skip), execution (no new submissions on unhealthy execution provider; no finalization on unhealthy observation provider).

## 6. Data sources: licensing and retention (PART 120, 122)

`data_sources` is the registry every tool and archive object references: `provider`, `kind`, `retention_class` (`FINANCIAL_RECORD`, `SECURITY_AUDIT`, `RAW_MARKET_DATA`, `SOCIAL_DATA`, `MODEL_IO`, `OPERATIONAL_LOG`), `retention_days` (configured per source; never a hardcoded forever; PROD `config.Validate` refuses values below `Retention.MinDays[class]` for the two audit classes, so critical audit retention can never be zero), `redistribution_policy`, `historical_use_permitted` (`YES/NO/UNKNOWN`), `persistence_capability` (`ALLOWED/BLOCKED`; the DB CHECK forces `BLOCKED` unless permission is `YES`), allowed `environments`, `license_ref`, `contract_ref`, `dedup_strategy`, `heartbeat_timeout_ms`, replay/sequence support, `status`. A `BLOCKED` source may be read live where licensed, but only the operational window is retained, tools over it are refused for `BACKTEST` dependencies, and its agents cannot pass the `→ BACKTEST_ELIGIBLE` transition. `SOCIAL_DATA` persistence additionally requires the `SOCIAL_DATA_PERSISTENCE` capability gate.

## 7. Staleness declarations (PART 174)

Each `strategy_dependencies` row carries the IR's `max_age_ms`; the evaluator decides freshness from the snapshot timestamps and the run records per dependency `{available_at, age_ms, max_age_ms, ok}` in `agent_runs.evidence`. A violation is `skip_reason = STALE_DATA`; if an intent somehow reaches the kernel the market snapshot age triggers `RISK_STALE_DATA` again. Prompt-template defaults (price 500 ms, wallet event 2 s, social per policy) are suggestions; the persisted IR value is what binds.

## 8. Backtesting

**Manifest (PART 80).** A `backtests` row is the reproducibility manifest: `strategy_version_id`, `ir_hash`, `ir_schema_version`, `simulator_version`, `evaluator_version`, `dataset_manifest` (`[{source, stream, range, clickhouse_parts_hash, raw_object_count}]`) and its hash, `data_cutoff_at`, window, `model_mode` (`NONE`, `REPLAY_RECORDED`, `FROZEN_SNAPSHOT`, `LIVE_MODEL_TEMPORALLY_IMPURE`), `risk_policy_version` + hash, fee/slippage/latency model versions and parameters, `random_seed` (always stored, PART 224), `run_at`, `finished_at`, `manifest_hash`, `result_hash`. Manifest columns are frozen by trigger once `run_at` is set. Same manifest ⇒ byte-equal `result_hash` (test).

**Labels (PART 81, 159).** `mode ∈ {BACKTEST, PAPER, SHADOW, CANARY, LIMITED, LIVE}` is a NOT NULL, guard-protected column on runs, predictions, intents, orders, tool invocations, performance snapshots and cost rows. PAPER = live data with simulated execution; SHADOW = live data through the real pipeline up to `INSPECT_TRANSACTION` in dry-run, never signed. No query, API response or UI element aggregates across modes: `mode` is part of the `performance_snapshots` unique key and a required field of every metric object in the OpenAPI schema.

**Point-in-time validity (PART 82).** `pit_validity = POINT_IN_TIME_VALID` iff the leakage test passed, `model_mode ∈ {NONE, REPLAY_RECORDED}`, no `UNRECOVERABLE` gap in the window, and every tool version used equals the version in force during the window. Otherwise `RESEARCH_ONLY_TEMPORALLY_IMPURE` with sorted `impurity_reasons` (`LIVE_MODEL_ON_HISTORICAL_TEXT`, `TOOL_VERSION_DRIFT`, `DATA_GAP:<id>`, `FEATURE_REDEFINED`). A current model reading old text is impure by construction. Impure reports carry the label in every rendering, are never described as historical performance, and are not accepted as promotion evidence (DB CHECK ties the label to the conditions).

**Execution simulation (PART 83).** `backtest.ExecutionSimulator` replaces only the execution adapter; planner, risk kernel, evaluator and instrument definitions are the production packages. Modelled where data permits: fees (fee policy version), quote age, spread, liquidity from recorded quotes/pool state, size → price impact, slippage model, network latency model, seeded failed-submission probability, priority fee, transaction expiry (blockhash validity), partial fills (not for atomic swaps), asset lifecycle (halts/delists at their historical times). Every assumption is a labelled entry `backtest_results.assumptions[] = {name, model_version, source: RECORDED | MODELLED | UNAVAILABLE}`; unavailable precision is stated, never invented.

**Counterfactuals (PART 84).** Branches `NO_TRADE`, `ENTRY_PLUS_5S`, `ENTRY_PLUS_30S`, `HALF_SIZE`, `DOUBLE_SIZE`, `ALTERNATIVE_ROUTE` are computed by the same simulator from the same snapshots and stored in `counterfactuals` with `is_estimate = true` (CHECK), the estimate basis and assumptions. They never enter realized performance and are never rendered beside realized figures without the label.

**Leakage test (PART 226).** `backtest.AssertNoLeakage(trace)`: for every decision and every observation it consumed, require `observation.decision_available_at ≤ decision.simulated_at`, else fail naming the pair. It runs at the end of every backtest (`leakage_test_passed`) and as a unit test against a deliberately leaky fake snapshotter that must be caught; a property test shifts every `decision_available_at` by +ε and asserts the snapshot at `T` drops those observations.

## 9. Performance engine (PART 86) and cost accounting (PART 127)

`performance.Computer.Snapshot` is pure over persisted rows (fills, lot dispositions, valuations at `as_of`, predictions, outcomes, cost rows) and writes one `performance_snapshots` row per `(scope, mode, window, as_of)`: realized and unrealized return (bps) and P&L (USD minor), max and current drawdown (bps), Sharpe and Sortino (scale-6 decimals with `sharpe_meaningful = n_observations ≥ config.Performance.MinObservations`; the API returns null when not meaningful), turnover, hit rate, trade and prediction counts, average slippage vs quote, execution quality vs decision-time mid, calibration means (from `calibration_snapshots`, never blended with return), strategy age, live capital history `[{at, allocation, deployed}]`, model/data/compute cost, execution and network fees, and `net_economic_alpha = realized_pnl − Σ costs`. Mode is always stated.

`cost_accounting` rows are written at the source of every cost (tool invocation, model call, fill, backtest compute) with `UNIQUE (kind, reference_type, reference_id)` so nothing is double counted, attributed to strategy version, agent, account and mode (PART 127). Compute cost is the metered simulator/runtime time at the configured rate.

## 10. Go contracts (fixed)

```go
// internal/reality
type Timestamps struct { SourceEventAt, ProviderPublishedAt, PlatformReceivedAt, NormalizedAt, FeatureAvailableAt, DecisionAvailableAt time.Time }
type RawObject struct { DataSource, Provider, EventType, SourceEventID, DedupKey string; SchemaVersion int; ContentType string; Body []byte; Timestamps Timestamps; RetentionClass string; Stream, Partition, Offset string; Sequence *uint64 }
type ArchiveRef struct { ObjectID, URI string; Hash []byte }
type Archive interface { Put(ctx context.Context, tx pgx.Tx, o RawObject) (ArchiveRef, error); Get(ctx, ref ArchiveRef) ([]byte, RawObjectMeta, error); Verify(ctx, ref ArchiveRef) error }
type NormalizedEvent struct { EventID, DedupID string; SchemaVersion int; Source, EventType string; Sequence *uint64; SourcePartition, SourceOffset string; Timestamps Timestamps; InstrumentID, AssetID, Wallet string; RawObjectID string; RawObjectHash []byte; Payload json.RawMessage }
type Normalizer interface { Source() string; Normalize(meta RawObjectMeta, body []byte, now time.Time) ([]NormalizedEvent, error) }   // pure given now
type Ingestor interface { Run(ctx context.Context) error }   // archive → normalize → publish → checkpoint; at-least-once; dedup by DedupID
type CheckpointStore interface { Load(ctx, q db.Querier, source, stream, partition, consumer string) (Checkpoint, error); Advance(ctx, tx pgx.Tx, c Checkpoint) error; RecordGap(ctx, tx, g Gap) (GapID, error); Resolve(ctx, tx, id GapID, resolution string, actor security.Principal) error; OpenGaps(ctx, q, source string, w Window) ([]Gap, error) }
type Snapshotter interface { SnapshotAt(ctx context.Context, deps []ir.Dependency, at time.Time, mode agent.Mode) (strategy.DataSnapshot, error) }   // invariant: every Observation.DecisionAvailableAt <= at
type HealthEvaluator interface { Sample(ctx, provider, role string, now time.Time) (HealthSample, error) }
type HealthReader interface { Current(ctx, q db.Querier, provider, role string, now time.Time) (HealthState, error) }
type DataSourceStore interface { Get(ctx, q, code string) (DataSource, error); List(ctx, q) ([]DataSource, error) }
// SolanaDataProvider and ChainObserver are the EXECUTION.md §6 interfaces; reality takes Primary SolanaDataProvider + Fallback ChainObserver.

// internal/backtest
type Manifest struct { StrategyVersionID string; IRHash []byte; IRSchemaVersion int; SimulatorVersion, EvaluatorVersion string; Dataset DatasetManifest; DataCutoffAt, WindowStart, WindowEnd time.Time; ModelMode string; RiskPolicyVersion string; RiskPolicyHash []byte; FeeModel, SlippageModel, LatencyModel ModelRef; RandomSeed int64 }
func (m Manifest) Hash() ([]byte, error)
type Runner interface { Run(ctx context.Context, id BacktestID) (Result, error) }   // drives strategy.Evaluate at simulated times; trace → ClickHouse; result → Postgres; resumable per simulated step
type ExecutionSimulator interface { Simulate(ctx context.Context, req SimRequest, snap strategy.DataSnapshot, a Assumptions, rng *SeededSource) (SimFill, error) }
type Assumptions struct { FeeModel, SlippageModel, LatencyModel ModelRef; Labels []AssumptionLabel }
type DecisionTrace struct { SimulatedAt time.Time; Observations []strategy.Observation; Output strategy.EvalOutput }
func AssertNoLeakage(trace []DecisionTrace) error
type Counterfactuals interface { Branch(ctx context.Context, subject Subject, branch string) (Estimate, error) }

// internal/performance
type Scope struct { Kind string /* STRATEGY_VERSION | AGENT | ACCOUNT | ENVELOPE */; ID string }
type Window struct { Start, End time.Time }
type Computer interface { Snapshot(ctx context.Context, q db.Querier, s Scope, mode agent.Mode, w Window, asOf time.Time) (Snapshot, error) }   // pure over persisted rows; no floats
type Store interface { Save(ctx, tx pgx.Tx, s Snapshot) (SnapshotID, error); Latest(ctx, q, s Scope, mode agent.Mode) (Snapshot, error) }
type CostLedger interface { Record(ctx context.Context, tx pgx.Tx, c Cost) error }   // idempotent on (kind, reference_type, reference_id)
type Cost struct { Kind string; StrategyVersionID, AgentID, AccountID, BacktestID *string; Mode agent.Mode; ReferenceType, ReferenceID string; Amount money.USD; Provider string; OccurredAt time.Time }
func Sharpe(returns []ir.Decimal, riskFree ir.Decimal, minN int) (value ir.Decimal, meaningful bool)
func Drawdown(equity []ir.Decimal) (maxBPS, currentBPS money.BPS)
```

## 11. Tests required before Stage 11–12 exit

- Timestamp property: for any ingested object, `platform_received_at ≤ normalized_at ≤ feature_available_at ≤ decision_available_at`, and `SnapshotAt(T)` never returns an observation with `decision_available_at > T` (rapid, both modes).
- Leakage: unit (leaky fake caught), property (+ε shift), and the end-of-run assertion on every backtest fixture.
- Dedup: replaying a Redpanda partition produces no duplicate normalized rows and no second agent run.
- Gap detection table test for every kind in §4; a strategy over a gapped window skips with `MISSING_DEPENDENCY`; a backtest over it is labelled impure.
- Reproducibility: two runs of the same manifest produce identical `result_hash`; changing `random_seed` alone changes only seeded assumptions.
- Mode integrity: cross-mode aggregation queries do not exist (SQL lint over `internal/performance` forbids `GROUP BY` without `mode`); guard trigger rejects mode changes.
- Licensing: a source with `historical_use_permitted = UNKNOWN` cannot be inserted with `persistence_capability = ALLOWED`; a tool over it is refused for BACKTEST dependencies.
- Archive verification: corrupting one object makes `Verify` fail and raise `archive_integrity_violation`.

## Appendix A — ClickHouse DDL (non-authoritative analytics store)

```sql
CREATE TABLE normalized_events (
    event_id UUID, dedup_id String, schema_version UInt16, source LowCardinality(String), event_type LowCardinality(String),
    sequence Nullable(UInt64), source_partition String, source_offset String,
    source_event_at DateTime64(9, 'UTC'), provider_published_at Nullable(DateTime64(9, 'UTC')),
    platform_received_at DateTime64(9, 'UTC'), normalized_at DateTime64(9, 'UTC'),
    feature_available_at DateTime64(9, 'UTC'), decision_available_at DateTime64(9, 'UTC'),
    instrument_id Nullable(UUID), asset_id Nullable(UUID), wallet String,
    raw_object_id UUID, raw_object_hash FixedString(32), payload String   -- canonical JSON; numbers as strings
) ENGINE = ReplacingMergeTree(platform_received_at)
PARTITION BY toYYYYMM(decision_available_at) ORDER BY (source, event_type, dedup_id) TTL toDateTime(platform_received_at) + INTERVAL 730 DAY;

CREATE TABLE market_prices (
    instrument_id UUID, source LowCardinality(String), price_mantissa Int128, price_scale UInt8,
    quote_asset_id UUID, liquidity_mantissa Nullable(Int128), liquidity_scale UInt8,
    source_event_at DateTime64(9, 'UTC'), platform_received_at DateTime64(9, 'UTC'), decision_available_at DateTime64(9, 'UTC'),
    raw_object_id UUID, dedup_id String
) ENGINE = ReplacingMergeTree(platform_received_at)
PARTITION BY toYYYYMM(decision_available_at) ORDER BY (instrument_id, source, decision_available_at, dedup_id);

CREATE TABLE strategy_decisions (
    run_id UUID, backtest_id Nullable(UUID), agent_id Nullable(UUID), strategy_version_id UUID, mode LowCardinality(String),
    decision_at DateTime64(9, 'UTC'), trigger_name String, information_set_hash FixedString(32), trace_hash FixedString(32),
    observations String, signals String, conditions String, actions String, skips String   -- canonical JSON arrays
) ENGINE = MergeTree PARTITION BY toYYYYMM(decision_at) ORDER BY (strategy_version_id, mode, decision_at, run_id);

CREATE TABLE backtest_trades (
    backtest_id UUID, decision_ref String, simulated_at DateTime64(9, 'UTC'), instrument_id UUID, side LowCardinality(String),
    input_quantity Int128, output_quantity Int128, fee_quantity Int128, slippage_bps Int32, price_impact_bps Int32,
    latency_ms UInt32, submission_failed UInt8, assumptions String, counterfactual_branch LowCardinality(String)
) ENGINE = MergeTree PARTITION BY toYYYYMM(simulated_at) ORDER BY (backtest_id, simulated_at, decision_ref);
```

ClickHouse rows are derived from archived raw objects and Postgres records; they can always be rebuilt and are never the source of a money decision.
