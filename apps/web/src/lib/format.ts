/**
 * NUMBER FORMATTING — the display spec, implemented on strings.
 *
 * `design/reference/format.ts` is the reference implementation of this
 * specification and it could not be adopted as written: every rung of its
 * ladder converts the value to a double first, using the numeric constructor,
 * fixed-point rounding, significant-figure rounding, the maths namespace or
 * locale number formatting. This repository's source guard refuses all five on
 * sight, and every one of them destroys the exactness the ledger holds. That
 * two tenths and one tenth do not add up to three tenths in binary floating
 * point is the whole reason the backend sends money as text.
 *
 * So the RULES are adopted and the ARITHMETIC is rewritten. Everything below
 * operates on decimal strings and integer strings by padding, slicing and
 * comparing digits. Nothing here divides, rounds or converts.
 *
 * The rules, and where they come from (WEBSITE_MASTER_GOAL PART 8):
 *
 *   - absent renders as an em dash, never as a zero and never as "N/A";
 *   - exactly zero renders as zero, because zero is a real answer;
 *   - below 1e-8 renders as a bounded statement rather than a figure;
 *   - four or more leading zeros render in subscript-zero notation, where the
 *     visible `0` stands for the WHOLE zero run: `0.0₄52` expands to 0.000052,
 *     not 0.0000052, which is the off-by-one every implementation gets wrong;
 *   - between 1e-4 and 1, four significant digits;
 *   - a stablecoin between 0.95 and 1.05 gets three decimals, so a depegged
 *     token reads 0.999 rather than rounding to 1.00 and saying nothing;
 *   - at or above 1, two decimals;
 *   - compact notation truncates and never rounds up across a threshold, so
 *     999,500 can never render as 1M;
 *   - counts are never abbreviated: a rounded count is a false statement about
 *     a discrete thing;
 *   - percentages carry two decimals below 100 and none above, are always
 *     signed with U+2212 MINUS rather than a hyphen, and zero is neutral
 *     because zero is not a direction;
 *   - timestamps are absolute UTC with the full instant available;
 *   - identifiers are first five, ellipsis, last four, with the whole value
 *     always reachable.
 *
 * EVERY ABBREVIATION IS LOSSY AND SAYS SO. A `Figure` whose displayed form is
 * not the exact value carries `abbreviated: true`, and the component renders
 * the exact value into `title` and into a visually-hidden span so a screen
 * reader hears the number rather than the notation.
 *
 * TRUNCATION, NOT ROUNDING. Where the display cannot show every digit, the
 * remaining digits are dropped rather than rounded. Rounding would let a
 * figure read higher than the value it stands for, and the one thing this
 * interface may never do is make a customer believe they have more than they
 * have.
 */

import { DECIMAL_PATTERN, MoneyFormatError, QUANTITY_PATTERN, groupDigits } from "./money.ts";

/** What an absent value renders as. Never a zero, never "N/A". */
export const ABSENT = "—";

/** U+2212 MINUS SIGN. A hyphen is not a minus sign. */
export const MINUS = "−";

/** The zero-run threshold, agreed by four independent implementations. */
export const SUBSCRIPT_THRESHOLD = 4;

/** Significant digits shown for a value below one. */
const SIGNIFICANT = 4;

export type Direction = "up" | "down" | "flat";

/**
 * A figure, decomposed for rendering. The component assembles it; nothing else
 * is allowed to build a figure from parts of its own.
 */
export interface Figure {
  /** "", "+" or U+2212. Already excludes zero, which takes no sign. */
  readonly sign: string;
  /** Everything before the small zero-run digit. */
  readonly head: string;
  /** The zero run the small digit stands for, or undefined. */
  readonly zeroRun: number | undefined;
  /** Digits after the zero run. Empty unless `zeroRun` is set. */
  readonly tail: string;
  /** "%", "K", "M", "B", a unit symbol, or "". */
  readonly suffix: string;
  /** The unabbreviated value, for `title` and for assistive technology. */
  readonly exact: string;
  /** True when the displayed form is not the whole value. */
  readonly abbreviated: boolean;
  readonly direction: Direction;
  /** True when there was no value to show. */
  readonly absent: boolean;
}

