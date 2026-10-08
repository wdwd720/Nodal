/**
 * Pure helpers for the derivatives section of the advanced (sandbox) area
 * (internal/derivatives, ADR-0020).
 *
 * Nothing here computes a financial figure. Every price, amount, rate,
 * requirement and valuation on a derivatives page is a string the backend
 * computed; these functions name things, say whether what a person typed is
 * exactly what the API accepts, move a decimal point through a digit string,
 * and place an interval on its grid — a time, never an amount.
 *
 * The enumerations repeat values the OpenAPI contract declares, because the
 * runtime validator needs them as values; each is tied to its generated type
 * at compile time, so a contract change fails `tsc` here.
 */
import type { Schemas } from "@controlplane/generated-client";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export type DerivativeKind = Schemas["DerivativeKind"];
export const DERIVATIVE_KINDS = ["FUTURE", "PERPETUAL", "OPTION"] as const;
export type DerivativeKindsMatchContract = Assert<Same<(typeof DERIVATIVE_KINDS)[number], DerivativeKind>>;

export type DerivativeSide = Schemas["DerivativeSide"];
export const DERIVATIVE_SIDES = ["LONG", "SHORT"] as const;
export type DerivativeSidesMatchContract = Assert<Same<(typeof DERIVATIVE_SIDES)[number], DerivativeSide>>;

export type PositionStatus = Schemas["DerivativePositionStatus"];
export const POSITION_STATUSES = ["OPEN", "CLOSED", "SETTLED"] as const;
export type PositionStatusesMatchContract = Assert<Same<(typeof POSITION_STATUSES)[number], PositionStatus>>;

export type FlowKind = Schemas["DerivativeFlowKind"];
export const FLOW_KINDS = [
  "REALIZED",
  "VARIATION_MARGIN",
  "FUNDING",
  "FUTURE_SETTLEMENT",
  "OPTION_PREMIUM",
  "OPTION_EXERCISE",
  "OPTION_ASSIGNMENT",
] as const;
export type FlowKindsMatchContract = Assert<Same<(typeof FLOW_KINDS)[number], FlowKind>>;

export type FundingDirection = Schemas["DerivativeFundingDirection"];
export const FUNDING_DIRECTIONS = ["PAY", "RECEIVE"] as const;
export type FundingDirectionsMatchContract = Assert<Same<(typeof FUNDING_DIRECTIONS)[number], FundingDirection>>;

export const FILL_SIDES = ["BUY", "SELL"] as const;
export type FillSidesMatchContract = Assert<Same<(typeof FILL_SIDES)[number], Schemas["DerivativeFillSide"]>>;

export const FILL_SOURCES = ["PAPER", "BOOK", "EXERCISE", "ASSIGNMENT", "EXPIRY"] as const;
export type FillSourcesMatchContract = Assert<Same<(typeof FILL_SOURCES)[number], Schemas["DerivativeFillSource"]>>;

export const SETTLEMENT_KINDS = ["FUTURE", "OPTION"] as const;
export type SettlementKindsMatchContract = Assert<
  Same<(typeof SETTLEMENT_KINDS)[number], Schemas["DerivativeSettlementKind"]>
>;

export type ValuationStatus = Schemas["DerivativeValuationStatus"];
export const VALUATION_STATUSES = ["VALUED", "REFUSED", "NOT_APPLICABLE", "NOT_COMPUTED"] as const;
export type ValuationStatusesMatchContract = Assert<Same<(typeof VALUATION_STATUSES)[number], ValuationStatus>>;

export const BOOK_STATUSES = ["VALUED", "REFUSED"] as const;
export type BookStatusesMatchContract = Assert<Same<(typeof BOOK_STATUSES)[number], Schemas["DerivativeBookStatus"]>>;

export const RUN_KINDS = ["VARIATION_MARGIN", "FUNDING"] as const;
export type RunKindsMatchContract = Assert<Same<(typeof RUN_KINDS)[number], Schemas["DerivativeRunKind"]>>;

export type SettlementMethod = Schemas["DerivativeSettlementMethod"];
export const SETTLEMENT_METHODS = ["RULE", "PROPOSE_OVERRIDE", "APPROVE_OVERRIDE", "REJECT_OVERRIDE"] as const;
export type SettlementMethodsMatchContract = Assert<Same<(typeof SETTLEMENT_METHODS)[number], SettlementMethod>>;

