/**
 * The prediction markets section's pure helpers (lib/predmarket.ts) and the
 * runtime contract its hooks hold the backend to (api/advanced/predmarket.ts).
 *
 * Each spec is shown a realistic answer, which it must accept unchanged, and
 * then that answer broken one way at a time, which it must refuse with a
 * ContractViolation naming the field. A refused answer becomes an explicit
 * error state on the page; an accepted malformed one would become a figure.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { advancedKeys } from "../api/advanced/core.ts";
import {
  booksSpec,
  claimResultSpec,
  heldPayoutSpec,
  marketDetailSpec,
  marketListSpec,
  observationSpec,
  positionListSpec,
  predmarketKeys,
  reconciliationSpec,
  setResultSpec,
  settlementResultSpec,
  voidProposalResultSpec,
  voidProposalSpec,
} from "../api/advanced/predmarket.ts";
import { ContractViolation, validated, type Spec } from "../api/contract.ts";
import {
  DEFAULT_LIST_STATUS,
  DISPUTE_WINDOW_CHOICES,
  MARKET_STATUSES,
  STATUS_MEANING,
  claimable,
  isEd25519KeyHex,
  lifecycleSteps,
  localInputToInstant,
  parseCompleteSets,
  parseMarketStatus,
  parseOutcomes,
  pendingVoidProposals,
  reasonProblem,
} from "./predmarket.ts";

const MARKET = "01a075f1-f847-7a98-adbc-5fc08c37e8a1";
const ACCOUNT = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const SANDBOX_DOLLAR = "01920000-0000-7000-8000-5342582d5553";
const ID_A = "01a075f1-f846-719e-bcbb-493c8281c524";
const ID_B = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const ID_C = "01a075f1-f82c-78ad-b6cb-a0b5e6e58684";
const ID_D = "01a075f1-f82c-78ad-b6cb-a0b5e6e58685";
const AT = "2026-10-07T12:00:00Z";
const KEY_HEX = "ab".repeat(32);

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

const market = {
  value_domain: "SIMULATED",
  id: MARKET,
  symbol: "PMWEB",
  question: "Which outcome will the sandbox election board certify?",
  mode: "SANDBOX",
  status: "SETTLED",
  facility_id: ID_A,
  instrument_id: ID_B,
  collateral_asset_id: SANDBOX_DOLLAR,
  collateral_symbol: "SBX-USD",
  collateral_decimals: 6,
  unit_quantity: "1000000",
  share_decimals: 6,
  outcomes: [
    { index: 0, symbol: "YES", share_asset_id: ID_C, instrument_id: ID_D },
    { index: 1, symbol: "NO", share_asset_id: ID_D, instrument_id: ID_C },
  ],
  open_at: AT,
  close_at: "2026-10-07T13:00:00Z",
  dispute_window_ms: 600000,
  oracle_source: "sandbox-election-board",
  oracle_public_key: KEY_HEX,
  definition_hash: "abcd",
  created_by: { actor_type: "OPERATOR", actor_id: ID_A },
  created_at: AT,
  updated_at: AT,
};

const settlement = {
  id: ID_A,
  market_id: MARKET,
  kind: "PAYOUT",
  resolution_id: ID_B,
  winning_outcome_index: 0,
  winning_outcome_symbol: "YES",
  outcome_count: 2,
  unit_quantity: "1000000",
  rounding_policy: "EXACT",
  escrow_before: "5000000",
  total_paid: "5000000",
  total_held: "2000000",
  residual: "0",
  holder_count: 2,
  evidence_ref: "proof://predmarket/settlement.json",
  evidence_sha256: "5e77",
  settled_by: { actor_type: "OPERATOR", actor_id: ID_C },
  settled_at: AT,
};

const detail = {
  value_domain: "SIMULATED",
  market,
  resolutions: [
    {
      id: ID_B,
      market_id: MARKET,
      kind: "ORACLE",
      outcome_index: 0,
      outcome_symbol: "YES",
      report_hash: "feed",
      report_source: "sandbox-election-board",
      report_observed_at: "2026-10-07T13:00:30Z",
      evidence_ref: "proof://predmarket/oracle.json",
      evidence_sha256: "0a",
      accepted_by: { actor_type: "OPERATOR", actor_id: ID_C },
      reason: "oracle report accepted",
      accepted_at: AT,
    },
  ],
  disputes: [],
  void_proposals: [] as unknown[],
  settlement,
};

const books = {
  value_domain: "SIMULATED",
  market_id: MARKET,
  market_status: "OPEN",
  collateral_asset_id: SANDBOX_DOLLAR,
  collateral_symbol: "SBX-USD",
  collateral_decimals: 6,
  share_decimals: 6,
  escrow: "5000000",
  collateral: "5000000",
  residual: "0",
  held: "0",
  outcomes: [
    { index: 0, symbol: "YES", share_asset_id: ID_C, shares_issued: "5000000", shares_outstanding: "5000000", held_by_customers: "5000000" },
    { index: 1, symbol: "NO", share_asset_id: ID_D, shares_issued: "5000000", shares_outstanding: "5000000", held_by_customers: "5000000" },
  ],
  reconciles: true,
  violations: [],
};

const intent = {
  id: ID_A,
  account_id: ACCOUNT,
  kind: "CREATE_PREDICTION_POSITION",
  mode: "SANDBOX",
  capability: "PREDICTION_MARKETS",
  actor_type: "USER",
  terms: { action: "ISSUE_COMPLETE_SETS" },
  content_hash: "01",
  status: "COMPLETED",
  quantity: "3000000",
  created_at: AT,
  updated_at: AT,
};

const setResult = {
  value_domain: "SIMULATED",
  market_id: MARKET,
  account_id: ACCOUNT,
  action: "ISSUE",
  quantity: "3000000",
  replay: false,
  intent,
  effect: {
    id: ID_B,
    effect_key: `predmarket:issue:${ID_A}`,
    subsystem: "PREDICTION_MARKETS",
    effect_kind: "PREDICTION_SET_ISSUANCE",
    posts_ledger: true,
    status: "APPLIED",
    journal_transaction_id: ID_C,
    created_at: AT,
    updated_at: AT,
  },
  journal_transaction_id: ID_C,
  risk_decision_hash: "decision-hash",
};

const heldPayout = {
  id: ID_D,
  market_id: MARKET,
  settlement_id: ID_A,
  account_id: ACCOUNT,
  kind: "PAYOUT",
  amount: "2000000",
  reason_codes: ["RISK_ACCOUNT_STATUS"],
  refused_intent_id: ID_B,
  journal_transaction_id: ID_C,
  held_at: AT,
  claimed: false,
};

const proposal = {
  id: ID_C,
  market_id: MARKET,
  market_status: "DISPUTED",
  resolution_id: ID_B,
  reason: "the event was cancelled by its organiser",
  evidence_ref: "proof://predmarket/void-proposal.json",
  evidence_sha256: "0e",
  proposed_by: { actor_type: "OPERATOR", actor_id: ID_A },
  proposed_at: AT,
};

/* --------------------------------------------------------------------------
 * Pure helpers
 * ------------------------------------------------------------------------ */

