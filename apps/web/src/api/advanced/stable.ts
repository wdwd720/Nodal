/**
 * The stable asset section's reads and commands (internal/stableasset,
 * ADR-0020).
 *
 * Modelled on `core.ts`: every response is checked against a spec before a
 * component sees it — exact base units (`quantity`), exact decimals
 * (`decimal`), closed enums, required fields present, and
 * `value_domain: SIMULATED` on everything that carries it — and refused as a
 * ContractViolation otherwise, never repaired. A 4xx is the backend's decision
 * and is not retried.
 *
 * Every command carries an Idempotency-Key that the page created once, when
 * the user confirmed, and reuses for every retry of that same request
 * (`confirmAttempt` in lib/stable.ts). A customer command invalidates the
 * whole account prefix (an issuance moves buying power, intents and effects
 * as well as holdings); an operator command invalidates everything advanced.
 *
 * A route answers UNSUPPORTED where the deployment has no sandbox chain; that
 * refusal reaches the page with its code like any other.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  STABLE_ALERT_KINDS,
  STABLE_ALERT_SEVERITIES,
  STABLE_ASSET_STATUSES,
  STABLE_EFFECT_STATUSES,
  STABLE_ISSUANCE_STATUSES,
  STABLE_RECONCILIATION_STATES,
  STABLE_REDEMPTION_MODES,
  STABLE_REDEMPTION_STATUSES,
  STABLE_REQUEST_MODES,
  STABLE_RESERVE_KINDS,
  STABLE_SNAPSHOT_FAILURES,
  type ConfirmedAttempt,
  type StableRedemptionMode,
  type StableRequestMode,
  type StableReserveKind,
} from "../../lib/stable.ts";
import { api } from "../client.ts";
import { cursorPageSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient, type CursorPage } from "./core.ts";

export type StableAsset = Schemas["StableAsset"];
export type StableAssetList = Schemas["StableAssetList"];
export type StableAssetDetail = Schemas["StableAssetDetail"];
export type StableReserveAsset = Schemas["StableReserveAsset"];
export type StablePrice = Schemas["StablePrice"];
export type StableAlert = Schemas["StableAlert"];
export type StableAssetTransition = Schemas["StableAssetTransition"];
export type StableReserve = Schemas["StableReserve"];
export type StableReserveHolding = Schemas["StableReserveHolding"];
export type StableSnapshot = Schemas["StableSnapshot"];
export type StableSnapshotChain = Schemas["StableSnapshotChain"];
export type StableSnapshotVerification = Schemas["StableSnapshotVerification"];
export type StableSnapshotResult = Schemas["StableSnapshotResult"];
export type StableIssuance = Schemas["StableIssuance"];
export type StableIssuanceResult = Schemas["StableIssuanceResult"];
export type StableRedemption = Schemas["StableRedemption"];
export type StableRedemptionResult = Schemas["StableRedemptionResult"];
export type StableReconciliation = Schemas["StableReconciliation"];
export type StableHoldings = Schemas["StableHoldings"];
export type StableHolding = Schemas["StableHolding"];
export type StablePaymentAsset = Schemas["StablePaymentAsset"];
export type StableSupplyReconciliation = Schemas["StableSupplyReconciliation"];
export type StableMonitorReport = Schemas["StableMonitorReport"];
export type StableSettleReport = Schemas["StableSettleReport"];

/** Rows per page of the issuance and redemption lists. */
export const STABLE_PAGE_SIZE = 25;
/** The asset list has no cursor: the contract's maximum is asked for. */
export const STABLE_LIST_LIMIT = 200;
/** Snapshots shown on the transparency page; the verification always covers the whole chain. */
export const SNAPSHOT_PAGE_LIMIT = 50;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const stableKeys = {
  /** Prefix of every stable asset read that is not one account's. */
  all: ["advanced", "stable"] as const,
  assets: ["advanced", "stable", "assets"] as const,
  asset: (stableAssetId: string) => ["advanced", "stable", "asset", stableAssetId] as const,
  reserve: (stableAssetId: string) => ["advanced", "stable", "reserve", stableAssetId] as const,
  snapshots: (stableAssetId: string) => ["advanced", "stable", "snapshots", stableAssetId] as const,
  holdings: (accountId: string) => [...advancedKeys.account(accountId), "stable", "holdings"] as const,
  issuances: (accountId: string, cursor: string) =>
    [...advancedKeys.account(accountId), "stable", "issuances", cursor] as const,
  issuance: (accountId: string, issuanceId: string) =>
    [...advancedKeys.account(accountId), "stable", "issuance", issuanceId] as const,
  redemptions: (accountId: string, cursor: string) =>
    [...advancedKeys.account(accountId), "stable", "redemptions", cursor] as const,
  redemption: (accountId: string, redemptionId: string) =>
    [...advancedKeys.account(accountId), "stable", "redemption", redemptionId] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
const RESERVE_KIND = oneOf(STABLE_RESERVE_KINDS);

export const stablePriceSpec: Spec = {
  required: {
    mantissa: "quantity",
    scale: "integer",
    decimal: "decimal",
    quote: "string",
    source: "string",
    observed_at: "timestamp",
  },
};

export const stableActorSpec: Spec = { required: { actor_type: "string", actor_id: "string" } };

export const stableReserveAssetSpec: Spec = {
  required: {
    asset_id: "uuid",
    symbol: "string",
    decimals: "integer",
    kind: RESERVE_KIND,
    factor_bps: "integer",
    accepted_for_mint: "boolean",
  },
};

export const stableAssetSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    name: "string",
    symbol: "string",
    peg_currency: "string",
    asset_id: "uuid",
    facility_id: "uuid",
    network: "string",
    chain: "string",
    mint: "string",
    decimals: "integer",
    status: oneOf(STABLE_ASSET_STATUSES),
    issuance_cap: "quantity",
    mint_authority_mode: "string",
    mint_authority: "string",
    burn_authority_mode: "string",
    redemption_mode: oneOf(STABLE_REDEMPTION_MODES),
    oracle_source: "string",
    max_price_age_ms: "integer",
    peg_deviation_threshold_bps: "integer",
    attestable: "boolean",
    created_at: "timestamp",
    updated_at: "timestamp",
  },
  objects: { peg: { required: true, spec: stablePriceSpec }, created_by: { required: true, spec: stableActorSpec } },
  arrays: { reserves: { required: true, spec: stableReserveAssetSpec } },
};