const NOTHING: Figure = {
  sign: "",
  head: ABSENT,
  zeroRun: undefined,
  tail: "",
  suffix: "",
  exact: "no value was returned",
  abbreviated: false,
  direction: "flat",
  absent: true,
};

/** The em-dash figure, for a field the backend did not send. */
export function absentFigure(what?: string): Figure {
  return what === undefined ? NOTHING : { ...NOTHING, exact: what };
}

/* -------------------------------------------------------------------------- *
 * Digit surgery. Everything else in this file is built out of these six.
 * -------------------------------------------------------------------------- */

interface Parts {
  readonly negative: boolean;
  /** Integer digits, leading zeros stripped, at least one digit. */
  readonly int: string;
  /** Fraction digits exactly as sent, trailing zeros preserved. */
  readonly frac: string;
}

function stripLeadingZeros(digits: string): string {
  let i = 0;
  while (i < digits.length - 1 && digits[i] === "0") {
    i = i + 1;
  }
  return digits.slice(i);
}

function allZeros(digits: string): boolean {
  for (const ch of digits) {
    if (ch !== "0") return false;
  }
  return true;
}

function split(value: string): Parts {
  if (typeof value !== "string" || !DECIMAL_PATTERN.test(value)) {
    throw new MoneyFormatError(`decimal: ${JSON.stringify(value)} is not a decimal string`);
  }
  const negative = value.startsWith("-");
  const unsigned = negative ? value.slice(1) : value;
  const dot = unsigned.indexOf(".");
  if (dot < 0) {
    return { negative, int: stripLeadingZeros(unsigned), frac: "" };
  }
  return {
    negative,
    int: stripLeadingZeros(unsigned.slice(0, dot)),
    frac: unsigned.slice(dot + 1),
  };
}

function isZero(p: Parts): boolean {
  return allZeros(p.int) && allZeros(p.frac);
}

/** Zeros between the point and the first significant digit. */
function zeroRunOf(frac: string): number {
  let n = 0;
  while (n < frac.length && frac[n] === "0") {
    n = n + 1;
  }
  return n;
}

/** Compares two fraction strings by value, padding the shorter with zeros. */
function compareFractions(a: string, b: string): -1 | 0 | 1 {
  const width = a.length > b.length ? a.length : b.length;
  const left = a.padEnd(width, "0");
  const right = b.padEnd(width, "0");
  if (left === right) return 0;
  return left < right ? -1 : 1;
}

/**
 * Moves the decimal point left by `scale` places through an integer string of
 * base units. The point is MOVED, never computed: the digits are padded and
 * sliced, so ("10000000000", 6) becomes "10000.000000" exactly.
 */
export function fromBaseUnits(baseUnits: string, scale: number): string {
  if (!Number.isInteger(scale) || scale < 0 || scale > 36) {
    throw new MoneyFormatError(`scale: ${String(scale)} is not a usable decimals count`);
  }
  if (!QUANTITY_PATTERN.test(baseUnits)) {
    throw new MoneyFormatError(`base units: ${JSON.stringify(baseUnits)} is not an integer string`);
  }
  const negative = baseUnits.startsWith("-");
  const digits = negative ? baseUnits.slice(1) : baseUnits;
  if (scale === 0) {
    return `${negative ? "-" : ""}${digits}`;
  }
  const padded = digits.padStart(scale + 1, "0");
  const cut = padded.length - scale;
  return `${negative ? "-" : ""}${padded.slice(0, cut)}.${padded.slice(cut)}`;
}

/** The exact value, grouped, for `title` and for assistive technology. */
function exactText(p: Parts, suffix: string): string {
  const body = p.frac === "" ? groupDigits(p.int) : `${groupDigits(p.int)}.${p.frac}`;
  return `${p.negative && !isZero(p) ? MINUS : ""}${body}${suffix}`;
}

