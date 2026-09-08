# Runbook: global new-risk kill and re-enable (PART 165)

Severity: procedure (invoked by SEV1 runbooks; exercised monthly in staging) · Owner: OPERATIONS activates, BREAK_GLASS releases · Related: every SEV1 runbook, [ledger-mismatch.md](./ledger-mismatch.md), [database-corruption.md](./database-corruption.md), [admin-compromise.md](./admin-compromise.md)

## Trigger

- Any SEV1 runbook that says "activate `GLOBAL_NEW_RISK_KILL`": ledger integrity violation, unauthorized signing, duplicate economic execution across accounts, signing credential compromise, admin compromise touching authority tables, database corruption.
- Global money-impacting reconciliation drift: `oldest_unresolved_mismatch` (seconds) above the risk policy `max_unresolved_age` (PART 158). The kernel already rejects with `RISK_RECONCILIATION_PENDING`; the kill makes the halt explicit and auditable.
- Monthly staging drill (`docs/operations/DISASTER_RECOVERY.md` §3).
- PENDING: the `global_kill` CRITICAL security event (only the `admin` audit stream records the transition today). BLOCKED_EXTERNAL: alarm routing (`infra/terraform/modules/observability`).

## Blast radius

Blocked (`ActionClass = NEW_RISK`, `WITHDRAW`): new manual trade intents, new agent intents, new funding sessions, withdrawals. Rejected with `KILL_SWITCH_ACTIVE` (field `switch = GLOBAL_NEW_RISK_KILL:*`); the risk kernel returns `RISK_KILL_SWITCH`.

Still running, by construction (`internal/killswitch/matrix.go`, `TestProp_NeverBlockedClasses`, `TestProp_GlobalKillNeverStopsRiskReduction`): `REDUCE_RISK` (closing positions), `OBSERVE`, `SETTLE`, `RECONCILE`, `LEDGER_POST`, `CANCEL`. Orders already `SUBMITTED` continue to finality, fills post, positions update, reconciliation keeps opening and resolving records, audit keeps appending. the executor and the reconciliation engine that exercise those paths end to end both exist (`internal/execution` with `cmd/execution-worker`; `internal/reconciliation` is absent), so "still runs" is proven only at the matrix level today.

## Immediate actions (first 10 minutes)

1. Activate. Single operator holding `kill:activate` (OPERATIONS, RISK, SECURITY, ADMIN); no step-up, no approval, effective on the next check (cache for pre-checks ≤ 1 s):
   ```http
   POST /admin/kill-switches
   Idempotency-Key: <uuid>
   {"kind":"GLOBAL_NEW_RISK_KILL","scope_id":"*","action":"activate","reason":"<INC-id>: <one line cause>"}
   ```
   Until it exists the only path is `killswitch.Controller.Activate(ctx, tx, killswitch.GlobalNewRiskKill, "*", reason)` from an operator-run Go program; record who ran it and when in the incident log.
2. Confirm the row and the transition (read-only, `cp_readonly`):
   ```sql
   SELECT kind, scope_id, active, severity, activated_by_actor_id, activated_at, version
     FROM kill_switches WHERE kind = 'GLOBAL_NEW_RISK_KILL';
   SELECT to_active, actor_type, actor_id, reason, occurred_at
     FROM kill_switch_transitions WHERE kind = 'GLOBAL_NEW_RISK_KILL' ORDER BY occurred_at DESC LIMIT 3;
   SELECT stream_seq, action, actor_id, reason, occurred_at
     FROM audit_events WHERE stream = 'admin' ORDER BY stream_seq DESC LIMIT 5;
   ```
3. Post the activation time, actor and reason in the incident channel; open the incident ticket if the calling runbook has not.
4. Snapshot what is in flight so nobody "helps" it: `SELECT status, count(*) FROM orders WHERE terminal_at IS NULL GROUP BY status;` and `SELECT status, count(*) FROM execution_attempts WHERE status IN ('SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN','OBSERVED','CONFIRMED') GROUP BY status;`. These are left to the executor/recoverer.
5. Decide whether narrower switches are also needed (`ACCOUNT_FREEZE`, `PROVIDER_DISABLE_NEW_ACTIONS`, `FUNDING_DISABLE`). A global kill does not replace them when the cause is provider- or account-specific, because they must persist after the global kill is released.

## Diagnosis

The global kill is a containment tool, not a diagnosis; follow the calling runbook. What to verify while it is active (the PART 165 checklist):

| Check | How |
|---|---|
| new manual trade rejected | `POST /v1/intents` returns problem `KILL_SWITCH_ACTIVE`; `risk_decisions` rows show `RISK_KILL_SWITCH` |
| new agent intent rejected | same reason code on an AGENT-actor intent; `agent_runs.skip_reason` (`internal/agent`) |
| existing submitted trade still reconciles | `reconciliation_records` keep appearing for in-flight attempts (PENDING: engine) |
| fills still post | `fills.journal_transaction_id` becomes non-null; `journal_transactions.kind = 'TRADE_FILL'` continues |
| positions still update | `fills.position_applied_at` set |
| audit still works | `audit_events.stream_seq` advancing on `account:*` and `system` streams; `make verify-audit` |
| operator sees state | `GET /admin/kill-switches` shows `active = true`; the SQL above |

