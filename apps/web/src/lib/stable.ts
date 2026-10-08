/**
 * Pure helpers for the stable asset section of the advanced area
 * (internal/stableasset).
 *
 * Nothing here computes a financial figure. Every amount the section shows is
 * a string the stable asset controller computed; these functions name
 * statuses, classify what a status says about a request's outcome, move a
 * decimal point through an integer's own digits for display, check what a
 * person typed, and decide which idempotency key a confirmed command carries.
 *
 * The enumerations repeat values the OpenAPI contract declares, because the
 * runtime validator needs them as values; each is tied to its generated type
 * at compile time, so a contract change fails `tsc` here.
 *
 * One reserve kind's wire token begins with a word the honesty rules ban as a
 * word (PART 112). The token itself is the contract's and is matched exactly;
 * on screen the kind is called a dollar-equivalent reserve.
 */
import type { Schemas } from "@controlplane/generated-client";

import { MoneyFormatError } from "./money.ts";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

export type Tone = "neutral" | "good" | "warn" | "bad" | "info";

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export type StableAssetStatus = Schemas["StableAssetStatus"];
export const STABLE_ASSET_STATUSES = ["ACTIVE", "PAUSED", "RETIRED"] as const;
export type StableAssetStatusesMatchContract = Assert<Same<(typeof STABLE_ASSET_STATUSES)[number], StableAssetStatus>>;

export type StableReserveKind = Schemas["StableReserveKind"];
/** The reserve kind that pays for an issuance and is paid by a redemption, counted at most at par. */
export const DOLLAR_EQUIVALENT_RESERVE = "CASH_EQUIVALENT";
export const TOKENIZED_RESERVE = "TOKENIZED";
export const STABLE_RESERVE_KINDS = [DOLLAR_EQUIVALENT_RESERVE, TOKENIZED_RESERVE] as const;
export type StableReserveKindsMatchContract = Assert<Same<(typeof STABLE_RESERVE_KINDS)[number], StableReserveKind>>;

export type StableRedemptionMode = Schemas["StableRedemptionMode"];
export const STABLE_REDEMPTION_MODES = ["INSTANT", "QUEUED"] as const;
export type StableRedemptionModesMatchContract = Assert<
  Same<(typeof STABLE_REDEMPTION_MODES)[number], StableRedemptionMode>
>;

export type StableRequestMode = Schemas["StableRequestMode"];
export const STABLE_REQUEST_MODES = ["SANDBOX", "DEVNET"] as const;
export type StableRequestModesMatchContract = Assert<Same<(typeof STABLE_REQUEST_MODES)[number], StableRequestMode>>;

export type StableIssuanceStatus = Schemas["StableIssuanceStatus"];
export const STABLE_ISSUANCE_STATUSES = ["SUBMITTING", "SUBMITTED", "SUBMISSION_UNKNOWN", "ISSUED", "FAILED"] as const;
export type StableIssuanceStatusesMatchContract = Assert<
  Same<(typeof STABLE_ISSUANCE_STATUSES)[number], StableIssuanceStatus>
>;

export type StableRedemptionStatus = Schemas["StableRedemptionStatus"];
export const STABLE_REDEMPTION_STATUSES = [
  "SUBMITTING",
  "SUBMITTED",
  "SUBMISSION_UNKNOWN",
  "REDEMPTION_QUEUED",
  "REDEEMED",
  "FAILED",
] as const;
export type StableRedemptionStatusesMatchContract = Assert<
  Same<(typeof STABLE_REDEMPTION_STATUSES)[number], StableRedemptionStatus>
>;

export type StableReconciliationState = Schemas["StableReconciliationState"];
export const STABLE_RECONCILIATION_STATES = ["NOT_REQUIRED", "AWAITING", "RESOLVED"] as const;
export type StableReconciliationStatesMatchContract = Assert<
  Same<(typeof STABLE_RECONCILIATION_STATES)[number], StableReconciliationState>
>;

