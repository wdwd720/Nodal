/**
 * The stable asset section's pure logic (lib/stable.ts) and its runtime
 * contract (api/advanced/stable.ts).
 *
 * What these hold the section to: a request whose submission outcome is
 * unknown is classified as unknown — never as issued, redeemed or failed —
 * until reconciliation settles it; a percentage is the backend's basis points
 * with the point moved, never a division; what a person typed is checked and
 * sent exactly; a confirmed command keeps one idempotency key across retries;
 * and every response is refused unless its figures are exact strings in the
 * simulated domain.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  stableAssetDetailSpec,
  stableHoldingsSpec,
  stableIssuanceResultSpec,
  stableKeys,
  stableRedemptionResultSpec,
  stableReserveSpec,
  stableSnapshotChainSpec,
  stableSupplyReconciliationSpec,
} from "../api/advanced/stable.ts";
import { advancedKeys } from "../api/advanced/core.ts";
import { ContractViolation, validated } from "../api/contract.ts";
import { MoneyFormatError } from "./money.ts";
import {
  ALERT_KIND_MEANING,
  ASSET_STATUS_MEANING,
  ISSUANCE_STATUS_MEANING,
  RECONCILIATION_STATE_MEANING,
  REDEMPTION_MODE_MEANING,
  REDEMPTION_STATUS_MEANING,
  REQUEST_MODE_MEANING,
  SNAPSHOT_FAILURE_MEANING,
  STABLE_ALERT_KINDS,
  STABLE_ASSET_STATUSES,
  STABLE_ISSUANCE_STATUSES,
  STABLE_RECONCILIATION_STATES,
  STABLE_REDEMPTION_MODES,
  STABLE_REDEMPTION_STATUSES,
  STABLE_REQUEST_MODES,
  STABLE_SNAPSHOT_FAILURES,
  confirmAttempt,
  formatBpsText,
  issuanceOutcome,
  outcomeLabel,
  parseBaseUnits,
  parsePositiveDecimal,
  parseReason,
  parseRegistryName,
  parseWholeNumber,
  redemptionOutcome,
  reserveKindLabel,
  snapshotsShownNote,
  verificationSummary,
} from "./stable.ts";

const ACCOUNT = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const ASSET = "01a075f1-f846-719e-bcbb-493c8281c524";
const STABLE = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const SANDBOX_DOLLAR = "01920000-0000-7000-8000-5342582d5553";
const REQUEST = "01a075f1-f900-7000-8000-000000000001";
const INTENT = "01a075f1-f900-7000-8000-000000000002";
const EFFECT = "01a075f1-f900-7000-8000-000000000003";
const AT = "2026-10-08T12:00:00Z";
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

const PEG = { mantissa: "1000000", scale: 6, decimal: "1.000000", quote: SANDBOX_DOLLAR, source: "TARGET_PEG", observed_at: AT };

/** An issuance whose submission timed out on the chain: recorded, its outcome unknown. */
const UNKNOWN_ISSUANCE = {
  value_domain: "SIMULATED",
  replay: false,
  settled: false,
  issuance: {
    value_domain: "SIMULATED",
    id: REQUEST,
    stable_asset_id: STABLE,
    symbol: "SBX-PSA",
    decimals: 6,
    account_id: ACCOUNT,
    intent_id: INTENT,
    effect_id: EFFECT,
    mode: "SANDBOX",
    quantity: "600000000",
    reserve_asset_id: SANDBOX_DOLLAR,
    reserve_symbol: "SBX-USD",
    reserve_decimals: 6,
    reserve_quantity: "600000000",
    reserve_price: { ...PEG, source: "PAR" },
    recipient: "8kYwPEm1jMtRDcn5U9XdPjYGsKq2cDNe2vWo9Z3Q1hPa",
    chain: "sbx-solana",
    tx_id: "3uYbM8sVt4kJqLdn7RzWQ1oPfX9eGc2hTnA5mK6yBvZs",
    plan_hash: HASH,
    status: "SUBMISSION_UNKNOWN",
    reconciliation: { state: "AWAITING", effect_status: "UNKNOWN", unknown_since: AT },
    created_at: AT,
    updated_at: AT,
  },
};

/* --------------------------------------------------------------------------
 * Outcomes
 * ------------------------------------------------------------------------ */

