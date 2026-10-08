/**
 * Pure helpers for the margin section of the advanced area (internal/margin).
 *
 * Nothing here computes a financial figure. Every amount the section shows is
 * a string the margin engine computed; these functions name statuses, move a
 * decimal point through an integer's own digits for display, and decide which
 * idempotency key a confirmed command carries.
 *
 * The enumerations repeat values the OpenAPI contract declares, because the
 * runtime validator needs them as values; each is tied to its generated type
 * at compile time, so a contract change fails `tsc` here.
 */
import type { Schemas } from "@controlplane/generated-client";

import { MoneyFormatError, formatQuantity } from "./money.ts";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;
/** True when T is a union of two or more members. */
type IsUnion<T, U = T> = T extends unknown ? ([U] extends [T] ? false : true) : never;

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export type MarginAccountStatus = Schemas["MarginAccountStatus"];
export const MARGIN_ACCOUNT_STATUSES = ["NORMAL", "WARNING", "MARGIN_CALL", "LIQUIDATING", "BANKRUPT", "CLOSED"] as const;
export type MarginAccountStatusesMatchContract = Assert<
  Same<(typeof MARGIN_ACCOUNT_STATUSES)[number], MarginAccountStatus>
>;

export type MarginCallStatus = Schemas["MarginCallStatus"];
export const MARGIN_CALL_STATUSES = ["OPEN", "MET", "LIQUIDATED"] as const;
export type MarginCallStatusesMatchContract = Assert<Same<(typeof MARGIN_CALL_STATUSES)[number], MarginCallStatus>>;

export type MarginCaseStatus = Schemas["MarginCaseStatus"];
export const MARGIN_CASE_STATUSES = [
  "AWAITING_PRICE",
  "AWAITING_VENUE",
  "IN_PROGRESS",
  "COMPLETED",
  "BANKRUPT",
  "MANUAL_REVIEW",
] as const;
export type MarginCaseStatusesMatchContract = Assert<Same<(typeof MARGIN_CASE_STATUSES)[number], MarginCaseStatus>>;

export type MarginPlanDecision = Schemas["MarginPlanDecision"];
export const MARGIN_PLAN_DECISIONS = [
  "COMPLETE",
  "AWAIT_PRICE",
  "AWAIT_VENUE",
  "SELL",
  "SELL_ALL",
  "APPLY_COLLATERAL",
  "BANKRUPT",
  "MANUAL_REVIEW",
] as const;
export type MarginPlanDecisionsMatchContract = Assert<Same<(typeof MARGIN_PLAN_DECISIONS)[number], MarginPlanDecision>>;

export type MarginStepAction = Schemas["MarginStepAction"];
export const MARGIN_STEP_ACTIONS = ["SELL", "APPLY_COLLATERAL"] as const;
export type MarginStepActionsMatchContract = Assert<Same<(typeof MARGIN_STEP_ACTIONS)[number], MarginStepAction>>;

export type MarginLiquidationTarget = Schemas["MarginLiquidationTarget"];
export const MARGIN_LIQUIDATION_TARGETS = ["INITIAL_MARGIN", "MAINTENANCE_PLUS_BUFFER"] as const;
export type MarginTargetsMatchContract = Assert<
  Same<(typeof MARGIN_LIQUIDATION_TARGETS)[number], MarginLiquidationTarget>
>;

export type MarginStressScenario = Schemas["MarginStressScenario"];
export const MARGIN_STRESS_SCENARIOS = [
  "GAP_DOWN_5",
  "GAP_DOWN_10",
  "GAP_DOWN_25",
  "GAP_DOWN_50",
  "GAP_UP_5",
  "GAP_UP_10",
  "GAP_UP_25",
  "GAP_UP_50",
  "ORACLE_UNAVAILABLE",
  "VENUE_UNAVAILABLE",
  "PARTIAL_FILL",
  "EXTREME_SPREAD",
  "BRIDGE_DELAYED",
  "COLLATERAL_DEPEG",
] as const;
export type MarginStressScenariosMatchContract = Assert<
  Same<(typeof MARGIN_STRESS_SCENARIOS)[number], MarginStressScenario>
>;

/**
 * The modes a margin account can run in. The database refuses any other
 * (margin_accounts_mode_check), so an account claiming LIVE or DEVNET is a
 * contract violation, not a row to display.
 */
export const MARGIN_RUN_MODES = ["PAPER", "SANDBOX"] as const satisfies readonly Schemas["AdvancedMode"][];

