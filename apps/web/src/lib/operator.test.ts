/**
 * The operator console's pure helpers (src/lib/operator.ts) and its runtime
 * contract (src/api/advanced/operator.ts).
 *
 * Each spec is shown a realistic answer, which it must accept unchanged, and
 * then that answer broken one way at a time, which it must refuse with a
 * ContractViolation naming the field: a refused answer becomes an explicit
 * error state on the console, an accepted malformed one would become a figure.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { advancedKeys } from "../api/advanced/core.ts";
import {
  opsChainsSpec,
  operatorKeys,
  opsOverviewSpec,
  referenceMarkListSpec,
  referenceMarkRecordedSpec,
} from "../api/advanced/operator.ts";
import { ContractViolation, validated, type Spec } from "../api/contract.ts";
import {
  ADVANCED_RECONCILIATION_KINDS,
  BRIDGE_LEG_ACTIONS,
  MARK_MAX_FRACTION_DIGITS,
  alertTone,
  awaitingSecondOperator,
  bridgeLegOffers,
  bridgeStatusTone,
  checkMarkPrice,
  checkOperatorReason,
  checkMarkSource,
  checkObservedAt,
  formatAge,
  formatDuration,
  oracleOrder,
  reconciliationKindMeaning,
  reconciliationKindTone,
  reconciliationStatusTone,
  stableStatusTone,
} from "./operator.ts";

const ASSET = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const SANDBOX_DOLLAR = "01920000-0000-7000-8000-5342582d5553";
const MARK = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const STABLE = "01a075f1-f846-719e-bcbb-493c8281c524";

function broken<T>(value: T, change: (copy: Record<string, unknown>) => void): unknown {
  const copy = structuredClone(value) as Record<string, unknown>;
  change(copy);
  return copy;
}

function refuses(value: unknown, spec: Spec, path: RegExp): void {
  assert.throws(
    () => validated(value, spec, "/t"),
    (error: unknown) => {
      assert.ok(error instanceof ContractViolation, `expected a ContractViolation, got ${String(error)}`);
      assert.match(error.path, path);
      return true;
    },
  );
}

/* --------------------------------------------------------------------------
 * What an operator types
 * ------------------------------------------------------------------------ */

test("a reference price is sent exactly as typed when it is a plain positive decimal", () => {
  for (const [raw, sent] of [
    ["2500", "2500"],
    [" 2500.125 ", "2500.125"],
    ["0.000000000000000001", "0.000000000000000001"],
    ["00012.50", "00012.50"],
  ] as const) {
    const check = checkMarkPrice(raw);
    assert.equal(check.ok, true, raw);
    assert.equal(check.value, sent, "never rounded, reformatted or converted");
  }
});

test("a reference price that is not positive, not plain or over-precise is refused before it is sent", () => {
  for (const raw of ["", "  ", "0", "0.000", "-1", "+1", "1e3", "1,000.5", "1.2.3", ".5", "5.", "NaN", "Infinity"]) {
    const check = checkMarkPrice(raw);
    assert.equal(check.ok, false, raw);
    assert.ok(check.error.length > 10, raw);
  }
  const limit = `1.${"1".repeat(MARK_MAX_FRACTION_DIGITS)}`;
  assert.equal(checkMarkPrice(limit).ok, true, "exactly 18 fractional digits is accepted");
  const tooFine = `1.${"1".repeat(MARK_MAX_FRACTION_DIGITS + 1)}`;
  const refused = checkMarkPrice(tooFine);
  assert.equal(refused.ok, false);
  assert.match(refused.error, /18 fractional digits/);
  assert.equal(checkMarkPrice("9".repeat(39)).ok, false, "a mantissa wider than numeric(38,0)");
  assert.equal(checkMarkPrice(`000${"9".repeat(38)}`).ok, true, "leading zeros are not significant");
});

test("a source label is required and bounded", () => {
  assert.deepEqual(checkMarkSource("  sandbox-desk  "), { ok: true, value: "sandbox-desk", error: "" });
  assert.equal(checkMarkSource("   ").ok, false);
  assert.equal(checkMarkSource("x".repeat(100)).ok, true);
  assert.equal(checkMarkSource("x".repeat(101)).ok, false);
});

