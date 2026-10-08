/**
 * Pure helpers for the treasury section of the advanced area: the house book
 * (internal/treasury, ADR-0020) and the operator's commands on it.
 *
 * Nothing here computes a financial figure. These functions name things,
 * check what an operator typed before it is sent, and read a refusal's
 * fields; every amount on a treasury page is a string the treasury computed,
 * which the page only formats (PART 110).
 *
 * The enumerations repeat values the OpenAPI contract declares, because the
 * runtime validator needs them as values and the generated client only has
 * them as types. Each list is tied to its generated type at compile time
 * (`Same`), so when the contract gains or loses a value `tsc` fails here.
 */
import { ApiProblem, type Schemas } from "@controlplane/generated-client";

import { formatQuantity } from "./money.ts";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export type TreasuryClass = Schemas["TreasuryClass"];
export const TREASURY_CLASSES = ["PRINCIPAL", "TREASURY"] as const;
export type TreasuryClassesMatchContract = Assert<Same<(typeof TREASURY_CLASSES)[number], TreasuryClass>>;

export type TreasuryStrategy = Schemas["TreasuryStrategy"];
export const TREASURY_STRATEGIES = [
  "LIQUIDITY_PROVISION",
  "MARKET_MAKING",
  "HEDGING",
  "INVENTORY_REBALANCING",
  "TREASURY_CONVERSION",
] as const;
export type TreasuryStrategiesMatchContract = Assert<Same<(typeof TREASURY_STRATEGIES)[number], TreasuryStrategy>>;

export type TreasuryBookStatus = Schemas["TreasuryBookStatus"];
export const TREASURY_BOOK_STATUSES = ["ACTIVE", "KILLED", "CLOSED"] as const;
export type TreasuryBookStatusesMatchContract = Assert<Same<(typeof TREASURY_BOOK_STATUSES)[number], TreasuryBookStatus>>;

export type TreasuryActionKind = Schemas["TreasuryActionKind"];
export const TREASURY_ACTION_KINDS = [
  "REGISTER",
  "SET_POLICY",
  "SET_SIGNING_POLICY",
  "FUND",
  "PLACE_ORDER",
  "CANCEL_ORDER",
  "PAPER_FILL",
  "MARK_TO_MARKET",
] as const;
export type TreasuryActionKindsMatchContract = Assert<Same<(typeof TREASURY_ACTION_KINDS)[number], TreasuryActionKind>>;

export type TreasuryOutcome = Schemas["TreasuryOutcome"];
export const TREASURY_OUTCOMES = ["APPLIED", "REJECTED"] as const;
export type TreasuryOutcomesMatchContract = Assert<Same<(typeof TREASURY_OUTCOMES)[number], TreasuryOutcome>>;

export type TreasurySide = Schemas["TreasurySide"];
export const TREASURY_SIDES = ["BUY", "SELL"] as const;
export type TreasurySidesMatchContract = Assert<Same<(typeof TREASURY_SIDES)[number], TreasurySide>>;

export type TreasuryOrderType = Schemas["TreasuryOrderType"];
export const TREASURY_ORDER_TYPES = ["LIMIT", "MARKET"] as const;
export type TreasuryOrderTypesMatchContract = Assert<Same<(typeof TREASURY_ORDER_TYPES)[number], TreasuryOrderType>>;

export type TreasuryTimeInForce = Schemas["TreasuryTimeInForce"];
export const TREASURY_TIMES_IN_FORCE = ["GTC", "IOC", "FOK"] as const;
export type TreasuryTimesInForceMatchContract = Assert<Same<(typeof TREASURY_TIMES_IN_FORCE)[number], TreasuryTimeInForce>>;

export type TreasuryOrderStatus = Schemas["TreasuryOrderStatus"];
export const TREASURY_ORDER_STATUSES = ["PENDING", "OPEN", "PARTIALLY_FILLED", "FILLED", "CANCELLED", "REJECTED"] as const;
export type TreasuryOrderStatusesMatchContract = Assert<Same<(typeof TREASURY_ORDER_STATUSES)[number], TreasuryOrderStatus>>;

export type TreasuryOwnerType = Schemas["TreasuryOwnerType"];
export const TREASURY_OWNER_TYPES = ["CUSTOMER", "HOUSE"] as const;
export type TreasuryOwnerTypesMatchContract = Assert<Same<(typeof TREASURY_OWNER_TYPES)[number], TreasuryOwnerType>>;

export type TreasuryEventKind = Schemas["TreasuryEventKind"];
export const TREASURY_EVENT_KINDS = ["FUNDING", "BOOK_FILL", "PAPER_FILL"] as const;
export type TreasuryEventKindsMatchContract = Assert<Same<(typeof TREASURY_EVENT_KINDS)[number], TreasuryEventKind>>;

