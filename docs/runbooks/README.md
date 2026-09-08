# Incident runbooks

Status: written 2026-09-05 against the working tree of the same date (goal PART 157; alert priorities PART 135; degradation rules PART 158). Every runbook names the exact control an operator uses. Where that control does not exist in this repository yet, the runbook says **PENDING: <what>** instead of describing it as available. Nothing here claims a subsystem is complete; the traceability rows are R-157-1 and R-157-2 in `docs/build/REQUIREMENTS_TRACEABILITY.md`.

## Index

| Runbook | Trigger / alert | Severity (PART 135) | First action | Owner role |
|---|---|---|---|---|
| [wallet-provider-compromise.md](./wallet-provider-compromise.md) | `signing_rejection`/`wallet_policy_violation` burst, unknown transaction signed by a platform wallet, provider credential change, provider breach notice | SEV1 (signing credential compromise / unauthorized signing) | `GLOBAL_NEW_RISK_KILL` + `PROVIDER_DISABLE_NEW_ACTIONS(<wallet provider>)`; revoke delegations | SECURITY |
| [funding-provider-compromise.md](./funding-provider-compromise.md) | `webhook_signature_failed` burst, deposit credited without chain receipt, provider breach notice or outage | SEV1 when a credit path is affected; SEV2 for outage | `FUNDING_DISABLE(*)`; rotate webhook secret | SECURITY / OPERATIONS |
| [helius-outage.md](./helius-outage.md) | chain observer health `UNHEALTHY`, stream `DISCONNECTED`, `data_freshness` breach | SEV2 (provider outage) | confirm fallback RPC healthy; `CHAIN_DISABLE_NEW_ACTIONS(solana)` only if both observers are down | OPERATIONS |
| [rpc-disagreement.md](./rpc-disagreement.md) | `chain` resolution `DISAGREED`, `reconciliation_mismatches{kind=EXECUTION}`, orders in `RECONCILIATION_REQUIRED` | SEV2 (chain-data disagreement); SEV1 if material and unexplained | nothing optimistic; let `blocks_new_risk` hold; disable a systematically wrong observer | OPERATIONS / RISK |
| [jupiter-outage.md](./jupiter-outage.md) | execution provider `UNHEALTHY`, `execution_failure_rate` breach, `quote_latency`/`submit_latency` p95 breach | SEV2 (provider outage) | `PROVIDER_DISABLE_NEW_ACTIONS(jupiter)` | OPERATIONS |
| [duplicated-trade-suspicion.md](./duplicated-trade-suspicion.md) | two landed attempts or two fills for one order, `provider_duplicate_events` + fill anomaly, customer report | SEV1 (duplicate economic execution) | `ACCOUNT_FREEZE(account)`; `GLOBAL_NEW_RISK_KILL` if the executor is suspected | OPERATIONS / FINANCE |
| [unknown-transaction.md](./unknown-transaction.md) | reconciliation record `kind=SUBMISSION_UNKNOWN` from wallet activity, `unknown_submissions` | SEV1 (unauthorized signing candidate) | `ACCOUNT_FREEZE(account)`; escalate to wallet-provider-compromise if outbound | SECURITY / OPERATIONS |
| [ledger-mismatch.md](./ledger-mismatch.md) | `ledger_integrity_violation` (`LEDGER_INTERNAL`), `oldest_unresolved_mismatch` > policy, `ledger_posting_errors` | SEV1 (ledger integrity violation / global drift) | `GLOBAL_NEW_RISK_KILL` for `LEDGER_INTERNAL`; `ACCOUNT_FREEZE` for one account | FINANCE |
| [chargeback-reversal.md](./chargeback-reversal.md) | `funding_reversed` (HIGH), `negative_deficit_accounts` (CRITICAL) security events | SEV2; SEV1 if reversals cluster or deficit total exceeds policy | verify the automatic freeze and the two deficit postings landed | COMPLIANCE / FINANCE |
| [model-malfunction.md](./model-malfunction.md) | `policy_violations`, `agent_pauses{MODEL_UNAVAILABLE}`, `model_cost` spike, schema-violation rate | SEV2 when CANARY/LIMITED/LIVE agents affected; else SEV3 | `MODEL_DISABLE(model_id|*)` (deterministic/manual product keeps running) | RISK / OPERATIONS |
| [stale-market-data.md](./stale-market-data.md) | `RISK_STALE_DATA` rejections, `data_freshness` breach, `stream_gaps` OPEN, `STALE_MARKET_DATA` errors | SEV2 (sustained stale data) | verify intents are being refused; `INSTRUMENT_CLOSE_ONLY` to make it explicit | OPERATIONS |
| [secret-exposure.md](./secret-exposure.md) | gitleaks finding, leaked credential report, `provider_credential_change` | SEV1 for signing/DB/KMS/OIDC secrets; SEV2 otherwise | kill the affected scope; rotate through the `SecretRef` indirection | SECURITY |
| [admin-compromise.md](./admin-compromise.md) | `admin_privilege_use`/break-glass outside a ticket, unexpected gate or kill transition, `cross_tenant_attempt` | SEV1 (cross-tenant financial access / unauthorized authority) | `Manager.RevokeAllForSubject`; `GLOBAL_NEW_RISK_KILL` if any authority table was touched | SECURITY |
| [database-corruption.md](./database-corruption.md) | `migrate verify` mismatch, unexplained `LEDGER_INTERNAL` drift, Postgres errors, PITR need | SEV1 | `GLOBAL_NEW_RISK_KILL` + `FUNDING_DISABLE`; no new financial commands (PART 158) | OPERATIONS / FINANCE |
| [redpanda-outage.md](./redpanda-outage.md) | outbox age / relay lag, consumer lag, broker unreachable | SEV2 | none for money safety (outbox keeps truth); watch `outbox_events` age | OPERATIONS |
| [temporal-outage.md](./temporal-outage.md) | task-queue backlog, Temporal unreachable, deposits stuck | SEV2 | none for trading; funding lifecycle pauses; never run activities by hand | OPERATIONS |
| [clickhouse-outage.md](./clickhouse-outage.md) | ClickHouse unreachable, ingest consumer lag | SEV3 (SEV2 if LIVE strategies depend on it) | confirm the trading path has no ClickHouse dependency; `AGENT_PAUSE` affected agents | OPERATIONS |
| [global-kill-and-reenable.md](./global-kill-and-reenable.md) | invoked by SEV1 runbooks; monthly staging drill (PART 165) | procedure | `POST /admin/kill-switches` `GLOBAL_NEW_RISK_KILL` | OPERATIONS (activate) / BREAK_GLASS (release) |
| [submission-unknown.md](./submission-unknown.md) | `unknown_submissions`, `unknown_submission_rate` breach, attempts in `SUBMISSION_UNKNOWN` (PART 48) | SEV2 (elevated unknown submissions); SEV1 on disagreement | do nothing to the order; let recovery adopt or prove absent | OPERATIONS |

