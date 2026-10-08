/**
 * Pure helpers for the agent authority section of the advanced area:
 * delegated agent signing and delegated agent withdrawals (ADR-0020,
 * internal/delegation behind /v1/advanced/delegation).
 *
 * Nothing here decides anything. The backend decides every rule -- the
 * capability's scope, limits and expiry, the transaction inspector, the
 * signing policy, the withdrawal policy, who may decide what -- and these
 * functions only name statuses, say what each one means, check that what a
 * person typed has the shape the API accepts, and build the request exactly
 * as typed. An agent never reaches the API this section talks to: its
 * requests are admitted inside the platform, and every page here is the
 * account owner's or an operator's view of them.
 *
 * The enumerations repeat values the OpenAPI contract declares, because the
 * runtime validator needs them as values; each is tied to its generated type
 * at compile time (`Same`), so a contract change fails `tsc` here.
 */
import type { Schemas } from "@controlplane/generated-client";

import type { AdvancedMode, Tone } from "./advanced.ts";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export type InstrumentClass = Schemas["DelegationInstrumentClass"];
export const INSTRUMENT_CLASSES = ["STABLE_VALUE", "SPOT_TOKEN", "SPECULATIVE_TOKEN"] as const;
export type InstrumentClassesMatchContract = Assert<Same<(typeof INSTRUMENT_CLASSES)[number], InstrumentClass>>;

export type ActionClass = Schemas["DelegationActionClass"];
export const ACTION_CLASSES = ["ACQUIRE_EXPOSURE", "REDUCE_EXPOSURE", "PROPOSE_WITHDRAWAL"] as const;
export type ActionClassesMatchContract = Assert<Same<(typeof ACTION_CLASSES)[number], ActionClass>>;

export type DelegationChain = Schemas["DelegationChain"];
export const CHAINS = ["sbx-solana", "sbx-ethereum"] as const;
export type ChainsMatchContract = Assert<Same<(typeof CHAINS)[number], DelegationChain>>;

export type CapabilityStatus = Schemas["DelegationCapabilityStatus"];
export const CAPABILITY_STATUSES = ["ACTIVE", "REVOKED"] as const;
export type CapabilityStatusesMatchContract = Assert<Same<(typeof CAPABILITY_STATUSES)[number], CapabilityStatus>>;

export type CapabilityStanding = Schemas["DelegationCapabilityStanding"];
export const CAPABILITY_STANDINGS = ["ACTIVE", "EXPIRED", "REVOKED"] as const;
export type CapabilityStandingsMatchContract = Assert<Same<(typeof CAPABILITY_STANDINGS)[number], CapabilityStanding>>;

export type SwapStatus = Schemas["DelegationSwapStatus"];
export const SWAP_STATUSES = [
  "ADMITTED",
  "AWAITING_APPROVAL",
  "APPROVED",
  "SIGNED",
  "SUBMITTED",
  "UNKNOWN",
  "SETTLED",
  "REJECTED",
  "CANCELLED",
  "FAILED",
] as const;
export type SwapStatusesMatchContract = Assert<Same<(typeof SWAP_STATUSES)[number], SwapStatus>>;

export type WithdrawalStatus = Schemas["DelegationWithdrawalStatus"];
export const WITHDRAWAL_STATUSES = [
  "PROPOSED",
  "AWAITING_APPROVAL",
  "APPROVED",
  "SIGNED",
  "SUBMITTED",
  "UNKNOWN",
  "SETTLED",
  "REJECTED",
  "DECLINED",
  "CANCELLED",
  "FAILED",
] as const;
export type WithdrawalStatusesMatchContract = Assert<Same<(typeof WITHDRAWAL_STATUSES)[number], WithdrawalStatus>>;

export type DestinationStatus = Schemas["DelegationDestinationStatus"];
export const DESTINATION_STATUSES = ["ACTIVE", "REMOVED"] as const;
export type DestinationStatusesMatchContract = Assert<Same<(typeof DESTINATION_STATUSES)[number], DestinationStatus>>;

export type Decision = Schemas["DelegationDecision"];
export const DECISIONS = ["APPROVE", "DECLINE"] as const;
export type DecisionsMatchContract = Assert<Same<(typeof DECISIONS)[number], Decision>>;

