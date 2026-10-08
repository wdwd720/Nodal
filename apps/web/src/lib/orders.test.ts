/**
 * The order-book section's pure helpers and its runtime contract
 * (src/api/advanced/orders.ts).
 *
 * Each spec is shown a realistic answer, which it must accept unchanged, and
 * then that answer broken one way at a time, which it must refuse with a
 * ContractViolation naming the field — including the rules no per-field spec
 * can state: a book's fee schedules are quoted in its own quote asset, its
 * default crossing rule forbids crossing, and a command's fills are its
 * order's own.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { advancedKeys } from "../api/advanced/core.ts";
import {
  bookDepthSpec,
  bookFillPageSpec,
  bookJournalVerificationSpec,
  bookOrderPageSpec,
  bookTradeProofSpec,
  checkBook,
  checkCommandResult,
  ordersKeys,
} from "../api/advanced/orders.ts";
import { ContractViolation, validated } from "../api/contract.ts";
import {
  BOOK_ORDER_STATUSES,
  BOOK_STATUSES,
  BOOK_STATUS_MEANING,
  ORDER_STATUS_MEANING,
  bookTransitions,
  cancelAvailability,
  isResting,
  isWorking,
  ladderRows,
  orderEntryAvailability,
  orderShapeProblem,
  parseClientOrderId,
  parsePriceInput,
  parseSequence,
  parseTermsJson,
  parseWholeNumber,
  reasonProblem,
  refusalReason,
  refusedOrderId,
  replaceAvailability,
} from "./orders.ts";

const ACCOUNT = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const INSTRUMENT = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const BOOK = "01a075f1-f82d-7c11-8a3e-4b2c1d0e9f81";
const ORDER = "01a075f1-f850-7f00-9b1a-2c3d4e5f6a7b";
const OTHER_ORDER = "01a075f1-f851-7f00-9b1a-2c3d4e5f6a7c";
const BASE = "01a075f1-f860-7000-8000-000000000001";
const QUOTE = "01a075f1-f860-7000-8000-000000000002";
const JOURNAL = "01a075f1-f870-7000-8000-000000000003";
const EFFECT = "01a075f1-f870-7000-8000-000000000004";
const HASH = "9f2c4be1d0a7c3e85b16f4a2d9e07c31a5b8f6e2c4d1a9b7e3f05c8d2a6b4e19";
const AT = "2026-10-07T14:00:00Z";

function schedule(version: string): Record<string, unknown> {
  return {
    version,
    quote_asset_id: QUOTE,
    quote_decimals: 6,
    percentage_fee_bps: 20,
    strategy_fee_bps: 0,
    subscription_discount_bps: 0,
    max_total_fee_bps: 10_000,
    hash: HASH,
  };
}

const book = {
  value_domain: "SIMULATED",
  id: BOOK,
  instrument_id: INSTRUMENT,
  venue_label: "SIMULATED_INTERNAL_BOOK",
  mode: "PAPER",
  status: "OPEN",
  instrument_kind: "SPOT_PAIR",
  base_asset_id: BASE,
  base_symbol: "SIMB",
  base_decimals: 8,
  quote_asset_id: QUOTE,
  quote_symbol: "SIMQ",
  quote_decimals: 6,
  price_scale: 2,
  tick_price: "0.01",
  lot_size: "10000",
  min_quantity: "10000",
  max_price: "92233720368547758.07",
  trading_calendar: "24x7",
  terms_source: "BOOK_SPOT",
  maker_fee: schedule("maker-v1"),
  taker_fee: schedule("taker-v1"),
  crossing_rule: { book_id: BOOK, version: 0, allow_house_customer_crossing: false, is_default: true },
  snapshot_interval: 1000,
  last_command_seq: 3,
  last_event_seq: 5,
  last_trade_seq: 1,
  state_hash: HASH,
};

const order = {
  value_domain: "SIMULATED",
  id: ORDER,
  book_id: BOOK,
  instrument_id: INSTRUMENT,
  account_id: ACCOUNT,
  venue_label: "SIMULATED_INTERNAL_BOOK",
  mode: "PAPER",
  side: "BUY",
  order_type: "LIMIT",
  time_in_force: "GTC",
  liquidity_intent: "TAKE_LIQUIDITY",
  stp: "CANCEL_TAKER",
  post_only: false,
  reduce_only: false,
  strategy_originated: false,
  status: "FILLED",
  price: "2500.00",
  price_ticks: 250000,
  quantity: "200000000",
  filled_quantity: "200000000",
  remaining_quantity: "0",
  cancelled_quantity: "0",
  base_symbol: "SIMB",
  base_decimals: 8,
  quote_symbol: "SIMQ",
  quote_decimals: 6,
  client_order_id: "bob-1",
  intent_id: EFFECT,
  created_at: AT,
  updated_at: AT,
};

const fill = {
  value_domain: "SIMULATED",
  book_id: BOOK,
  instrument_id: INSTRUMENT,
  trade_seq: 1,
  command_seq: 3,
  order_id: ORDER,
  side: "BUY",
  liquidity: "TAKER",
  counterparty_owner_type: "CUSTOMER",
  price: "2500.00",
  price_ticks: 250000,
  quantity: "200000000",
  quote_notional: "5000000000",
  fee: "10000000",
  base_symbol: "SIMB",
  base_decimals: 8,
  quote_symbol: "SIMQ",
  quote_decimals: 6,
  crossing_rule_version: 0,
  journal_transaction_id: JOURNAL,
  effect_id: EFFECT,
  evidence_ref: "book:trade:1",
  evidence_sha256: HASH,
  executed_at: AT,
};

const proof = {
  value_domain: "SIMULATED",
  book_id: BOOK,
  instrument_id: INSTRUMENT,
  venue_label: "SIMULATED_INTERNAL_BOOK",
  trade_seq: 1,
  command_seq: 3,
  event_seq: 4,
  price: "2500.00",
  price_ticks: 250000,
  quantity: "200000000",
  quote_notional: "5000000000",
  base_decimals: 8,
  quote_decimals: 6,
  aggressor_side: "BUY",
  maker_order_id: OTHER_ORDER,
  taker_order_id: ORDER,
  maker_owner_type: "CUSTOMER",
  taker_owner_type: "CUSTOMER",
  maker_fee: "5000000",
  taker_fee: "10000000",
  maker_schedule_hash: HASH,
  taker_schedule_hash: HASH,
  crossing_rule_version: 0,
  journal_transaction_id: JOURNAL,
  effect_id: EFFECT,
  evidence_ref: "book:trade:1",
  evidence_sha256: HASH,
  executed_at: AT,
  snapshot_seq: 0,
  book_last_command_seq: 3,
  book_state_hash: HASH,
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

test("a book as the backend sends it is accepted unchanged", () => {
  const accepted = checkBook(structuredClone(book), "/b");
  assert.deepEqual(accepted, book);
});

test("a book is refused when a figure, an enum or the domain is not what the contract says", () => {
  refuses(() => checkBook(broken(book, (b) => (b["value_domain"] = "REAL")), "/b"), /^\/b\.value_domain$/);
  refuses(() => checkBook(broken(book, (b) => (b["status"] = "PAUSED")), "/b"), /^\/b\.status$/);
  refuses(() => checkBook(broken(book, (b) => (b["mode"] = "LIVE")), "/b"), /^\/b\.mode$/);
  refuses(() => checkBook(broken(book, (b) => (b["lot_size"] = "1.5")), "/b"), /^\/b\.lot_size$/);
  refuses(() => checkBook(broken(book, (b) => (b["tick_price"] = 1)), "/b"), /^\/b\.tick_price$/);
  refuses(() => checkBook(broken(book, (b) => delete b["crossing_rule"]), "/b"), /^\/b\.crossing_rule$/);
  refuses(() => checkBook(broken(book, (b) => (b["venue_label"] = "NYSE")), "/b"), /^\/b\.venue_label$/);
});

test("a book's fee schedules are quoted in its own quote asset, at its decimals", () => {
  refuses(
    () => checkBook(broken(book, (b) => ((b["maker_fee"] as Record<string, unknown>)["quote_asset_id"] = BASE)), "/b"),
    /^\/b\.maker_fee\.quote_asset_id$/,
  );
  refuses(
    () => checkBook(broken(book, (b) => ((b["taker_fee"] as Record<string, unknown>)["quote_decimals"] = 2)), "/b"),
    /^\/b\.taker_fee\.quote_decimals$/,
  );
  // The same asset in another letter case is the same asset.
  assert.doesNotThrow(() =>
    checkBook(broken(book, (b) => ((b["maker_fee"] as Record<string, unknown>)["quote_asset_id"] = QUOTE.toUpperCase())), "/b"),
  );
});

test("the default crossing rule forbids crossing, and the rule is the book's own", () => {
  refuses(
    () =>
      checkBook(
        broken(book, (b) => ((b["crossing_rule"] as Record<string, unknown>)["allow_house_customer_crossing"] = true)),
        "/b",
      ),
    /^\/b\.crossing_rule$/,
  );
  refuses(
    () => checkBook(broken(book, (b) => ((b["crossing_rule"] as Record<string, unknown>)["book_id"] = INSTRUMENT)), "/b"),
    /^\/b\.crossing_rule\.book_id$/,
  );
  const recorded = broken(book, (b) => {
    b["crossing_rule"] = {
      book_id: BOOK,
      version: 2,
      allow_house_customer_crossing: true,
      is_default: false,
      reason: "market making",
      actor_type: "OPERATOR",
      actor_id: "op-1",
      created_at: AT,
    };
  });
  assert.doesNotThrow(() => checkBook(recorded, "/b"));
});

test("a command's fills are its order's own", () => {
  const result = { value_domain: "SIMULATED", order, fills: [fill], intent_status: "COMPLETED", command_seq: 3, state_hash: HASH };
  assert.deepEqual(checkCommandResult(structuredClone(result), "/c"), result);
  refuses(
    () => checkCommandResult(broken(result, (r) => ((r["fills"] as Array<Record<string, unknown>>)[0]!["order_id"] = OTHER_ORDER)), "/c"),
    /^\/c\.fills\[0\]\.order_id$/,
  );
  refuses(() => checkCommandResult(broken(result, (r) => delete r["fills"]), "/c"), /^\/c\.fills$/);
  refuses(
    () => checkCommandResult(broken(result, (r) => ((r["order"] as Record<string, unknown>)["status"] = "DONE")), "/c"),
    /^\/c\.order\.status$/,
  );
});

test("a page of orders or trades states its domain and its cursor, even when null", () => {
  const orders = { value_domain: "SIMULATED", items: [order], next_cursor: null };
  assert.doesNotThrow(() => validated(orders, bookOrderPageSpec, "/o"));
  refuses(() => validated(broken(orders, (p) => delete p["next_cursor"]), bookOrderPageSpec, "/o"), /^\/o\.next_cursor$/);
  refuses(() => validated(broken(orders, (p) => delete p["value_domain"]), bookOrderPageSpec, "/o"), /^\/o\.value_domain$/);

  const trades = { value_domain: "SIMULATED", items: [fill], next_cursor: "abc" };
  assert.doesNotThrow(() => validated(trades, bookFillPageSpec, "/t"));
  refuses(
    () => validated(broken(trades, (p) => ((p["items"] as Array<Record<string, unknown>>)[0]!["fee"] = "0.1")), bookFillPageSpec, "/t"),
    /^\/t\.items\[0\]\.fee$/,
  );
  refuses(
    () =>
      validated(
        broken(trades, (p) => ((p["items"] as Array<Record<string, unknown>>)[0]!["counterparty_owner_type"] = "ALICE")),
        bookFillPageSpec,
        "/t",
      ),
    /^\/t\.items\[0\]\.counterparty_owner_type$/,
  );
});

test("depth, the trade proof and the journal verification are accepted only in their contract's shape", () => {
  const depth = {
    value_domain: "SIMULATED",
    book_id: BOOK,
    instrument_id: INSTRUMENT,
    status: "OPEN",
    base_decimals: 8,
    quote_decimals: 6,
    last_command_seq: 3,
    state_hash: HASH,
    bids: [{ price: "2499.99", price_ticks: 249999, quantity: "10000", orders: 1 }],
    asks: [],
  };
  assert.doesNotThrow(() => validated(depth, bookDepthSpec, "/d"));
  refuses(
    () => validated(broken(depth, (d) => ((d["bids"] as Array<Record<string, unknown>>)[0]!["quantity"] = 10000)), bookDepthSpec, "/d"),
    /^\/d\.bids\[0\]\.quantity$/,
  );
  refuses(() => validated(broken(depth, (d) => delete d["asks"]), bookDepthSpec, "/d"), /^\/d\.asks$/);

  assert.doesNotThrow(() => validated(proof, bookTradeProofSpec, "/p"));
  refuses(() => validated(broken(proof, (p) => (p["maker_owner_type"] = "BANK")), bookTradeProofSpec, "/p"), /^\/p\.maker_owner_type$/);
  refuses(() => validated(broken(proof, (p) => delete p["journal_transaction_id"]), bookTradeProofSpec, "/p"), /^\/p\.journal_transaction_id$/);

  const verification = {
    book_id: BOOK,
    instrument_id: INSTRUMENT,
    commands: 3,
    events: 5,
    trades: 1,
    orders_checked: 2,
    snapshot_seq: 0,
    genesis_hash: HASH,
    full_replay_hash: HASH,
    snapshot_replay_hash: HASH,
    state_hash: HASH,
    verified_at: AT,
  };
  assert.doesNotThrow(() => validated(verification, bookJournalVerificationSpec, "/j"));
  refuses(() => validated(broken(verification, (v) => (v["commands"] = "3")), bookJournalVerificationSpec, "/j"), /^\/j\.commands$/);
});

test("every account-scoped order-book key sits under its account's advanced prefix", () => {
  const prefix = advancedKeys.account(ACCOUNT);
  for (const key of [
    ordersKeys.orders(ACCOUNT, "WORKING", ""),
    ordersKeys.order(ACCOUNT, ORDER),
    ordersKeys.trades(ACCOUNT, "c"),
    ordersKeys.proof(ACCOUNT, INSTRUMENT, "1"),
  ]) {
    assert.deepEqual(key.slice(0, prefix.length), [...prefix]);
  }
  assert.deepEqual(ordersKeys.book(INSTRUMENT).slice(0, ordersKeys.all.length), [...ordersKeys.all]);
  assert.deepEqual(ordersKeys.depth(INSTRUMENT, 20).slice(0, ordersKeys.book(INSTRUMENT).length), [...ordersKeys.book(INSTRUMENT)]);
});

/* --------------------------------------------------------------------------
 * What a status lets a person do
 * ------------------------------------------------------------------------ */

