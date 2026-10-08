/**
 * The advanced core's runtime contract (src/api/advanced/core.ts), and the
 * three facilities it added to src/api/contract.ts: closed enums (`oneOf`),
 * nullable-but-present fields, and nested objects and scalar lists.
 *
 * Each spec is shown a realistic answer, which it must accept unchanged, and
 * then that answer broken one way at a time, which it must refuse with a
 * ContractViolation naming the field. A refused answer becomes an explicit
 * error state on the page; an accepted malformed one would become a figure.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { ApiProblem } from "@controlplane/generated-client";

import {
  advancedInstrumentDetailSpec,
  advancedInstrumentListSpec,
  advancedKeys,
  buyingPowerV2Spec,
  checkCapabilityList,
  economicEffectSpec,
  economicIntentSpec,
  problemCode,
  retryTransient,
  riskDecisionListSpec,
} from "../api/advanced/core.ts";
import { ContractViolation, cursorPageSpec, oneOf, validated, type Spec } from "../api/contract.ts";
import { BUYING_POWER_V2_FIELDS } from "./advanced.ts";

const ACCOUNT = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const INTENT = "01a075f1-f846-719e-bcbb-493c8281c524";
const INSTRUMENT = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const SANDBOX_DOLLAR = "01920000-0000-7000-8000-5342582d5553";
const HASH = "9f2c4be1d0a7c3e85b16f4a2d9e07c31a5b8f6e2c4d1a9b7e3f05c8d2a6b4e19";

/** A deep copy with one change applied, so every case starts from the valid fixture. */
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

/* --------------------------------------------------------------------------
 * contract.ts facilities
 * ------------------------------------------------------------------------ */

test("oneOf accepts exactly the listed strings", () => {
  const spec: Spec = { required: { status: oneOf(["PENDING", "APPLIED"]) } };
  assert.doesNotThrow(() => validated({ status: "APPLIED" }, spec, "/x"));
  refuses(() => validated({ status: "applied" }, spec, "/x"), /^\/x\.status$/);
  refuses(() => validated({ status: "DONE" }, spec, "/x"), /^\/x\.status$/);
  refuses(() => validated({ status: 1 }, spec, "/x"), /^\/x\.status$/);
});

test("a nullable field may be null but may not be absent", () => {
  const spec: Spec = { nullable: { next_cursor: "string" } };
  assert.doesNotThrow(() => validated({ next_cursor: null }, spec, "/p"));
  assert.doesNotThrow(() => validated({ next_cursor: "abc" }, spec, "/p"));
  refuses(() => validated({}, spec, "/p"), /^\/p\.next_cursor$/);
  refuses(() => validated({ next_cursor: 7 }, spec, "/p"), /^\/p\.next_cursor$/);
});

test("nested objects and scalar lists are checked element by element", () => {
  const spec: Spec = {
    objects: { terms: { required: true, spec: { required: { size: "quantity" } } } },
    lists: { modes: { required: true, kind: oneOf(["PAPER", "LIVE"]) }, tags: { kind: "string" } },
  };
  assert.doesNotThrow(() => validated({ terms: { size: "5" }, modes: ["PAPER"] }, spec, "/o"));
  refuses(() => validated({ modes: [] }, spec, "/o"), /^\/o\.terms$/);
  refuses(() => validated({ terms: { size: "5.0" }, modes: [] }, spec, "/o"), /^\/o\.terms\.size$/);
  refuses(() => validated({ terms: { size: "5" } }, spec, "/o"), /^\/o\.modes$/);
  refuses(() => validated({ terms: { size: "5" }, modes: "PAPER" }, spec, "/o"), /^\/o\.modes$/);
  refuses(() => validated({ terms: { size: "5" }, modes: ["PAPER", "TURBO"] }, spec, "/o"), /^\/o\.modes\[1\]$/);
  refuses(() => validated({ terms: { size: "5" }, modes: [], tags: [3] }, spec, "/o"), /^\/o\.tags\[0\]$/);
});

/* --------------------------------------------------------------------------
 * GET /advanced/capabilities
 * ------------------------------------------------------------------------ */

