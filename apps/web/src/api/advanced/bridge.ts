/**
 * The bridge section's reads and commands (internal/bridge and the simulated
 * chain host, ADR-0020).
 *
 * Modelled on `core.ts`: every response is checked against a spec before a
 * component sees it — exact base units (`quantity`), two-decimal cent strings
 * (`usd`), closed enums, required fields present, and
 * `value_domain: SIMULATED` on everything that carries it — and refused as a
 * ContractViolation otherwise, never repaired. A 4xx is the backend's decision
 * and is not retried.
 *
 * Every command carries an Idempotency-Key that the page created once, when
 * the user confirmed, and reuses for every retry of that same request. A
 * customer's transfer invalidates the whole account prefix; an operator
 * command invalidates everything advanced, because a block produced on a
 * chain can finalize anyone's leg.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  BRIDGE_ATTEMPT_KINDS,
  BRIDGE_CHAIN_IDS,
  BRIDGE_EFFECT_STATUSES,
  BRIDGE_FAULT_KINDS,
  BRIDGE_LEG_HOLDINGS,
  BRIDGE_LEG_MODES,
  BRIDGE_LEG_STATUSES,
  BRIDGE_RECONCILIATION_STATES,
  BRIDGE_ROUTE_STATUSES,
  BRIDGE_TX_STATES,
  type BridgeChainId,
  type BridgeFaultKind,
  type BridgeLegMode,
} from "../../lib/bridge.ts";
import type { ConfirmedAttempt } from "../../lib/stable.ts";
import { api } from "../client.ts";
import { cursorPageSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient, type CursorPage } from "./core.ts";
import { stableKeys } from "./stable.ts";

export type BridgeRoute = Schemas["BridgeRoute"];
export type BridgeRouteList = Schemas["BridgeRouteList"];
export type BridgeLeg = Schemas["BridgeLeg"];
export type BridgeLegDetail = Schemas["BridgeLegDetail"];
export type BridgeLegResult = Schemas["BridgeLegResult"];
export type BridgeLegAttempt = Schemas["BridgeLegAttempt"];
export type BridgeLegTransition = Schemas["BridgeLegTransition"];
export type BridgeChainTx = Schemas["BridgeChainTx"];
export type BridgeChain = Schemas["BridgeChain"];
export type BridgeChains = Schemas["BridgeChains"];
export type BridgeBlocksResult = Schemas["BridgeBlocksResult"];
export type BridgeFaultResult = Schemas["BridgeFaultResult"];
export type BridgeHoldings = Schemas["BridgeHoldings"];
export type BridgeRouteReconciliation = Schemas["BridgeRouteReconciliation"];

/** Rows per page of the leg list. */
export const BRIDGE_PAGE_SIZE = 25;
/** The route list has no cursor: the contract's maximum is asked for. */
export const BRIDGE_ROUTE_LIMIT = 200;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const bridgeKeys = {
  /** Prefix of every bridge read that is not one account's. */
  all: ["advanced", "bridge"] as const,
  routes: ["advanced", "bridge", "routes"] as const,
  route: (routeId: string) => ["advanced", "bridge", "route", routeId] as const,
  chains: ["advanced", "bridge", "chains"] as const,
  holdings: (accountId: string) => [...advancedKeys.account(accountId), "bridge", "holdings"] as const,
  legs: (accountId: string, cursor: string) => [...advancedKeys.account(accountId), "bridge", "legs", cursor] as const,
  leg: (accountId: string, legId: string) => [...advancedKeys.account(accountId), "bridge", "leg", legId] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
const LEG_STATUS = oneOf(BRIDGE_LEG_STATUSES);
const EFFECT_STATUS = oneOf(BRIDGE_EFFECT_STATUSES);

export const bridgePriceSpec: Spec = {
  required: { mantissa: "quantity", scale: "integer", decimal: "decimal", quote: "string" },
};

export const bridgeRouteSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    name: "string",
    status: oneOf(BRIDGE_ROUTE_STATUSES),
    network: "string",
    facility_id: "uuid",
    source_chain: "string",
    source_asset_id: "uuid",
    source_symbol: "string",
    source_mint: "string",
    destination_chain: "string",
    destination_asset_id: "uuid",
    destination_symbol: "string",
    destination_mint: "string",
    decimals: "integer",
    attester: "string",
    max_release_attempts: "integer",
    created_at: "timestamp",
  },
  objects: { unit_price: { required: true, spec: bridgePriceSpec } },
};

export const bridgeRouteListSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: { items: { required: true, spec: bridgeRouteSpec } },
};