test("every status has its meaning", () => {
  for (const status of BOOK_STATUSES) assert.ok(BOOK_STATUS_MEANING[status].length > 0);
  for (const status of BOOK_ORDER_STATUSES) assert.ok(ORDER_STATUS_MEANING[status].length > 0);
});

test("working and resting orders are told apart, and cancel and replace follow them", () => {
  assert.deepEqual(BOOK_ORDER_STATUSES.filter(isWorking), ["PENDING", "OPEN", "PARTIALLY_FILLED"]);
  assert.deepEqual(BOOK_ORDER_STATUSES.filter(isResting), ["OPEN", "PARTIALLY_FILLED"]);

  assert.equal(cancelAvailability("PENDING", "OPEN").available, true, "a pending order is cancelled before it reaches the book");
  assert.equal(cancelAvailability("OPEN", "HALTED").available, true, "a halted book still accepts cancels");
  assert.equal(cancelAvailability("OPEN", "CLOSED").available, false);
  assert.match(cancelAvailability("FILLED", "OPEN").reason, /FILLED/);

  assert.equal(replaceAvailability("PARTIALLY_FILLED", "OPEN").available, true);
  assert.equal(replaceAvailability("PENDING", "OPEN").available, false, "only a resting order is replaced");
  assert.match(replaceAvailability("OPEN", "HALTED").reason, /HALTED/);
  assert.equal(replaceAvailability("OPEN", undefined).available, true, "an unread book status is the backend's to decide");

  assert.equal(orderEntryAvailability("OPEN").available, true);
  assert.match(orderEntryAvailability("HALTED").reason, /refused/);
});

