/**
 * The agent authority section's pure helpers and its runtime contract
 * (src/api/advanced/delegation.ts): each spec is shown a realistic answer,
 * which it must accept unchanged, and then that answer broken one way at a
 * time, which it must refuse with a ContractViolation. A refused answer
 * becomes an explicit error state on the page; an accepted malformed one
 * would become a figure.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  capabilityPageSpec,
  capabilitySpec,
  executionRunSpec,
  policyStateSpec,
  swapDecisionResultSpec,
  swapSpec,
  venueListSpec,
  withdrawalSpec,
} from "../api/advanced/delegation.ts";
import { ContractViolation, validated, type Spec } from "../api/contract.ts";
import {
  ACTION_CLASSES,
  INSTRUMENT_CLASSES,
  awaitsDecision,
  cancellable,
  checkAddress,
  checkAddressList,
  checkGrant,
  checkPolicy,
  checkUsd,
  checkUuid,
  checkWholeNumber,
  confirmAttempt,
  expiryAfterDays,
  grantBody,
  planPriceText,
  policyDraftFrom,
  standingTone,
  statusPath,
  subjectStatusTone,
  toggled,
  type GrantDraft,
} from "./delegation.ts";

const ACCOUNT = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const AGENT = "01a075f1-f846-719e-bcbb-493c8281c524";
const STRATEGY = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const CAPABILITY = "01a075f1-f8aa-7000-8000-00000000c001";
const SWAP = "01a075f1-f8aa-7000-8000-00000000c002";
const WITHDRAWAL = "01a075f1-f8aa-7000-8000-00000000c003";
const OTHER = "01a075f1-f8aa-7000-8000-00000000c004";
const VENUE_ADDRESS = "7sQ4eKXhD3vJdzv1T9QmkJrWzW2k9mH8pq3N4cRbYuZa";
const WALLET = "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin";
const HASH = "9f2c4be1d0a7c3e85b16f4a2d9e07c31a5b8f6e2c4d1a9b7e3f05c8d2a6b4e19";
const NOW = "2026-10-08T12:00:00Z";

/** A deep copy with one change applied, so every case starts from the valid fixture. */
function broken<T>(value: T, change: (copy: Record<string, unknown>) => void): unknown {
  const copy = structuredClone(value) as Record<string, unknown>;
  change(copy);
  return copy;
}

function refuses(value: unknown, spec: Spec, path: string): void {
  assert.throws(
    () => validated(value, spec, "/test"),
    (error: unknown) => error instanceof ContractViolation && error.path.includes(path),
    `expected a contract violation at ${path}`,
  );
}

const actor = { actor_type: "USER", actor_id: AGENT };

const capability = {
  id: CAPABILITY,
  account_id: ACCOUNT,
  agent_id: AGENT,
  agent_name: "momentum agent",
  strategy_id: STRATEGY,
  environment: "TEST",
  mode: "SANDBOX",
  instrument_classes: ["STABLE_VALUE", "SPOT_TOKEN"],
  action_classes: ["ACQUIRE_EXPOSURE", "PROPOSE_WITHDRAWAL"],
  max_notional_usd: "1000.00",
  max_daily_notional_usd: "500.00",
  max_single_trade_usd: "100.00",
  destination_allowlist: [VENUE_ADDRESS],
  chain_allowlist: ["sbx-solana"],
  granted_at: NOW,
  expires_at: "2026-10-09T12:00:00Z",
  requires_human_approval: true,
  status: "ACTIVE",
  standing: "ACTIVE",
  content_hash: HASH,
  granted_by: actor,
  usage: {
    day: "2026-10-08",
    uses: 2,
    used_today_usd: "40.00",
    used_total_usd: "140.00",
    remaining_today_usd: "460.00",
    remaining_total_usd: "860.00",
  },
  as_of: NOW,
  value_domain: "SIMULATED",
};

const price = { mantissa: "10000", scale: "2", source: "test-oracle", observed_at: NOW, par: false };

