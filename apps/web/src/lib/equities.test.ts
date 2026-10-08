/**
 * The equities section's pure helpers and its runtime contract
 * (src/api/advanced/equities.ts).
 *
 * Each spec is shown a realistic answer, which it must accept unchanged, and
 * then that answer broken one way at a time, which it must refuse with a
 * ContractViolation naming the field — including the rules no per-field spec
 * can state: a FILLED order carries its fill, a fill and a settlement are
 * their order's and fill's own, a SETTLED settlement says when, nothing is
 * due before its trade date, and a placement that did not happen has no
 * venue answer.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { advancedKeys } from "../api/advanced/core.ts";
import {
  checkFill,
  checkOrder,
  checkSettlement,
  checkSubmission,
  equitiesKeys,
  equityActionPageSpec,
  equityActionResultSpec,
  equityApplicationPageSpec,
  equityCalendarSpec,
  equityListingListSpec,
  equitySettlementRunSpec,
  equityVenueRunSpec,
} from "../api/advanced/equities.ts";
import { ContractViolation, validated } from "../api/contract.ts";
import {
  ACTION_STATUS_MEANING,
  ACTION_TYPE_LABEL,
  EQUITY_ACTION_STATUSES,
  EQUITY_ACTION_TYPES,
  EQUITY_ORDER_STATUSES,
  EQUITY_ROUNDINGS,
  EQUITY_SESSIONS,
  ORDER_STATUS_MEANING,
  ROUNDING_LABEL,
  SESSION_LABEL,
  isLive,
  parseBaseUnitsInput,
  parseDateInput,
  parsePriceInput,
  parseSymbolInput,
  parseWholeNumber,
  reasonProblem,
  refusalField,
  sessionNote,
} from "./equities.ts";

const ACCOUNT = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const INSTRUMENT = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const ORDER = "01a075f1-f850-7f00-9b1a-2c3d4e5f6a7b";
const OTHER = "01a075f1-f851-7f00-9b1a-2c3d4e5f6a7c";
const FILL = "01a075f1-f852-7f00-9b1a-2c3d4e5f6a7d";
const SETTLEMENT = "01a075f1-f853-7f00-9b1a-2c3d4e5f6a7e";
const SHARE = "01a075f1-f860-7000-8000-000000000001";
const DOLLAR = "01920000-0000-7000-8000-5342582d5553";
const INTENT = "01a075f1-f870-7000-8000-000000000003";
const JOURNAL = "01a075f1-f870-7000-8000-000000000004";
const EFFECT = "01a075f1-f870-7000-8000-000000000005";
const ACTION = "01a075f1-f880-7000-8000-000000000006";
const HASH = "9f2c4be1d0a7c3e85b16f4a2d9e07c31a5b8f6e2c4d1a9b7e3f05c8d2a6b4e19";
const AT = "2026-10-07T14:00:00Z";

const session = {
  calendar: "XNYS",
  timezone: "America/New_York",
  as_of: AT,
  session: "REGULAR",
  trade_date: "2026-10-07",
  trading_day: true,
  next_trading_day: "2026-10-08",
  settlement_date: "2026-10-08",
};

const listing = {
  value_domain: "SIMULATED",
  instrument_id: INSTRUMENT,
  kind: "EQUITY",
  symbol: "EXMP",
  name: "Example Corp",
  exchange_mic: "XNAS",
  status: "ACTIVE",
  terms_version: 1,
  terms_hash: HASH,
  trading_calendar: "XNYS",
  venue: "nodal-paper-equities",
  share_asset_id: SHARE,
  share_symbol: "EXMP",
  share_decimals: 6,
  settlement_asset_id: DOLLAR,
  settlement_symbol: "SBX-USD",
  settlement_decimals: 6,
  price_scale: 2,
  tick_price: "0.01",
  lot_size: "1",
  min_quantity: "1",
  oracle_max_age_ms: 60000,
  mark: { price: "150.00", source: "itest", observed_at: AT },
};

const settlement = {
  value_domain: "SIMULATED",
  id: SETTLEMENT,
  fill_id: FILL,
  order_id: ORDER,
  account_id: ACCOUNT,
  instrument_id: INSTRUMENT,
  asset_id: SHARE,
  asset_symbol: "EXMP",
  asset_decimals: 6,
  quantity: "2000000",
  trade_date: "2026-10-07",
  due_date: "2026-10-08",
  state: "PENDING",
};

const fill = {
  value_domain: "SIMULATED",
  id: FILL,
  order_id: ORDER,
  account_id: ACCOUNT,
  instrument_id: INSTRUMENT,
  side: "BUY",
  quantity: "2000000",
  share_decimals: 6,
  price: "150.15",
  slippage_bps: 10,
  mark: { price: "150.00", source: "itest", observed_at: AT },
  notional: "300300000",
  spread: "300300",
  platform_fees: "300300",
  venue_fees: "0",
  total: "300900600",
  settlement_asset_id: DOLLAR,
  settlement_symbol: "SBX-USD",
  settlement_decimals: 6,
  fee_schedule_version: "equities-sandbox-v1",
  fee_schedule_hash: HASH,
  fee_disclosure_hash: HASH,
  venue: "nodal-paper-equities",
  venue_fill_id: "paper-1",
  executed_at: AT,
  trade_date: "2026-10-07",
  settlement_date: "2026-10-08",
  journal_transaction_id: JOURNAL,
  effect_id: EFFECT,
  settlement,
};

const order = {
  value_domain: "SIMULATED",
  id: ORDER,
  account_id: ACCOUNT,
  instrument_id: INSTRUMENT,
  symbol: "EXMP",
  exchange_mic: "XNAS",
  side: "BUY",
  order_type: "MARKET",
  mode: "PAPER",
  status: "FILLED",
  quantity: "2000000",
  share_asset_id: SHARE,
  share_decimals: 6,
  settlement_asset_id: DOLLAR,
  price_scale: 2,
  tick_price: "0.01",
  extended_hours: false,
  session: "REGULAR",
  trade_date: "2026-10-07",
  expires_at: "2026-10-07T20:00:00Z",
  intent_id: INTENT,
  venue: "nodal-paper-equities",
  created_at: AT,
  updated_at: AT,
  protection_price: "151.50",
  rejection_reasons: [],
  fill,
};

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
 * The contract
 * ------------------------------------------------------------------------ */

