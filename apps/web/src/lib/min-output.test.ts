/**
 * The minimum the customer agreed to, checked at the magnitudes it will meet.
 *
 * This is the one number on the trading ticket the browser produces rather than
 * reads, so the test is aimed at the two properties that make producing it
 * defensible: it is exact at any size, and every remainder rounds towards the
 * customer rather than away from them.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { TOLERANCES, ToleranceError, meetsMinimum, minimumOutput } from "./min-output.ts";

test("a whole-percent tolerance comes out exact", () => {
  assert.equal(minimumOutput("1000000", 100), "990000");
  assert.equal(minimumOutput("1000000", 500), "950000");
  assert.equal(minimumOutput("1000000", 50), "995000");
});

test("a tolerance of nothing is the quote itself, and of everything is nothing", () => {
  assert.equal(minimumOutput("123456789", 0), "123456789");
  assert.equal(minimumOutput("123456789", 10_000), "0");
});

test("a remainder rounds the minimum up, never down", () => {
  // 7 * 9900 / 10000 = 6.93. Rounding down would hand the customer a weaker
  // protection than the one they chose; this rounds to 7.
  assert.equal(minimumOutput("7", 100), "7");
  // 1234 * 9950 / 10000 = 1227.83 -> 1228.
  assert.equal(minimumOutput("1234", 50), "1228");
  // 101 * 9500 / 10000 = 95.95 -> 96.
  assert.equal(minimumOutput("101", 500), "96");
});

test("it stays exact past the largest integer a double holds", () => {
  // 9007199254740993 is Number.MAX_SAFE_INTEGER + 2. A double implementation
  // loses the last digit before it has done any arithmetic at all.
  assert.equal(minimumOutput("9007199254740993", 0), "9007199254740993");
  assert.equal(minimumOutput("10000000000000000000000", 100), "9900000000000000000000");
});

test("zero expected output stays zero at every tolerance", () => {
  for (const tolerance of TOLERANCES) {
    assert.equal(minimumOutput("0", tolerance.bps), "0");
  }
});

test("a malformed input is refused rather than coerced", () => {
  assert.throws(() => minimumOutput("1.5", 100), ToleranceError);
  assert.throws(() => minimumOutput("-100", 100), ToleranceError);
  assert.throws(() => minimumOutput("", 100), ToleranceError);
  assert.throws(() => minimumOutput("1000", -1), ToleranceError);
  assert.throws(() => minimumOutput("1000", 10_001), ToleranceError);
});

test("the offered tolerances are ordered and each one says what it costs", () => {
  assert.ok(TOLERANCES.length >= 2, "a single tolerance is not a choice");
  let previous = 0;
  for (const tolerance of TOLERANCES) {
    assert.ok(tolerance.bps > previous, "tolerances are offered loosest-last");
    previous = tolerance.bps;
    assert.ok(tolerance.label !== "", `${String(tolerance.bps)} bps has a label`);
    assert.ok(tolerance.note.length > 20, `${tolerance.label} says what it means`);
  }
});

test("meeting the minimum is decided by exact comparison", () => {
  assert.equal(meetsMinimum("9007199254740993", "9007199254740992"), true);
  assert.equal(meetsMinimum("9007199254740992", "9007199254740993"), false);
  assert.equal(meetsMinimum("100", "100"), true);
  assert.equal(meetsMinimum("not a number", "100"), undefined);
});