## Rules that apply to every runbook

1. **Kill switches stop new risk only.** `OBSERVE`, `SETTLE`, `RECONCILE`, `LEDGER_POST` and `CANCEL` are never blocked (`internal/killswitch/matrix.go`, PART 52). No runbook asks you to stop reconciliation, settlement or ledger posting.
2. **Activation is fast; release is slow.** Any operator with `kill:activate` (OPERATIONS, RISK, SECURITY, ADMIN) activates any switch with a reason of at least 8 characters; no step-up, no approval, effective on the next check. Release of a `SEVERE` switch (`GLOBAL_NEW_RISK_KILL`, `CHAIN_DISABLE_NEW_ACTIONS`, `PROVIDER_DISABLE_NEW_ACTIONS`, `FUNDING_DISABLE`, `WITHDRAWALS_DISABLE`) needs an approved dual-control `KILL_SWITCH_RELEASE` admin action plus `kill:release` plus step-up; release of a `STANDARD` switch needs `kill:release` plus step-up. `kill:release` is held by no standing role, so every release goes through a break-glass elevation (see [global-kill-and-reenable.md](./global-kill-and-reenable.md)).
3. **Balances are never edited.** The only financial repair is a compensating journal transaction (`LEDGER_CORRECTION`, `RECONCILIATION_RESOLVE_MATERIAL`), dual-controlled, reason-coded, referenced from the reconciliation record (PART 129, 195).
4. **A timeout is not a failure.** Never re-submit after a transport timeout; the order goes to `SUBMISSION_UNKNOWN` and is adopted or proven absent (PART 48).
5. **Never choose the optimistic answer** when observers disagree (PART 196).
6. **Read-only SQL runs as `cp_readonly`** (or `cp_ops`). No runbook writes to the database directly; every state change goes through a service that writes the transition row and audit event in the same transaction (migration 00603 raises SQLSTATE `AU001` otherwise).
7. **Brand-neutral.** Runbooks and incident communications refer to "the platform" (`PUBLIC_PRODUCT_NAME` is configuration; EB-009).

