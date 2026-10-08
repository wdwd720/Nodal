/**
 * The derivatives section's pure helpers and its runtime contract
 * (src/api/advanced/derivatives.ts).
 *
 * Each spec is shown a realistic answer, which it must accept unchanged, and
 * then that answer broken one way at a time, which it must refuse with a
 * ContractViolation naming the field — including the rule no per-field spec
 * can state: an answer that says it valued something carries the valuation,
 * one that says the backend refused carries the refusal, and never neither.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  checkContract,
  checkPosition,
  checkPositionPage,
  checkSettlementCommand,
  checkTradeResult,
  derivativeAnalyticsSpec,
  derivativeClearingReportSpec,
  derivativeFlowPageSpec,
  derivativeRunResultSpec,
  derivativesKeys,
} from "../api/advanced/derivatives.ts";
import { advancedKeys } from "../api/advanced/core.ts";
import { ContractViolation, validated } from "../api/contract.ts";
import {
  buildContractDefinition,
  exerciseAvailability,
  formatRateE8,
  instantOf,
  latestEndedIntervalStart,
  mantissaDecimal,
  parseBpsInput,
  parseContractsInput,
  parseDecimalInput,
  parseInstantInput,
  parseReasonInput,
  parseSha256Input,
  priceMantissa,
  refusedIntentId,
  type ContractDefinitionInput,
} from "./derivatives.ts";

const ACCOUNT = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const INSTRUMENT = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const POSITION = "01a075f1-f846-719e-bcbb-493c8281c5e1";
const MARK = "01a075f1-f846-719e-bcbb-493c8281c5e2";
const INTENT = "01a075f1-f846-719e-bcbb-493c8281c524";
const SANDBOX_DOLLAR = "01920000-0000-7000-8000-5342582d5553";
const NOW = "2026-10-07T16:00:00Z";

function broken<T>(value: T, change: (copy: Record<string, unknown>) => void): unknown {
  const copy = structuredClone(value) as Record<string, unknown>;
  change(copy);
  return copy;
}

function refuses(check: () => unknown, path: RegExp): void {
  assert.throws(check, (error: unknown) => {
    assert.ok(error instanceof ContractViolation, `expected a ContractViolation, got ${String(error)}`);
    assert.match(error.path, path);
    return true;
  });
}

const asset = { id: SANDBOX_DOLLAR, symbol: "SBX-USD", decimals: 6 };

const mark = {
  value_domain: "SIMULATED",
  id: MARK,
  instrument_id: INSTRUMENT,
  mark_mantissa: "6600000",
  price_scale: 2,
  has_index: true,
  index_mantissa: "6593400",
  index_scale: 2,
  index_source: "itest-operator",
  source: "itest-operator",
  observed_at: NOW,
  recorded_at: NOW,
};

const contract = {
  value_domain: "SIMULATED",
  id: INSTRUMENT,
  kind: "PERPETUAL",
  canonical_name: "DVBTC-PERP",
  status: "ACTIVE",
  risk_class: "SPECULATIVE",
  terms_version: 1,
  terms_hash: "9f2c4be1",
  terms: {
    kind: "PERPETUAL",
    symbol: "DVBTC-PERP",
    venue_code: "NODAL-SANDBOX",
    settlement_asset_id: SANDBOX_DOLLAR,
    contract_multiplier: "1000000",
    price_scale: 2,
    tick_size: "50",
    lot_size: "1",
    min_quantity: "1",
    quantity_decimals: 0,
    settlement_style: "PHYSICAL",
    trading_calendar: "24x7",
    jurisdiction_tags: [],
    oracle_max_age_ms: 60000,
    oracle_sources: ["itest-operator"],
    margin_model: "LINEAR",
    corporate_action_behavior: "NONE",
    transferability: "NON_TRANSFERABLE",
    funding_interval_ms: 28800000,
  },
  settlement_asset: asset,
  marked: true,
  latest_mark: mark,
  settled: false,
};

const valuation = {
  mark_id: MARK,
  mark_mantissa: "6600000",
  mark_observed_at: NOW,
  average_entry_price_mantissa: "6506500",
  unrealized_pnl: "93500000",
  equity_adjustment: "0",
  requirement: {
    notional: "6600000000",
    initial: "808500000",
    maintenance: "330000000",
    scenario_loss: "0",
    funding_buffer: "148500000",
  },
};

const position = {
  id: POSITION,
  account_id: ACCOUNT,
  instrument_id: INSTRUMENT,
  symbol: "DVBTC-PERP",
  kind: "PERPETUAL",
  status: "OPEN",
  side: "LONG",
  quantity: "10",
  price_scale: 2,
  settlement_asset: asset,
  cost_value: "6506500000",
  settled_value: "6600000000",
  realized_pnl: "0",
  funding_pnl: "-4404370",
  margin_posted: "796250000",
  shortfall: "0",
  version: 4,
  opened_at: NOW,
  valuation_status: "VALUED",
  valuation,
};

const page = {
  value_domain: "SIMULATED",
  next_cursor: null,
  items: [position],
  book_status: "VALUED",
  book: {
    as_of: NOW,
    policy_version: "derivatives-sandbox-v1",
    policy_hash: "ph",
    quote_asset: asset,
    gross_notional: "6600.00",
    initial_margin: "808.50",
    maintenance_margin: "330.00",
    unrealized_pnl: "93.50",
    equity_adjustment: "0.00",
    max_initial_bps: 1000,
    positions: 1,
  },
};

/* --------------------------------------------------------------------------
 * Pure helpers
 * ------------------------------------------------------------------------ */

