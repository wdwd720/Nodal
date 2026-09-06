import assert from "node:assert/strict";
import { test } from "node:test";

import {
  MoneyFormatError,
  compareUsd,
  formatBps,
  formatBpsRaw,
  formatDecimalString,
  formatQuantity,
  formatUsd,
  parseQuantityInput,
  parseUsdAmountInput,
  quantitySign,
  usdSign,
} from "./money.ts";

test("formatUsd groups digits and keeps both fraction digits", () => {
  const cases: ReadonlyArray<readonly [string, string]> = [
    ["0.00", "$0.00"],
    ["0.01", "$0.01"],
    ["1234.56", "$1,234.56"],
    ["10000.00", "$10,000.00"],
    ["-1234.56", "-$1,234.56"],
    ["1000000000.99", "$1,000,000,000.99"],
    ["-0.00", "$0.00"],
  ];
  for (const [input, expected] of cases) {
    assert.equal(formatUsd(input), expected, input);
  }
});

test("formatUsd signs only when asked, and never signs zero", () => {
  assert.equal(formatUsd("12.30", { signed: true }), "+$12.30");
  assert.equal(formatUsd("-12.30", { signed: true }), "-$12.30");
  assert.equal(formatUsd("0.00", { signed: true }), "$0.00");
  assert.equal(formatUsd("12.30", { symbol: false }), "12.30");
});

test("formatUsd refuses anything that is not the wire contract", () => {
  const bad = ["12.3", "12", "12.345", "", "1,234.56", "1e3", "NaN", "  1.00", "+1.00"];
  for (const input of bad) {
    assert.throws(() => formatUsd(input), MoneyFormatError, input);
  }
});

test("values that lose precision as IEEE-754 doubles survive formatting exactly", () => {
  // Every one of these is a value a double cannot hold. Formatting is string
  // surgery, so the digits that come out are the digits that came in.
  const exact: ReadonlyArray<readonly [string, string]> = [
    ["9007199254740993.01", "$9,007,199,254,740,993.01"],
    ["0.10", "$0.10"],
    ["0.20", "$0.20"],
    ["0.30", "$0.30"],
    ["1000000000000000000000.99", "$1,000,000,000,000,000,000,000.99"],
    ["-9007199254740993.99", "-$9,007,199,254,740,993.99"],
  ];
  for (const [input, expected] of exact) {
    assert.equal(formatUsd(input), expected, input);
    // The digits are preserved verbatim; a float round-trip would not be.
    assert.equal(formatUsd(input).replace(/[$,+-]/g, ""), input.replace(/^-/, ""));
  }
});

test("usdSign reads the digits rather than subtracting", () => {
  assert.equal(usdSign("0.00"), 0);
  assert.equal(usdSign("-0.00"), 0);
  assert.equal(usdSign("0.01"), 1);
  assert.equal(usdSign("-0.01"), -1);
  assert.equal(usdSign("9007199254740993.01"), 1);
});

test("compareUsd orders values a double would tie", () => {
  assert.equal(compareUsd("9007199254740993.00", "9007199254740992.00"), 1);
  assert.equal(compareUsd("9007199254740992.00", "9007199254740993.00"), -1);
  assert.equal(compareUsd("1.00", "1.00"), 0);
  assert.equal(compareUsd("-2.00", "-1.00"), -1);
  assert.equal(compareUsd("-1.00", "1.00"), -1);
  assert.equal(compareUsd("0.00", "-0.00"), 0);
  const sorted = ["10.00", "-3.00", "2.50", "0.00"].slice().sort(compareUsd);
  assert.deepEqual(sorted, ["-3.00", "0.00", "2.50", "10.00"]);
});

test("formatQuantity moves the decimal point through base units", () => {
  const cases: ReadonlyArray<readonly [string, number, string]> = [
    ["10000000000", 6, "10,000"],
    ["1", 6, "0.000001"],
    ["0", 6, "0"],
    ["1500000000", 9, "1.5"],
    ["123456789", 9, "0.123456789"],
    ["999999999999999999999999", 18, "999,999.999999999999999999"],
    ["-1500000000", 9, "-1.5"],
    ["-0", 9, "0"],
  ];
  for (const [units, decimals, expected] of cases) {
    assert.equal(formatQuantity(units, decimals), expected, units);
  }
});

