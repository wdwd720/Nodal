/**
 * The bridge section's pure logic (lib/bridge.ts) and its runtime contract
 * (api/advanced/bridge.ts).
 *
 * What these hold the section to: a leg's progress claims only what its own
 * transitions record and says so where it cannot tell; a leg with a
 * submission whose outcome is unknown is unknown, never bridged or failed;
 * fault injection is described as the chain host defines it; and every
 * response is refused unless its figures are exact strings in the simulated
 * domain.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  bridgeBlocksResultSpec,
  bridgeChainsSpec,
  bridgeHoldingsSpec,
  bridgeKeys,
  bridgeLegDetailSpec,
  bridgeLegResultSpec,
  bridgeRouteListSpec,
  bridgeRouteReconciliationSpec,
} from "../api/advanced/bridge.ts";
import { advancedKeys } from "../api/advanced/core.ts";
import { ContractViolation, validated } from "../api/contract.ts";
import {
  BRIDGE_FAULT_KINDS,
  BRIDGE_LEG_HOLDINGS,
  BRIDGE_LEG_STATUSES,
  BRIDGE_RECONCILIATION_STATES,
  BRIDGE_ROUTE_STATUSES,
  BRIDGE_TX_STATES,
  FAULT_KIND_MEANING,
  HOLDING_MEANING,
  LEG_MAIN_PATH,
  LEG_STATUS_MEANING,
  RECONCILIATION_STATE_MEANING,
  ROUTE_STATUS_MEANING,
  TX_STATE_MEANING,
  chainIdOf,
  faultTakesCount,
  legOutcome,
  legProgress,
  parseSymbol,
} from "./bridge.ts";

const ACCOUNT = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const ROUTE = "01a075f1-f846-719e-bcbb-493c8281c524";
const LEG = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const SOURCE = "01a075f1-f900-7000-8000-000000000001";
const DESTINATION = "01a075f1-f900-7000-8000-000000000002";
const INTENT = "01a075f1-f900-7000-8000-000000000003";
const AT = "2026-10-08T12:00:00Z";
const HASH = "9f2c4be1d0a7c3e85b16f4a2d9e07c31a5b8f6e2c4d1a9b7e3f05c8d2a6b4e19";
const OWNER = "8kYwPEm1jMtRDcn5U9XdPjYGsKq2cDNe2vWo9Z3Q1hPa";

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
 * Progress
 * ------------------------------------------------------------------------ */

test("a leg's progress marks what is done, where it is and what is ahead", () => {
  const steps = legProgress("MESSAGE_IN_FLIGHT", ["AVAILABLE", "RESERVED", "BRIDGE_REQUESTED", "SOURCE_FINALIZED", "MESSAGE_IN_FLIGHT"]);
  assert.deepEqual(
    steps.map((step) => `${step.status}:${step.state}`),
    [
      "BRIDGE_REQUESTED:done",
      "SOURCE_FINALIZED:done",
      "MESSAGE_IN_FLIGHT:current",
      "DESTINATION_OBSERVED:ahead",
      "DESTINATION_FINALIZED:ahead",
      "RECONCILED:ahead",
    ],
  );
});

test("a release that finalized without being observed first shows the step as not taken", () => {
  const steps = legProgress("RECONCILED", [
    "AVAILABLE",
    "RESERVED",
    "BRIDGE_REQUESTED",
    "SOURCE_FINALIZED",
    "MESSAGE_IN_FLIGHT",
    "DESTINATION_FINALIZED",
    "RECONCILED",
  ]);
  assert.equal(steps.find((step) => step.status === "DESTINATION_OBSERVED")?.state, "skipped");
  assert.equal(steps.find((step) => step.status === "RECONCILED")?.state, "current");
});

test("without the leg's transitions, what cannot be told is said to be not shown", () => {
  const steps = legProgress("RECONCILED", undefined);
  assert.equal(steps.find((step) => step.status === "DESTINATION_OBSERVED")?.state, "unknown");
  assert.equal(steps.find((step) => step.status === "SOURCE_FINALIZED")?.state, "done");
  const failed = legProgress("FAILED", undefined);
  assert.equal(failed.length, LEG_MAIN_PATH.length + 1);
  assert.ok(failed.slice(0, LEG_MAIN_PATH.length).every((step) => step.state === "unknown"));
  assert.deepEqual(failed[failed.length - 1], { status: "FAILED", state: "current" });
});