test("a funding rate in 10^-8 renders as the percentage it is, exactly", () => {
  assert.equal(formatRateE8(66_733), "0.066733%");
  assert.equal(formatRateE8(750_000), "0.75%");
  assert.equal(formatRateE8(-20_000), "-0.02%");
  assert.equal(formatRateE8(1), "0.000001%");
  assert.equal(formatRateE8(0), "0%");
  assert.equal(formatRateE8(100_000_000), "100%");
});

test("a price is taken exactly as written, or refused", () => {
  assert.deepEqual(parseDecimalInput(" 65,000.50 "), { ok: true, value: "65000.50", error: "" });
  assert.deepEqual(parseDecimalInput("66000"), { ok: true, value: "66000", error: "" });
  for (const bad of ["", "1e5", "-1", "1.", ".5", "6.5.0", `1.${"0".repeat(39)}`]) {
    assert.equal(parseDecimalInput(bad).ok, false, bad);
  }
});

test("contracts are a positive whole number", () => {
  assert.deepEqual(parseContractsInput("010"), { ok: true, value: "10", error: "" });
  for (const bad of ["", "0", "000", "1.5", "-3", "ten"]) {
    assert.equal(parseContractsInput(bad).ok, false, bad);
  }
});

test("an instant is UTC with a Z, and a real date", () => {
  assert.deepEqual(parseInstantInput("2026-10-07T08:00:00Z"), { ok: true, value: "2026-10-07T08:00:00.000Z", error: "" });
  assert.equal(parseInstantInput("2026-10-07T08:00Z").ok, true);
  for (const bad of ["", "2026-10-07 08:00:00", "2026-10-07T08:00:00+02:00", "2026-13-45T99:00:00Z"]) {
    assert.equal(parseInstantInput(bad).ok, false, bad);
  }
});

test("basis points are whole and within the model's bounds", () => {
  assert.deepEqual(parseBpsInput("8000", 1, 100_000), { ok: true, value: 8000, error: "" });
  assert.deepEqual(parseBpsInput("-25", -10_000, 10_000), { ok: true, value: -25, error: "" });
  assert.equal(parseBpsInput("0", 1, 100_000).ok, false, "a volatility must be positive");
  assert.equal(parseBpsInput("100001", 1, 100_000).ok, false);
  assert.equal(parseBpsInput("1.5", 1, 100_000).ok, false);
  assert.equal(parseBpsInput("1234567", 1, 100_000).ok, false, "at most six digits");
});

test("the latest ended interval sits on the grid", () => {
  const hour = 3_600_000;
  const at = (iso: string): number => new Date(iso).getTime();
  assert.equal(latestEndedIntervalStart(at("2026-10-07T09:00:30Z"), hour), at("2026-10-07T08:00:00Z"));
  assert.equal(latestEndedIntervalStart(at("2026-10-07T09:00:00Z"), hour), at("2026-10-07T08:00:00Z"));
  assert.equal(latestEndedIntervalStart(at("2026-10-07T16:00:00Z"), 8 * hour), at("2026-10-07T08:00:00Z"));
  assert.equal(latestEndedIntervalStart(10, hour), undefined);
  assert.equal(latestEndedIntervalStart(at(NOW), 0), undefined);
  assert.equal(instantOf(at("2026-10-07T08:00:00Z")), "2026-10-07T08:00:00Z");
});

