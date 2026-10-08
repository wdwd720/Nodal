/**
 * The fees section's pure helpers: what statuses and components mean, and
 * the checks that run on what someone typed before the fee engine is asked
 * anything. None of them computes a fee.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  BOOKING_MODE_MEANING,
  BOOKING_STATUS_MEANING,
  CUSTOMER_FAVOURABLE_ROUNDING,
  FEE_BOOKING_MODES,
  FEE_BOOKING_STATUSES,
  FEE_LINE_KINDS,
  FEE_PERIOD_STATUSES,
  LINE_KIND_MEANING,
  PERIOD_STATUS_MEANING,
  ROUNDING_FIELDS,
  ROUNDING_MODES,
  bookingStatusTone,
  componentLabel,
  parseReasonCode,
  parseScheduleKey,
  parseStrategyRef,
  parseTierLines,
  parseUsdInput,
  parseUtcInstant,
  periodStatusTone,
  scheduleQuery,
  verificationSummary,
} from "./fees.ts";

test("every status, mode and line kind says what it means", () => {
  for (const status of FEE_PERIOD_STATUSES) {
    assert.ok(PERIOD_STATUS_MEANING[status].length > 0, status);
  }
  for (const kind of FEE_LINE_KINDS) {
    assert.ok(LINE_KIND_MEANING[kind].length > 0, kind);
  }
  for (const mode of FEE_BOOKING_MODES) {
    assert.ok(BOOKING_MODE_MEANING[mode].length > 0, mode);
  }
  for (const status of FEE_BOOKING_STATUSES) {
    assert.ok(BOOKING_STATUS_MEANING[status].length > 0, status);
  }
  assert.equal(periodStatusTone("CRYSTALLIZED"), "good");
  assert.equal(periodStatusTone("OPEN"), "info");
  assert.equal(periodStatusTone("REVERSED"), "warn");
  assert.equal(periodStatusTone("SOMETHING_NEW"), "neutral");
  assert.equal(bookingStatusTone("REVERSED"), "warn");
});

test("a fee component is named, and an unknown one keeps the backend's own code", () => {
  assert.equal(componentLabel("PERCENTAGE_FEE"), "Percentage fee");
  assert.equal(componentLabel("VENUE_PASS_THROUGH"), "Venue pass-through");
  assert.equal(componentLabel("A_NEW_COMPONENT"), "A_NEW_COMPONENT");
});

test("the default rounding names a mode for every field, each one the backend spells", () => {
  for (const field of ROUNDING_FIELDS) {
    assert.ok((ROUNDING_MODES as readonly string[]).includes(CUSTOMER_FAVOURABLE_ROUNDING[field]), field);
  }
  assert.equal(CUSTOMER_FAVOURABLE_ROUNDING.buy_notional, "down");
  assert.equal(CUSTOMER_FAVOURABLE_ROUNDING.sell_notional, "up");
  assert.equal(CUSTOMER_FAVOURABLE_ROUNDING.slippage_reserve, "up");
});

test("a verification is either re-proved or names the code it failed with", () => {
  assert.deepEqual(verificationSummary({ verified: true }), { tone: "good", label: "re-proved from the record" });
  assert.deepEqual(verificationSummary({ verified: false, code: "VALIDATION_FAILED" }), {
    tone: "bad",
    label: "does not verify (VALIDATION_FAILED)",
  });
  assert.deepEqual(verificationSummary({ verified: false }), { tone: "bad", label: "does not verify" });
});

test("a typed USD figure is normalised by moving digits", () => {
  assert.deepEqual(parseUsdInput("1,250.5", "NAV"), { ok: true, value: "1250.50" });
  assert.deepEqual(parseUsdInput("0012", "NAV"), { ok: true, value: "12.00" });
  assert.deepEqual(parseUsdInput("7.", "NAV"), { ok: true, value: "7.00" });
  assert.equal(parseUsdInput("-5.00", "NAV").ok, false, "unsigned unless asked");
  assert.deepEqual(parseUsdInput("-5", "Flow", true), { ok: true, value: "-5.00" });
  assert.deepEqual(parseUsdInput("-0.00", "Flow", true), { ok: true, value: "0.00" });
  assert.equal(parseUsdInput("1.234", "NAV").ok, false);
  assert.equal(parseUsdInput("abc", "NAV").ok, false);
  assert.equal(parseUsdInput("", "NAV").ok, false);
});

test("an instant is written in UTC and returned in canonical form", () => {
  assert.deepEqual(parseUtcInstant("2026-10-07T08:00:00Z", "At"), { ok: true, value: "2026-10-07T08:00:00.000Z" });
  assert.deepEqual(parseUtcInstant(" 2026-10-07T08:00Z ", "At"), { ok: true, value: "2026-10-07T08:00:00.000Z" });
  assert.equal(parseUtcInstant("2026-10-07 08:00:00", "At").ok, false);
  assert.equal(parseUtcInstant("2026-10-07T08:00:00+02:00", "At").ok, false);
  assert.equal(parseUtcInstant("2026-13-45T08:00:00Z", "At").ok, false);
});

test("volume tiers are read one per line, a dash keeping the base rate", () => {
  assert.deepEqual(parseTierLines("100000 8 -\n\n250000.5 - 15"), {
    ok: true,
    value: [
      { min_trailing_volume_usd: "100000.00", overrides_percentage_fee: true, percentage_fee_bps: 8, overrides_spread: false, spread_bps: 0 },
      { min_trailing_volume_usd: "250000.50", overrides_percentage_fee: false, percentage_fee_bps: 0, overrides_spread: true, spread_bps: 15 },
    ],
  });
  assert.deepEqual(parseTierLines("  "), { ok: true, value: [] });
  assert.equal(parseTierLines("100000 8").ok, false);
  assert.equal(parseTierLines("100000 10001 -").ok, false);
  assert.equal(parseTierLines("abc 8 -").ok, false);
  const bad = parseTierLines("1 1 1\n2 x 1");
  assert.equal(bad.ok, false);
  assert.match(bad.ok ? "" : bad.error, /line 2/);
});

test("keys, reason codes and strategy references are the shapes the backend takes", () => {
  assert.deepEqual(parseScheduleKey(" orders:paper:sbx/usd "), { ok: true, value: "orders:paper:sbx/usd" });
  assert.equal(parseScheduleKey("Orders").ok, false);
  assert.equal(parseScheduleKey("").ok, false);
  assert.equal(parseScheduleKey("a".repeat(129)).ok, false);
  assert.deepEqual(parseReasonCode("CUSTOMER_COMPLAINT"), { ok: true, value: "CUSTOMER_COMPLAINT" });
  assert.equal(parseReasonCode("customer").ok, false);
  assert.equal(parseReasonCode("AB").ok, false);
  assert.deepEqual(parseStrategyRef("grid-v1"), { ok: true, value: "grid-v1" });
  assert.equal(parseStrategyRef("-grid").ok, false);
  assert.equal(scheduleQuery("orders:paper/x y"), "?key=orders%3Apaper%2Fx%20y");
});
