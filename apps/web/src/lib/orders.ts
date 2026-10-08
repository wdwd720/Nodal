/**
 * Pure helpers for the order-book section of the advanced (sandbox) area: the
 * persistent internal book of internal/orders (ADR-0020).
 *
 * Nothing here computes a financial figure. Every price, quantity, notional
 * and fee on an order-book page is a string the backend computed; these
 * functions name things, say what a status lets a person do (the backend
 * still decides every request), say whether what a person typed is exactly
 * what the API accepts, and arrange the levels the backend returned into a
 * ladder without adding them up.
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

export type BookMode = Schemas["AdvancedBookMode"];
export const BOOK_MODES = ["PAPER", "SANDBOX"] as const;
export type BookModesMatchContract = Assert<Same<(typeof BOOK_MODES)[number], BookMode>>;

export type BookStatus = Schemas["AdvancedBookStatus"];
export const BOOK_STATUSES = ["OPEN", "HALTED", "CLOSED"] as const;
export type BookStatusesMatchContract = Assert<Same<(typeof BOOK_STATUSES)[number], BookStatus>>;

export const BOOK_VENUE_LABELS = ["SIMULATED_INTERNAL_BOOK"] as const;
export type BookVenueLabelsMatchContract = Assert<
  Same<(typeof BOOK_VENUE_LABELS)[number], Schemas["AdvancedBookVenueLabel"]>
>;

export const BOOK_TERMS_SOURCES = ["CANONICAL", "BOOK_SPOT"] as const;
export type BookTermsSourcesMatchContract = Assert<
  Same<(typeof BOOK_TERMS_SOURCES)[number], Schemas["AdvancedBookTermsSource"]>
>;

export type BookSide = Schemas["AdvancedBookSide"];
export const BOOK_SIDES = ["BUY", "SELL"] as const;
export type BookSidesMatchContract = Assert<Same<(typeof BOOK_SIDES)[number], BookSide>>;

export type BookOrderType = Schemas["AdvancedBookOrderType"];
export const BOOK_ORDER_TYPES = ["LIMIT", "MARKET"] as const;
export type BookOrderTypesMatchContract = Assert<Same<(typeof BOOK_ORDER_TYPES)[number], BookOrderType>>;

export type BookTimeInForce = Schemas["AdvancedBookTimeInForce"];
export const BOOK_TIMES_IN_FORCE = ["GTC", "IOC", "FOK"] as const;
export type BookTimesInForceMatchContract = Assert<Same<(typeof BOOK_TIMES_IN_FORCE)[number], BookTimeInForce>>;

export type BookStp = Schemas["AdvancedBookSTP"];
export const BOOK_STP_MODES = ["CANCEL_TAKER", "CANCEL_MAKER", "CANCEL_BOTH"] as const;
export type BookStpModesMatchContract = Assert<Same<(typeof BOOK_STP_MODES)[number], BookStp>>;

export const BOOK_LIQUIDITY_INTENTS = ["TAKE_LIQUIDITY", "PROVIDE_LIQUIDITY"] as const;
export type BookLiquidityIntentsMatchContract = Assert<
  Same<(typeof BOOK_LIQUIDITY_INTENTS)[number], Schemas["AdvancedBookLiquidityIntent"]>
>;

export type BookOrderStatus = Schemas["AdvancedBookOrderStatus"];
export const BOOK_ORDER_STATUSES = ["PENDING", "OPEN", "PARTIALLY_FILLED", "FILLED", "CANCELLED", "REJECTED"] as const;
export type BookOrderStatusesMatchContract = Assert<Same<(typeof BOOK_ORDER_STATUSES)[number], BookOrderStatus>>;

export type BookOrderScope = Schemas["AdvancedBookOrderScope"];
export const BOOK_ORDER_SCOPES = ["WORKING", "ALL"] as const;
export type BookOrderScopesMatchContract = Assert<Same<(typeof BOOK_ORDER_SCOPES)[number], BookOrderScope>>;

export const BOOK_LIQUIDITY = ["MAKER", "TAKER"] as const;
export type BookLiquidityMatchContract = Assert<Same<(typeof BOOK_LIQUIDITY)[number], Schemas["AdvancedBookLiquidity"]>>;

export const BOOK_OWNER_TYPES = ["CUSTOMER", "HOUSE"] as const;
export type BookOwnerTypesMatchContract = Assert<Same<(typeof BOOK_OWNER_TYPES)[number], Schemas["AdvancedBookOwnerType"]>>;

/* --------------------------------------------------------------------------
 * Words
 * ------------------------------------------------------------------------ */

