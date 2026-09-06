/**
 * The console's copy of the operator-authority decision, ported from
 * `internal/adminplane/decision.go`.
 *
 * Why a copy exists at all: the console has to decide what to render before it
 * has asked the server. The alternative to a copy is a guess, and a guess is
 * how an operator ends up clicking Approve on their own proposal and being
 * told no — or, worse, being shown a greyed-out button for something they
 * could actually have done.
 *
 * Why the copy is safe: `internal/adminplane` generates
 * `src/generated/decisions.json`, a vector for every (principal shape, action
 * shape, verb, instant) combination, and `decide.test.ts` replays every one of
 * them through this function. The Go integration suite separately proves those
 * same verdicts equal what `internal/admin` and the database actually do. So a
 * green console test means this file agrees with the enforcing layer, and a
 * drifting port fails the test rather than an operator.
 *
 * This is advisory. Every command still goes to the server, which refuses it
 * again with the real authority. Nothing here is a security control.
 */
import type { ActionKind, AuthorityIndex, Permission, Role } from "./authority.ts";

export const VERBS = ["propose", "approve", "reject", "execute", "cancel"] as const;
export type Verb = (typeof VERBS)[number];

export type Reason =
  | "ALLOWED"
  | "UNAUTHENTICATED"
  | "AGENT_PRINCIPAL"
  | "INVALID_PRINCIPAL"
  | "SUBJECT_NOT_A_USER"
  | "UNKNOWN_KIND"
  | "MISSING_PERMISSION"
  | "STEP_UP_REQUIRED"
  | "SELF_APPROVAL"
  | "NOT_PROPOSER"
  | "KIND_TAKES_NO_APPROVAL"
  | "EXPIRED"
  | "WRONG_STATUS"
  | "AWAITING_APPROVAL"
  | "APPROVER_NOT_DISTINCT";

export interface Decision {
  readonly verb: Verb;
  readonly allowed: boolean;
  readonly reason: Reason;
  /** The errs.Code the server would answer with; absent when allowed. */
  readonly code?: string;
  readonly permission?: Permission;
  readonly permissions?: readonly Permission[];
  readonly stepUpMaxAgeSeconds?: number;
  /**
   * True when the verdict depends on a break-glass elevation whose deadline
   * this console cannot see. GET /v1/me returns the BREAK_GLASS role but not
   * `break_glass_until`, so the console can tell that an elevation was granted
   * and not whether it is still live. The affordance is offered and labelled;
   * the server is the one that knows.
   */
  readonly elevationUnverified?: boolean;
}

/**
 * The principal as the console holds it. `breakGlassUntil` is the deadline
 * when it is known, `"unknown"` when the BREAK_GLASS role is present but the
 * API did not say when it ends, and null when there is no elevation.
 */
export interface DecidePrincipal {
  readonly absent?: boolean;
  readonly subjectId: string;
  readonly actorType: string;
  readonly roles: readonly Role[];
  readonly authTime: string | null;
  readonly amr: readonly string[];
  readonly breakGlassUntil: string | "unknown" | null;
  readonly accountIds?: readonly string[];
}

/** One stored admin action, as the console receives it. */
export interface DecideAction {
  readonly kind: string;
  readonly status: string;
  readonly requiresDual: boolean;
  readonly proposedBy: string;
  readonly approvedBy: string | null;
  readonly expiresAt: string;
}

const ACTOR_AGENT = "AGENT";
const ROLE_BREAK_GLASS = "BREAK_GLASS";
const KNOWN_ACTOR_TYPES = new Set(["USER", "OPERATOR", "SERVICE", "AGENT", "SYSTEM"]);

/**
 * Authentication method references that count as a second factor, mirroring
 * security.StrongAMR. Password, SMS and knowledge-based answers do not.
 */
const STRONG_AMR = new Set(["mfa", "otp", "hwk", "swk", "pop", "webauthn", "passkey"]);

const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
const NIL_UUID = "00000000-0000-0000-0000-000000000000";

