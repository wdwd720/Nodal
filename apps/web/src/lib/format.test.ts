/**
 * The display spec, checked rung by rung.
 *
 * Every assertion below corresponds to a line of WEBSITE_MASTER_GOAL PART 8,
 * and several of them exist because the reference implementation gets them
 * wrong in a way that is invisible until it is somebody's balance: rounding up
 * across an order of magnitude, abbreviating a count, reading a subscript run
 * one digit short.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { MoneyFormatError } from "./money.ts";
import {
  ABSENT,
  MINUS,
  absentFigure,
  ageMillis,
  figureText,
  formatAge,
  formatBpsFigure,
  formatCount,
  formatMoney,
  formatPercent,
  formatUnits,
  fromBaseUnits,
  percentFromBps,
  percentOfTotal,
  staleness,
  truncateIdentifier,
  utcClock,
  utcStamp,
} from "./format.ts";

test("an absent value is an em dash, never a zero", () => {
  const figure = absentFigure();
  assert.equal(figure.head, ABSENT);
  assert.equal(figure.absent, true);
  assert.equal(figure.sign, "");
  assert.notEqual(figureText(figure), "0.00");
});

test("exactly zero renders as zero, unsigned and without a direction", () => {
  const figure = formatMoney("0.00");
  assert.equal(figureText(figure), "0.00");
  assert.equal(figure.sign, "");
  assert.equal(figure.direction, "flat");
  // A negative zero is still zero and still takes no sign.
  assert.equal(figureText(formatMoney("-0.00")), "0.00");
  assert.equal(figureText(formatMoney("0.00", { signed: true })), "0.00");
});

test("a value below 1e-8 states a bound instead of drawing digits", () => {
  const figure = formatMoney("0.000000009");
  assert.equal(figure.head, "<0.00000001");
  assert.equal(figure.abbreviated, true);
  assert.equal(figure.exact, "0.000000009");
});

test("four leading zeros switch to subscript notation, and the run means the WHOLE run", () => {
  // 0.000052 has four leading zeros. The small digit stands for all four, so
  // it expands to 0.000052 and NOT to 0.0000052 — the off-by-one that every
  // implementation of this notation gets wrong at least once.
  const figure = formatMoney("0.000052");
  assert.equal(figure.head, "0.0");
  assert.equal(figure.zeroRun, 4);
  assert.equal(figure.tail, "52");
  assert.equal(figure.abbreviated, true);
  assert.equal(figure.exact, "0.000052");

  // Three leading zeros is below the threshold and stays literal.
  assert.equal(formatMoney("0.000523").zeroRun, undefined);
  assert.equal(formatMoney("0.000523").head, "0.000523");
});

test("between 1e-4 and 1 the figure carries four significant digits and truncates", () => {
  // Truncation, never rounding: a rounded figure can read higher than the
  // value it stands for.
  assert.equal(formatMoney("0.123456").head, "0.1234");
  assert.equal(formatMoney("0.123456").abbreviated, true);
  assert.equal(formatMoney("0.00129999").head, "0.001299");
  assert.equal(formatMoney("0.5").head, "0.5");
  assert.equal(formatMoney("0.5").abbreviated, false);
});

test("a stablecoin between 0.95 and 1.05 gets three decimals so a depeg is visible", () => {
  assert.equal(formatMoney("0.9991", { stablecoin: true }).head, "0.999");
  assert.equal(formatMoney("1.0004", { stablecoin: true }).head, "1.000");
  // Without the flag the same value rounds into the ordinary band and says
  // nothing is wrong, which is exactly what the band exists to prevent.
  assert.equal(formatMoney("0.9991").head, "0.9991");
  // Outside the band the ordinary rules apply.
  assert.equal(formatMoney("1.2500", { stablecoin: true }).head, "1.25");
});

test("at or above one the figure is two decimals, grouped, truncated", () => {
  assert.equal(figureText(formatMoney("1234.56")), "1,234.56");
  assert.equal(figureText(formatMoney("1234567.891")), "1,234,567.89");
  assert.equal(formatMoney("1234567.891").abbreviated, true);
  assert.equal(figureText(formatMoney("1.999")), "1.99");
  assert.equal(figureText(formatMoney("-42.5")), `${MINUS}42.50`);
  assert.equal(figureText(formatMoney("42.5", { signed: true })), "+42.50");
});

test("compact notation truncates and can never round up across a threshold", () => {
  // The bug this rule exists for: Intl's default renders 999,500 as "1M", so a
  // 999,999 position and a 1,000,000 one become the same pixels.
  assert.equal(figureText(formatMoney("999500.00", { compact: true })), "999.5K");
  assert.equal(figureText(formatMoney("999999.99", { compact: true })), "999.9K");
  assert.equal(figureText(formatMoney("1000000.00", { compact: true })), "1M");
  assert.equal(figureText(formatMoney("1234567.00", { compact: true })), "1.23M");
  assert.equal(figureText(formatMoney("12345678.00", { compact: true })), "12.3M");
  assert.equal(figureText(formatMoney("2500000000.00", { compact: true })), "2.5B");
  // Under a thousand nothing is abbreviated at all.
  assert.equal(figureText(formatMoney("999.99", { compact: true })), "999.99");
  // And every abbreviated form declares itself lossy.
  assert.equal(formatMoney("1234567.00", { compact: true }).abbreviated, true);
  assert.equal(formatMoney("1234567.00", { compact: true }).exact, "1,234,567.00");
});

test("a count is never abbreviated", () => {
  assert.equal(figureText(formatCount("107429")), "107,429");
  assert.equal(figureText(formatCount(1104)), "1,104");
  assert.equal(formatCount("1104").abbreviated, false);
  assert.equal(figureText(formatCount("0")), "0");
  assert.equal(formatCount("0").sign, "");
});

test("a percentage is signed with U+2212, two decimals below 100 and none above", () => {
  assert.equal(figureText(formatPercent("4.1234")), "+4.12%");
  assert.equal(figureText(formatPercent("-9.15")), `${MINUS}9.15%`);
  assert.equal(figureText(formatPercent("128.4")), "+128%");
  assert.equal(figureText(formatPercent("100")), "+100%");
  assert.equal(figureText(formatPercent("99.999")), "+99.99%");
  // A hyphen is not a minus sign.
  assert.equal(formatPercent("-1.00").sign, MINUS);
  assert.notEqual(formatPercent("-1.00").sign, "-");
});

test("zero percent is neutral, because zero is not a direction", () => {
  const zero = formatPercent("0.00");
  assert.equal(figureText(zero), "0%");
  assert.equal(zero.direction, "flat");
  assert.equal(zero.sign, "");
});

test("basis points become a percentage by moving the point, not by dividing", () => {
  assert.equal(figureText(percentFromBps(1234)), "+12.34%");
  assert.equal(figureText(percentFromBps(25)), "+0.25%");
  assert.equal(figureText(percentFromBps(0)), "0%");
  assert.equal(figureText(percentFromBps(-50)), `${MINUS}0.50%`);
  assert.equal(figureText(formatBpsFigure(1234)), "1,234 bps");
  assert.equal(figureText(formatBpsFigure(0)), "0 bps");
  assert.throws(() => percentFromBps(1.5), MoneyFormatError);
});

test("base units become a decimal by moving the point through the digits", () => {
  assert.equal(fromBaseUnits("10000000000", 6), "10000.000000");
  assert.equal(fromBaseUnits("1", 6), "0.000001");
  assert.equal(fromBaseUnits("-1500000000", 9), "-1.500000000");
  assert.equal(fromBaseUnits("42", 0), "42");
  assert.throws(() => fromBaseUnits("1.5", 6), MoneyFormatError);
  assert.throws(() => fromBaseUnits("1", -1), MoneyFormatError);
});

test("a quantity takes its scale from the instrument, not from the value", () => {
  // Six decimals is what a Credit has, so a whole Credit shows six of them.
  assert.equal(figureText(formatUnits("1000000", 6, { symbol: "Credits" })), "1.000000 Credits");
  // An indivisible unit shows none: half of one cannot be held.
  assert.equal(figureText(formatUnits("1104", 0, { symbol: "units" })), "1,104 units");
  assert.equal(formatUnits("1000000", 6).abbreviated, false);
});

test("a malformed value is refused rather than coerced into something readable", () => {
  assert.throws(() => formatMoney("not a number"), MoneyFormatError);
  assert.throws(() => formatMoney("1,234.56"), MoneyFormatError);
  assert.throws(() => formatCount("1.5"), MoneyFormatError);
});

test("an identifier keeps its first five and last four, and short ones are untouched", () => {
  assert.equal(truncateIdentifier("So11111111111111111111111111111111111111112"), "So111…1112");
  assert.equal(truncateIdentifier("short"), "short");
  assert.equal(truncateIdentifier("exactlytwelv"), "exactlytwelv");
});

test("a timestamp is absolute UTC with the whole instant still available", () => {
  assert.equal(utcClock("2026-09-10T06:22:39.123Z"), "06:22:39 UTC");
  assert.equal(utcStamp("2026-09-10T06:22:39.123Z"), "2026-09-10T06:22:39.123Z");
  assert.equal(utcClock(undefined), "not recorded");
  assert.equal(utcStamp(""), "not recorded");
});

test("staleness escalates on the clock and reports an unreadable stamp as unknown", () => {
  const at = "2026-09-10T06:22:39.000Z";
  const plus = (ms: number): Date => new Date(Date.parse(at) + ms);
  assert.equal(staleness(at, plus(0)), "fresh");
  assert.equal(staleness(at, plus(4_999)), "fresh");
  assert.equal(staleness(at, plus(5_000)), "aging");
  assert.equal(staleness(at, plus(29_999)), "aging");
  assert.equal(staleness(at, plus(30_000)), "stale");
  assert.equal(staleness(undefined, plus(0)), "unknown");
  assert.equal(ageMillis(at, plus(1_500)), 1_500);
  // A stamp in the future is clamped rather than reported as negative age.
  assert.equal(ageMillis(at, plus(-4_000)), 0);
});

test("age is reported in the coarsest unit that still says something", () => {
  const at = "2026-09-10T06:22:39.000Z";
  const plus = (ms: number): Date => new Date(Date.parse(at) + ms);
  assert.equal(formatAge(at, plus(42_000)), "42s");
  assert.equal(formatAge(at, plus(7 * 60_000)), "7m");
  assert.equal(formatAge(at, plus(3 * 3_600_000)), "3h");
  assert.equal(formatAge(at, plus(2 * 86_400_000)), "2d");
  assert.equal(formatAge(undefined), "not recorded");
});

test("a proportion is exact integer arithmetic and truncates rather than rounds", () => {
  assert.equal(percentOfTotal("1", "3"), "33");
  assert.equal(percentOfTotal("2", "3"), "66");
  assert.equal(percentOfTotal("999999999999999999999", "1000000000000000000000"), "99");
  assert.equal(percentOfTotal("0", "100"), "0");
  // No total means no proportion. It does not mean zero.
  assert.equal(percentOfTotal("5", "0"), undefined);
  assert.equal(percentOfTotal("5", "not a number"), undefined);
});