test("exercise is offered only to the holder of an open American option", () => {
  const long = { kind: "OPTION", status: "OPEN", side: "LONG" };
  assert.equal(exerciseAvailability(long, "AMERICAN").available, true);
  assert.equal(exerciseAvailability(long, "EUROPEAN").available, false);
  assert.match(exerciseAvailability(long, "EUROPEAN").reason, /automatically at expiry/);
  assert.equal(exerciseAvailability({ ...long, side: "SHORT" }, "AMERICAN").available, false);
  assert.equal(exerciseAvailability({ ...long, status: "CLOSED" }, "AMERICAN").available, false);
  assert.equal(exerciseAvailability({ kind: "PERPETUAL", status: "OPEN", side: "LONG" }, undefined).available, false);
});

test("a recorded refusal's intent is found only when it is named", () => {
  assert.equal(refusedIntentId({ intent_id: INTENT, reasons: ["RISK_ORACLE_STALE"] }), INTENT);
  assert.equal(refusedIntentId(undefined), undefined);
  assert.equal(refusedIntentId({ intent_id: 7 }), undefined);
  assert.equal(refusedIntentId({ intent_id: "" }), undefined);
});

test("account-scoped derivatives keys sit under the account's prefix", () => {
  const prefix = advancedKeys.account(ACCOUNT);
  for (const key of [
    derivativesKeys.positions(ACCOUNT, "", ""),
    derivativesKeys.position(ACCOUNT, POSITION),
    derivativesKeys.flows(ACCOUNT, "", "", ""),
    derivativesKeys.accruals(ACCOUNT, "", ""),
  ]) {
    assert.deepEqual(key.slice(0, prefix.length), [...prefix]);
  }
  assert.deepEqual(derivativesKeys.contract(INSTRUMENT).slice(0, advancedKeys.all.length), [...advancedKeys.all]);
});

/* --------------------------------------------------------------------------
 * Specs
 * ------------------------------------------------------------------------ */

test("a contract is accepted as sent, and must carry what it says it has", () => {
  assert.doesNotThrow(() => checkContract(contract, "/c"));
  refuses(() => checkContract(broken(contract, (c) => delete c["latest_mark"]), "/c"), /^\/c\.latest_mark$/);
  refuses(() => checkContract(broken(contract, (c) => (c["settled"] = true)), "/c"), /^\/c\.settlement$/);
  refuses(() => checkContract(broken(contract, (c) => (c["kind"] = "SPOT_PAIR")), "/c"), /^\/c\.kind$/);
  refuses(() => checkContract(broken(contract, (c) => (c["value_domain"] = "LIVE")), "/c"), /^\/c\.value_domain$/);
  refuses(
    () => checkContract(broken(contract, (c) => ((c["latest_mark"] as Record<string, unknown>)["mark_mantissa"] = "66000.00")), "/c"),
    /^\/c\.latest_mark\.mark_mantissa$/,
  );
});

test("a position carries exactly what its valuation status says", () => {
  assert.doesNotThrow(() => checkPosition(position, "/p"));
  refuses(() => checkPosition(broken(position, (p) => delete p["valuation"]), "/p"), /^\/p\.valuation_status$/);
  const refused = broken(position, (p) => {
    delete p["valuation"];
    p["valuation_status"] = "REFUSED";
    p["valuation_refusal"] = { code: "STALE_MARKET_DATA", detail: "a position's mark is stale" };
  });
  assert.doesNotThrow(() => checkPosition(refused, "/p"));
  refuses(() => checkPosition(broken(refused, (p) => delete p["valuation_refusal"]), "/p"), /^\/p\.valuation_status$/);
  refuses(() => checkPosition(broken(position, (p) => (p["valuation_status"] = "NOT_APPLICABLE")), "/p"), /^\/p\.valuation_status$/);
  refuses(() => checkPosition(broken(position, (p) => (p["quantity"] = 10)), "/p"), /^\/p\.quantity$/);
  refuses(
    () => checkPosition(broken(position, (p) => ((p["valuation"] as Record<string, unknown>)["unrealized_pnl"] = "93.5")), "/p"),
    /^\/p\.valuation\.unrealized_pnl$/,
  );
});