test("the book transitions an operator is offered mirror the service's", () => {
  assert.deepEqual(bookTransitions("OPEN"), ["HALTED", "CLOSED"]);
  assert.deepEqual(bookTransitions("HALTED"), ["OPEN", "CLOSED"]);
  assert.deepEqual(bookTransitions("CLOSED"), []);
});

test("an order shape the book refuses is named before anything is sent", () => {
  const ok = { side: "BUY", orderType: "LIMIT", timeInForce: "GTC", postOnly: false, reduceOnly: false } as const;
  assert.equal(orderShapeProblem(ok), undefined);
  assert.match(orderShapeProblem({ ...ok, orderType: "MARKET" }) ?? "", /never rests/);
  assert.equal(orderShapeProblem({ ...ok, orderType: "MARKET", timeInForce: "IOC" }), undefined);
  assert.match(orderShapeProblem({ ...ok, postOnly: true, timeInForce: "IOC" }) ?? "", /Post-only/);
  assert.match(orderShapeProblem({ ...ok, reduceOnly: true }) ?? "", /reduce-only/);
  assert.equal(orderShapeProblem({ ...ok, side: "SELL", reduceOnly: true }), undefined);
});

/* --------------------------------------------------------------------------
 * What a person typed
 * ------------------------------------------------------------------------ */