test("complete sets are whole, and become base units by appending digits", () => {
  assert.deepEqual(parseCompleteSets("3", 6), { ok: true, value: "3000000", sets: "3", error: "" });
  assert.equal(parseCompleteSets(" 003 ", 6).value, "3000000", "leading zeros and spaces are dropped");
  assert.equal(parseCompleteSets("1,000", 6).value, "1000000000");
  assert.equal(parseCompleteSets("12", 0).value, "12");
  for (const bad of ["", "0", "000", "1.5", "-1", "1e6", "abc", "1234567890123456"]) {
    assert.equal(parseCompleteSets(bad, 6).ok, false, `${JSON.stringify(bad)} is refused`);
  }
  assert.equal(parseCompleteSets("3", -1).ok, false, "an unusable precision is refused");
});

test("a status in the address is one the service has, or the default with the value named", () => {
  assert.deepEqual(parseMarketStatus(null), { status: DEFAULT_LIST_STATUS, ignored: undefined });
  assert.deepEqual(parseMarketStatus(""), { status: DEFAULT_LIST_STATUS, ignored: undefined });
  assert.deepEqual(parseMarketStatus("SETTLED"), { status: "SETTLED", ignored: undefined });
  assert.deepEqual(parseMarketStatus("LIVE"), { status: DEFAULT_LIST_STATUS, ignored: "LIVE" });
  assert.deepEqual(parseMarketStatus("open"), { status: DEFAULT_LIST_STATUS, ignored: "open" });
});