test("an unknown submission is classified as unknown, never as issued, redeemed or failed", () => {
  assert.equal(issuanceOutcome("SUBMISSION_UNKNOWN", "AWAITING"), "UNKNOWN");
  assert.equal(redemptionOutcome("SUBMISSION_UNKNOWN", "AWAITING"), "UNKNOWN");
  // The reconciliation state alone is enough to say so.
  assert.equal(issuanceOutcome("SUBMITTING", "AWAITING"), "UNKNOWN");
  assert.equal(redemptionOutcome("SUBMITTED", "AWAITING"), "UNKNOWN");
  assert.equal(outcomeLabel("UNKNOWN", "Issued"), "Outcome unknown");

  assert.equal(issuanceOutcome("ISSUED", "RESOLVED"), "COMPLETED", "settled from the chain, then final");
  assert.equal(issuanceOutcome("FAILED", "NOT_REQUIRED"), "FAILED");
  assert.equal(issuanceOutcome("SUBMITTED", "NOT_REQUIRED"), "IN_PROGRESS");
  assert.equal(redemptionOutcome("REDEEMED", "NOT_REQUIRED"), "COMPLETED");
  assert.equal(redemptionOutcome("REDEMPTION_QUEUED", "NOT_REQUIRED"), "IN_PROGRESS", "a queued payout is not final");
  assert.equal(redemptionOutcome("FAILED", "RESOLVED"), "FAILED");
  assert.equal(outcomeLabel("COMPLETED", "Redeemed"), "Redeemed");
  assert.equal(outcomeLabel("IN_PROGRESS", "Issued"), "Not final yet");
});

test("every contract value has its meaning", () => {
  for (const status of STABLE_ASSET_STATUSES) assert.ok(ASSET_STATUS_MEANING[status].length > 10, status);
  for (const status of STABLE_ISSUANCE_STATUSES) assert.ok(ISSUANCE_STATUS_MEANING[status].length > 10, status);
  for (const status of STABLE_REDEMPTION_STATUSES) assert.ok(REDEMPTION_STATUS_MEANING[status].length > 10, status);
  for (const state of STABLE_RECONCILIATION_STATES) assert.ok(RECONCILIATION_STATE_MEANING[state].length > 10, state);
  for (const mode of STABLE_REDEMPTION_MODES) assert.ok(REDEMPTION_MODE_MEANING[mode].length > 10, mode);
  for (const mode of STABLE_REQUEST_MODES) assert.ok(REQUEST_MODE_MEANING[mode].length > 10, mode);
  for (const kind of STABLE_ALERT_KINDS) assert.ok(ALERT_KIND_MEANING[kind].length > 10, kind);
  for (const reason of STABLE_SNAPSHOT_FAILURES) assert.ok(SNAPSHOT_FAILURE_MEANING[reason].length > 10, reason);
  // Neither unknown state is ever described as a success or a failure.
  assert.match(RECONCILIATION_STATE_MEANING.AWAITING, /not a success and not a failure/);
});

test("a reserve kind is named in words this interface may use", () => {
  assert.equal(reserveKindLabel("TOKENIZED"), "Tokenized reserve");
  assert.equal(reserveKindLabel("CASH_EQUIVALENT"), "Dollar-equivalent reserve");
});

/* --------------------------------------------------------------------------
 * Display
 * ------------------------------------------------------------------------ */

test("basis points sent as text become a percentage by moving the point", () => {
  assert.equal(formatBpsText("10000"), "100%");
  assert.equal(formatBpsText("2500"), "25%");
  assert.equal(formatBpsText("1234"), "12.34%");
  assert.equal(formatBpsText("5"), "0.05%");
  assert.equal(formatBpsText("50"), "0.5%");
  assert.equal(formatBpsText("0"), "0%");
  assert.equal(formatBpsText("15000"), "150%", "over-collateralised coverage is shown as it is");
  assert.throws(() => formatBpsText("12.5"), MoneyFormatError);
  assert.throws(() => formatBpsText(""), MoneyFormatError);
});

test("the snapshot chain's verification is told from the backend's result", () => {
  assert.match(verificationSummary({ verified: true, checked: 2 }), /^All 2 snapshots of the chain verify/);
  assert.match(verificationSummary({ verified: true, checked: 0 }), /nothing to verify/);
  const failed = verificationSummary({ verified: false, checked: 3, failed_index: 1, reason: "SNAPSHOT_SIGNATURE_INVALID" });
  assert.match(failed, /position 2/);
  assert.match(failed, /SNAPSHOT_SIGNATURE_INVALID/);
  assert.match(failed, /every snapshot after it are unverified/);
  assert.match(snapshotsShownNote(3, 3), /^All 3 snapshots/);
  assert.match(snapshotsShownNote(80, 50), /newest 50 of 80/);
});