export type StableEffectStatus = Schemas["StableEffectStatus"];
export const STABLE_EFFECT_STATUSES = ["PENDING", "APPLIED", "UNKNOWN", "FAILED", "COMPENSATED"] as const;
export type StableEffectStatusesMatchContract = Assert<Same<(typeof STABLE_EFFECT_STATUSES)[number], StableEffectStatus>>;

export type StableAlertKind = Schemas["StableAlertKind"];
export const STABLE_ALERT_KINDS = [
  "PRICE_DEVIATION",
  "PRICE_STALE",
  "RESERVE_SHORTFALL",
  "RESERVE_PRICE_STALE",
  "SUPPLY_MISMATCH",
] as const;
export type StableAlertKindsMatchContract = Assert<Same<(typeof STABLE_ALERT_KINDS)[number], StableAlertKind>>;

export type StableAlertSeverity = Schemas["StableAlertSeverity"];
export const STABLE_ALERT_SEVERITIES = ["WARNING", "CRITICAL"] as const;
export type StableAlertSeveritiesMatchContract = Assert<
  Same<(typeof STABLE_ALERT_SEVERITIES)[number], StableAlertSeverity>
>;

export type StableSnapshotFailure = Schemas["StableSnapshotFailure"];
export const STABLE_SNAPSHOT_FAILURES = [
  "SNAPSHOT_BODY_NOT_CANONICAL",
  "SNAPSHOT_HASH_MISMATCH",
  "SNAPSHOT_SIGNATURE_INVALID",
  "SNAPSHOT_CHAIN_BROKEN",
  "SNAPSHOT_KEY_MISMATCH",
  "SNAPSHOT_DISCLAIMER_MISSING",
] as const;
export type StableSnapshotFailuresMatchContract = Assert<
  Same<(typeof STABLE_SNAPSHOT_FAILURES)[number], StableSnapshotFailure>
>;

/* --------------------------------------------------------------------------
 * What statuses mean
 * ------------------------------------------------------------------------ */

export const ASSET_STATUS_MEANING: Readonly<Record<StableAssetStatus, string>> = {
  ACTIVE: "Issuing and redeeming.",
  PAUSED:
    "Neither issuing nor redeeming: an operator or the circuit breaker paused it while a peg, reserve or supply problem is looked at. Only an operator resumes it.",
  RETIRED: "Issuing nothing new; redemptions continue until nothing is outstanding.",
};

export function assetStatusTone(status: string): Tone {
  switch (status) {
    case "ACTIVE":
      return "good";
    case "PAUSED":
      return "warn";
    default:
      return "neutral";
  }
}

/** What a reserve kind is, in words this interface may use. */
export function reserveKindLabel(kind: string): string {
  return kind === TOKENIZED_RESERVE ? "Tokenized reserve" : "Dollar-equivalent reserve";
}

export function reserveKindNote(kind: string): string {
  return kind === TOKENIZED_RESERVE
    ? "A tokenized simulated instrument, counted at its verified price times its eligibility factor. It over-collateralises; it never pays for an issuance or a redemption."
    : "A simulated dollar or equivalent, counted at the lower of its verified price and par, times its eligibility factor. Only this kind pays for an issuance or is paid out by a redemption.";
}

export const REDEMPTION_MODE_MEANING: Readonly<Record<StableRedemptionMode, string>> = {
  INSTANT: "A redemption posts as soon as its burn is final on the simulated chain.",
  QUEUED: "A final burn is queued; its payout posts when an operator runs the redemption queue.",
};

export const REQUEST_MODE_MEANING: Readonly<Record<StableRequestMode, string>> = {
  SANDBOX:
    "The platform produces the simulated chain's blocks and drives the request to a final state before it answers.",
  DEVNET:
    "The request is only observed: it finalizes as the simulated chain's blocks arrive, so the first answer is usually not final.",
};

