/**
 * Pure helpers for the prediction markets section of the advanced area
 * (internal/predmarket, ADR-0020).
 *
 * Nothing here computes a financial figure. Every amount on these pages is a
 * base-unit string the backend computed; these functions name lifecycle
 * states, order the lifecycle, and turn a typed whole number of complete sets
 * into the base units the API takes by moving digits, never by multiplying.
 *
 * Enumerations the contract declares as enums are tied to their generated
 * types (`Same`), so a contract change fails `tsc` here. The market status,
 * the settlement kind and the reconciliation status are plain strings in the
 * contract (their values are shared with other schemas' enums, and a shared
 * enum value renames other schemas' generated constants), so their closed
 * sets are stated here from the service: internal/predmarket's
 * AllMarketStatuses and AllSettlementKinds, and the reconciliation statuses.
 */
import type { Schemas } from "@controlplane/generated-client";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export const PREDICTION_MODES = ["PAPER", "SANDBOX"] as const;
export type PredictionModesMatchContract = Assert<
  Same<(typeof PREDICTION_MODES)[number], Schemas["PredictionMarketMode"]>
>;

export const RESOLUTION_KINDS = ["ORACLE", "DISPUTE_UPHELD", "DISPUTE_OVERTURNED"] as const;
export type ResolutionKindsMatchContract = Assert<
  Same<(typeof RESOLUTION_KINDS)[number], Schemas["PredictionResolutionKind"]>
>;

export const ROUNDING_POLICIES = ["EXACT", "FLOOR_PER_HOLDER"] as const;
export type RoundingPoliciesMatchContract = Assert<
  Same<(typeof ROUNDING_POLICIES)[number], Schemas["PredictionRoundingPolicy"]>
>;

export const SET_ACTIONS = ["ISSUE", "REDEEM"] as const;
export type SetActionsMatchContract = Assert<Same<(typeof SET_ACTIONS)[number], Schemas["PredictionSetAction"]>>;
export type SetAction = (typeof SET_ACTIONS)[number];

/** Every market status, in lifecycle order (the service's AllMarketStatuses). */
export const MARKET_STATUSES = [
  "DRAFT",
  "OPEN",
  "CLOSED",
  "RESOLVING",
  "RESOLVED",
  "DISPUTED",
  "SETTLED",
  "VOIDED",
] as const;
export type MarketStatus = (typeof MARKET_STATUSES)[number];

/** A settlement either paid the winners or refunded a void (AllSettlementKinds). */
export const SETTLEMENT_KINDS = ["PAYOUT", "REFUND"] as const;

/** The statuses a reconciliation record can be in. */
export const RECONCILIATION_STATUSES = [
  "OPEN",
  "MATCHED",
  "MISMATCH",
  "INVESTIGATING",
  "RESOLVED_AUTOMATIC",
  "RESOLVED_MANUAL",
  "ESCALATED",
] as const;

/** The status the market list opens on. */
export const DEFAULT_LIST_STATUS: MarketStatus = "OPEN";

/** A `status` search parameter: a status the service has, the default, or a value to say was ignored. */
export function parseMarketStatus(raw: string | null): {
  readonly status: MarketStatus;
  readonly ignored: string | undefined;
} {
  if (raw === null || raw === "") return { status: DEFAULT_LIST_STATUS, ignored: undefined };
  const status = MARKET_STATUSES.find((candidate) => candidate === raw);
  return status === undefined ? { status: DEFAULT_LIST_STATUS, ignored: raw } : { status, ignored: undefined };
}

/* --------------------------------------------------------------------------
 * Words for the lifecycle
 * ------------------------------------------------------------------------ */

export type Tone = "neutral" | "good" | "warn" | "bad" | "info";

/** What each market status means, in the service's own terms. */
export const STATUS_MEANING: Readonly<Record<MarketStatus, string>> = {
  DRAFT: "Defined, not yet open. Nothing can be issued until an operator opens it at or after its open time.",
  OPEN: "Complete sets can be issued and redeemed until the close time.",
  CLOSED: "Past its close time. Nothing is issued or redeemed; shares wait for a resolution.",
  RESOLVING: "An oracle report has been requested and is awaited.",
  RESOLVED:
    "A verified oracle report named the winning outcome. It settles once its dispute window has passed, unless disputed.",
  DISPUTED:
    "The resolution was disputed. Nothing settles until an independent operator rules on the dispute, or two operators void the market.",
  SETTLED:
    "Paid out from the escrow: each winning share paid one collateral unit and every share was retired. A payout that could not be delivered is held in the escrow until its holder claims it.",
  VOIDED:
    "Voided by two operators: every share was refunded at the disclosed rate and the residual stayed in the escrow. A refund that could not be delivered is held there until its holder claims it.",
};