export function hasStrongAmr(amr: readonly string[]): boolean {
  return amr.some((v) => STRONG_AMR.has(v.toLowerCase()));
}

/** Mirrors id.ParseAny plus the non-zero check in adminplane.NewActor. */
export function isUserId(subject: string): boolean {
  return UUID_RE.test(subject) && subject.toLowerCase() !== NIL_UUID;
}

function ms(value: string | null): number | null {
  if (!value) return null;
  const t = Date.parse(value);
  return Number.isNaN(t) ? null : t;
}

/** The break-glass state of a principal at `now`, without judging authority. */
export type ElevationState = "NONE" | "ACTIVE" | "EXPIRED" | "UNKNOWN_DEADLINE" | "INCOHERENT";

export function elevationOf(p: DecidePrincipal, now: Date): ElevationState {
  const hasRole = p.roles.includes(ROLE_BREAK_GLASS);
  const until = p.breakGlassUntil;
  if (!hasRole && until === null) return "NONE";
  if (hasRole && until === "unknown") return "UNKNOWN_DEADLINE";
  if (hasRole !== (until !== null)) return "INCOHERENT";
  const deadline = ms(typeof until === "string" && until !== "unknown" ? until : null);
  if (deadline === null) return "INCOHERENT";
  return now.getTime() < deadline ? "ACTIVE" : "EXPIRED";
}

/** True while a break-glass grant is live. An unknown deadline counts as live. */
function breakGlassActive(p: DecidePrincipal, now: Date): boolean {
  const state = elevationOf(p, now);
  return state === "ACTIVE" || state === "UNKNOWN_DEADLINE";
}

/** Mirrors security.Principal.Validate. */
export function validatePrincipal(p: DecidePrincipal, index: AuthorityIndex): string | null {
  if (p.subjectId === "") return "principal has empty subject";
  if (!KNOWN_ACTOR_TYPES.has(p.actorType)) return `unknown actor type ${p.actorType}`;
  for (const r of p.roles) {
    if (!index.knownRole(r)) return `unknown role ${r}`;
  }
  if (p.actorType === ACTOR_AGENT) {
    if (p.roles.length !== 0) return "agent principal must not carry roles";
    if ((p.accountIds?.length ?? 1) !== 1) return "agent principal must be bound to exactly one account";
    if (p.breakGlassUntil !== null) return "agent principal must not carry break-glass";
  }
  if (p.roles.includes(ROLE_BREAK_GLASS) && p.breakGlassUntil === null) {
    return "BREAK_GLASS role requires a deadline";
  }
  return null;
}

/** Mirrors security.Principal.Has. */
export function holds(p: DecidePrincipal, permission: Permission, now: Date, index: AuthorityIndex): boolean {
  if (validatePrincipal(p, index) !== null) return false;
  if (p.actorType === ACTOR_AGENT) return index.agentHolds(permission);
  const live = breakGlassActive(p, now);
  for (const role of p.roles) {
    if (role === ROLE_BREAK_GLASS && !live) continue;
    if (index.roleGrants(role, permission)) return true;
  }
  return false;
}

function holdsAny(p: DecidePrincipal, perms: readonly Permission[], now: Date, index: AuthorityIndex): boolean {
  return perms.some((perm) => holds(p, perm, now, index));
}

/** Mirrors security.RequireStepUp. */
export function steppedUp(p: DecidePrincipal, maxAgeSeconds: number, now: Date): boolean {
  if (maxAgeSeconds <= 0) return false;
  const authTime = ms(p.authTime);
  if (authTime === null) return false;
  const age = Math.max(0, now.getTime() - authTime);
  return age <= maxAgeSeconds * 1000 && hasStrongAmr(p.amr);
}

/**
 * True when the verdict rests on an elevation whose deadline is unknown, so
 * the console must say the server has the last word.
 */
