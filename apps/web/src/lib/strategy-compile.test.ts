/**
 * Proves the browser compile path produces exactly what the SDK produces.
 *
 * The web app cannot call the SDK's `compile()`, because that finishes with
 * Node's crypto. It therefore re-does the digest through WebCrypto. This test
 * builds real documents, compiles them both ways, and asserts the normalised
 * document, the effect set, the canonical semantic projection and the hash all
 * agree. If the SDK's algorithm changes, this fails here rather than in
 * production, where the symptom would be a server rejecting a hash it could not
 * reproduce.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { StrategyBuildError, decimal, strategy } from "@controlplane/strategy-sdk";

import { prepare, semanticHashInBrowser, sha256Hex } from "./strategy-compile.ts";

const ACCOUNT = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const USER = "01a075f1-f846-719e-bcbb-493c8281c524";
const INSTRUMENT = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";

function buildValid(notional: string) {
  return strategy({ strategyId: "test_strategy", accountId: ACCOUNT, userId: USER })
    .instrument("target", INSTRUMENT)
    .onInterval("tick", 300_000)
    .dependency({
      name: "price_feed",
      kind: "PRICE",
      tool_code: "price.spot",
      tool_version: 1,
      dependency_version: 1,
      params: {},
      max_age_ms: 30_000,
      required: true,
    })
    .signal(
      "trend",
      {
        window: {
          fn: "RETURN",
          dependency: "price_feed",
          path: "price",
          lookback_ms: 3_600_000,
          scale: 4,
          rounding: "half_even",
        },
      },
      4,
    )
    .when("trend_is_strong", {
      cmp: { op: "GT", l: { signal: "trend" }, r: { const: decimal("0.0200") } },
    })
    .predict(
      "call_it",
      {
        instrument: "target",
        horizon_ms: 3_600_000,
        direction: "UP",
        probability: { const: decimal("0.5500") },
        expected_return_bps: { const: decimal("120") },
        downside_probability: { const: decimal("0.4500") },
        max_downside_bps: { const: decimal("300") },
        confidence: { const: decimal("0.6000") },
      },
      "trend_is_strong",
    )
    .intent(
      "act_on_it",
      {
        action: "ACQUIRE_NOTIONAL",
        instrument: "target",
        sizing: { kind: "FIXED_NOTIONAL", notional_usd: notional },
        constraints: {
          max_slippage_bps: 50,
          max_fee_bps: 30,
          max_price_impact_bps: 50,
          quote_freshness_ms: 30_000,
          allowed_venues: [],
        },
        deadline_ms: 60_000,
        prediction: "call_it",
      },
      "trend_is_strong",
    )
    .envelope({
      min_allocation: "100.00",
      max_single_trade: "50.00",
      max_position: "500.00",
      max_daily_loss: "100.00",
      instruments: [INSTRUMENT],
      asset_classes: [],
      venues: [],
      max_intents_per_hour: 4,
      max_runs_per_minute: 2,
    });
}

test("the browser path reproduces the SDK's document, effects, projection and hash", async () => {
  for (const notional of ["50.00", "12345.67", "9007199254740993.01"]) {
    const fromSdk = buildValid(notional).compile();
    const fromBrowser = prepare(buildValid(notional));

    assert.deepEqual(fromBrowser.issues, [], notional);
    assert.deepEqual(fromBrowser.document, fromSdk.document, notional);
    assert.deepEqual(fromBrowser.document.effects, fromSdk.document.effects, notional);
    assert.equal(fromBrowser.semanticJson, fromSdk.semanticJson, notional);
    assert.equal(await semanticHashInBrowser(fromBrowser), fromSdk.hash, notional);
  }
});

test("the effect set is derived from the document, not declared", async () => {
  const prepared = prepare(buildValid("50.00"));
  assert.deepEqual(prepared.document.effects, [
    "COMMIT_PREDICTION",
    "CREATE_TRADE_INTENT",
    "READ_MARKET_DATA",
  ]);
  // Nothing that signs, transfers, withdraws or touches policy is expressible.
  for (const forbidden of ["RAW_SIGN", "TRANSFER_VALUE", "WITHDRAW", "CHANGE_RISK", "CHANGE_CAPITAL"]) {
    assert.equal(prepared.document.effects.includes(forbidden as never), false, forbidden);
  }
});

test("an incomplete document reports structural issues instead of hashing", () => {
  const bare = strategy({ strategyId: "", accountId: ACCOUNT, userId: USER });
  const prepared = prepare(bare);
  assert.ok(prepared.issues.length > 0);
  assert.ok(prepared.issues.some((issue) => issue.includes("strategy_id")));
  // The SDK refuses the same document, which is the behaviour being mirrored.
  assert.throws(() => bare.compile(), StrategyBuildError);
});

test("WebCrypto digests match the known SHA-256 of the empty string", async () => {
  assert.equal(
    await sha256Hex(""),
    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  );
});