test("every status has its meaning", () => {
  assert.deepEqual(Object.keys(STATUS_MEANING).sort(), [...MARKET_STATUSES].sort());
});

function states(status: string, resolved: boolean, disputed: boolean): string[] {
  return lifecycleSteps(status, { resolved, disputed }).map((step) => `${step.status}:${step.state}`);
}

test("the lifecycle marks what is done, current and ahead", () => {
  assert.deepEqual(states("OPEN", false, false), [
    "DRAFT:done",
    "OPEN:current",
    "CLOSED:ahead",
    "RESOLVING:ahead",
    "RESOLVED:ahead",
    "SETTLED:ahead",
  ]);
  assert.deepEqual(states("SETTLED", true, false), [
    "DRAFT:done",
    "OPEN:done",
    "CLOSED:done",
    "RESOLVING:done",
    "RESOLVED:done",
    "SETTLED:current",
  ]);
});

test("a dispute appears after the resolution, and stays done once ruled on", () => {
  assert.deepEqual(states("DISPUTED", true, true).slice(4), ["RESOLVED:done", "DISPUTED:current", "SETTLED:ahead"]);
  assert.deepEqual(states("RESOLVED", true, true).slice(4), ["RESOLVED:current", "DISPUTED:done", "SETTLED:ahead"]);
  assert.deepEqual(states("SETTLED", true, true).slice(4), ["RESOLVED:done", "DISPUTED:done", "SETTLED:current"]);
});

test("a void claims only the steps the market's records prove", () => {
  assert.deepEqual(states("VOIDED", false, false), [
    "DRAFT:done",
    "OPEN:unknown",
    "CLOSED:unknown",
    "RESOLVING:unknown",
    "RESOLVED:unknown",
    "VOIDED:current",
  ]);
  assert.deepEqual(states("VOIDED", true, true), [
    "DRAFT:done",
    "OPEN:done",
    "CLOSED:done",
    "RESOLVING:done",
    "RESOLVED:done",
    "DISPUTED:done",
    "VOIDED:current",
  ]);
});

test("a status the service does not have marks nothing as current", () => {
  assert.ok(states("LIVE", false, false).every((step) => step.endsWith(":ahead")));
});

test("the operator's form helpers check shape and change nothing else", () => {
  assert.equal(isEd25519KeyHex(KEY_HEX), true);
  assert.equal(isEd25519KeyHex(KEY_HEX.toUpperCase()), true);
  assert.equal(isEd25519KeyHex("ab".repeat(31)), false);
  assert.equal(isEd25519KeyHex(`${"ab".repeat(31)}zz`), false);
  assert.deepEqual(parseOutcomes(" YES, NO ,, maybe "), ["YES", "NO", "maybe"], "sent as typed: not upper-cased");
  assert.equal(reasonProblem("too short") === undefined, true, "nine characters is enough");
  assert.notEqual(reasonProblem("  short  "), undefined);
  assert.notEqual(reasonProblem("x".repeat(2001)), undefined);
  assert.equal(localInputToInstant(""), undefined);
  assert.equal(localInputToInstant("not a time"), undefined);
  // A local wall-clock time: the UTC date depends on the zone the test runs in.
  assert.match(localInputToInstant("2026-10-07T12:30") ?? "", /^2026-10-0[678]T[0-9:.]+Z$/);
  assert.deepEqual(
    DISPUTE_WINDOW_CHOICES.map((choice) => choice.ms),
    [...DISPUTE_WINDOW_CHOICES.map((choice) => choice.ms)].sort((a, b) => a - b),
    "the choices are in increasing order",
  );
});