test("formatQuantity honours precision bounds without rounding", () => {
  assert.equal(formatQuantity("1999999999", 9, { maxFractionDigits: 2 }), "1.99");
  assert.equal(formatQuantity("1000000000", 9, { minFractionDigits: 2 }), "1.00");
  assert.equal(formatQuantity("1000000000", 9), "1");
});

test("formatQuantity refuses non-integer base units", () => {
  assert.throws(() => formatQuantity("1.5", 9), MoneyFormatError);
  assert.throws(() => formatQuantity("", 9), MoneyFormatError);
  assert.throws(() => formatQuantity("1e9", 9), MoneyFormatError);
  assert.throws(() => formatQuantity("100", 1.5), MoneyFormatError);
});

test("formatBps moves the point instead of dividing", () => {
  assert.equal(formatBps(0), "0%");
  assert.equal(formatBps(1), "0.01%");
  assert.equal(formatBps(50), "0.5%");
  assert.equal(formatBps(1234), "12.34%");
  assert.equal(formatBps(10000), "100%");
  assert.equal(formatBps(-25), "-0.25%");
  assert.equal(formatBpsRaw(1234), "1,234 bps");
  assert.throws(() => formatBps(1.5), MoneyFormatError);
});

test("formatDecimalString keeps every digit of a price", () => {
  assert.equal(formatDecimalString("123456.789012345678"), "123,456.789012345678");
  assert.equal(formatDecimalString("0.000000000000000001"), "0.000000000000000001");
  assert.equal(formatDecimalString("-1000.5"), "-1,000.5");
  assert.throws(() => formatDecimalString("1.2.3"), MoneyFormatError);
});

test("parseUsdAmountInput canonicalises exactly or refuses", () => {
  assert.deepEqual(parseUsdAmountInput("25"), { ok: true, value: "25.00", error: "" });
  assert.deepEqual(parseUsdAmountInput("25.5"), { ok: true, value: "25.50", error: "" });
  assert.deepEqual(parseUsdAmountInput(" 1,234.56 "), { ok: true, value: "1234.56", error: "" });
  assert.deepEqual(parseUsdAmountInput("0007.10"), { ok: true, value: "7.10", error: "" });
  assert.equal(parseUsdAmountInput("25.555").ok, false);
  assert.equal(parseUsdAmountInput("0").ok, false);
  assert.equal(parseUsdAmountInput("0.00").ok, false);
  assert.equal(parseUsdAmountInput("-5").ok, false);
  assert.equal(parseUsdAmountInput("abc").ok, false);
  assert.equal(parseUsdAmountInput("").ok, false);
  assert.equal(parseUsdAmountInput("1e5").ok, false);
});

test("parseQuantityInput shifts digits and refuses over-precision", () => {
  assert.deepEqual(parseQuantityInput("1.5", 9), { ok: true, value: "1500000000", error: "" });
  assert.deepEqual(parseQuantityInput("10", 6), { ok: true, value: "10000000", error: "" });
  assert.deepEqual(parseQuantityInput("0.000001", 6), { ok: true, value: "1", error: "" });
  assert.equal(parseQuantityInput("0.0000001", 6).ok, false);
  assert.equal(parseQuantityInput("0", 6).ok, false);
  assert.equal(parseQuantityInput("1.5", -1).ok, false);
});

test("a full round-trip through parse and format is lossless", () => {
  const typed = ["0.000000001", "123456789.123456789", "1", "999999999999.999999999"];
  for (const value of typed) {
    const parsed = parseQuantityInput(value, 9);
    assert.equal(parsed.ok, true, value);
    // Trailing zeros are trimmed on the way out, so compare against the
    // canonical form of the input rather than the input's own padding.
    const canonical = value.includes(".") ? value.replace(/0+$/, "").replace(/\.$/, "") : value;
    assert.equal(formatQuantity(parsed.value, 9).replace(/,/g, ""), canonical, value);
  }
});

test("quantitySign reads digits", () => {
  assert.equal(quantitySign("0"), 0);
  assert.equal(quantitySign("-0"), 0);
  assert.equal(quantitySign("1"), 1);
  assert.equal(quantitySign("-1"), -1);
});