/** What each book status lets anyone do, as internal/orders documents it. */
export const BOOK_STATUS_MEANING: Readonly<Record<BookStatus, string>> = {
  OPEN: "Accepting orders, cancels and replacements, and matching them.",
  HALTED: "Matching is stopped: new orders and replacements are refused, and resting orders can only be cancelled.",
  CLOSED: "The book accepts nothing. A book closes only once no order on it is working.",
};

/** What each order status means. */
export const ORDER_STATUS_MEANING: Readonly<Record<BookOrderStatus, string>> = {
  PENDING: "Accepted and recorded; it has not reached the book yet, and a cancel now ends it before it does.",
  OPEN: "Resting on the book with nothing filled.",
  PARTIALLY_FILLED: "Resting on the book with part of its quantity filled.",
  FILLED: "Every unit of its quantity traded.",
  CANCELLED: "Ended before it filled completely; what did not trade was released.",
  REJECTED: "Refused, with the reason recorded on the order. Nothing of it traded.",
};

export const TIME_IN_FORCE_LABEL: Readonly<Record<BookTimeInForce, string>> = {
  GTC: "Good till cancelled — rests on the book until it fills or is cancelled",
  IOC: "Immediate or cancel — trades what it can now, the rest is cancelled",
  FOK: "Fill or kill — trades its whole quantity now or nothing at all",
};

export const STP_LABEL: Readonly<Record<BookStp, string>> = {
  CANCEL_TAKER: "Cancel the incoming order if it would trade with your own resting order",
  CANCEL_MAKER: "Cancel your resting order if the incoming one would trade with it",
  CANCEL_BOTH: "Cancel both orders if they would trade with each other",
};

export const TERMS_SOURCE_LABEL: Readonly<Record<Schemas["AdvancedBookTermsSource"], string>> = {
  CANONICAL: "The instrument's canonical terms, at the version shown",
  BOOK_SPOT: "Spot terms the operator stated when the book was opened",
};

/** What the venue label on every book, order and fill says. */
export const INTERNAL_BOOK_NOTE =
  "SIMULATED_INTERNAL_BOOK: a book this deployment runs itself. Orders match only against other orders on it, " +
  "every trade is posted to the ledger in simulated assets, and nothing reaches an exchange.";

/** What the crossing rule is, wherever it is shown. */
export const CROSSING_RULE_NOTE =
  "Whether an order of the house may trade against a customer's order on this book. Forbidden unless a rule " +
  "recorded through the house book's treasury controls allows it; every change is a new version, with its " +
  "reason and who made it, and every trade names the version it was matched under.";

/* --------------------------------------------------------------------------
 * What a status lets a person do (the backend decides every request)
 * ------------------------------------------------------------------------ */

/** Statuses the account's "working" list holds: not yet ended. */
export function isWorking(status: string): boolean {
  return status === "PENDING" || status === "OPEN" || status === "PARTIALLY_FILLED";
}

/** Only an order resting on the book is cancelled or replaced. */
export function isResting(status: string): boolean {
  return status === "OPEN" || status === "PARTIALLY_FILLED";
}

export interface Availability {
  readonly available: boolean;
  /** Why not, when not. */
  readonly reason: string;
}

/**
 * Whether a cancel is offered: for a working order, on a book that is not
 * closed. A pending order is cancelled before it reaches the book, a resting
 * one is taken off it — on a halted book too.
 */