export type Verdict = Schemas["DelegationVerdict"];
export const VERDICTS = ["APPROVED", "REJECTED"] as const;
export type VerdictsMatchContract = Assert<Same<(typeof VERDICTS)[number], Verdict>>;

export type EvaluationOutcome = Schemas["DelegationEvaluationOutcome"];
export const EVALUATION_OUTCOMES = ["AUTO_APPROVE", "HUMAN_REQUIRED"] as const;
export type EvaluationOutcomesMatchContract = Assert<Same<(typeof EVALUATION_OUTCOMES)[number], EvaluationOutcome>>;

export type SubjectKind = Schemas["DelegationSubjectKind"];
export const SUBJECT_KINDS = ["SWAP", "WITHDRAWAL"] as const;
export type SubjectKindsMatchContract = Assert<Same<(typeof SUBJECT_KINDS)[number], SubjectKind>>;

/**
 * The modes delegated authority exists in: SANDBOX and DEVNET, the database's
 * own check. A capability, swap or withdrawal that claims another mode is
 * refused as a contract violation rather than drawn.
 */
export const DELEGATION_MODES = ["SANDBOX", "DEVNET"] as const satisfies readonly AdvancedMode[];
export type DelegationMode = (typeof DELEGATION_MODES)[number];

/* --------------------------------------------------------------------------
 * What things mean
 * ------------------------------------------------------------------------ */

export const INSTRUMENT_CLASS_LABEL: Readonly<Record<InstrumentClass, string>> = {
  STABLE_VALUE: "USD-pegged simulated units",
  SPOT_TOKEN: "Ordinary simulated tokens",
  SPECULATIVE_TOKEN: "Speculative simulated tokens",
};

export const ACTION_CLASS_LABEL: Readonly<Record<ActionClass, string>> = {
  ACQUIRE_EXPOSURE: "Swap into an asset",
  REDUCE_EXPOSURE: "Swap out of an asset",
  PROPOSE_WITHDRAWAL: "Propose a withdrawal (never execute one)",
};

export const CHAIN_LABEL: Readonly<Record<DelegationChain, string>> = {
  "sbx-solana": "sbx-solana (simulated chain)",
  "sbx-ethereum": "sbx-ethereum (simulated chain)",
};

export const STANDING_MEANING: Readonly<Record<CapabilityStanding, string>> = {
  ACTIVE: "In force: the agent may ask within its scope and limits until it expires.",
  EXPIRED: "Past its expiry. Nothing more is admitted or signed under it; expiry is decided against the clock.",
  REVOKED: "Revoked. Nothing not yet signed under it can be signed afterwards.",
};

export function standingTone(standing: string): Tone {
  switch (standing) {
    case "ACTIVE":
      return "good";
    case "EXPIRED":
      return "warn";
    case "REVOKED":
      return "bad";
    default:
      return "neutral";
  }
}

export const SWAP_STATUS_MEANING: Readonly<Record<SwapStatus, string>> = {
  ADMITTED: "Admitted under the capability; funds reserved and the plan recorded. Nothing is signed yet.",
  AWAITING_APPROVAL: "Waiting for the account owner to approve or decline this exact plan.",
  APPROVED: "Approved by the owner; it is signed only after the inspector proves the transaction is this plan.",
  SIGNED: "Inspected and signed by the delegated wallet and the venue; submitted exactly once.",
  SUBMITTED: "Accepted by the simulated chain; waiting for finality.",
  UNKNOWN: "The submission's outcome is unknown. It is reconciled by transaction id, never submitted again.",
  SETTLED: "Finalized on the simulated chain, reconciled against the signed transaction and posted.",
  REJECTED: "Refused before signing: the code says why. The reserved funds were returned.",
  CANCELLED: "Declined by the account owner. The reserved funds were returned.",
  FAILED: "Signed but never settled: the chain refused it or it expired. The reserved funds were returned.",
};