/* --------------------------------------------------------------------------
 * Margin modes
 *
 * The contract declares three. Two are leveraged and are the ones this
 * interface offers when an account is opened. The third is the fully paid
 * mode, whose wire token is a word the honesty rules ban from this
 * interface's source and screens (PART 112), so it is never spelled here: the
 * validator reads the field as a string, the types below prove the contract
 * still has the two leveraged modes plus exactly one other, and the page names
 * the other mode for what it means.
 * ------------------------------------------------------------------------ */

export type MarginMode = Schemas["MarginMode"];
export const LEVERAGED_MARGIN_MODES = ["ISOLATED", "CROSS"] as const satisfies readonly MarginMode[];
export type LeveragedMarginMode = (typeof LEVERAGED_MARGIN_MODES)[number];
type FullyPaidMode = Exclude<MarginMode, LeveragedMarginMode>;
export type MarginModeHasExactlyOneFullyPaidMode = Assert<
  Same<[FullyPaidMode] extends [never] ? "none" : IsUnion<FullyPaidMode>, false>
>;

/** True for the margin modes that may borrow. */
export function isLeveragedMode(mode: string): mode is LeveragedMarginMode {
  return (LEVERAGED_MARGIN_MODES as readonly string[]).includes(mode);
}

/** What a margin mode means, in words this interface may use. */
export function marginModeLabel(mode: string): string {
  switch (mode) {
    case "ISOLATED":
      return "Isolated — the collateral backs one risky asset and nothing else";
    case "CROSS":
      return "Cross — the collateral is shared across every position";
    default:
      return "Fully paid — no borrowing and no leverage";
  }
}

/* --------------------------------------------------------------------------
 * What statuses mean
 * ------------------------------------------------------------------------ */

export type Tone = "neutral" | "good" | "warn" | "bad" | "info";

export const ACCOUNT_STATUS_MEANING: Readonly<Record<MarginAccountStatus, string>> = {
  NORMAL: "Equity is at or above maintenance plus the warning buffer, or the account owes nothing.",
  WARNING: "Equity is below maintenance plus the warning buffer. Nothing is sold; the account is told.",
  MARGIN_CALL: "Equity is below maintenance. A margin call is open and the account is eligible for liquidation.",
  LIQUIDATING: "A liquidation case is open and decides what happens next.",
  BANKRUPT: "Everything was sold and liabilities remain: a deficit the insurance fund covers and the account owes.",
  CLOSED: "The margin account holds nothing and owes the lending pool nothing.",
};

export function accountStatusTone(status: string): Tone {
  switch (status) {
    case "NORMAL":
      return "good";
    case "WARNING":
      return "warn";
    case "MARGIN_CALL":
    case "LIQUIDATING":
    case "BANKRUPT":
      return "bad";
    default:
      return "neutral";
  }
}

export const CALL_STATUS_MEANING: Readonly<Record<MarginCallStatus, string>> = {
  OPEN: "Equity is below maintenance and the call has not been met.",
  MET: "Equity recovered above maintenance and the call was closed.",
  LIQUIDATED: "A liquidation case took the call over.",
};

export function callStatusTone(status: string): Tone {
  return status === "OPEN" ? "bad" : status === "MET" ? "good" : "neutral";
}

export const CASE_STATUS_MEANING: Readonly<Record<MarginCaseStatus, string>> = {
  AWAITING_PRICE: "A mark the case needs is stale or missing. Nothing is sold on a price nobody can vouch for.",
  AWAITING_VENUE: "The paper venue cannot execute. The case waits.",
  IN_PROGRESS: "Steps are being executed, one deterministic step at a time.",
  COMPLETED: "The account was restored to the case's target.",
  BANKRUPT: "Everything was sold and a deficit remains.",
  MANUAL_REVIEW: "The step budget is spent or the planner can make no further progress. A person owns the case.",
};

export function caseStatusTone(status: string): Tone {
  switch (status) {
    case "COMPLETED":
      return "good";
    case "BANKRUPT":
    case "MANUAL_REVIEW":
      return "bad";
    case "AWAITING_PRICE":
    case "AWAITING_VENUE":
      return "warn";
    default:
      return "info";
  }
}

export const PLAN_DECISION_MEANING: Readonly<Record<MarginPlanDecision, string>> = {
  COMPLETE: "No (further) liquidation is needed.",
  AWAIT_PRICE: "A mark is stale or missing; nothing is sold.",
  AWAIT_VENUE: "The venue cannot execute; nothing is sold.",
  SELL: "A partial liquidation restores the target.",
  SELL_ALL: "No partial liquidation restores the target; every risky holding is sold.",
  APPLY_COLLATERAL: "Nothing risky is left; numeraire collateral repays the loans.",
  BANKRUPT: "Nothing is left to sell or apply and liabilities remain.",
  MANUAL_REVIEW: "The step budget is spent or the remaining risk is not something the planner can reduce.",
};

