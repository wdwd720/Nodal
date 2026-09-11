# ADR-0024 — How a principal becomes an operator: a declaration in the deployment, never a claim from the identity provider

Status: **Accepted** (2026-09-10)

Supersedes nothing. Constrains ADR-0022: it decides how the first row appears in
the directory ADR-0022 made authoritative, without changing what that directory
is or how it is read.

## Context

`operator_roles` has decided who is an operator since migration 00010.
`internal/identity.Complete` reads it on every login — `SELECT role FROM
operator_roles WHERE user_id = $1 AND revoked_at IS NULL AND (expires_at IS NULL
OR expires_at > now())` — and a non-empty result is what makes a session an
`OPERATOR` carrying roles. Nothing else in the system decides an actor type or a
role.

**Nothing writes it.** There is no HTTP route that grants a role, no
`admin.Kind` that does, no CLI, and no worker. The only writer in the repository
is `scripts/seed`, which inserts an ADMIN row for the LOCAL fixture. The
consequence is not cosmetic: a deployment that has never had an operator cannot
get one, so `gate:propose`, `gate:approve`, `account:freeze`, `kill:activate`,
`admin:audit_read` and every route behind them are unreachable in it — including
the three-principal gate ceremony that is the only path from a disabled
capability to a live one. STAGING is in exactly that state today.

The productization goal requires an admin product-support surface (§38) and a
settings surface whose security page is real (§37). Neither is reachable without
an operator, and "run an INSERT against the production database by hand" is not
a mechanism, because it is not audited, not reviewable, and not available to
anybody who does not hold the migration credential.

## Options considered

| | Option | Authority stays in Neon | Reviewable grant | Revocation has one home | Verifiable before launch | Verdict |
|---|---|---|---|---|---|---|
| A | **A declaration in the deployment (`CP_AUTH_BOOTSTRAP_OPERATORS`), reconciled into `operator_roles` at the named identity's login** | Yes: the row is still the only thing the login reads | Yes: `granted_by`, `granted_at`, `reason` and an audit event | Yes: revoke the row | Yes: it is exercised by the integration suite against a real database | **Chosen** |
| B | ZITADEL project roles in the ID token, mapped to Nodal roles by an allowlist | No: the provider becomes the source of operator authority | No: a claim carries no grantor, no reason and no expiry | No: a role can be removed in ZITADEL or in Neon, with no reconciliation between them | No: it needs a ZITADEL console change nobody can make or verify from the repository | Rejected |
| C | An HTTP route that grants a role (behind an admin permission) | Yes | Yes | Yes | Yes | Rejected for the bootstrap: it requires an operator to exist, which is the problem. It remains the right answer for the *second* operator and is not built here |
| D | A dual-control admin action of a new kind, `OPERATOR_ROLE_GRANT` | Yes | Yes | Yes | Yes | Rejected for the same reason as C, and more strongly: dual control needs two operators |
| E | Do nothing; grant by hand with the migration credential | Yes | No: no reason, no audit row | Yes | — | Rejected: it is the status quo, and it is what the goal's §38 cannot be built on |

Option B is the one worth writing down, because it looks like the modern answer
and it is refused on three independent grounds:

1. **It contradicts a stated invariant.** `internal/identity`'s package doc has
   said since it was written that the package must never "take roles from
   identity-provider claims", and ADR-0022 §2 put the Nodal user, and therefore
   Nodal's authority, in Neon. Reversing both is exactly the kind of change an
   ADR exists for, and the reversal buys nothing the chosen option does not.
2. **A claim cannot be reviewed.** `operator_roles` has `granted_by`,
   `granted_at`, `expires_at`, `revoked_at` and `reason`. Those five columns are
   what makes "who gave this person ADMIN, when, and why" an answerable
   question. A provider claim answers none of them, and replacing a reviewable
   record with a console setting in a different system is a loss whatever the
   ergonomics.
3. **Revocation would have two homes.** A role removed in ZITADEL and present in
   Neon, or the reverse, has no reconciliation and no alarm. The failure mode of
   the disagreement is a live operator role somebody believes they removed,
   which is the worst possible failure mode for this particular thing.

## Decision

1. **`operator_roles` remains the only source of operator authority.** The
   login path is unchanged: it reads the directory and nothing else.
2. **`CP_AUTH_BOOTSTRAP_OPERATORS` declares `issuer|subject=ROLE` entries.** At
   login, when the declaration names the identity that just authenticated, the
   row is written — `granted_by` NULL, `reason` naming the variable — inside the
   login transaction, and an `operator_role.bootstrapped` audit event is appended
   on the admin stream as the SYSTEM actor `config:CP_AUTH_BOOTSTRAP_OPERATORS`.
   It is the shape D-052 established for `CP_API_SANDBOX_GATES`.
