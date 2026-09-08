# Runbook: database corruption (Postgres financial database)

Severity: SEV1 · Owner: OPERATIONS (restore), FINANCE (reconciliation sign-off), SECURITY (forensics) · Related: [global-kill-and-reenable.md](./global-kill-and-reenable.md), [ledger-mismatch.md](./ledger-mismatch.md), [submission-unknown.md](./submission-unknown.md), `docs/operations/BACKUP_RESTORE.md`, `docs/operations/DISASTER_RECOVERY.md`

## Trigger

- `cmd/migrate verify` reports a checksum mismatch or an applied-version disagreement.
- `LEDGER_INTERNAL` drift that cannot be explained by application behaviour ([ledger-mismatch.md](./ledger-mismatch.md) step 5), or audit chain breaks (`prev_hash` mismatch) across streams.
- Postgres errors: `invalid page`, `could not read block`, `XX001` data corruption, `XX002` index corruption, failed Multi-AZ failover, replica divergence.
- A bad deployment or operator error that destroyed or rewrote financial rows (logical corruption).
- PART 158 rule: **if the financial database fails, do not accept new financial commands.** `/readyz` returns 503 when the database is unreachable.
- BLOCKED_EXTERNAL: RDS/CloudWatch alarms (`infra/terraform` has `modules/rds` but empty environments; EB-012).

## Blast radius

- Everything: Postgres is the only truth for accounting, reservations, orders, policies, gates, sessions (SYSTEM.md §4). Nothing else may be promoted to truth: Redis, ClickHouse, Temporal and Redpanda are explicitly never balance truth.
- While the database is down, no reconciliation, settlement or ledger posting can run either, not because of a switch but because their truth store is unavailable. External transactions keep landing; the recovery must discover them (PART 141, 48).
- Sessions fail closed (`httpmw.Session` returns 503 on store outage), so no one can act through the API during the outage.

## Immediate actions (first 10 minutes)

1. Activate `GLOBAL_NEW_RISK_KILL` per [global-kill-and-reenable.md](./global-kill-and-reenable.md) if the database still accepts writes; if it does not, record the intent and activate it on the restored instance before any service is repointed. Also activate `FUNDING_DISABLE(*)` so no provider session is created against an unknown ledger state.
2. Stop the bleeding without destroying evidence: scale the API and workers to zero (they cannot do useful work against a corrupt store), but **do not** restart, reboot or "repair" the primary instance; keep it for forensics (BACKUP_RESTORE.md §3.7). BLOCKED_EXTERNAL: ECS services (Terraform); locally `make stop`.
3. Establish the corruption time: `SELECT max(recorded_at) FROM audit_events;`, `SELECT max(posted_at) FROM journal_transactions;`, last `pg_stat_activity` snapshot, deployment log, the first alert. This is the candidate PITR point.
4. Check whether it is physical or logical: run `cmd/migrate verify` against the primary (checksum mismatch ⇒ schema tampering or bad deploy); run `SELECT * FROM pg_stat_database_conflicts;` and check the RDS event log for storage errors (BLOCKED_EXTERNAL: AWS environment).
5. Declare the incident; page FINANCE and SECURITY; freeze deployments.

## Diagnosis

- **Logical vs physical.** Logical corruption (bad migration, operator DML) is bounded by the deployment/DML window and usually leaves the audit chain intact up to that point; physical corruption produces read errors and may leave gaps in `stream_seq`.
- **Audit chain.** `make verify-audit` (use `audit.Verifier.VerifyStream`) on every stream; the first broken link bounds the damage. `SELECT stream, count(*), max(stream_seq) FROM audit_events GROUP BY stream;` should have no `stream_seq` gaps (compare to `count`).
- **Ledger invariants.** `ledger.VerifyBalances`, `positions.VerifyAgainstLedger`, `capital.VerifyReservationTotals` on the primary if readable; the recompute query in [ledger-mismatch.md](./ledger-mismatch.md) step 5.
- **Who touched it.** `pg_stat_user_tables.n_tup_upd/n_tup_del` on journal tables (must be 0), `pg_stat_activity` for `cp_migrate`, the migrations table (`cmd/migrate status`) for unexpected versions, RDS audit/pgaudit logs (BLOCKED_EXTERNAL: Terraform).
- **What is in flight externally.** `execution_attempts` in `SUBMITTED | SUBMISSION_UNKNOWN | OBSERVED | CONFIRMED` and `deposits` between `PROVIDER_CONFIRMED` and `AVAILABLE`; these are the rows the restored copy must reconcile.

