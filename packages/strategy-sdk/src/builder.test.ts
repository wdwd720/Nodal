import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

import { StrategyBuildError, strategy } from "./builder.ts";
import { semanticHash } from "./hash.ts";
import { deriveEffects } from "./normalize.ts";
import { decimal, decimalToString, usd } from "./types.ts";
import type { Expr, IRDocument } from "./types.ts";

const here = dirname(fileURLToPath(import.meta.url));
const parityDir = join(here, "..", "..", "..", "internal", "strategy", "ir", "testdata", "parity");

function momentumFixture(): IRDocument {
  const raw = readFileSync(join(parityDir, "momentum.json"), "utf8");
  return (JSON.parse(raw) as { ir: IRDocument }).ir;
}

const IDS = {
  strategy: "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e0f",
  account: "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e10",
  user: "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e11",
  instrument: "0192b8e0-1a2b-7c3d-8e4f-5a6b7c8d9e12",
};

const signalRef = (name: string): Expr => ({ signal: name });

/** Rebuilds the momentum strategy through the public builder API. */
function buildMomentum() {
  const fixture = momentumFixture();
  return strategy({
    strategyId: IDS.strategy,
    accountId: IDS.account,
    userId: IDS.user,
    riskPolicyVersion: fixture.risk_policy.version,
    riskPolicyHash: fixture.risk_policy.hash,
  })
    .instrument("sol_usdc", IDS.instrument)
    .onInterval("tick", 5000, 5000)
    .dependency({
      name: "price",
      kind: "PRICE",
      tool_code: "price_spot",
      tool_version: 1,
      dependency_version: 1,
      params: { instrument_id: "sol_usdc" },
      max_age_ms: 500,
      required: true,
    })
    .signal(
      "ret_5m",
      {
        window: {
          fn: "RETURN",
          dependency: "price",
          path: "mid",
          lookback_ms: 300_000,
          scale: 4,
          rounding: "half_even",
        },
      },
      4,
      "half_even",
    )
    .when("momentum_up", {
      cmp: { op: "GT", l: signalRef("ret_5m"), r: { const: decimal("0.0200") } },
    })
    .predict(
      "predict",
      {
        instrument: "sol_usdc",
        horizon_ms: 900_000,
        direction: "UP",
        probability: { const: decimal("0.6500") },
        expected_return_bps: { const: decimal("150") },
        downside_probability: { const: decimal("0.3500") },
        max_downside_bps: { const: decimal("300") },
        confidence: { const: decimal("0.6000") },
      },
      "momentum_up",
    )
    .intent(
      "buy",
      {
        action: "ACQUIRE_NOTIONAL",
        instrument: "sol_usdc",
        sizing: { kind: "FIXED_NOTIONAL", notional_usd: usd(5000) },
        constraints: {
          max_slippage_bps: 50,
          max_fee_bps: 30,
          max_price_impact_bps: 50,
          quote_freshness_ms: 500,
          allowed_venues: ["JUPITER"],
        },
        deadline_ms: 30_000,
        prediction: "predict",
      },
      "momentum_up",
    )
    .dataBudget({
      max_tool_calls_per_run: 4,
      max_tool_calls_per_day: 500,
      max_spend_per_day: usd(500),
      max_lookback_ms: 3_600_000,
    })
    .envelope({
      min_allocation: usd(10000),
      max_single_trade: usd(5000),
      max_position: usd(20000),
      max_daily_loss: usd(3000),
      asset_classes: ["CRYPTO"],
      venues: ["JUPITER"],
      max_intents_per_hour: 6,
      max_runs_per_minute: 12,
    });
}

// The point of the builder: a strategy written with the typed API produces
// the exact document the Go compiler already knows, hash included. If this
// drifts, an SDK author's strategy would compile to a different artifact
// than the same strategy described in words.
test("the builder reproduces the momentum fixture byte for byte", () => {
  const compiled = buildMomentum().compile();
  const fixture = momentumFixture();

  assert.equal(
    compiled.hash,
    "8487051d87f34f11fc81bdab65b5baad3f6ec5cc77872331bfa3fa46f72513d5",
    "the builder must reproduce the golden hash",
  );
  assert.equal(compiled.hash, semanticHash(fixture), "and agree with the stored fixture");
});

test("compile derives the effect set rather than trusting a caller", () => {
  const compiled = buildMomentum().compile();
  assert.deepEqual(compiled.document.effects, [
    "COMMIT_PREDICTION",
    "CREATE_TRADE_INTENT",
    "READ_MARKET_DATA",
  ]);
  assert.deepEqual(compiled.document.effects, deriveEffects(compiled.document));

  // A caller cannot smuggle a capability in: whatever is on the draft, the
  // derived set replaces it.
  const builder = buildMomentum();
  const draft = builder.draft();
  (draft.effects as string[]).push("TRANSFER_VALUE");
  const recompiled = builder.compile();
  assert.ok(!recompiled.document.effects.includes("TRANSFER_VALUE" as never));
});

test("adding an on-chain dependency widens the derived effect set", () => {
  const compiled = buildMomentum()
    .dependency({
      name: "wallet_moves",
      kind: "WALLET_EVENT",
      tool_code: "wallet_events",
      tool_version: 1,
      dependency_version: 1,
      params: { wallet_set: "w1" },
      max_age_ms: 2000,
      required: true,
    })
    .compile();
  assert.ok(compiled.document.effects.includes("READ_ONCHAIN_DATA"));
  assert.notEqual(
    compiled.hash,
    "8487051d87f34f11fc81bdab65b5baad3f6ec5cc77872331bfa3fa46f72513d5",
    "a strategy that reads more is a different strategy",
  );
});

