/**
 * Pure helpers for the equities section of the advanced (sandbox) area:
 * paper and sandbox equities and ETFs of internal/equities (ADR-0020).
 *
 * Nothing here computes a financial figure. Every price, notional, charge
 * and settlement amount on an equities page is a string the backend
 * computed; these functions name things, say what a status or a session lets
 * a person do (the backend still decides every request), and say whether
 * what a person typed is exactly what the API accepts.
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

export type EquityKind = Schemas["AdvancedEquityKind"];
export const EQUITY_KINDS = ["EQUITY", "ETF"] as const;
export type EquityKindsMatchContract = Assert<Same<(typeof EQUITY_KINDS)[number], EquityKind>>;

export type EquityMode = Schemas["AdvancedEquityMode"];
export const EQUITY_MODES = ["PAPER", "SANDBOX"] as const;
export type EquityModesMatchContract = Assert<Same<(typeof EQUITY_MODES)[number], EquityMode>>;

export type EquitySide = Schemas["AdvancedEquitySide"];
export const EQUITY_SIDES = ["BUY", "SELL"] as const;
export type EquitySidesMatchContract = Assert<Same<(typeof EQUITY_SIDES)[number], EquitySide>>;

export type EquityOrderType = Schemas["AdvancedEquityOrderType"];
export const EQUITY_ORDER_TYPES = ["MARKET", "LIMIT"] as const;
export type EquityOrderTypesMatchContract = Assert<Same<(typeof EQUITY_ORDER_TYPES)[number], EquityOrderType>>;

export type EquityOrderStatus = Schemas["AdvancedEquityOrderStatus"];
export const EQUITY_ORDER_STATUSES = ["ACCEPTED", "REJECTED", "OPEN", "FILLED", "CANCELLED", "EXPIRED"] as const;
export type EquityOrderStatusesMatchContract = Assert<Same<(typeof EQUITY_ORDER_STATUSES)[number], EquityOrderStatus>>;

export type EquitySession = Schemas["AdvancedEquitySession"];
export const EQUITY_SESSIONS = ["CLOSED", "PRE_MARKET", "REGULAR", "AFTER_HOURS"] as const;
export type EquitySessionsMatchContract = Assert<Same<(typeof EQUITY_SESSIONS)[number], EquitySession>>;

export type EquityRiskClass = Schemas["AdvancedEquityRiskClass"];
export const EQUITY_RISK_CLASSES = ["MAJOR", "STANDARD", "SPECULATIVE"] as const;
export type EquityRiskClassesMatchContract = Assert<Same<(typeof EQUITY_RISK_CLASSES)[number], EquityRiskClass>>;

export type EquityActionType = Schemas["AdvancedEquityActionType"];
export const EQUITY_ACTION_TYPES = ["SPLIT", "REVERSE_SPLIT", "CASH_DIVIDEND", "SYMBOL_CHANGE", "DELISTING"] as const;
export type EquityActionTypesMatchContract = Assert<Same<(typeof EQUITY_ACTION_TYPES)[number], EquityActionType>>;

export type EquityActionStatus = Schemas["AdvancedEquityActionStatus"];
export const EQUITY_ACTION_STATUSES = ["ANNOUNCED", "APPLYING", "APPLIED", "CANCELLED"] as const;
export type EquityActionStatusesMatchContract = Assert<Same<(typeof EQUITY_ACTION_STATUSES)[number], EquityActionStatus>>;

export type EquityRounding = Schemas["AdvancedEquityRounding"];
export const EQUITY_ROUNDINGS = ["SHARES_DOWN_CASH_DOWN", "SHARES_DOWN_CASH_HALF_EVEN"] as const;
export type EquityRoundingsMatchContract = Assert<Same<(typeof EQUITY_ROUNDINGS)[number], EquityRounding>>;

export type EquitySettlementState = Schemas["AdvancedEquitySettlementState"];
export const EQUITY_SETTLEMENT_STATES = ["PENDING", "SETTLED"] as const;
export type EquitySettlementStatesMatchContract = Assert<
  Same<(typeof EQUITY_SETTLEMENT_STATES)[number], EquitySettlementState>
>;

export type EquitySettlementFilter = Schemas["AdvancedEquitySettlementFilter"];
export const EQUITY_SETTLEMENT_FILTERS = ["PENDING", "SETTLED", "ALL"] as const;
export type EquitySettlementFiltersMatchContract = Assert<
  Same<(typeof EQUITY_SETTLEMENT_FILTERS)[number], EquitySettlementFilter>
>;

export const EQUITY_VENUE_STATUSES = ["OPEN", "FILLED", "REJECTED", "CANCELLED", "EXPIRED"] as const;
export type EquityVenueStatusesMatchContract = Assert<
  Same<(typeof EQUITY_VENUE_STATUSES)[number], Schemas["AdvancedEquityVenueStatus"]>
>;

export const EQUITY_PLACEMENT_STATUSES = ["PLACED", "NOT_PLACED"] as const;
export type EquityPlacementStatusesMatchContract = Assert<
  Same<(typeof EQUITY_PLACEMENT_STATUSES)[number], Schemas["AdvancedEquityPlacementStatus"]>
>;

export const EQUITY_VENUE_ACTIONS = ["PLACE", "POLL"] as const;
export type EquityVenueActionsMatchContract = Assert<
  Same<(typeof EQUITY_VENUE_ACTIONS)[number], Schemas["AdvancedEquityVenueAction"]>
>;

/* --------------------------------------------------------------------------
 * Words
 * ------------------------------------------------------------------------ */