export const ISSUANCE_STATUS_MEANING: Readonly<Record<StableIssuanceStatus, string>> = {
  SUBMITTING: "Signed and recorded; the submission is about to happen, or happened and is not yet recorded.",
  SUBMITTED: "The simulated chain accepted the transaction; it is not final yet.",
  SUBMISSION_UNKNOWN:
    "The submission's outcome is unknown. It is never resubmitted: it is settled from the chain by its transaction id.",
  ISSUED: "Final on the simulated chain and STABLE_ISSUANCE posted.",
  FAILED: "Failed on chain, expired or refused: the payment's reservation was released and nothing was posted.",
};

export const REDEMPTION_STATUS_MEANING: Readonly<Record<StableRedemptionStatus, string>> = {
  SUBMITTING: "Signed and recorded; the submission is about to happen, or happened and is not yet recorded.",
  SUBMITTED: "The simulated chain accepted the burn; it is not final yet.",
  SUBMISSION_UNKNOWN:
    "The submission's outcome is unknown. It is never resubmitted: it is settled from the chain by its transaction id.",
  REDEMPTION_QUEUED: "The burn is final on the simulated chain; its payout waits for the operator's queue run.",
  REDEEMED: "Final on the simulated chain and STABLE_REDEMPTION posted.",
  FAILED: "Failed on chain, expired or refused: the units' reservation was released and nothing was posted.",
};

export const RECONCILIATION_STATE_MEANING: Readonly<Record<StableReconciliationState, string>> = {
  NOT_REQUIRED: "The submission's outcome was never unknown.",
  AWAITING:
    "The submission's outcome is unknown and waits to be settled from the simulated chain by transaction id. It is not a success and not a failure.",
  RESOLVED: "The submission's outcome was unknown and has since been settled from the simulated chain.",
};

export function reconciliationTone(state: string): Tone {
  switch (state) {
    case "AWAITING":
      return "warn";
    case "RESOLVED":
      return "info";
    default:
      return "neutral";
  }
}

export const ALERT_KIND_MEANING: Readonly<Record<StableAlertKind, string>> = {
  PRICE_DEVIATION: "The market price moved further from the peg than the asset's threshold.",
  PRICE_STALE: "No fresh market price from the asset's oracle source: the monitor could not see the peg.",
  RESERVE_SHORTFALL: "The verified eligible reserve did not cover issued supply at the peg.",
  RESERVE_PRICE_STALE: "A reserve asset held a balance with no fresh price, so it counted zero.",
  SUPPLY_MISMATCH: "The books, the holdings and the simulated chain's supply disagreed.",
};

export function alertTone(severity: string): Tone {
  return severity === "CRITICAL" ? "bad" : "warn";
}

export const SNAPSHOT_FAILURE_MEANING: Readonly<Record<StableSnapshotFailure, string>> = {
  SNAPSHOT_BODY_NOT_CANONICAL: "The stored body does not re-encode to exactly its canonical bytes.",
  SNAPSHOT_HASH_MISMATCH: "The body does not hash to the recorded hash.",
  SNAPSHOT_SIGNATURE_INVALID: "The signature is not a valid Ed25519 signature by the snapshot signer over the hash.",
  SNAPSHOT_CHAIN_BROKEN: "The snapshot does not name the previous snapshot's hash, or its sequence is out of order.",
  SNAPSHOT_KEY_MISMATCH: "The snapshot was signed by a key other than the snapshot signer's.",
  SNAPSHOT_DISCLAIMER_MISSING: "The body does not carry the required statement of what a snapshot is.",
};

/* --------------------------------------------------------------------------
 * Outcomes: what a request's status says, never more
 * ------------------------------------------------------------------------ */

/**
 * The outcome a request's status states. UNKNOWN is its own outcome: a
 * submission whose outcome is unknown is shown as unknown, never as a
 * success or a failure, until reconciliation settles it from the chain.
 */
export type RequestOutcome = "COMPLETED" | "FAILED" | "IN_PROGRESS" | "UNKNOWN";

