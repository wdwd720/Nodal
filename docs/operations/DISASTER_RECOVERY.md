# DISASTER RECOVERY

Status: design and local evidence only (2026-09-06). No production or staging environment exists (BLOCKERS EB-012), so every RTO/RPO figure below is a **design objective**, not a measurement (PART 205). Measured values are recorded here only after a staging drill.

## 1. Failure model and objectives

| Scenario | Design objective | Mechanism | Evidence today |
|---|---|---|---|
| Single AZ loss | RPO 0 for committed financial transactions; RTO minutes | RDS Multi-AZ synchronous standby; ECS services spread across 3 AZs; per-AZ NAT in prod | Terraform design (pending validate/apply) |
| Primary database instance failure | RPO 0; automatic failover | RDS Multi-AZ | — |
| Logical corruption (bad deploy, operator error) | RPO ≤ 5 min (PITR granularity); RTO hours | PITR restore into a **new** instance, verify, reconcile, promote (BACKUP_RESTORE.md §3) | local drill proves dump→restore→verify→ledger-consistency in about 11 s on a small dataset |
| Region loss | RPO ≤ 24 h from cross-region snapshot copy; RTO ≥ hours; **new risk stays killed until reconciliation converges** | cross-region automated backup replication (design), evidence buckets replicated, Terraform re-apply in the secondary region | not designed in Terraform yet; single-primary-region V1 (ADR 0017) |
| Evidence archive loss | none for audit archive: Object Lock COMPLIANCE + versioning + replication | S3 | Terraform design |
| Event bus (Redpanda) loss | no financial loss: Postgres + outbox is the source; relay republishes after recovery | `internal/event` relay is at-least-once from the outbox table | outbox/relay tests (crash between publish and mark) |
| Temporal loss | no financial loss: workflow state is orchestration only; activities are idempotent against Postgres state | PART 116 | funding lifecycle driver is Temporal-free at the domain layer |
| ClickHouse loss | trading continues if required critical data remains available; analytics degrade | PART 158 | design |
| Provider loss (Helius, Jupiter, Stripe, Privy) | no new risk on the affected path; observation/reconciliation continue on the fallback observer | provider health + kill switches (PART 107) | `internal/provider`, `internal/killswitch` tests |

## 2. What must be true before "recovered" is declared

1. `cmd/migrate verify` passes on the restored database.
2. `ledger.VerifyBalances`, `positions.VerifyAgainstLedger`, `capital.VerifyReservationTotals`, and the audit chain verifier report zero drift.
3. Full reconciliation against external truth (chain observers, providers) has run and every mismatch is a reconciliation record — none are silently absorbed.
4. Unknown submissions discovered during the outage are adopted or proven absent (PART 48); nothing is re-submitted blindly.
5. Capability gates remain in their pre-incident state or more restrictive; `GLOBAL_NEW_RISK_KILL` is released only through the dual-controlled path.

## 3. Drill cadence (planned)

| Drill | Cadence | Evidence artefact |
|---|---|---|
| Local restore drill (`make restore-drill`) | every CI run on main (planned) and before every release | `dist/restore-drill.json` |
| Staging PITR restore + reconciliation convergence | quarterly | signed report with measured RTO/RPO |
| Region failover tabletop | semi-annual | incident log |
| Kill-switch activation/release | monthly, staging | audit stream export |

## 4. Related

`docs/operations/BACKUP_RESTORE.md`, `docs/architecture/RECONCILIATION.md`, `docs/adr/0017-single-primary-region-v1.md`, `docs/runbooks/` (pending), BLOCKERS EB-012.