export const stableAssetListSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: { items: { required: true, spec: stableAssetSpec } },
};

export const stableTransitionSpec: Spec = {
  required: { from_status: "string", to_status: oneOf(STABLE_ASSET_STATUSES), reason: "string", occurred_at: "timestamp" },
  objects: { actor: { required: true, spec: stableActorSpec } },
};

export const stableAlertSpec: Spec = {
  required: {
    id: "uuid",
    kind: oneOf(STABLE_ALERT_KINDS),
    severity: oneOf(STABLE_ALERT_SEVERITIES),
    breaker_tripped: "boolean",
    observed: "object",
    observed_at: "timestamp",
  },
};

export const stableAssetDetailSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: { asset: { required: true, spec: stableAssetSpec } },
  arrays: {
    transitions: { required: true, spec: stableTransitionSpec },
    alerts: { required: true, spec: stableAlertSpec },
  },
};

export const stableReserveHoldingSpec: Spec = {
  required: {
    asset_id: "uuid",
    symbol: "string",
    decimals: "integer",
    kind: RESERVE_KIND,
    quantity: "quantity",
    factor_bps: "integer",
    eligible: "decimal",
  },
  objects: { price: { spec: stablePriceSpec } },
};

export const stableReserveSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    stable_asset_id: "uuid",
    symbol: "string",
    decimals: "integer",
    as_of: "timestamp",
    issued_supply: "quantity",
    pending_mint: "quantity",
    pending_burn: "quantity",
    supply_value: "decimal",
    eligible_reserve: "decimal",
    coverage_bps: "integer",
    covered: "boolean",
  },
  objects: { peg: { required: true, spec: stablePriceSpec } },
  arrays: {
    cash_equivalent_reserve: { required: true, spec: stableReserveHoldingSpec },
    tokenized_reserve: { required: true, spec: stableReserveHoldingSpec },
    receivable: { required: true, spec: stableReserveHoldingSpec },
    pending_redemption: { required: true, spec: stableReserveHoldingSpec },
  },
  lists: { unverified_reserve_asset_ids: { required: true, kind: "uuid" } },
};