test("an observation time is optional, and an entered one becomes an RFC 3339 instant", () => {
  assert.deepEqual(checkObservedAt(""), { ok: true, value: undefined, error: "" });
  assert.deepEqual(checkObservedAt("   "), { ok: true, value: undefined, error: "" });
  const utc = checkObservedAt("2026-10-08T12:34:56Z");
  assert.equal(utc.ok, true);
  assert.equal(utc.value, "2026-10-08T12:34:56.000Z");
  const local = checkObservedAt("2026-10-08T12:34:56");
  assert.equal(local.ok, true, "a datetime-local value is read in the browser's zone");
  assert.match(local.value ?? "", /^2026-10-0[789]T[0-9]{2}:[0-9]{2}:56\.000Z$/);
  assert.equal(checkObservedAt("yesterday").ok, false);
  assert.equal(checkObservedAt("2026-13-45T99:99").ok, false);
});

/* --------------------------------------------------------------------------
 * Durations and statuses the backend reported
 * ------------------------------------------------------------------------ */

test("a duration is rendered from whole milliseconds without floating point", () => {
  assert.equal(formatDuration(0), "0 ms");
  assert.equal(formatDuration(850), "850 ms");
  assert.equal(formatDuration(1_000), "1 s");
  assert.equal(formatDuration(45_999), "45 s");
  assert.equal(formatDuration(300_000), "5 min");
  assert.equal(formatDuration(3_725_000), "1 h 2 min");
  assert.equal(formatDuration(90_061_000), "1 d 1 h");
  assert.equal(formatDuration(86_400_000 + 5_000), "1 d", "only the two largest units, and a zero unit is skipped");
  assert.match(formatDuration(-1), /not a whole, non-negative/);
  assert.match(formatDuration(1.5), /not a whole, non-negative/);
});

test("an age says ago, and a future observation says so", () => {
  assert.equal(formatAge(65_000), "1 min 5 s ago");
  assert.equal(formatAge(-120_000), "2 min in the future");
  assert.equal(formatAge(0), "0 ms ago");
  assert.match(formatAge(2.5), /did not report as a whole number/);
});

test("statuses other subsystems store are toned, and unknown ones stay neutral", () => {
  assert.equal(stableStatusTone("ACTIVE"), "good");
  assert.equal(stableStatusTone("PAUSED"), "bad");
  assert.equal(stableStatusTone("RETIRED"), "neutral");
  assert.equal(stableStatusTone("SOMETHING"), "neutral");
  assert.equal(bridgeStatusTone("ACTIVE"), "good");
  assert.equal(bridgeStatusTone("PAUSED"), "warn");
  assert.equal(bridgeStatusTone("CLOSED"), "neutral");
  assert.equal(alertTone("WARNING", false), "warn");
  assert.equal(alertTone("WARNING", true), "bad", "a tripped breaker is never a mere warning");
  assert.equal(alertTone("CRITICAL", false), "bad");
  for (const status of ["OPEN", "MISMATCH", "INVESTIGATING", "ESCALATED"]) {
    assert.equal(reconciliationStatusTone(status), "bad", status);
  }
  for (const status of ["MATCHED", "RESOLVED_AUTOMATIC", "RESOLVED_MANUAL"]) {
    assert.equal(reconciliationStatusTone(status), "good", status);
  }
  assert.equal(reconciliationKindTone({ unresolved: 0, blocks_new_risk: 0 }), "good");
  assert.equal(reconciliationKindTone({ unresolved: 2, blocks_new_risk: 0 }), "warn");
  assert.equal(reconciliationKindTone({ unresolved: 2, blocks_new_risk: 1 }), "bad");
  for (const kind of ADVANCED_RECONCILIATION_KINDS) {
    assert.ok(reconciliationKindMeaning(kind).length > 20, kind);
  }
  assert.match(reconciliationKindMeaning("EXECUTION"), /no description/);
});

test("the oracle rows that need attention come first, without reordering the input", () => {
  const rows = [
    { symbol: "B", stale: false, age_ms: 1_000 },
    { symbol: "A", stale: true, age_ms: 900_000 },
    { symbol: "C", stale: true },
    { symbol: "D", stale: true, age_ms: 1_200_000 },
    { symbol: "E", stale: false, age_ms: 5_000 },
  ];
  const before = rows.map((row) => row.symbol).join("");
  assert.deepEqual(
    oracleOrder(rows).map((row) => row.symbol),
    ["C", "D", "A", "E", "B"],
    "no mark first, then the oldest stale mark, then fresh ones oldest first",
  );
  assert.equal(rows.map((row) => row.symbol).join(""), before);
});

