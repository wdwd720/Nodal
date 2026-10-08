/**
 * Prediction markets (internal/predmarket, ADR-0020): reads of markets, their
 * escrow books, the oracle and an account's shares; the customer's
 * complete-set issuance and redemption; and the operator's lifecycle,
 * resolution and settlement actions.
 *
 * Follows `core.ts`: every response is checked against a spec before a
 * component sees it (quantities are exact base-unit strings, enums are closed,
 * every answer states `value_domain: SIMULATED`), nothing is repaired, and a
 * 4xx answer is the backend's decision and is not retried. Every command
 * carries an Idempotency-Key the caller created once, when the person
 * confirmed, and reuses on every retry of that same action.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  MARKET_STATUSES,
  PREDICTION_MODES,
  RECONCILIATION_STATUSES,
  RESOLUTION_KINDS,
  ROUNDING_POLICIES,
  SET_ACTIONS,
  SETTLEMENT_KINDS,
  type SetAction,
} from "../../lib/predmarket.ts";
import { api } from "../client.ts";
import { oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, economicEffectSpec, economicIntentSpec, retryTransient } from "./core.ts";

export type PredictionMarket = Schemas["PredictionMarket"];
export type PredictionMarketList = Schemas["PredictionMarketList"];
export type PredictionMarketDetail = Schemas["PredictionMarketDetail"];
export type PredictionOutcome = Schemas["PredictionOutcome"];
export type PredictionResolution = Schemas["PredictionResolution"];
export type PredictionDispute = Schemas["PredictionDispute"];
export type PredictionSettlement = Schemas["PredictionSettlement"];
export type PredictionEscrowBooks = Schemas["PredictionEscrowBooks"];
export type PredictionPositionList = Schemas["PredictionPositionList"];
export type PredictionOracle = Schemas["PredictionOracle"];
export type PredictionSetResult = Schemas["PredictionSetResult"];
export type PredictionOracleObservation = Schemas["PredictionOracleObservation"];
export type PredictionResolutionResult = Schemas["PredictionResolutionResult"];
export type PredictionDisputeResult = Schemas["PredictionDisputeResult"];
export type PredictionSettlementResult = Schemas["PredictionSettlementResult"];
export type PredictionEscrowReconciliation = Schemas["PredictionEscrowReconciliation"];

/** The service lists at most this many markets per status; the API has no cursor for them. */
export const MARKET_LIST_LIMIT = 200;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const predmarketKeys = {
  /** Prefix of every market-level read: lists, markets, books, the oracle. */
  all: ["advanced", "prediction-markets"] as const,
  list: (status: string, limit: number) => ["advanced", "prediction-markets", "list", status, limit] as const,
  market: (marketId: string) => ["advanced", "prediction-markets", "market", marketId] as const,
  books: (marketId: string) => ["advanced", "prediction-markets", "books", marketId] as const,
  oracle: ["advanced", "prediction-markets", "oracle"] as const,
  /** An account's shares sit with that account's other rows. */
  positions: (accountId: string, marketId: string) =>
    [...advancedKeys.account(accountId), "prediction-positions", marketId] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
const STATUS = oneOf(MARKET_STATUSES);

export const actorSpec: Spec = { required: { actor_type: "string", actor_id: "string" } };

export const outcomeSpec: Spec = {
  required: { index: "integer", symbol: "string", share_asset_id: "uuid", instrument_id: "uuid" },
};

export const predictionMarketSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    symbol: "string",
    question: "string",
    mode: oneOf(PREDICTION_MODES),
    status: STATUS,
    facility_id: "uuid",
    instrument_id: "uuid",
    collateral_asset_id: "uuid",
    collateral_symbol: "string",
    collateral_decimals: "integer",
    unit_quantity: "quantity",
    share_decimals: "integer",
    open_at: "timestamp",
    close_at: "timestamp",
    dispute_window_ms: "integer",
    oracle_source: "string",
    oracle_public_key: "string",
    definition_hash: "string",
    created_at: "timestamp",
    updated_at: "timestamp",
  },
  optional: { current_resolution_id: "uuid" },
  arrays: { outcomes: { required: true, spec: outcomeSpec } },
  objects: { created_by: { required: true, spec: actorSpec } },
};