export const bridgeReconciliationSpec: Spec = {
  required: { state: oneOf(BRIDGE_RECONCILIATION_STATES) },
  optional: { unknown_since: "timestamp", resolved_at: "timestamp" },
};

export const bridgeLegSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    route_id: "uuid",
    account_id: "uuid",
    intent_id: "uuid",
    mode: oneOf(BRIDGE_LEG_MODES),
    quantity: "quantity",
    decimals: "integer",
    source_chain: "string",
    source_asset_id: "uuid",
    source_symbol: "string",
    destination_chain: "string",
    destination_asset_id: "uuid",
    destination_symbol: "string",
    source_owner: "string",
    destination_owner: "string",
    nonce: "quantity",
    plan_hash: "string",
    status: LEG_STATUS,
    holding: oneOf(BRIDGE_LEG_HOLDINGS),
    created_at: "timestamp",
    updated_at: "timestamp",
  },
  optional: {
    failure_code: "string",
    lock_tx_id: "string",
    source_effect_status: EFFECT_STATUS,
    destination_effect_status: EFFECT_STATUS,
    refund_effect_status: EFFECT_STATUS,
    source_journal_transaction_id: "uuid",
    destination_journal_transaction_id: "uuid",
    refund_journal_transaction_id: "uuid",
  },
  objects: { reconciliation: { required: true, spec: bridgeReconciliationSpec } },
};

export const bridgeChainTxSpec: Spec = {
  required: { chain: "string", tx_id: "string", state: oneOf(BRIDGE_TX_STATES), final: "boolean" },
  optional: { height: "integer", error: "string" },
};

export const bridgeLegDetailSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: { leg: { required: true, spec: bridgeLegSpec }, lock: { spec: bridgeChainTxSpec } },
  arrays: {
    attempts: {
      required: true,
      spec: {
        required: {
          number: "integer",
          kind: oneOf(BRIDGE_ATTEMPT_KINDS),
          chain: "string",
          tx_id: "string",
          blockhash_height: "integer",
          created_at: "timestamp",
        },
        objects: { on_chain: { required: true, spec: bridgeChainTxSpec } },
      },
    },
    transitions: {
      required: true,
      spec: {
        required: {
          from_status: "string",
          to_status: LEG_STATUS,
          actor_type: "string",
          actor_id: "string",
          reason: "string",
          occurred_at: "timestamp",
        },
      },
    },
  },
};

export const bridgeLegPageSpec: Spec = {
  ...cursorPageSpec(bridgeLegSpec),
  required: { value_domain: SIMULATED },
};

export const bridgeLegResultSpec: Spec = {
  required: { value_domain: SIMULATED, replay: "boolean", settled: "boolean" },
  objects: { leg: { required: true, spec: bridgeLegSpec } },
};

export const bridgeChainSpec: Spec = {
  required: {
    id: oneOf(BRIDGE_CHAIN_IDS),
    tip_slot: "integer",
    tip_height: "integer",
    tip_hash: "string",
    finalized_height: "integer",
    blockhash_window: "integer",
    finality_depth: "integer",
    fee_per_tx: "quantity",
  },
  optional: { bridge_authority: "string" },
};

export const bridgeChainsSpec: Spec = {
  required: { value_domain: SIMULATED, network: "string", fault_injection_allowed: "boolean" },
  arrays: { chains: { required: true, spec: bridgeChainSpec } },
};

export const bridgeBlocksResultSpec: Spec = {
  required: { value_domain: SIMULATED, produced: "integer" },
  objects: { chain: { required: true, spec: bridgeChainSpec } },
};

export const bridgeFaultResultSpec: Spec = {
  required: { value_domain: SIMULATED, kind: oneOf(BRIDGE_FAULT_KINDS) },
  optional: { count: "integer" },
  objects: { chain: { required: true, spec: bridgeChainSpec } },
};

export const bridgeHoldingsSpec: Spec = {
  required: { value_domain: SIMULATED, account_id: "uuid", as_of: "timestamp", pending_locks_usd: "usd" },
  objects: {
    policy: {
      required: true,
      spec: { required: { version: "string", in_flight_bps: "integer", observed_bps: "integer" } },
    },
  },
  arrays: {
    chains: {
      required: true,
      spec: {
        required: { chain: "string" },
        arrays: {
          holdings: {
            required: true,
            spec: { required: { asset_id: "uuid", symbol: "string", decimals: "integer", quantity: "quantity" } },
          },
        },
      },
    },
    in_flight: {
      required: true,
      spec: {
        required: {
          asset_id: "uuid",
          symbol: "string",
          decimals: "integer",
          quantity: "quantity",
          marked_usd: "usd",
          factor_bps: "integer",
          reason: "string",
        },
      },
    },
    legs: {
      required: true,
      spec: {
        required: {
          leg_id: "uuid",
          route_id: "uuid",
          asset_id: "uuid",
          symbol: "string",
          decimals: "integer",
          status: LEG_STATUS,
          holding: oneOf(BRIDGE_LEG_HOLDINGS),
          quantity: "quantity",
          factor_bps: "integer",
          reason: "string",
        },
      },
    },
  },
};

export const bridgeRouteReconciliationSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    route_id: "uuid",
    locked: "quantity",
    outstanding: "quantity",
    in_flight: "quantity",
    refunded: "quantity",
    escrow: "quantity",
    destination_supply: "quantity",
    landed_unposted_locks: "quantity",
    landed_unposted_releases: "quantity",
    expected_escrow: "quantity",
    expected_supply: "quantity",
    books_conserve: "boolean",
    escrow_matches: "boolean",
    supply_matches: "boolean",
    matched: "boolean",
    breaker_tripped: "boolean",
  },
  optional: { record_id: "uuid" },
};

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

export function useBridgeRoutes(): UseQueryResult<BridgeRouteList> {
  return useQuery({
    queryKey: bridgeKeys.routes,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/bridge/routes", { params: { query: { limit: BRIDGE_ROUTE_LIMIT } } });
      return validated<BridgeRouteList>(data, bridgeRouteListSpec, "/advanced/bridge/routes");
    },
  });
}

export function useBridgeChains(): UseQueryResult<BridgeChains> {
  return useQuery({
    queryKey: bridgeKeys.chains,
    // The tips move with every block: read fresh, never reused.
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/bridge/chains", {});
      return validated<BridgeChains>(data, bridgeChainsSpec, "/advanced/bridge/chains");
    },
  });
}

export function useBridgeHoldings(accountId: string | undefined): UseQueryResult<BridgeHoldings> {
  return useQuery({
    queryKey: bridgeKeys.holdings(accountId ?? ""),
    enabled: accountId !== undefined,
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/bridge/holdings", { params: { query: { account_id: accountId ?? "" } } });
      return validated<BridgeHoldings>(data, bridgeHoldingsSpec, "/advanced/bridge/holdings");
    },
  });
}

export function useBridgeLegs(accountId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<BridgeLeg>> {
  return useQuery({
    queryKey: bridgeKeys.legs(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/bridge/legs", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit: BRIDGE_PAGE_SIZE,
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["BridgeLegPage"]>(data, bridgeLegPageSpec, "/advanced/bridge/legs");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/**
 * One leg with its transactions on both chains. The backend scopes it by the
 * leg's own account and answers NOT_FOUND to anyone who may not read it.
 */
export function useBridgeLeg(accountId: string | undefined, legId: string | undefined): UseQueryResult<BridgeLegDetail> {
  return useQuery({
    queryKey: bridgeKeys.leg(accountId ?? "", legId ?? ""),
    enabled: legId !== undefined && legId !== "",
    // The chains' word on each transaction is read at the instant of the read.
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/bridge/legs/{bridgeLegId}", {
        params: { path: { bridgeLegId: legId ?? "" } },
      });
      return validated<BridgeLegDetail>(data, bridgeLegDetailSpec, "/advanced/bridge/legs/{id}");
    },
  });
}

/* --------------------------------------------------------------------------
 * Commands
 * ------------------------------------------------------------------------ */

/** A mutation of one confirmed attempt; the attempt carries its idempotency key. */
export type BridgeCommand<R, T> = UseMutationResult<T, unknown, ConfirmedAttempt<R>>;

export interface TransferRequest {
  readonly accountId: string;
  readonly routeId: string;
  /** Exact base units of the route's source asset. */
  readonly quantity: string;
  readonly mode: BridgeLegMode;
}

export function useBridgeTransfer(): BridgeCommand<TransferRequest, BridgeLegResult> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ request, idempotencyKey }: ConfirmedAttempt<TransferRequest>) => {
      const { data } = await api.POST("/advanced/bridge/legs", {
        ...idempotent(idempotencyKey),
        body: { account_id: request.accountId, route_id: request.routeId, quantity: request.quantity, mode: request.mode },
      });
      return validated<BridgeLegResult>(data, bridgeLegResultSpec, "/advanced/bridge/legs");
    },
    onSettled: (_data, _error, attempt) => {
      // A refusal is recorded too, so the reads are refreshed whatever the answer was.
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(attempt.request.accountId) });
      void queryClient.invalidateQueries({ queryKey: bridgeKeys.all });
      void queryClient.invalidateQueries({ queryKey: stableKeys.all });
    },
  });
}