/* -------------------------------------------------------------------------- *
 * Compact notation.
 *
 * The correctness bug this exists to avoid is `Intl`'s default: two significant
 * digits, rounded half-expand, which renders 999,500 as "1M" — a $999,999
 * position and a $1,000,000 one become the same pixels.
 *
 * Two fixes are required and both are here: truncate, never round; and cut over
 * on an explicit threshold. The reference implementation's thresholds
 * (995 / 999,995 / 999,999,995) are calibrated for half-expand rounding and
 * understate badly under truncation — 999,995 would render as "0.99M" — so the
 * cutover here is the unit itself. Truncation alone already makes "1000K"
 * unreachable: 999,999 truncates to 999.9K.
 * -------------------------------------------------------------------------- */

const UNITS: ReadonlyArray<readonly [number, string]> = [
  [9, "B"],
  [6, "M"],
  [3, "K"],
];

interface Compact {
  readonly head: string;
  readonly suffix: string;
}

function compactOf(p: Parts): Compact | undefined {
  for (const [zeros, suffix] of UNITS) {
    if (p.int.length > zeros) {
      const cut = p.int.length - zeros;
      const whole = p.int.slice(0, cut);
      // One decimal once the whole part reaches two digits, two below that:
      // "1.23M" carries as much information as "12.3M" in the same width.
      const places = whole.length > 1 ? 1 : 2;
      const rest = `${p.int.slice(cut)}${p.frac}`.slice(0, places);
      const trimmed = rest.replace(/0+$/, "");
      return {
        head: trimmed === "" ? groupDigits(whole) : `${groupDigits(whole)}.${trimmed}`,
        suffix,
      };
    }
  }
  return undefined;
}

/* -------------------------------------------------------------------------- *
 * The money ladder.
 * -------------------------------------------------------------------------- */

export interface MoneyOptions {
  /** Prefix a positive value with "+". P&L opts in; a balance does not. */
  readonly signed?: boolean;
  /** Allow K/M/B abbreviation above a thousand. */
  readonly compact?: boolean;
  /** Apply the 0.95–1.05 three-decimal band. Stablecoins only. */
  readonly stablecoin?: boolean;
  /** Rendered after the figure, e.g. "Credits". */
  readonly symbol?: string;
}

/** True when 0.95 <= |value| <= 1.05, decided on digits alone. */
function inStableBand(p: Parts): boolean {
  if (p.int === "1") {
    return compareFractions(p.frac, "05") <= 0;
  }
  if (p.int === "0") {
    return compareFractions(p.frac, "95") >= 0;
  }
  return false;
}

function signOf(p: Parts, signed: boolean): string {
  if (isZero(p)) return "";
  if (p.negative) return MINUS;
  return signed ? "+" : "";
}

function directionOf(p: Parts): Direction {
  if (isZero(p)) return "flat";
  return p.negative ? "down" : "up";
}

/**
 * Formats an exact decimal string as a monetary figure.
 *
 * Throws rather than guessing when the input does not match the contract, so a
 * broken response surfaces as a stated fault instead of a number a customer
 * could mistake for their balance.
 */