export function cancelAvailability(orderStatus: string, bookStatus: string | undefined): Availability {
  if (!isWorking(orderStatus)) {
    return { available: false, reason: `The order is ${orderStatus}; only a working order is cancelled.` };
  }
  if (bookStatus === "CLOSED") return { available: false, reason: "The book is closed and accepts nothing." };
  return { available: true, reason: "" };
}

/** Whether a replacement is offered: a resting order on an OPEN book. */
export function replaceAvailability(orderStatus: string, bookStatus: string | undefined): Availability {
  if (!isResting(orderStatus)) {
    return { available: false, reason: `The order is ${orderStatus}; only a resting order is replaced.` };
  }
  if (bookStatus !== undefined && bookStatus !== "OPEN") {
    return { available: false, reason: `The book is ${bookStatus}; replacements need an OPEN book.` };
  }
  return { available: true, reason: "" };
}

/** Whether new orders are offered on a book. */
export function orderEntryAvailability(bookStatus: string): Availability {
  if (bookStatus !== "OPEN") {
    return { available: false, reason: `The book is ${bookStatus}: ${BOOK_STATUS_MEANING[bookStatusOrOpen(bookStatus)]}` };
  }
  return { available: true, reason: "" };
}

function bookStatusOrOpen(status: string): BookStatus {
  return BOOK_STATUSES.find((s) => s === status) ?? "OPEN";
}

/** The statuses an operator may move a book to from `from`, mirroring the service's transitions. */
export function bookTransitions(from: string): readonly BookStatus[] {
  switch (from) {
    case "OPEN":
      return ["HALTED", "CLOSED"];
    case "HALTED":
      return ["OPEN", "CLOSED"];
    default:
      return [];
  }
}

/**
 * Whether an order shape is one the book accepts, as internal/orders states
 * its own rules. A shape it refuses is shown as a reason before anything is
 * sent; the backend still checks every order.
 */
