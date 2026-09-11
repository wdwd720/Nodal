/**
 * The arithmetic the Buy Credits page needs, and nothing more.
 *
 * Two things live here because neither belongs in `money.ts` — which owns the
 * decimal-string ladder — and neither may be written inside a page, where the
 * source guards forbid a decimal literal and a money-shaped string.
 *
 *   1. MINOR UNITS. `POST /v1/payments` takes `amount_minor` as an integer and
 *      `GET /v1/credits/pricing` states its bounds the same way, while every
 *      amount a person types or reads is a decimal string. Something has to
 *      cross that line, and it has to cross it without a float: every numeric
 *      parse and coercion is banned by `source-scan.test.ts`, so the conversion
 *      here is digit arithmetic on the characters themselves. Every value in
 *      range (the policy caps a purchase at a million minor units) is exactly
 *      representable as an integer, so nothing is lost and nothing is rounded.
 *      The numeric-coercion guard is what makes this the only honest route.
 *
 *   2. THE SCALE OF A CREDIT, which the API does not state anywhere. The
 *      `CreditBalance`, `NativeMarket` and `InternalProduct` responses all
 *      carry Credit figures as exact base units and none of them carries the
 *      asset's decimals. The seeded CREDIT asset declares six, and the pages
 *      that shipped before this one hardcoded six, so this is six — named once
 *      here rather than a fourth time in a page, and flagged as a gap the
 *      backend should close by returning the scale with the figure.
 *
 * What is deliberately NOT here: any function that turns an amount of money
 * into a number of Credits. The server owns that (`internal/credit/pricing.go`
 * is the only place that decides it), and a second implementation in the
 * browser would eventually disagree with the one that issues.
 */
import { MoneyFormatError, USD_PATTERN } from "./money.ts";

/**
 * How many decimal places a Credit has.
 *
 * See the note above: the API does not say, and this is the value the rest of
 * the application already assumes. Every Credit figure in the product is
 * rendered at this scale, so a balance reads the same on every page.
 */
export const CREDIT_DECIMALS = 6;

/** The digits, used as a lookup so a character can become a value without a numeric parse. */
const DIGITS = "0123456789";

/**
 * The presets the goal document names, in minor units of the pricing currency:
 * $10, $25, $50 and $100.
 *
 * They are minor units rather than strings because a page may not contain a
 * money-shaped literal — `source-scan.test.ts` refuses `"10.00"` in interface
 * code, and rightly, since a hardcoded figure there is indistinguishable from
 * a fabricated balance. A preset outside the policy's bounds is filtered out by
 * the page rather than shown and then refused.
 */
export const PRESET_AMOUNTS_MINOR: readonly number[] = [1000, 2500, 5000, 10000];

/**
 * Turns a canonical USD string ("25.00") into exact minor units (2500).
 *
 * Digit arithmetic, not a parse: each character is looked up in `DIGITS` and
 * accumulated, which is integer-exact and touches no float. Throws rather than
 * guessing on anything that is not the two-decimal form the API contract uses,
 * because a silently-repaired amount is an amount the customer did not agree
 * to.
 */
export function usdToMinor(value: string): number {
  if (!USD_PATTERN.test(value)) {
    throw new MoneyFormatError(`usd: ${value} is not a two-decimal amount`);
  }
  const negative = value.startsWith("-");
  const digits = (negative ? value.slice(1) : value).replace(".", "");
  let total = 0;
  for (const character of digits) {
    const digit = DIGITS.indexOf(character);
    total = total * 10 + digit;
    if (!Number.isSafeInteger(total)) {
      throw new MoneyFormatError(`usd: ${value} is larger than this can represent exactly`);
    }
  }
  return negative ? -total : total;
}

/**
 * Turns exact minor units (2500) back into the canonical USD string ("25.00").
 *
 * String surgery on the decimal representation of an integer: no division, no
 * rounding, nothing that could move a cent.
 */
export function minorToUsd(minor: number): string {
  if (!Number.isSafeInteger(minor)) {
    throw new MoneyFormatError(`usd: ${String(minor)} is not an exact integer of minor units`);
  }
  const negative = minor < 0;
  const digits = String(negative ? -minor : minor).padStart(3, "0");
  const cut = digits.length - 2;
  return `${negative ? "-" : ""}${digits.slice(0, cut)}.${digits.slice(cut)}`;
}

/** The bounds one purchase must fall inside, as the pricing policy states them. */
export interface AmountBounds {
  readonly minMinor: number;
  readonly maxMinor: number;
}

/**
 * Checks a typed amount against the policy's bounds.
 *
 * Returns the sentence to show under the field, or the empty string when the
 * amount is acceptable. The server checks this again and is the authority; this
 * exists so a customer is not sent to a payment provider to be refused there.
 */
export function outOfBounds(minor: number, bounds: AmountBounds): string {
  if (minor < bounds.minMinor) {
    return `The smallest purchase this deployment sells is ${minorToUsd(bounds.minMinor)}.`;
  }
  if (minor > bounds.maxMinor) {
    return `The largest single purchase this deployment sells is ${minorToUsd(bounds.maxMinor)}.`;
  }
  return "";
}