export type TreasuryDirection = Schemas["TreasuryDirection"];
export const TREASURY_DIRECTIONS = ["INCREASE", "DECREASE"] as const;
export type TreasuryDirectionsMatchContract = Assert<Same<(typeof TREASURY_DIRECTIONS)[number], TreasuryDirection>>;

export type TreasuryVerdict = Schemas["TreasuryVerdict"];
export const TREASURY_VERDICTS = ["ALLOW", "REJECT"] as const;
export type TreasuryVerdictsMatchContract = Assert<Same<(typeof TREASURY_VERDICTS)[number], TreasuryVerdict>>;

export type TreasuryLimitKind = Schemas["TreasuryLimitKind"];
export const TREASURY_LIMIT_KINDS = ["DAILY_LOSS", "INVENTORY", "INSTRUMENT_EXPOSURE"] as const;
export type TreasuryLimitKindsMatchContract = Assert<Same<(typeof TREASURY_LIMIT_KINDS)[number], TreasuryLimitKind>>;

/**
 * The modes the house paper venue and fundings run in. The contract's mode
 * type admits DEVNET and LIVE as well; the treasury refuses them, so the
 * forms never offer them.
 */
export const TREASURY_RUN_MODES = ["PAPER", "SANDBOX"] as const satisfies readonly Schemas["AdvancedMode"][];
export type TreasuryRunMode = (typeof TREASURY_RUN_MODES)[number];

/** The activities a two-sided quote may be placed under. */
export const QUOTING_ACTIVITIES = ["MARKET_MAKING", "LIQUIDITY_PROVISION"] as const satisfies readonly TreasuryStrategy[];

/* --------------------------------------------------------------------------
 * What things mean
 * ------------------------------------------------------------------------ */

export type Tone = "neutral" | "good" | "warn" | "bad" | "info";

export const CLASS_MEANING: Readonly<Record<TreasuryClass, string>> = {
  PRINCIPAL: "Trades as principal: liquidity provision, market making and hedging.",
  TREASURY: "Manages the platform's own sandbox inventory: rebalancing and conversion.",
};

export const STRATEGY_MEANING: Readonly<Record<TreasuryStrategy, string>> = {
  LIQUIDITY_PROVISION: "Rests orders on the internal book so that others can trade.",
  MARKET_MAKING: "Quotes both sides of the internal book.",
  HEDGING: "Offsets the book's exposure on the internal book or the simulated paper venue.",
  INVENTORY_REBALANCING: "Moves a holding to a target on the simulated paper venue.",
  TREASURY_CONVERSION: "Converts between the valuation asset and one other sandbox asset on the simulated paper venue.",
};

export const BOOK_STATUS_MEANING: Readonly<Record<TreasuryBookStatus, string>> = {
  ACTIVE: "The house book may act, within its policies.",
  KILLED: "Stopped: the house book takes no new risk.",
  CLOSED: "Closed: the house book acts no more.",
};

export const ACTION_KIND_MEANING: Readonly<Record<TreasuryActionKind, string>> = {
  REGISTER: "The house book was registered with its class, valuation asset and first signing policy.",
  SET_POLICY: "A new version of the risk limits was recorded.",
  SET_SIGNING_POLICY: "A new version of the signing policy was recorded.",
  FUND: "Simulated house capital was injected into the book.",
  PLACE_ORDER: "A house order was sent to the internal book after every treasury control.",
  CANCEL_ORDER: "A resting house order was cancelled.",
  PAPER_FILL: "An immediate fill on the simulated paper venue, its spread and fee disclosed.",
  MARK_TO_MARKET: "The book's PnL report was stored as a hashed snapshot.",
};

export const LIMIT_KIND_LABEL: Readonly<Record<TreasuryLimitKind, string>> = {
  DAILY_LOSS: "Daily loss",
  INVENTORY: "Inventory",
  INSTRUMENT_EXPOSURE: "Instrument exposure",
};

export const LIMIT_KIND_MEANING: Readonly<Record<TreasuryLimitKind, string>> = {
  DAILY_LOSS:
    "Used is the loss measure: the realized PnL of the UTC day plus every open loss at a fresh mark (negative is a loss). New risk stops while it is below the negative of the cap.",
  INVENTORY: "Used is what the book holds of the asset plus every working buy. An asset without a cap cannot be accumulated.",
  INSTRUMENT_EXPOSURE:
    "Used is the base held at a fresh mark plus the working buy notional on the instrument. Without a fresh mark it cannot be known.",
};

export const EVENT_KIND_MEANING: Readonly<Record<TreasuryEventKind, string>> = {
  FUNDING: "Simulated house capital entered the register.",
  BOOK_FILL: "A trade on the internal book, ingested exactly once.",
  PAPER_FILL: "An immediate fill on the simulated paper venue.",
};

