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
 *   2. THE SCALE OF A CREDIT, which the API now states and which this file
 *      reads rather than assumes. `GET /v1/credits/pricing` carries `decimals`
 *      and `GET /v1/credits/balance` carries `credit_decimals`, both added
 *      after the server was found issuing Credits at a scale nothing had ever
 *      compared against the registered asset: the page rendered "100 Credits
 *      per 1 USD" and then "0.001000 Credits" for the result, and the two
 *      halves of that sentence were a factor of a million apart. A hardcoded
 *      six could not have told the difference, which is exactly why it is no
 *      longer the source.
 *

 *   3. EXACT INTEGER ARITHMETIC OVER BASE-UNIT STRINGS, for the two questions
 *      an interface has to ask about a quantity without rendering it: does it
 *      hold anything, and does a set of parts reach its whole. Both are BigInt
 *      comparisons rather than numeric parses -- a balance can exceed what a
 *      JavaScript number represents exactly -- and both live here for the same
 *      reason as everything above: a component is not where money arithmetic
 *      belongs, and `source-scan.test.ts` is why.
 *
 * What is deliberately NOT here: any function that turns an amount of money
 * into a number of Credits. The server owns that (`internal/credit/pricing.go`
 * is the only place that decides it), and a second implementation in the
 * browser would eventually disagree with the one that issues.
 */
import { MoneyFormatError, QUANTITY_PATTERN, USD_PATTERN } from "./money.ts";

/**
 * The scale to render a Credit figure at when the response carrying it does
 * not state one.
 *
 * It is a FALLBACK, not the source. Every response that carries a Credit
 * figure this application renders on its own pages now carries the scale with
 * it, and `creditScale` prefers what the server said. This exists because some
 * responses (a market summary, a product price) still do not, and rendering a
 * base-unit string with no scale at all is not an option — it would show a
 * balance of 300 Credits as 300,000,000.
 *
 * Six is what the CREDIT asset is registered with everywhere in the repository
 * (`scripts/seedeconomy`, `cmd/api`'s sandbox-tier registration,
 * `docs/product/CREDIT_ECONOMY.md`). A deployment that registered another scale
 * would render those remaining figures wrong, which is why the ones that
 * matter — a balance, a purchase — no longer come through here.
 */
export const CREDIT_DECIMALS = 6;

/**
 * The scale the API stated, or the fallback when it stated none.
 *
 * `stated` is a `credit_decimals` or `decimals` field off a validated response.
 * A value that is not a usable scale is treated as absent rather than trusted:
 * a negative or fractional decimals count would make `formatQuantity` throw
 * inside a render, and a page that cannot render its balance is worse than one
 * that renders it at the documented scale.
 */
export function creditScale(stated: number | undefined): number {
  if (stated === undefined) return CREDIT_DECIMALS;
  if (!Number.isInteger(stated) || stated < 0 || stated > 36) return CREDIT_DECIMALS;
  return stated;
}

/**
 * Whether an exact base-unit string holds anything at all.
 *
 * Used to decide whether a bucket that should always be zero -- the Credits a
 * reversal removed -- is worth a field on the page. It is a comparison against
 * zero and never a rendered figure, and it is BigInt rather than a numeric
 * parse because a balance can exceed what a JavaScript number represents
 * exactly. An unreadable string is treated as empty: a page must not decide to
 * show a figure it cannot read.
 */
export function hasCredits(baseUnits: string): boolean {
  try {
    return BigInt(baseUnits) > 0n;
  } catch {
    return false;
  }
}

/**
 * The part of `total` that `parts` does not cover, as an exact base-unit
 * string, or undefined when there is nothing left over.
 *
 * SegmentedBar draws it. A bar whose segments do not reach its own total used
 * to draw short and say nothing, so a bucket the API returned and the caller
 * forgot to pass simply vanished from the picture -- which is what happened to
 * the `reversed` balance: returned by `GET /v1/credits/balance`, rendered by no
 * page, and therefore able to be wrong forever without anybody seeing it
 * (F-156).
 *
 * A NEGATIVE difference -- parts that overshoot their whole -- returns
 * undefined rather than a signed string. That is a caller mixing parts from one
 * response with a whole from another, and drawing it would turn a bug into a
 * picture. An unreadable input returns undefined for the same reason.
 */
export function unaccountedBaseUnits(
  parts: readonly string[],
  total: string | undefined,
): string | undefined {
  if (total === undefined) return undefined;
  try {
    let sum = 0n;
    for (const part of parts) sum += BigInt(part);
    const rest = BigInt(total) - sum;
    return rest > 0n ? String(rest) : undefined;
  } catch {
    return undefined;
  }
}

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

/**
 * The same conversion for a value that arrived as an integer STRING.
 *
 * The activity feed carries the money side of a Credit purchase as
 * `unit: "MONEY_MINOR"` with an exact integer string, which may be negative on
 * a reversal. Converting it through the number form would put a wire value
 * through a numeric parse for no reason, so this is digit shifting on the
 * string itself and is exact at any size.
 */
export function minorStringToUsd(value: string): string {
  if (!QUANTITY_PATTERN.test(value)) {
    throw new MoneyFormatError(`minor units: ${value} is not an exact integer`);
  }
  const negative = value.startsWith("-");
  const digits = (negative ? value.slice(1) : value).padStart(3, "0");
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
