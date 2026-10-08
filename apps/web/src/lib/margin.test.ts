import assert from "node:assert/strict";
import { test } from "node:test";

import {
  marginDeficitRepaymentResultSpec,
  marginDeficitStatusSpec,
  marginKeys,
  marginResolutionDecisionResultSpec,
  marginReviewQueueSpec,
} from "../api/advanced/margin.ts";
import { ContractViolation, validated, type Spec } from "../api/contract.ts";
import {
  ACCOUNT_STATUS_MEANING,
  CALL_STATUS_MEANING,
  CASE_STATUS_MEANING,
  LEVERAGED_MARGIN_MODES,
  MARGIN_ACCOUNT_STATUSES,
  MARGIN_CALL_STATUSES,
  MARGIN_CASE_STATUSES,
  MARGIN_PLAN_DECISIONS,
  MARGIN_RESOLUTION_DECISIONS,
  MARGIN_RESOLUTION_OUTCOMES,
  MARGIN_STRESS_SCENARIOS,
  PLAN_DECISION_MEANING,
  RESOLUTION_DECISION_MEANING,
  RESOLUTION_OUTCOME_MEANING,
  STRESS_SCENARIO_MEANING,
  accountStatusTone,
  assetById,
  checkResolutionReason,
  confirmAttempt,
  formatLeverage,
  formatMarginPrice,
  isLeveragedMode,
  isOwnProposal,
  marginModeLabel,
  resolutionTone,
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

/* --------------------------------------------------------------------------
 * Leaving MANUAL_REVIEW, and repaying a covered deficit
 * ------------------------------------------------------------------------ */

const CASE_ID = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const MARGIN_ACCOUNT = "01a075f1-f846-719e-bcbb-493c8281c524";
const ACCOUNT = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const NUMERAIRE = "01920000-0000-7000-8000-5342582d5553";
const REVIEW = "01a075f1-f848-7d10-8f3a-1c0e5e2f4a11";
const RESOLUTION = "01a075f1-f849-7c2b-9a4d-2d1f6f3e5b22";

const REVIEW_CASE = {
  id: CASE_ID,
  margin_account_id: MARGIN_ACCOUNT,
  account_id: ACCOUNT,
  margin_call_id: REVIEW,
  status: "MANUAL_REVIEW",
  policy_version: "margin-sandbox-v1",
  target: "INITIAL_MARGIN",
  max_steps: 5,
  steps_executed: 5,
  opened_at: "2026-10-08T10:00:00Z",
  last_reason: "the step budget is spent",
  numeraire_asset_id: NUMERAIRE,
  numeraire_decimals: 6,
} as const;

const PENDING_RESOLUTION = {
  id: RESOLUTION,
  case_id: CASE_ID,
  review_transition_id: REVIEW,
  outcome: "RESUME",
  reason: "the marks are back",
  proposed_by: ACCOUNT,
  proposed_at: "2026-10-08T11:00:00Z",
  pending: true,
} as const;

function refusesSpec(value: unknown, spec: Spec, path: RegExp): void {
  assert.throws(
    () => validated(value, spec, "/t"),
    (error: unknown) => {
      assert.ok(error instanceof ContractViolation, `expected a ContractViolation, got ${String(error)}`);
      assert.match(error.path, path);
      return true;
    },
  );
}

test("the resolution enumerations are named, and a reason is checked only for presence and length", () => {
  assert.deepEqual([...MARGIN_RESOLUTION_OUTCOMES], ["RESUME", "DECLARE_BANKRUPT"]);
  assert.deepEqual([...MARGIN_RESOLUTION_DECISIONS], ["APPROVED", "REJECTED"]);
  for (const outcome of MARGIN_RESOLUTION_OUTCOMES) assert.ok(RESOLUTION_OUTCOME_MEANING[outcome].length > 40, outcome);
  for (const decision of MARGIN_RESOLUTION_DECISIONS) assert.ok(RESOLUTION_DECISION_MEANING[decision].length > 30, decision);
  assert.equal(resolutionTone(undefined), "info");
  assert.equal(resolutionTone("APPROVED"), "warn");
  assert.equal(resolutionTone("REJECTED"), "neutral");
  assert.deepEqual(checkResolutionReason("  marks are back  "), { ok: true, value: "marks are back", error: "" });
  assert.equal(checkResolutionReason("   ").ok, false);
  assert.equal(checkResolutionReason("x".repeat(1000)).ok, true);
  assert.equal(checkResolutionReason("x".repeat(1001)).ok, false);
  assert.equal(isOwnProposal(ACCOUNT, ACCOUNT.toUpperCase()), true);
  assert.equal(isOwnProposal(ACCOUNT, MARGIN_ACCOUNT), false);
  assert.equal(isOwnProposal(ACCOUNT, undefined), false);
});

test("the review queue and a decision are accepted unchanged, and refused when malformed", () => {
  const queue = {
    value_domain: "SIMULATED",
    items: [
      {
        case: REVIEW_CASE,
        current_review_transition_id: REVIEW,
        entered_review_at: "2026-10-08T10:30:00Z",
        resolutions: [PENDING_RESOLUTION],
      },
    ],
  };
  assert.doesNotThrow(() => validated(queue, marginReviewQueueSpec, "/t"));
  refusesSpec({ ...queue, value_domain: "REAL" }, marginReviewQueueSpec, /\.value_domain$/);
  refusesSpec(
    { ...queue, items: [{ ...queue.items[0], resolutions: [{ ...PENDING_RESOLUTION, outcome: "LIQUIDATE" }] }] },
    marginReviewQueueSpec,
    /\.items\[0\]\.resolutions\[0\]\.outcome$/,
  );
  refusesSpec(
    { ...queue, items: [{ ...queue.items[0], resolutions: [{ ...PENDING_RESOLUTION, pending: "yes" }] }] },
    marginReviewQueueSpec,
    /\.items\[0\]\.resolutions\[0\]\.pending$/,
  );

  const decided = {
    value_domain: "SIMULATED",
    resolution: {
      ...PENDING_RESOLUTION,
      pending: false,
      decision: "APPROVED",
      decided_by: MARGIN_ACCOUNT,
      decided_at: "2026-10-08T11:05:00Z",
      decision_reason: "agreed",
    },
    case: { ...REVIEW_CASE, status: "IN_PROGRESS" },
    account_status: "LIQUIDATING",
    waived: [
      { lot_id: REVIEW, period_start: "2026-10-08T00:00:00Z", period_end: "2026-10-08T11:05:00Z", amount: "0", waived: true },
    ],
    replayed: false,
  };
  assert.doesNotThrow(() => validated(decided, marginResolutionDecisionResultSpec, "/t"));
  refusesSpec({ ...decided, account_status: "FINE" }, marginResolutionDecisionResultSpec, /\.account_status$/);
  refusesSpec(
    { ...decided, waived: [{ ...decided.waived[0], amount: "0.00" }] },
    marginResolutionDecisionResultSpec,
    /\.waived\[0\]\.amount$/,
  );
  refusesSpec({ ...decided, resolution: { ...decided.resolution, decision: "MAYBE" } }, marginResolutionDecisionResultSpec, /\.resolution\.decision$/);
});

test("the deficit read carries its figures together, in exact base units", () => {
  const none = {
    value_domain: "SIMULATED",
    account_id: ACCOUNT,
    margin_account_id: MARGIN_ACCOUNT,
    numeraire_asset_id: NUMERAIRE,
    numeraire_decimals: 6,
    repayments: [],
  };
  assert.doesNotThrow(() => validated(none, marginDeficitStatusSpec, "/t"), "no covered deficit is an answer");
  const noMarginAccount = { value_domain: "SIMULATED", account_id: ACCOUNT, repayments: [] };
  assert.doesNotThrow(
    () => validated(noMarginAccount, marginDeficitStatusSpec, "/t"),
    "an account without a margin account owes nothing and is answered without a numeraire",
  );
  refusesSpec({ ...noMarginAccount, account_id: undefined }, marginDeficitStatusSpec, /\.account_id$/);
  refusesSpec({ ...none, numeraire_decimals: "6" }, marginDeficitStatusSpec, /\.numeraire_decimals$/);
  const owed = {
    ...none,
    case: { ...REVIEW_CASE, status: "BANKRUPT", deficit: "150000000", deficit_covered_at: "2026-10-08T12:00:00Z" },
    deficit: "150000000",
    repaid: "50000000",
    outstanding: "100000000",
    repayments: [
      {
        id: RESOLUTION,
        amount: "50000000",
        journal_transaction_id: REVIEW,
        effect_id: CASE_ID,
        intent_id: MARGIN_ACCOUNT,
        repaid_at: "2026-10-08T12:10:00Z",
      },
    ],
  };
  assert.doesNotThrow(() => validated(owed, marginDeficitStatusSpec, "/t"));
  refusesSpec({ ...owed, outstanding: "100.000000" }, marginDeficitStatusSpec, /\.outstanding$/);
  refusesSpec({ ...owed, repayments: undefined }, marginDeficitStatusSpec, /\.repayments$/);
  refusesSpec({ ...owed, value_domain: "REAL" }, marginDeficitStatusSpec, /\.value_domain$/);

  const repaid = {
    value_domain: "SIMULATED",
    margin_account_id: MARGIN_ACCOUNT,
    account_id: ACCOUNT,
    case_id: CASE_ID,
    repayment_id: RESOLUTION,
    intent_id: REVIEW,
    journal_transaction_id: CASE_ID,
    effect_id: MARGIN_ACCOUNT,
    repaid: "50000000",
    outstanding: "100000000",
    numeraire_asset_id: NUMERAIRE,
    numeraire_decimals: 6,
    replayed: false,
  };
  assert.doesNotThrow(() => validated(repaid, marginDeficitRepaymentResultSpec, "/t"));
  refusesSpec({ ...repaid, repaid: 50000000 }, marginDeficitRepaymentResultSpec, /\.repaid$/);
  refusesSpec({ ...repaid, replayed: undefined }, marginDeficitRepaymentResultSpec, /\.replayed$/);
});

test("the deficit key is the account's and the reviews key is the operator's", () => {
  assert.deepEqual(marginKeys.deficit(ACCOUNT).slice(0, 3), ["advanced", "account", ACCOUNT]);
  assert.deepEqual(marginKeys.reviews.slice(0, marginKeys.operator.length), [...marginKeys.operator]);
});
