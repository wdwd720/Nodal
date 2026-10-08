/**
 * The holdings contract (src/api/contract.ts: holdingsSpec, checkHoldings) and
 * the words for an unvalued holding (src/lib/holdings.ts).
 *
 * The validator is shown the backend's answer for an account that holds USDC,
 * the sandbox dollar a faucet grant left and a token without a current price
 * -- the shape TestIntegration_HoldingsSayWhatTheyCannotValueInsteadOfAZero
 * asserts -- which it must accept unchanged, and then that answer broken one
 * way at a time, which it must refuse with a ContractViolation naming the
 * field. A refused answer is an explicit error state on the page; an accepted
 * malformed one would become a figure, and the first live run of the suite
 * found exactly that: the sandbox dollar drawn as $0.00.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { ContractViolation, checkHoldings } from "../api/contract.ts";
import {
  BUYING_POWER_UNVALUED_STATUSES,
  HOLDING_UNVALUED_REASONS,
  HOLDING_VALUATION_STATUSES,
  UNVALUED_REASON_COPY,
  balanceIsValued,
  unvaluedFigureNote,
} from "./holdings.ts";

const PATH = "/accounts/{id}/holdings";

const ANSWER = {
  as_of: "2026-10-08T17:20:31.123456Z",
  holdings: [
    {
      asset: "01a11b2c-4dc6-76f9-a46e-00000000c0c0", symbol: "USDC", chain: "solana-devnet",
      mint_address: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", decimals: 6, quantity: "10000000000",
      valuation_status: "VALUED", usd_mark: "10000.00", price_ref: "face:USD", cost_basis_usd: "0.00",
      unrealized_pnl_usd: "10000.00", location: "",
    },
    {
      asset: "01920000-0000-7000-8000-5342582d5553", symbol: "SBX-USD", chain: "nodal-sandbox",
      mint_address: "01920000-0000-7000-8000-5342582d5553", decimals: 6, quantity: "1000000",
      valuation_status: "UNVALUED", unvalued_reason: "SIMULATED_VALUE", location: "",
    },
    {
      asset: "01a11b2c-4dc6-76f9-a46e-0000000057a1", symbol: "STALE", chain: "solana-devnet",
      mint_address: "mint-stale", decimals: 9, quantity: "2000000000",
      valuation_status: "UNVALUED", unvalued_reason: "NO_FRESH_MARK", cost_basis_usd: "300.00",
      realized_pnl_usd: "-12.50", location: "",
    },
  ],
};

type Mutable = Record<string, unknown>;

/** A deep copy of the answer with one change applied to it. */
function broken(change: (holdings: Mutable[]) => void): unknown {
  const copy = JSON.parse(JSON.stringify(ANSWER)) as { holdings: Mutable[] };
  change(copy.holdings);
  return copy;
}

function refuses(value: unknown, path: RegExp): void {
  assert.throws(
    () => checkHoldings(value, PATH),
    (error: unknown) => {
      assert.ok(error instanceof ContractViolation, `expected a ContractViolation, got ${String(error)}`);
      assert.match(error.path, path);
      return true;
    },
  );
}

test("the backend's answer is accepted unchanged, valued and unvalued holdings alike", () => {
  const out = checkHoldings(ANSWER, PATH);
  assert.equal(out.holdings.length, 3);
  assert.equal(out.holdings[0]?.usd_mark, "10000.00");
  assert.equal(out.holdings[1]?.unvalued_reason, "SIMULATED_VALUE");
  assert.equal(out.holdings[1]?.usd_mark, undefined);
  assert.equal(out.holdings[2]?.cost_basis_usd, "300.00");
  assert.deepEqual(checkHoldings({ as_of: ANSWER.as_of, holdings: [] }, PATH).holdings, []);
});

test("an unvalued holding is refused when it carries any valuation, a zero included", () => {
  refuses(broken((h) => { (h[1] as Mutable).usd_mark = "0.00"; }), /\.holdings\[1\]\.usd_mark$/);
  refuses(broken((h) => { (h[1] as Mutable).price_ref = "face:USD"; }), /\.holdings\[1\]\.price_ref$/);
  refuses(broken((h) => { (h[1] as Mutable).unrealized_pnl_usd = "0.00"; }), /\.holdings\[1\]\.unrealized_pnl_usd$/);
  refuses(broken((h) => { (h[2] as Mutable).usd_mark = "0.00"; }), /\.holdings\[2\]\.usd_mark$/);
  refuses(broken((h) => { (h[2] as Mutable).unrealized_pnl_usd = "-300.00"; }), /\.holdings\[2\]\.unrealized_pnl_usd$/);
});