/** The sessions of the XNYS-style calendar, in New York time, as internal/equities defines them. */
export const SESSION_LABEL: Readonly<Record<EquitySession, string>> = {
  CLOSED: "Closed",
  PRE_MARKET: "Pre-market, 04:00 to 09:30 New York time",
  REGULAR: "Regular session, 09:30 to 16:00 New York time",
  AFTER_HOURS: "After hours, 16:00 to 20:00 New York time",
};

/** What each order status means. */
export const ORDER_STATUS_MEANING: Readonly<Record<EquityOrderStatus, string>> = {
  ACCEPTED: "Accepted and reserved, not yet placed at the venue. An operator's venue run places it.",
  OPEN: "Resting at the venue until it fills, is cancelled or expires at the end of its day.",
  FILLED: "Filled at the venue; the trade is posted and settles on its settlement date.",
  REJECTED: "Refused, with the reason recorded; whatever it reserved was released.",
  CANCELLED: "Cancelled at the venue before it filled; its reservation was released.",
  EXPIRED: "Its day ended before it filled; its reservation was released.",
};

/** What each settlement state means. */
export const SETTLEMENT_STATE_MEANING: Readonly<Record<EquitySettlementState, string>> = {
  PENDING:
    "Not settled yet: held as unsettled until the settlement date, and not spendable, sellable or withdrawable before an operator's settlement run settles it.",
  SETTLED: "Settled: moved into the account's wallet, where it is spendable.",
};

/** The trade-date accounting every equities page that shows a trade relies on. */
export const T_PLUS_ONE_NOTE =
  "Trades settle T+1: one trading day after the trade date. A buyer pays on the trade date and the shares wait as unsettled; " +
  "a seller's shares leave on the trade date and the proceeds, net of the disclosed spread and fees, wait as unsettled. " +
  "Neither is spendable until the settlement date's settlement run settles it.";

/** How the paper venue fills, in the service's own terms. */
export const PAPER_VENUE_NOTE =
  "The paper venue is deterministic: a market order fills at the latest mark moved against the order by the disclosed " +
  "slippage, a limit order only when the mark crosses its limit. Every order is a day order.";

export const ACTION_TYPE_LABEL: Readonly<Record<EquityActionType, string>> = {
  SPLIT: "Split",
  REVERSE_SPLIT: "Reverse split",
  CASH_DIVIDEND: "Dividend, paid in the settlement asset",
  SYMBOL_CHANGE: "Symbol change",
  DELISTING: "Delisting",
};

export const ACTION_STATUS_MEANING: Readonly<Record<EquityActionStatus, string>> = {
  ANNOUNCED: "Announced; nothing has been posted for it.",
  APPLYING: "Partly applied: a delisting that has begun and has not reached its final date.",
  APPLIED: "Applied; each holder's posting is listed with it.",
  CANCELLED: "Withdrawn before it was applied; nothing was posted.",
};

export const ROUNDING_LABEL: Readonly<Record<EquityRounding, string>> = {
  SHARES_DOWN_CASH_DOWN: "Shares rounded down; the amount paid in lieu of a fraction rounded down",
  SHARES_DOWN_CASH_HALF_EVEN: "Shares rounded down; the amount paid in lieu of a fraction rounded half to even",
};

export const RISK_CLASS_LABEL: Readonly<Record<EquityRiskClass, string>> = {
  MAJOR: "Major",
  STANDARD: "Standard",
  SPECULATIVE: "Speculative",
};

/* --------------------------------------------------------------------------
 * What a status or a session lets a person do (the backend decides)
 * ------------------------------------------------------------------------ */

/** A live order — accepted or resting — is the only kind a cancel can end. */
export function isLive(status: string): boolean {
  return status === "ACCEPTED" || status === "OPEN";
}

/**
 * What the session in force says about an order of this shape, as
 * internal/equities documents its rule: a market order only in the regular
 * session; a limit order in the regular session, or in pre-market and after
 * hours as well when it asks for extended hours. Undefined when the session
 * admits it. The session can change after the page read it, so this is a
 * note beside the form; the backend decides when the order arrives.
 */