## Operator tooling status (what exists on disk today)

| Tool named in the runbooks | Status |
|---|---|
| `POST /admin/kill-switches`, `/admin/gates/{capability}/{action}`, `/admin/actions`, `/admin/actions/{id}/{decision}`, `/admin/accounts/{id}/status`, `/admin/instruments/{id}/status`, `/admin/providers`, `/admin/reconciliation/records[/{id}/resolve]` | **served.** Specified in `openapi/openapi.yaml`, handled in `internal/httpapi/handlers_admin.go`, permission-gated and step-up-protected in `internal/httpapi/authz.go`, wired in `cmd/api`. `TestDocs_EveryRunbookRouteIsServed` holds every path these runbooks name against the spec. |
| Reconciliation engine, `/admin/reconciliation/*` behaviour | **built.** `internal/reconciliation` (engine, `RunPeriodic`, `RunFull`), `cmd/reconciliation-worker` (ticker), read and resolve endpoints wired in `cmd/api` through `httpapi.NewReconciliationPort`. Compensation is refused by name as `UNSUPPORTED` -- see [RECONCILIATION.md](../operations/RECONCILIATION.md). |
| `make verify-audit` | **runs.** `cmd/audit-worker verify` drives `audit.Verifier.VerifyStream`. PENDING: the target is invoked by no CI job, so it is a tool an operator can run rather than a check that runs itself. **BLOCKED_EXTERNAL:** the KMS-signed Merkle tail and WORM export need a KMS and an object store. |
| `make restore-drill` | implemented, local only (`scripts/restoredrill`); production procedure unexercised (EB-012). |
| `make migrate`, `cmd/migrate verify`, `make migrate-status` | implemented. |
| Alarms and paging | **BLOCKED_EXTERNAL (R-135-1).** The metric instruments exist in `internal/observability/metrics.go` and are emitted; the collector, the `infra/terraform/modules/observability` environments and the paging routes need a cloud account. No work in this tree closes it. |
| Security event emitter | partial: `login` (identity), `funding_reversed`, `negative_deficit_accounts` (funding), and `webhook_signature_failed` (`internal/webhook`, HIGH, on every verification failure) are written to `security_events`. **PENDING:** `signing_rejection`, `global_kill`, `capability_activation`, `admin_privilege_use`, `cross_tenant_attempt`, `wallet_policy_violation`, `provider_credential_change`, `login_anomaly`, `mfa_change`. |
| Chain observers | Helius adapter (`internal/provider/helius`) and fallback RPC (`internal/provider/solanarpc`) are implemented, and `cmd/market-ingest-worker` constructs one of them. **PENDING `chain.MultiObserver`**: `openProvider` returns a single `chain.SolanaDataProvider`, so the two-observer agreement runs in tests only. **BLOCKED_EXTERNAL:** contract tests against the live provider need credentials. |
| Wallet / signing provider | contracts and repository (`internal/wallet`), pure inspector (`internal/signing/inspect`), the adapter (`internal/provider/privy`) and `signing.Service` (`internal/signing/service.go`) all exist. **BLOCKED_EXTERNAL:** a real custody provider account, without which the adapter has only ever run against its fake. |
| Execution provider (Jupiter) | client and fake (`internal/provider/jupiter`), contract fixtures (`test/contract/jupiter`), and the worker binary exists. **PENDING `bindProviders`**, which returns an error in every production build -- "the venue adapter, chain observer, inspector, signing client and recoverer are not wired in this build" -- and is replaced only in tests, so the worker never starts. **BLOCKED_EXTERNAL:** a funded mainnet account. |
| Funding provider (Stripe) | client, webhook verifier (`internal/provider/stripe`, `internal/webhook`), `test/contract/stripe`; reversal posting (`funding.Service.Reverse`). |
| Redpanda / Temporal / ClickHouse clients | outbox/relay/inbox in `internal/event`; the production clients are wired in `cmd/relay-worker`, `cmd/workflow-worker` and `cmd/market-ingest-worker`, and their integration tests pass against the local compose stack. **BLOCKED_EXTERNAL:** a deployed Redpanda, Temporal and ClickHouse beyond that stack. |
| Agent / model / market-ingest runtime | **built.** `internal/agent`, `internal/model`, `internal/strategy`, `internal/reality`, `cmd/agent-worker` and `cmd/market-ingest-worker` all exist and carry integration tests. **PENDING:** `internal/backtest` and `internal/performance` (Stage 12) do not exist, so the point-in-time store has no backtester sitting on it. |

