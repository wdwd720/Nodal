import assert from "node:assert/strict";
import { test } from "node:test";

import { MoneyFormatError } from "./money.ts";
import {
  CREDIT_DECIMALS,
  PRESET_AMOUNTS_MINOR,
  creditScale,
  hasCredits,
  minorStringToUsd,
  minorToUsd,
  outOfBounds,
  unaccountedBaseUnits,
  usdToMinor,
} from "./credits.ts";

test("a canonical USD string becomes exact minor units", () => {
  const cases: ReadonlyArray<readonly [string, number]> = [
    ["0.01", 1],
    ["0.10", 10],
    ["1.00", 100],
    ["10.00", 1000],
    ["25.00", 2500],
    ["99.99", 9999],
    ["100.00", 10000],
    ["10000.00", 1000000],
    ["-25.00", -2500],
  ];
  for (const [value, minor] of cases) {
    assert.equal(usdToMinor(value), minor, value);
  }
});

test("minor units become the canonical USD string again", () => {
  const cases: ReadonlyArray<readonly [number, string]> = [
    [0, "0.00"],
    [1, "0.01"],
    [10, "0.10"],
    [100, "1.00"],
    [2500, "25.00"],
    [1000000, "10000.00"],
    [-2500, "-25.00"],
  ];
  for (const [minor, value] of cases) {
    assert.equal(minorToUsd(minor), value, String(minor));
  }
});

test("the round trip is exact for every preset and every bound", () => {
  for (const minor of [...PRESET_AMOUNTS_MINOR, 1, 100, 999_999, 1_000_000]) {
    assert.equal(usdToMinor(minorToUsd(minor)), minor, String(minor));
  }
});

test("an amount that is not the two-decimal form is refused, never repaired", () => {
  for (const bad of ["25", "25.0", "25.000", "", "  25.00", "1,000.00", "$25.00", "abc"]) {
    assert.throws(() => usdToMinor(bad), MoneyFormatError, bad);
  }
});

test("a non-integer number of minor units is refused", () => {
  assert.throws(() => minorToUsd(Number.NaN), MoneyFormatError);
  assert.throws(() => minorToUsd(Number.POSITIVE_INFINITY), MoneyFormatError);
});

test("the presets are the four the goal document names", () => {
  assert.deepEqual(
    PRESET_AMOUNTS_MINOR.map(minorToUsd),
    ["10.00", "25.00", "50.00", "100.00"],
  );
});

test("bounds are stated in the currency the customer typed, not in minor units", () => {
  const bounds = { minMinor: 100, maxMinor: 1_000_000 };
  assert.equal(outOfBounds(2500, bounds), "");
  assert.equal(outOfBounds(100, bounds), "");
  assert.equal(outOfBounds(1_000_000, bounds), "");
  assert.match(outOfBounds(99, bounds), /smallest purchase .* is 1\.00\./);
  assert.match(outOfBounds(1_000_001, bounds), /largest single purchase .* is 10000\.00\./);
});

test("a Credit's scale is stated in exactly one place", () => {
  // Not an assertion about the value so much as about where it lives: every
  // Credit figure in the product is rendered at this scale, and a second copy
  // of it somewhere else is how two pages come to disagree about a balance.
  assert.equal(CREDIT_DECIMALS, 6);
});

test("minor units that arrived as a string become the same USD string", () => {
  const cases: ReadonlyArray<readonly [string, string]> = [
    ["0", "0.00"],
    ["1", "0.01"],
    ["10", "0.10"],
    ["100", "1.00"],
    ["2500", "25.00"],
    ["1000000", "10000.00"],
    // A reversal is negative, and the sign survives.
    ["-2500", "-25.00"],
    // Bigger than a double can hold exactly. String surgery does not care.
    ["123456789012345678901", "1234567890123456789.01"],
  ];
  for (const [value, usd] of cases) {
    assert.equal(minorStringToUsd(value), usd, value);
  }
});

test("the two minor-unit conversions agree wherever both apply", () => {
  for (const minor of [0, 1, 10, 100, 2500, 1_000_000]) {
    assert.equal(minorStringToUsd(String(minor)), minorToUsd(minor), String(minor));
  }
});

test("anything that is not an exact integer of minor units is refused", () => {
  for (const bad of ["", "25.00", "1e3", "1,000", " 100", "abc", "+100"]) {
    assert.throws(() => minorStringToUsd(bad), MoneyFormatError, bad);
  }
});

/* ---------------------------------------------------------------------------
 * The scale of a Credit, and the arithmetic a segmented bar needs.
 * ------------------------------------------------------------------------ */

test("the scale comes from the response, and the constant is only a fallback", () => {
  // What the API states wins, including a scale nothing in this repository
  // registers: the point of reading it is that a deployment could.
  assert.equal(creditScale(6), 6);
  assert.equal(creditScale(0), 0, "an indivisible Credit is a legitimate scale");
  assert.equal(creditScale(8), 8);

  // Absent, or not a scale anything could render at, falls back to the
  // documented one rather than throwing inside a render.
  assert.equal(creditScale(undefined), CREDIT_DECIMALS);
  assert.equal(creditScale(-1), CREDIT_DECIMALS);
  assert.equal(creditScale(1.5), CREDIT_DECIMALS);
  assert.equal(creditScale(Number.NaN), CREDIT_DECIMALS);
  assert.equal(creditScale(1000), CREDIT_DECIMALS);
});

test("a balance holds something, or it does not, at any size", () => {
  assert.equal(hasCredits("0"), false);
  assert.equal(hasCredits("000"), false);
  assert.equal(hasCredits("1"), true);
  assert.equal(hasCredits("300000000"), true);
  assert.equal(hasCredits("99999999999999999999999999999999"), true, "past Number.MAX_SAFE_INTEGER");
  assert.equal(hasCredits("-5"), false, "a negative balance is not a balance to show");
  assert.equal(hasCredits("not a number"), false);
  assert.equal(hasCredits(""), false);
});

test("a segmented bar's parts either reach its whole or the difference is visible", () => {
  // The case F-156 was: gross 300, spendable 0, frozen 0, and a `reversed`
  // bucket the page never passed. The bar drew nothing and said nothing.
  assert.equal(unaccountedBaseUnits(["0", "0"], "300"), "300");

  // Every bucket passed: nothing left over, nothing drawn.
  assert.equal(unaccountedBaseUnits(["0", "0", "300"], "300"), undefined);
  assert.equal(unaccountedBaseUnits(["100", "200"], "300"), undefined);

  // Exact at any size: no float, no rounding.
  assert.equal(
    unaccountedBaseUnits(["1"], "100000000000000000000000000000001"),
    "100000000000000000000000000000000",
  );

  // Parts that overshoot their own whole are a caller mixing two responses.
  // The bar refuses to draw it rather than rendering a negative width.
  assert.equal(unaccountedBaseUnits(["400"], "300"), undefined);

  // No whole to measure against, and unreadable input, are both undefined.
  assert.equal(unaccountedBaseUnits(["100"], undefined), undefined);
  assert.equal(unaccountedBaseUnits(["oops"], "300"), undefined);
  assert.equal(unaccountedBaseUnits([], "300"), "300");
});