test("only an undecided resolution of the current review awaits a second operator", () => {
  const items = [
    {
      current_review_transition_id: "r2",
      resolutions: [
        { id: "old", pending: true, review_transition_id: "r1" },
        { id: "decided", pending: false, review_transition_id: "r2" },
        { id: "waiting", pending: true, review_transition_id: "r2" },
      ],
    },
    { current_review_transition_id: "r9", resolutions: [] },
  ];
  assert.deepEqual(
    awaitingSecondOperator(items).map((entry) => entry.resolution.id),
    ["waiting"],
  );
});

/* --------------------------------------------------------------------------
 * The contract
 * ------------------------------------------------------------------------ */

const MARK_BODY = {
  id: MARK,
  asset_id: ASSET,
  quote_asset_id: SANDBOX_DOLLAR,
  price: "2500.125",
  mantissa: "2500125",
  scale: 3,
  source: "sandbox-desk",
  observed_at: "2026-10-08T12:00:00Z",
  received_at: "2026-10-08T12:00:01Z",
  age_ms: 61000,
  stale: false,
  future: false,
} as const;

test("a recorded mark and a list of marks are accepted unchanged", () => {
  const recorded = {
    mark: MARK_BODY,
    asset_symbol: "SBX-ETH",
    quote_asset_symbol: "SBX-USD",
    duplicate: false,
    max_age_ms: 300000,
    value_domain: "SIMULATED",
  };
  assert.doesNotThrow(() => validated(recorded, referenceMarkRecordedSpec, "/t"));
  const list = {
    asset_id: ASSET,
    asset_symbol: "SBX-ETH",
    asset_kind: "SIMULATED",
    quote_asset_id: SANDBOX_DOLLAR,
    quote_asset_symbol: "SBX-USD",
    max_age_ms: 300000,
    as_of: "2026-10-08T12:01:01Z",
    items: [MARK_BODY, { ...MARK_BODY, id: STABLE, age_ms: -5000, future: true }],
    value_domain: "SIMULATED",
  };
  assert.doesNotThrow(() => validated(list, referenceMarkListSpec, "/t"), "a negative age is a future observation");

  refuses(broken(recorded, (c) => delete c["value_domain"]), referenceMarkRecordedSpec, /\.value_domain$/);
  refuses(
    broken(recorded, (c) => (c["mark"] = { ...MARK_BODY, price: 2500.125 })),
    referenceMarkRecordedSpec,
    /\.mark\.price$/,
  );
  refuses(
    broken(recorded, (c) => (c["mark"] = { ...MARK_BODY, price: "2.5e3" })),
    referenceMarkRecordedSpec,
    /\.mark\.price$/,
  );
  refuses(
    broken(recorded, (c) => (c["mark"] = { ...MARK_BODY, mantissa: "2500.125" })),
    referenceMarkRecordedSpec,
    /\.mark\.mantissa$/,
  );
  refuses(
    broken(recorded, (c) => (c["mark"] = { ...MARK_BODY, age_ms: "61000" })),
    referenceMarkRecordedSpec,
    /\.mark\.age_ms$/,
  );
  refuses(broken(list, (c) => delete c["items"]), referenceMarkListSpec, /\.items$/);
  refuses(broken(list, (c) => (c["value_domain"] = "REAL")), referenceMarkListSpec, /\.value_domain$/);
});