export function outcomeTone(outcome: string): Tone {
  return outcome === "APPLIED" ? "good" : outcome === "REJECTED" ? "bad" : "neutral";
}

export function bookStatusTone(status: string): Tone {
  return status === "ACTIVE" ? "good" : status === "KILLED" ? "bad" : "neutral";
}

export function orderStatusTone(status: string): Tone {
  switch (status) {
    case "FILLED":
      return "good";
    case "OPEN":
    case "PARTIALLY_FILLED":
    case "PENDING":
      return "info";
    case "REJECTED":
      return "bad";
    default:
      return "neutral";
  }
}

export function verdictTone(verdict: string): Tone {
  return verdict === "ALLOW" ? "good" : verdict === "REJECT" ? "bad" : "neutral";
}

/* --------------------------------------------------------------------------
 * Limits in use
 * ------------------------------------------------------------------------ */

/** Where one limit stands, read from the treasury's own evaluation. */
export type LimitState = "room" | "refusing" | "unknown";

/**
 * A limit refuses new risk when the treasury's evaluation names a code for
 * it; it leaves room when it names none and the use is known; and it is
 * unknown when the treasury could not state the use (no fresh mark). The
 * codes decide, never a comparison made here.
 */
export function limitState(limit: { readonly codes: readonly string[]; readonly used?: string | undefined }): LimitState {
  if (limit.codes.length > 0) return "refusing";
  if (limit.used === undefined) return "unknown";
  return "room";
}

export const LIMIT_STATE_LABEL: Readonly<Record<LimitState, string>> = {
  room: "leaves room for new risk",
  refusing: "refuses new risk now",
  unknown: "use unknown: no fresh mark",
};

export function limitStateTone(state: LimitState): Tone {
  return state === "room" ? "good" : state === "refusing" ? "bad" : "warn";
}

/* --------------------------------------------------------------------------
 * Display
 * ------------------------------------------------------------------------ */

/**
 * An exact price, mantissa × 10^-scale, as a decimal: ("10025", 2) -> "100.25".
 * The point is moved through the digits; nothing is divided.
 */
export function formatExactPrice(mantissa: string, scale: number): string {
  return formatQuantity(mantissa, scale);
}

/* --------------------------------------------------------------------------
 * Refusals
 * ------------------------------------------------------------------------ */

/** What a recorded treasury refusal carries in its problem fields. */
export interface RefusalDetail {
  readonly reason: string | undefined;
  readonly rejectionCode: string | undefined;
  readonly reasonCodes: readonly string[];
  readonly actionId: string | undefined;
}

function text(value: unknown): string | undefined {
  return typeof value === "string" && value !== "" ? value : undefined;
}

/**
 * The stable codes of a refusal, read from the problem's fields exactly as
 * the backend sent them; undefined when the failure carries none.
 */
export function refusalDetail(error: unknown): RefusalDetail | undefined {
  if (!(error instanceof ApiProblem) || error.fields === undefined) return undefined;
  const fields = error.fields;
  const codes = Array.isArray(fields["reason_codes"])
    ? (fields["reason_codes"] as unknown[]).filter((code): code is string => typeof code === "string")
    : [];
  const detail: RefusalDetail = {
    reason: text(fields["reason"]),
    rejectionCode: text(fields["rejection_code"]),
    reasonCodes: codes,
    actionId: text(fields["action_id"]),
  };
  if (
    detail.reason === undefined &&
    detail.rejectionCode === undefined &&
    detail.reasonCodes.length === 0 &&
    detail.actionId === undefined
  ) {
    return undefined;
  }
  return detail;
}

