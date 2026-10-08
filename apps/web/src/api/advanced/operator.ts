/**
 * The operator console's reads and its one command (ADR-0020), following the
 * pattern of `core.ts`:
 *
 *   - every response is checked against a spec before a component sees it:
 *     a reference price is an exact decimal string with its mantissa in exact
 *     integer digits, durations and counts are whole numbers, statuses that
 *     other subsystems store are plain strings exactly as their tables hold
 *     them, and every response says `value_domain: SIMULATED`;
 *   - every key sits under `operatorKeys.all`, itself under the advanced
 *     prefix, so recording a mark refreshes everything a mark can move;
 *   - a 4xx answer is the backend's decision (a customer reading the console
 *     is refused; a deployment with no simulated chain host answers
 *     UNSUPPORTED) and is not retried.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import type { ConfirmedAttempt } from "../../lib/margin.ts";
import { api } from "../client.ts";
import { oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient } from "./core.ts";

export type OpsOverview = Schemas["AdvancedOpsOverview"];
export type OpsKillSwitch = Schemas["AdvancedOpsKillSwitch"];
export type OpsStableAsset = Schemas["AdvancedOpsStableAsset"];
export type OpsBridgeRoute = Schemas["AdvancedOpsBridgeRoute"];
export type OpsReconciliationKind = Schemas["AdvancedOpsReconciliationKind"];
export type OpsReconciliationRecord = Schemas["AdvancedOpsReconciliationRecord"];
export type OpsOracleAsset = Schemas["AdvancedOpsOracleAsset"];
export type OpsChains = Schemas["AdvancedOpsChains"];
export type ReferenceMark = Schemas["AdvancedReferenceMark"];
export type ReferenceMarkList = Schemas["AdvancedReferenceMarkList"];
export type ReferenceMarkRecorded = Schemas["AdvancedReferenceMarkRecorded"];

/** How many of an asset's latest marks the console reads. */
export const MARK_LIST_LIMIT = 50;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const operatorKeys = {
  all: [...advancedKeys.all, "operator"] as const,
  overview: [...advancedKeys.all, "operator", "overview"] as const,
  marks: (assetId: string) => [...advancedKeys.all, "operator", "marks", assetId] as const,
  chains: [...advancedKeys.all, "operator", "chains"] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);

export const referenceMarkSpec: Spec = {
  required: {
    id: "uuid",
    asset_id: "uuid",
    quote_asset_id: "uuid",
    price: "decimal",
    mantissa: "quantity",
    scale: "integer",
    source: "string",
    observed_at: "timestamp",
    received_at: "timestamp",
    age_ms: "integer",
    stale: "boolean",
    future: "boolean",
  },
};

export const referenceMarkRecordedSpec: Spec = {
  required: {
    asset_symbol: "string",
    quote_asset_symbol: "string",
    duplicate: "boolean",
    max_age_ms: "integer",
    value_domain: SIMULATED,
  },
  objects: { mark: { required: true, spec: referenceMarkSpec } },
};

export const referenceMarkListSpec: Spec = {
  required: {
    asset_id: "uuid",
    asset_symbol: "string",
    asset_kind: "string",
    quote_asset_id: "uuid",
    quote_asset_symbol: "string",
    max_age_ms: "integer",
    as_of: "timestamp",
    value_domain: SIMULATED,
  },
  arrays: { items: { required: true, spec: referenceMarkSpec } },
};

const reconciliationRecordSpec: Spec = {
  required: {
    id: "uuid",
    kind: "string",
    status: "string",
    material: "boolean",
    blocks_new_risk: "boolean",
    opened_at: "timestamp",
  },
  optional: { resolved_at: "timestamp" },
};

