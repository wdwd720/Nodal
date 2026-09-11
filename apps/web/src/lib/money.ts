/**
 * Money formatting. Every function in this file operates on decimal *strings*
 * and integer *strings* and never converts a monetary value to a JavaScript
 * number: `0.1 + 0.2 !== 0.3` is why the backend sends money as text, and a
 * single numeric coercion on the display path would quietly undo that.
 *
 * The rules this module obeys, which `source-scan.test.ts` enforces for the
 * whole `src/` tree:
 *
 *   - none of the numeric-coercion built-ins appear anywhere in the app: the
 *     numeric constructor, the two string-to-number parsers, fixed-point
 *     rounding, the maths namespace and locale number formatting are all
 *     refused by the scanner, in comments as well as in code, so the rule has
 *     no grey area to argue about;
 *   - arithmetic operators are only ever applied to string *lengths* and
 *     *indices*, never to a value that came off the wire as money;
 *   - a value that does not match its contract pattern is refused, not coerced,
 *     so a malformed figure can never be rendered as if it were a real balance.
 *
 * The UI formats. The backend computes (PART 110).
 */

/** USD as sent by the API: optional sign, integer part, exactly two fraction digits. */
export const USD_PATTERN = /^-?[0-9]+\.[0-9]{2}$/;

/** An exact asset amount in base units: optional sign, digits only. */
export const QUANTITY_PATTERN = /^-?[0-9]+$/;

/** A free-form decimal string (prices, ratios) with an optional fraction. */
export const DECIMAL_PATTERN = /^-?[0-9]+(\.[0-9]+)?$/;

export class MoneyFormatError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "MoneyFormatError";
  }
}

interface Split {
  negative: boolean;
  integer: string;
  fraction: string;
}

function splitDecimal(value: string, pattern: RegExp, what: string): Split {
  if (typeof value !== "string" || !pattern.test(value)) {
    throw new MoneyFormatError(`${what}: ${JSON.stringify(value)} is not a valid ${what}`);
  }
  const negative = value.startsWith("-");
  const unsigned = negative ? value.slice(1) : value;
  const dot = unsigned.indexOf(".");
  if (dot < 0) {
    return { negative, integer: unsigned, fraction: "" };
  }
  return { negative, integer: unsigned.slice(0, dot), fraction: unsigned.slice(dot + 1) };
}

function allZeroDigits(digits: string): boolean {
  for (const ch of digits) {
    if (ch !== "0") {
      return false;
    }
  }
  return true;
}

/** Strips leading zeros, keeping at least one digit. Pure string surgery. */
function trimLeadingZeros(digits: string): string {
  let i = 0;
  while (i < digits.length - 1 && digits[i] === "0") {
    i = i + 1;
  }
  return digits.slice(i);
}

/** Inserts thousands separators into a run of digits, right to left. */
function group(digits: string, separator: string): string {
  const trimmed = trimLeadingZeros(digits);
  if (trimmed.length <= 3) {
    return trimmed;
  }
  const parts: string[] = [];
  let end = trimmed.length;
  while (end > 3) {
    parts.unshift(trimmed.slice(end - 3, end));
    end = end - 3;
  }
  parts.unshift(trimmed.slice(0, end));
  return parts.join(separator);
}

/**
 * Thousands grouping for a run of digits, exported so `format.ts` groups the
 * same way this module does. Two grouping implementations in one application
 * is two conventions for what a thousand looks like.
 */
export function groupDigits(digits: string): string {
  return group(digits, ",");
}

/** -1, 0 or 1 for a USD string, decided by inspecting digits — never by subtraction. */
export function usdSign(value: string): -1 | 0 | 1 {
  const { negative, integer, fraction } = splitDecimal(value, USD_PATTERN, "USD amount");
  if (allZeroDigits(integer) && allZeroDigits(fraction)) {
    return 0;
  }
  return negative ? -1 : 1;
}

/** -1, 0 or 1 for a base-unit quantity string. */
export function quantitySign(value: string): -1 | 0 | 1 {
  const { negative, integer } = splitDecimal(value, QUANTITY_PATTERN, "quantity");
  if (allZeroDigits(integer)) {
    return 0;
  }
  return negative ? -1 : 1;
}