test("an account's shares sit under that account's key prefix", () => {
  const key = predmarketKeys.positions(ACCOUNT, MARKET);
  assert.deepEqual(key.slice(0, advancedKeys.account(ACCOUNT).length), [...advancedKeys.account(ACCOUNT)]);
  assert.deepEqual(predmarketKeys.market(MARKET).slice(0, predmarketKeys.all.length), [...predmarketKeys.all]);
  assert.deepEqual(predmarketKeys.books(MARKET).slice(0, predmarketKeys.all.length), [...predmarketKeys.all]);
});

/* --------------------------------------------------------------------------
 * The runtime contract
 * ------------------------------------------------------------------------ */

function accepts(value: unknown, spec: Spec): void {
  assert.doesNotThrow(() => validated(value, spec, "/x"));
}

test("a market and its detail are accepted as the backend sends them", () => {
  accepts(detail, marketDetailSpec);
  accepts({ value_domain: "SIMULATED", status: "OPEN", items: [market] }, marketListSpec);
  accepts(broken(detail, (copy) => delete copy["settlement"]), marketDetailSpec);
});

test("a market is refused when a figure, a status or the domain is not what the contract says", () => {
  refuses(() => validated(broken(detail, (c) => ((c["market"] as Record<string, unknown>)["unit_quantity"] = 1000000)), marketDetailSpec, "/x"), /unit_quantity$/);
  refuses(() => validated(broken(detail, (c) => ((c["market"] as Record<string, unknown>)["unit_quantity"] = "1.00")), marketDetailSpec, "/x"), /unit_quantity$/);
  refuses(() => validated(broken(detail, (c) => ((c["market"] as Record<string, unknown>)["status"] = "LIVE")), marketDetailSpec, "/x"), /status$/);
  refuses(() => validated(broken(detail, (c) => ((c["market"] as Record<string, unknown>)["mode"] = "LIVE")), marketDetailSpec, "/x"), /mode$/);
  refuses(() => validated(broken(detail, (c) => (c["value_domain"] = "REAL")), marketDetailSpec, "/x"), /value_domain$/);
  refuses(() => validated(broken(detail, (c) => ((c["market"] as Record<string, unknown>)["value_domain"] = undefined)), marketDetailSpec, "/x"), /value_domain$/);
  refuses(() => validated(broken(detail, (c) => delete c["resolutions"]), marketDetailSpec, "/x"), /resolutions$/);
});

test("a settlement record is refused unless every amount is exact base units", () => {
  refuses(() => validated(broken(detail, (c) => ((c["settlement"] as Record<string, unknown>)["total_paid"] = "5.00")), marketDetailSpec, "/x"), /total_paid$/);
  refuses(() => validated(broken(detail, (c) => ((c["settlement"] as Record<string, unknown>)["residual"] = 0)), marketDetailSpec, "/x"), /residual$/);
  refuses(() => validated(broken(detail, (c) => ((c["settlement"] as Record<string, unknown>)["kind"] = "BONUS")), marketDetailSpec, "/x"), /kind$/);
  refuses(() => validated(broken(detail, (c) => ((c["settlement"] as Record<string, unknown>)["rounding_policy"] = "ROUND_HALF_UP")), marketDetailSpec, "/x"), /rounding_policy$/);
});

test("the escrow books carry the verdict and every violation as a list", () => {
  accepts(books, booksSpec);
  accepts({ ...books, reconciles: false, violations: ["outcome 0: customers hold 4000000 shares and 5000000 are outstanding"] }, booksSpec);
  refuses(() => validated(broken(books, (c) => (c["violations"] = null)), booksSpec, "/x"), /violations$/);
  refuses(() => validated(broken(books, (c) => (c["reconciles"] = "yes")), booksSpec, "/x"), /reconciles$/);
  refuses(() => validated(broken(books, (c) => (c["escrow"] = "5000000.0")), booksSpec, "/x"), /escrow$/);
});