## Containment and recovery

This section is the re-enable path. Release is deliberately slow (PART 53). Preconditions before anyone proposes a release:

1. Root cause of the calling incident is fixed and written up.
2. No open material reconciliation record: `SELECT count(*) FROM reconciliation_records WHERE material AND status IN ('MISMATCH','INVESTIGATING','ESCALATED');` returns 0, and no order remains in `SUBMISSION_UNKNOWN` or `RECONCILIATION_REQUIRED`.
3. Audit verification passes: `make verify-audit` (until then an engineer runs `audit.Verifier.VerifyStream` on `admin`, `system` and the affected `account:*` streams and archives the output).
4. Capability gates are in their pre-incident state or more restrictive: `SELECT capability, state, approval_version FROM capability_gates WHERE environment = 'PROD';`.

Steps (each step is a different human; every step needs step-up within 15 minutes):

1. Break-glass for the approver, because `kill:release` is held by no standing role: two distinct ADMIN principals holding `break_glass:request` propose and approve an `admin_actions` row of kind `BREAK_GLASS_GRANT` (step-up ≤ 5 min, expires 30 min); the grantee rotates their session to obtain `BreakGlassUntil`.
2. Propose the release (any `kill:activate` holder):
   ```http
   POST /admin/actions
   {"kind":"KILL_SWITCH_RELEASE","target_type":"kill_switch","target_id":"GLOBAL_NEW_RISK_KILL:*",
    "params":{"kind":"GLOBAL_NEW_RISK_KILL","scope_id":"*"},
    "reason":"<INC-id>: root cause fixed; reconciliation converged; audit verified"}
   ```
   Evidence to attach (ids and SHA-256 hashes in `params`): incident ticket, reconciliation convergence report, audit verification output, list of adopted/expired unknown submissions.
3. Approve (a different principal holding `kill:release` via break-glass): `POST /admin/actions/{actionId}/approve` with a note. The action expires 1 hour after proposal (`internal/admin/kinds.go`).
4. Release, quoting the approval:
   ```http
   POST /admin/kill-switches
   {"kind":"GLOBAL_NEW_RISK_KILL","scope_id":"*","action":"release","reason":"<INC-id>: release per action <id>","approval_id":"<actionId>"}
   ```
   `killswitch.Controller.Release` verifies the action is `APPROVED`, dual-controlled, targets the same `(kind, scope)`, and that the caller holds `kill:release` with fresh step-up (`TestController_ApprovalVerificationRules`).
5. Confirm `active = false` with the SQL in Immediate actions step 2; confirm the `admin` audit stream has the release row carrying `approval_id`.

Every HTTP call above is served. The services behind them (`admin.Actions.Propose/Approve`, `killswitch.Controller.Release`) exist and are integration-tested.

## What NOT to do

- Never release without the approved `KILL_SWITCH_RELEASE` action, even in staging drills; the drill exists to prove the path.
- Never "release" by updating `kill_switches.active` in SQL: `cp_app` cannot do it without a transition row (migration 00603, SQLSTATE `AU001`), and the migrate role must never touch application state.
- Never stop the reconciliation or execution workers "to be safe": the switch already stops new risk, and stopping workers blinds the platform to already-executed transactions (PART 52).
- Never resubmit or cancel in-flight orders by hand while the kill is active.
- Never activate silently: the reason is mandatory and the activation is announced in the incident channel.

## Verification / exit criteria

- `kill_switches` row `active = false`, `release_approval_id` non-null, and the approver of that action is not the proposer.
- One new manual intent in a CANARY account passes `PRE_TRADE` risk with no `RISK_KILL_SWITCH`.
- No `KILL_SWITCH_ACTIVE` rejections in the 10 minutes after release, except from narrower switches deliberately left active.
- Incident log records activation time, release time, both actors and the approval id.

## Post-incident

- Export the `admin` audit stream slice from activation to release (`audit_events WHERE stream = 'admin' AND occurred_at BETWEEN...`) into the evidence archive with its content hashes.
- Review `security_events` for the window (PENDING: `global_kill` emitter; none is written today).
- Record measured activation-to-effect latency and release lead time against R-053-13 and R-165-1 in `docs/build/REQUIREMENTS_TRACEABILITY.md`; file `test/e2e/global_kill_test.go` as a SOFTWARE BLOCKER in `docs/build/BLOCKERS.md` if still absent.
- If any never-blocked class was in fact blocked, that is a SEV1 defect: open a BLOCKERS entry and do not release the next kill until it is fixed.