export const opsOverviewSpec: Spec = {
  required: {
    generated_at: "timestamp",
    environment: "string",
    max_mark_age_ms: "integer",
    chains_configured: "boolean",
    value_domain: SIMULATED,
  },
  objects: {
    dual_control: {
      required: true,
      spec: {
        required: {
          transfer_reversals_pending: "integer",
          margin_cases_in_review: "integer",
          margin_resolutions_pending: "integer",
        },
      },
    },
  },
  arrays: {
    kill_switches: {
      required: true,
      spec: {
        required: { kind: "string", scope_id: "string", severity: "string" },
        optional: { reason: "string", activated_by: "string", activated_at: "timestamp" },
      },
    },
    stable_assets: {
      required: true,
      spec: {
        required: { id: "uuid", name: "string", asset_id: "uuid", chain_id: "string", status: "string" },
        objects: {
          latest_alert: {
            spec: {
              required: { kind: "string", severity: "string", breaker_tripped: "boolean", observed_at: "timestamp" },
            },
          },
          latest_supply_reconciliation: { spec: reconciliationRecordSpec },
        },
      },
    },
    bridge_routes: {
      required: true,
      spec: {
        required: { id: "uuid", name: "string", source_chain: "string", destination_chain: "string", status: "string" },
        objects: { open_reconciliation: { spec: reconciliationRecordSpec } },
      },
    },
    reconciliation: {
      required: true,
      spec: {
        required: { kind: "string", unresolved: "integer", material: "integer", blocks_new_risk: "integer" },
        optional: { oldest_opened_at: "timestamp", oldest_age_ms: "integer" },
      },
    },
    oracle: {
      required: true,
      spec: {
        required: { asset_id: "uuid", symbol: "string", stale: "boolean" },
        optional: { observed_at: "timestamp", source: "string", age_ms: "integer" },
      },
    },
  },
};

export const opsChainsSpec: Spec = {
  required: { network: "string", observed_at: "timestamp", value_domain: SIMULATED },
  arrays: {
    items: {
      required: true,
      spec: {
        required: {
          chain_id: "string",
          slot: "integer",
          height: "integer",
          hash: "string",
          final: "boolean",
          finalized_height: "integer",
        },
        optional: { last_block_produced_at: "timestamp" },
      },
    },
  },
};

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

/** One read of what needs an operator now. Computed by the backend on every call. */
export function useOpsOverview(): UseQueryResult<OpsOverview> {
  return useQuery({
    queryKey: operatorKeys.overview,
    staleTime: 0,
    refetchOnWindowFocus: true,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/ops/overview", {});
      return validated<OpsOverview>(data, opsOverviewSpec, "/advanced/ops/overview");
    },
  });
}

/** An asset's latest reference marks in the sandbox dollar, newest first, with their age at the read. */
export function useReferenceMarks(assetId: string | undefined): UseQueryResult<ReferenceMarkList> {
  return useQuery({
    queryKey: operatorKeys.marks(assetId ?? ""),
    enabled: assetId !== undefined && assetId !== "",
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/ops/marks", {
        params: { query: { asset_id: assetId ?? "", limit: MARK_LIST_LIMIT } },
      });
      return validated<ReferenceMarkList>(data, referenceMarkListSpec, "/advanced/ops/marks");
    },
  });
}

/** The tip of each simulated chain, or the backend's UNSUPPORTED when no devnet host runs. */
export function useOpsChains(): UseQueryResult<OpsChains> {
  return useQuery({
    queryKey: operatorKeys.chains,
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/ops/chains", {});
      return validated<OpsChains>(data, opsChainsSpec, "/advanced/ops/chains");
    },
  });
}

/* --------------------------------------------------------------------------
 * The command
 * ------------------------------------------------------------------------ */

/** A reference mark exactly as the operator confirmed it. */
export interface MarkRequest {
  readonly assetId: string;
  /** Sandbox dollars per one whole unit, as typed. */
  readonly price: string;
  readonly source: string;
  /** RFC 3339; absent means the time of the request. */
  readonly observedAt: string | undefined;
}

/**
 * Records a reference mark. The attempt carries the idempotency key the page
 * created when the operator confirmed, reused for every retry of that same
 * request. A mark moves every valuation of its asset -- margin collateral,
 * buying power, the stable asset's peg, prediction shares -- so every
 * advanced read is refetched whatever the answer was.
 */
export function useRecordReferenceMark(): UseMutationResult<ReferenceMarkRecorded, unknown, ConfirmedAttempt<MarkRequest>> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ request, idempotencyKey }: ConfirmedAttempt<MarkRequest>) => {
      const { data } = await api.POST("/advanced/ops/marks", {
        ...idempotent(idempotencyKey),
        body: {
          asset_id: request.assetId,
          price: request.price,
          source: request.source,
          ...(request.observedAt === undefined ? {} : { observed_at: request.observedAt }),
        },
      });
      return validated<ReferenceMarkRecorded>(data, referenceMarkRecordedSpec, "/advanced/ops/marks");
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}