export const marketListSpec: Spec = {
  required: { value_domain: SIMULATED, status: STATUS },
  arrays: { items: { required: true, spec: predictionMarketSpec } },
};

export const resolutionSpec: Spec = {
  required: {
    id: "uuid",
    market_id: "uuid",
    kind: oneOf(RESOLUTION_KINDS),
    outcome_index: "integer",
    outcome_symbol: "string",
    evidence_ref: "string",
    evidence_sha256: "string",
    reason: "string",
    accepted_at: "timestamp",
  },
  optional: {
    supersedes_id: "uuid",
    dispute_id: "uuid",
    report_hash: "string",
    report_source: "string",
    report_observed_at: "timestamp",
  },
  objects: { accepted_by: { required: true, spec: actorSpec } },
};

export const disputeSpec: Spec = {
  required: {
    id: "uuid",
    market_id: "uuid",
    resolution_id: "uuid",
    reason: "string",
    evidence_ref: "string",
    evidence_sha256: "string",
    raised_at: "timestamp",
  },
  objects: { raised_by: { required: true, spec: actorSpec } },
};

export const settlementSpec: Spec = {
  required: {
    id: "uuid",
    market_id: "uuid",
    kind: oneOf(SETTLEMENT_KINDS),
    outcome_count: "integer",
    unit_quantity: "quantity",
    rounding_policy: oneOf(ROUNDING_POLICIES),
    escrow_before: "quantity",
    total_paid: "quantity",
    residual: "quantity",
    holder_count: "integer",
    evidence_ref: "string",
    evidence_sha256: "string",
    settled_at: "timestamp",
  },
  optional: {
    resolution_id: "uuid",
    winning_outcome_index: "integer",
    winning_outcome_symbol: "string",
    refund_rate_per_share: "quantity",
  },
  objects: { settled_by: { required: true, spec: actorSpec } },
};

export const marketDetailSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: {
    market: { required: true, spec: predictionMarketSpec },
    settlement: { spec: settlementSpec },
  },
  arrays: {
    resolutions: { required: true, spec: resolutionSpec },
    disputes: { required: true, spec: disputeSpec },
  },
};

export const booksSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    market_id: "uuid",
    market_status: STATUS,
    collateral_asset_id: "uuid",
    collateral_symbol: "string",
    collateral_decimals: "integer",
    share_decimals: "integer",
    escrow: "quantity",
    collateral: "quantity",
    residual: "quantity",
    reconciles: "boolean",
  },
  arrays: {
    outcomes: {
      required: true,
      spec: {
        required: {
          index: "integer",
          symbol: "string",
          share_asset_id: "uuid",
          shares_issued: "quantity",
          shares_outstanding: "quantity",
          held_by_customers: "quantity",
        },
      },
    },
  },
  lists: { violations: { required: true, kind: "string" } },
};

export const positionListSpec: Spec = {
  required: { value_domain: SIMULATED, market_id: "uuid", account_id: "uuid", share_decimals: "integer" },
  arrays: {
    items: {
      required: true,
      spec: {
        required: {
          outcome_index: "integer",
          outcome_symbol: "string",
          share_asset_id: "uuid",
          quantity: "quantity",
          as_of: "timestamp",
        },
      },
    },
  },
};

export const oracleSpec: Spec = { required: { kind: oneOf(["SANDBOX"]), source: "string", public_key: "string" } };

export const setResultSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    market_id: "uuid",
    account_id: "uuid",
    action: oneOf(SET_ACTIONS),
    quantity: "quantity",
    replay: "boolean",
  },
  optional: { journal_transaction_id: "uuid", risk_decision_hash: "string" },
  objects: {
    intent: { required: true, spec: economicIntentSpec },
    effect: { spec: economicEffectSpec },
  },
};

export const observationSpec: Spec = {
  required: { value_domain: SIMULATED, report_hash: "string" },
  objects: {
    report: {
      required: true,
      spec: {
        required: {
          market_id: "uuid",
          outcome: "string",
          source: "string",
          observed_at: "timestamp",
          evidence: "string",
          signature: "string",
        },
      },
    },
  },
};