## Containment and recovery

Follow `docs/operations/BACKUP_RESTORE.md` §3 exactly. It is designed but **unexercised in production** (EB-012); the local drill (`make restore-drill`, `scripts/restoredrill`) is the only proven path.

1. Choose the restore point: latest automated snapshot or the PITR timestamp just before the corruption time from step 3. Record the choice and rationale in the incident log.
2. Restore into a **new** RDS instance; never overwrite the original. Apply the production parameter group and security groups; no public exposure. (BLOCKED_EXTERNAL: Terraform RDS environment.)
3. `cmd/migrate verify` against the restored instance with the migration role (`DATABASE_MIGRATE_URL`). A checksum mismatch means the restore point predates a deployed migration: stop and escalate; do not "fix" by re-applying migrations blind.
4. Confirm the global kill and `FUNDING_DISABLE` rows are active on the restored copy (activate them there if the original activation post-dates the restore point).
5. Point a **single** reconciliation worker at the restored instance in read-only mode and run full reconciliation against external truth: `reconciliation.Engine.RunFull` for every active account and `RunPeriodic` per provider since the restore point (`internal/reconciliation`, `cmd/reconciliation-worker`). Every external transaction since the restore point that the copy does not know becomes a `SUBMISSION_UNKNOWN`/`FUNDING` record; unknown submissions are adopted or proven absent per [submission-unknown.md](./submission-unknown.md); nothing is re-submitted.
6. Run the invariant verifiers and the audit verifier on the restored copy; all must report zero drift. Compare row counts and the journal hash with the incident timeline (the restore drill's checks).
7. Obtain dual approval to promote: an `admin_actions` row proposed by FINANCE and approved by SECURITY is the recorded evidence (use kind `LEDGER_CORRECTION` if any compensating postings are needed for gaps; otherwise record the promotion decision in the incident ticket with both approvers' step-up timestamps). PENDING: a dedicated `DATABASE_PROMOTE` action kind does not exist.
8. Rotate database credentials (`aws-sm://` SecretRef targets; BLOCKED_EXTERNAL: the aws-sm:// resolver), repoint services, bring workers up one at a time (reconciliation first, execution last), watch `reconciliation_mismatches` and `ledger_posting_errors`.
9. Release `FUNDING_DISABLE` then `GLOBAL_NEW_RISK_KILL` via the dual-controlled path only after reconciliation has converged (DISASTER_RECOVERY.md §2).
10. Keep the original instance until SECURITY closes forensics.

## What NOT to do

- Never accept new financial commands against a database whose integrity is in question (PART 158).
- Never restore in place or "repair" rows by hand; never run `pg_resetwal`-style tools on the primary.
- Never re-submit signed transactions found in `execution_attempts` during recovery; discover them on chain instead (PART 48).
- Never promote the restored copy before full reconciliation has run; a clean `migrate verify` is not convergence.
- Never edit balances to "match the chain"; every difference is a reconciliation record with a compensating posting.
- Never let the migrate role be used for application data, before or after the restore.

## Verification / exit criteria

- `cmd/migrate verify` OK on the promoted instance; version equals the deployed build's expectation.
- `ledger.VerifyBalances`, `positions.VerifyAgainstLedger`, `capital.VerifyReservationTotals`, audit chain: zero drift, no gaps.
- Every reconciliation record opened during recovery is `MATCHED`, `RESOLVED_AUTOMATIC` or `RESOLVED_MANUAL` with approval; no order in `SUBMISSION_UNKNOWN`/`RECONCILIATION_REQUIRED`; no deposit stuck between `PROVIDER_CONFIRMED` and `AVAILABLE`.
- Capability gates unchanged or more restrictive (`SELECT capability, state FROM capability_gates WHERE environment = 'PROD'`).
- Measured RTO/RPO recorded; kill switches released through the approval path.

## Post-incident

- Archive: restore decision record, `migrate verify` output, verifier outputs, reconciliation convergence report, promotion approval, credential rotation log; all hashed into the WORM archive.
- Security event review: any `cp_migrate` session, any break-glass grant, any gate/kill transition in the window.
- Update `docs/operations/DISASTER_RECOVERY.md` §1 with measured RTO/RPO (PART 205) and `docs/build/BLOCKERS.md` EB-012 status; update R-141-1 and R-205-1 in `docs/build/REQUIREMENTS_TRACEABILITY.md`.
- Schedule the quarterly staging PITR drill if this was the first real restore.
