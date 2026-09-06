/**
 * The signed-in operator, as this console knows them.
 *
 * `GET /v1/me` returns the principal's roles, actor type, authentication time
 * and AMR — but not `break_glass_until`. The session carries that column
 * (internal/auth/session.go) and `internal/httpapi.toAPIPrincipal` does not
 * project it, so a console can see *that* a break-glass elevation was granted
 * and not *when it ends*.
 *
 * This module makes that gap explicit rather than papering over it. The
 * deadline is reported as `"unknown"`, `decide()` treats an unknown deadline as
 * potentially live and flags the verdict, and the console labels the affordance
 * accordingly. It never renders a countdown it cannot compute, and never hides
 * an approval the operator may in fact be able to perform.
 *
 * The fix is one field on the Principal schema; see docs/runbooks/operator-console.md.
 */
import type { Principal } from "./api.ts";
import type { DecidePrincipal } from "./decide.ts";

const ROLE_BREAK_GLASS = "BREAK_GLASS";

export interface Session {
  readonly principal: DecidePrincipal;
  readonly raw: Principal;
  /** True when the API told us a break-glass role but not its deadline. */
  readonly elevationDeadlineUnknown: boolean;
  /** Step-up validity as the server computes it, when it reports one. */
  readonly stepUpValidUntil: string | null;
}

/** Builds the console's principal from the API's. */
export function toSession(me: Principal): Session {
  const roles = [...me.roles];
  const hasElevation = roles.includes(ROLE_BREAK_GLASS);
  const principal: DecidePrincipal = {
    subjectId: me.subject_id,
    actorType: me.actor_type,
    roles,
    authTime: me.auth_time,
    amr: [...me.amr],
    // The one honest value available: present but undated.
    breakGlassUntil: hasElevation ? "unknown" : null,
    accountIds: [...me.account_ids],
  };
  return {
    principal,
    raw: me,
    elevationDeadlineUnknown: hasElevation,
    stepUpValidUntil: me.step_up_valid_until ?? null,
  };
}

/** True when the session is an operator rather than a customer or an agent. */
export function isOperator(session: Session): boolean {
  return session.principal.actorType === "OPERATOR";
}

/** True when the server currently considers the step-up fresh. */
export function stepUpFresh(session: Session, now: Date): boolean {
  if (!session.stepUpValidUntil) return false;
  const until = new Date(session.stepUpValidUntil).getTime();
  if (globalThis.isNaN(until)) return false;
  return now.getTime() < until;
}