/* --------------------------------------------------------------------------
 * What a person typed
 * ------------------------------------------------------------------------ */

test("what a person typed is checked and kept exactly", () => {
  assert.deepEqual(parseWholeNumber("18", 0, 18, "Decimals"), { ok: true, value: 18 });
  assert.equal(parseWholeNumber("19", 0, 18, "Decimals").ok, false);
  assert.equal(parseWholeNumber("1.5", 0, 18, "Decimals").ok, false);
  assert.equal(parseWholeNumber("-1", 0, 18, "Decimals").ok, false);
  assert.equal(parseWholeNumber("", 0, 18, "Decimals").ok, false);

  assert.deepEqual(parsePositiveDecimal("1.000000", "The peg"), { ok: true, value: "1.000000" }, "kept as typed, trailing zeros too");
  assert.deepEqual(parsePositiveDecimal(" 0.98 ", "The peg"), { ok: true, value: "0.98" });
  for (const bad of ["0", "0.000", "-1", "1e3", "1,000", ".5", "1.", "abc"]) {
    assert.equal(parsePositiveDecimal(bad, "The peg").ok, false, bad);
  }

  assert.deepEqual(parseBaseUnits("000600000000", "Units"), { ok: true, value: "600000000" });
  assert.equal(parseBaseUnits("0", "Units").ok, false);
  assert.equal(parseBaseUnits("6e8", "Units").ok, false);

  assert.equal(parseRegistryName("platform-sandbox-dollar", "Name").ok, true);
  for (const bad of ["", "-leading", "Upper", "with space", "a".repeat(64)]) {
    assert.equal(parseRegistryName(bad, "Name").ok, false, bad);
  }
  assert.equal(parseReason("ok").ok, false, "a reason says something");
  assert.deepEqual(parseReason("  peg restored  "), { ok: true, value: "peg restored" });
});

test("a confirmation creates a key once and every retry of the same request reuses it", () => {
  let made = 0;
  const newKey = (): string => {
    made = made + 1;
    return `key-${String(made)}-0000`;
  };
  const request = { stableAssetId: STABLE, accountId: ACCOUNT, quantity: "600000000", mode: "SANDBOX" };
  const first = confirmAttempt(undefined, request, newKey);
  const retry = confirmAttempt(first, { ...request }, newKey);
  assert.equal(retry.idempotencyKey, first.idempotencyKey);
  assert.equal(made, 1);
  const changed = confirmAttempt(first, { ...request, quantity: "1" }, newKey);
  assert.notEqual(changed.idempotencyKey, first.idempotencyKey, "a different request is a new attempt");
  assert.equal(made, 2);
});

/* --------------------------------------------------------------------------
 * The runtime contract
 * ------------------------------------------------------------------------ */

test("an issuance with an unknown outcome is accepted as the backend sends it", () => {
  const result = validated<{ issuance: { status: string } }>(UNKNOWN_ISSUANCE, stableIssuanceResultSpec, "/x");
  assert.equal(result.issuance.status, "SUBMISSION_UNKNOWN");
});

test("an issuance is refused unless its figures are exact and its domain simulated", () => {
  refuses(
    () => validated(broken(UNKNOWN_ISSUANCE, (c) => (c["value_domain"] = "LIVE")), stableIssuanceResultSpec, "/x"),
    /^\/x\.value_domain$/,
  );
  refuses(
    () =>
      validated(
        broken(UNKNOWN_ISSUANCE, (c) => ((c["issuance"] as Record<string, unknown>)["quantity"] = "600.5")),
        stableIssuanceResultSpec,
        "/x",
      ),
    /^\/x\.issuance\.quantity$/,
  );
  refuses(
    () =>
      validated(
        broken(UNKNOWN_ISSUANCE, (c) => ((c["issuance"] as Record<string, unknown>)["reserve_quantity"] = 600000000)),
        stableIssuanceResultSpec,
        "/x",
      ),
    /^\/x\.issuance\.reserve_quantity$/,
  );
  refuses(
    () =>
      validated(
        broken(UNKNOWN_ISSUANCE, (c) => ((c["issuance"] as Record<string, unknown>)["status"] = "SUCCEEDED")),
        stableIssuanceResultSpec,
        "/x",
      ),
    /^\/x\.issuance\.status$/,
  );
  refuses(
    () =>
      validated(
        broken(UNKNOWN_ISSUANCE, (c) => {
          const issuance = c["issuance"] as Record<string, unknown>;
          issuance["reconciliation"] = { state: "PENDING", effect_status: "UNKNOWN" };
        }),
        stableIssuanceResultSpec,
        "/x",
      ),
    /^\/x\.issuance\.reconciliation\.state$/,
  );
});