test("a failed or reviewed leg shows the path its transitions prove, then where it ended", () => {
  const failed = legProgress("FAILED", ["AVAILABLE", "RESERVED", "BRIDGE_REQUESTED", "FAILED"]);
  assert.deepEqual(
    failed.map((step) => `${step.status}:${step.state}`),
    [
      "BRIDGE_REQUESTED:done",
      "SOURCE_FINALIZED:skipped",
      "MESSAGE_IN_FLIGHT:skipped",
      "DESTINATION_OBSERVED:skipped",
      "DESTINATION_FINALIZED:skipped",
      "RECONCILED:skipped",
      "FAILED:current",
    ],
  );
  const review = legProgress("MANUAL_REVIEW", ["BRIDGE_REQUESTED", "SOURCE_FINALIZED", "MESSAGE_IN_FLIGHT", "MANUAL_REVIEW"]);
  assert.equal(review[review.length - 1]?.status, "MANUAL_REVIEW");
  assert.equal(review.find((step) => step.status === "MESSAGE_IN_FLIGHT")?.state, "done");
  assert.ok(legProgress("RESERVED", ["AVAILABLE", "RESERVED"]).every((step) => step.state === "ahead"));
});

/* --------------------------------------------------------------------------
 * Outcomes and meanings
 * ------------------------------------------------------------------------ */

test("a leg with an unknown submission is unknown, never bridged or failed", () => {
  assert.equal(legOutcome("BRIDGE_REQUESTED", "AWAITING"), "UNKNOWN");
  assert.equal(legOutcome("MESSAGE_IN_FLIGHT", "AWAITING"), "UNKNOWN");
  assert.equal(legOutcome("RECONCILED", "RESOLVED"), "COMPLETED");
  assert.equal(legOutcome("FAILED", "NOT_REQUIRED"), "FAILED");
  assert.equal(legOutcome("MANUAL_REVIEW", "NOT_REQUIRED"), "REVIEW");
  assert.equal(legOutcome("SOURCE_FINALIZED", "NOT_REQUIRED"), "IN_PROGRESS");
  assert.match(RECONCILIATION_STATE_MEANING.AWAITING, /not a success and not a failure/);
});

test("every contract value has its meaning, and only two faults take a count", () => {
  for (const status of BRIDGE_ROUTE_STATUSES) assert.ok(ROUTE_STATUS_MEANING[status].length > 5, status);
  for (const status of BRIDGE_LEG_STATUSES) assert.ok(LEG_STATUS_MEANING[status].length > 10, status);
  for (const holding of BRIDGE_LEG_HOLDINGS) assert.ok(HOLDING_MEANING[holding].length > 10, holding);
  for (const state of BRIDGE_TX_STATES) assert.ok(TX_STATE_MEANING[state].length > 10, state);
  for (const state of BRIDGE_RECONCILIATION_STATES) assert.ok(RECONCILIATION_STATE_MEANING[state].length > 10, state);
  for (const kind of BRIDGE_FAULT_KINDS) assert.ok(FAULT_KIND_MEANING[kind].length > 10, kind);
  assert.deepEqual(
    BRIDGE_FAULT_KINDS.filter((kind) => faultTakesCount(kind)),
    ["DROP_NEXT", "DELAY_INCLUSION"],
  );
});

test("a symbol and a chain are checked only for what the page can know", () => {
  assert.deepEqual(parseSymbol("SBX-PUSD.E"), { ok: true, value: "SBX-PUSD.E" });
  for (const bad of ["", " SBX", "SBX ", "S".repeat(33)]) assert.equal(parseSymbol(bad).ok, false, bad);
  assert.equal(chainIdOf("sbx-ethereum"), "sbx-ethereum");
  assert.equal(chainIdOf("ethereum"), undefined);
  assert.equal(chainIdOf(undefined), undefined);
});

/* --------------------------------------------------------------------------
 * The runtime contract
 * ------------------------------------------------------------------------ */

const LEG_BODY = {
  value_domain: "SIMULATED",
  id: LEG,
  route_id: ROUTE,
  account_id: ACCOUNT,
  intent_id: INTENT,
  mode: "SANDBOX",
  quantity: "100000000",
  decimals: 6,
  source_chain: "sbx-solana",
  source_asset_id: SOURCE,
  source_symbol: "SBX-PUSD",
  destination_chain: "sbx-ethereum",
  destination_asset_id: DESTINATION,
  destination_symbol: "SBX-PUSD.E",
  source_owner: OWNER,
  destination_owner: OWNER,
  nonce: "18446744073709551615",
  plan_hash: HASH,
  status: "BRIDGE_REQUESTED",
  holding: "WALLET_SOURCE_RESERVED",
  lock_tx_id: "3uYbM8sVt4kJqLdn7RzWQ1oPfX9eGc2hTnA5mK6yBvZs",
  source_effect_status: "UNKNOWN",
  reconciliation: { state: "AWAITING", unknown_since: AT },
  created_at: AT,
  updated_at: AT,
};