export const resolutionResultSpec: Spec = {
  required: { value_domain: SIMULATED, replay: "boolean" },
  objects: {
    market: { required: true, spec: predictionMarketSpec },
    resolution: { required: true, spec: resolutionSpec },
  },
};

export const disputeResultSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: {
    market: { required: true, spec: predictionMarketSpec },
    dispute: { required: true, spec: disputeSpec },
  },
};

export const settlementResultSpec: Spec = {
  required: { value_domain: SIMULATED, replay: "boolean" },
  objects: {
    market: { required: true, spec: predictionMarketSpec },
    settlement: { required: true, spec: settlementSpec },
  },
  arrays: {
    lines: {
      required: true,
      spec: {
        required: { account_id: "uuid", amount: "quantity", intent_id: "uuid" },
        optional: { journal_transaction_id: "uuid" },
        arrays: {
          shares: { required: true, spec: { required: { outcome_symbol: "string", quantity: "quantity" } } },
        },
      },
    },
  },
};

export const reconciliationSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    kind: oneOf(["PREDICTION_ESCROW"]),
    mode: "string",
    scope_type: "string",
    scope_id: "string",
    status: oneOf(RECONCILIATION_STATUSES),
    material: "boolean",
    blocks_new_risk: "boolean",
    expected: "object",
    observed: "object",
    difference: "object",
    opened_at: "timestamp",
  },
  optional: { matched_at: "timestamp" },
};

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

export function usePredictionMarkets(status: string): UseQueryResult<PredictionMarketList> {
  return useQuery({
    queryKey: predmarketKeys.list(status, MARKET_LIST_LIMIT),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/prediction-markets", {
        params: { query: { status, limit: MARKET_LIST_LIMIT } },
      });
      return validated<PredictionMarketList>(data, marketListSpec, "/advanced/prediction-markets");
    },
  });
}

export function usePredictionMarket(marketId: string | undefined): UseQueryResult<PredictionMarketDetail> {
  return useQuery({
    queryKey: predmarketKeys.market(marketId ?? ""),
    enabled: marketId !== undefined && marketId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/prediction-markets/{marketId}", {
        params: { path: { marketId: marketId ?? "" } },
      });
      return validated<PredictionMarketDetail>(data, marketDetailSpec, "/advanced/prediction-markets/{id}");
    },
  });
}

export function usePredictionBooks(marketId: string | undefined): UseQueryResult<PredictionEscrowBooks> {
  return useQuery({
    queryKey: predmarketKeys.books(marketId ?? ""),
    enabled: marketId !== undefined && marketId !== "",
    // One snapshot of the books at the moment it was read; never reused as current.
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/prediction-markets/{marketId}/books", {
        params: { path: { marketId: marketId ?? "" } },
      });
      return validated<PredictionEscrowBooks>(data, booksSpec, "/advanced/prediction-markets/{id}/books");
    },
  });
}

export function usePredictionPositions(
  accountId: string | undefined,
  marketId: string | undefined,
): UseQueryResult<PredictionPositionList> {
  return useQuery({
    queryKey: predmarketKeys.positions(accountId ?? "", marketId ?? ""),
    enabled: accountId !== undefined && marketId !== undefined && marketId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/prediction-markets/{marketId}/positions", {
        params: { path: { marketId: marketId ?? "" }, query: { account_id: accountId ?? "" } },
      });
      return validated<PredictionPositionList>(data, positionListSpec, "/advanced/prediction-markets/{id}/positions");
    },
  });
}

export function usePredictionOracle(enabled: boolean): UseQueryResult<PredictionOracle> {
  return useQuery({
    queryKey: predmarketKeys.oracle,
    enabled,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/prediction-markets/oracle", {});
      return validated<PredictionOracle>(data, oracleSpec, "/advanced/prediction-markets/oracle");
    },
  });
}

/* --------------------------------------------------------------------------
 * The customer's complete sets
 * ------------------------------------------------------------------------ */