test("a redemption carries its payout and redemption mode", () => {
  const redemption = {
    value_domain: "SIMULATED",
    replay: true,
    settled: true,
    redemption: {
      ...UNKNOWN_ISSUANCE.issuance,
      payout_asset_id: SANDBOX_DOLLAR,
      payout_symbol: "SBX-USD",
      payout_decimals: 6,
      payout_quantity: "200000000",
      redemption_mode: "INSTANT",
      holder: "8kYwPEm1jMtRDcn5U9XdPjYGsKq2cDNe2vWo9Z3Q1hPa",
      status: "REDEEMED",
      journal_transaction_id: REQUEST,
      reconciliation: { state: "NOT_REQUIRED", effect_status: "APPLIED" },
    },
  };
  assert.doesNotThrow(() => validated(redemption, stableRedemptionResultSpec, "/x"));
  refuses(
    () =>
      validated(
        broken(redemption, (c) => ((c["redemption"] as Record<string, unknown>)["redemption_mode"] = "LATER")),
        stableRedemptionResultSpec,
        "/x",
      ),
    /^\/x\.redemption\.redemption_mode$/,
  );
});

const HOLDING = {
  asset_id: SANDBOX_DOLLAR,
  symbol: "SBX-USD",
  kind: "CASH_EQUIVALENT",
  decimals: 6,
  quantity: "600000000",
  factor_bps: 10000,
  price: { ...PEG, source: "PAR" },
  eligible: "600.000000",
};

const RESERVE = {
  value_domain: "SIMULATED",
  stable_asset_id: STABLE,
  symbol: "SBX-PSA",
  decimals: 6,
  as_of: AT,
  peg: PEG,
  issued_supply: "600000000",
  cash_equivalent_reserve: [HOLDING],
  tokenized_reserve: [],
  pending_mint: "0",
  receivable: [],
  pending_burn: "0",
  pending_redemption: [],
  supply_value: "600.000000",
  eligible_reserve: "600.000000",
  coverage_bps: 10000,
  covered: true,
  unverified_reserve_asset_ids: [],
};

test("the reserve ledger is accepted with exact figures and an unpriced holding, and refused with a malformed figure", () => {
  assert.doesNotThrow(() => validated(RESERVE, stableReserveSpec, "/x"));
  // A holding with no fresh price has no price key at all and counts zero.
  const unpriced = broken(RESERVE, (c) => {
    c["tokenized_reserve"] = [{ ...HOLDING, kind: "TOKENIZED", price: undefined, eligible: "0" }];
    c["unverified_reserve_asset_ids"] = [ASSET];
  });
  assert.doesNotThrow(() => validated(unpriced, stableReserveSpec, "/x"));
  refuses(() => validated(broken(RESERVE, (c) => (c["supply_value"] = 600)), stableReserveSpec, "/x"), /^\/x\.supply_value$/);
  refuses(
    () => validated(broken(RESERVE, (c) => (c["cash_equivalent_reserve"] = [{ ...HOLDING, kind: "GOLD" }])), stableReserveSpec, "/x"),
    /^\/x\.cash_equivalent_reserve\[0\]\.kind$/,
  );
  refuses(() => validated(broken(RESERVE, (c) => (c["issued_supply"] = "6e8")), stableReserveSpec, "/x"), /^\/x\.issued_supply$/);
});