export type SettlementPriceRule = Schemas["DerivativeSettlementPriceRule"];
export const PRICE_RULES = ["EXPIRY_MARK", "OPERATOR_OVERRIDE"] as const;
export type PriceRulesMatchContract = Assert<Same<(typeof PRICE_RULES)[number], SettlementPriceRule>>;

export type OverrideStatus = Schemas["DerivativeOverrideStatus"];
export const OVERRIDE_STATUSES = ["PROPOSED", "APPROVED", "REJECTED"] as const;
export type OverrideStatusesMatchContract = Assert<Same<(typeof OVERRIDE_STATUSES)[number], OverrideStatus>>;

export const OVERRIDE_DECISIONS = ["APPROVED", "REJECTED"] as const;
export type OverrideDecisionsMatchContract = Assert<
  Same<(typeof OVERRIDE_DECISIONS)[number], Schemas["DerivativeOverrideDecisionKind"]>
>;

/**
 * The modes a derivative intent is offered in. The derivatives service runs
 * in PAPER and SANDBOX only; any other mode is the backend's to refuse.
 */
export const TRADE_MODES = ["PAPER", "SANDBOX"] as const satisfies readonly Schemas["AdvancedMode"][];
export type TradeMode = (typeof TRADE_MODES)[number];

/* --------------------------------------------------------------------------
 * Words
 * ------------------------------------------------------------------------ */

export const KIND_LABEL: Readonly<Record<DerivativeKind, string>> = {
  FUTURE: "Dated future",
  PERPETUAL: "Perpetual future",
  OPTION: "Option",
};

/** What each flow of the clearing-house register is. */
export const FLOW_KIND_LABEL: Readonly<Record<FlowKind, string>> = {
  REALIZED: "Realized on a reducing trade",
  VARIATION_MARGIN: "Variation margin",
  FUNDING: "Perpetual funding",
  FUTURE_SETTLEMENT: "Final settlement of a future",
  OPTION_PREMIUM: "Option premium",
  OPTION_EXERCISE: "Option exercise",
  OPTION_ASSIGNMENT: "Option assignment",
};

/** What each valuation answer means. The backend decides which one applies. */
export const VALUATION_MEANING: Readonly<Record<ValuationStatus, string>> = {
  VALUED: "Valued by the backend at the contract's latest mark.",
  REFUSED:
    "The backend could not value the account's open book and said why. It refuses to state a figure that " +
    "could understate the risk, so none is shown.",
  NOT_APPLICABLE: "The position is no longer open, so it has no current valuation.",
  NOT_COMPUTED: "This answer did not value the position; the position page does.",
};

/**
 * Shown with every analytics figure. The backend labels the values itself;
 * this says, in this interface's words, what they are not.
 */
export const REFERENCE_VALUES_NOTE =
  "These are reference values of a pricing model, computed by the backend from the inputs shown with " +
  "them. They are not a quote, not a price anyone will trade at, not a settlement price and not advice, " +
  "and nothing is ever posted from them.";

/** How a paper fill is priced, in the policy's own terms. */
export const PAPER_FILL_NOTE =
  "A paper fill happens at the contract's latest mark moved against you by the disclosed slippage, then " +
  "onto the tick grid against you. A buy fills above the mark and a sell below it. The mark must be fresh: " +
  "without one there is no price, and an order — a close included — is refused and the refusal recorded.";

/** What decided a final settlement price. */
export const PRICE_RULE_LABEL: Readonly<Record<SettlementPriceRule, string>> = {
  EXPIRY_MARK: "The settlement rule: the last mark observed and recorded by expiry, fresh at expiry",
  OPERATOR_OVERRIDE: "An override one operator proposed within the policy's bound and a second operator approved",
};

/** What each state of an override proposal means. */
export const OVERRIDE_STATUS_MEANING: Readonly<Record<OverrideStatus, string>> = {
  PROPOSED: "Waiting for a second operator's decision. Nothing has settled.",
  APPROVED: "A second operator approved it, and the contract settled at its price.",
  REJECTED: "Rejected. It settled nothing.",
};

/* --------------------------------------------------------------------------
 * Rates
 * ------------------------------------------------------------------------ */