test("listings and the session in force are accepted as sent, a mark being optional", () => {
  const list = { value_domain: "SIMULATED", items: [listing, broken(listing, (l) => delete l["mark"])], session };
  assert.doesNotThrow(() => validated(list, equityListingListSpec, "/l"));
  refuses(() => validated(broken(list, (l) => delete l["session"]), equityListingListSpec, "/l"), /^\/l\.session$/);
  refuses(
    () => validated(broken(list, (l) => ((l["session"] as Record<string, unknown>)["session"] = "LUNCH")), equityListingListSpec, "/l"),
    /^\/l\.session\.session$/,
  );
  refuses(
    () =>
      validated(
        broken(list, (l) => ((l["items"] as Array<Record<string, unknown>>)[0]!["mark"] = { price: 150, source: "x", observed_at: AT })),
        equityListingListSpec,
        "/l",
      ),
    /^\/l\.items\[0\]\.mark\.price$/,
  );
  const calendar = { state: session, settlement_cycle_days: 1, holidays: [{ date: "2026-11-26", name: "Thanksgiving Day" }] };
  assert.doesNotThrow(() => validated(calendar, equityCalendarSpec, "/c"));
  refuses(() => validated(broken(calendar, (c) => delete c["holidays"]), equityCalendarSpec, "/c"), /^\/c\.holidays$/);
});

test("an order with its fill and its pending settlement is accepted unchanged", () => {
  assert.deepEqual(checkOrder(structuredClone(order), "/o"), order);
});