export function orderShapeProblem(shape: {
  readonly side: BookSide;
  readonly orderType: BookOrderType;
  readonly timeInForce: BookTimeInForce;
  readonly postOnly: boolean;
  readonly reduceOnly: boolean;
}): string | undefined {
  if (shape.orderType === "MARKET" && shape.timeInForce === "GTC") {
    return "A market order never rests on the book: use IOC or FOK.";
  }
  if (shape.postOnly && (shape.orderType !== "LIMIT" || shape.timeInForce !== "GTC")) {
    return "Post-only is a good-till-cancelled limit order.";
  }
  if (shape.reduceOnly && shape.side === "BUY") {
    return "Spot has no short position: reduce-only is a sell capped at what the account holds.";
  }
  return undefined;
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

const PRICE_INPUT = /^[0-9]{1,38}(\.[0-9]{1,38})?$/;

/**
 * A price exactly as written ("2,500.00" -> "2500.00"). It is never rounded:
 * the backend refuses a price that is not exact at the book's price scale or
 * not on its tick, and that refusal is what is shown.
 */
export function parsePriceInput(raw: string): InputParse {
  const cleaned = raw.trim().replace(/,/g, "");
  if (cleaned === "") return { ok: false, value: "", error: "Enter a price." };
  if (!PRICE_INPUT.test(cleaned)) {
    return { ok: false, value: "", error: "Write the price with digits and at most one decimal point." };
  }
  if (/^[0.]+$/.test(cleaned)) return { ok: false, value: "", error: "A price is greater than zero." };
  return { ok: true, value: cleaned, error: "" };
}

/** A client order id as internal/orders accepts one: 1 to 128 bytes, no surrounding space, no control characters. */
export function parseClientOrderId(raw: string): InputParse {
  if (raw === "") return { ok: true, value: "", error: "" };
  if (raw.trim() !== raw) return { ok: false, value: "", error: "A client order id has no surrounding spaces." };
  if (new TextEncoder().encode(raw).length > 128) {
    return { ok: false, value: "", error: "A client order id is at most 128 bytes." };
  }
  for (const ch of raw) {
    const code = ch.codePointAt(0) ?? 0;
    if (code < 32 || (code >= 127 && code < 160)) {
      return { ok: false, value: "", error: "A client order id has no control characters." };
    }
  }
  return { ok: true, value: raw, error: "" };
}

/** A required free-text reason, as every operator command records one. */
export function reasonProblem(raw: string): string | undefined {
  return raw.trim() === "" ? "Give the reason this is being done; it is recorded with the change." : undefined;
}

const DIGITS = "0123456789";

export interface IntegerParse {
  readonly ok: boolean;
  /** The whole number when `ok`. */
  readonly value: number;
  readonly error: string;
}

/**
 * A whole number within [min, max] for a parameter such as basis points or a
 * snapshot interval — never an amount. The digits are accumulated one by one
 * (at most nine, far inside exact integer range) rather than handed to a
 * numeric parser.
 */
export function parseWholeNumber(raw: string, min: number, max: number, what: string): IntegerParse {
  const cleaned = raw.trim().replace(/,/g, "");
  if (!/^[0-9]{1,9}$/.test(cleaned)) {
    return { ok: false, value: 0, error: `Enter ${what} as a whole number.` };
  }
  let value = 0;
  for (const ch of cleaned) value = value * 10 + DIGITS.indexOf(ch);
  if (value < min || value > max) {
    return { ok: false, value: 0, error: `Use a value from ${String(min)} to ${String(max)}.` };
  }
  return { ok: true, value, error: "" };
}

/**
 * A trade's sequence number as written in an address: a positive whole
 * number with no leading zero, at most fifteen digits — exactly representable
 * — accumulated digit by digit. Undefined for anything else.
 */
export function parseSequence(raw: string | undefined): number | undefined {
  if (raw === undefined || !/^[1-9][0-9]{0,14}$/.test(raw)) return undefined;
  let value = 0;
  for (const ch of raw) value = value * 10 + DIGITS.indexOf(ch);
  return value;
}

/**
 * Spot terms pasted by an operator as a JSON object. Only the shape is checked
 * here — an object, not an array or a scalar; every term is the backend's to
 * validate, and its refusal names the term.
 */
export function parseTermsJson(raw: string): { readonly ok: boolean; readonly value: Record<string, unknown> | undefined; readonly error: string } {
  if (raw.trim() === "") return { ok: true, value: undefined, error: "" };
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { ok: false, value: undefined, error: "The spot terms are not valid JSON." };
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return { ok: false, value: undefined, error: "The spot terms are one JSON object of instrument terms." };
  }
  return { ok: true, value: parsed as Record<string, unknown>, error: "" };
}

/* --------------------------------------------------------------------------
 * The depth ladder
 * ------------------------------------------------------------------------ */

export interface LadderRow<L> {
  readonly side: BookSide;
  readonly level: L;
  /** 1 for the best level of its side. */
  readonly rank: number;
}

/**
 * The ladder as it is read top to bottom: the asks from the furthest level
 * down to the best one, then the bids from the best one down. The backend
 * returns each side best level first; nothing is summed or recomputed.
 */
export function ladderRows<L>(depth: { readonly bids: readonly L[]; readonly asks: readonly L[] }): LadderRow<L>[] {
  const asks = depth.asks.map((level, index) => ({ side: "SELL" as const, level, rank: index + 1 }));
  const bids = depth.bids.map((level, index) => ({ side: "BUY" as const, level, rank: index + 1 }));
  return [...asks.reverse(), ...bids];
}

/* --------------------------------------------------------------------------
 * Refusals
 * ------------------------------------------------------------------------ */

/** The order a recorded refusal names, so the page can link to it. */
export function refusedOrderId(fields: Readonly<Record<string, unknown>> | undefined): string | undefined {
  const value = fields?.["order_id"];
  return typeof value === "string" && value !== "" ? value : undefined;
}

/** The stable reason internal/orders attached to a refusal, when it attached one. */
export function refusalReason(fields: Readonly<Record<string, unknown>> | undefined): string | undefined {
  const value = fields?.["reason"];
  return typeof value === "string" && value !== "" ? value : undefined;
}