/** Lexicographic-by-magnitude comparison of two digit runs of any length. */
function compareDigitRuns(a: string, b: string): -1 | 0 | 1 {
  const left = trimLeadingZeros(a);
  const right = trimLeadingZeros(b);
  if (left.length !== right.length) {
    return left.length < right.length ? -1 : 1;
  }
  if (left === right) {
    return 0;
  }
  return left < right ? -1 : 1;
}

/**
 * Orders two USD strings. Used for sorting tables; it decides nothing
 * financial, and it never turns either operand into a number.
 */
export function compareUsd(a: string, b: string): -1 | 0 | 1 {
  const sa = usdSign(a);
  const sb = usdSign(b);
  if (sa !== sb) {
    return sa < sb ? -1 : 1;
  }
  const left = splitDecimal(a, USD_PATTERN, "USD amount");
  const right = splitDecimal(b, USD_PATTERN, "USD amount");
  const magnitude = compareDigitRuns(left.integer + left.fraction, right.integer + right.fraction);
  if (magnitude === 0) {
    return 0;
  }
  // Both operands share a sign here; a larger magnitude is smaller when negative.
  if (sa < 0) {
    return magnitude === -1 ? 1 : -1;
  }
  return magnitude;
}

export interface UsdFormatOptions {
  /** Prefix a positive value with "+". Off by default; only P&L opts in. */
  readonly signed?: boolean;
  /** Render the "$" symbol. On by default. */
  readonly symbol?: boolean;
}

/**
 * Formats a wire USD string for display: "1234.56" -> "$1,234.56".
 *
 * Throws rather than guessing when the input does not match the contract, so a
 * broken response surfaces as an explicit error state instead of a number the
 * user could mistake for their balance.
 */
export function formatUsd(value: string, options: UsdFormatOptions = {}): string {
  const { negative, integer, fraction } = splitDecimal(value, USD_PATTERN, "USD amount");
  const zero = allZeroDigits(integer) && allZeroDigits(fraction);
  const symbol = options.symbol === false ? "" : "$";
  const body = `${symbol}${group(integer, ",")}.${fraction}`;
  if (zero) {
    return body;
  }
  if (negative) {
    return `-${body}`;
  }
  return options.signed === true ? `+${body}` : body;
}

export interface QuantityFormatOptions {
  /** Never show more than this many fraction digits. Defaults to `decimals`. */
  readonly maxFractionDigits?: number;
  /** Always show at least this many fraction digits. Defaults to 0. */
  readonly minFractionDigits?: number;
}

/**
 * Renders exact base units as a human decimal: ("10000000000", 6) -> "10,000".
 *
 * The decimal point is *moved*, never computed: the base-unit string is padded
 * and sliced. Digits that fall outside `maxFractionDigits` are dropped, which
 * is a display truncation and is never fed back into a request.
 */
export function formatQuantity(
  baseUnits: string,
  decimals: number,
  options: QuantityFormatOptions = {},
): string {
  if (!Number.isInteger(decimals) || decimals < 0 || decimals > 36) {
    throw new MoneyFormatError(`quantity: ${String(decimals)} is not a usable decimals count`);
  }
  const { negative, integer } = splitDecimal(baseUnits, QUANTITY_PATTERN, "quantity");
  const padded = integer.padStart(decimals + 1, "0");
  const cut = padded.length - decimals;
  const wholePart = padded.slice(0, cut);
  const fractionPart = padded.slice(cut);

  const maxFraction = options.maxFractionDigits ?? decimals;
  const minFraction = options.minFractionDigits ?? 0;
  let shown = fractionPart.slice(0, maxFraction);
  while (shown.length > minFraction && shown.endsWith("0")) {
    shown = shown.slice(0, shown.length - 1);
  }
  const magnitude = shown.length > 0 ? `${group(wholePart, ",")}.${shown}` : group(wholePart, ",");
  const isZero = allZeroDigits(integer);
  return negative && !isZero ? `-${magnitude}` : magnitude;
}

/** Formats an arbitrary decimal string (an effective price, say) with grouping. */
export function formatDecimalString(value: string): string {
  const { negative, integer, fraction } = splitDecimal(value, DECIMAL_PATTERN, "decimal");
  const body = fraction.length > 0 ? `${group(integer, ",")}.${fraction}` : group(integer, ",");
  const isZero = allZeroDigits(integer) && allZeroDigits(fraction);
  return negative && !isZero ? `-${body}` : body;
}