export const WITHDRAWAL_STATUS_MEANING: Readonly<Record<WithdrawalStatus, string>> = {
  PROPOSED: "Proposed by the agent and being evaluated.",
  AWAITING_APPROVAL: "Waiting for the account owner to approve or decline this exact plan.",
  APPROVED: "Approved: it is signed once its cooling period has passed and the inspector proves the transaction.",
  SIGNED: "Inspected and signed by the delegated wallet; submitted exactly once.",
  SUBMITTED: "Accepted by the simulated chain; waiting for finality.",
  UNKNOWN: "The submission's outcome is unknown. It is reconciled by transaction id, never submitted again.",
  SETTLED: "Finalized on the simulated chain, reconciled and posted. The simulated value left the account.",
  REJECTED: "Refused before signing: the code says why. The reserved funds were returned.",
  DECLINED: "Declined by the account owner. The reserved funds were returned.",
  CANCELLED: "Cancelled by the account owner before it was signed. The reserved funds were returned.",
  FAILED: "Signed but never settled. The reserved funds were returned.",
};

export function subjectStatusTone(status: string): Tone {
  switch (status) {
    case "SETTLED":
      return "good";
    case "REJECTED":
    case "FAILED":
      return "bad";
    case "DECLINED":
    case "CANCELLED":
      return "neutral";
    case "UNKNOWN":
    case "AWAITING_APPROVAL":
      return "warn";
    default:
      return "info";
  }
}

export function verdictTone(verdict: string): Tone {
  return verdict === "APPROVED" ? "good" : verdict === "REJECTED" ? "bad" : "neutral";
}

export const EVALUATION_OUTCOME_MEANING: Readonly<Record<EvaluationOutcome, string>> = {
  AUTO_APPROVE:
    "Approved by the account's policy without a person. That happens only in a test environment, with every " +
    "automatic bound of the policy met.",
  HUMAN_REQUIRED: "The account's policy requires the owner's decision; the reason codes say why.",
};

/* --------------------------------------------------------------------------
 * What may be asked of a subject now
 * ------------------------------------------------------------------------ */

/** A swap or a withdrawal takes a decision only while it awaits one. */
export function awaitsDecision(status: string): boolean {
  return status === "AWAITING_APPROVAL";
}

export function noDecisionReason(status: string): string {
  return `Only a request AWAITING_APPROVAL takes a decision, and this one is ${status}.`;
}

/** A withdrawal can be cancelled until it is signed. */
export function cancellable(status: string): boolean {
  return status === "AWAITING_APPROVAL" || status === "APPROVED";
}

export function notCancellableReason(status: string): string {
  return `Only a withdrawal AWAITING_APPROVAL or APPROVED can be cancelled, and this one is ${status}.`;
}

/* --------------------------------------------------------------------------
 * What a person typed
 * ------------------------------------------------------------------------ */

export interface InputCheck<T = string> {
  readonly ok: boolean;
  /** The value to send when `ok`. */
  readonly value: T;
  /** A message to show under the field when not `ok`. */
  readonly error: string;
}

const USD_INPUT = /^[0-9]{1,13}\.[0-9]{2}$/;
const ADDRESS = /^[1-9A-HJ-NP-Za-km-z]{32,44}$/;
const UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
const PLAN_HASH = /^[0-9a-f]{64}$/;
const DIGITS = "0123456789";

/**
 * A sandbox-dollar amount exactly as the contract's USD shape takes it:
 * digits, a point and exactly two digits of cents ("250.00"). It is sent as
 * typed; whether it is a sensible limit is the backend's to say.
 */
export function checkUsd(raw: string, what: string): InputCheck {
  const value = raw.trim().replace(/,/g, "");
  if (value === "") return { ok: false, value: "", error: `Enter ${what}.` };
  if (!USD_INPUT.test(value)) {
    return { ok: false, value: "", error: `Write ${what} with exactly two digits of cents, such as 250 dollars written with ".00".` };
  }
  return { ok: true, value, error: "" };
}

/** A simulated-chain address: base58, 32 to 44 characters. Whether it names anything is the backend's answer. */
export function checkAddress(raw: string): InputCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: "Enter an address." };
  if (!ADDRESS.test(value)) {
    return { ok: false, value: "", error: "An address on a simulated chain is 32 to 44 base58 characters." };
  }
  return { ok: true, value, error: "" };
}

/**
 * A list of addresses, one per line or separated by commas or spaces, in the
 * order written and without repeats. Every one must have the address shape.
 */
