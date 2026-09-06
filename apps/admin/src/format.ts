/**
 * Wording. This module turns the machine-readable facts the server and the
 * authority document produce into the sentences an operator reads.
 *
 * The rule it exists to enforce: every refusal an operator sees names the real
 * reason. There is no "something went wrong" here and no generic fallback that
 * swallows a code the console has not been taught — an unknown code is shown
 * verbatim, which is ugly and honest, and `format.test.ts` fails when the Go
 * side grows a reason this file does not cover.
 */
import type { Reason } from "./decide.ts";

/** How a refusal is phrased for an operator, per adminplane Reason. */
const REASON_TEXT: Record<Reason, string> = {
  ALLOWED: "Available",
  UNAUTHENTICATED: "You are not signed in.",
  AGENT_PRINCIPAL: "Agents can never reach the operator plane.",
  INVALID_PRINCIPAL: "This session is structurally invalid and grants nothing.",
  SUBJECT_NOT_A_USER: "This session's subject is not a user record, so no action can reference it.",
  UNKNOWN_KIND: "This action kind is not in the closed table of controlled actions.",
  MISSING_PERMISSION: "Your roles do not hold the permission this step needs.",
  STEP_UP_REQUIRED: "This step needs a recent multi-factor sign-in.",
  SELF_APPROVAL: "You proposed this action. Dual control requires a different person to approve it.",
  NOT_PROPOSER: "Only the operator who proposed an action can withdraw it.",
  KIND_TAKES_NO_APPROVAL: "This kind is single-control: there is no approval step to perform.",
  EXPIRED: "This proposal has expired and can no longer move.",
  WRONG_STATUS: "The action's current status does not allow this step.",
  AWAITING_APPROVAL: "This action still needs a second person's approval before it can run.",
  APPROVER_NOT_DISTINCT: "The stored approval does not name someone other than the proposer.",
};

/** Phrasing for one decision reason. */
export function reasonText(reason: string): string {
  return REASON_TEXT[reason as Reason] ?? reason;
}

/** True when the console has wording for every reason the server declares. */
export function coveredReasons(): readonly string[] {
  return Object.keys(REASON_TEXT);
}

/**
 * How an API problem code is explained. Only codes an operator can act on are
 * listed; anything else falls back to the server's own `detail`, which
 * `problemText` prefers over anything invented here.
 */
const CODE_TEXT: Record<string, string> = {
  UNAUTHENTICATED: "Your session has ended. Sign in again.",
  FORBIDDEN: "The server refused this: your roles do not authorise it.",
  STEP_UP_REQUIRED: "The server needs a recent multi-factor sign-in before this step.",
  INVALID_STATE_TRANSITION: "The record moved since this page was loaded. Reload and look again.",
  CONFLICT: "The record changed concurrently. Reload and look again.",
  VALIDATION_FAILED: "The request was rejected as malformed.",
  NOT_FOUND: "No such record.",
  UNSUPPORTED: "This deployment does not have that capability wired.",
  PROVIDER_UNAVAILABLE: "An upstream provider is unavailable, so the server refused rather than guess.",
  RATE_LIMITED: "Too many requests. Wait, then retry.",
  KILL_SWITCH_ACTIVE: "A kill switch is stopping this class of action.",
  RECONCILIATION_REQUIRED: "An open reconciliation mismatch is blocking new risk here.",
  INVALID_IDEMPOTENCY_REUSE: "That idempotency key was already used with a different request body.",
  IDEMPOTENCY_IN_PROGRESS: "The same command is already running. Wait for it rather than retrying.",
  INTERNAL: "The server failed. Nothing was recorded as a conclusion; the request id below identifies it.",
};

export function codeText(code: string): string {
  return CODE_TEXT[code] ?? code;
}

/** Codes the console explains in its own words. */
export function coveredCodes(): readonly string[] {
  return Object.keys(CODE_TEXT);
}

/** Renders an ISO-8601 instant for an operator, in UTC, to the second. */
export function formatInstant(iso: string | null | undefined): string {
  if (!iso) return "—";
  const parsed = new Date(iso);
  if (globalThis.isNaN(parsed.getTime())) return iso;
  return `${parsed.toISOString().slice(0, 19).replace("T", " ")}Z`;
}

/**
 * Renders a signed duration in whole units, for expiries and elevations.
 * Everything the console counts down is minutes-to-hours, so seconds are the
 * finest unit and no rounding decision is hidden.
 */
export function formatDuration(seconds: number): string {
  const negative = seconds < 0;
  let left = Math.floor(Math.abs(seconds));
  const days = Math.floor(left / 86400);
  left -= days * 86400;
  const hours = Math.floor(left / 3600);
  left -= hours * 3600;
  const minutes = Math.floor(left / 60);
  const secs = left - minutes * 60;
  const parts: string[] = [];
  if (days > 0) parts.push(`${days}d`);
  if (hours > 0) parts.push(`${hours}h`);
  if (minutes > 0 && days === 0) parts.push(`${minutes}m`);
  if (parts.length === 0) parts.push(`${secs}s`);
  return `${negative ? "-" : ""}${parts.join(" ")}`;
}

/** Seconds between two instants, or null when either is unreadable. */
export function secondsUntil(iso: string | null | undefined, now: Date): number | null {
  if (!iso) return null;
  const at = new Date(iso).getTime();
  if (globalThis.isNaN(at)) return null;
  return Math.floor((at - now.getTime()) / 1000);
}

/** "in 3h" / "12m ago" / "—" for an instant relative to now. */
export function relativeInstant(iso: string | null | undefined, now: Date): string {
  const secs = secondsUntil(iso, now);
  if (secs === null) return "—";
  return secs >= 0 ? `in ${formatDuration(secs)}` : `${formatDuration(-secs)} ago`;
}

/** Turns an UPPER_SNAKE identifier into Title Case for a heading. */
export function titleCase(value: string): string {
  return value
    .toLowerCase()
    .split(/[_\s]+/)
    .filter((w) => w.length > 0)
    .map((w) => `${w.charAt(0).toUpperCase()}${w.slice(1)}`)
    .join(" ");
}

/** A short, stable label for an admin action kind. */
export function kindLabel(kind: string): string {
  return titleCase(kind);
}