test("a leg whose lock outcome is unknown is accepted as the backend sends it", () => {
  assert.doesNotThrow(() => validated({ value_domain: "SIMULATED", replay: false, settled: false, leg: LEG_BODY }, bridgeLegResultSpec, "/x"));
});

test("a leg is refused unless its figures are exact, its states declared and its domain simulated", () => {
  const result = { value_domain: "SIMULATED", replay: false, settled: false, leg: LEG_BODY };
  refuses(
    () => validated(broken(result, (c) => ((c["leg"] as Record<string, unknown>)["quantity"] = 100)), bridgeLegResultSpec, "/x"),
    /^\/x\.leg\.quantity$/,
  );
  refuses(
    () => validated(broken(result, (c) => ((c["leg"] as Record<string, unknown>)["status"] = "DONE")), bridgeLegResultSpec, "/x"),
    /^\/x\.leg\.status$/,
  );
  refuses(
    () =>
      validated(broken(result, (c) => ((c["leg"] as Record<string, unknown>)["source_effect_status"] = "OK")), bridgeLegResultSpec, "/x"),
    /^\/x\.leg\.source_effect_status$/,
  );
  refuses(
    () => validated(broken(result, (c) => ((c["leg"] as Record<string, unknown>)["value_domain"] = "REAL")), bridgeLegResultSpec, "/x"),
    /^\/x\.leg\.value_domain$/,
  );
});

test("a leg's detail carries the chains' word on each transaction", () => {
  const detail = {
    value_domain: "SIMULATED",
    leg: { ...LEG_BODY, status: "RECONCILED", holding: "WALLET_DESTINATION", reconciliation: { state: "NOT_REQUIRED" } },
    lock: { chain: "sbx-solana", tx_id: "lock-tx", state: "FINALIZED", final: true, height: 12 },
    attempts: [
      {
        number: 1,
        kind: "RELEASE",
        chain: "sbx-ethereum",
        tx_id: "release-tx",
        blockhash_height: 9,
        created_at: AT,
        on_chain: { chain: "sbx-ethereum", tx_id: "release-tx", state: "FINALIZED", final: true, height: 10 },
      },
    ],
    transitions: [
      { from_status: "NONE", to_status: "AVAILABLE", actor_type: "USER", actor_id: "u", reason: "requested", occurred_at: AT },
    ],
  };
  assert.doesNotThrow(() => validated(detail, bridgeLegDetailSpec, "/x"));
  refuses(
    () =>
      validated(
        broken(detail, (c) => {
          const attempts = c["attempts"] as Array<Record<string, unknown>>;
          (attempts[0] as Record<string, unknown>)["on_chain"] = { chain: "sbx-ethereum", tx_id: "t", state: "SETTLED", final: true };
        }),
        bridgeLegDetailSpec,
        "/x",
      ),
    /^\/x\.attempts\[0\]\.on_chain\.state$/,
  );
});

test("holdings by chain and value in flight are exact strings in the simulated domain", () => {
  const holdings = {
    value_domain: "SIMULATED",
    account_id: ACCOUNT,
    as_of: AT,
    chains: [
      { chain: "nodal-sandbox", holdings: [{ asset_id: SOURCE, symbol: "SBX-USD", decimals: 6, quantity: "200000000" }] },
      { chain: "sbx-ethereum", holdings: [{ asset_id: DESTINATION, symbol: "SBX-PUSD.E", decimals: 6, quantity: "100000000" }] },
    ],
    in_flight: [
      { asset_id: SOURCE, symbol: "SBX-PUSD", decimals: 6, quantity: "50000000", marked_usd: "50.00", factor_bps: 0, reason: "not final" },
    ],
    legs: [
      {
        leg_id: LEG,
        route_id: ROUTE,
        asset_id: SOURCE,
        symbol: "SBX-PUSD",
        decimals: 6,
        status: "MESSAGE_IN_FLIGHT",
        holding: "BRIDGE_IN_FLIGHT",
        quantity: "50000000",
        factor_bps: 0,
        reason: "bridge lock final on the source, release not yet observed",
      },
    ],
    pending_locks_usd: "0.00",
    policy: { version: "bridge-haircut-v1", in_flight_bps: 0, observed_bps: 0 },
  };
  assert.doesNotThrow(() => validated(holdings, bridgeHoldingsSpec, "/x"));
  refuses(() => validated(broken(holdings, (c) => (c["pending_locks_usd"] = "0")), bridgeHoldingsSpec, "/x"), /^\/x\.pending_locks_usd$/);
  refuses(
    () =>
      validated(
        broken(holdings, (c) => (c["in_flight"] = [{ ...holdings.in_flight[0], marked_usd: 50 }])),
        bridgeHoldingsSpec,
        "/x",
      ),
    /^\/x\.in_flight\[0\]\.marked_usd$/,
  );
  refuses(
    () => validated(broken(holdings, (c) => (c["legs"] = [{ ...holdings.legs[0], holding: "SOMEWHERE" }])), bridgeHoldingsSpec, "/x"),
    /^\/x\.legs\[0\]\.holding$/,
  );
});