export function checkAddressList(raw: string): InputCheck<readonly string[]> {
  const parts = raw
    .split(/[\s,]+/)
    .map((part) => part.trim())
    .filter((part) => part !== "");
  if (parts.length === 0) return { ok: false, value: [], error: "Name at least one destination address." };
  const seen = new Set<string>();
  const out: string[] = [];
  for (const part of parts) {
    if (!ADDRESS.test(part)) {
      return { ok: false, value: [], error: `"${part}" is not a simulated-chain address (32 to 44 base58 characters).` };
    }
    if (!seen.has(part)) {
      seen.add(part);
      out.push(part);
    }
  }
  if (out.length > 64) return { ok: false, value: [], error: "A capability names at most 64 destinations." };
  return { ok: true, value: out, error: "" };
}

/** An id the backend gave: a canonical UUID. */
export function checkUuid(raw: string, what: string): InputCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: `Enter ${what}.` };
  if (!UUID.test(value)) return { ok: false, value: "", error: `${what} is a UUID in its canonical 36-character form.` };
  return { ok: true, value: value.toLowerCase(), error: "" };
}

/** Non-empty text within the backend's bound, trimmed: a reason, a label. */
export function checkText(raw: string, what: string, max: number): InputCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: `Enter ${what}.` };
  if (value.length > max) return { ok: false, value: "", error: `${what} is at most ${String(max)} characters.` };
  return { ok: true, value, error: "" };
}

/**
 * A whole number within [min, max] -- seconds, a count, a score, basis points;
 * never an amount. The digits are accumulated one by one (at most nine, far
 * inside exact integer range) rather than handed to a numeric parser.
 */
export function checkWholeNumber(raw: string, min: number, max: number, what: string): InputCheck<number> {
  const cleaned = raw.trim().replace(/,/g, "");
  if (!/^[0-9]{1,9}$/.test(cleaned)) return { ok: false, value: 0, error: `Enter ${what} as a whole number.` };
  let value = 0;
  for (const ch of cleaned) value = value * 10 + DIGITS.indexOf(ch);
  if (value < min || value > max) {
    return { ok: false, value: 0, error: `Use ${what} from ${String(min)} to ${String(max)}.` };
  }
  return { ok: true, value, error: "" };
}

/** True when the text is a plan hash as the reads render one. */
export function isPlanHash(value: string): boolean {
  return PLAN_HASH.test(value);
}

/** The instant `days` whole days after `now`, as the API's timestamp. The backend bounds a grant to 90 days. */
export function expiryAfterDays(days: number, now: Date): string {
  return new Date(now.getTime() + days * 86_400_000).toISOString();
}

/* --------------------------------------------------------------------------
 * Requests, built exactly as typed
 * ------------------------------------------------------------------------ */

export interface GrantDraft {
  readonly agentId: string;
  readonly mode: DelegationMode;
  readonly instrumentClasses: readonly InstrumentClass[];
  readonly actionClasses: readonly ActionClass[];
  readonly maxNotional: string;
  readonly maxDailyNotional: string;
  readonly maxSingleTrade: string;
  readonly destinations: string;
  readonly chains: readonly DelegationChain[];
  readonly days: string;
  readonly requiresHumanApproval: boolean;
}

export type GrantBody = Schemas["DelegationGrantRequest"];

export interface GrantCheck {
  readonly ok: boolean;
  /** One message per field that is not ready, keyed by field. */
  readonly problems: Readonly<Record<string, string>>;
}

/** The first message of each unready field. Only shapes are judged; every rule is the backend's. */
export function checkGrant(draft: GrantDraft): GrantCheck {
  const problems: Record<string, string> = {};
  const agent = checkUuid(draft.agentId, "the agent's id");
  if (!agent.ok) problems["agent"] = agent.error;
  if (draft.instrumentClasses.length === 0) problems["instrumentClasses"] = "Allow at least one kind of asset.";
  if (draft.actionClasses.length === 0) problems["actionClasses"] = "Allow at least one action.";
  if (draft.chains.length === 0) problems["chains"] = "Allow at least one simulated chain.";
  for (const [field, raw, what] of [
    ["maxNotional", draft.maxNotional, "the lifetime limit"],
    ["maxDailyNotional", draft.maxDailyNotional, "the daily limit"],
    ["maxSingleTrade", draft.maxSingleTrade, "the single-request limit"],
  ] as const) {
    const usd = checkUsd(raw, what);
    if (!usd.ok) problems[field] = usd.error;
  }
  const destinations = checkAddressList(draft.destinations);
  if (!destinations.ok) problems["destinations"] = destinations.error;
  const days = checkWholeNumber(draft.days, 1, 90, "the number of days");
  if (!days.ok) problems["days"] = days.error;
  return { ok: Object.keys(problems).length === 0, problems };
}