test("an account's shares are exact base units", () => {
  const list = {
    value_domain: "SIMULATED",
    market_id: MARKET,
    account_id: ACCOUNT,
    share_decimals: 6,
    items: [{ outcome_index: 0, outcome_symbol: "YES", share_asset_id: ID_C, quantity: "3000000", as_of: AT }],
  };
  accepts(list, positionListSpec);
  accepts({ ...list, items: [] }, positionListSpec);
  refuses(() => validated(broken(list, (c) => ((c["items"] as Array<Record<string, unknown>>)[0]!["quantity"] = 3)), positionListSpec, "/x"), /quantity$/);
});

test("a complete-set answer carries the economic intent and effect that carried it out", () => {
  accepts(setResult, setResultSpec);
  accepts(broken(setResult, (c) => delete c["effect"]), setResultSpec);
  refuses(() => validated(broken(setResult, (c) => ((c["intent"] as Record<string, unknown>)["mode"] = "LIVE")), setResultSpec, "/x"), /intent\.mode$/);
  refuses(() => validated(broken(setResult, (c) => (c["action"] = "TRADE")), setResultSpec, "/x"), /action$/);
  refuses(() => validated(broken(setResult, (c) => delete c["replay"]), setResultSpec, "/x"), /replay$/);
});

test("a settlement answer lists what it paid; a replay lists nothing", () => {
  const result = {
    value_domain: "SIMULATED",
    market,
    settlement,
    replay: false,
    held_payouts: [] as unknown[],
    lines: [
      {
        account_id: ACCOUNT,
        amount: "3000000",
        intent_id: ID_A,
        journal_transaction_id: ID_B,
        held: false,
        shares: [
          { outcome_symbol: "YES", quantity: "3000000" },
          { outcome_symbol: "NO", quantity: "3000000" },
        ],
      },
    ],
  };
  accepts(result, settlementResultSpec);
  accepts({ ...result, replay: true, lines: [], held_payouts: [] }, settlementResultSpec);
  accepts({ ...result, held_payouts: [heldPayout] }, settlementResultSpec);
  refuses(() => validated(broken(result, (c) => delete c["held_payouts"]), settlementResultSpec, "/x"), /held_payouts$/);
  refuses(() => validated(broken(result, (c) => delete (c["lines"] as Array<Record<string, unknown>>)[0]!["held"]), settlementResultSpec, "/x"), /held$/);
  refuses(() => validated(broken(result, (c) => ((c["lines"] as Array<Record<string, unknown>>)[0]!["amount"] = "3.00")), settlementResultSpec, "/x"), /amount$/);
});

test("an observation and a reconciliation are accepted only in their contract's shape", () => {
  const observation = {
    value_domain: "SIMULATED",
    observation_id: ID_D,
    report_hash: "feed",
    recorded_by: { actor_type: "OPERATOR", actor_id: ID_A },
    recorded_at: AT,
    replay: false,
    report: {
      market_id: MARKET,
      outcome: "YES",
      source: "sandbox-election-board",
      observed_at: "2026-10-07T13:00:30Z",
      evidence: "eyJjZXJ0aWZpZWQiOiJZRVMifQ==",
      signature: "c2lnbmF0dXJl",
    },
  };
  accepts(observation, observationSpec);
  refuses(() => validated(broken(observation, (c) => delete c["report"]), observationSpec, "/x"), /report$/);
  refuses(() => validated(broken(observation, (c) => delete c["recorded_by"]), observationSpec, "/x"), /recorded_by$/);

  const record = {
    value_domain: "SIMULATED",
    id: ID_A,
    kind: "PREDICTION_ESCROW",
    mode: "FULL",
    scope_type: "prediction_market",
    scope_id: MARKET,
    status: "MATCHED",
    material: false,
    blocks_new_risk: false,
    expected: { residual: "0" },
    observed: { escrow: "5000000" },
    difference: { violations: [] },
    opened_at: AT,
  };
  accepts(record, reconciliationSpec);
  refuses(() => validated(broken(record, (c) => (c["kind"] = "WALLET_BALANCE")), reconciliationSpec, "/x"), /kind$/);
  refuses(() => validated(broken(record, (c) => (c["status"] = "FINE")), reconciliationSpec, "/x"), /status$/);
});