const OVERVIEW = {
  generated_at: "2026-10-08T12:00:00Z",
  environment: "LOCAL",
  max_mark_age_ms: 300000,
  kill_switches: [
    { kind: "CAPABILITY_DISABLE", scope_id: "MARGIN", severity: "SEVERE", reason: "drill", activated_at: "2026-10-08T11:00:00Z" },
  ],
  stable_assets: [
    {
      id: STABLE,
      name: "sbx-dollar",
      asset_id: ASSET,
      chain_id: "sbx-solana",
      status: "PAUSED",
      latest_alert: { kind: "PRICE_DEVIATION", severity: "CRITICAL", breaker_tripped: true, observed_at: "2026-10-08T11:30:00Z" },
      latest_supply_reconciliation: {
        id: MARK,
        kind: "STABLE_RESERVE",
        status: "MISMATCH",
        material: true,
        blocks_new_risk: true,
        opened_at: "2026-10-08T11:31:00Z",
      },
    },
  ],
  bridge_routes: [
    { id: MARK, name: "sol-to-eth", source_chain: "sbx-solana", destination_chain: "sbx-ethereum", status: "ACTIVE" },
  ],
  bridge_legs_in_review: [
    {
      id: STABLE,
      route_id: MARK,
      route_name: "sol-to-eth",
      account_id: ASSET,
      quantity: "2500000",
      decimals: 6,
      source_symbol: "SBX-USD",
      released: false,
      refund_requested: false,
      entered_review_at: "2026-10-08T11:00:00Z",
      review_reason: "every release attempt was refused",
    },
    { id: MARK, route_id: MARK, route_name: "sol-to-eth", account_id: ASSET, quantity: "1", decimals: 6, source_symbol: "SBX-USD", released: true, refund_requested: false },
  ],
  reconciliation: [
    { kind: "STABLE_RESERVE", unresolved: 1, material: 1, blocks_new_risk: 1, oldest_opened_at: "2026-10-08T11:31:00Z", oldest_age_ms: 1740000 },
    { kind: "BRIDGE", unresolved: 0, material: 0, blocks_new_risk: 0 },
  ],
  dual_control: { transfer_reversals_pending: 2, margin_cases_in_review: 1, margin_resolutions_pending: 0 },
  oracle: [
    { asset_id: ASSET, symbol: "SBX-ETH", stale: true },
    { asset_id: STABLE, symbol: "SBX-SOL", stale: false, observed_at: "2026-10-08T11:59:00Z", source: "desk", age_ms: 60000 },
  ],
  chains_configured: false,
  value_domain: "SIMULATED",
} as const;

test("the overview is accepted unchanged, and a malformed part of it is refused by name", () => {
  assert.doesNotThrow(() => validated(OVERVIEW, opsOverviewSpec, "/t"));
  assert.doesNotThrow(() =>
    validated(
      broken(OVERVIEW, (c) => {
        c["kill_switches"] = [];
        c["stable_assets"] = [];
        c["bridge_routes"] = [];
        c["bridge_legs_in_review"] = [];
        c["oracle"] = [];
      }),
      opsOverviewSpec,
      "/t",
    ),
    "empty lists are answers",
  );
  refuses(broken(OVERVIEW, (c) => delete c["dual_control"]), opsOverviewSpec, /\.dual_control$/);
  refuses(
    broken(OVERVIEW, (c) => (c["dual_control"] = { transfer_reversals_pending: "2", margin_cases_in_review: 1, margin_resolutions_pending: 0 })),
    opsOverviewSpec,
    /\.dual_control\.transfer_reversals_pending$/,
  );
  refuses(broken(OVERVIEW, (c) => delete c["reconciliation"]), opsOverviewSpec, /\.reconciliation$/);
  refuses(
    broken(OVERVIEW, (c) => (c["oracle"] = [{ asset_id: ASSET, symbol: "SBX-ETH" }])),
    opsOverviewSpec,
    /\.oracle\[0\]\.stale$/,
  );
  refuses(
    broken(OVERVIEW, (c) => (c["stable_assets"] = [{ ...OVERVIEW.stable_assets[0], latest_alert: { kind: "X", severity: "CRITICAL", observed_at: "2026-10-08T11:30:00Z" } }])),
    opsOverviewSpec,
    /\.stable_assets\[0\]\.latest_alert\.breaker_tripped$/,
  );
  refuses(broken(OVERVIEW, (c) => (c["value_domain"] = "REAL")), opsOverviewSpec, /\.value_domain$/);
  refuses(broken(OVERVIEW, (c) => delete c["chains_configured"]), opsOverviewSpec, /\.chains_configured$/);
  refuses(broken(OVERVIEW, (c) => delete c["bridge_legs_in_review"]), opsOverviewSpec, /\.bridge_legs_in_review$/);
  const leg = OVERVIEW.bridge_legs_in_review[0];
  refuses(
    broken(OVERVIEW, (c) => (c["bridge_legs_in_review"] = [{ ...leg, quantity: "2.5" }])),
    opsOverviewSpec,
    /\.bridge_legs_in_review\[0\]\.quantity$/,
  );
  refuses(
    broken(OVERVIEW, (c) => (c["bridge_legs_in_review"] = [{ ...leg, released: "no" }])),
    opsOverviewSpec,
    /\.bridge_legs_in_review\[0\]\.released$/,
  );
  refuses(
    broken(OVERVIEW, (c) => {
      const { refund_requested: _dropped, ...rest } = leg;
      c["bridge_legs_in_review"] = [rest];
    }),
    opsOverviewSpec,
    /\.bridge_legs_in_review\[0\]\.refund_requested$/,
  );
});