const capabilities = {
  environment: "LOCAL",
  value_domain: "SIMULATED",
  items: [
    {
      capability: "MARGIN",
      high_risk: true,
      simulated_modes: ["PAPER", "SANDBOX"],
      ungated_environments: ["LOCAL", "TEST", "DEV"],
      live_implemented: false,
      live_evidence: ["LEGAL_REVIEW", "PROVIDER_CONTRACT", "RISK_APPROVAL", "SECURITY_APPROVAL"],
      min_approvers: 2,
      max_live_validity_seconds: 2_592_000,
      max_live_capital: "0.00",
      jurisdictions: [],
      provider_readiness: "a licensed margin lender or broker-dealer carrying the credit",
      gate_active: false,
      kill_switched: false,
      decisions: [
        { mode: "PAPER", allowed: true, reasons: [] },
        { mode: "SANDBOX", allowed: true, reasons: [] },
        { mode: "DEVNET", allowed: false, reasons: ["MODE_NOT_SUPPORTED"] },
        { mode: "LIVE", allowed: false, reasons: ["CAPABILITY_GATE_NOT_ACTIVE", "LIVE_NOT_IMPLEMENTED"] },
      ],
    },
  ],
};

test("the capability list is accepted as the backend sends it", () => {
  assert.deepEqual(checkCapabilityList(structuredClone(capabilities)), capabilities);
});

test("the capability list is refused when it does not say SIMULATED", () => {
  refuses(() => checkCapabilityList(broken(capabilities, (c) => (c["value_domain"] = "REAL"))), /\.value_domain$/);
  refuses(() => checkCapabilityList(broken(capabilities, (c) => delete c["value_domain"])), /\.value_domain$/);
});

test("a capability is refused for a malformed figure, mode or decision", () => {
  const item = (c: Record<string, unknown>): Record<string, unknown> =>
    (c["items"] as Array<Record<string, unknown>>)[0] as Record<string, unknown>;
  refuses(() => checkCapabilityList(broken(capabilities, (c) => (item(c)["max_live_capital"] = "0"))), /max_live_capital$/);
  refuses(() => checkCapabilityList(broken(capabilities, (c) => (item(c)["max_live_capital"] = "0.000"))), /max_live_capital$/);
  refuses(() => checkCapabilityList(broken(capabilities, (c) => (item(c)["simulated_modes"] = ["PAPER", "TURBO"]))), /simulated_modes\[1\]$/);
  refuses(
    () => checkCapabilityList(broken(capabilities, (c) => ((item(c)["decisions"] as Array<Record<string, unknown>>)[3] as Record<string, unknown>)["mode"] = "REAL")),
    /decisions\[3\]\.mode$/,
  );
  refuses(
    () => checkCapabilityList(broken(capabilities, (c) => delete ((item(c)["decisions"] as Array<Record<string, unknown>>)[0] as Record<string, unknown>)["reasons"])),
    /decisions\[0\]\.reasons$/,
  );
});

test("a capability that reports one mode twice is refused rather than shown either way", () => {
  refuses(
    () =>
      checkCapabilityList(
        broken(capabilities, (c) => {
          const item = (c["items"] as Array<Record<string, unknown>>)[0] as Record<string, unknown>;
          (item["decisions"] as unknown[]).push({ mode: "LIVE", allowed: true, reasons: [] });
        }),
      ),
    /items\[0\]\.decisions$/,
  );
});

/* --------------------------------------------------------------------------
 * GET /advanced/instruments and /advanced/instruments/{id}
 * ------------------------------------------------------------------------ */

