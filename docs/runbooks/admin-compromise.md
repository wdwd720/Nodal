# Runbook: admin / operator account compromise

Severity: SEV1 (cross-tenant financial access; unauthorized use of authority) · Owner: SECURITY · Related: [secret-exposure.md](./secret-exposure.md), [global-kill-and-reenable.md](./global-kill-and-reenable.md), [ledger-mismatch.md](./ledger-mismatch.md), `docs/security/SECURITY.md` §3–5, §7, `docs/threat-model/THREAT_MODEL.md` §3.4, §5.2

## Trigger

- A gate, kill-switch, policy, envelope or admin-action transition nobody on the on-call rota performed: rows in `capability_gate_transitions`, `kill_switch_transitions`, `admin_action_transitions`, `capital_envelope_changes`, `risk_policies`, `account_status_transitions`, `instrument` status transitions (all append to the `admin` audit stream).
- Break-glass granted or used outside an incident ticket (`admin_actions.kind = 'BREAK_GLASS_GRANT'`, `sessions.break_glass_until` set).
- `cross_tenant_attempt` bursts from an operator principal (`ErrCrossTenant`; PENDING emitter -- the error is raised and the request refused, but nothing writes a `security_events` row, so there is no burst to see), `login_anomaly`, session creation from a new IP/device for an operator.
- Identity-provider incident (EB-017) or a reported phished/stolen operator device.
- PENDING: emitters for `admin_privilege_use`, `capability_activation`, `global_kill`, `cross_tenant_attempt`, `login_anomaly`, `mfa_change` (the `security_events` table exists; only `login`, `funding_reversed`, `negative_deficit_accounts` are written today); BLOCKED_EXTERNAL: alarm routing.

## Blast radius

What a single compromised principal can and cannot do (SECURITY.md §4):

- Any operator with `kill:activate` can activate any kill switch (denial of service against new risk, but never against reconciliation/settlement/ledger); that is loud and reversible.
- COMPLIANCE/ADMIN can freeze accounts; OPERATIONS/RISK/ADMIN can change instrument status; RISK can write risk policies (only tighter or looser limits; every version is persisted).
- No single principal can: activate a capability gate, release a SEVERE switch, post a ledger correction, resolve a material mismatch, approve a withdrawal, or grant break-glass — all dual-control permissions are held by no standing role, self-approval is refused (`RequireDualControl`, DB CHECK `approved_by_user_id <> proposed_by_user_id`), and `agent`-type principals never reach these subsystems.
- Two compromised principals (or one plus a live break-glass) can reach dual-control actions; that is the scenario to assume until proven otherwise.
- Known gap (THREAT_MODEL.md §5.2 step 8, closed by migration 00603): a bare `UPDATE capability_gates SET state` now fails with `AU001` without a transition row; the migrate role can still bypass triggers — treat any `cp_migrate` session as part of the compromise.

## Immediate actions (first 10 minutes)

1. Revoke every session of the suspect principal(s): `auth.Manager.RevokeAllForSubject(subject)` (`session:revoke_any`: SECURITY, ADMIN), served by `cmd/api`. Then, read-only:
   ```sql
   SELECT id, actor_type, roles, ip, user_agent, device_label, auth_time, amr, created_at, revoked_at, break_glass_until
     FROM sessions WHERE user_id = '<user_id>' ORDER BY created_at DESC;
   ```
2. Disable the identity at the OIDC provider (EB-017) and remove operator roles: `operator_roles` for the user (through the identity service; never by direct SQL).
3. Freeze authority changes platform-wide while you look: activate `GLOBAL_NEW_RISK_KILL` per [global-kill-and-reenable.md](./global-kill-and-reenable.md) **if** any gate, kill switch, risk policy, envelope or admin action was touched by the principal in the suspect window. If only reads occurred, `ACCOUNT_FREEZE` on any account the principal wrote to is enough.
4. Pull the principal's trail from the `admin` audit stream and account streams:
   ```sql
   SELECT stream, stream_seq, action, resource_type, resource_id, reason, source_ip, occurred_at
     FROM audit_events WHERE actor_id = '<subject>' AND occurred_at > now - interval '7 days' ORDER BY occurred_at;
   SELECT id, kind, target_type, target_id, status, proposed_by_user_id, approved_by_user_id, proposed_at, approved_at
     FROM admin_actions WHERE proposed_by_user_id = '<user_id>' OR approved_by_user_id = '<user_id>' ORDER BY proposed_at DESC;
   SELECT capability, environment, state, approval_version, approvers FROM capability_gates WHERE environment = 'PROD';
   SELECT kind, scope_id, active, activated_by_actor_id, released_by_actor_id, release_approval_id FROM kill_switches;
   ```