/** The grant as the API takes it. Call only when `checkGrant` is ok. */
export function grantBody(accountId: string, draft: GrantDraft, now: Date): GrantBody {
  const days = checkWholeNumber(draft.days, 1, 90, "the number of days");
  return {
    account_id: accountId,
    agent_id: checkUuid(draft.agentId, "the agent's id").value,
    mode: draft.mode,
    instrument_classes: [...draft.instrumentClasses],
    action_classes: [...draft.actionClasses],
    max_notional_usd: checkUsd(draft.maxNotional, "").value,
    max_daily_notional_usd: checkUsd(draft.maxDailyNotional, "").value,
    max_single_trade_usd: checkUsd(draft.maxSingleTrade, "").value,
    destination_allowlist: [...checkAddressList(draft.destinations).value],
    chain_allowlist: [...draft.chains],
    expires_at: expiryAfterDays(days.value, now),
    requires_human_approval: draft.requiresHumanApproval,
  };
}

/** Toggles a value in a set-like list, keeping the contract's order. */
export function toggled<T extends string>(order: readonly T[], current: readonly T[], value: T): T[] {
  const has = current.includes(value);
  return order.filter((item) => (item === value ? !has : current.includes(item)));
}

export interface PolicyDraft {
  readonly autoApproveEnabled: boolean;
  readonly autoApproveMax: string;
  readonly autoApproveMaxRiskScore: string;
  readonly maxRiskScore: string;
  readonly perWithdrawalLimit: string;
  readonly dailyLimit: string;
  readonly velocityMaxCount: string;
  readonly minDestinationAgeSeconds: string;
  readonly minAccountAgeSeconds: string;
  readonly coolingPeriodSeconds: string;
  readonly strongAuthRequired: boolean;
  readonly strongAuthMaxAgeSeconds: string;
}

export type PolicyBody = Schemas["DelegationWithdrawalPolicyRequest"];

/** A policy version as a form starts from it: the one in force, or nothing typed. */
export function policyDraftFrom(policy: Schemas["DelegationWithdrawalPolicy"] | undefined): PolicyDraft {
  if (policy === undefined) {
    return {
      autoApproveEnabled: false,
      autoApproveMax: "",
      autoApproveMaxRiskScore: "0",
      maxRiskScore: "",
      perWithdrawalLimit: "",
      dailyLimit: "",
      velocityMaxCount: "",
      minDestinationAgeSeconds: "0",
      minAccountAgeSeconds: "0",
      coolingPeriodSeconds: "0",
      strongAuthRequired: true,
      strongAuthMaxAgeSeconds: "900",
    };
  }
  return {
    autoApproveEnabled: policy.auto_approve_enabled,
    autoApproveMax: policy.auto_approve_max_usd,
    autoApproveMaxRiskScore: String(policy.auto_approve_max_risk_score),
    maxRiskScore: String(policy.max_risk_score),
    perWithdrawalLimit: policy.per_withdrawal_limit_usd,
    dailyLimit: policy.daily_limit_usd,
    velocityMaxCount: String(policy.velocity_max_count),
    minDestinationAgeSeconds: String(policy.min_destination_age_seconds),
    minAccountAgeSeconds: String(policy.min_account_age_seconds),
    coolingPeriodSeconds: String(policy.cooling_period_seconds),
    strongAuthRequired: policy.strong_auth_required,
    strongAuthMaxAgeSeconds: String(policy.strong_auth_max_age_seconds),
  };
}

const SECONDS_PER_YEAR = 31_536_000;