test("a FILLED order carries its fill, and the fill is its own", () => {
  refuses(() => checkOrder(broken(order, (o) => delete o["fill"]), "/o"), /^\/o\.fill$/);
  refuses(() => checkOrder(broken(order, (o) => ((o["fill"] as Record<string, unknown>)["order_id"] = OTHER)), "/o"), /^\/o\.fill\.order_id$/);
  assert.doesNotThrow(() =>
    checkOrder(
      broken(order, (o) => {
        o["status"] = "OPEN";
        delete o["fill"];
      }),
      "/o",
    ),
  );
  refuses(() => checkOrder(broken(order, (o) => (o["status"] = "PARTIAL")), "/o"), /^\/o\.status$/);
  refuses(() => checkOrder(broken(order, (o) => delete o["rejection_reasons"]), "/o"), /^\/o\.rejection_reasons$/);
  refuses(() => checkOrder(broken(order, (o) => (o["trade_date"] = "07/10/2026")), "/o"), /^\/o\.trade_date$/);
});

test("a fill's charges are exact base units, and its settlement is its own and due after it", () => {
  assert.doesNotThrow(() => checkFill(structuredClone(fill), "/f"));
  refuses(() => checkFill(broken(fill, (f) => (f["total"] = "300.90")), "/f"), /^\/f\.total$/);
  refuses(() => checkFill(broken(fill, (f) => (f["value_domain"] = "REAL")), "/f"), /^\/f\.value_domain$/);
  refuses(() => checkFill(broken(fill, (f) => ((f["settlement"] as Record<string, unknown>)["fill_id"] = OTHER)), "/f"), /^\/f\.settlement\.fill_id$/);
  refuses(() => checkFill(broken(fill, (f) => (f["settlement_date"] = "2026-10-06")), "/f"), /^\/f\.settlement_date$/);
});

test("a settlement says when it settled exactly when it is SETTLED, and is never due before its trade date", () => {
  assert.doesNotThrow(() => checkSettlement(structuredClone(settlement), "/s"));
  const settled = broken(settlement, (s) => {
    s["state"] = "SETTLED";
    s["settled_at"] = "2026-10-08T14:00:00Z";
    s["journal_transaction_id"] = JOURNAL;
  });
  assert.doesNotThrow(() => checkSettlement(settled, "/s"));
  refuses(() => checkSettlement(broken(settlement, (s) => (s["state"] = "SETTLED")), "/s"), /^\/s\.settled_at$/);
  refuses(() => checkSettlement(broken(settlement, (s) => (s["settled_at"] = AT)), "/s"), /^\/s\.settled_at$/);
  refuses(() => checkSettlement(broken(settlement, (s) => (s["due_date"] = "2026-10-06")), "/s"), /^\/s\.due_date$/);
  refuses(() => checkSettlement(broken(settlement, (s) => (s["state"] = "DONE")), "/s"), /^\/s\.state$/);
});

test("a placement that did not happen has no venue answer", () => {
  const placed = { value_domain: "SIMULATED", order, placement: { status: "PLACED", venue_status: "FILLED", detail: "placed" } };
  assert.doesNotThrow(() => checkSubmission(structuredClone(placed), "/x"));
  const notPlaced = {
    value_domain: "SIMULATED",
    order: broken(order, (o) => {
      o["status"] = "ACCEPTED";
      delete o["fill"];
    }),
    placement: { status: "NOT_PLACED", error_code: "PROVIDER_UNAVAILABLE", detail: "the venue did not answer; an operator's venue run places it" },
  };
  assert.doesNotThrow(() => checkSubmission(notPlaced, "/x"));
  refuses(
    () => checkSubmission(broken(notPlaced, (s) => ((s["placement"] as Record<string, unknown>)["venue_status"] = "OPEN")), "/x"),
    /^\/x\.placement\.venue_status$/,
  );
  refuses(() => checkSubmission(broken(placed, (s) => delete s["placement"]), "/x"), /^\/x\.placement$/);
});