export const STRESS_SCENARIO_MEANING: Readonly<Record<MarginStressScenario, string>> = {
  GAP_DOWN_5: "Every risky mark falls 5%.",
  GAP_DOWN_10: "Every risky mark falls 10%.",
  GAP_DOWN_25: "Every risky mark falls 25%.",
  GAP_DOWN_50: "Every risky mark falls 50%.",
  GAP_UP_5: "Every risky mark rises 5%; derivative exposure is taken as adverse.",
  GAP_UP_10: "Every risky mark rises 10%; derivative exposure is taken as adverse.",
  GAP_UP_25: "Every risky mark rises 25%; derivative exposure is taken as adverse.",
  GAP_UP_50: "Every risky mark rises 50%; derivative exposure is taken as adverse.",
  ORACLE_UNAVAILABLE: "Every non-numeraire mark disappears; a liquidation would wait for a price.",
  VENUE_UNAVAILABLE: "The policy's execution gap, then a liquidation with no venue.",
  PARTIAL_FILL: "The policy's execution gap, then a liquidation whose every step only partly fills.",
  EXTREME_SPREAD: "The policy's execution gap, then every sale at the slippage cap.",
  BRIDGE_DELAYED: "Collateral in flight between chains is worth nothing.",
  COLLATERAL_DEPEG: "Stable, non-numeraire collateral falls by the policy's depeg shock.",
};

/** The haircut components, in the order the engine composes them. */
export const HAIRCUT_COMPONENTS = [
  ["volatility_bps", "Volatility class"],
  ["liquidity_bps", "Liquidity class"],
  ["oracle_bps", "Oracle age"],
  ["venue_bps", "Chain and venue tier"],
  ["concentration_bps", "Concentration"],
  ["settlement_bps", "Settlement delay"],
  ["cross_chain_bps", "Cross-chain delay"],
] as const satisfies ReadonlyArray<readonly [keyof Schemas["MarginHaircut"], string]>;

/* --------------------------------------------------------------------------
 * Display: leverage and prices, by moving a decimal point
 * ------------------------------------------------------------------------ */

/**
 * A leverage the engine states in basis points, as a multiple: 28654 ->
 * "2.8654×", 50000 -> "5×". The point is moved through the integer's digits;
 * nothing is divided.
 */
export function formatLeverage(bps: number): string {
  if (!Number.isInteger(bps) || bps < 0) {
    throw new MoneyFormatError(`leverage: ${String(bps)} is not a whole, non-negative number of basis points`);
  }
  const digits = String(bps).padStart(5, "0");
  const cut = digits.length - 4;
  let whole = digits.slice(0, cut);
  while (whole.length > 1 && whole.startsWith("0")) whole = whole.slice(1);
  let fraction = digits.slice(cut);
  while (fraction.endsWith("0")) fraction = fraction.slice(0, fraction.length - 1);
  return fraction === "" ? `${whole}×` : `${whole}.${fraction}×`;
}

/** An exact price, mantissa × 10^-scale, as a decimal string: ("1001000", 4) -> "100.1". */
export function formatMarginPrice(mantissa: string, scale: number): string {
  return formatQuantity(mantissa, scale);
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
 * Assets
 * ------------------------------------------------------------------------ */

interface AssetLike {
  readonly id: string;
  readonly kind: string;
  readonly symbol: string;
  readonly decimals: number;
  readonly status: string;
}

/**
 * The registry's simulated assets, in symbol order. Margin collateral and
 * margin positions are SIMULATED value only; whether a given asset is
 * eligible is the margin policy's decision, made by the backend.
 */
export function simulatedAssets<A extends AssetLike>(registry: readonly A[]): A[] {
  return registry
    .filter((asset) => asset.kind === "SIMULATED" && asset.status === "ACTIVE")
    .sort((a, b) => (a.symbol < b.symbol ? -1 : a.symbol > b.symbol ? 1 : 0));
}

/** An asset of the registry by id, or undefined when the registry does not list it. */
export function assetById<A extends AssetLike>(registry: readonly A[], id: string): A | undefined {
  const wanted = id.toLowerCase();
  return registry.find((asset) => asset.id.toLowerCase() === wanted);
}