5. Check for a second compromised principal: every counterparty on the actions above (approver of what they proposed, proposer of what they approved) is a suspect until they confirm the action out of band.
6. Declare SEV1; page SECURITY lead and FINANCE; freeze deployments and secrets rotation decisions until forensics has the list.

## Diagnosis

- Reconstruct authority changes in the window and decide, per change, whether it stands: gate transitions (revert with `suspend`/`revoke`), kill releases (re-activate), risk policy versions (RISK writes a new stricter version; old ones stay for audit), envelope changes (`ENVELOPE_AUTHORITY_CHANGE` to restore), account status changes, instrument status changes.
- Financial effect: did any `LEDGER_CORRECTION`, `RECONCILIATION_RESOLVE_MATERIAL` or `WITHDRAWAL_APPROVE` execute? Their compensating postings are immutable; an illegitimate one is reversed by another `LEDGER_CORRECTION` under fresh dual control. Cross-check with [ledger-mismatch.md](./ledger-mismatch.md).
- Tenant access: `audit_events` with `resource_type IN ('account', 'wallet', 'ledger', 'position',...)` for accounts the principal has no business reason to read (`account:read_any` is broad by design; the audit stream is the control).
- Audit integrity: `make verify-audit` (`audit.Verifier.VerifyStream`) — a break in `admin` or `system` streams means the database itself was touched, escalate to [database-corruption.md](./database-corruption.md).
- Database sessions: `pg_stat_activity` for `cp_migrate`/`cp_ops` from unexpected hosts; RDS audit logs (BLOCKED_EXTERNAL: Terraform).
- How: phishing, stolen device, IdP compromise (a fully compromised IdP mints valid tokens; step-up and dual control bound the blast radius), leaked session cookie (`__Host-` cookie, `HttpOnly`, `Secure`, idle 30 min, absolute expiry never extended).

## Containment and recovery

1. Revert illegitimate authority changes through the normal controlled paths, each with a reason citing the incident: gates via `POST /admin/gates/{capability}/suspend` (fast, `kill:activate`) then `revoke`; kill switches re-activated; risk policies superseded; envelopes restored via dual-controlled `ENVELOPE_AUTHORITY_CHANGE`; accounts unfrozen/refrozen via `ACCOUNT_UNFREEZE`/`POST /admin/accounts/{id}/status`.
2. If any secret could have been read (config dumps, provider dashboards), run [secret-exposure.md](./secret-exposure.md) for each.
3. Re-verify every ACTIVE gate's approval chain (`approvers` JSON: two distinct approvers, neither the proposer, step-up within 15 minutes of acting, evidence hashes present); anything doubtful is `SUSPENDED` and needs a fresh dual-controlled approval version.
4. Rotate operator credentials at the IdP for everyone who acted in the window; require re-enrolment of MFA for the compromised principal.
5. Release the global kill only after every reverted change is verified and the audit chain verifies ([global-kill-and-reenable.md](./global-kill-and-reenable.md)). The release approvers must not be anyone under investigation.

## What NOT to do

- Never fix authority state with direct SQL (`AU001` will refuse a bare state change, and the migrate role must never be used for this); every revert must leave its own transition row and audit event.
- Never delete or rewrite audit or transition rows to "clean up"; they are immutable and are the evidence.
- Never let a suspect principal approve anything, including their own session revocation ticket.
- Never grant break-glass to speed up the response without the two-ADMIN `BREAK_GLASS_GRANT` action (narrow scope, 30 minute expiry).
- Never assume a single compromised account: check counterparties on every dual-control action.

## Verification / exit criteria

- Suspect sessions all `revoked_at` set; IdP identity disabled or re-enrolled; roles restored to the intended set.
- Authority state (gates, switches, policies, envelopes, account/instrument statuses) matches the last known-good snapshot, with a transition row and audit event for every revert.
- Audit chains verify; no unexplained `cp_migrate` activity.
- Any financial effect reversed under fresh dual control; reconciliation shows no open material record from the window.
- Global kill released through the approval path by principals outside the investigation.

## Post-incident

- Full audit export for the principal and the window (`admin`, `system`, affected `account:*` streams) into the WORM archive; `security_events` export (what exists) and the IdP logs.
- Security review of every dual-control action in the last 90 days for the same counterparties.
- Implement the missing emitters (`admin_privilege_use`, `capability_activation`, `global_kill`, `cross_tenant_attempt`, `login_anomaly`) — R-130-1 — and the `test/security/admin_privilege_misuse_test.go` case for the path used; update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-091-1, R-093-1 and `docs/build/BLOCKERS.md`.