export function formatMoney(value: string, options: MoneyOptions = {}): Figure {
  const p = split(value);
  const symbol = options.symbol === undefined ? "" : ` ${options.symbol}`;
  const exact = exactText(p, symbol);
  const sign = signOf(p, options.signed === true);
  const direction = directionOf(p);
  const base = {
    sign,
    zeroRun: undefined,
    tail: "",
    suffix: symbol,
    exact,
    direction,
    absent: false,
  } as const;

  if (isZero(p)) {
    return { ...base, sign: "", head: "0.00", abbreviated: false, direction: "flat" };
  }

  if (p.int === "0") {
    const run = zeroRunOf(p.frac);

    // Below 1e-8 there is no honest way to draw the digits at this size, so the
    // figure states a bound instead of pretending to be one.
    if (run >= 8) {
      return { ...base, head: "<0.00000001", abbreviated: true };
    }

    if (run >= SUBSCRIPT_THRESHOLD) {
      const significant = p.frac.slice(run, run + SIGNIFICANT);
      return { ...base, head: "0.0", zeroRun: run, tail: significant, abbreviated: true };
    }

    if (options.stablecoin === true && inStableBand(p)) {
      const shown = p.frac.padEnd(3, "0").slice(0, 3);
      return { ...base, head: `0.${shown}`, abbreviated: p.frac.length > 3 };
    }

    const significant = p.frac.slice(0, run + SIGNIFICANT).replace(/0+$/, "");
    const shown = significant === "" ? p.frac.slice(0, run + SIGNIFICANT) : significant;
    return { ...base, head: `0.${shown}`, abbreviated: shown.length < p.frac.length };
  }

  if (options.stablecoin === true && inStableBand(p)) {
    const shown = p.frac.padEnd(3, "0").slice(0, 3);
    return { ...base, head: `${p.int}.${shown}`, abbreviated: p.frac.length > 3 };
  }

  if (options.compact === true) {
    const compact = compactOf(p);
    if (compact !== undefined) {
      return {
        ...base,
        head: compact.head,
        suffix: `${compact.suffix}${symbol}`,
        abbreviated: true,
      };
    }
  }

  const cents = p.frac.padEnd(2, "0").slice(0, 2);
  return {
    ...base,
    head: `${groupDigits(p.int)}.${cents}`,
    abbreviated: p.frac.length > 2,
  };
}

/**
 * An asset quantity. The scale comes from the instrument, never from the
 * value: an indivisible unit shows no decimals, because showing them would
 * imply half of one can be held.
 */
export function formatUnits(
  baseUnits: string,
  scale: number,
  options: MoneyOptions = {},
): Figure {
  const decimal = fromBaseUnits(baseUnits, scale);
  const p = split(decimal);
  const symbol = options.symbol === undefined ? "" : ` ${options.symbol}`;
  const exact = exactText(p, symbol);
  const sign = signOf(p, options.signed === true);

  // The band is a property of the VALUE, not of the wire form it arrived in. A
  // stablecoin sent as `{decimal}` and the same stablecoin sent as base units
  // are the same holding, and a reader who can see a depeg in one and not the
  // other is being told two different things by one design rule.
  if (options.stablecoin === true && inStableBand(p)) {
    const shown = p.frac.padEnd(3, "0").slice(0, 3);
    return {
      sign,
      head: `${groupDigits(p.int)}.${shown}`,
      zeroRun: undefined,
      tail: "",
      suffix: symbol,
      exact,
      abbreviated: p.frac.length > 3,
      direction: directionOf(p),
      absent: false,
    };
  }

  if (options.compact === true) {
    const compact = compactOf(p);
    if (compact !== undefined) {
      return {
        sign,
        head: compact.head,
        zeroRun: undefined,
        tail: "",
        suffix: `${compact.suffix}${symbol}`,
        exact,
        abbreviated: true,
        direction: directionOf(p),
        absent: false,
      };
    }
  }

  const shown = p.frac === "" ? groupDigits(p.int) : `${groupDigits(p.int)}.${p.frac}`;
  return {
    sign,
    head: shown,
    zeroRun: undefined,
    tail: "",
    suffix: symbol,
    exact,
    abbreviated: false,
    direction: directionOf(p),
    absent: false,
  };
}

/**
 * A percentage. Two decimals below 100, none above; always signed except zero,
 * which is neutral because zero is not a direction.
 */