3. **The write is idempotent and never revives a revocation.** `ON CONFLICT
   (user_id, role) DO NOTHING`: a grant an operator revoked does not come back at
   the grantee's next login. Removing the declaration is how a deployment stops
   offering it. Until migration 00799 that promise was made about the bootstrap
   path alone while `cp_app` held blanket UPDATE on the directory, so a
   revocation could be undone by one statement and a `SUPPORT_READ_ONLY` row
   could become `ADMIN` in place with its provenance columns unchanged (F-175).
   The directory is now bound the way `accounts`, `users` and
   `account_closure_requests` are: the application holds no UPDATE on it,
   `operator_role_transitions` records every movement of a grant with an actor
   and a reason, and `revoked_at` is one-way whoever writes it.
4. **BREAK_GLASS is not grantable this way, and neither is CUSTOMER.**
   `operatorroles.ParseDeclarations` refuses both by name, migration 00760's
   CHECK refuses BREAK_GLASS in the column and 00800 refuses CUSTOMER. The two
   exclusions are different facts. A standing grant of the approve side of dual
   control would hand one principal both halves. CUSTOMER is not an operator role
   at all: it is what `internal/identity` gives a principal the directory says
   nothing about, so a row naming it produces a session whose `ActorType` is
   OPERATOR carrying only customer permissions — and that person's own terms
   acceptance is then written with `actor_type = 'OPERATOR'`, which 00759
   documents as an acceptance recorded on somebody's behalf (F-181).
5. **STAGING and PROD accept nothing but an empty declaration or exactly one
   ADMIN**, enforced by `config.Validate` (`RuleBootstrapOperators`). The
   variable exists to make a *first* operator possible; every grant after that is
   a decision a person makes in a directory a person can read. It says STAGING as
   well as PROD because STAGING is an internet-reachable deployment with PROD's
   cookie topology — the three auth rules either side of this one
   (`NO_DEBUG_AUTH`, `COOKIE_HOST_ONLY`, `COOKIE_SECURE`) have always said both —
   and because the grant this variable writes is permanent: by §3, removing the
   declaration stops it being re-offered and revokes nothing. Narrowing PROD
   alone left a tier that could stand up an unbounded staff directory in an
   environment variable (F-180).
6. **Dual control is unaffected.** Every approval path compares subjects, not
   roles: `security.RequireDualControl` refuses `p.SubjectID ==
   proposerSubjectID`, `internal/gates` requires two distinct approvers neither
   of whom proposed, and `internal/admin` requires `approver != proposer` (plus
   `ApproverIsNotTarget` for a break-glass grant). A principal holding every role
   the directory may name still cannot be two people.
7. **There is no API.** Nothing in the HTTP surface writes `operator_roles`, so
   no principal can grant themselves anything by making a request.

## Consequences

- A deployment names its first operator in its blueprint, the operator logs in
  once, and the grant is in the directory with a reason attached. The boot log
  names every declared entry, so an operator reading the logs can check what the
  deployment claims against what the identity provider shows.
- The subject is the identity provider's opaque identifier, which an operator
  must copy from the ZITADEL console. That is a human action and belongs in
  `HUMAN_ACTIONS_QUEUE.md`; it is also the point, because it is what makes the
  declaration specific rather than a role name anybody could match.
- **Granting the second operator is still not possible in the product.** This
  ADR does not build option C, and a deployment that wants a second operator
  either declares them too (outside PROD) or runs the INSERT with the migration
  credential. Recorded as a known gap rather than hidden: the route that closes
  it is an admin action of a new kind, which now has an operator to propose it.
- `operator_roles.role` gains a CHECK. A typo used to insert cleanly and produce
  a session `Principal.Validate` refused, which locked the operator out for a
  reason that lived in a column nobody read.

## Evidence

`internal/operatorroles` (`ParseDeclarations`, `Directory`,
`IsSafeForProduction`) and its unit tests; `internal/identity/bootstrap.go` and
`TestIntegration_Bootstrap_*`;
`TestBootstrap_APrincipalWithEveryRoleStillCannotApproveItsOwnProposal` and
`TestBootstrap_APrincipalWithEveryRoleCannotActivateAGateAlone`; migration 00760
and `TestIntegration_OperatorDirectoryRefusesAnUnknownRoleAndBreakGlass`;
`internal/config` (`RuleBootstrapOperators`); `test/integration/enums`
(`operator_roles_role_check` ↔ `operatorroles.Directory()`,
`operator_role_transitions_action_check` ↔
`operatorroles.AllTransitionActions()`). D-056, D-100.

Amended 2026-09-10 by the accounts-auth audit: migrations 00799 and 00800,
`TestAudit_TheOperatorDirectoryIsRewritableByTheApplicationRole`,
`TestIntegration_OperatorDirectoryAuthority`,
`TestAudit_StagingAcceptsAnUnboundedBootstrapDeclaration`,
`TestAudit_TheOperatorDirectoryMayNameCUSTOMER`.