/** A price inside a snapshot body: every number a string except the scale. */
export const stableSnapshotPriceSpec: Spec = {
  required: { mantissa: "quantity", scale: "integer", quote: "string", source: "string", at: "timestamp" },
};

export const stableSnapshotBodySpec: Spec = {
  required: {
    version: "string",
    disclaimer: "string",
    stable_asset_id: "string",
    asset_id: "string",
    chain: "string",
    mint: "string",
    seq: "quantity",
    taken_at: "timestamp",
    // Empty for the first snapshot of a chain, so a string of any length.
    prev_hash: "string",
    ledger_supply: "quantity",
    chain_supply: "quantity",
    pending_mint: "quantity",
    pending_burn: "quantity",
    supply_value: "decimal",
    eligible_reserve: "decimal",
    coverage_bps: "quantity",
  },
  objects: { peg: { required: true, spec: stableSnapshotPriceSpec } },
  arrays: {
    reserves: {
      required: true,
      spec: {
        required: {
          asset_id: "string",
          kind: "string",
          balance: "quantity",
          decimals: "quantity",
          factor_bps: "quantity",
          eligible: "decimal",
        },
        objects: { price: { spec: stableSnapshotPriceSpec } },
      },
    },
    composition: {
      required: true,
      spec: { required: { kind: "string", eligible: "decimal", share_bps: "quantity" } },
    },
  },
};

export const stableSnapshotSpec: Spec = {
  required: {
    seq: "integer",
    taken_at: "timestamp",
    hash: "string",
    // Empty for the first snapshot of a chain, so a string of any length.
    prev_hash: "string",
    signature: "string",
    signer_public_key: "string",
    evidence_ref: "string",
    verified: "boolean",
    canonical_body: "string",
  },
  objects: { body: { required: true, spec: stableSnapshotBodySpec } },
};

export const stableSnapshotVerificationSpec: Spec = {
  required: { verified: "boolean", checked: "integer", public_key: "string", disclaimer: "string" },
  optional: { failed_index: "integer", reason: oneOf(STABLE_SNAPSHOT_FAILURES) },
};

export const stableSnapshotChainSpec: Spec = {
  required: { value_domain: SIMULATED, stable_asset_id: "uuid", total: "integer" },
  objects: { verification: { required: true, spec: stableSnapshotVerificationSpec } },
  arrays: { items: { required: true, spec: stableSnapshotSpec } },
};

export const stableSnapshotResultSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: {
    snapshot: { required: true, spec: stableSnapshotSpec },
    verification: { required: true, spec: stableSnapshotVerificationSpec },
  },
};

export const stableReconciliationSpec: Spec = {
  required: { state: oneOf(STABLE_RECONCILIATION_STATES), effect_status: oneOf(STABLE_EFFECT_STATUSES) },
  optional: { unknown_since: "timestamp", resolved_at: "timestamp" },
};

const requestCommon = {
  value_domain: SIMULATED,
  id: "uuid",
  stable_asset_id: "uuid",
  symbol: "string",
  decimals: "integer",
  account_id: "uuid",
  intent_id: "uuid",
  effect_id: "uuid",
  mode: oneOf(STABLE_REQUEST_MODES),
  quantity: "quantity",
  chain: "string",
  tx_id: "string",
  plan_hash: "string",
  created_at: "timestamp",
  updated_at: "timestamp",
} as const;

export const stableIssuanceSpec: Spec = {
  required: {
    ...requestCommon,
    reserve_asset_id: "uuid",
    reserve_symbol: "string",
    reserve_decimals: "integer",
    reserve_quantity: "quantity",
    recipient: "string",
    status: oneOf(STABLE_ISSUANCE_STATUSES),
  },
  optional: { failure_code: "string", journal_transaction_id: "uuid", final_slot: "integer" },
  objects: {
    reserve_price: { required: true, spec: stablePriceSpec },
    reconciliation: { required: true, spec: stableReconciliationSpec },
  },
};

