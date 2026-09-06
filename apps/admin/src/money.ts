/**
 * Money and quantities, as strings, forever.
 *
 * The API sends USD as a decimal string with exactly two fraction digits and
 * asset quantities as an exact integer count of base units. Both routinely
 * exceed what an IEEE-754 double represents exactly: 9,007,199,254,740,993
 * base units of a 9-decimal token is an ordinary balance and an inexact
 * `number`. So nothing in this console parses a monetary value into a number,
 * and nothing here does arithmetic on one — the backend is the only place
 * financial arithmetic happens (PART 166, CONVENTIONS §1).
 *
 * These functions therefore only ever *format* or *validate*. There is
 * deliberately no add, no subtract and no convert.
 */

/**
 * The digit-group separator. U+2009 THIN SPACE is unambiguous across locales,
 * unlike the comma and the period, which swap roles between them. It is
 * written as an escape so it cannot be mistaken for an ordinary space in the
 * source, and callers that need the raw digits keep the unformatted string.
 */
export const THIN_SPACE = String.fromCharCode(0x2009);

const USD_RE = /^-?[0-9]+\.[0-9]{2}$/;
const QUANTITY_RE = /^-?[0-9]+$/;

/** True when s is the exact USD wire form: "1234.56", "-0.01". */
export function isUSD(s: string): boolean {
  return USD_RE.test(s);
}

/** True when s is the exact base-unit wire form: "1500000000". */
export function isQuantity(s: string): boolean {
  return QUANTITY_RE.test(s);
}

/**
 * Groups the integer part of a decimal string with thin spaces for reading.
 * It is pure string surgery: the digits that go in are the digits that come
 * out, in the same order.
 */
export function groupDigits(value: string): string {
  const negative = value.startsWith("-");
  const body = negative ? value.slice(1) : value;
  const dot = body.indexOf(".");
  const whole = dot === -1 ? body : body.slice(0, dot);
  const fraction = dot === -1 ? "" : body.slice(dot);
  let grouped = "";
  for (let i = 0; i < whole.length; i++) {
    const fromEnd = whole.length - i;
    grouped += whole[i];
    if (fromEnd > 1 && fromEnd % 3 === 1) grouped += THIN_SPACE;
  }
  return `${negative ? "-" : ""}${grouped}${fraction}`;
}

/**
 * Renders a USD wire string for display. An input that is not the exact wire
 * form is returned verbatim and flagged, because silently reformatting
 * something the backend did not promise is how a wrong number acquires the
 * appearance of a right one.
 */
export function formatUSD(value: string | null | undefined): { text: string; exact: boolean } {
  if (value === null || value === undefined || value === "") return { text: "—", exact: true };
  if (!isUSD(value)) return { text: value, exact: false };
  return { text: `$${groupDigits(value)}`, exact: true };
}

/**
 * Renders an exact base-unit quantity as a decimal string with `decimals`
 * fraction digits, by moving the decimal point. No division, no rounding, no
 * loss: the digits are rearranged, not recomputed.
 */
export function formatQuantity(value: string, decimals: number): { text: string; exact: boolean } {
  if (!isQuantity(value) || !Number.isInteger(decimals) || decimals < 0 || decimals > 38) {
    return { text: value, exact: false };
  }
  const negative = value.startsWith("-");
  let digits = negative ? value.slice(1) : value;
  if (decimals === 0) return { text: `${negative ? "-" : ""}${groupDigits(digits)}`, exact: true };
  digits = digits.padStart(decimals + 1, "0");
  const whole = digits.slice(0, digits.length - decimals);
  const fraction = digits.slice(digits.length - decimals);
  return { text: `${negative ? "-" : ""}${groupDigits(whole)}.${fraction}`, exact: true };
}

/** True when the string denotes zero, without parsing it as a number. */
export function isZeroDecimal(value: string): boolean {
  if (!isUSD(value) && !isQuantity(value)) return false;
  return /^-?0+(\.0+)?$/.test(value);
}

/**
 * Renders one field of a reconciliation evidence document.
 *
 * The evidence objects are free-form JSON. The domain writes exact quantities
 * as strings, but `JSON.parse` has already turned any JSON *number* in there
 * into a double before this console sees it, so such a value may already have
 * lost digits. Rather than hide that, a numeric field is rendered from its own
 * string form and reported as inexact, so an operator reading evidence knows
 * which figures are exact and which are not.
 */
export function renderEvidenceValue(value: unknown): { text: string; exact: boolean } {
  if (value === null) return { text: "null", exact: true };
  switch (typeof value) {
    case "string":
      return { text: value, exact: true };
    case "boolean":
      return { text: value ? "true" : "false", exact: true };
    case "number":
      // A JSON number reached the console. Show it, and say it is not exact.
      return { text: String(value), exact: false };
    case "object":
      return { text: JSON.stringify(value), exact: !containsNumber(value) };
    default:
      return { text: String(value), exact: false };
  }
}

function containsNumber(value: unknown): boolean {
  if (typeof value === "number") return true;
  if (Array.isArray(value)) return value.some(containsNumber);
  if (typeof value === "object" && value !== null) {
    return Object.values(value as Record<string, unknown>).some(containsNumber);
  }
  return false;
}