test("corporate actions, postings and the workers' runs are accepted only in their contract's shape", () => {
  const action = {
    id: ACTION,
    instrument_id: INSTRUMENT,
    type: "SPLIT",
    status: "APPLIED",
    effective_date: "2026-10-09",
    share_asset_id: SHARE,
    settlement_asset_id: DOLLAR,
    rounding: "SHARES_DOWN_CASH_DOWN",
    reason: "two-for-one",
    content_hash: HASH,
    announced_by_actor_type: "OPERATOR",
    announced_by_actor_id: "op-1",
    created_at: AT,
    applied_at: AT,
    ratio: { numerator: 2, denominator: 1 },
  };
  assert.doesNotThrow(() => validated({ items: [action], next_cursor: null }, equityActionPageSpec, "/a"));
  refuses(() => validated({ items: [broken(action, (a) => (a["type"] = "MERGER"))], next_cursor: null }, equityActionPageSpec, "/a"), /^\/a\.items\[0\]\.type$/);

  const app = {
    value_domain: "SIMULATED",
    id: SETTLEMENT,
    action_id: ACTION,
    account_id: ACCOUNT,
    entitled_quantity: "1000000",
    shares_delta: "1000000",
    share_symbol: "EXMP",
    share_decimals: 6,
    settlement_amount: "0",
    settlement_symbol: "SBX-USD",
    settlement_decimals: 6,
    exact_amount_numerator: "0",
    exact_amount_denominator: "1",
    rounding: "SHARES_DOWN_CASH_DOWN",
    created_at: AT,
    action_type: "SPLIT",
  };
  assert.doesNotThrow(() => validated({ value_domain: "SIMULATED", items: [app], next_cursor: null }, equityApplicationPageSpec, "/p"));
  assert.doesNotThrow(() =>
    validated({ value_domain: "SIMULATED", action, applications: [app], replayed: false }, equityActionResultSpec, "/r"),
  );
  refuses(
    () => validated({ value_domain: "SIMULATED", action, applications: [app] }, equityActionResultSpec, "/r"),
    /^\/r\.replayed$/,
  );

  const settlementRun = { value_domain: "SIMULATED", as_of: AT, trade_date: "2026-10-08", batches: 1, settled: [settlement], failures: [] };
  assert.doesNotThrow(() => validated(settlementRun, equitySettlementRunSpec, "/sr"));
  const failure = {
    settlement_id: ORDER,
    fill_id: ORDER,
    account_id: ACCOUNT,
    attempts: 2,
    retry_after: AT,
    error_code: "CONFLICT",
    detail: "the settlement's effect is claimed and not applied",
  };
  assert.doesNotThrow(() => validated({ ...settlementRun, failures: [failure] }, equitySettlementRunSpec, "/sr"));
  // A run always says what it could not settle, even when that is nothing.
  refuses(() => validated(broken(settlementRun, (v) => delete v["failures"]), equitySettlementRunSpec, "/sr"), /^\/sr\.failures$/);
  refuses(
    () => validated({ ...settlementRun, failures: [{ ...failure, retry_after: "soon" }] }, equitySettlementRunSpec, "/sr"),
    /^\/sr\.failures\[0\]\.retry_after$/,
  );
  const venueRun = {
    value_domain: "SIMULATED",
    as_of: AT,
    items: [{ order_id: ORDER, action: "POLL", status_before: "OPEN", status_after: "FILLED", venue_status: "FILLED" }],
  };
  assert.doesNotThrow(() => validated(venueRun, equityVenueRunSpec, "/vr"));
  refuses(
    () => validated(broken(venueRun, (v) => ((v["items"] as Array<Record<string, unknown>>)[0]!["action"] = "RETRY")), equityVenueRunSpec, "/vr"),
    /^\/vr\.items\[0\]\.action$/,
  );
});

test("every account-scoped equities key sits under its account's advanced prefix", () => {
  const prefix = advancedKeys.account(ACCOUNT);
  for (const key of [
    equitiesKeys.orders(ACCOUNT, ""),
    equitiesKeys.order(ACCOUNT, ORDER),
    equitiesKeys.fills(ACCOUNT, "c"),
    equitiesKeys.settlements(ACCOUNT, "PENDING", ""),
    equitiesKeys.applications(ACCOUNT, ""),
  ]) {
    assert.deepEqual(key.slice(0, prefix.length), [...prefix]);
  }
  assert.deepEqual(equitiesKeys.listing(INSTRUMENT).slice(0, equitiesKeys.all.length), [...equitiesKeys.all]);
});