export interface SetCommand {
  readonly action: SetAction;
  readonly marketId: string;
  /** The session's own account: a write never acts on an account the principal does not own. */
  readonly accountId: string;
  /** Share base units, a whole number of complete sets. */
  readonly quantity: string;
  /** Created once when the person confirmed, reused for every retry of this action. */
  readonly idempotencyKey: string;
}

export function useCompleteSets(): UseMutationResult<PredictionSetResult, unknown, SetCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: SetCommand) => {
      const params = idempotent(input.idempotencyKey, { path: { marketId: input.marketId } });
      const body = { account_id: input.accountId, quantity: input.quantity };
      const { data } =
        input.action === "ISSUE"
          ? await api.POST("/advanced/prediction-markets/{marketId}/issuances", { ...params, body })
          : await api.POST("/advanced/prediction-markets/{marketId}/redemptions", { ...params, body });
      return validated<PredictionSetResult>(data, setResultSpec, "/advanced/prediction-markets/{id}/sets");
    },
    onSettled: (_result, _error, input) => {
      // A refusal can still have changed what the backend records (a
      // REJECTED intent), so the account's rows are refetched either way.
      void queryClient.invalidateQueries({ queryKey: predmarketKeys.all });
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(input.accountId) });
    },
  });
}

/* --------------------------------------------------------------------------
 * The operator's actions
 * ------------------------------------------------------------------------ */

export interface CreateMarketCommand {
  readonly idempotencyKey: string;
  readonly body: Schemas["CreatePredictionMarketRequest"];
}

export function useCreatePredictionMarket(): UseMutationResult<PredictionMarket, unknown, CreateMarketCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: CreateMarketCommand) => {
      const { data } = await api.POST("/advanced/prediction-markets", {
        ...idempotent(input.idempotencyKey),
        body: input.body,
      });
      return validated<PredictionMarket>(data, predictionMarketSpec, "/advanced/prediction-markets");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: predmarketKeys.all });
    },
  });
}

/** A step that takes no body and answers with the market after it. */
export type MarketStep = "open" | "close" | "request-resolution";

export interface StepCommand {
  readonly marketId: string;
  readonly idempotencyKey: string;
}

export function useMarketStep(step: MarketStep): UseMutationResult<PredictionMarket, unknown, StepCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: StepCommand) => {
      const params = idempotent(input.idempotencyKey, { path: { marketId: input.marketId } });
      const { data } =
        step === "open"
          ? await api.POST("/advanced/prediction-markets/{marketId}/open", params)
          : step === "close"
            ? await api.POST("/advanced/prediction-markets/{marketId}/close", params)
            : await api.POST("/advanced/prediction-markets/{marketId}/request-resolution", params);
      return validated<PredictionMarket>(data, predictionMarketSpec, `/advanced/prediction-markets/{id}/${step}`);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: predmarketKeys.all });
    },
  });
}

export interface ObserveCommand {
  readonly marketId: string;
  readonly idempotencyKey: string;
  readonly outcome: string;
  readonly observedAt: string;
  readonly evidence: string;
}

export function useObserveOutcome(): UseMutationResult<PredictionOracleObservation, unknown, ObserveCommand> {
  return useMutation({
    mutationFn: async (input: ObserveCommand) => {
      const { data } = await api.POST("/advanced/prediction-markets/{marketId}/oracle-observations", {
        ...idempotent(input.idempotencyKey, { path: { marketId: input.marketId } }),
        body: { outcome: input.outcome, observed_at: input.observedAt, evidence: input.evidence },
      });
      return validated<PredictionOracleObservation>(
        data,
        observationSpec,
        "/advanced/prediction-markets/{id}/oracle-observations",
      );
    },
  });
}

export interface ReasonCommand {
  readonly marketId: string;
  readonly idempotencyKey: string;
  readonly reason: string;
}

export function useResolveMarket(): UseMutationResult<PredictionResolutionResult, unknown, ReasonCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: ReasonCommand) => {
      const { data } = await api.POST("/advanced/prediction-markets/{marketId}/resolve", {
        ...idempotent(input.idempotencyKey, { path: { marketId: input.marketId } }),
        body: input.reason.trim() === "" ? {} : { reason: input.reason.trim() },
      });
      return validated<PredictionResolutionResult>(data, resolutionResultSpec, "/advanced/prediction-markets/{id}/resolve");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: predmarketKeys.all });
    },
  });
}