/** The policy as the API takes it, or the first problem of each field. Shapes only; the bounds are the backend's too. */
export function checkPolicy(accountId: string, draft: PolicyDraft): InputCheck<PolicyBody | undefined> & {
  readonly problems: Readonly<Record<string, string>>;
} {
  const problems: Record<string, string> = {};
  const usd = (field: string, raw: string, what: string): string => {
    const check = checkUsd(raw, what);
    if (!check.ok) problems[field] = check.error;
    return check.value;
  };
  const whole = (field: string, raw: string, min: number, max: number, what: string): number => {
    const check = checkWholeNumber(raw, min, max, what);
    if (!check.ok) problems[field] = check.error;
    return check.value;
  };
  const body: PolicyBody = {
    account_id: accountId,
    auto_approve_enabled: draft.autoApproveEnabled,
    auto_approve_max_usd: usd("autoApproveMax", draft.autoApproveMax, "the automatic approval limit"),
    auto_approve_max_risk_score: whole("autoApproveMaxRiskScore", draft.autoApproveMaxRiskScore, 0, 100, "the automatic risk score"),
    max_risk_score: whole("maxRiskScore", draft.maxRiskScore, 0, 100, "the highest risk score"),
    per_withdrawal_limit_usd: usd("perWithdrawalLimit", draft.perWithdrawalLimit, "the per-withdrawal limit"),
    daily_limit_usd: usd("dailyLimit", draft.dailyLimit, "the daily limit"),
    velocity_max_count: whole("velocityMaxCount", draft.velocityMaxCount, 1, 1000, "the withdrawals per 24 hours"),
    min_destination_age_seconds: whole(
      "minDestinationAgeSeconds",
      draft.minDestinationAgeSeconds,
      0,
      SECONDS_PER_YEAR,
      "the destination's minimum age in seconds",
    ),
    min_account_age_seconds: whole(
      "minAccountAgeSeconds",
      draft.minAccountAgeSeconds,
      0,
      SECONDS_PER_YEAR,
      "the account's minimum age in seconds",
    ),
    cooling_period_seconds: whole("coolingPeriodSeconds", draft.coolingPeriodSeconds, 0, 604_800, "the cooling period in seconds"),
    strong_auth_required: draft.strongAuthRequired,
    strong_auth_max_age_seconds: whole(
      "strongAuthMaxAgeSeconds",
      draft.strongAuthMaxAgeSeconds,
      1,
      86_400,
      "the strong sign-in age in seconds",
    ),
  };
  const ok = Object.keys(problems).length === 0;
  return { ok, value: ok ? body : undefined, error: ok ? "" : "Some fields need attention.", problems };
}

/* --------------------------------------------------------------------------
 * Idempotency
 * ------------------------------------------------------------------------ */

/** A request the person confirmed, with the key that makes it exactly once. */
export interface ConfirmedAttempt<R> {
  readonly request: R;
  readonly idempotencyKey: string;
}

/**
 * The attempt a confirmation sends. The key is created at the moment of
 * confirmation and only then: confirming the same request again (a retry after
 * a failure, a double click) reuses the attempt's key, so the backend answers
 * it as one request; a different request is a new attempt with a new key.
 */
export function confirmAttempt<R>(current: ConfirmedAttempt<R> | undefined, request: R, newKey: () => string): ConfirmedAttempt<R> {
  if (current !== undefined && JSON.stringify(current.request) === JSON.stringify(request)) {
    return current;
  }
  return { request, idempotencyKey: newKey() };
}

/* --------------------------------------------------------------------------
 * Reading what the backend said
 * ------------------------------------------------------------------------ */

/** A plan price as the plan carries it: whole sandbox dollars per whole unit, mantissa x 10^-scale. */
export function planPriceText(price: { readonly mantissa: string; readonly scale: string; readonly par: boolean }): string {
  const scale = price.scale === "0" ? "" : ` × 10^-${price.scale}`;
  return `${price.mantissa}${scale}${price.par ? " (par: the registry's USD peg)" : ""}`;
}

/** The path statuses a subject went through, oldest first, as its transitions record them. */
export function statusPath(transitions: readonly { readonly to_status: string }[]): string[] {
  return transitions.map((transition) => transition.to_status);
}