const swap = {
  id: SWAP,
  capability_id: CAPABILITY,
  account_id: ACCOUNT,
  agent_id: AGENT,
  request_nonce: "7",
  intent_id: OTHER,
  reservation_id: OTHER,
  venue: { id: OTHER, name: "sandbox-solana", address: VENUE_ADDRESS },
  mode: "SANDBOX",
  action_class: "ACQUIRE_EXPOSURE",
  chain: "sbx-solana",
  wallet: WALLET,
  pay_asset: { id: OTHER, symbol: "SBXUSD", decimals: 6 },
  receive_asset: { id: OTHER, symbol: "SBXSOL", decimals: 9 },
  amount_in: "100000000",
  amount_out: "995000000",
  min_amount_out: "985050000",
  notional_usd: "100.00",
  requested_at: NOW,
  quote_expires_at: NOW,
  requires_approval: true,
  plan: {
    version: "nodal-sandbox/delegated-swap-plan/v1",
    swap_id: SWAP,
    account_id: ACCOUNT,
    agent_id: AGENT,
    capability_id: CAPABILITY,
    capability_hash: HASH,
    intent_id: OTHER,
    request_nonce: "7",
    action_class: "ACQUIRE_EXPOSURE",
    mode: "SANDBOX",
    chain: "sbx-solana",
    wallet: WALLET,
    venue_id: OTHER,
    venue_address: VENUE_ADDRESS,
    pay_asset_id: OTHER,
    pay_mint: WALLET,
    pay_decimals: "6",
    receive_asset_id: OTHER,
    receive_mint: WALLET,
    receive_decimals: "9",
    amount_in: "100000000",
    amount_out: "995000000",
    min_amount_out: "985050000",
    pay_price: { ...price, mantissa: "1", scale: "0", par: true },
    receive_price: price,
    spread_bps: "50",
    max_slippage_bps: "100",
    notional_usd: "100.00",
    fee_per_tx: "10",
    requested_at: NOW,
    quote_expires_at: NOW,
  },
  plan_hash: HASH,
  status: "SETTLED",
  tx_id: "5GtTx",
  journal_transaction_id: OTHER,
  inspection: {
    id: OTHER,
    verdict: "APPROVED",
    reason_codes: [],
    signer_public_key: WALLET,
    capability_hash: HASH,
    plan_hash: HASH,
    message_sha256: HASH,
    policy_version: "delegated-signing-v1",
    policy_hash: HASH,
    inspector_version: "delegation-inspector/1",
    decided_at: NOW,
  },
  transitions: [{ from_status: "NONE", to_status: "ADMITTED", actor: { actor_type: "AGENT", actor_id: AGENT }, reason: "admitted", occurred_at: NOW }],
  created_at: NOW,
  updated_at: NOW,
  value_domain: "SIMULATED",
};

const withdrawal = {
  id: WITHDRAWAL,
  capability_id: CAPABILITY,
  account_id: ACCOUNT,
  agent_id: AGENT,
  proposal_nonce: "3",
  intent_id: OTHER,
  reservation_id: OTHER,
  policy_id: OTHER,
  destination: { id: OTHER, label: "cold wallet", status: "ACTIVE" },
  environment: "TEST",
  mode: "SANDBOX",
  chain: "sbx-solana",
  wallet: WALLET,
  destination_address: VENUE_ADDRESS,
  asset: { id: OTHER, symbol: "SBXUSD", decimals: 6 },
  amount: "25000000",
  notional_usd: "25.00",
  proposed_at: NOW,
  approval_expires_at: NOW,
  executable_at: NOW,
  evaluation: {
    outcome: "HUMAN_REQUIRED",
    risk_score: 30,
    reason_codes: ["HUMAN_AUTO_APPROVAL_DISABLED"],
    anomalies: [],
    destination_age_seconds: 86400,
    account_age_seconds: 172800,
    day_total_before_usd: "12.50",
    count_last_24h: 1,
    policy_version: "2",
    policy_hash: HASH,
    environment: "TEST",
    compliance_clear: true,
  },
  plan: {
    version: "nodal-sandbox/delegated-withdrawal-plan/v1",
    withdrawal_id: WITHDRAWAL,
    account_id: ACCOUNT,
    agent_id: AGENT,
    capability_id: CAPABILITY,
    capability_hash: HASH,
    intent_id: OTHER,
    proposal_nonce: "3",
    mode: "SANDBOX",
    chain: "sbx-solana",
    wallet: WALLET,
    destination_id: OTHER,
    destination_address: VENUE_ADDRESS,
    asset_id: OTHER,
    mint: WALLET,
    decimals: "6",
    amount: "25000000",
    notional_usd: "25.00",
    price: { ...price, mantissa: "1", scale: "0", par: true },
    policy_id: OTHER,
    policy_version: "2",
    policy_hash: HASH,
    evaluation_outcome: "HUMAN_REQUIRED",
    risk_score: "30",
    fee_per_tx: "10",
    proposed_at: NOW,
    executable_at: NOW,
    approval_expires_at: NOW,
  },
  plan_hash: HASH,
  status: "AWAITING_APPROVAL",
  transitions: [],
  created_at: NOW,
  updated_at: NOW,
  value_domain: "SIMULATED",
};