const SNAPSHOT_BODY = {
  version: "nodal-sandbox/stable-reserve-snapshot/v1",
  disclaimer: "Machine-verifiable snapshot of SIMULATED sandbox books and a simulated chain.",
  stable_asset_id: STABLE,
  asset_id: ASSET,
  chain: "sbx-solana",
  mint: "8kYwPEm1jMtRDcn5U9XdPjYGsKq2cDNe2vWo9Z3Q1hPa",
  seq: "1",
  taken_at: AT,
  prev_hash: "",
  ledger_supply: "600000000",
  chain_supply: "600000000",
  pending_mint: "0",
  pending_burn: "0",
  peg: { mantissa: "1000000", scale: 6, quote: SANDBOX_DOLLAR, source: "TARGET_PEG", at: AT },
  reserves: [
    { asset_id: SANDBOX_DOLLAR, kind: "CASH_EQUIVALENT", balance: "600000000", decimals: "6", factor_bps: "10000", eligible: "600.000000" },
  ],
  composition: [
    { kind: "CASH_EQUIVALENT", eligible: "600.000000", share_bps: "10000" },
    { kind: "TOKENIZED", eligible: "0", share_bps: "0" },
  ],
  supply_value: "600.000000",
  eligible_reserve: "600.000000",
  coverage_bps: "10000",
};

const CHAIN = {
  value_domain: "SIMULATED",
  stable_asset_id: STABLE,
  total: 1,
  verification: { verified: true, checked: 1, public_key: HASH, disclaimer: SNAPSHOT_BODY.disclaimer },
  items: [
    {
      seq: 1,
      taken_at: AT,
      hash: HASH,
      prev_hash: "",
      signature: `${HASH}${HASH}`,
      signer_public_key: HASH,
      evidence_ref: "proof://stableasset/snapshots/1",
      verified: true,
      canonical_body: JSON.stringify(SNAPSHOT_BODY),
      body: SNAPSHOT_BODY,
    },
  ],
};

test("the snapshot chain is accepted with its first snapshot naming no previous hash", () => {
  assert.doesNotThrow(() => validated(CHAIN, stableSnapshotChainSpec, "/x"));
  const failed = broken(CHAIN, (c) => {
    c["verification"] = { verified: false, checked: 1, failed_index: 0, reason: "SNAPSHOT_HASH_MISMATCH", public_key: HASH, disclaimer: "d" };
  });
  assert.doesNotThrow(() => validated(failed, stableSnapshotChainSpec, "/x"));
});

test("the snapshot chain is refused when its verification or its body is not the contract's", () => {
  refuses(
    () =>
      validated(
        broken(CHAIN, (c) => ((c["verification"] as Record<string, unknown>)["reason"] = "LOOKS_FINE")),
        stableSnapshotChainSpec,
        "/x",
      ),
    /^\/x\.verification\.reason$/,
  );
  refuses(
    () =>
      validated(
        broken(CHAIN, (c) => {
          const item = (c["items"] as Array<Record<string, unknown>>)[0] as Record<string, unknown>;
          item["verified"] = "yes";
        }),
        stableSnapshotChainSpec,
        "/x",
      ),
    /^\/x\.items\[0\]\.verified$/,
  );
  refuses(
    () =>
      validated(
        broken(CHAIN, (c) => {
          const item = (c["items"] as Array<Record<string, unknown>>)[0] as Record<string, unknown>;
          item["body"] = { ...SNAPSHOT_BODY, coverage_bps: "100%" };
        }),
        stableSnapshotChainSpec,
        "/x",
      ),
    /^\/x\.items\[0\]\.body\.coverage_bps$/,
  );
});

test("the supply reconciliation states the holdings held elsewhere next to the wallets", () => {
  const rec = {
    value_domain: "SIMULATED",
    stable_asset_id: STABLE,
    ledger_outstanding: "400000000",
    customer_wallet_sum: "300000000",
    held_elsewhere: "100000000",
    chain_supply: "400000000",
    landed_unposted_mints: "0",
    landed_unposted_burns: "0",
    expected_chain_supply: "400000000",
    supply_value: "400.000000",
    eligible_reserve: "400.000000",
    coverage_bps: 10000,
    supply_matches: true,
    wallets_match: true,
    covered: true,
    matched: true,
    breaker_tripped: false,
  };
  assert.doesNotThrow(() => validated(rec, stableSupplyReconciliationSpec, "/x"));
  refuses(() => validated(broken(rec, (c) => delete c["held_elsewhere"]), stableSupplyReconciliationSpec, "/x"), /^\/x\.held_elsewhere$/);
});