export function sessionNote(session: string, orderType: EquityOrderType, extendedHours: boolean): string | undefined {
  if (session === "REGULAR") return undefined;
  if (orderType === "MARKET") {
    return "A market order is accepted only in the regular session; outside it the backend refuses one rather than queue it.";
  }
  if (session === "PRE_MARKET" || session === "AFTER_HOURS") {
    return extendedHours ? undefined : "Outside the regular session a limit order needs extended hours, or the backend refuses it.";
  }
  return "The market is closed: the backend refuses orders until the next session opens.";
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

const PRICE_INPUT = /^[0-9]{1,30}(\.[0-9]{1,18})?$/;

/**
 * A price or a per-share amount exactly as written ("1,250.50" ->
 * "1250.50"), at most eighteen decimal places. It is never rounded: the
 * backend refuses one that is not exact at the listing's price scale or not
 * on its tick, and that refusal is what is shown.
 */
export function parsePriceInput(raw: string, what = "a price"): InputParse {
  const cleaned = raw.trim().replace(/,/g, "");
  if (cleaned === "") return { ok: false, value: "", error: `Enter ${what}.` };
  if (!PRICE_INPUT.test(cleaned)) {
    return { ok: false, value: "", error: `Write ${what} with digits and at most one decimal point.` };
  }
  if (/^[0.]+$/.test(cleaned)) return { ok: false, value: "", error: `${capitalise(what)} is greater than zero.` };
  return { ok: true, value: cleaned, error: "" };
}

function capitalise(text: string): string {
  return text === "" ? text : `${text.slice(0, 1).toUpperCase()}${text.slice(1)}`;
}

const DATE_INPUT = /^[0-9]{4}-[0-9]{2}-[0-9]{2}$/;

/** A calendar day written YYYY-MM-DD that exists (no 30th of February). */
export function parseDateInput(raw: string): InputParse {
  const cleaned = raw.trim();
  if (cleaned === "") return { ok: false, value: "", error: "Enter a date." };
  if (!DATE_INPUT.test(cleaned)) return { ok: false, value: "", error: "Write the date as YYYY-MM-DD." };
  const day = new Date(`${cleaned}T00:00:00Z`);
  if (isNaN(day.getTime()) || day.toISOString().slice(0, 10) !== cleaned) {
    return { ok: false, value: "", error: "That day does not exist." };
  }
  return { ok: true, value: cleaned, error: "" };
}

const DIGITS = "0123456789";

export interface IntegerParse {
  readonly ok: boolean;
  readonly value: number;
  readonly error: string;
}

/**
 * A whole number within [min, max] for a parameter — decimals, a scale, a
 * ratio term, a limit — never an amount. Accumulated digit by digit (at most
 * twelve, far inside exact integer range) rather than by a numeric parser.
 */
export function parseWholeNumber(raw: string, min: number, max: number, what: string): IntegerParse {
  const cleaned = raw.trim().replace(/,/g, "");
  if (!/^[0-9]{1,12}$/.test(cleaned)) return { ok: false, value: 0, error: `Enter ${what} as a whole number.` };
  let value = 0;
  for (const ch of cleaned) value = value * 10 + DIGITS.indexOf(ch);
  if (value < min || value > max) {
    return { ok: false, value: 0, error: `Use ${what} from ${String(min)} to ${String(max)}.` };
  }
  return { ok: true, value, error: "" };
}

/** A whole number of base units, at least one, as an integer string ("0100" -> "100"). */
export function parseBaseUnitsInput(raw: string, what: string): InputParse {
  const cleaned = raw.trim().replace(/,/g, "");
  if (!/^[0-9]{1,38}$/.test(cleaned)) return { ok: false, value: "", error: `Enter ${what} as a whole number of base units.` };
  let start = 0;
  while (start < cleaned.length - 1 && cleaned[start] === "0") start = start + 1;
  const value = cleaned.slice(start);
  if (value === "0") return { ok: false, value: "", error: `${capitalise(what)} is at least one base unit.` };
  return { ok: true, value, error: "" };
}

/** A listing's symbol as internal/equities accepts one. */
export function parseSymbolInput(raw: string): InputParse {
  const cleaned = raw.trim().toUpperCase();
  if (!/^[A-Z0-9][A-Z0-9._/-]{0,63}$/.test(cleaned)) {
    return { ok: false, value: "", error: "A symbol is letters and digits, optionally with . _ / -, at most 64 characters." };
  }
  return { ok: true, value: cleaned, error: "" };
}

/** A required free-text reason, as every operator command records one. */
export function reasonProblem(raw: string): string | undefined {
  return raw.trim() === "" ? "Give the reason this is being done; it is recorded with it." : undefined;
}

/* --------------------------------------------------------------------------
 * Refusals
 * ------------------------------------------------------------------------ */

/** A field a refusal carries as a non-empty string, such as order_id, rejection_code or intent_id. */
export function refusalField(fields: Readonly<Record<string, unknown>> | undefined, name: string): string | undefined {
  const value = fields?.[name];
  return typeof value === "string" && value !== "" ? value : undefined;
}