export const stableRedemptionSpec: Spec = {
  required: {
    ...requestCommon,
    payout_asset_id: "uuid",
    payout_symbol: "string",
    payout_decimals: "integer",
    payout_quantity: "quantity",
    redemption_mode: oneOf(STABLE_REDEMPTION_MODES),
    holder: "string",
    status: oneOf(STABLE_REDEMPTION_STATUSES),
  },
  optional: { failure_code: "string", journal_transaction_id: "uuid", final_slot: "integer" },
  objects: { reconciliation: { required: true, spec: stableReconciliationSpec } },
};

export const stableIssuancePageSpec: Spec = {
  ...cursorPageSpec(stableIssuanceSpec),
  required: { value_domain: SIMULATED },
};

export const stableRedemptionPageSpec: Spec = {
  ...cursorPageSpec(stableRedemptionSpec),
  required: { value_domain: SIMULATED },
};

export const stableIssuanceResultSpec: Spec = {
  required: { value_domain: SIMULATED, replay: "boolean", settled: "boolean" },
  objects: { issuance: { required: true, spec: stableIssuanceSpec } },
};

export const stableRedemptionResultSpec: Spec = {
  required: { value_domain: SIMULATED, replay: "boolean", settled: "boolean" },
  objects: { redemption: { required: true, spec: stableRedemptionSpec } },
};

export const stableHoldingsSpec: Spec = {
  required: { value_domain: SIMULATED, account_id: "uuid", holder_address: "string", as_of: "timestamp" },
  arrays: {
    items: {
      required: true,
      spec: {
        required: {
          stable_asset_id: "uuid",
          asset_id: "uuid",
          symbol: "string",
          decimals: "integer",
          chain: "string",
          status: oneOf(STABLE_ASSET_STATUSES),
          wallet: "quantity",
          reserved: "quantity",
          held: "quantity",
          available: "quantity",
          chain_balance: "quantity",
        },
      },
    },
    payment_assets: {
      required: true,
      spec: {
        required: {
          asset_id: "uuid",
          symbol: "string",
          decimals: "integer",
          wallet: "quantity",
          reserved: "quantity",
          held: "quantity",
          available: "quantity",
        },
      },
    },
  },
};

export const stableSupplyReconciliationSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    stable_asset_id: "uuid",
    ledger_outstanding: "quantity",
    customer_wallet_sum: "quantity",
    held_elsewhere: "quantity",
    chain_supply: "quantity",
    landed_unposted_mints: "quantity",
    landed_unposted_burns: "quantity",
    expected_chain_supply: "quantity",
    supply_value: "decimal",
    eligible_reserve: "decimal",
    coverage_bps: "integer",
    supply_matches: "boolean",
    wallets_match: "boolean",
    covered: "boolean",
    matched: "boolean",
    breaker_tripped: "boolean",
  },
  optional: { record_id: "uuid" },
};

export const stableMonitorReportSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    stable_asset_id: "uuid",
    observed_at: "timestamp",
    price_stale: "boolean",
    deviation_bps: "integer",
    breaker_tripped: "boolean",
    status: oneOf(STABLE_ASSET_STATUSES),
  },
  objects: {
    market_price: { spec: stablePriceSpec },
    reserve: { required: true, spec: stableReserveSpec },
    supply: { required: true, spec: stableSupplyReconciliationSpec },
  },
  lists: { alerts: { required: true, kind: oneOf(STABLE_ALERT_KINDS) } },
};

/** A request a pass could not act on at all: left as it was and retried after its backoff. */
export const stableRequestFailureSpec: Spec = {
  required: {
    operation: oneOf(["stableasset.settle_mint", "stableasset.settle_burn", "stableasset.pay_redemption"]),
    request_id: "uuid",
    attempts: "integer",
    retry_after: "timestamp",
    error_code: "string",
    detail: "string",
  },
};

export const stableSettleReportSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    examined: "integer",
    issued: "integer",
    redeemed: "integer",
    queued: "integer",
    failed: "integer",
    waiting: "integer",
  },
  arrays: { failures: { required: true, spec: stableRequestFailureSpec } },
};

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