export function issuanceOutcome(status: string, reconciliationState: string): RequestOutcome {
  if (status === "SUBMISSION_UNKNOWN" || reconciliationState === "AWAITING") return "UNKNOWN";
  if (status === "ISSUED") return "COMPLETED";
  if (status === "FAILED") return "FAILED";
  return "IN_PROGRESS";
}

export function redemptionOutcome(status: string, reconciliationState: string): RequestOutcome {
  if (status === "SUBMISSION_UNKNOWN" || reconciliationState === "AWAITING") return "UNKNOWN";
  if (status === "REDEEMED") return "COMPLETED";
  if (status === "FAILED") return "FAILED";
  return "IN_PROGRESS";
}

export function outcomeTone(outcome: RequestOutcome): Tone {
  switch (outcome) {
    case "COMPLETED":
      return "good";
    case "FAILED":
      return "bad";
    case "UNKNOWN":
      return "warn";
    default:
      return "info";
  }
}

/** The words for an outcome. `done` names what completion is: issued, redeemed. */
export function outcomeLabel(outcome: RequestOutcome, done: string): string {
  switch (outcome) {
    case "COMPLETED":
      return done;
    case "FAILED":
      return "Failed";
    case "UNKNOWN":
      return "Outcome unknown";
    default:
      return "Not final yet";
  }
}

export function requestStatusTone(status: string): Tone {
  switch (status) {
    case "ISSUED":
    case "REDEEMED":
      return "good";
    case "FAILED":
      return "bad";
    case "SUBMISSION_UNKNOWN":
      return "warn";
    default:
      return "info";
  }
}

/* --------------------------------------------------------------------------
 * Display, by moving a decimal point
 * ------------------------------------------------------------------------ */

const INTEGER_TEXT = /^-?[0-9]+$/;

/**
 * Basis points sent as a string (a snapshot body states every number as a
 * string), as a percentage: "2500" -> "25%", "1234" -> "12.34%". The point is
 * moved through the digits; nothing is divided.
 */
export function formatBpsText(text: string): string {
  if (!INTEGER_TEXT.test(text)) {
    throw new MoneyFormatError(`bps: ${JSON.stringify(text)} is not an integer string`);
  }
  const negative = text.startsWith("-");
  const digits = (negative ? text.slice(1) : text).padStart(3, "0");
  const cut = digits.length - 2;
  let whole = digits.slice(0, cut);
  while (whole.length > 1 && whole.startsWith("0")) whole = whole.slice(1);
  let fraction = digits.slice(cut);
  while (fraction.endsWith("0")) fraction = fraction.slice(0, fraction.length - 1);
  const body = fraction === "" ? `${whole}%` : `${whole}.${fraction}%`;
  return negative && body !== "0%" ? `-${body}` : body;
}

/* --------------------------------------------------------------------------
 * What a person typed
 * ------------------------------------------------------------------------ */

export type Parsed<T> = { readonly ok: true; readonly value: T } | { readonly ok: false; readonly error: string };

const DIGITS = "0123456789";

/** A whole number typed in digits, within [min, max]. Built digit by digit; nothing is parsed as a float. */
export function parseWholeNumber(raw: string, min: number, max: number, what: string): Parsed<number> {
  const text = raw.trim();
  if (!/^[0-9]{1,9}$/.test(text)) {
    return { ok: false, error: `${what} is a whole number written in digits.` };
  }
  let value = 0;
  for (const ch of text) {
    value = value * 10 + DIGITS.indexOf(ch);
  }
  if (value < min || value > max) {
    return { ok: false, error: `${what} must be between ${String(min)} and ${String(max)}.` };
  }
  return { ok: true, value };
}

/**
 * A positive decimal exactly as typed: digits with an optional fraction, no
 * sign, no grouping, no exponent. It is kept as text and sent as text.
 */