export function statusTone(status: string): Tone {
  switch (status) {
    case "OPEN":
      return "good";
    case "DISPUTED":
      return "warn";
    case "VOIDED":
      return "bad";
    case "SETTLED":
      return "neutral";
    default:
      return "info";
  }
}

/** What each resolution kind means. */
export const RESOLUTION_MEANING: Readonly<Record<(typeof RESOLUTION_KINDS)[number], string>> = {
  ORACLE: "The first resolution: a report signed by the market's oracle and verified against its key.",
  DISPUTE_UPHELD: "A dispute was closed keeping the disputed outcome.",
  DISPUTE_OVERTURNED: "A dispute was closed with a different outcome, backed by a newly verified report.",
};

/** What each rounding policy means. */
export const ROUNDING_MEANING: Readonly<Record<(typeof ROUNDING_POLICIES)[number], string>> = {
  EXACT: "Each winning share paid exactly one collateral unit; nothing was rounded and nothing was left over.",
  FLOOR_PER_HOLDER:
    "Each share refunded the disclosed rate per share, rounded down once per holder in favour of the escrow; what was not paid is the residual shown.",
};

/**
 * The service's own explanation of the order book: outcome shares are
 * instruments an internal order book can trade, and that book is not part of
 * this build yet.
 */
export const ORDER_BOOK_NOTE =
  "Each outcome has its own instrument for the internal order book. Trading opens when the internal order book " +
  "lands; until then shares are only issued and redeemed here, as complete sets, against the escrow.";

/** Why a complete set is the unit here. */
export const COMPLETE_SET_NOTE =
  "A complete set is one share of every outcome and costs one collateral unit, which goes into the market's " +
  "escrow. Redeeming a set returns the unit. When the market settles, each share of the winning outcome pays one " +
  "unit and every share is retired.";

/* --------------------------------------------------------------------------
 * The lifecycle, as a sequence of steps
 * ------------------------------------------------------------------------ */

/**
 * "unknown": the API does not say whether a voided market reached this step.
 * It exposes the market's resolutions and disputes, not its transition rows,
 * and a void can come from any state but SETTLED.
 */
export type StepState = "done" | "current" | "ahead" | "unknown";

export interface LifecycleStep {
  readonly status: MarketStatus;
  readonly state: StepState;
}

/** What the market's own records prove happened. */
export interface LifecycleEvidence {
  /** At least one resolution was accepted. */
  readonly resolved: boolean;
  /** At least one dispute was raised. */
  readonly disputed: boolean;
}

/** The path a market takes when nothing goes wrong. */
const MAIN_PATH: readonly MarketStatus[] = ["DRAFT", "OPEN", "CLOSED", "RESOLVING", "RESOLVED", "SETTLED"];

/**
 * The lifecycle drawn for a market in `status`: the main path, with DISPUTED
 * after RESOLVED when the market was ever disputed, and VOIDED in place of
 * SETTLED when it was voided. Steps before the current one are done and steps
 * after it are ahead; a dispute that was ruled on is done even though the
 * market is RESOLVED again. For a voided market, a step is done only when its
 * records prove it (every market starts in DRAFT; a resolution proves every
 * step up to RESOLVED; a dispute proves DISPUTED) and unknown otherwise. A
 * status the service does not have marks nothing as current.
 */
export function lifecycleSteps(status: string, evidence: LifecycleEvidence): LifecycleStep[] {
  const voided = status === "VOIDED";
  const path: MarketStatus[] = [];
  for (const step of MAIN_PATH) {
    if (step === "SETTLED" && voided) continue;
    path.push(step);
    if (step === "RESOLVED" && (evidence.disputed || status === "DISPUTED")) path.push("DISPUTED");
  }
  if (voided) path.push("VOIDED");

  const resolvedAt = path.indexOf("RESOLVED");
  if (voided) {
    return path.map((step, index) => {
      if (step === "VOIDED") return { status: step, state: "current" };
      const proven =
        step === "DRAFT" || (evidence.resolved && index <= resolvedAt) || (step === "DISPUTED" && evidence.disputed);
      return { status: step, state: proven ? "done" : "unknown" };
    });
  }
  const at = path.indexOf(status as MarketStatus);
  return path.map((step, index) => {
    if (step === "DISPUTED" && evidence.disputed && status !== "DISPUTED" && at >= resolvedAt) {
      return { status: step, state: "done" };
    }
    return { status: step, state: at < 0 ? "ahead" : index < at ? "done" : index === at ? "current" : "ahead" };
  });
}

/* --------------------------------------------------------------------------
 * Typing a number of complete sets
 * ------------------------------------------------------------------------ */

