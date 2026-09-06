# ADR-0016: Production capabilities are software-enforced, evidence-backed gates

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Whether the platform may fund accounts, trade live, run agents with real capital,
or permit withdrawals depends on legal determinations, provider contracts, risk
sign-off, and security review that live outside the repository (BLOCKERS
EB-001 through EB-016). Goal PART 54 requires those external answers to become
software-enforced gates. The failure modes of anything weaker:

- One environment variable turns on live trading; a misconfigured deployment or a
  copied `.env` file is a regulatory event (PART 55).
- Approvals recorded in a document nobody can find, with no link between the
  approval and the running system.
- Expired approvals that stay effective because nothing checks expiry.
- A single operator activating a high-risk capability alone.
- An agent, through an admin tool exposed to it, enabling its own capabilities
  (PART 9).
- Approvals in a configuration file that can be edited without review.
- Documentation claiming a capability is "gated" while code has no check.

## Decision

- A capability registry lives in Postgres with the fields in PART 54: capability,
  environment, state, approval version, legal review reference, provider contract
  reference, risk approval, security approval, approvers, evidence hashes,
  effective date, expiry, revocation. States: `DISABLED`, `PENDING_APPROVAL`,
  `APPROVED`, `ACTIVE`, `SUSPENDED`, `REVOKED`, `EXPIRED`.
- A capability is effective only when all three hold at once (PART 55): deployment
  configuration enables it, the persistent registry record is `ACTIVE` for this
  environment, and its evidence is present and non-expired. Any one missing means
  not effective.
- High-risk capabilities (`LIVE_FUNDING`, `LIVE_MANUAL_TRADING`,
  `LIVE_AGENT_TRADING`, `WITHDRAWALS`, and any capability that permits value to
  leave the platform) require dual authorization by distinct approvers with the
  required roles; the approval itself is a step-up action (PART 91, 93).
- Every money path checks the relevant gate at the point of action, not only at
  the API edge; the check is a typed error (`CAPABILITY_NOT_APPROVED`) when it
  fails.
- Revocation and suspension take effect immediately and are audit events; expiry
  is evaluated on every check.
- Agents cannot reach the capability subsystem: the agent principal type has no
  permission on it, and agent packages do not import it (ADR-0003, ADR-0012).
- Default for every capability in every environment is `DISABLED` (PART 244).
  Production configuration validation refuses to start with fake providers, seed
  data, or debug authentication regardless of gate state.
- Capability state per environment is reported in the readiness report using the
  goal's status values (PART 243), and each external blocker maps to the gate it
  blocks (BLOCKERS.md).

### Explicitly not decided / deferred

- The composition of approver roles for each capability, and the evidence document
  formats. These are compliance decisions recorded in
  `docs/compliance-gates/PRODUCTION_GATES.md` when made.
- Per-jurisdiction capability variants; jurisdiction is handled by the eligibility
  engine (PART 56, 57), and a gate is global per environment.

## Consequences

### Positive

- External blockers are visible in software state, so `BLOCKED_EXTERNAL` is a
  verifiable claim (PART 167).
- No single person or configuration change can activate live money movement.

### Negative

- Activation is slow by design; staging and canary must exercise the full approval
  path so it is rehearsed before it matters.
- One more table on the critical path; the check must be cheap and cached only in
  process memory with short lifetime, never as authority.

### Operational

- Gate transitions are P1 security events with notification (PART 130, 135).
- Expiry reminders are an operations task; an expired gate stops live activity.

## Alternatives considered

- Feature-flag services: single-click activation from an external control plane,
  no evidence binding, no dual authorization. Rejected for money capabilities;
  acceptable for UI flags.
- Environment variables alone: explicitly rejected by PART 55.
- Hard-coded constants per release: a deploy becomes an approval; no expiry, no
  revocation without a release.
- Approval in a ticketing system only: not enforceable by software.

## Related

- ADR-0003, ADR-0012, ADR-0013, ADR-0017, ADR-0018, ADR-0019.
- Goal PART 9, 54, 55, 56, 91, 93, 94, 128, 130, 164, 167, 208, 240–244.
- BLOCKERS EB-001–EB-016 (each maps to a gate).

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Tests: configuration alone cannot make a capability effective; an `ACTIVE`
  record without configuration is not effective; expired or missing evidence makes
  an `ACTIVE` record ineffective.
- Dual-authorization test: a single approver cannot move a high-risk capability to
  `ACTIVE`; the same approver twice is rejected.
- Agent principal receives `FORBIDDEN` on every capability endpoint; import
  boundary test covers the capability package.
- E2E per capability (PART 160–165): each live path returns
  `CAPABILITY_NOT_APPROVED` when the gate is not effective, at the action point.
- Admin E2E (PART 164) covering approve, activate, suspend, revoke, expire, each
  producing an audit event with approvers and evidence hashes.
- PROD configuration validation test.
- Readiness report section listing every capability's state per environment.