export function useStableAssets(): UseQueryResult<StableAssetList> {
  return useQuery({
    queryKey: stableKeys.assets,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/stable-assets", { params: { query: { limit: STABLE_LIST_LIMIT } } });
      return validated<StableAssetList>(data, stableAssetListSpec, "/advanced/stable-assets");
    },
  });
}

export function useStableAsset(stableAssetId: string | undefined): UseQueryResult<StableAssetDetail> {
  return useQuery({
    queryKey: stableKeys.asset(stableAssetId ?? ""),
    enabled: stableAssetId !== undefined && stableAssetId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/stable-assets/{stableAssetId}", {
        params: { path: { stableAssetId: stableAssetId ?? "" } },
      });
      return validated<StableAssetDetail>(data, stableAssetDetailSpec, "/advanced/stable-assets/{id}");
    },
  });
}

export function useStableReserve(stableAssetId: string | undefined): UseQueryResult<StableReserve> {
  return useQuery({
    queryKey: stableKeys.reserve(stableAssetId ?? ""),
    enabled: stableAssetId !== undefined && stableAssetId !== "",
    // Valued by the backend at the instant of the read (prices age): refetched, never reused.
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/stable-assets/{stableAssetId}/reserve", {
        params: { path: { stableAssetId: stableAssetId ?? "" } },
      });
      return validated<StableReserve>(data, stableReserveSpec, "/advanced/stable-assets/{id}/reserve");
    },
  });
}

export function useStableSnapshots(stableAssetId: string | undefined): UseQueryResult<StableSnapshotChain> {
  return useQuery({
    queryKey: stableKeys.snapshots(stableAssetId ?? ""),
    enabled: stableAssetId !== undefined && stableAssetId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/stable-assets/{stableAssetId}/snapshots", {
        params: { path: { stableAssetId: stableAssetId ?? "" }, query: { limit: SNAPSHOT_PAGE_LIMIT } },
      });
      return validated<StableSnapshotChain>(data, stableSnapshotChainSpec, "/advanced/stable-assets/{id}/snapshots");
    },
  });
}

export function useStableHoldings(accountId: string | undefined): UseQueryResult<StableHoldings> {
  return useQuery({
    queryKey: stableKeys.holdings(accountId ?? ""),
    enabled: accountId !== undefined,
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/stable-assets/holdings", {
        params: { query: { account_id: accountId ?? "" } },
      });
      return validated<StableHoldings>(data, stableHoldingsSpec, "/advanced/stable-assets/holdings");
    },
  });
}

function pageQuery(cursor: string | undefined): { limit: number; cursor?: string } {
  return cursor === undefined ? { limit: STABLE_PAGE_SIZE } : { limit: STABLE_PAGE_SIZE, cursor };
}

export function useStableIssuances(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<StableIssuance>> {
  return useQuery({
    queryKey: stableKeys.issuances(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/stable-assets/issuances", {
        params: { query: { account_id: accountId ?? "", ...pageQuery(cursor) } },
      });
      const page = validated<Schemas["StableIssuancePage"]>(data, stableIssuancePageSpec, "/advanced/stable-assets/issuances");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/**
 * One issuance request. The backend scopes it by the request's own account
 * and answers NOT_FOUND to anyone who may not read it.
 */
export function useStableIssuance(accountId: string | undefined, issuanceId: string | undefined): UseQueryResult<StableIssuance> {
  return useQuery({
    queryKey: stableKeys.issuance(accountId ?? "", issuanceId ?? ""),
    enabled: issuanceId !== undefined && issuanceId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/stable-assets/issuances/{stableIssuanceId}", {
        params: { path: { stableIssuanceId: issuanceId ?? "" } },
      });
      return validated<StableIssuance>(data, stableIssuanceSpec, "/advanced/stable-assets/issuances/{id}");
    },
  });
}