**Every row above said the opposite until F-55.** Each carried a `PENDING` naming a package or a binary that is on disk, and this paragraph told an operator that `cmd/api` did not exist and that the only way to activate a kill switch was to write a Go program against the database. `POST /admin/kill-switches` has been served, permission-gated and step-up-protected for some time.

That is a documentation defect with an operational cost: a runbook is read under time pressure, and one that sends a responder to a Go compiler instead of an endpoint spends the minutes an incident is made of. Nothing in the production code was wrong; the map was.

The two markers now mean different things, and both are checked:

- **PENDING** -- missing from this repository. Somebody here can close it.
  `TestDocs_NothingMarkedPendingAlreadyExists` fails if a marker names a
  package, command or make target that has since been built.
- **BLOCKED_EXTERNAL** -- needs a cloud account, a provider credential, a
  funded wallet or a signed agreement. No work in this tree closes it.

A runbook may also not tell an operator to call an endpoint the API does not
declare: `TestDocs_EveryRunbookRouteIsServed` holds every `GET`/`POST` path in
these documents against `openapi/openapi.yaml`.

## Roles and permissions cheat sheet (`internal/security/roles.go`)

| Need | Permission | Standing roles |
|---|---|---|
| activate any kill switch, suspend a gate | `kill:activate` | OPERATIONS, RISK, SECURITY, ADMIN |
| release a kill switch | `kill:release` | none (BREAK_GLASS only) |
| approve / activate / resume a gate | `gate:approve` | none (BREAK_GLASS only) |
| freeze an account, propose `ACCOUNT_UNFREEZE` | `account:freeze` | COMPLIANCE, ADMIN |
| propose a reconciliation resolution | `reconciliation:resolve` | OPERATIONS, FINANCE |
| approve a material resolution | `reconciliation:approve` | none (BREAK_GLASS only) |
| propose a ledger correction | `ledger:post_correction` | FINANCE |
| approve a ledger correction | `ledger:approve_correction` | none (BREAK_GLASS only) |
| disable a provider / pause an agent | `provider:disable` / `agent:pause` | OPERATIONS, SECURITY / OPERATIONS |
| change instrument status | `instrument:status_write` | OPERATIONS, RISK, ADMIN |
| revoke anyone's sessions | `session:revoke_any` | SECURITY, ADMIN |
| request break-glass | `break_glass:request` | ADMIN (two distinct holders must agree) |

## Related

`docs/architecture/POLICY_AUTHORITY.md` §1–2, §5 · `docs/architecture/EXECUTION.md` §4, §6 · `docs/architecture/RECONCILIATION.md` · `docs/architecture/FINANCIAL_MODEL.md` §2.2 · `docs/operations/BACKUP_RESTORE.md` · `docs/operations/DISASTER_RECOVERY.md` · `docs/security/SECURITY.md` · `docs/threat-model/THREAT_MODEL.md` §6 · `docs/compliance-gates/PRODUCTION_GATES.md` · `docs/api/providers/README.md`