/* --------------------------------------------------------------------------
 * The runtime contract
 * ------------------------------------------------------------------------ */

test("a capability is accepted as the backend sends it, and refused when it says anything else", () => {
  assert.doesNotThrow(() => validated(capability, capabilitySpec, "/test"));
  assert.doesNotThrow(() =>
    validated({ items: [capability], next_cursor: null, value_domain: "SIMULATED" }, capabilityPageSpec, "/test"),
  );
  refuses(broken(capability, (c) => (c["max_single_trade_usd"] = "100.5")), capabilitySpec, "max_single_trade_usd");
  refuses(broken(capability, (c) => (c["value_domain"] = "LIVE")), capabilitySpec, "value_domain");
  refuses(broken(capability, (c) => (c["mode"] = "LIVE")), capabilitySpec, "mode");
  refuses(broken(capability, (c) => (c["standing"] = "PAUSED")), capabilitySpec, "standing");
  refuses(broken(capability, (c) => (c["instrument_classes"] = ["BONDS"])), capabilitySpec, "instrument_classes");
  refuses(broken(capability, (c) => (c["chain_allowlist"] = ["mainnet"])), capabilitySpec, "chain_allowlist");
  refuses(
    broken(capability, (c) => ((c["usage"] as Record<string, unknown>)["remaining_today_usd"] = 460)),
    capabilitySpec,
    "remaining_today_usd",
  );
  refuses(broken(capability, (c) => delete c["usage"]), capabilitySpec, "usage");
  refuses({ items: [capability], value_domain: "SIMULATED" }, capabilityPageSpec, "next_cursor");
});

test("a signing request is accepted with its plan, inspection and trail, and refused when any of it is malformed", () => {
  assert.doesNotThrow(() => validated(swap, swapSpec, "/test"));
  refuses(broken(swap, (s) => (s["amount_in"] = "100.0")), swapSpec, "amount_in");
  refuses(broken(swap, (s) => (s["status"] = "PAID")), swapSpec, "status");
  refuses(broken(swap, (s) => (s["notional_usd"] = "100")), swapSpec, "notional_usd");
  refuses(broken(swap, (s) => ((s["plan"] as Record<string, unknown>)["notional_usd"] = "1e2")), swapSpec, "plan.notional_usd");
  refuses(broken(swap, (s) => ((s["inspection"] as Record<string, unknown>)["verdict"] = "MAYBE")), swapSpec, "inspection.verdict");
  refuses(broken(swap, (s) => delete s["transitions"]), swapSpec, "transitions");
  assert.doesNotThrow(() =>
    validated(
      { swap, execution: { attempted: true, error: { code: "SIGNING_REJECTED", reason: "DELEGATION_QUOTE_EXPIRED", detail: "expired" } }, value_domain: "SIMULATED" },
      swapDecisionResultSpec,
      "/test",
    ),
  );
  refuses({ swap, execution: { attempted: "yes" }, value_domain: "SIMULATED" }, swapDecisionResultSpec, "attempted");
});

test("a withdrawal is accepted with its evaluation, and refused when its figures or statuses are not the contract's", () => {
  assert.doesNotThrow(() => validated(withdrawal, withdrawalSpec, "/test"));
  refuses(broken(withdrawal, (w) => (w["status"] = "PAID")), withdrawalSpec, "status");
  refuses(
    broken(withdrawal, (w) => ((w["evaluation"] as Record<string, unknown>)["day_total_before_usd"] = "1250")),
    withdrawalSpec,
    "day_total_before_usd",
  );
  refuses(broken(withdrawal, (w) => ((w["evaluation"] as Record<string, unknown>)["outcome"] = "REJECT")), withdrawalSpec, "outcome");
  refuses(broken(withdrawal, (w) => ((w["destination"] as Record<string, unknown>)["status"] = "GONE")), withdrawalSpec, "destination.status");
});