export function useStableRedemptions(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<StableRedemption>> {
  return useQuery({
    queryKey: stableKeys.redemptions(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/stable-assets/redemptions", {
        params: { query: { account_id: accountId ?? "", ...pageQuery(cursor) } },
      });
      const page = validated<Schemas["StableRedemptionPage"]>(
        data,
        stableRedemptionPageSpec,
        "/advanced/stable-assets/redemptions",
      );
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useStableRedemption(
  accountId: string | undefined,
  redemptionId: string | undefined,
): UseQueryResult<StableRedemption> {
  return useQuery({
    queryKey: stableKeys.redemption(accountId ?? "", redemptionId ?? ""),
    enabled: redemptionId !== undefined && redemptionId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/stable-assets/redemptions/{stableRedemptionId}", {
        params: { path: { stableRedemptionId: redemptionId ?? "" } },
      });
      return validated<StableRedemption>(data, stableRedemptionSpec, "/advanced/stable-assets/redemptions/{id}");
    },
  });
}

/* --------------------------------------------------------------------------
 * Commands
 * ------------------------------------------------------------------------ */

/** A mutation of one confirmed attempt; the attempt carries its idempotency key. */
export type StableCommand<R, T> = UseMutationResult<T, unknown, ConfirmedAttempt<R>>;

export interface IssueRequest {
  readonly stableAssetId: string;
  readonly accountId: string;
  /** Exact base units of the stable asset. */
  readonly quantity: string;
  readonly reserveAssetId: string;
  readonly mode: StableRequestMode;
}

export interface RedeemRequest {
  readonly stableAssetId: string;
  readonly accountId: string;
  /** Exact base units of the stable asset. */
  readonly quantity: string;
  readonly mode: StableRequestMode;
}

/** A customer command changes what the account's advanced reads report, and the asset's reserve. */
function useAccountCommand<R extends { readonly accountId: string; readonly stableAssetId: string }, T>(
  run: (attempt: ConfirmedAttempt<R>) => Promise<T>,
): StableCommand<R, T> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: run,
    onSettled: (_data, _error, attempt) => {
      // A refusal is recorded too (a REJECTED intent, a risk decision), so the
      // reads are refreshed whatever the answer was.
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(attempt.request.accountId) });
      void queryClient.invalidateQueries({ queryKey: stableKeys.all });
    },
  });
}

export function useIssueStable(): StableCommand<IssueRequest, StableIssuanceResult> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/stable-assets/{stableAssetId}/issuances", {
      ...idempotent(idempotencyKey, { path: { stableAssetId: request.stableAssetId } }),
      body: {
        account_id: request.accountId,
        quantity: request.quantity,
        reserve_asset_id: request.reserveAssetId,
        mode: request.mode,
      },
    });
    return validated<StableIssuanceResult>(data, stableIssuanceResultSpec, "/advanced/stable-assets/{id}/issuances");
  });
}

export function useRedeemStable(): StableCommand<RedeemRequest, StableRedemptionResult> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/stable-assets/{stableAssetId}/redemptions", {
      ...idempotent(idempotencyKey, { path: { stableAssetId: request.stableAssetId } }),
      body: { account_id: request.accountId, quantity: request.quantity, mode: request.mode },
    });
    return validated<StableRedemptionResult>(data, stableRedemptionResultSpec, "/advanced/stable-assets/{id}/redemptions");
  });
}

/* Operator commands (sandbox:operate); those that register, pause, resume or move value need a step-up. */

export interface RegisterReserve {
  readonly assetId: string;
  readonly kind: StableReserveKind;
  readonly factorBps: number;
  readonly acceptedForMint: boolean;
}

export interface RegisterRequest {
  readonly name: string;
  readonly symbol: string;
  readonly pegCurrency: string;
  readonly decimals: number;
  /** Exact base units. */
  readonly issuanceCap: string;
  readonly redemptionMode: StableRedemptionMode;
  /** The peg as the operator typed it, an exact decimal of the sandbox dollar. */
  readonly peg: string;
  readonly oracleSource: string;
  readonly maxPriceAgeMs: number;
  readonly pegDeviationThresholdBps: number;
  readonly attestable: boolean;
  readonly reserves: readonly RegisterReserve[];
}

export interface AssetActionRequest {
  readonly stableAssetId: string;
}

export interface ReasonRequest extends AssetActionRequest {
  readonly reason: string;
}

export interface QueueRequest extends AssetActionRequest {
  readonly limit: number | undefined;
}

export interface ResolveRequest {
  readonly limit: number | undefined;
}