export interface EvidenceCommand extends ReasonCommand {
  /** A short statement of the evidence, archived as the evidence document. */
  readonly evidence: string;
}

/** The evidence document the API archives: what the operator stated, as they stated it. */
function evidenceDocument(evidence: string): Record<string, unknown> {
  return { statement: evidence.trim() };
}

export function useDisputeMarket(): UseMutationResult<PredictionDisputeResult, unknown, EvidenceCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: EvidenceCommand) => {
      const { data } = await api.POST("/advanced/prediction-markets/{marketId}/dispute", {
        ...idempotent(input.idempotencyKey, { path: { marketId: input.marketId } }),
        body: { reason: input.reason.trim(), evidence: evidenceDocument(input.evidence) },
      });
      return validated<PredictionDisputeResult>(data, disputeResultSpec, "/advanced/prediction-markets/{id}/dispute");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: predmarketKeys.all });
    },
  });
}

export function useUpholdResolution(): UseMutationResult<PredictionResolutionResult, unknown, EvidenceCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: EvidenceCommand) => {
      const { data } = await api.POST("/advanced/prediction-markets/{marketId}/uphold", {
        ...idempotent(input.idempotencyKey, { path: { marketId: input.marketId } }),
        body:
          input.evidence.trim() === ""
            ? { reason: input.reason.trim() }
            : { reason: input.reason.trim(), evidence: evidenceDocument(input.evidence) },
      });
      return validated<PredictionResolutionResult>(data, resolutionResultSpec, "/advanced/prediction-markets/{id}/uphold");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: predmarketKeys.all });
    },
  });
}

/** Settling and voiding move value: every holder's account is affected. */
function invalidateAfterSettlement(queryClient: ReturnType<typeof useQueryClient>): void {
  void queryClient.invalidateQueries({ queryKey: predmarketKeys.all });
  void queryClient.invalidateQueries({ queryKey: ["advanced", "account"] });
}

export function useVoidMarket(): UseMutationResult<PredictionSettlementResult, unknown, EvidenceCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: EvidenceCommand) => {
      const { data } = await api.POST("/advanced/prediction-markets/{marketId}/void", {
        ...idempotent(input.idempotencyKey, { path: { marketId: input.marketId } }),
        body:
          input.evidence.trim() === ""
            ? { reason: input.reason.trim() }
            : { reason: input.reason.trim(), evidence: evidenceDocument(input.evidence) },
      });
      return validated<PredictionSettlementResult>(data, settlementResultSpec, "/advanced/prediction-markets/{id}/void");
    },
    onSettled: () => {
      invalidateAfterSettlement(queryClient);
    },
  });
}

export function useSettleMarket(): UseMutationResult<PredictionSettlementResult, unknown, StepCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: StepCommand) => {
      const { data } = await api.POST(
        "/advanced/prediction-markets/{marketId}/settle",
        idempotent(input.idempotencyKey, { path: { marketId: input.marketId } }),
      );
      return validated<PredictionSettlementResult>(data, settlementResultSpec, "/advanced/prediction-markets/{id}/settle");
    },
    onSettled: () => {
      invalidateAfterSettlement(queryClient);
    },
  });
}

export function useReconcileMarket(): UseMutationResult<PredictionEscrowReconciliation, unknown, StepCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: StepCommand) => {
      const { data } = await api.POST(
        "/advanced/prediction-markets/{marketId}/reconcile",
        idempotent(input.idempotencyKey, { path: { marketId: input.marketId } }),
      );
      return validated<PredictionEscrowReconciliation>(
        data,
        reconciliationSpec,
        "/advanced/prediction-markets/{id}/reconcile",
      );
    },
    onSuccess: (_record, input) => {
      void queryClient.invalidateQueries({ queryKey: predmarketKeys.books(input.marketId) });
    },
  });
}