test("a page's book is valued or refused, and its figures are exact", () => {
  assert.doesNotThrow(() => checkPositionPage(page, "/page"));
  refuses(() => checkPositionPage(broken(page, (p) => delete p["book"]), "/page"), /^\/page\.book_status$/);
  refuses(() => checkPositionPage(broken(page, (p) => delete p["next_cursor"]), "/page"), /^\/page\.next_cursor$/);
  refuses(
    () => checkPositionPage(broken(page, (p) => ((p["book"] as Record<string, unknown>)["initial_margin"] = "808.5")), "/page"),
    /^\/page\.book\.initial_margin$/,
  );
  refuses(
    () => checkPositionPage(broken(page, (p) => ((p["items"] as Array<Record<string, unknown>>)[0] = { ...position, valuation_status: "REFUSED" })), "/page"),
    /^\/page\.items\[0\]\.valuation_status$/,
  );
});

test("a trade result's position is held to the same rule", () => {
  const result = {
    value_domain: "SIMULATED",
    replay: false,
    intent: {
      id: INTENT,
      account_id: ACCOUNT,
      kind: "OPEN_POSITION",
      mode: "PAPER",
      capability: "DERIVATIVES",
      actor_type: "USER",
      terms: { side: "LONG" },
      content_hash: "0c0d",
      status: "COMPLETED",
      created_at: NOW,
      updated_at: NOW,
    },
    position: { ...position, valuation_status: "NOT_COMPUTED", valuation: undefined },
    journal_transaction_ids: [MARK],
  };
  assert.doesNotThrow(() => checkTradeResult(result, "/t"));
  refuses(() => checkTradeResult(broken(result, (r) => ((r["intent"] as Record<string, unknown>)["mode"] = "LIVE")), "/t"), /^\/t\.intent\.mode$/);
  refuses(
    () => checkTradeResult(broken(result, (r) => (r["position"] = { ...position, valuation_status: "NOT_COMPUTED" })), "/t"),
    /^\/t\.position\.valuation_status$/,
  );
});

test("flows, runs, analytics and reconciliation are exact strings in the simulated domain", () => {
  const flows = {
    value_domain: "SIMULATED",
    next_cursor: "c-2",
    items: [
      {
        id: MARK,
        position_id: POSITION,
        account_id: ACCOUNT,
        instrument_id: INSTRUMENT,
        symbol: "DVBTC-PERP",
        kind: "FUNDING",
        amount: "-4404370",
        shortfall: "0",
        settlement_asset: asset,
        journal_transaction_id: INTENT,
        reference_type: "funding_rate",
        reference_id: INTENT,
        occurred_at: NOW,
      },
    ],
  };
  assert.doesNotThrow(() => validated(flows, derivativeFlowPageSpec, "/f"));
  refuses(() => validated(broken(flows, (f) => (f["value_domain"] = "REAL")), derivativeFlowPageSpec, "/f"), /^\/f\.value_domain$/);
  refuses(
    () => validated(broken(flows, (f) => ((f["items"] as Array<Record<string, unknown>>)[0]!["kind"] = "DIVIDEND")), derivativeFlowPageSpec, "/f"),
    /^\/f\.items\[0\]\.kind$/,
  );

  const run = {
    value_domain: "SIMULATED",
    kind: "FUNDING",
    run_id: INTENT,
    instrument_id: INSTRUMENT,
    replay: true,
    interval_start: NOW,
    interval_end: NOW,
    rate_e8: 66733,
    premium_index_e8: 66733,
    clamped: false,
    per_contract_amount: "440437",
    settlement_asset: asset,
    journal_transaction_ids: [],
  };
  assert.doesNotThrow(() => validated(run, derivativeRunResultSpec, "/r"), "a replay without totals is a complete answer");
  refuses(() => validated(broken(run, (r) => (r["paid"] = 4404370)), derivativeRunResultSpec, "/r"), /^\/r\.paid$/);

  const analytics = {
    value_domain: "SIMULATED",
    label: "REFERENCE_MODEL_VALUE: not a quote, not a settlement price, never posted",
    model: "BLACK_SCHOLES_MERTON",
    instrument_id: INSTRUMENT,
    right: "CALL",
    style: "AMERICAN",
    regime: "NORMAL",
    spot: "65934",
    strike: "65000",
    time_years: "0.082191780821917808",
    vol: "0.8",
    price: "6123.456789012345678901",
    delta: "0.55",
    gamma: "0.00002",
    theta_per_year: "-36210.5",
    theta_per_day: "-99.2",
    vega: "7400.1",
    vega_per_vol_point: "74.001",
    intrinsic: "934",
    implied_vol_reason: "PRICE_ABOVE_UPPER_BOUND",
    as_of: NOW,
    inputs: { vol_bps: 8000, rate_bps: 0, dividend_yield_bps: 0 },
    mark,
  };
  assert.doesNotThrow(() => validated(analytics, derivativeAnalyticsSpec, "/a"));
  refuses(() => validated(broken(analytics, (a) => (a["price"] = 6123.45)), derivativeAnalyticsSpec, "/a"), /^\/a\.price$/);
  refuses(() => validated(broken(analytics, (a) => delete a["inputs"]), derivativeAnalyticsSpec, "/a"), /^\/a\.inputs$/);

  const report = {
    value_domain: "SIMULATED",
    clearing_house_id: INTENT,
    asset,
    holdings: "1000019600000",
    capital: "1000000000000",
    result: "19600000",
    register_net: "-19600000",
    register_shortfall: "0",
    flows: 5,
    expected_result: "19600000",
    difference: "0",
    net_open_interest: [{ instrument_id: INSTRUMENT, quantity: "-10" }],
    internally_matched: false,
    matched: true,
  };
  assert.doesNotThrow(() => validated(report, derivativeClearingReportSpec, "/x"));
  refuses(() => validated(broken(report, (r) => (r["difference"] = "0.0")), derivativeClearingReportSpec, "/x"), /^\/x\.difference$/);
});