export function parsePositiveDecimal(raw: string, what: string): Parsed<string> {
  const text = raw.trim();
  if (!/^[0-9]+(\.[0-9]+)?$/.test(text)) {
    return { ok: false, error: `${what} is a decimal written in digits, with an optional fraction and no sign or grouping.` };
  }
  if (/^0+(\.0+)?$/.test(text)) {
    return { ok: false, error: `${what} must be greater than zero.` };
  }
  return { ok: true, value: text };
}

/** Base units typed as an integer string, kept exactly. */
export function parseBaseUnits(raw: string, what: string): Parsed<string> {
  const text = raw.trim();
  if (!/^[0-9]+$/.test(text)) {
    return { ok: false, error: `${what} is a whole number of base units written in digits.` };
  }
  let start = 0;
  while (start < text.length - 1 && text[start] === "0") start = start + 1;
  const value = text.slice(start);
  if (value === "0") {
    return { ok: false, error: `${what} must be greater than zero.` };
  }
  return { ok: true, value };
}

/** A generic name of the kind the registry takes: 1..63 bytes of [a-z0-9-], starting with a letter or digit. */
export function parseRegistryName(raw: string, what: string): Parsed<string> {
  const text = raw.trim();
  if (!/^[a-z0-9][a-z0-9-]{0,62}$/.test(text)) {
    return {
      ok: false,
      error: `${what} is 1 to 63 lowercase letters, digits and hyphens, starting with a letter or digit.`,
    };
  }
  return { ok: true, value: text };
}

/** Free text an operator records as the reason for a decision. */
export function parseReason(raw: string): Parsed<string> {
  const text = raw.trim();
  if (text.length < 3) {
    return { ok: false, error: "Say why, in a few words: the reason is recorded with the decision." };
  }
  return { ok: true, value: text };
}

/* --------------------------------------------------------------------------
 * Commands: one idempotency key per confirmed request
 * ------------------------------------------------------------------------ */

/** A request the user confirmed, with the key that makes it exactly once. */
export interface ConfirmedAttempt<R> {
  readonly request: R;
  readonly idempotencyKey: string;
}

/**
 * The attempt a confirmation sends. A key is created at the moment of
 * confirmation and only then: confirming the same request again (a retry
 * after a failure, a double click) reuses the attempt's key, so the backend
 * answers it as one request; confirming a different request starts a new
 * attempt with a new key.
 */
export function confirmAttempt<R>(
  current: ConfirmedAttempt<R> | undefined,
  request: R,
  newKey: () => string,
): ConfirmedAttempt<R> {
  if (current !== undefined && sameRequest(current.request, request)) {
    return current;
  }
  return { request, idempotencyKey: newKey() };
}

/** Two requests are the same when every field the backend would receive is the same. */
export function sameRequest<R>(a: R, b: R): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

/* --------------------------------------------------------------------------
 * The snapshot chain
 * ------------------------------------------------------------------------ */

interface VerificationLike {
  readonly verified: boolean;
  readonly checked: number;
  readonly failed_index?: number;
  readonly reason?: string;
}

/** The chain's verification in one sentence, from the backend's own result. */
export function verificationSummary(v: VerificationLike): string {
  if (v.checked === 0) {
    return "No snapshot has been published yet, so there is nothing to verify.";
  }
  if (v.verified) {
    return `All ${String(v.checked)} snapshot${v.checked === 1 ? "" : "s"} of the chain verify against the snapshot signer's public key.`;
  }
  const position = v.failed_index === undefined ? "One snapshot" : `The snapshot at position ${String(v.failed_index + 1)} (oldest first)`;
  const reason = v.reason === undefined ? "" : ` (${v.reason})`;
  return `${position} of ${String(v.checked)} does not verify${reason}; it and every snapshot after it are unverified.`;
}

/** Says how many of the chain's snapshots the page shows. */
export function snapshotsShownNote(total: number, shown: number): string {
  if (total === shown) return `All ${String(total)} snapshot${total === 1 ? "" : "s"} of the chain are shown, oldest first.`;
  return `The newest ${String(shown)} of ${String(total)} snapshots are shown, oldest first; the verification covers the whole chain.`;
}