export function formatPercent(value: string, options: { readonly signed?: boolean } = {}): Figure {
  const p = split(value);
  const exact = exactText(p, "%");
  const signed = options.signed !== false;

  if (isZero(p)) {
    return {
      sign: "",
      head: "0",
      zeroRun: undefined,
      tail: "",
      suffix: "%",
      exact,
      abbreviated: false,
      direction: "flat",
      absent: false,
    };
  }

  const large = p.int.length > 3 || (p.int.length === 3 && p.int >= "100");
  const head = large
    ? groupDigits(p.int)
    : `${groupDigits(p.int)}.${p.frac.padEnd(2, "0").slice(0, 2)}`;
  return {
    sign: signOf(p, signed),
    head,
    zeroRun: undefined,
    tail: "",
    suffix: "%",
    exact,
    abbreviated: large ? p.frac !== "" : p.frac.length > 2,
    direction: directionOf(p),
    absent: false,
  };
}

/** Basis points as a percentage, by moving the point through the integer. */
export function percentFromBps(bps: number, options: { readonly signed?: boolean } = {}): Figure {
  if (!Number.isInteger(bps)) {
    throw new MoneyFormatError(`bps: ${String(bps)} is not an integer`);
  }
  const text = String(bps);
  const negative = text.startsWith("-");
  const digits = (negative ? text.slice(1) : text).padStart(3, "0");
  const cut = digits.length - 2;
  return formatPercent(`${negative ? "-" : ""}${digits.slice(0, cut)}.${digits.slice(cut)}`, options);
}

/** The literal basis-point count, for the places where the policy figure matters. */
export function formatBpsFigure(bps: number): Figure {
  if (!Number.isInteger(bps)) {
    throw new MoneyFormatError(`bps: ${String(bps)} is not an integer`);
  }
  const text = String(bps);
  const negative = text.startsWith("-");
  const digits = negative ? text.slice(1) : text;
  const grouped = groupDigits(digits);
  const zero = allZeros(digits);
  return {
    sign: zero ? "" : negative ? MINUS : "",
    head: grouped,
    zeroRun: undefined,
    tail: "",
    suffix: " bps",
    exact: `${negative && !zero ? MINUS : ""}${grouped} basis points`,
    abbreviated: false,
    direction: zero ? "flat" : negative ? "down" : "up",
    absent: false,
  };
}

/**
 * A count. Never abbreviated: a count is discrete, and a rounded count is a
 * false statement about a discrete thing. "1.1K holders" cannot be checked
 * against anything; "1,104 holders" can.
 */
export function formatCount(
  value: string | number,
  options: { readonly symbol?: string } = {},
): Figure {
  const text = typeof value === "number" ? String(value) : value;
  if (!QUANTITY_PATTERN.test(text)) {
    throw new MoneyFormatError(`count: ${JSON.stringify(text)} is not an integer string`);
  }
  const negative = text.startsWith("-");
  const digits = stripLeadingZeros(negative ? text.slice(1) : text);
  const grouped = groupDigits(digits);
  const zero = allZeros(digits);
  // A count without its unit is a number the reader has to guess at: the
  // Credits-per-dollar rate rendered as a bare "100" says nothing about what a
  // hundred of. Every other kind honours `symbol`; so does this one.
  const symbol = options.symbol === undefined ? "" : ` ${options.symbol}`;
  return {
    sign: zero ? "" : negative ? MINUS : "",
    head: grouped,
    zeroRun: undefined,
    tail: "",
    suffix: symbol,
    exact: `${negative && !zero ? MINUS : ""}${grouped}${symbol}`,
    abbreviated: false,
    direction: "flat",
    absent: false,
  };
}

/** The whole displayed string, for a title attribute or a test. */
export function figureText(figure: Figure): string {
  if (figure.zeroRun === undefined) {
    return `${figure.sign}${figure.head}${figure.tail}${figure.suffix}`;
  }
  return `${figure.sign}${figure.head}${String(figure.zeroRun)}${figure.tail}${figure.suffix}`;
}

/* -------------------------------------------------------------------------- *
 * Time.
 * -------------------------------------------------------------------------- */