const instrument = {
  id: INSTRUMENT,
  kind: "OPTION",
  canonical_name: "SBX-BTC 2026-12-31 70000 CALL",
  status: "ACTIVE",
  risk_class: "SPECULATIVE",
  terms_version: 2,
  terms_hash: HASH,
  terms: {
    kind: "OPTION",
    symbol: "SBX-BTC-C-70000",
    venue_code: "SANDBOX",
    settlement_asset_id: SANDBOX_DOLLAR,
    underlying_instrument_id: INTENT,
    contract_multiplier: "1",
    price_scale: 2,
    tick_size: "1",
    lot_size: "1",
    min_quantity: "1",
    quantity_decimals: 0,
    expiry: "2026-12-31T00:00:00Z",
    strike_mantissa: "7000000",
    option_right: "CALL",
    exercise_style: "EUROPEAN",
    settlement_style: "PHYSICAL",
    trading_calendar: "24x7",
    jurisdiction_tags: [],
    oracle_max_age_ms: 60_000,
    oracle_sources: ["sandbox-oracle"],
    margin_model: "OPTION",
    corporate_action_behavior: "NONE",
    transferability: "TRANSFERABLE",
  },
};

const instrumentDetail = {
  ...instrument,
  versions: [
    { version: 1, terms_hash: HASH, reason: "listed", actor_type: "OPERATOR", created_at: "2026-10-01T12:00:00Z" },
    { version: 2, terms_hash: HASH, reason: "tick size", actor_type: "OPERATOR", created_at: "2026-10-02T12:00:00Z" },
  ],
};

test("instruments are accepted with their terms and history", () => {
  assert.doesNotThrow(() => validated({ items: [instrument] }, advancedInstrumentListSpec, "/advanced/instruments"));
  assert.doesNotThrow(() => validated(instrumentDetail, advancedInstrumentDetailSpec, "/advanced/instruments/{id}"));
});

test("an instrument is refused for an unknown kind, a fractional quantity or missing terms", () => {
  const terms = (c: Record<string, unknown>): Record<string, unknown> => c["terms"] as Record<string, unknown>;
  const check = (value: unknown): unknown => validated(value, advancedInstrumentDetailSpec, "/i");
  refuses(() => check(broken(instrumentDetail, (c) => (c["kind"] = "BOND"))), /^\/i\.kind$/);
  refuses(() => check(broken(instrumentDetail, (c) => (terms(c)["tick_size"] = "0.01"))), /^\/i\.terms\.tick_size$/);
  refuses(() => check(broken(instrumentDetail, (c) => (terms(c)["option_right"] = "STRADDLE"))), /^\/i\.terms\.option_right$/);
  refuses(() => check(broken(instrumentDetail, (c) => (terms(c)["margin_model"] = "PORTFOLIO"))), /^\/i\.terms\.margin_model$/);
  refuses(() => check(broken(instrumentDetail, (c) => delete terms(c)["oracle_sources"])), /^\/i\.terms\.oracle_sources$/);
  refuses(() => check(broken(instrumentDetail, (c) => delete c["terms"])), /^\/i\.terms$/);
  refuses(() => check(broken(instrumentDetail, (c) => delete c["versions"])), /^\/i\.versions$/);
  refuses(() => check(broken(instrumentDetail, (c) => (c["terms_version"] = "2"))), /^\/i\.terms_version$/);
});

/* --------------------------------------------------------------------------
 * GET /advanced/intents, /advanced/effects, /advanced/risk/decisions
 * ------------------------------------------------------------------------ */

const intent = {
  id: INTENT,
  account_id: ACCOUNT,
  kind: "ACQUIRE_EXPOSURE",
  mode: "SANDBOX",
  capability: "MARGIN",
  actor_type: "USER",
  instrument_id: INSTRUMENT,
  notional_usd: "25.00",
  terms: { side: "BUY" },
  content_hash: HASH,
  status: "COMPLETED",
  created_at: "2026-10-07T19:00:00Z",
  updated_at: "2026-10-07T19:00:01Z",
  terminal_at: "2026-10-07T19:00:01Z",
};

const intentPage = { items: [intent], next_cursor: "eyJpZCI6IjEifQ" };

test("an intent page is accepted, with or without a further page", () => {
  const spec = cursorPageSpec(economicIntentSpec);
  assert.doesNotThrow(() => validated(intentPage, spec, "/advanced/intents"));
  assert.doesNotThrow(() => validated({ ...intentPage, next_cursor: null }, spec, "/advanced/intents"));
});