/* --------------------------------------------------------------------------
 * Words, statuses and sessions
 * ------------------------------------------------------------------------ */

test("every status, session, action and rounding has its words", () => {
  for (const s of EQUITY_ORDER_STATUSES) assert.ok(ORDER_STATUS_MEANING[s].length > 0);
  for (const s of EQUITY_SESSIONS) assert.ok(SESSION_LABEL[s].length > 0);
  for (const t of EQUITY_ACTION_TYPES) assert.ok(ACTION_TYPE_LABEL[t].length > 0);
  for (const s of EQUITY_ACTION_STATUSES) assert.ok(ACTION_STATUS_MEANING[s].length > 0);
  for (const r of EQUITY_ROUNDINGS) assert.ok(ROUNDING_LABEL[r].length > 0);
});

test("only a live order is offered a cancel", () => {
  assert.deepEqual(EQUITY_ORDER_STATUSES.filter(isLive), ["ACCEPTED", "OPEN"]);
});

test("the session note states the service's rule and leaves the decision to the backend", () => {
  assert.equal(sessionNote("REGULAR", "MARKET", false), undefined);
  assert.match(sessionNote("PRE_MARKET", "MARKET", false) ?? "", /regular session/);
  assert.match(sessionNote("AFTER_HOURS", "LIMIT", false) ?? "", /extended hours/);
  assert.equal(sessionNote("AFTER_HOURS", "LIMIT", true), undefined);
  assert.match(sessionNote("CLOSED", "LIMIT", true) ?? "", /closed/);
});

/* --------------------------------------------------------------------------
 * What a person typed
 * ------------------------------------------------------------------------ */

test("a price is sent exactly as written, or refused", () => {
  assert.deepEqual(parsePriceInput(" 1,250.50 "), { ok: true, value: "1250.50", error: "" });
  assert.equal(parsePriceInput("0.25", "the amount per share").value, "0.25");
  assert.match(parsePriceInput("", "the amount per share").error, /the amount per share/);
  for (const bad of ["-1", "1e2", "0", "0.000", "1.1234567890123456789", "1,2.3.4"]) {
    assert.equal(parsePriceInput(bad).ok, false, bad);
  }
});

test("a date is a real YYYY-MM-DD day", () => {
  assert.deepEqual(parseDateInput("2026-10-08"), { ok: true, value: "2026-10-08", error: "" });
  assert.equal(parseDateInput("2026-02-30").ok, false);
  assert.equal(parseDateInput("2026-13-01").ok, false);
  assert.equal(parseDateInput("10/08/2026").ok, false);
  assert.equal(parseDateInput("").ok, false);
});

test("whole numbers and base units are read digit by digit", () => {
  assert.deepEqual(parseWholeNumber("6", 0, 18, "decimals"), { ok: true, value: 6, error: "" });
  assert.equal(parseWholeNumber("19", 0, 18, "decimals").ok, false);
  assert.equal(parseWholeNumber("-1", 0, 18, "decimals").ok, false);
  assert.deepEqual(parseBaseUnitsInput("0100", "the lot size"), { ok: true, value: "100", error: "" });
  assert.equal(parseBaseUnitsInput("0", "the lot size").ok, false);
  assert.equal(parseBaseUnitsInput("1.5", "the lot size").ok, false);
});

test("symbols, reasons and refusal fields are checked only for what the page can know", () => {
  assert.equal(parseSymbolInput(" brk.b ").value, "BRK.B");
  assert.equal(parseSymbolInput("-ABC").ok, false);
  assert.equal(parseSymbolInput("").ok, false);
  assert.notEqual(reasonProblem(""), undefined);
  assert.equal(reasonProblem("mis-announced"), undefined);
  assert.equal(refusalField({ order_id: ORDER, rejection_code: "INSUFFICIENT_BUYING_POWER" }, "rejection_code"), "INSUFFICIENT_BUYING_POWER");
  assert.equal(refusalField({ order_id: 1 }, "order_id"), undefined);
  assert.equal(refusalField(undefined, "order_id"), undefined);
});