test("a released leg in review is offered only its resolution", () => {
  const offers = bridgeLegOffers({ released: true, refund_requested: false });
  assert.deepEqual(
    offers.map((offer) => offer.action),
    [...BRIDGE_LEG_ACTIONS],
    "every action is listed, offered or not, so the console says why one is not",
  );
  const byAction = new Map(offers.map((offer) => [offer.action, offer]));
  assert.equal(byAction.get("resolve-review")?.blocked, undefined);
  assert.match(byAction.get("resume-relay")?.blocked ?? "", /count its value twice/);
  assert.match(byAction.get("refund")?.blocked ?? "", /count its value twice/);
});

test("a leg that never released is offered a relay or a refund, never a resolution", () => {
  const byAction = new Map(bridgeLegOffers({ released: false, refund_requested: false }).map((offer) => [offer.action, offer]));
  assert.equal(byAction.get("resume-relay")?.blocked, undefined);
  assert.equal(byAction.get("refund")?.blocked, undefined);
  assert.equal(byAction.get("refund")?.label, "Refund it on the source chain");
  assert.match(byAction.get("resolve-review")?.blocked ?? "", /nothing to reconcile/);
});

test("once a refund is requested the leg never relays, and asking again only observes the refund", () => {
  const byAction = new Map(bridgeLegOffers({ released: false, refund_requested: true }).map((offer) => [offer.action, offer]));
  assert.match(byAction.get("resume-relay")?.blocked ?? "", /never relays again/);
  const refund = byAction.get("refund");
  assert.equal(refund?.blocked, undefined, "still callable, to observe the refund");
  assert.equal(refund?.label, "Observe the refund");
  assert.match(refund?.note ?? "", /never sent twice/);
  assert.match(byAction.get("resolve-review")?.blocked ?? "", /nothing to reconcile/);
  for (const offer of bridgeLegOffers({ released: true, refund_requested: true })) {
    assert.equal(offer.blocked === undefined, offer.action === "resolve-review", offer.action);
  }
});

test("an operator's reason is required and bounded", () => {
  assert.deepEqual(checkOperatorReason("  the destination mint was unfrozen  "), {
    ok: true,
    value: "the destination mint was unfrozen",
    error: "",
  });
  assert.equal(checkOperatorReason("   ").ok, false);
  assert.equal(checkOperatorReason("x".repeat(1000)).ok, true);
  assert.equal(checkOperatorReason("x".repeat(1001)).ok, false);
});

test("the chain tips are whole numbers in the simulated domain", () => {
  const chains = {
    network: "nodal-devnet",
    observed_at: "2026-10-08T12:00:00Z",
    items: [
      { chain_id: "sbx-ethereum", slot: 120, height: 118, hash: "ab12", final: false, finalized_height: 86 },
      { chain_id: "sbx-solana", slot: 40, height: 40, hash: "cd34", final: true, finalized_height: 8, last_block_produced_at: "2026-10-08T11:59:59Z" },
    ],
    value_domain: "SIMULATED",
  };
  assert.doesNotThrow(() => validated(chains, opsChainsSpec, "/t"));
  refuses(broken(chains, (c) => (c["items"] = [{ ...chains.items[0], height: "118" }])), opsChainsSpec, /\.items\[0\]\.height$/);
  refuses(broken(chains, (c) => delete c["network"]), opsChainsSpec, /\.network$/);
});

test("every operator key sits under the advanced prefix, so a recorded mark refreshes them all", () => {
  const prefix = advancedKeys.all;
  for (const key of [operatorKeys.all, operatorKeys.overview, operatorKeys.marks(ASSET), operatorKeys.chains]) {
    assert.deepEqual(key.slice(0, prefix.length), [...prefix]);
  }
  assert.notDeepEqual(operatorKeys.marks(ASSET), operatorKeys.marks(STABLE));
});