/* --------------------------------------------------------------------------
 * The settlement rule and its dual-controlled override
 * ------------------------------------------------------------------------ */

test("a confirmed price is the preview's mantissa with its point moved, never computed", () => {
  assert.equal(mantissaDecimal("6600000", 2), "66000.00");
  assert.equal(mantissaDecimal("5", 2), "0.05");
  assert.equal(mantissaDecimal("6600000", 0), "6600000");
  assert.equal(mantissaDecimal("-1", 2), undefined);
  assert.equal(mantissaDecimal("66000.00", 2), undefined);
  assert.equal(mantissaDecimal("1", -1), undefined);
});

test("an override's evidence digest and reason are taken as the API takes them", () => {
  const digest = "AB".repeat(32);
  assert.deepEqual(parseSha256Input(` ${digest} `), { ok: true, value: "ab".repeat(32), error: "" });
  assert.equal(parseSha256Input("ab").ok, false);
  assert.equal(parseSha256Input("zz".repeat(32)).ok, false);
  assert.equal(parseSha256Input("").ok, false);
  assert.deepEqual(parseReasonInput("  stalled feed  ", 1000), { ok: true, value: "stalled feed", error: "" });
  assert.equal(parseReasonInput("   ", 1000).ok, false);
  assert.equal(parseReasonInput("x".repeat(11), 10).ok, false);
});

const override = {
  value_domain: "SIMULATED",
  id: INTENT,
  instrument_id: INSTRUMENT,
  status: "PROPOSED",
  price_mantissa: "6700000",
  price_scale: 2,
  reference_mark_id: MARK,
  reference_mantissa: "6600000",
  max_deviation_bps: 1000,
  deviation_bps: 152,
  reason: "the feed stalled before expiry",
  evidence_ref: "s3://sandbox-evidence/bulletin.pdf",
  evidence_sha256: "ab".repeat(32),
  proposed_by: { actor_type: "OPERATOR", actor_id: "op-alice" },
  proposed_at: NOW,
};

const settlementResult = {
  value_domain: "SIMULATED",
  settlement_id: POSITION,
  instrument_id: INSTRUMENT,
  kind: "FUTURE",
  replay: false,
  price_mantissa: "6700000",
  price_scale: 2,
  price_rule: "OPERATOR_OVERRIDE",
  price_source: `override:${INTENT}`,
  mark_id: MARK,
  override_id: INTENT,
  positions: 2,
  exercise_ids: [],
  settlement_asset: asset,
  journal_transaction_ids: [],
};