/** One leg of a two-sided quote: applied, or refused with its codes. */
export function legOutcome(leg: {
  readonly action: { readonly outcome: string; readonly rejection_code?: string; readonly reason_codes: readonly string[] };
}): { readonly applied: boolean; readonly codes: readonly string[] } {
  if (leg.action.outcome === "APPLIED") return { applied: true, codes: [] };
  const codes = [...leg.action.reason_codes];
  if (leg.action.rejection_code !== undefined && !codes.includes(leg.action.rejection_code)) {
    codes.unshift(leg.action.rejection_code);
  }
  return { applied: false, codes };
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
 * What an operator typed, checked before it is sent
 * ------------------------------------------------------------------------ */

export type Parsed<T> = { readonly ok: true; readonly value: T } | { readonly ok: false; readonly error: string };

const DIGITS = "0123456789";
const CANONICAL_UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** A UUID in its canonical 36-character form, lower-cased. */
export function parseUuid(raw: string, what: string): Parsed<string> {
  const value = raw.trim();
  if (!CANONICAL_UUID.test(value)) {
    return { ok: false, error: `${what} is a UUID in its canonical 36-character form.` };
  }
  return { ok: true, value: value.toLowerCase() };
}

function stripLeadingZeros(digits: string): string {
  let i = 0;
  while (i < digits.length - 1 && digits[i] === "0") i = i + 1;
  return digits.slice(i);
}

/**
 * Exact base units of an asset: digits only, no sign, no point. The string is
 * kept as typed (leading zeros dropped); it is never converted to a number.
 */
export function parseBaseUnits(raw: string, what: string, allowZero = false): Parsed<string> {
  const value = raw.trim().replace(/[,_ ]/g, "");
  if (!/^[0-9]{1,38}$/.test(value)) {
    return { ok: false, error: `${what} is a whole number of base units, written in digits.` };
  }
  const canonical = stripLeadingZeros(value);
  if (!allowZero && canonical === "0") {
    return { ok: false, error: `${what} must be more than zero base units.` };
  }
  return { ok: true, value: canonical };
}

/**
 * A plain decimal price, such as the limit price of a house order: digits,
 * an optional fraction, no sign and no exponent. It is sent exactly as typed
 * (less surrounding space), and the backend reads it exactly.
 */
export function parseDecimalPrice(raw: string, what: string): Parsed<string> {
  const value = raw.trim();
  if (!/^[0-9]{1,30}(\.[0-9]{1,18})?$/.test(value)) {
    return { ok: false, error: `${what} is a plain decimal written in digits, with an optional fraction.` };
  }
  if (!/[1-9]/.test(value)) {
    return { ok: false, error: `${what} must be more than zero.` };
  }
  return { ok: true, value };
}

/**
 * A whole number between min and max, built digit by digit from at most
 * nine of them, so it is exact. It is a rate in basis points, a count or a
 * duration, never an amount.
 */
export function parseWholeNumber(raw: string, min: number, max: number, what: string): Parsed<number> {
  const value = raw.trim();
  if (!/^[0-9]{1,9}$/.test(value)) {
    return { ok: false, error: `${what} is a whole number written in digits.` };
  }
  let n = 0;
  for (const ch of value) {
    n = n * 10 + DIGITS.indexOf(ch);
  }
  if (n < min || n > max) {
    return { ok: false, error: `${what} must be between ${String(min)} and ${String(max)}.` };
  }
  return { ok: true, value: n };
}

/** A required piece of text of at most `max` characters, without surrounding space. */
export function parseText(raw: string, what: string, max: number): Parsed<string> {
  const value = raw.trim();
  if (value === "") return { ok: false, error: `${what} is required.` };
  if (value.length > max) return { ok: false, error: `${what} is at most ${String(max)} characters.` };
  return { ok: true, value };
}

/** One cap of a risk policy: an asset or instrument id and its cap in base units. */
export interface LimitLine {
  readonly id: string;
  readonly max: string;
}

/**
 * The caps of a risk policy, one per line: an id, then the cap in base
 * units, separated by spaces, "=" or ",". Empty lines are ignored; an id may
 * appear only once. An empty list is a valid answer: it names nothing, and
 * the treasury then refuses new risk in anything.
 */
export function parseLimitLines(raw: string, what: string): Parsed<LimitLine[]> {
  const out: LimitLine[] = [];
  const seen = new Set<string>();
  const lines = raw.split("\n");
  for (let i = 0; i < lines.length; i = i + 1) {
    const line = (lines[i] ?? "").trim();
    if (line === "") continue;
    const parts = line.split(/[\s=,]+/).filter((part) => part !== "");
    const where = `${what}, line ${String(i + 1)}`;
    if (parts.length !== 2) {
      return { ok: false, error: `${where}: write an id and a cap in base units.` };
    }
    const id = parseUuid(parts[0] ?? "", `${where}: the id`);
    if (!id.ok) return id;
    const max = parseBaseUnits(parts[1] ?? "", `${where}: the cap`, true);
    if (!max.ok) return max;
    if (seen.has(id.value)) {
      return { ok: false, error: `${where}: that id is already capped above.` };
    }
    seen.add(id.value);
    out.push({ id: id.value, max: max.value });
  }
  return { ok: true, value: out };
}

/**
 * The customer-facing disclosure of a crossing rule: 20 to 2000 characters
 * that say the activity is simulated. The backend refuses anything else; the
 * form says so before anything is sent.
 */
export function parseCrossingDisclosure(raw: string): Parsed<string> {
  const value = raw.trim();
  if (value.length < 20 || value.length > 2000) {
    return { ok: false, error: "The disclosure is 20 to 2000 characters." };
  }
  if (!/simulated/i.test(value)) {
    return { ok: false, error: "The disclosure must say that the activity is simulated." };
  }
  return { ok: true, value };
}