test("routes, chains and a route reconciliation are accepted only in their contract's shape", () => {
  const routes = {
    value_domain: "SIMULATED",
    items: [
      {
        value_domain: "SIMULATED",
        id: ROUTE,
        name: "pusd-solana-to-ethereum",
        status: "ACTIVE",
        network: "nodal-sandbox",
        facility_id: INTENT,
        source_chain: "sbx-solana",
        source_asset_id: SOURCE,
        source_symbol: "SBX-PUSD",
        source_mint: OWNER,
        destination_chain: "sbx-ethereum",
        destination_asset_id: DESTINATION,
        destination_symbol: "SBX-PUSD.E",
        destination_mint: OWNER,
        decimals: 6,
        attester: OWNER,
        unit_price: { mantissa: "1", scale: 0, decimal: "1", quote: "USD" },
        max_release_attempts: 3,
        created_at: AT,
      },
    ],
  };
  assert.doesNotThrow(() => validated(routes, bridgeRouteListSpec, "/x"));
  refuses(
    () => validated(broken(routes, (c) => (c["items"] = [{ ...routes.items[0], status: "OPEN" }])), bridgeRouteListSpec, "/x"),
    /^\/x\.items\[0\]\.status$/,
  );

  const chains = {
    value_domain: "SIMULATED",
    network: "nodal-sandbox",
    fault_injection_allowed: true,
    chains: [
      {
        id: "sbx-solana",
        tip_slot: 40,
        tip_height: 40,
        tip_hash: HASH,
        finalized_height: 8,
        blockhash_window: 150,
        finality_depth: 32,
        fee_per_tx: "5000",
        bridge_authority: OWNER,
      },
    ],
  };
  assert.doesNotThrow(() => validated(chains, bridgeChainsSpec, "/x"));
  refuses(
    () => validated(broken(chains, (c) => (c["chains"] = [{ ...chains.chains[0], id: "mainnet" }])), bridgeChainsSpec, "/x"),
    /^\/x\.chains\[0\]\.id$/,
  );

  // Producing blocks answers with how many were produced and the chain after them, in the simulated domain.
  const blocks = { value_domain: "SIMULATED", produced: 40, chain: chains.chains[0] };
  assert.doesNotThrow(() => validated(blocks, bridgeBlocksResultSpec, "/x"));
  refuses(() => validated(broken(blocks, (c) => delete c["value_domain"]), bridgeBlocksResultSpec, "/x"), /^\/x\.value_domain$/);
  refuses(() => validated(broken(blocks, (c) => (c["produced"] = "40")), bridgeBlocksResultSpec, "/x"), /^\/x\.produced$/);

  const rec = {
    value_domain: "SIMULATED",
    route_id: ROUTE,
    locked: "100000000",
    outstanding: "100000000",
    in_flight: "0",
    refunded: "0",
    escrow: "100000000",
    destination_supply: "100000000",
    landed_unposted_locks: "0",
    landed_unposted_releases: "0",
    expected_escrow: "100000000",
    expected_supply: "100000000",
    books_conserve: true,
    escrow_matches: true,
    supply_matches: true,
    matched: true,
    breaker_tripped: false,
  };
  assert.doesNotThrow(() => validated(rec, bridgeRouteReconciliationSpec, "/x"));
  refuses(() => validated(broken(rec, (c) => (c["escrow"] = "1e8")), bridgeRouteReconciliationSpec, "/x"), /^\/x\.escrow$/);
});

test("every account-scoped bridge key sits under its account's advanced prefix", () => {
  const prefix = advancedKeys.account(ACCOUNT);
  for (const key of [bridgeKeys.holdings(ACCOUNT), bridgeKeys.legs(ACCOUNT, ""), bridgeKeys.leg(ACCOUNT, LEG)]) {
    assert.deepEqual(key.slice(0, prefix.length), [...prefix]);
  }
});