test("a settlement command answers with exactly what its method produced", () => {
  const rule = { value_domain: "SIMULATED", method: "RULE", settlement: { ...settlementResult, price_rule: "EXPIRY_MARK" } };
  assert.doesNotThrow(() => checkSettlementCommand(rule, "/s"));
  refuses(() => checkSettlementCommand(broken(rule, (r) => delete r["settlement"]), "/s"), /^\/s\.settlement$/);
  refuses(() => checkSettlementCommand(broken(rule, (r) => (r["override"] = override)), "/s"), /^\/s\.override$/);

  const proposed = { value_domain: "SIMULATED", method: "PROPOSE_OVERRIDE", override };
  assert.doesNotThrow(() => checkSettlementCommand(proposed, "/s"));
  refuses(() => checkSettlementCommand(broken(proposed, (r) => (r["settlement"] = settlementResult)), "/s"), /^\/s\.settlement$/);
  refuses(
    () => checkSettlementCommand(broken(proposed, (r) => ((r["override"] as Record<string, unknown>)["status"] = "MAYBE")), "/s"),
    /^\/s\.override\.status$/,
  );

  const approved = {
    value_domain: "SIMULATED",
    method: "APPROVE_OVERRIDE",
    settlement: settlementResult,
    override: {
      ...override,
      status: "APPROVED",
      settlement_id: POSITION,
      decision: { decision: "APPROVED", reason: "checked", decided_by: { actor_type: "OPERATOR", actor_id: "op-bob" }, decided_at: NOW },
    },
  };
  assert.doesNotThrow(() => checkSettlementCommand(approved, "/s"));
  refuses(() => checkSettlementCommand(broken(approved, (r) => delete r["override"]), "/s"), /^\/s\.override$/);
  refuses(
    () => checkSettlementCommand(broken(approved, (r) => ((r["settlement"] as Record<string, unknown>)["price_rule"] = "CALLER")), "/s"),
    /^\/s\.settlement\.price_rule$/,
  );
  refuses(() => checkSettlementCommand(broken(approved, (r) => (r["method"] = "CHOOSE")), "/s"), /^\/s\.method$/);
});

test("a dated contract's preview carries the reference it says it has", () => {
  const dated = {
    ...contract,
    kind: "FUTURE",
    settlement_preview: {
      expiry: NOW,
      expired: true,
      settled: false,
      price_scale: 2,
      has_reference: true,
      reference_mark: mark,
      reference_price_mantissa: "6600000",
      price_source: `mark:${MARK}`,
      settleable: false,
      override_min_price_mantissa: "5940000",
      override_max_price_mantissa: "7260000",
      max_deviation_bps: 1000,
      rule_statement: "EXPIRY_MARK v1: ...",
    },
    settlement_overrides: [override],
  };
  assert.doesNotThrow(() => checkContract(dated, "/c"));
  refuses(
    () => checkContract(broken(dated, (c) => delete (c["settlement_preview"] as Record<string, unknown>)["reference_price_mantissa"]), "/c"),
    /^\/c\.settlement_preview\.reference_price_mantissa$/,
  );
  refuses(
    () => checkContract(broken(dated, (c) => (((c["settlement_overrides"] as unknown[])[0] as Record<string, unknown>)["price_mantissa"] = "67000.00")), "/c"),
    /^\/c\.settlement_overrides\[0\]\.price_mantissa$/,
  );
});

/* --------------------------------------------------------------------------
 * Defining a contract
 * ------------------------------------------------------------------------ */

const PERPETUAL_INPUT: ContractDefinitionInput = {
  kind: "PERPETUAL",
  symbol: " sbxsol-perp ",
  name: "",
  underlyingId: INSTRUMENT.toUpperCase(),
  settlementId: "",
  multiplier: "1,000,000,000",
  priceScale: "2",
  tick: "0.01",
  lot: "1",
  minimum: "001",
  expiry: "",
  fundingHours: "8",
  strike: "",
  right: "CALL",
  style: "EUROPEAN",
  oracleSeconds: "600",
  sources: " e2e-operator , , sandbox ",
  risk: "SPECULATIVE",
};

