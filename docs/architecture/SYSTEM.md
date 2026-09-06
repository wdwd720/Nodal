# SYSTEM ARCHITECTURE

Status: design fixed 2026-09-05; implementation state per subsystem is in `docs/build/REQUIREMENTS_TRACEABILITY.md` and `docs/build/MASTER_BUILD_STATE.md`. Nothing in this document claims a subsystem is complete.

## 1. What the system is

A USD-native financial control plane. Customers and typed strategies express economic intent; the platform deterministically translates intent into safe financial operations on Solana spot markets, keeps exact multi-asset accounting, reconciles against external truth continuously, and records tamper-evident evidence for every money-affecting decision. V1 scope and exclusions are defined in the goal document (PARTS 2–4) and restated in `README.md`.

## 2. Trust boundaries and binaries

| Binary | Responsibility | Credentials it may hold | Must never hold |
|---|---|---|---|
| `cmd/api` | REST `/v1` commands and queries, SSE streams, webhooks ingestion, session auth | DB app role, Redis, event bus producer, provider read credentials, funding provider webhook secret | wallet signing credentials |
| `cmd/execution-worker` | settlement executor, transaction build/inspect/sign/submit, finality observation | DB app role, execution provider, chain observers, **bounded signing credential** (signing provider) | admin/gate mutation |
| `cmd/reconciliation-worker` | event-driven/periodic/full reconciliation, internal consistency verification | DB app role, chain observers, execution provider status/reconcile endpoints | signing credentials |
| `cmd/market-ingest-worker` | provider streams → raw archive → normalizer → Redpanda → ClickHouse; checkpoints and gap detection | data provider credentials, archive write, bus producer, ClickHouse write; DB ingest-state tables only | any financial write beyond ingest state |
| `cmd/agent-worker` | strategy evaluation, ToolBroker, model calls, prediction commits, intent creation | DB app role (agent-scoped), model provider key via ToolBroker, bus consumer | **no signing credentials, no wallet provider, no admin, no capital/risk authority** |
| `cmd/workflow-worker` | Temporal workflows/activities: funding lifecycle, reconciliation escalation, promotion, withdrawal (disabled) | DB app role, Temporal, provider clients needed by activities | signing credentials |
| `cmd/audit-worker` | hash-chain verification, Merkle checkpoints, KMS signing, WORM archive, `verify` command | DB read (audit), KMS sign, archive write | financial writes |
| `cmd/migrate` | schema migrations under the migration role | DB migrate role | application data access in production |

Each binary maps to its own ECS task role (PART 100) and DB role where distinct (PART 101).

## 3. Control path

```
EVENT / USER OBJECTIVE
 → AGENT / STRATEGY EVALUATION (agent-worker; untrusted proposal generator)
 → STRUCTURED PREDICTION (prediction ledger; must predate execution)
 → TYPED TRADE INTENT (intent; idempotency key; mode)
 → ELIGIBILITY ENGINE (eligibility; persisted decision)
 → DETERMINISTIC RISK KERNEL (risk; PRE_TRADE decision)
 → ATOMIC CAPITAL RESERVATION (capital; Postgres row locks + outbox)
 → SETTLEMENT COMPILER (settlement; immutable plan or NO_VALID_PLAN)
 → EXECUTABLE QUOTE (execution adapter; evidence archived)
 → FINAL RISK VALIDATION (risk; FINAL decision against the quote)
 → TRANSACTION CONSTRUCTION (execution adapter)
 → TRANSACTION INSPECTION (signing/inspect; pure)
 → BOUNDED SIGNING (signing service; re-validates from persisted state; then wallet SigningProvider)
 → EXTERNAL SUBMISSION (UNKNOWN_EFFECT_WRITE; timeout ⇒ SUBMISSION_UNKNOWN)
 → FINALITY / FILL OBSERVATION (Helius + fallback RPC; agreement policy)
 → RECONCILIATION (reconciliation; records; blocks new risk on mismatch)
 → LEDGER POSTING (ledger; per-asset double entry; immutable)
 → POSITION UPDATE (positions; lots)
 → AUDIT EVIDENCE (audit; per-stream hash chain → Merkle → KMS → S3 Object Lock)
```

No alternate money path exists. Manual trades enter at TYPED TRADE INTENT with `actor_type = USER`.

## 4. Data stores and their truth

| Store | Holds | Is truth for | Never truth for |
|---|---|---|---|
| PostgreSQL (RDS Multi-AZ) | all financial and control state, outbox/inbox, audit events | accounting, reservations, orders, policies, gates, sessions | market history |
| Redpanda | normalized market/chain events, internal domain events (from outbox) | transport | any balance |
| ClickHouse | market history, normalized events, decisions analytics, backtests, telemetry datasets | analytics | any balance or eligibility |
| Redis | rate limits, cache of rendered read responses (≤ 2 s), ephemeral supporting state | nothing financial | reservations, ledger, state machines |
| S3 (+ Object Lock for audit) | raw provider payloads, transaction evidence, audit archives | evidence | live state |
| Temporal | workflow orchestration state | orchestration progress | balances |

## 5. Package map (`internal/`)

`auth, identity, accounts, compliance, eligibility, funding, wallet, money, ledger, capital, assets, instruments, valuation, positions, quote, intent, settlement, execution, signing, reconciliation, risk, strategy, agent, model, prediction, reality, backtest, performance, proof, audit, event, provider, admin, config, observability, security, db, id, clock, errs, idempotency, gates, killswitch, fees, notification`.

Import rules enforced by lint (depguard): `agent/**` and `strategy/**` may not import `signing`, `wallet`, `admin`, `capital` mutation APIs, `risk` policy mutation, or `gates`; `signing` is imported only by `cmd/execution-worker`; nothing in production wiring imports `*test`/`testkit` packages; financial packages may not use floating point (`lintfin`).

## 6. Environments and capability defaults

`LOCAL, TEST, DEV, STAGING, PROD`. Fake providers are accepted only in LOCAL/TEST (and DEV where explicitly configured), and rejected programmatically in STAGING/PROD by `config.Validate`. Every production capability gate starts `DISABLED` (PART 244) and can only become ACTIVE through deployment config **and** persisted dual-approved evidence (POLICY_AUTHORITY.md §1).

## 7. Cross-cutting

- **Identity/time/money primitives**: `id` (UUIDv7 typed IDs), `clock`, `money` (exact), `errs` (problem+json codes).
- **Exactly-once economic effect**: idempotency keys on commands, inbox on events, unique external fill ids, immutable attempts, reservation locking, reconciliation.
- **Observability**: OpenTelemetry traces/metrics across HTTP, gRPC, Temporal, providers, DB, event processing; structured redacted logs with request/correlation/intent/order/workflow ids; financial, execution, and agent metric sets (PARTS 131–134).
- **Security**: OIDC-based identity provider abstraction, server-side sessions, RBAC + tenant scoping + step-up, dual control for high-risk admin actions, kill switches, secrets via references, least-privilege task and DB roles, supply-chain scanning and signed images.

## 8. Related documents

`FINANCIAL_MODEL.md`, `POLICY_AUTHORITY.md`, `SETTLEMENT_COMPILER.md`, `EXECUTION.md`, `RECONCILIATION.md`, `AGENT_RUNTIME.md`, `STRATEGY_IR.md`, `POINT_IN_TIME.md`, `CONVENTIONS.md`, `docs/adr/`, `docs/security/SECURITY.md`, `docs/threat-model/THREAT_MODEL.md`, `docs/operations/*`, `docs/compliance-gates/PRODUCTION_GATES.md`.