test("the holdings and the asset detail are accepted only in their contract's shape", () => {
  const holdings = {
    value_domain: "SIMULATED",
    account_id: ACCOUNT,
    holder_address: "8kYwPEm1jMtRDcn5U9XdPjYGsKq2cDNe2vWo9Z3Q1hPa",
    as_of: AT,
    items: [
      {
        stable_asset_id: STABLE,
        asset_id: ASSET,
        symbol: "SBX-PSA",
        decimals: 6,
        chain: "sbx-solana",
        status: "ACTIVE",
        wallet: "600000000",
        reserved: "0",
        held: "0",
        available: "600000000",
        chain_balance: "600000000",
      },
    ],
    payment_assets: [
      { asset_id: SANDBOX_DOLLAR, symbol: "SBX-USD", decimals: 6, wallet: "400000000", reserved: "0", held: "0", available: "400000000" },
    ],
  };
  assert.doesNotThrow(() => validated(holdings, stableHoldingsSpec, "/x"));
  refuses(
    () =>
      validated(
        broken(holdings, (c) => ((c["items"] as Array<Record<string, unknown>>)[0] as Record<string, unknown>)["available"] = "-"),
        stableHoldingsSpec,
        "/x",
      ),
    /^\/x\.items\[0\]\.available$/,
  );

  const asset = {
    value_domain: "SIMULATED",
    id: STABLE,
    name: "platform-sandbox-dollar",
    symbol: "SBX-PSA",
    peg_currency: "USD",
    asset_id: ASSET,
    facility_id: REQUEST,
    network: "nodal-sandbox",
    chain: "sbx-solana",
    mint: "8kYwPEm1jMtRDcn5U9XdPjYGsKq2cDNe2vWo9Z3Q1hPa",
    decimals: 6,
    status: "PAUSED",
    issuance_cap: "1000000000000",
    mint_authority_mode: "PLATFORM_SIGNER",
    mint_authority: "3uYbM8sVt4kJqLdn7RzWQ1oPfX9eGc2hTnA5mK6yBvZs",
    burn_authority_mode: "HOLDER_SIGNED",
    redemption_mode: "INSTANT",
    peg: PEG,
    oracle_source: "sandbox-oracle",
    max_price_age_ms: 3600000,
    peg_deviation_threshold_bps: 50,
    attestable: true,
    reserves: [{ asset_id: SANDBOX_DOLLAR, symbol: "SBX-USD", decimals: 6, kind: "CASH_EQUIVALENT", factor_bps: 10000, accepted_for_mint: true }],
    created_by: { actor_type: "OPERATOR", actor_id: "operator-1" },
    created_at: AT,
    updated_at: AT,
  };
  const detail = {
    value_domain: "SIMULATED",
    asset,
    transitions: [
      { from_status: "ACTIVE", to_status: "PAUSED", actor: { actor_type: "SYSTEM", actor_id: "breaker" }, reason: "circuit breaker", occurred_at: AT },
    ],
    alerts: [
      { id: REQUEST, kind: "PRICE_DEVIATION", severity: "CRITICAL", breaker_tripped: true, observed: { deviation_bps: 200 }, observed_at: AT },
    ],
  };
  assert.doesNotThrow(() => validated(detail, stableAssetDetailSpec, "/x"));
  refuses(
    () => validated(broken(detail, (c) => ((c["asset"] as Record<string, unknown>)["issuance_cap"] = "1e12")), stableAssetDetailSpec, "/x"),
    /^\/x\.asset\.issuance_cap$/,
  );
  refuses(
    () => validated(broken(detail, (c) => (c["alerts"] = [{ ...detail.alerts[0], observed: "deviation" }])), stableAssetDetailSpec, "/x"),
    /^\/x\.alerts\[0\]\.observed$/,
  );
});

test("every account-scoped stable key sits under its account's advanced prefix", () => {
  const prefix = advancedKeys.account(ACCOUNT);
  for (const key of [
    stableKeys.holdings(ACCOUNT),
    stableKeys.issuances(ACCOUNT, ""),
    stableKeys.issuance(ACCOUNT, REQUEST),
    stableKeys.redemptions(ACCOUNT, "c"),
    stableKeys.redemption(ACCOUNT, REQUEST),
  ]) {
    assert.deepEqual(key.slice(0, prefix.length), [...prefix]);
  }
  assert.deepEqual(stableKeys.reserve(STABLE).slice(0, stableKeys.all.length), [...stableKeys.all]);
});