test("an intent page without its cursor is refused rather than read as the last page", () => {
  refuses(() => validated({ items: [intent] }, cursorPageSpec(economicIntentSpec), "/p"), /^\/p\.next_cursor$/);
});

test("an intent that claims to be LIVE is refused: the database cannot store one", () => {
  refuses(() => validated(broken(intent, (c) => (c["mode"] = "LIVE")), economicIntentSpec, "/i"), /^\/i\.mode$/);
});

test("an intent's amount must be an exact decimal or exact base units", () => {
  refuses(() => validated(broken(intent, (c) => (c["notional_usd"] = "25")), economicIntentSpec, "/i"), /notional_usd$/);
  refuses(() => validated(broken(intent, (c) => (c["notional_usd"] = "25.5")), economicIntentSpec, "/i"), /notional_usd$/);
  refuses(() => validated(broken(intent, (c) => (c["quantity"] = "1.5")), economicIntentSpec, "/i"), /quantity$/);
  refuses(() => validated(broken(intent, (c) => delete c["terms"]), economicIntentSpec, "/i"), /terms$/);
});

const effect = {
  id: "01a075f1-f900-7a98-adbc-5fc08c37e83f",
  effect_key: "SANDBOX_FUNDING:01a075f1:1",
  subsystem: "SANDBOX_FUNDING",
  effect_kind: "FUND",
  intent_id: INTENT,
  account_id: ACCOUNT,
  posts_ledger: true,
  status: "APPLIED",
  journal_transaction_id: "01a075f1-f901-7a98-adbc-5fc08c37e83f",
  evidence_ref: "evidence/econ/01a075f1",
  evidence_sha256: HASH,
  created_at: "2026-10-07T19:00:00Z",
  updated_at: "2026-10-07T19:00:01Z",
};

test("an effect is accepted, and refused for a status outside the closed set", () => {
  const spec = cursorPageSpec(economicEffectSpec);
  assert.doesNotThrow(() => validated({ items: [effect], next_cursor: null }, spec, "/advanced/effects"));
  refuses(
    () => validated({ items: [broken(effect, (c) => (c["status"] = "DONE"))], next_cursor: null }, spec, "/e"),
    /^\/e\.items\[0\]\.status$/,
  );
  refuses(
    () => validated({ items: [broken(effect, (c) => (c["journal_transaction_id"] = "journal-1"))], next_cursor: null }, spec, "/e"),
    /journal_transaction_id$/,
  );
});

const decision = {
  id: "01a075f1-f902-7a98-adbc-5fc08c37e83f",
  intent_id: INTENT,
  intent_kind: "ACQUIRE_EXPOSURE",
  stage: "PRE_TRADE",
  verdict: "REJECT",
  reason_codes: ["RISK_INSUFFICIENT_BALANCE"],
  input_hash: HASH,
  policy_version: "risk-v2-sandbox-1",
  policy_hash: HASH,
  evaluator_version: "risk-kernel/2",
  decision_hash: HASH,
  evaluated_at: "2026-10-07T19:00:00Z",
};

test("a risk decision is accepted, and refused for an unknown verdict or missing codes", () => {
  assert.doesNotThrow(() => validated({ items: [decision] }, riskDecisionListSpec, "/advanced/risk/decisions"));
  refuses(
    () => validated({ items: [broken(decision, (c) => (c["verdict"] = "MAYBE"))] }, riskDecisionListSpec, "/r"),
    /verdict$/,
  );
  refuses(
    () => validated({ items: [broken(decision, (c) => delete c["reason_codes"])] }, riskDecisionListSpec, "/r"),
    /reason_codes$/,
  );
});

/* --------------------------------------------------------------------------
 * GET /advanced/buying-power
 * ------------------------------------------------------------------------ */