function useOperatorCommand<R, T>(run: (attempt: ConfirmedAttempt<R>) => Promise<T>): StableCommand<R, T> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: run,
    onSettled: () => {
      // An operator action can change any account the area may be reading.
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export function useRegisterStableAsset(): StableCommand<RegisterRequest, StableAssetDetail> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/stable-assets", {
      ...idempotent(idempotencyKey),
      body: {
        name: request.name,
        symbol: request.symbol,
        peg_currency: request.pegCurrency,
        decimals: request.decimals,
        issuance_cap: request.issuanceCap,
        redemption_mode: request.redemptionMode,
        peg: request.peg,
        oracle_source: request.oracleSource,
        max_price_age_ms: request.maxPriceAgeMs,
        peg_deviation_threshold_bps: request.pegDeviationThresholdBps,
        attestable: request.attestable,
        reserves: request.reserves.map((reserve) => ({
          asset_id: reserve.assetId,
          kind: reserve.kind,
          factor_bps: reserve.factorBps,
          accepted_for_mint: reserve.acceptedForMint,
        })),
      },
    });
    return validated<StableAssetDetail>(data, stableAssetDetailSpec, "/advanced/stable-assets");
  });
}

export function usePauseStableAsset(): StableCommand<ReasonRequest, StableAssetDetail> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/stable-assets/{stableAssetId}/pause", {
      ...idempotent(idempotencyKey, { path: { stableAssetId: request.stableAssetId } }),
      body: { reason: request.reason },
    });
    return validated<StableAssetDetail>(data, stableAssetDetailSpec, "/advanced/stable-assets/{id}/pause");
  });
}

export function useResumeStableAsset(): StableCommand<ReasonRequest, StableAssetDetail> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/stable-assets/{stableAssetId}/resume", {
      ...idempotent(idempotencyKey, { path: { stableAssetId: request.stableAssetId } }),
      body: { reason: request.reason },
    });
    return validated<StableAssetDetail>(data, stableAssetDetailSpec, "/advanced/stable-assets/{id}/resume");
  });
}

export function useRunStableMonitor(): StableCommand<AssetActionRequest, StableMonitorReport> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/stable-assets/{stableAssetId}/monitor-runs", {
      ...idempotent(idempotencyKey, { path: { stableAssetId: request.stableAssetId } }),
    });
    return validated<StableMonitorReport>(data, stableMonitorReportSpec, "/advanced/stable-assets/{id}/monitor-runs");
  });
}

export function useReconcileStableSupply(): StableCommand<AssetActionRequest, StableSupplyReconciliation> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/stable-assets/{stableAssetId}/reconciliations", {
      ...idempotent(idempotencyKey, { path: { stableAssetId: request.stableAssetId } }),
    });
    return validated<StableSupplyReconciliation>(
      data,
      stableSupplyReconciliationSpec,
      "/advanced/stable-assets/{id}/reconciliations",
    );
  });
}

export function useTakeStableSnapshot(): StableCommand<AssetActionRequest, StableSnapshotResult> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/stable-assets/{stableAssetId}/snapshots", {
      ...idempotent(idempotencyKey, { path: { stableAssetId: request.stableAssetId } }),
    });
    return validated<StableSnapshotResult>(data, stableSnapshotResultSpec, "/advanced/stable-assets/{id}/snapshots");
  });
}

export function useRunRedemptionQueue(): StableCommand<QueueRequest, StableSettleReport> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/stable-assets/{stableAssetId}/redemption-queue/run", {
      ...idempotent(idempotencyKey, { path: { stableAssetId: request.stableAssetId } }),
      body: request.limit === undefined ? {} : { limit: request.limit },
    });
    return validated<StableSettleReport>(data, stableSettleReportSpec, "/advanced/stable-assets/{id}/redemption-queue/run");
  });
}

export function useResolveUnknownStable(): StableCommand<ResolveRequest, StableSettleReport> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/stable-assets/ops/resolve-unknown", {
      ...idempotent(idempotencyKey),
      body: request.limit === undefined ? {} : { limit: request.limit },
    });
    return validated<StableSettleReport>(data, stableSettleReportSpec, "/advanced/stable-assets/ops/resolve-unknown");
  });
}