test("a price is sent exactly as written, or refused", () => {
  assert.deepEqual(parsePriceInput(" 2,500.00 "), { ok: true, value: "2500.00", error: "" });
  assert.equal(parsePriceInput("2500").value, "2500");
  for (const bad of ["", "-1", "1e3", "2.5.0", "0", "0.00", "abc"]) {
    assert.equal(parsePriceInput(bad).ok, false, bad);
  }
});

test("a client order id is checked for what internal/orders refuses", () => {
  assert.deepEqual(parseClientOrderId(""), { ok: true, value: "", error: "" });
  assert.equal(parseClientOrderId("bob-1").value, "bob-1");
  assert.equal(parseClientOrderId(" bob").ok, false);
  assert.equal(parseClientOrderId("a".repeat(129)).ok, false);
  assert.equal(parseClientOrderId("é".repeat(65)).ok, false, "128 bytes, not characters");
  assert.equal(parseClientOrderId("a\tb").ok, false);
});

test("whole numbers, sequences and reasons are read digit by digit", () => {
  assert.deepEqual(parseWholeNumber("20", 0, 10_000, "bps"), { ok: true, value: 20, error: "" });
  assert.equal(parseWholeNumber("10001", 0, 10_000, "bps").ok, false);
  assert.equal(parseWholeNumber("2.5", 0, 10_000, "bps").ok, false);
  assert.equal(parseSequence("42"), 42);
  assert.equal(parseSequence("042"), undefined);
  assert.equal(parseSequence("0"), undefined);
  assert.equal(parseSequence("1".repeat(16)), undefined, "beyond exact integers");
  assert.equal(parseSequence(undefined), undefined);
  assert.equal(reasonProblem("  "), "Give the reason this is being done; it is recorded with the change.");
  assert.equal(reasonProblem("quarterly review"), undefined);
});