test("simulated value carries no US dollar figure of any kind", () => {
  refuses(broken((h) => { (h[1] as Mutable).cost_basis_usd = "0.00"; }), /\.holdings\[1\]\.cost_basis_usd$/);
  refuses(broken((h) => { (h[1] as Mutable).realized_pnl_usd = "0.00"; }), /\.holdings\[1\]\.realized_pnl_usd$/);
});

test("an unvalued holding says why, with a reason the contract declares", () => {
  refuses(broken((h) => { delete (h[1] as Mutable).unvalued_reason; }), /\.holdings\[1\]\.unvalued_reason$/);
  refuses(broken((h) => { (h[1] as Mutable).unvalued_reason = "UNKNOWN"; }), /\.holdings\[1\]\.unvalued_reason$/);
  // A real asset without a mark still states what was paid for it.
  refuses(broken((h) => { delete (h[2] as Mutable).cost_basis_usd; }), /\.holdings\[2\]\.cost_basis_usd$/);
});

test("a valued holding carries its mark, the price it came from, its basis and its unrealized result", () => {
  for (const field of ["usd_mark", "price_ref", "cost_basis_usd", "unrealized_pnl_usd"]) {
    refuses(broken((h) => { delete (h[0] as Mutable)[field]; }), new RegExp(`\\.holdings\\[0\\]\\.${field}$`));
  }
  refuses(broken((h) => { (h[0] as Mutable).unvalued_reason = "NO_FRESH_MARK"; }), /\.holdings\[0\]\.unvalued_reason$/);
});

test("a status the contract does not declare, or none, is refused", () => {
  refuses(broken((h) => { (h[0] as Mutable).valuation_status = "ESTIMATED"; }), /\.holdings\[0\]\.valuation_status$/);
  refuses(broken((h) => { delete (h[0] as Mutable).valuation_status; }), /\.holdings\[0\]\.valuation_status$/);
});

test("figures that are stated are still held to their exact formats", () => {
  refuses(broken((h) => { (h[0] as Mutable).usd_mark = "10000"; }), /\.holdings\[0\]\.usd_mark$/);
  refuses(broken((h) => { (h[0] as Mutable).usd_mark = 10000; }), /\.holdings\[0\]\.usd_mark$/);
  refuses(broken((h) => { (h[1] as Mutable).quantity = "1.0"; }), /\.holdings\[1\]\.quantity$/);
  refuses(broken((h) => { (h[2] as Mutable).cost_basis_usd = "300"; }), /\.holdings\[2\]\.cost_basis_usd$/);
});

test("every reason has words, and an absent figure is never described as a value", () => {
  assert.deepEqual([...HOLDING_VALUATION_STATUSES], ["VALUED", "UNVALUED"]);
  for (const reason of HOLDING_UNVALUED_REASONS) {
    const copy = UNVALUED_REASON_COPY[reason];
    assert.ok(copy.label.length > 0 && copy.detail.length > 0, reason);
    for (const figure of ["mark", "unrealized", "basis", "price"] as const) {
      const note = unvaluedFigureNote(reason, figure);
      assert.ok(note.length > 0, `${reason} ${figure}`);
      assert.doesNotMatch(note, /[0-9]/, "a note in place of a figure contains no digit that could read as one");
    }
  }
  assert.match(UNVALUED_REASON_COPY.SIMULATED_VALUE.detail, /no economic value/);
});

test("buying power's unvalued balances are told apart from valued ones by their status", () => {
  for (const status of BUYING_POWER_UNVALUED_STATUSES) assert.equal(balanceIsValued(status), false, status);
  for (const status of ["NORMAL", "DEGRADED", "ACTIVE", "CLOSE_ONLY", "DELISTING", "PORTFOLIO_ONLY"]) {
    assert.equal(balanceIsValued(status), true, status);
  }
});