function restsOnUnverifiedElevation(
  p: DecidePrincipal,
  permission: Permission | undefined,
  now: Date,
  index: AuthorityIndex,
): boolean {
  if (permission === undefined || elevationOf(p, now) !== "UNKNOWN_DEADLINE") return false;
  if (!index.isDualControl(permission)) return false;
  // Would a standing role alone have sufficed? If so nothing rests on the
  // elevation.
  const withoutElevation: DecidePrincipal = { ...p, roles: p.roles.filter((r) => r !== ROLE_BREAK_GLASS), breakGlassUntil: null };
  return !holds(withoutElevation, permission, now, index);
}

function isAbsent(p: DecidePrincipal): boolean {
  return p.absent === true || (p.subjectId === "" && p.actorType === "" && p.roles.length === 0);
}

interface Draft {
  verb: Verb;
  allowed: boolean;
  reason: Reason;
  code?: string;
  permission?: Permission;
  permissions?: readonly Permission[];
  stepUpMaxAgeSeconds?: number;
  elevationUnverified?: boolean;
}

function finish(d: Draft): Decision {
  const out: Decision = {
    verb: d.verb,
    allowed: d.allowed,
    reason: d.reason,
    ...(d.code === undefined ? {} : { code: d.code }),
    ...(d.permission === undefined || d.permission === "" ? {} : { permission: d.permission }),
    ...(d.permissions === undefined ? {} : { permissions: d.permissions }),
    ...(d.stepUpMaxAgeSeconds ? { stepUpMaxAgeSeconds: d.stepUpMaxAgeSeconds } : {}),
    ...(d.elevationUnverified ? { elevationUnverified: true } : {}),
  };
  return out;
}

function denied(verb: Verb, reason: Reason, code: string): Decision {
  return finish({ verb, allowed: false, reason, code });
}

/** Mirrors admin.caller: present, not an agent, structurally valid. */
function authenticated(p: DecidePrincipal, verb: Verb, index: AuthorityIndex): Decision | null {
  if (isAbsent(p)) return denied(verb, "UNAUTHENTICATED", "UNAUTHENTICATED");
  if (p.actorType === ACTOR_AGENT) return denied(verb, "AGENT_PRINCIPAL", "FORBIDDEN");
  if (validatePrincipal(p, index) !== null) return denied(verb, "INVALID_PRINCIPAL", "FORBIDDEN");
  return null;
}

/** The any-of set for reject and execute: propose plus a distinct approve. */
function actorPermissions(spec: ActionKind): readonly Permission[] {
  const perms = [spec.propose_permission];
  if (spec.approve_permission && spec.approve_permission !== spec.propose_permission) {
    perms.push(spec.approve_permission);
  }
  return [...perms].sort();
}

/** Whether the state machine allows from → to (admin.CanTransition). */
function canTransition(index: AuthorityIndex, from: string, to: string): boolean {
  return (index.doc.action_transitions[from] ?? []).includes(to);
}

/** Whether the action can no longer move (expiry is inclusive). */
function expired(action: DecideAction, now: Date): boolean {
  const at = ms(action.expiresAt);
  return at === null || now.getTime() >= at;
}

/** Whether the actor may propose `kind` at `now`. */
export function decideProposal(
  p: DecidePrincipal,
  kind: string,
  now: Date,
  index: AuthorityIndex,
): Decision {
  const early = authenticated(p, "propose", index);
  if (early) return early;
  const spec = index.kind(kind);
  if (!spec) return denied("propose", "UNKNOWN_KIND", "VALIDATION_FAILED");

  const d: Draft = {
    verb: "propose",
    allowed: false,
    reason: "ALLOWED",
    permission: spec.propose_permission,
    stepUpMaxAgeSeconds: spec.step_up_max_age_seconds,
  };
  if (!holds(p, spec.propose_permission, now, index)) {
    d.reason = "MISSING_PERMISSION";
    d.code = "FORBIDDEN";
  } else if (!steppedUp(p, spec.step_up_max_age_seconds, now)) {
    d.reason = "STEP_UP_REQUIRED";
    d.code = "STEP_UP_REQUIRED";
  } else if (!isUserId(p.subjectId)) {
    d.reason = "SUBJECT_NOT_A_USER";
    d.code = "FORBIDDEN";
  } else {
    d.allowed = true;
    d.elevationUnverified = restsOnUnverifiedElevation(p, spec.propose_permission, now, index);
  }
  return finish(d);
}

