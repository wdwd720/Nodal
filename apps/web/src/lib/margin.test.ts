import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ACCOUNT_STATUS_MEANING,
  CALL_STATUS_MEANING,
  CASE_STATUS_MEANING,
  LEVERAGED_MARGIN_MODES,
  MARGIN_ACCOUNT_STATUSES,
  MARGIN_CALL_STATUSES,
  MARGIN_CASE_STATUSES,
  MARGIN_PLAN_DECISIONS,
  MARGIN_STRESS_SCENARIOS,
  PLAN_DECISION_MEANING,
  STRESS_SCENARIO_MEANING,
  accountStatusTone,
  assetById,
  confirmAttempt,
  formatLeverage,
  formatMarginPrice,
  isLeveragedMode,
  marginModeLabel,
  sameRequest,
  simulatedAssets,
} from "./margin.ts";
import { MoneyFormatError } from "./money.ts";

test("leverage moves the point through the digits and never divides", () => {
  assert.equal(formatLeverage(28654), "2.8654×");
  assert.equal(formatLeverage(50000), "5×");
  assert.equal(formatLeverage(10000), "1×");
  assert.equal(formatLeverage(0), "0×");
  assert.equal(formatLeverage(5), "0.0005×");
  assert.equal(formatLeverage(1_000_000_000), "100000×");
  assert.throws(() => formatLeverage(-1), MoneyFormatError);
});

test("a price is its mantissa with the point moved by its scale", () => {
  assert.equal(formatMarginPrice("1001000", 4), "100.1");
  assert.equal(formatMarginPrice("100", 0), "100");
  assert.equal(formatMarginPrice("399000", 4), "39.9");
  assert.throws(() => formatMarginPrice("1.5", 2), MoneyFormatError);
});

test("a confirmed request keeps its key across retries and a changed request gets a new one", () => {
  let minted = 0;
  const mint = (): string => {
    minted = minted + 1;
    return `key-${String(minted)}`;
  };
  const first = confirmAttempt(undefined, { account_id: "a", amount: "100" }, mint);
  assert.equal(first.idempotencyKey, "key-1");

  const retry = confirmAttempt(first, { account_id: "a", amount: "100" }, mint);
  assert.equal(retry.idempotencyKey, "key-1", "a retry of the same request is the same request");
  assert.equal(minted, 1, "no key is minted for a retry");

  const changed = confirmAttempt(retry, { account_id: "a", amount: "101" }, mint);
  assert.equal(changed.idempotencyKey, "key-2");
  assert.deepEqual(changed.request, { account_id: "a", amount: "101" });
});

test("two requests are the same only when every field is", () => {
  assert.equal(sameRequest({ a: "1", b: "2" }, { a: "1", b: "2" }), true);
  assert.equal(sameRequest({ a: "1", b: "2" }, { a: "1", b: "3" }), false);
});

test("the leveraged modes are offered and the fully paid mode is named, not spelled", () => {
  assert.deepEqual([...LEVERAGED_MARGIN_MODES], ["ISOLATED", "CROSS"]);
  assert.equal(isLeveragedMode("CROSS"), true);
  assert.equal(isLeveragedMode("ISOLATED"), true);
  assert.equal(isLeveragedMode("PAPER"), false);
  assert.match(marginModeLabel("ISOLATED"), /one risky asset/);
  assert.match(marginModeLabel("CROSS"), /shared/);
  assert.match(marginModeLabel("anything else"), /no borrowing and no leverage/);
});

test("every status, decision and scenario the contract declares has a meaning", () => {
  for (const status of MARGIN_ACCOUNT_STATUSES) assert.ok(ACCOUNT_STATUS_MEANING[status].length > 10, status);
  for (const status of MARGIN_CALL_STATUSES) assert.ok(CALL_STATUS_MEANING[status].length > 10, status);
  for (const status of MARGIN_CASE_STATUSES) assert.ok(CASE_STATUS_MEANING[status].length > 10, status);
  for (const decision of MARGIN_PLAN_DECISIONS) assert.ok(PLAN_DECISION_MEANING[decision].length > 10, decision);
  for (const scenario of MARGIN_STRESS_SCENARIOS) assert.ok(STRESS_SCENARIO_MEANING[scenario].length > 10, scenario);
  assert.equal(accountStatusTone("NORMAL"), "good");
  assert.equal(accountStatusTone("MARGIN_CALL"), "bad");
  assert.equal(accountStatusTone("CLOSED"), "neutral");
});

test("only active simulated assets are offered, in symbol order, and found by id in any case", () => {
  const registry = [
    { id: "B", kind: "SIMULATED", symbol: "ZED", decimals: 9, status: "ACTIVE" },
    { id: "A", kind: "SPL_TOKEN", symbol: "USDC", decimals: 6, status: "ACTIVE" },
    { id: "C", kind: "SIMULATED", symbol: "ABC", decimals: 6, status: "ACTIVE" },
    { id: "D", kind: "SIMULATED", symbol: "OLD", decimals: 6, status: "DELISTED" },
  ];
  assert.deepEqual(
    simulatedAssets(registry).map((asset) => asset.symbol),
    ["ABC", "ZED"],
  );
  assert.equal(assetById(registry, "c")?.symbol, "ABC");
  assert.equal(assetById(registry, "missing"), undefined);
});