/** The staleness tiers a snapshot escalates through. */
export type Staleness = "fresh" | "aging" | "stale" | "unknown";

const AGING_MS = 5_000;
const STALE_MS = 30_000;

function instant(value: string | undefined): Date | undefined {
  if (value === undefined || value === "") return undefined;
  const parsed = new Date(value);
  return isNaN(parsed.getTime()) ? undefined : parsed;
}

/** Absolute UTC clock time, in mono: "06:22:39 UTC". Never local without a label. */
export function utcClock(value: string | undefined): string {
  const parsed = instant(value);
  if (parsed === undefined) return "not recorded";
  return `${parsed.toISOString().slice(11, 19)} UTC`;
}

/** The full UTC instant, for the title attribute and for reconciliation. */
export function utcStamp(value: string | undefined): string {
  const parsed = instant(value);
  return parsed === undefined ? "not recorded" : parsed.toISOString();
}

/** Whole milliseconds since an instant, or undefined when it is unreadable. */
export function ageMillis(value: string | undefined, now: Date = new Date()): number | undefined {
  const parsed = instant(value);
  if (parsed === undefined) return undefined;
  const elapsed = now.getTime() - parsed.getTime();
  return elapsed < 0 ? 0 : elapsed;
}

/**
 * Which tier a snapshot has reached. Under five seconds it is current; to
 * thirty it is aging and says so in amber; past thirty it is stale, the stamp
 * turns red and the figure beside it goes faint. A number nobody is updating
 * must stop looking authoritative — but it is never blanked, because an absent
 * figure is a different claim from an old one.
 */
export function staleness(value: string | undefined, now: Date = new Date()): Staleness {
  const age = ageMillis(value, now);
  if (age === undefined) return "unknown";
  if (age < AGING_MS) return "fresh";
  if (age < STALE_MS) return "aging";
  return "stale";
}

/** Age in the coarsest unit that still says something: "42s", "7m", "3h", "2d". */
export function formatAge(value: string | undefined, now: Date = new Date()): string {
  const age = ageMillis(value, now);
  if (age === undefined) return "not recorded";
  const seconds = (age - (age % 1000)) / 1000;
  if (seconds < 60) return `${String(seconds)}s`;
  const minutes = (seconds - (seconds % 60)) / 60;
  if (minutes < 60) return `${String(minutes)}m`;
  const hours = (minutes - (minutes % 60)) / 60;
  if (hours < 24) return `${String(hours)}h`;
  const days = (hours - (hours % 24)) / 24;
  return `${String(days)}d`;
}

/* -------------------------------------------------------------------------- *
 * Identifiers.
 * -------------------------------------------------------------------------- */

/**
 * First five, ellipsis, last four. The whole value stays in `title` and in the
 * accessible name, and it is NEVER truncated inside a confirmation step: at
 * the moment of an irreversible action the customer sees all of it.
 */
export function truncateIdentifier(id: string): string {
  return id.length <= 12 ? id : `${id.slice(0, 5)}…${id.slice(id.length - 4)}`;
}

/* -------------------------------------------------------------------------- *
 * Proportions.
 *
 * The one place this file derives a number rather than formatting one, and the
 * derivation is GEOMETRY, not finance: how wide a segment of a bar is drawn and
 * how that width is described to a screen reader. The exact figures are always
 * in the legend beside it, unaltered.
 *
 * BigInt, not a double, and truncated rather than rounded, so a segment can
 * never claim a larger share than it holds.
 * -------------------------------------------------------------------------- */

/** A truncated whole-percent share, as a string. Empty when there is no total. */
export function percentOfTotal(part: string, total: string): string | undefined {
  if (!QUANTITY_PATTERN.test(part) || !QUANTITY_PATTERN.test(total)) return undefined;
  const whole = BigInt(total);
  if (whole <= 0n) return undefined;
  const share = BigInt(part);
  if (share < 0n) return undefined;
  return String((share * 100n) / whole);
}