/** Whether the actor may apply `verb` to `action` at `now`. */
export function decide(
  p: DecidePrincipal,
  action: DecideAction,
  verb: Verb,
  now: Date,
  index: AuthorityIndex,
): Decision {
  const early = authenticated(p, verb, index);
  if (early) return early;
  switch (verb) {
    case "approve":
      return decideApprove(p, action, now, index);
    case "reject":
      return decideReject(p, action, now, index);
    case "cancel":
      return decideCancel(p, action);
    case "execute":
      return decideExecute(p, action, now, index);
    case "propose":
      return decideProposal(p, action.kind, now, index);
    default:
      return denied(verb, "UNKNOWN_KIND", "VALIDATION_FAILED");
  }
}

/** Every verb that applies to a stored record, in declaration order. */
export function decideAll(
  p: DecidePrincipal,
  action: DecideAction,
  now: Date,
  index: AuthorityIndex,
): readonly Decision[] {
  return VERBS.filter((v) => v !== "propose").map((v) => decide(p, action, v, now, index));
}

function decideApprove(p: DecidePrincipal, action: DecideAction, now: Date, index: AuthorityIndex): Decision {
  const spec = index.kind(action.kind);
  if (!spec) return denied("approve", "UNKNOWN_KIND", "INTERNAL");
  const approve = spec.approve_permission;
  const d: Draft = {
    verb: "approve",
    allowed: false,
    reason: "ALLOWED",
    ...(approve ? { permission: approve } : {}),
    stepUpMaxAgeSeconds: spec.step_up_max_age_seconds,
  };
  if (!spec.requires_dual || !approve) {
    delete d.permission;
    d.reason = "KIND_TAKES_NO_APPROVAL";
    d.code = "INVALID_STATE_TRANSITION";
  } else if (!holds(p, approve, now, index)) {
    d.reason = "MISSING_PERMISSION";
    d.code = "FORBIDDEN";
  } else if (!steppedUp(p, spec.step_up_max_age_seconds, now)) {
    d.reason = "STEP_UP_REQUIRED";
    d.code = "STEP_UP_REQUIRED";
  } else if (!isUserId(p.subjectId)) {
    d.reason = "SUBJECT_NOT_A_USER";
    d.code = "FORBIDDEN";
  } else if (p.subjectId.toLowerCase() === action.proposedBy.toLowerCase()) {
    // Dual control. The console must never let this be clickable.
    d.reason = "SELF_APPROVAL";
    d.code = "FORBIDDEN";
  } else if (expired(action, now)) {
    d.reason = "EXPIRED";
    d.code = "INVALID_STATE_TRANSITION";
  } else if (!canTransition(index, action.status, "APPROVED")) {
    d.reason = "WRONG_STATUS";
    d.code = "INVALID_STATE_TRANSITION";
  } else {
    d.allowed = true;
    d.elevationUnverified = restsOnUnverifiedElevation(p, approve, now, index);
  }
  return finish(d);
}

function decideReject(p: DecidePrincipal, action: DecideAction, now: Date, index: AuthorityIndex): Decision {
  const spec = index.kind(action.kind);
  if (!spec) return denied("reject", "UNKNOWN_KIND", "INTERNAL");
  const perms = actorPermissions(spec);
  const d: Draft = { verb: "reject", allowed: false, reason: "ALLOWED", permissions: perms };
  if (!holdsAny(p, perms, now, index)) {
    d.reason = "MISSING_PERMISSION";
    d.code = "FORBIDDEN";
  } else if (!isUserId(p.subjectId)) {
    d.reason = "SUBJECT_NOT_A_USER";
    d.code = "FORBIDDEN";
  } else if (action.status !== "PROPOSED") {
    d.reason = "WRONG_STATUS";
    d.code = "INVALID_STATE_TRANSITION";
  } else {
    d.allowed = true;
  }
  return finish(d);
}