test("a price is moved to its mantissa at the price scale, never rounded", () => {
  assert.equal(priceMantissa("0.50", 2), "50");
  assert.equal(priceMantissa("65000", 2), "6500000");
  assert.equal(priceMantissa("65,000.5", 2), "6500050");
  assert.equal(priceMantissa("0.005", 3), "5");
  assert.equal(priceMantissa("7", 0), "7");
  assert.equal(priceMantissa("0.505", 2), undefined, "more precise than the scale is refused, not rounded");
  assert.equal(priceMantissa("0", 2), undefined, "a price above zero");
  assert.equal(priceMantissa("0.00", 2), undefined);
  assert.equal(priceMantissa("1e3", 2), undefined);
  assert.equal(priceMantissa("-1", 2), undefined);
  assert.equal(priceMantissa("1", 19), undefined);
});

test("a perpetual's definition is exactly what the contract takes", () => {
  const built = buildContractDefinition(PERPETUAL_INPUT);
  assert.equal(built.ok, true);
  if (!built.ok) return;
  assert.deepEqual(built.body, {
    kind: "PERPETUAL",
    symbol: "SBXSOL-PERP",
    underlying_instrument_id: INSTRUMENT,
    contract_multiplier: "1000000000",
    price_scale: 2,
    tick_size: "1",
    lot_size: "1",
    min_quantity: "1",
    oracle_max_age_ms: 600_000,
    oracle_sources: ["e2e-operator", "sandbox"],
    risk_class: "SPECULATIVE",
    funding_interval_ms: 28_800_000,
  });
});

test("a future carries its expiry, an option its expiry, strike, right and style, and neither a funding interval", () => {
  const future = buildContractDefinition({ ...PERPETUAL_INPUT, kind: "FUTURE", expiry: "2026-12-31T16:00:00Z" });
  assert.equal(future.ok, true);
  if (future.ok) {
    assert.equal(future.body.expiry, "2026-12-31T16:00:00.000Z");
    assert.equal(future.body.funding_interval_ms, undefined);
    assert.equal(future.body.strike_mantissa, undefined);
  }
  const option = buildContractDefinition({
    ...PERPETUAL_INPUT,
    kind: "OPTION",
    expiry: "2026-12-31T16:00:00Z",
    strike: "150.5",
    right: "PUT",
    style: "AMERICAN",
    settlementId: SANDBOX_DOLLAR,
    name: "SOL put 150.50",
  });
  assert.equal(option.ok, true);
  if (option.ok) {
    assert.equal(option.body.strike_mantissa, "15050");
    assert.equal(option.body.option_right, "PUT");
    assert.equal(option.body.exercise_style, "AMERICAN");
    assert.equal(option.body.settlement_asset_id, SANDBOX_DOLLAR);
    assert.equal(option.body.canonical_name, "SOL put 150.50");
    assert.equal(option.body.funding_interval_ms, undefined);
  }
});

test("what a definition cannot be is said before anything is sent", () => {
  const problem = (change: Partial<ContractDefinitionInput>): string => {
    const built = buildContractDefinition({ ...PERPETUAL_INPUT, ...change });
    assert.equal(built.ok, false, JSON.stringify(change));
    return built.ok ? "" : built.error;
  };
  assert.match(problem({ symbol: "two words" }), /symbol/);
  assert.match(problem({ underlyingId: "" }), /underlying/);
  assert.match(problem({ settlementId: "dollar" }), /settlement asset/);
  assert.match(problem({ multiplier: "0" }), /contract size/);
  assert.match(problem({ multiplier: "0.5" }), /contract size/);
  assert.match(problem({ priceScale: "19" }), /price scale/);
  assert.match(problem({ tick: "0.001" }), /tick/);
  assert.match(problem({ lot: "" }), /lot size/);
  assert.match(problem({ minimum: "0" }), /minimum/);
  assert.match(problem({ oracleSeconds: "0" }), /maximum age/);
  assert.match(problem({ sources: " , " }), /oracle source/);
  assert.match(problem({ fundingHours: "0" }), /funding interval/);
  assert.match(problem({ kind: "FUTURE", expiry: "tomorrow" }), /Expiry/);
  assert.match(problem({ kind: "OPTION", expiry: "2026-12-31T16:00:00Z", strike: "" }), /strike/);
});