test("a void proposal waits for a second operator until it is approved or the market ends", () => {
  const approved = { ...proposal, id: ID_D, approval: { id: ID_A, approved_by: { actor_type: "OPERATOR", actor_id: ID_B }, reason: "confirmed", approved_at: AT } };
  assert.deepEqual(pendingVoidProposals([proposal, approved], "DISPUTED"), [proposal]);
  assert.deepEqual(pendingVoidProposals([proposal], "VOIDED"), [], "a voided market waits for nothing");
  assert.deepEqual(pendingVoidProposals([proposal], "SETTLED"), [], "a settled market can no longer be voided");
});

test("a held payout can be claimed until it has been", () => {
  assert.equal(claimable(undefined), false, "nothing held, nothing to claim");
  assert.equal(claimable({ claimed: false }), true);
  assert.equal(claimable({ claimed: true }), false);
});

test("void proposals, held payouts and claims are accepted only in their contract's shape", () => {
  accepts(proposal, voidProposalSpec);
  accepts(broken(proposal, (c) => delete c["resolution_id"]), voidProposalSpec);
  refuses(() => validated(broken(proposal, (c) => (c["market_status"] = "LIVE")), voidProposalSpec, "/x"), /market_status$/);
  refuses(() => validated(broken(proposal, (c) => delete c["proposed_by"]), voidProposalSpec, "/x"), /proposed_by$/);
  refuses(
    () => validated(broken(proposal, (c) => (c["approval"] = { id: ID_A, reason: "x", approved_at: AT })), voidProposalSpec, "/x"),
    /approval\.approved_by$/,
  );
  accepts({ value_domain: "SIMULATED", market, proposal }, voidProposalResultSpec);
  accepts({ ...detail, void_proposals: [proposal] }, marketDetailSpec);
  refuses(() => validated(broken(detail, (c) => delete c["void_proposals"]), marketDetailSpec, "/x"), /void_proposals$/);

  accepts(heldPayout, heldPayoutSpec);
  refuses(() => validated(broken(heldPayout, (c) => (c["amount"] = "2.00")), heldPayoutSpec, "/x"), /amount$/);
  refuses(() => validated(broken(heldPayout, (c) => (c["reason_codes"] = "RISK_ACCOUNT_STATUS")), heldPayoutSpec, "/x"), /reason_codes$/);
  refuses(() => validated(broken(heldPayout, (c) => (c["kind"] = "BONUS")), heldPayoutSpec, "/x"), /kind$/);
  const claimed = {
    ...heldPayout,
    claimed: true,
    claim: { id: ID_A, amount: "2000000", intent_id: ID_B, journal_transaction_id: ID_C, claimed_by: { actor_type: "USER", actor_id: ACCOUNT }, claimed_at: AT },
  };
  accepts(claimed, heldPayoutSpec);
  const claim = {
    value_domain: "SIMULATED",
    market_id: MARKET,
    account_id: ACCOUNT,
    held_payout: claimed,
    replay: false,
    intent: { ...intent, kind: "RESOLVE_PREDICTION_POSITION" },
    journal_transaction_id: ID_C,
    risk_decision_hash: "decision-hash-claim",
  };
  accepts(claim, claimResultSpec);
  refuses(() => validated(broken(claim, (c) => (c["value_domain"] = "REAL")), claimResultSpec, "/x"), /value_domain$/);
  refuses(() => validated(broken(claim, (c) => delete c["held_payout"]), claimResultSpec, "/x"), /held_payout$/);

  accepts({ ...books, held: "2000000" }, booksSpec);
  refuses(() => validated(broken(books, (c) => delete c["held"]), booksSpec, "/x"), /held$/);
  const list = {
    value_domain: "SIMULATED",
    market_id: MARKET,
    account_id: ACCOUNT,
    share_decimals: 6,
    items: [],
    held_payout: heldPayout,
  };
  accepts(list, positionListSpec);
  refuses(() => validated(broken(list, (c) => ((c["held_payout"] as Record<string, unknown>)["claimed"] = "no")), positionListSpec, "/x"), /held_payout\.claimed$/);
});