/* Operator commands (sandbox:operate, each with a recent step-up except the route reconciliation). */

export interface RouteDefinitionRequest {
  readonly name: string;
  readonly sourceAssetId: string;
  readonly destinationChain: BridgeChainId;
  readonly destinationSymbol: string;
  /** The value of one whole unit as typed: an exact decimal. */
  readonly unitPrice: string;
  readonly maxReleaseAttempts: number;
}

export interface RouteActionRequest {
  readonly routeId: string;
}

export interface RouteReasonRequest extends RouteActionRequest {
  readonly reason: string;
}

export interface LegActionRequest {
  readonly legId: string;
}

export interface BlocksRequest {
  readonly chain: BridgeChainId;
  readonly count: number;
}

export interface FaultRequest {
  readonly chain: BridgeChainId;
  readonly kind: BridgeFaultKind;
  readonly count: number | undefined;
}

function useOperatorCommand<R, T>(run: (attempt: ConfirmedAttempt<R>) => Promise<T>): BridgeCommand<R, T> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: run,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export function useCreateBridgeRoute(): BridgeCommand<RouteDefinitionRequest, BridgeRoute> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/bridge/routes", {
      ...idempotent(idempotencyKey),
      body: {
        name: request.name,
        source_asset_id: request.sourceAssetId,
        destination_chain: request.destinationChain,
        destination_symbol: request.destinationSymbol,
        unit_price: request.unitPrice,
        max_release_attempts: request.maxReleaseAttempts,
      },
    });
    return validated<BridgeRoute>(data, bridgeRouteSpec, "/advanced/bridge/routes");
  });
}

export function usePauseBridgeRoute(): BridgeCommand<RouteReasonRequest, BridgeRoute> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/bridge/routes/{bridgeRouteId}/pause", {
      ...idempotent(idempotencyKey, { path: { bridgeRouteId: request.routeId } }),
      body: { reason: request.reason },
    });
    return validated<BridgeRoute>(data, bridgeRouteSpec, "/advanced/bridge/routes/{id}/pause");
  });
}

export function useResumeBridgeRoute(): BridgeCommand<RouteReasonRequest, BridgeRoute> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/bridge/routes/{bridgeRouteId}/resume", {
      ...idempotent(idempotencyKey, { path: { bridgeRouteId: request.routeId } }),
      body: { reason: request.reason },
    });
    return validated<BridgeRoute>(data, bridgeRouteSpec, "/advanced/bridge/routes/{id}/resume");
  });
}

export function useReconcileBridgeRoute(): BridgeCommand<RouteActionRequest, BridgeRouteReconciliation> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/bridge/routes/{bridgeRouteId}/reconciliations", {
      ...idempotent(idempotencyKey, { path: { bridgeRouteId: request.routeId } }),
    });
    return validated<BridgeRouteReconciliation>(data, bridgeRouteReconciliationSpec, "/advanced/bridge/routes/{id}/reconciliations");
  });
}

export function useReconcileBridgeLeg(): BridgeCommand<LegActionRequest, BridgeLegResult> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/bridge/legs/{bridgeLegId}/reconcile", {
      ...idempotent(idempotencyKey, { path: { bridgeLegId: request.legId } }),
    });
    return validated<BridgeLegResult>(data, bridgeLegResultSpec, "/advanced/bridge/legs/{id}/reconcile");
  });
}

export function useProduceBlocks(): BridgeCommand<BlocksRequest, BridgeBlocksResult> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/bridge/chains/{bridgeChainId}/blocks", {
      ...idempotent(idempotencyKey, { path: { bridgeChainId: request.chain } }),
      body: { count: request.count },
    });
    return validated<BridgeBlocksResult>(data, bridgeBlocksResultSpec, "/advanced/bridge/chains/{id}/blocks");
  });
}

export function useInjectFault(): BridgeCommand<FaultRequest, BridgeFaultResult> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/bridge/chains/{bridgeChainId}/faults", {
      ...idempotent(idempotencyKey, { path: { bridgeChainId: request.chain } }),
      body: request.count === undefined ? { kind: request.kind } : { kind: request.kind, count: request.count },
    });
    return validated<BridgeFaultResult>(data, bridgeFaultResultSpec, "/advanced/bridge/chains/{id}/faults");
  });
}