test("the policy state, venue list and execution run hold their shapes", () => {
  assert.doesNotThrow(() => validated({ account_id: ACCOUNT, versions: [], value_domain: "SIMULATED" }, policyStateSpec, "/test"));
  refuses({ account_id: ACCOUNT, value_domain: "SIMULATED" }, policyStateSpec, "versions");
  assert.doesNotThrow(() =>
    validated(
      { items: [], co_signers: [{ name: "sandbox-solana", chain: "sbx-solana", address: VENUE_ADDRESS, registered: false }] },
      venueListSpec,
      "/test",
    ),
  );
  refuses({ items: [], co_signers: [{ name: "x", chain: "sbx-bitcoin", address: VENUE_ADDRESS, registered: false }] }, venueListSpec, "chain");
  assert.doesNotThrow(() =>
    validated(
      {
        items: [{ kind: "SWAP", id: SWAP, account_id: ACCOUNT, status_before: "ADMITTED", status: "SETTLED" }],
        value_domain: "SIMULATED",
      },
      executionRunSpec,
      "/test",
    ),
  );
  refuses({ items: [{ kind: "TRADE", id: SWAP, account_id: ACCOUNT, status_before: "A", status: "B" }], value_domain: "SIMULATED" }, executionRunSpec, "kind");
});

/* --------------------------------------------------------------------------
 * What a person typed
 * ------------------------------------------------------------------------ */

test("a sandbox-dollar amount is taken exactly as typed, with exactly two digits of cents", () => {
  assert.deepEqual(checkUsd("250.00", "a limit"), { ok: true, value: "250.00", error: "" });
  assert.equal(checkUsd(" 1,000.00 ", "a limit").value, "1000.00");
  for (const bad of ["", "250", "250.5", "250.000", "-1.00", "1e3", "two hundred"]) {
    assert.equal(checkUsd(bad, "a limit").ok, false, bad);
  }
});

test("addresses are simulated-chain base58 and a list keeps its order without repeats", () => {
  assert.equal(checkAddress(VENUE_ADDRESS).ok, true);
  assert.equal(checkAddress("0OIl" + VENUE_ADDRESS.slice(4)).ok, false, "0, O, I and l are not base58");
  assert.equal(checkAddress("short").ok, false);
  const list = checkAddressList(`${VENUE_ADDRESS}\n${WALLET}, ${VENUE_ADDRESS}`);
  assert.deepEqual(list.value, [VENUE_ADDRESS, WALLET]);
  assert.equal(checkAddressList("").ok, false);
  assert.equal(checkAddressList(`${VENUE_ADDRESS} nope`).ok, false);
});

test("whole numbers are read digit by digit within their bounds, and ids must be canonical", () => {
  assert.deepEqual(checkWholeNumber("900", 1, 86400, "seconds"), { ok: true, value: 900, error: "" });
  assert.equal(checkWholeNumber("0", 1, 50, "a limit").ok, false);
  assert.equal(checkWholeNumber("51", 1, 50, "a limit").ok, false);
  assert.equal(checkWholeNumber("1.5", 1, 50, "a limit").ok, false);
  assert.equal(checkUuid(AGENT.toUpperCase(), "the agent").value, AGENT);
  assert.equal(checkUuid("agent-7", "the agent").ok, false);
});