test("spot terms are one JSON object, or nothing", () => {
  assert.deepEqual(parseTermsJson(""), { ok: true, value: undefined, error: "" });
  assert.deepEqual(parseTermsJson('{"kind":"SPOT_PAIR"}').value, { kind: "SPOT_PAIR" });
  assert.equal(parseTermsJson("[1]").ok, false);
  assert.equal(parseTermsJson("{").ok, false);
  assert.equal(parseTermsJson("7").ok, false);
});

/* --------------------------------------------------------------------------
 * The ladder and refusals
 * ------------------------------------------------------------------------ */

test("the ladder reads the asks from furthest to best, then the bids from best down, and adds nothing", () => {
  const rows = ladderRows({ asks: ["a1", "a2", "a3"], bids: ["b1", "b2"] });
  assert.deepEqual(
    rows.map((r) => [r.side, r.level, r.rank]),
    [
      ["SELL", "a3", 3],
      ["SELL", "a2", 2],
      ["SELL", "a1", 1],
      ["BUY", "b1", 1],
      ["BUY", "b2", 2],
    ],
  );
  assert.deepEqual(ladderRows({ asks: [], bids: [] }), []);
});

test("a recorded refusal's order and reason are read from the fields the backend sent", () => {
  assert.equal(refusedOrderId({ order_id: ORDER, reason: "DUPLICATE_CLIENT_ORDER_ID" }), ORDER);
  assert.equal(refusalReason({ order_id: ORDER, reason: "DUPLICATE_CLIENT_ORDER_ID" }), "DUPLICATE_CLIENT_ORDER_ID");
  assert.equal(refusedOrderId({ order_id: 7 }), undefined);
  assert.equal(refusedOrderId(undefined), undefined);
  assert.equal(refusalReason({ reason: "" }), undefined);
});