test("the builder refuses a sub-second interval", () => {
  const b = strategy({ strategyId: IDS.strategy, accountId: IDS.account, userId: IDS.user })
    .instrument("sol_usdc", IDS.instrument)
    .onInterval("spin", 100)
    .dependency({
      name: "price",
      kind: "PRICE",
      tool_code: "price_spot",
      tool_version: 1,
      dependency_version: 1,
      params: {},
      max_age_ms: 500,
      required: true,
    })
    .predict("p", {
      instrument: "sol_usdc",
      horizon_ms: 1000,
      direction: "UP",
      probability: { const: decimal("0.5000") },
      expected_return_bps: { const: decimal("10") },
      downside_probability: { const: decimal("0.5000") },
      max_downside_bps: { const: decimal("10") },
      confidence: { const: decimal("0.5000") },
    })
    .envelope({ max_runs_per_minute: 12 });

  assert.throws(
    () => b.compile(),
    (err: unknown) => {
      assert.ok(err instanceof StrategyBuildError);
      assert.ok(err.issues.some((i) => i.includes("faster than 1000 ms")));
      return true;
    },
  );
});

test("the builder refuses a trade without a preceding prediction", () => {
  const b = strategy({ strategyId: IDS.strategy, accountId: IDS.account, userId: IDS.user })
    .instrument("sol_usdc", IDS.instrument)
    .onInterval("tick", 5000)
    .dependency({
      name: "price",
      kind: "PRICE",
      tool_code: "price_spot",
      tool_version: 1,
      dependency_version: 1,
      params: {},
      max_age_ms: 500,
      required: true,
    })
    .intent("buy", {
      action: "ACQUIRE_NOTIONAL",
      instrument: "sol_usdc",
      sizing: { kind: "FIXED_NOTIONAL", notional_usd: usd(5000) },
      constraints: {
        max_slippage_bps: 50,
        max_fee_bps: 30,
        max_price_impact_bps: 50,
        quote_freshness_ms: 500,
        allowed_venues: ["JUPITER"],
      },
      deadline_ms: 30_000,
      prediction: "nonexistent",
    })
    .envelope({ max_runs_per_minute: 12, max_intents_per_hour: 6 });

  assert.throws(
    () => b.compile(),
    (err: unknown) => {
      assert.ok(err instanceof StrategyBuildError);
      assert.ok(err.issues.some((i) => i.includes("COMMIT_PREDICTION")));
      return true;
    },
  );
});

test("the builder refuses a forward signal reference", () => {
  const b = strategy({ strategyId: IDS.strategy, accountId: IDS.account, userId: IDS.user })
    .instrument("sol_usdc", IDS.instrument)
    .onInterval("tick", 5000)
    .dependency({
      name: "price",
      kind: "PRICE",
      tool_code: "price_spot",
      tool_version: 1,
      dependency_version: 1,
      params: {},
      max_age_ms: 500,
      required: true,
    })
    .signal("first", signalRef("second"), 4)
    .signal("second", { const: decimal("1.0000") }, 4)
    .predict("p", {
      instrument: "sol_usdc",
      horizon_ms: 1000,
      direction: "UP",
      probability: { const: decimal("0.5000") },
      expected_return_bps: { const: decimal("10") },
      downside_probability: { const: decimal("0.5000") },
      max_downside_bps: { const: decimal("10") },
      confidence: { const: decimal("0.5000") },
    })
    .envelope({ max_runs_per_minute: 12 });

  assert.throws(
    () => b.compile(),
    (err: unknown) => {
      assert.ok(err instanceof StrategyBuildError);
      assert.ok(err.issues.some((i) => i.includes("not an earlier signal")));
      return true;
    },
  );
});

test("decimal helpers mirror the Go implementation", () => {
  assert.deepEqual(decimal("0.0200"), { m: "200", s: 4 });
  assert.deepEqual(decimal("150"), { m: "150", s: 0 });
  assert.deepEqual(decimal("-0.5"), { m: "-5", s: 1 });
  assert.deepEqual(decimal("0.000"), { m: "0", s: 3 });
  assert.deepEqual(decimal("00123.4"), { m: "1234", s: 1 });

  assert.equal(decimalToString({ m: "200", s: 4 }), "0.0200");
  assert.equal(decimalToString({ m: "-5", s: 1 }), "-0.5");
  assert.equal(decimalToString({ m: "0", s: 2 }), "0.00");
  assert.equal(decimalToString({ m: "12345", s: 3 }), "12.345");

  // The same spellings Go rejects.
  for (const bad of ["", ".", "1.", ".5", "1e5", "NaN", "abc", "1.2.3"]) {
    assert.throws(() => decimal(bad), /is not a decimal literal/, `${bad} must not parse`);
  }
});

test("usd renders whole minor units as two decimals", () => {
  assert.equal(usd(0), "0.00");
  assert.equal(usd(5000), "50.00");
  assert.equal(usd(1), "0.01");
  assert.equal(usd(-123), "-1.23");
  assert.equal(usd(100000), "1000.00");
  assert.throws(() => usd(1.5), /whole minor units/, "USD is expressed in whole cents");
});

test("compile is deterministic", () => {
  const first = buildMomentum().compile();
  for (let i = 0; i < 20; i++) {
    const again = buildMomentum().compile();
    assert.equal(again.hash, first.hash);
    assert.equal(again.semanticJson, first.semanticJson);
  }
});

test("venue and effect ordering does not affect the hash", () => {
  const a = buildMomentum().envelope({ venues: ["JUPITER", "ORCA"] }).compile();
  const b = buildMomentum().envelope({ venues: ["ORCA", "JUPITER", "ORCA"] }).compile();
  assert.equal(a.hash, b.hash, "allowlists are sets, not sequences");
});