const buyingPower: Record<string, unknown> = {
  value_domain: "SIMULATED",
  policy_version: "buying-power/2|bp2-sandbox-v1",
  as_of: "2026-10-07T19:00:00Z",
  reasons: [{ code: "CAPITAL_RESERVED", detail: "open orders", amount: "10.00" }],
  lines: [
    { field: "eligible_collateral", component: "WALLET", asset_id: SANDBOX_DOLLAR, amount_usd: "1000.00", detail: "SBX-USD" },
    { field: "eligible_equity", component: "BORROW_LIABILITIES", amount_usd: "-0.00", detail: "margin loan" },
  ],
};
for (const field of BUYING_POWER_V2_FIELDS) buyingPower[field] = "1000.00";

test("buying power V2 is accepted with every component", () => {
  assert.doesNotThrow(() => validated(buyingPower, buyingPowerV2Spec, "/advanced/buying-power"));
});

test("buying power V2 is refused when any component is missing or not an exact two-decimal string", () => {
  for (const field of BUYING_POWER_V2_FIELDS) {
    const path = new RegExp(`\\.${field}$`);
    refuses(() => validated(broken(buyingPower, (c) => delete c[field]), buyingPowerV2Spec, "/bp"), path);
    refuses(() => validated(broken(buyingPower, (c) => (c[field] = "1000.5")), buyingPowerV2Spec, "/bp"), path);
    refuses(() => validated(broken(buyingPower, (c) => (c[field] = 1000)), buyingPowerV2Spec, "/bp"), path);
  }
});

test("buying power V2 is refused when it does not say SIMULATED, or its explanation is malformed", () => {
  refuses(() => validated(broken(buyingPower, (c) => (c["value_domain"] = "REAL")), buyingPowerV2Spec, "/bp"), /value_domain$/);
  refuses(
    () => validated(broken(buyingPower, (c) => ((c["lines"] as Array<Record<string, unknown>>)[0] as Record<string, unknown>)["amount_usd"] = "1000"), buyingPowerV2Spec, "/bp"),
    /lines\[0\]\.amount_usd$/,
  );
  refuses(
    () => validated(broken(buyingPower, (c) => ((c["reasons"] as Array<Record<string, unknown>>)[0] as Record<string, unknown>)["amount"] = "ten"), buyingPowerV2Spec, "/bp"),
    /reasons\[0\]\.amount$/,
  );
  refuses(() => validated(broken(buyingPower, (c) => delete c["lines"]), buyingPowerV2Spec, "/bp"), /\.lines$/);
});

/* --------------------------------------------------------------------------
 * Keys and retries
 * ------------------------------------------------------------------------ */

test("every account-scoped key is namespaced by its account", () => {
  const scoped = [
    advancedKeys.intents(ACCOUNT, "", 25),
    advancedKeys.intent(ACCOUNT, INTENT),
    advancedKeys.effects(ACCOUNT, "c", 25),
    advancedKeys.riskDecisions(ACCOUNT, 50),
    advancedKeys.buyingPower(ACCOUNT),
  ];
  for (const key of scoped) {
    assert.deepEqual(key.slice(0, 3), [...advancedKeys.account(ACCOUNT)]);
  }
  assert.notDeepEqual(advancedKeys.buyingPower(ACCOUNT), advancedKeys.buyingPower(INTENT));
});

function problem(status: number, code: string): ApiProblem {
  return new ApiProblem({ type: "about:blank", title: code, status, code });
}

test("a 4xx answer and a contract violation are never retried; a fault is, twice", () => {
  assert.equal(retryTransient(0, problem(404, "NOT_FOUND")), false);
  assert.equal(retryTransient(0, problem(403, "FORBIDDEN")), false);
  assert.equal(retryTransient(0, problem(429, "RATE_LIMITED")), false);
  assert.equal(retryTransient(0, new ContractViolation("/x", "bad")), false);
  assert.equal(retryTransient(0, problem(503, "PROVIDER_UNAVAILABLE")), true);
  assert.equal(retryTransient(1, new Error("network")), true);
  assert.equal(retryTransient(2, new Error("network")), false);
});

test("problemCode reads the backend's code and nothing else", () => {
  assert.equal(problemCode(problem(404, "NOT_FOUND")), "NOT_FOUND");
  assert.equal(problemCode(new Error("NOT_FOUND")), undefined);
  assert.equal(problemCode(undefined), undefined);
});