/**
 * A funding rate in units of 10^-8 per interval, as a percentage:
 * 66733 -> "0.066733%". The point is moved through the integer's own
 * digits (10^-8 is 10^-6 of a percent); nothing is divided.
 */
export function formatRateE8(rateE8: number): string {
  if (!Number.isInteger(rateE8)) return "not a whole number of 10^-8";
  const negative = rateE8 < 0;
  const digits = (negative ? String(rateE8).slice(1) : String(rateE8)).padStart(7, "0");
  const cut = digits.length - 6;
  const whole = digits.slice(0, cut);
  let fraction = digits.slice(cut);
  while (fraction.endsWith("0")) fraction = fraction.slice(0, fraction.length - 1);
  const body = fraction === "" ? `${whole}%` : `${whole}.${fraction}%`;
  const zero = /^0+$/.test(digits);
  return negative && !zero ? `-${body}` : body;
}

/* --------------------------------------------------------------------------
 * What a person typed
 * ------------------------------------------------------------------------ */

export interface InputParse {
  readonly ok: boolean;
  /** The exact wire form when `ok`. */
  readonly value: string;
  /** What to say under the field when not `ok`. */
  readonly error: string;
}

const DECIMAL_INPUT = /^[0-9]{1,38}(\.[0-9]{1,38})?$/;

/**
 * A price exactly as written ("65,000.50" -> "65000.50"). It is never
 * rounded: the backend refuses one that is not exact at the contract's price
 * scale or not on its tick grid, and that refusal is what is shown.
 */
export function parseDecimalInput(raw: string): InputParse {
  const cleaned = raw.trim().replace(/,/g, "");
  if (cleaned === "") return { ok: false, value: "", error: "Enter a price." };
  if (!DECIMAL_INPUT.test(cleaned)) {
    return { ok: false, value: "", error: "Write the price with digits and at most one decimal point, such as 65000.50." };
  }
  return { ok: true, value: cleaned, error: "" };
}

/** A positive whole number of contracts ("010" -> "10"). */
export function parseContractsInput(raw: string): InputParse {
  const cleaned = raw.trim().replace(/,/g, "");
  if (cleaned === "") return { ok: false, value: "", error: "Enter a number of contracts." };
  if (!/^[0-9]+$/.test(cleaned)) {
    return { ok: false, value: "", error: "Contracts are a whole number, such as 10." };
  }
  let start = 0;
  while (start < cleaned.length - 1 && cleaned[start] === "0") start = start + 1;
  const value = cleaned.slice(start);
  if (value === "0") return { ok: false, value: "", error: "Enter at least one contract." };
  if (value.length > 30) return { ok: false, value: "", error: "That is more contracts than the API accepts." };
  return { ok: true, value, error: "" };
}

export interface BpsParse {
  readonly ok: boolean;
  /** The whole number of basis points when `ok`. */
  readonly value: number;
  readonly error: string;
}

const DIGITS = "0123456789";

/**
 * A whole number of basis points within [min, max], for a model input such
 * as a volatility — a parameter, never an amount. The query parameter is a
 * JSON integer, so the digits are accumulated one by one (at most six, far
 * inside exact integer range) rather than handed to a numeric parser.
 */
export function parseBpsInput(raw: string, min: number, max: number): BpsParse {
  const cleaned = raw.trim().replace(/,/g, "");
  if (!/^-?[0-9]{1,6}$/.test(cleaned)) {
    return { ok: false, value: 0, error: "Enter a whole number of basis points, such as 8000 for 80%." };
  }
  const negative = cleaned.startsWith("-");
  let value = 0;
  for (const ch of negative ? cleaned.slice(1) : cleaned) {
    value = value * 10 + DIGITS.indexOf(ch);
  }
  if (negative) value = -value;
  if (value < min || value > max) {
    return { ok: false, value: 0, error: `Use a value from ${String(min)} to ${String(max)} basis points.` };
  }
  return { ok: true, value, error: "" };
}

/** A SHA-256 digest as the API takes it: 64 lowercase hexadecimal digits. */
export function parseSha256Input(raw: string): InputParse {
  const cleaned = raw.trim().toLowerCase();
  if (cleaned === "") return { ok: false, value: "", error: "Enter the evidence's SHA-256 digest." };
  if (!/^[0-9a-f]{64}$/.test(cleaned)) {
    return { ok: false, value: "", error: "A SHA-256 digest is 64 hexadecimal digits." };
  }
  return { ok: true, value: cleaned, error: "" };
}