export interface SetsParse {
  readonly ok: boolean;
  /** Share base units, as the API takes them, when `ok`. */
  readonly value: string;
  /** The whole number of sets, without leading zeros, when `ok`. */
  readonly sets: string;
  readonly error: string;
}

/**
 * A whole number of complete sets typed by a person, as share base units:
 * ("3", 6) -> "3000000". Sets are whole, so the input is digits only; the
 * decimal point is never involved and the base units are the digits with
 * `decimals` zeros appended.
 */
export function parseCompleteSets(raw: string, decimals: number): SetsParse {
  const fail = (error: string): SetsParse => ({ ok: false, value: "", sets: "", error });
  if (!Number.isInteger(decimals) || decimals < 0 || decimals > 36) {
    return fail("The market declares no usable share precision.");
  }
  const cleaned = raw.trim().replace(/,/g, "");
  if (cleaned === "") return fail("Enter a number of complete sets.");
  if (!/^[0-9]+$/.test(cleaned)) return fail("Complete sets are whole: use digits only.");
  let start = 0;
  while (start < cleaned.length - 1 && cleaned[start] === "0") start = start + 1;
  const sets = cleaned.slice(start);
  if (sets === "0") return fail("Enter at least one complete set.");
  if (sets.length > 15) return fail("That is more sets than the API accepts in one request.");
  return { ok: true, value: `${sets}${"0".repeat(decimals)}`, sets, error: "" };
}

/* --------------------------------------------------------------------------
 * The operator's forms
 * ------------------------------------------------------------------------ */

/** An Ed25519 public key in hex: 32 bytes, 64 hex characters. */
export function isEd25519KeyHex(value: string): boolean {
  return /^[0-9a-fA-F]{64}$/.test(value);
}

/** Outcome symbols typed as a comma-separated list, trimmed, empty entries dropped. Not otherwise changed. */
export function parseOutcomes(raw: string): string[] {
  return raw
    .split(",")
    .map((part) => part.trim())
    .filter((part) => part !== "");
}

/** The dispute windows the create form offers, in milliseconds (the contract's unit). */
export const DISPUTE_WINDOW_CHOICES: ReadonlyArray<{ readonly ms: number; readonly label: string }> = [
  { ms: 0, label: "None — settle as soon as it resolves" },
  { ms: 300_000, label: "5 minutes" },
  { ms: 3_600_000, label: "1 hour" },
  { ms: 86_400_000, label: "1 day" },
  { ms: 604_800_000, label: "7 days" },
];

/**
 * A `datetime-local` value (local wall-clock time, no zone) as the RFC 3339
 * instant the API takes, or undefined when it is not a time.
 */
export function localInputToInstant(value: string): string | undefined {
  if (value.trim() === "") return undefined;
  const parsed = new Date(value);
  return isNaN(parsed.getTime()) ? undefined : parsed.toISOString();
}

/* --------------------------------------------------------------------------
 * Voids and held payouts
 * ------------------------------------------------------------------------ */

/** How a void works, in the service's terms. */
export const VOID_DUAL_CONTROL_NOTE =
  "A void needs two operators: one proposes it with a reason and evidence, a different one approves it, and only " +
  "the approval refunds. Neither may be the market's creator or the author of its resolution in force, and a " +
  "resolved market inside its dispute window is disputed, not voided.";

/** What a held payout is. */
export const HELD_PAYOUT_NOTE =
  "When a market settled or was voided, this account's payout could not be delivered (the reasons are the risk " +
  "kernel's codes). Its shares were retired like everyone else's and the amount is held in the market's escrow " +
  "for this account. Once the account is eligible, claim it; the claim is checked again and happens once.";

/**
 * The void proposals still waiting for a second operator: not approved, on a
 * market that has not settled or been voided. A proposal made against another
 * resolution than the one now in force is still listed; the backend refuses
 * its approval and says why.
 */
export function pendingVoidProposals<P extends { readonly id: string; readonly approval?: unknown }>(
  proposals: readonly P[],
  marketStatus: string,
): P[] {
  if (marketStatus === "SETTLED" || marketStatus === "VOIDED") return [];
  return proposals.filter((proposal) => proposal.approval === undefined || proposal.approval === null);
}

/** Whether the account can ask for its held payout now: there is one, and it has not been claimed. */
export function claimable(held: { readonly claimed: boolean } | undefined): boolean {
  return held !== undefined && !held.claimed;
}

/** A reason the service accepts: 8 to 2000 characters once trimmed. */
export function reasonProblem(reason: string): string | undefined {
  const length = reason.trim().length;
  if (length < 8) return "State the reason in at least 8 characters; the service refuses a shorter one.";
  if (length > 2000) return "The service takes at most 2000 characters.";
  return undefined;
}