function decideCancel(p: DecidePrincipal, action: DecideAction): Decision {
  const d: Draft = { verb: "cancel", allowed: false, reason: "ALLOWED" };
  if (!isUserId(p.subjectId)) {
    d.reason = "SUBJECT_NOT_A_USER";
    d.code = "FORBIDDEN";
  } else if (p.subjectId.toLowerCase() !== action.proposedBy.toLowerCase()) {
    d.reason = "NOT_PROPOSER";
    d.code = "FORBIDDEN";
  } else if (action.status !== "PROPOSED") {
    d.reason = "WRONG_STATUS";
    d.code = "INVALID_STATE_TRANSITION";
  } else {
    d.allowed = true;
  }
  return finish(d);
}

function decideExecute(p: DecidePrincipal, action: DecideAction, now: Date, index: AuthorityIndex): Decision {
  const spec = index.kind(action.kind);
  if (!spec) return denied("execute", "UNKNOWN_KIND", "INTERNAL");
  const perms = actorPermissions(spec);
  const d: Draft = {
    verb: "execute",
    allowed: false,
    reason: "ALLOWED",
    permissions: perms,
    stepUpMaxAgeSeconds: spec.step_up_max_age_seconds,
  };
  if (!holdsAny(p, perms, now, index)) {
    d.reason = "MISSING_PERMISSION";
    d.code = "FORBIDDEN";
  } else if (!steppedUp(p, spec.step_up_max_age_seconds, now)) {
    d.reason = "STEP_UP_REQUIRED";
    d.code = "STEP_UP_REQUIRED";
  } else if (!isUserId(p.subjectId)) {
    d.reason = "SUBJECT_NOT_A_USER";
    d.code = "FORBIDDEN";
  } else if (expired(action, now)) {
    d.reason = "EXPIRED";
    d.code = "INVALID_STATE_TRANSITION";
  } else if (action.status === "APPROVED") {
    if (spec.requires_dual && (action.approvedBy === null || action.approvedBy === action.proposedBy)) {
      d.reason = "APPROVER_NOT_DISTINCT";
      d.code = "FORBIDDEN";
    } else {
      d.allowed = true;
    }
  } else if (action.status === "PROPOSED") {
    if (spec.requires_dual) {
      d.reason = "AWAITING_APPROVAL";
      d.code = "INVALID_STATE_TRANSITION";
    } else {
      d.allowed = true;
    }
  } else {
    d.reason = "WRONG_STATUS";
    d.code = "INVALID_STATE_TRANSITION";
  }
  return finish(d);
}

/** Which surfaces this principal may read at `now`. */
export function visibleSurfaces(p: DecidePrincipal, now: Date, index: AuthorityIndex): readonly string[] {
  if (isAbsent(p) || p.actorType === ACTOR_AGENT || validatePrincipal(p, index) !== null) return [];
  return index.doc.surfaces
    .filter((s) => holdsAny(p, s.read_any_of, now, index))
    .map((s) => s.id);
}

/** Which writes on a surface this principal may attempt at `now`. */
export function allowedWrites(
  p: DecidePrincipal,
  surfaceId: string,
  now: Date,
  index: AuthorityIndex,
): readonly string[] {
  const surface = index.surface(surfaceId);
  if (!surface || isAbsent(p) || p.actorType === ACTOR_AGENT || validatePrincipal(p, index) !== null) return [];
  return (surface.writes ?? [])
    .filter((w) => holdsAny(p, w.any_of, now, index))
    .filter((w) => !w.step_up_max_age_seconds || steppedUp(p, w.step_up_max_age_seconds, now))
    .map((w) => w.id);
}
