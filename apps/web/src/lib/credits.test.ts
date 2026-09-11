import assert from "node:assert/strict";
import { test } from "node:test";

import { MoneyFormatError } from "./money.ts";
import {
  CREDIT_DECIMALS,
  PRESET_AMOUNTS_MINOR,
  minorToUsd,
  outOfBounds,
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