/**
 * Renders basis points as a percentage: 1234 -> "12.34%".
 *
 * `BPS` is declared as a JSON integer in the contract, so it arrives as a
 * number; the conversion to a percentage is still done by moving a decimal
 * point through the integer's own digits rather than dividing by 100.
 */
export function formatBps(bps: number): string {
  if (!Number.isInteger(bps)) {
    throw new MoneyFormatError(`bps: ${String(bps)} is not an integer`);
  }
  const negative = bps < 0;
  const digits = negative ? String(bps).slice(1) : String(bps);
  const padded = digits.padStart(3, "0");
  const cut = padded.length - 2;
  const whole = group(padded.slice(0, cut), ",");
  let fraction = padded.slice(cut);
  while (fraction.endsWith("0")) {
    fraction = fraction.slice(0, fraction.length - 1);
  }
  const body = fraction === "" ? `${whole}%` : `${whole}.${fraction}%`;
  return negative ? `-${body}` : body;
}

/** The literal basis-point count, for places where the raw policy figure matters. */
export function formatBpsRaw(bps: number): string {
  if (!Number.isInteger(bps)) {
    throw new MoneyFormatError(`bps: ${String(bps)} is not an integer`);
  }
  const digits = bps < 0 ? String(bps).slice(1) : String(bps);
  return `${bps < 0 ? "-" : ""}${group(digits, ",")} bps`;
}

export interface AmountParse {
  readonly ok: boolean;
  /** Canonical wire form, "1234.56", when `ok`. */
  readonly value: string;
  /** A message to show under the field when not `ok`. */
  readonly error: string;
}

/**
 * Normalises what a user typed into the exact two-decimal string the API
 * accepts. Rejects anything it cannot represent exactly rather than rounding
 * silently: the amount the user sees is the amount that is sent.
 */
export function parseUsdAmountInput(raw: string): AmountParse {
  const cleaned = raw.trim().replace(/,/g, "");
  if (cleaned === "") {
    return { ok: false, value: "", error: "Enter an amount." };
  }
  if (!/^[0-9]*(\.[0-9]*)?$/.test(cleaned)) {
    return { ok: false, value: "", error: "Use digits and at most one decimal point." };
  }
  const dot = cleaned.indexOf(".");
  const whole = dot < 0 ? cleaned : cleaned.slice(0, dot);
  const fraction = dot < 0 ? "" : cleaned.slice(dot + 1);
  if (fraction.length > 2) {
    return { ok: false, value: "", error: "US dollars carry at most two decimal places." };
  }
  const integer = trimLeadingZeros(whole === "" ? "0" : whole);
  if (integer.length > 15) {
    return { ok: false, value: "", error: "That amount is larger than the API accepts." };
  }
  const value = `${integer}.${fraction.padEnd(2, "0")}`;
  if (allZeroDigits(integer) && allZeroDigits(fraction.padEnd(2, "0"))) {
    return { ok: false, value: "", error: "Enter an amount greater than zero." };
  }
  return { ok: true, value, error: "" };
}

/**
 * Normalises a typed human amount of an asset into exact base units.
 * ("1.5", 9) -> "1500000000". Digit shifting only; no multiplication.
 */
export function parseQuantityInput(raw: string, decimals: number): AmountParse {
  if (!Number.isInteger(decimals) || decimals < 0 || decimals > 36) {
    return { ok: false, value: "", error: "This asset has no usable precision declared." };
  }
  const cleaned = raw.trim().replace(/,/g, "");
  if (cleaned === "") {
    return { ok: false, value: "", error: "Enter an amount." };
  }
  if (!/^[0-9]*(\.[0-9]*)?$/.test(cleaned)) {
    return { ok: false, value: "", error: "Use digits and at most one decimal point." };
  }
  const dot = cleaned.indexOf(".");
  const whole = dot < 0 ? cleaned : cleaned.slice(0, dot);
  const fraction = dot < 0 ? "" : cleaned.slice(dot + 1);
  if (fraction.length > decimals) {
    return {
      ok: false,
      value: "",
      error: `This asset holds ${String(decimals)} decimal places; that amount is more precise than it can represent.`,
    };
  }
  const digits = trimLeadingZeros(`${whole === "" ? "0" : whole}${fraction.padEnd(decimals, "0")}`);
  if (allZeroDigits(digits)) {
    return { ok: false, value: "", error: "Enter an amount greater than zero." };
  }
  return { ok: true, value: digits, error: "" };
}