test("a grant is built exactly as typed, its expiry counted in whole days from now", () => {
  const draft: GrantDraft = {
    agentId: AGENT,
    mode: "SANDBOX",
    instrumentClasses: ["STABLE_VALUE"],
    actionClasses: ["ACQUIRE_EXPOSURE", "PROPOSE_WITHDRAWAL"],
    maxNotional: "1000.00",
    maxDailyNotional: "500.00",
    maxSingleTrade: "100.00",
    destinations: `${VENUE_ADDRESS}\n${WALLET}`,
    chains: ["sbx-solana"],
    days: "7",
    requiresHumanApproval: true,
  };
  assert.equal(checkGrant(draft).ok, true);
  const body = grantBody(ACCOUNT, draft, new Date(NOW));
  assert.deepEqual(body, {
    account_id: ACCOUNT,
    agent_id: AGENT,
    mode: "SANDBOX",
    instrument_classes: ["STABLE_VALUE"],
    action_classes: ["ACQUIRE_EXPOSURE", "PROPOSE_WITHDRAWAL"],
    max_notional_usd: "1000.00",
    max_daily_notional_usd: "500.00",
    max_single_trade_usd: "100.00",
    destination_allowlist: [VENUE_ADDRESS, WALLET],
    chain_allowlist: ["sbx-solana"],
    expires_at: "2026-10-15T12:00:00.000Z",
    requires_human_approval: true,
  });
  const empty = checkGrant({ ...draft, agentId: "", actionClasses: [], maxSingleTrade: "100", days: "91" });
  assert.equal(empty.ok, false);
  assert.deepEqual(Object.keys(empty.problems).sort(), ["actionClasses", "agent", "days", "maxSingleTrade"]);
  assert.equal(expiryAfterDays(1, new Date(NOW)), "2026-10-09T12:00:00.000Z");
});

test("a policy is built from what was typed, and every unready field is named", () => {
  const draft = { ...policyDraftFrom(undefined), maxRiskScore: "80", perWithdrawalLimit: "250.00", dailyLimit: "1000.00", velocityMaxCount: "10", autoApproveMax: "0.00" };
  const ok = checkPolicy(ACCOUNT, draft);
  assert.equal(ok.ok, true);
  assert.equal(ok.value?.strong_auth_max_age_seconds, 900);
  assert.equal(ok.value?.per_withdrawal_limit_usd, "250.00");
  const bad = checkPolicy(ACCOUNT, { ...draft, coolingPeriodSeconds: "604801", dailyLimit: "1000" });
  assert.equal(bad.ok, false);
  assert.deepEqual(Object.keys(bad.problems).sort(), ["coolingPeriodSeconds", "dailyLimit"]);
});

test("a confirmation reuses its attempt's key for the same request and starts a new one for any other", () => {
  let n = 0;
  const fresh = (): string => `key-${String((n = n + 1))}`;
  const first = confirmAttempt(undefined, { id: SWAP, reason: "ok" }, fresh);
  assert.equal(confirmAttempt(first, { id: SWAP, reason: "ok" }, fresh).idempotencyKey, first.idempotencyKey);
  assert.notEqual(confirmAttempt(first, { id: SWAP, reason: "changed" }, fresh).idempotencyKey, first.idempotencyKey);
});

/* --------------------------------------------------------------------------
 * Reading what the backend said
 * ------------------------------------------------------------------------ */

test("what may be asked of a request follows its status alone", () => {
  assert.equal(awaitsDecision("AWAITING_APPROVAL"), true);
  assert.equal(awaitsDecision("APPROVED"), false);
  assert.equal(cancellable("APPROVED"), true);
  assert.equal(cancellable("SIGNED"), false);
  assert.equal(subjectStatusTone("SETTLED"), "good");
  assert.equal(subjectStatusTone("REJECTED"), "bad");
  assert.equal(standingTone("EXPIRED"), "warn");
});

test("lists keep the contract's order, and plans read as they were hashed", () => {
  assert.deepEqual(toggled(ACTION_CLASSES, ["PROPOSE_WITHDRAWAL"], "ACQUIRE_EXPOSURE"), ["ACQUIRE_EXPOSURE", "PROPOSE_WITHDRAWAL"]);
  assert.deepEqual(toggled(INSTRUMENT_CLASSES, ["STABLE_VALUE", "SPOT_TOKEN"], "STABLE_VALUE"), ["SPOT_TOKEN"]);
  assert.equal(planPriceText({ mantissa: "10000", scale: "2", par: false }), "10000 × 10^-2");
  assert.equal(planPriceText({ mantissa: "1", scale: "0", par: true }), "1 (par: the registry's USD peg)");
  assert.deepEqual(statusPath(swap.transitions), ["ADMITTED"]);
});