/** Required free text, trimmed, within a length the API accepts. */
export function parseReasonInput(raw: string, max: number): InputParse {
  const cleaned = raw.trim();
  if (cleaned === "") return { ok: false, value: "", error: "State the reason." };
  if (cleaned.length > max) return { ok: false, value: "", error: `Keep it to ${String(max)} characters.` };
  return { ok: true, value: cleaned, error: "" };
}

/**
 * A non-negative price mantissa at a scale as the exact decimal the API takes
 * back: ("6600000", 2) -> "66000.00". The point is moved through the digit
 * string; nothing is divided. Undefined for anything that is not a mantissa.
 */
export function mantissaDecimal(mantissa: string, scale: number): string | undefined {
  if (!/^[0-9]+$/.test(mantissa) || !Number.isInteger(scale) || scale < 0 || scale > 38) return undefined;
  if (scale === 0) return mantissa;
  const padded = mantissa.padStart(scale + 1, "0");
  const cut = padded.length - scale;
  return `${padded.slice(0, cut)}.${padded.slice(cut)}`;
}

const INSTANT_INPUT = /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]{1,3})?)?Z$/;

/** An instant written in UTC with a trailing Z, such as 2026-10-07T08:00:00Z. */
export function parseInstantInput(raw: string): InputParse {
  const cleaned = raw.trim();
  if (cleaned === "") return { ok: false, value: "", error: "Enter an instant in UTC." };
  if (!INSTANT_INPUT.test(cleaned) || isNaN(new Date(cleaned).getTime())) {
    return { ok: false, value: "", error: "Write the instant in UTC, such as 2026-10-07T08:00:00Z." };
  }
  return { ok: true, value: new Date(cleaned).toISOString(), error: "" };
}

/* --------------------------------------------------------------------------
 * Interval grids (times, never amounts)
 * ------------------------------------------------------------------------ */

/**
 * The start of the latest interval of `intervalMs` that has fully ended at
 * `nowMs`, on the grid of whole intervals since the Unix epoch — the default
 * an operator form offers. Whether that interval may run is the backend's
 * decision.
 */
export function latestEndedIntervalStart(nowMs: number, intervalMs: number): number | undefined {
  if (!Number.isInteger(nowMs) || !Number.isInteger(intervalMs) || intervalMs <= 0 || nowMs < intervalMs) {
    return undefined;
  }
  return nowMs - (nowMs % intervalMs) - intervalMs;
}

/** The instant of a millisecond count, in the UTC form the forms accept. */
export function instantOf(ms: number): string {
  return new Date(ms).toISOString().replace(".000Z", "Z");
}

/* --------------------------------------------------------------------------
 * What a page offers
 * ------------------------------------------------------------------------ */

export interface Availability {
  readonly available: boolean;
  /** Why not, when not. */
  readonly reason: string;
}

/**
 * Whether the exercise form is offered for a position. It mirrors what the
 * service documents — only the holder of an open long position in an
 * AMERICAN option exercises; a European option is exercised automatically at
 * expiry — and the backend still decides every request.
 */
export function exerciseAvailability(
  position: { readonly kind: string; readonly status: string; readonly side?: string },
  exerciseStyle: string | undefined,
): Availability {
  if (position.kind !== "OPTION") return { available: false, reason: "Only an option is exercised." };
  if (position.status !== "OPEN") return { available: false, reason: "The position is no longer open." };
  if (position.side !== "LONG") {
    return { available: false, reason: "Only the holder of a long position exercises; a writer is assigned." };
  }
  if (exerciseStyle !== "AMERICAN") {
    return {
      available: false,
      reason:
        "A European option is exercised automatically at expiry when it is in the money; its holder cannot exercise it early.",
    };
  }
  return { available: true, reason: "" };
}

/** The intent a recorded refusal names, so the page can link to it. */
export function refusedIntentId(fields: Readonly<Record<string, unknown>> | undefined): string | undefined {
  const value = fields?.["intent_id"];
  return typeof value === "string" && value !== "" ? value : undefined;
}
