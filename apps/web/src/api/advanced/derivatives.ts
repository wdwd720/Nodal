/**
 * Reads and commands of the derivatives section (internal/derivatives,
 * ADR-0020): contracts, marks, the margin and funding policy, an account's
 * positions with the backend's valuation of its book, the clearing-house flow
 * register, funding accruals, reference option analytics, the customer's
 * open / close / exercise, and the operator's marks, runs, settlement and
 * clearing house.
 *
 * Like core.ts, which it follows:
 *
 *   - every response is checked against a spec before a component sees it,
 *     including the one rule a per-field spec cannot state: an answer that
 *     says it valued something carries the valuation, and one that says the
 *     backend refused carries the refusal — never neither;
 *   - query keys start with `advancedKeys.all`, and account-scoped ones with
 *     `advancedKeys.account(accountId)`, so a command invalidates everything
 *     it may have changed for the account with one key;
 *   - a 4xx answer is the backend's decision and is not retried;
 *   - every command carries the idempotency key its form created once, when
 *     the person confirmed, and reuses it for every retry.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  BOOK_STATUSES,
  DERIVATIVE_KINDS,
  DERIVATIVE_SIDES,
  FILL_SIDES,
  FILL_SOURCES,
  FLOW_KINDS,
  FUNDING_DIRECTIONS,
  OVERRIDE_DECISIONS,
  OVERRIDE_STATUSES,
  POSITION_STATUSES,
  PRICE_RULES,
  RUN_KINDS,
  SETTLEMENT_KINDS,
  SETTLEMENT_METHODS,
  VALUATION_STATUSES,
  type ContractDefinitionBody,
  type DerivativeKind,
  type DerivativeSide,
  type FlowKind,
  type PositionStatus,
  type TradeMode,
} from "../../lib/derivatives.ts";
import { api } from "../client.ts";
import { ContractViolation, cursorPageSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, economicIntentSpec, instrumentTermsSpec, retryTransient, type CursorPage } from "./core.ts";

export type DerivativeAsset = Schemas["DerivativeAsset"];
export type DerivativeMark = Schemas["DerivativeMark"];
export type DerivativeContract = Schemas["DerivativeContract"];
export type DerivativePolicy = Schemas["DerivativePolicy"];
export type DerivativePosition = Schemas["DerivativePosition"];
export type DerivativePositionPage = Schemas["DerivativePositionPage"];
export type DerivativeBook = Schemas["DerivativeBook"];
export type DerivativeFlow = Schemas["DerivativeFlow"];
export type DerivativeFundingAccrual = Schemas["DerivativeFundingAccrual"];
export type DerivativeOptionAnalytics = Schemas["DerivativeOptionAnalytics"];
export type DerivativeTradeResult = Schemas["DerivativeTradeResult"];
export type DerivativeRunResult = Schemas["DerivativeRunResult"];
export type DerivativeSettlementResult = Schemas["DerivativeSettlementResult"];
export type DerivativeClearingFundingResult = Schemas["DerivativeClearingFundingResult"];
export type DerivativeClearingReport = Schemas["DerivativeClearingReport"];
export type DerivativeSettlementCommandResult = Schemas["DerivativeSettlementCommandResult"];
export type DerivativeSettlementOverride = Schemas["DerivativeSettlementOverride"];
export type DerivativeSettlementPreview = Schemas["DerivativeSettlementPreview"];

/** Rows per page of the derivatives lists. */
export const DERIVATIVES_PAGE_SIZE = 25;
/** The contract list has no cursor, so the contract's maximum is asked for. */
export const CONTRACT_LIST_LIMIT = 200;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const derivativesKeys = {
  /** Prefix of every derivatives read that is not scoped to an account. */
  all: [...advancedKeys.all, "derivatives"] as const,
  policy: [...advancedKeys.all, "derivatives", "policy"] as const,
  contracts: (kind: string) => [...advancedKeys.all, "derivatives", "contracts", kind] as const,
  /** Prefix of every read about one contract. */
  contract: (instrumentId: string) => [...advancedKeys.all, "derivatives", "contract", instrumentId] as const,
  marks: (instrumentId: string, cursor: string) =>
    [...advancedKeys.all, "derivatives", "contract", instrumentId, "marks", cursor] as const,
  analytics: (instrumentId: string, inputs: string) =>
    [...advancedKeys.all, "derivatives", "contract", instrumentId, "analytics", inputs] as const,
  positions: (accountId: string, status: string, cursor: string) =>
    [...advancedKeys.account(accountId), "derivatives", "positions", status, cursor] as const,
  position: (accountId: string, positionId: string) =>
    [...advancedKeys.account(accountId), "derivatives", "position", positionId] as const,
  flows: (accountId: string, positionId: string, kind: string, cursor: string) =>
    [...advancedKeys.account(accountId), "derivatives", "flows", positionId, kind, cursor] as const,
  accruals: (accountId: string, positionId: string, cursor: string) =>
    [...advancedKeys.account(accountId), "derivatives", "accruals", positionId, cursor] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);

export const derivativeAssetSpec: Spec = { required: { id: "uuid", symbol: "string", decimals: "integer" } };

export const derivativeMarkSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    instrument_id: "uuid",
    mark_mantissa: "quantity",
    price_scale: "integer",
    has_index: "boolean",
    source: "string",
    observed_at: "timestamp",
    recorded_at: "timestamp",
  },
  optional: { index_mantissa: "quantity", index_scale: "integer", index_source: "string" },
};

export const derivativeSettlementSpec: Spec = {
  required: {
    id: "uuid",
    kind: oneOf(SETTLEMENT_KINDS),
    price_mantissa: "quantity",
    price_scale: "integer",
    price_source: "string",
    positions: "integer",
    settled_at: "timestamp",
  },
  optional: { price_rule: oneOf(PRICE_RULES), mark_id: "uuid", override_id: "uuid" },
};

const derivativeActorSpec: Spec = { required: { actor_type: "string", actor_id: "string" } };

export const derivativeOverrideSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    instrument_id: "uuid",
    status: oneOf(OVERRIDE_STATUSES),
    price_mantissa: "quantity",
    price_scale: "integer",
    reference_mark_id: "uuid",
    reference_mantissa: "quantity",
    max_deviation_bps: "integer",
    deviation_bps: "integer",
    reason: "string",
    evidence_ref: "string",
    evidence_sha256: "string",
    proposed_at: "timestamp",
  },
  optional: { settlement_id: "uuid" },
  objects: {
    proposed_by: { required: true, spec: derivativeActorSpec },
    decision: {
      spec: {
        required: { decision: oneOf(OVERRIDE_DECISIONS), reason: "string", decided_at: "timestamp" },
        objects: { decided_by: { required: true, spec: derivativeActorSpec } },
      },
    },
  },
};

export const derivativePreviewSpec: Spec = {
  required: {
    expiry: "timestamp",
    expired: "boolean",
    settled: "boolean",
    price_scale: "integer",
    has_reference: "boolean",
    settleable: "boolean",
    max_deviation_bps: "integer",
    rule_statement: "string",
  },
  optional: {
    reference_price_mantissa: "quantity",
    price_source: "string",
    override_min_price_mantissa: "quantity",
    override_max_price_mantissa: "quantity",
  },
  objects: { reference_mark: { spec: derivativeMarkSpec } },
};

export const derivativeContractSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    kind: oneOf(DERIVATIVE_KINDS),
    canonical_name: "string",
    status: "string",
    risk_class: "string",
    terms_version: "integer",
    terms_hash: "string",
    marked: "boolean",
    settled: "boolean",
  },
  objects: {
    terms: { required: true, spec: instrumentTermsSpec },
    settlement_asset: { required: true, spec: derivativeAssetSpec },
    latest_mark: { spec: derivativeMarkSpec },
    settlement: { spec: derivativeSettlementSpec },
    settlement_preview: { spec: derivativePreviewSpec },
  },
  arrays: { settlement_overrides: { spec: derivativeOverrideSpec } },
};

export const derivativeContractListSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: { items: { required: true, spec: derivativeContractSpec } },
};

export const derivativeMarkPageSpec: Spec = { ...cursorPageSpec(derivativeMarkSpec), required: { value_domain: SIMULATED } };

export const derivativePolicySpec: Spec = {
  required: {
    version: "string",
    hash: "string",
    funding_buffer_intervals: "integer",
    max_funding_rate_e8: "integer",
    funding_rate_scale: "integer",
    option_min_margin_bps: "integer",
    option_maintenance_ratio_bps: "integer",
    expiry_gap_window_ms: "integer",
    expiry_gap_shock_bps: "integer",
    paper_slippage_bps: "integer",
    variation_margin_interval_ms: "integer",
    max_mark_lag_ms: "integer",
    settlement_override_max_bps: "integer",
  },
  arrays: {
    linear: {
      required: true,
      spec: { required: { risk_class: "string", initial_bps: "integer", maintenance_bps: "integer" } },
    },
  },
  lists: { option_shocks_bps: { required: true, kind: "integer" } },
};

const refusalSpec: Spec = { required: { code: "string", detail: "string" } };

export const derivativeValuationSpec: Spec = {
  required: {
    mark_id: "uuid",
    mark_mantissa: "quantity",
    mark_observed_at: "timestamp",
    average_entry_price_mantissa: "quantity",
    unrealized_pnl: "quantity",
    equity_adjustment: "quantity",
  },
  objects: {
    requirement: {
      required: true,
      spec: {
        required: {
          notional: "quantity",
          initial: "quantity",
          maintenance: "quantity",
          scenario_loss: "quantity",
          funding_buffer: "quantity",
        },
      },
    },
  },
};

export const derivativePositionSpec: Spec = {
  required: {
    id: "uuid",
    account_id: "uuid",
    instrument_id: "uuid",
    symbol: "string",
    kind: oneOf(DERIVATIVE_KINDS),
    status: oneOf(POSITION_STATUSES),
    quantity: "quantity",
    price_scale: "integer",
    cost_value: "quantity",
    settled_value: "quantity",
    realized_pnl: "quantity",
    funding_pnl: "quantity",
    margin_posted: "quantity",
    shortfall: "quantity",
    version: "integer",
    opened_at: "timestamp",
    valuation_status: oneOf(VALUATION_STATUSES),
  },
  optional: { side: oneOf(DERIVATIVE_SIDES), closed_at: "timestamp" },
  objects: {
    settlement_asset: { required: true, spec: derivativeAssetSpec },
    valuation: { spec: derivativeValuationSpec },
    valuation_refusal: { spec: refusalSpec },
  },
};

export const derivativeBookSpec: Spec = {
  required: {
    as_of: "timestamp",
    policy_version: "string",
    policy_hash: "string",
    gross_notional: "usd",
    initial_margin: "usd",
    maintenance_margin: "usd",
    unrealized_pnl: "usd",
    equity_adjustment: "usd",
    max_initial_bps: "integer",
    positions: "integer",
  },
  objects: { quote_asset: { required: true, spec: derivativeAssetSpec } },
};

export const derivativePositionPageSpec: Spec = {
  ...cursorPageSpec(derivativePositionSpec),
  required: { value_domain: SIMULATED, book_status: oneOf(BOOK_STATUSES) },
  objects: { book: { spec: derivativeBookSpec }, book_refusal: { spec: refusalSpec } },
};

export const derivativeFlowSpec: Spec = {
  required: {
    id: "uuid",
    position_id: "uuid",
    account_id: "uuid",
    instrument_id: "uuid",
    symbol: "string",
    kind: oneOf(FLOW_KINDS),
    amount: "quantity",
    shortfall: "quantity",
    reference_type: "string",
    reference_id: "string",
    occurred_at: "timestamp",
  },
  optional: { journal_transaction_id: "uuid" },
  objects: { settlement_asset: { required: true, spec: derivativeAssetSpec } },
};

export const derivativeFlowPageSpec: Spec = { ...cursorPageSpec(derivativeFlowSpec), required: { value_domain: SIMULATED } };

export const derivativeAccrualSpec: Spec = {
  required: {
    id: "uuid",
    funding_rate_id: "uuid",
    position_id: "uuid",
    instrument_id: "uuid",
    symbol: "string",
    interval_start: "timestamp",
    interval_end: "timestamp",
    quantity: "quantity",
    direction: oneOf(FUNDING_DIRECTIONS),
    amount: "quantity",
    rate_e8: "integer",
    premium_index_e8: "integer",
    clamped: "boolean",
    per_contract_amount: "quantity",
    mark_mantissa: "quantity",
    price_scale: "integer",
  },
  optional: { flow_id: "uuid" },
  objects: { settlement_asset: { required: true, spec: derivativeAssetSpec } },
};

export const derivativeAccrualPageSpec: Spec = {
  ...cursorPageSpec(derivativeAccrualSpec),
  required: { value_domain: SIMULATED },
};

export const derivativeAnalyticsSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    label: "string",
    model: "string",
    instrument_id: "uuid",
    right: "string",
    style: "string",
    regime: "string",
    spot: "decimal",
    strike: "decimal",
    time_years: "decimal",
    vol: "decimal",
    price: "decimal",
    delta: "decimal",
    gamma: "decimal",
    theta_per_year: "decimal",
    theta_per_day: "decimal",
    vega: "decimal",
    vega_per_vol_point: "decimal",
    intrinsic: "decimal",
    as_of: "timestamp",
  },
  optional: { time_value: "decimal", implied_vol: "decimal", implied_vol_reason: "string" },
  objects: {
    inputs: {
      required: true,
      spec: { required: { vol_bps: "integer", rate_bps: "integer", dividend_yield_bps: "integer" } },
    },
    mark: { required: true, spec: derivativeMarkSpec },
  },
};

export const derivativeFillSpec: Spec = {
  required: {
    id: "uuid",
    position_id: "uuid",
    account_id: "uuid",
    instrument_id: "uuid",
    source: oneOf(FILL_SOURCES),
    side: oneOf(FILL_SIDES),
    quantity: "quantity",
    price_mantissa: "quantity",
    price_scale: "integer",
    value: "quantity",
    realized_pnl: "quantity",
    amount_settled: "quantity",
    slippage_bps: "integer",
    occurred_at: "timestamp",
  },
  optional: { mark_id: "uuid", intent_id: "uuid", risk_decision_id: "uuid" },
};

export const derivativeTradeResultSpec: Spec = {
  required: { value_domain: SIMULATED, replay: "boolean" },
  optional: { risk_decision_id: "uuid", exercise_id: "uuid" },
  objects: {
    intent: { required: true, spec: economicIntentSpec },
    fill: { spec: derivativeFillSpec },
    position: { spec: derivativePositionSpec },
  },
  lists: { journal_transaction_ids: { required: true, kind: "uuid" } },
};

export const derivativeRunResultSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    kind: oneOf(RUN_KINDS),
    run_id: "uuid",
    instrument_id: "uuid",
    replay: "boolean",
    interval_start: "timestamp",
    interval_end: "timestamp",
  },
  optional: {
    positions: "integer",
    mark_id: "uuid",
    paid: "quantity",
    received: "quantity",
    shortfall: "quantity",
    premium_index_e8: "integer",
    rate_e8: "integer",
    clamped: "boolean",
    per_contract_amount: "quantity",
  },
  objects: { settlement_asset: { required: true, spec: derivativeAssetSpec } },
  lists: { journal_transaction_ids: { required: true, kind: "uuid" } },
};

export const derivativeSettlementResultSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    settlement_id: "uuid",
    instrument_id: "uuid",
    kind: oneOf(SETTLEMENT_KINDS),
    replay: "boolean",
    price_mantissa: "quantity",
    price_scale: "integer",
    price_source: "string",
    positions: "integer",
  },
  optional: {
    paid: "quantity",
    received: "quantity",
    shortfall: "quantity",
    intrinsic_per_contract: "quantity",
    price_rule: oneOf(PRICE_RULES),
    mark_id: "uuid",
    override_id: "uuid",
  },
  objects: { settlement_asset: { required: true, spec: derivativeAssetSpec } },
  lists: { exercise_ids: { required: true, kind: "uuid" }, journal_transaction_ids: { required: true, kind: "uuid" } },
};

export const derivativeSettlementCommandSpec: Spec = {
  required: { value_domain: SIMULATED, method: oneOf(SETTLEMENT_METHODS) },
  objects: {
    settlement: { spec: derivativeSettlementResultSpec },
    override: { spec: derivativeOverrideSpec },
  },
};

export const derivativeClearingFundingSpec: Spec = {
  required: { value_domain: SIMULATED, quantity: "quantity" },
  objects: { asset: { required: true, spec: derivativeAssetSpec } },
  lists: { journal_transaction_ids: { required: true, kind: "uuid" } },
};

export const derivativeClearingReportSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    clearing_house_id: "uuid",
    holdings: "quantity",
    capital: "quantity",
    result: "quantity",
    register_net: "quantity",
    register_shortfall: "quantity",
    flows: "integer",
    expected_result: "quantity",
    difference: "quantity",
    internally_matched: "boolean",
    matched: "boolean",
  },
  optional: { record_id: "uuid" },
  objects: { asset: { required: true, spec: derivativeAssetSpec } },
  arrays: { net_open_interest: { required: true, spec: { required: { instrument_id: "uuid", quantity: "quantity" } } } },
};

/* --------------------------------------------------------------------------
 * The rules a per-field spec cannot state
 * ------------------------------------------------------------------------ */

/** A contract says whether it is marked and settled, and carries what it says it has. */
export function checkContract(value: unknown, path: string): DerivativeContract {
  const c = validated<DerivativeContract>(value, derivativeContractSpec, path);
  if (c.marked !== (c.latest_mark !== undefined)) {
    throw new ContractViolation(`${path}.latest_mark`, "marked and latest_mark disagree");
  }
  if (c.settled !== (c.settlement !== undefined)) {
    throw new ContractViolation(`${path}.settlement`, "settled and settlement disagree");
  }
  const preview = c.settlement_preview;
  if (preview !== undefined && preview.has_reference !== (preview.reference_price_mantissa !== undefined)) {
    throw new ContractViolation(`${path}.settlement_preview.reference_price_mantissa`, "has_reference and the reference disagree");
  }
  return c;
}

/**
 * A settlement command answers with what its method produced: the settlement
 * for RULE, the proposal for PROPOSE_OVERRIDE and REJECT_OVERRIDE, both for
 * APPROVE_OVERRIDE — never anything else.
 */
export function checkSettlementCommand(value: unknown, path: string): DerivativeSettlementCommandResult {
  const r = validated<DerivativeSettlementCommandResult>(value, derivativeSettlementCommandSpec, path);
  const settles = r.method === "RULE" || r.method === "APPROVE_OVERRIDE";
  const proposes = r.method !== "RULE";
  if (settles !== (r.settlement !== undefined)) {
    throw new ContractViolation(`${path}.settlement`, `a ${r.method} answer ${settles ? "carries" : "carries no"} settlement`);
  }
  if (proposes !== (r.override !== undefined)) {
    throw new ContractViolation(`${path}.override`, `a ${r.method} answer ${proposes ? "carries" : "carries no"} override`);
  }
  return r;
}

/**
 * A position carries exactly what its valuation status says: the valuation
 * when VALUED, the refusal when REFUSED, and neither otherwise.
 */
export function checkPosition(value: unknown, path: string): DerivativePosition {
  const p = validated<DerivativePosition>(value, derivativePositionSpec, path);
  const valued = p.valuation !== undefined;
  const refused = p.valuation_refusal !== undefined;
  const consistent =
    (p.valuation_status === "VALUED" && valued && !refused) ||
    (p.valuation_status === "REFUSED" && refused && !valued) ||
    ((p.valuation_status === "NOT_APPLICABLE" || p.valuation_status === "NOT_COMPUTED") && !valued && !refused);
  if (!consistent) {
    throw new ContractViolation(`${path}.valuation_status`, `${p.valuation_status} does not match what the position carries`);
  }
  return p;
}

/** A page of positions, each consistent, and a book that is valued or refused — not both, not neither. */
export function checkPositionPage(value: unknown, path: string): DerivativePositionPage {
  const page = validated<DerivativePositionPage>(value, derivativePositionPageSpec, path);
  page.items.forEach((item, index) => checkPosition(item, `${path}.items[${String(index)}]`));
  const valued = page.book !== undefined;
  const refused = page.book_refusal !== undefined;
  if ((page.book_status === "VALUED") !== valued || (page.book_status === "REFUSED") !== refused) {
    throw new ContractViolation(`${path}.book_status`, `${page.book_status} does not match what the page carries`);
  }
  return page;
}

/** A trade result, with its position held to the same rule. */
export function checkTradeResult(value: unknown, path: string): DerivativeTradeResult {
  const r = validated<DerivativeTradeResult>(value, derivativeTradeResultSpec, path);
  if (r.position !== undefined) checkPosition(r.position, `${path}.position`);
  return r;
}

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

export function useDerivativePolicy(): UseQueryResult<DerivativePolicy> {
  return useQuery({
    queryKey: derivativesKeys.policy,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/derivatives/policy", {});
      return validated<DerivativePolicy>(data, derivativePolicySpec, "/advanced/derivatives/policy");
    },
  });
}

export function useDerivativeContracts(kind: DerivativeKind | undefined): UseQueryResult<DerivativeContract[]> {
  return useQuery({
    queryKey: derivativesKeys.contracts(kind ?? ""),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/derivatives/contracts", {
        params: { query: kind === undefined ? { limit: CONTRACT_LIST_LIMIT } : { kind, limit: CONTRACT_LIST_LIMIT } },
      });
      const list = validated<Schemas["DerivativeContractList"]>(data, derivativeContractListSpec, "/advanced/derivatives/contracts");
      return list.items.map((item, index) => checkContract(item, `/advanced/derivatives/contracts.items[${String(index)}]`));
    },
  });
}

export function useDerivativeContract(instrumentId: string | undefined): UseQueryResult<DerivativeContract> {
  return useQuery({
    queryKey: derivativesKeys.contract(instrumentId ?? ""),
    enabled: instrumentId !== undefined && instrumentId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/derivatives/contracts/{instrumentId}", {
        params: { path: { instrumentId: instrumentId ?? "" } },
      });
      return checkContract(data, "/advanced/derivatives/contracts/{id}");
    },
  });
}

export function useDerivativeMarks(
  instrumentId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<DerivativeMark>> {
  return useQuery({
    queryKey: derivativesKeys.marks(instrumentId ?? "", cursor ?? ""),
    enabled: instrumentId !== undefined && instrumentId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/derivatives/contracts/{instrumentId}/marks", {
        params: {
          path: { instrumentId: instrumentId ?? "" },
          query: { limit: DERIVATIVES_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) },
        },
      });
      const page = validated<Schemas["DerivativeMarkPage"]>(data, derivativeMarkPageSpec, "/advanced/derivatives/contracts/{id}/marks");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/** The model inputs a person submitted, in basis points. */
export interface AnalyticsInputs {
  readonly volBps: number;
  readonly rateBps: number;
  readonly dividendYieldBps: number;
}

/** Asked only once inputs have been submitted: the model needs a volatility from the person. */
export function useDerivativeAnalytics(
  instrumentId: string | undefined,
  inputs: AnalyticsInputs | undefined,
): UseQueryResult<DerivativeOptionAnalytics> {
  const key = inputs === undefined ? "" : `${String(inputs.volBps)}/${String(inputs.rateBps)}/${String(inputs.dividendYieldBps)}`;
  return useQuery({
    queryKey: derivativesKeys.analytics(instrumentId ?? "", key),
    enabled: instrumentId !== undefined && instrumentId !== "" && inputs !== undefined,
    // Reference values of the latest mark at this moment; asked again, not reused.
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/derivatives/contracts/{instrumentId}/analytics", {
        params: {
          path: { instrumentId: instrumentId ?? "" },
          query: {
            vol_bps: inputs?.volBps ?? 0,
            rate_bps: inputs?.rateBps ?? 0,
            dividend_yield_bps: inputs?.dividendYieldBps ?? 0,
          },
        },
      });
      return validated<DerivativeOptionAnalytics>(data, derivativeAnalyticsSpec, "/advanced/derivatives/contracts/{id}/analytics");
    },
  });
}

export function useDerivativePositions(
  accountId: string | undefined,
  status: PositionStatus | undefined,
  cursor: string | undefined,
): UseQueryResult<DerivativePositionPage> {
  return useQuery({
    queryKey: derivativesKeys.positions(accountId ?? "", status ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    // Valued by the backend on every call at the latest marks; never reused as truth.
    staleTime: 0,
    refetchOnWindowFocus: true,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/derivatives/positions", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit: DERIVATIVES_PAGE_SIZE,
            ...(status === undefined ? {} : { status }),
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      return checkPositionPage(data, "/advanced/derivatives/positions");
    },
  });
}

/**
 * One position. The key carries the account the area reads; the backend
 * decides from the session whether the position may be read and answers
 * NOT_FOUND when it may not.
 */
export function useDerivativePosition(
  accountId: string | undefined,
  positionId: string | undefined,
): UseQueryResult<DerivativePosition> {
  return useQuery({
    queryKey: derivativesKeys.position(accountId ?? "", positionId ?? ""),
    enabled: positionId !== undefined && positionId !== "",
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/derivatives/positions/{positionId}", {
        params: { path: { positionId: positionId ?? "" } },
      });
      return checkPosition(data, "/advanced/derivatives/positions/{id}");
    },
  });
}

export function useDerivativeFlows(
  accountId: string | undefined,
  filter: { readonly positionId?: string; readonly kind?: FlowKind },
  cursor: string | undefined,
): UseQueryResult<CursorPage<DerivativeFlow>> {
  return useQuery({
    queryKey: derivativesKeys.flows(accountId ?? "", filter.positionId ?? "", filter.kind ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/derivatives/flows", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit: DERIVATIVES_PAGE_SIZE,
            ...(filter.positionId === undefined ? {} : { position_id: filter.positionId }),
            ...(filter.kind === undefined ? {} : { kind: filter.kind }),
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["DerivativeFlowPage"]>(data, derivativeFlowPageSpec, "/advanced/derivatives/flows");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useDerivativeFundingAccruals(
  accountId: string | undefined,
  positionId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<DerivativeFundingAccrual>> {
  return useQuery({
    queryKey: derivativesKeys.accruals(accountId ?? "", positionId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/derivatives/funding-accruals", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit: DERIVATIVES_PAGE_SIZE,
            ...(positionId === undefined ? {} : { position_id: positionId }),
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["DerivativeFundingAccrualPage"]>(
        data,
        derivativeAccrualPageSpec,
        "/advanced/derivatives/funding-accruals",
      );
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/* --------------------------------------------------------------------------
 * The customer's commands
 * ------------------------------------------------------------------------ */

export interface OpenPositionInput {
  readonly accountId: string;
  readonly instrumentId: string;
  readonly side: DerivativeSide;
  /** Whole contracts, as an integer string. */
  readonly quantity: string;
  readonly mode: TradeMode;
  /** Created once when the person confirmed, reused for every retry of this action. */
  readonly idempotencyKey: string;
}

export function useOpenDerivativePosition(): UseMutationResult<DerivativeTradeResult, unknown, OpenPositionInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: OpenPositionInput) => {
      const { data } = await api.POST("/advanced/derivatives/positions", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          instrument_id: input.instrumentId,
          side: input.side,
          quantity: input.quantity,
          mode: input.mode,
        },
      });
      return checkTradeResult(data, "/advanced/derivatives/positions");
    },
    // A refusal is recorded too: the account's intents and decisions changed either way.
    onSettled: (_result, _error, input) => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(input.accountId) });
    },
  });
}

export interface ReducePositionInput {
  /** The account the area reads, for the reads to refresh. */
  readonly accountId: string;
  readonly positionId: string;
  /** Contracts; undefined closes (or exercises) every one. */
  readonly quantity: string | undefined;
  readonly mode: TradeMode;
  readonly idempotencyKey: string;
}

function reduceBody(input: ReducePositionInput): { quantity?: string; mode: TradeMode } {
  return input.quantity === undefined ? { mode: input.mode } : { quantity: input.quantity, mode: input.mode };
}

export function useCloseDerivativePosition(): UseMutationResult<DerivativeTradeResult, unknown, ReducePositionInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: ReducePositionInput) => {
      const { data } = await api.POST("/advanced/derivatives/positions/{positionId}/close", {
        ...idempotent(input.idempotencyKey, { path: { positionId: input.positionId } }),
        body: reduceBody(input),
      });
      return checkTradeResult(data, "/advanced/derivatives/positions/{id}/close");
    },
    onSettled: (_result, _error, input) => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(input.accountId) });
    },
  });
}

export function useExerciseDerivativePosition(): UseMutationResult<DerivativeTradeResult, unknown, ReducePositionInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: ReducePositionInput) => {
      const { data } = await api.POST("/advanced/derivatives/positions/{positionId}/exercise", {
        ...idempotent(input.idempotencyKey, { path: { positionId: input.positionId } }),
        body: reduceBody(input),
      });
      return checkTradeResult(data, "/advanced/derivatives/positions/{id}/exercise");
    },
    onSettled: (_result, _error, input) => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(input.accountId) });
    },
  });
}

/* --------------------------------------------------------------------------
 * The operator's commands
 * ------------------------------------------------------------------------ */

export interface DefineContractInput {
  /** Exactly what the form built (src/lib/derivatives.ts buildContractDefinition). */
  readonly definition: ContractDefinitionBody;
  readonly idempotencyKey: string;
}

/** Defines a contract through the derivatives service. The answer is the contract as the backend reads it back. */
export function useDefineDerivativeContract(): UseMutationResult<DerivativeContract, unknown, DefineContractInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: DefineContractInput) => {
      const { data } = await api.POST("/advanced/derivatives/contracts", {
        ...idempotent(input.idempotencyKey),
        body: input.definition,
      });
      return checkContract(data, "/advanced/derivatives/contracts");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: derivativesKeys.all });
    },
  });
}

export interface RecordMarkInput {
  readonly instrumentId: string;
  /** Exact decimals as typed; the backend rescales them exactly or refuses. */
  readonly mark: string;
  readonly index: string | undefined;
  readonly source: string;
  readonly idempotencyKey: string;
}

export function useRecordDerivativeMark(): UseMutationResult<DerivativeMark, unknown, RecordMarkInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: RecordMarkInput) => {
      const { data } = await api.POST("/advanced/derivatives/contracts/{instrumentId}/marks", {
        ...idempotent(input.idempotencyKey, { path: { instrumentId: input.instrumentId } }),
        body: {
          mark: input.mark,
          source: input.source,
          ...(input.index === undefined ? {} : { index: input.index }),
        },
      });
      return validated<DerivativeMark>(data, derivativeMarkSpec, "/advanced/derivatives/contracts/{id}/marks");
    },
    onSuccess: () => {
      // A new mark moves every valuation of every account that holds the contract.
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export interface RunInput {
  readonly instrumentId: string;
  /** RFC 3339 instant in UTC, on the interval grid. */
  readonly intervalStart: string;
  readonly idempotencyKey: string;
}

export function useRunVariationMargin(): UseMutationResult<DerivativeRunResult, unknown, RunInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: RunInput) => {
      const { data } = await api.POST("/advanced/derivatives/contracts/{instrumentId}/variation-margin-runs", {
        ...idempotent(input.idempotencyKey, { path: { instrumentId: input.instrumentId } }),
        body: { interval_start: input.intervalStart },
      });
      return validated<DerivativeRunResult>(data, derivativeRunResultSpec, "/advanced/derivatives/contracts/{id}/variation-margin-runs");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export function useRunFunding(): UseMutationResult<DerivativeRunResult, unknown, RunInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: RunInput) => {
      const { data } = await api.POST("/advanced/derivatives/contracts/{instrumentId}/funding-runs", {
        ...idempotent(input.idempotencyKey, { path: { instrumentId: input.instrumentId } }),
        body: { interval_start: input.intervalStart },
      });
      return validated<DerivativeRunResult>(data, derivativeRunResultSpec, "/advanced/derivatives/contracts/{id}/funding-runs");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export interface SettleInput {
  readonly instrumentId: string;
  /**
   * The price the operator confirms, as the exact decimal the contract's
   * settlement preview states, and its source. They never choose the price:
   * the backend settles by its rule and refuses anything else.
   */
  readonly price: string | undefined;
  readonly priceSource: string | undefined;
  readonly idempotencyKey: string;
}

const SETTLEMENT_PATH = "/advanced/derivatives/contracts/{id}/settlement";

/** Settles an expired contract by the settlement rule. */
export function useSettleDerivative(): UseMutationResult<DerivativeSettlementCommandResult, unknown, SettleInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: SettleInput) => {
      const { data } = await api.POST("/advanced/derivatives/contracts/{instrumentId}/settlement", {
        ...idempotent(input.idempotencyKey, { path: { instrumentId: input.instrumentId } }),
        body: {
          method: "RULE",
          ...(input.price === undefined ? {} : { price: input.price }),
          ...(input.priceSource === undefined ? {} : { price_source: input.priceSource }),
        },
      });
      return checkSettlementCommand(data, SETTLEMENT_PATH);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export interface ProposeOverrideInput {
  readonly instrumentId: string;
  /** The proposed settlement price, an exact decimal. */
  readonly price: string;
  readonly reason: string;
  readonly evidenceRef: string;
  /** 64 lowercase hex digits. */
  readonly evidenceSha256: string;
  readonly idempotencyKey: string;
}

/** Proposes, as this operator, an override of an expired contract's settlement price. */
export function useProposeSettlementOverride(): UseMutationResult<DerivativeSettlementCommandResult, unknown, ProposeOverrideInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: ProposeOverrideInput) => {
      const { data } = await api.POST("/advanced/derivatives/contracts/{instrumentId}/settlement", {
        ...idempotent(input.idempotencyKey, { path: { instrumentId: input.instrumentId } }),
        body: {
          method: "PROPOSE_OVERRIDE",
          price: input.price,
          reason: input.reason,
          evidence_ref: input.evidenceRef,
          evidence_sha256: input.evidenceSha256,
        },
      });
      return checkSettlementCommand(data, SETTLEMENT_PATH);
    },
    onSuccess: (_result, input) => {
      void queryClient.invalidateQueries({ queryKey: derivativesKeys.contract(input.instrumentId) });
    },
  });
}

export interface DecideOverrideInput {
  readonly instrumentId: string;
  readonly overrideId: string;
  readonly decision: "APPROVE_OVERRIDE" | "REJECT_OVERRIDE";
  readonly reason: string;
  /** On an approval, the proposal's price the operator confirms. */
  readonly price: string | undefined;
  readonly idempotencyKey: string;
}

/**
 * Approves (and so settles at) or rejects a proposal, as this operator. The
 * backend refuses an approval by the operator who proposed it.
 */
export function useDecideSettlementOverride(): UseMutationResult<DerivativeSettlementCommandResult, unknown, DecideOverrideInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: DecideOverrideInput) => {
      const { data } = await api.POST("/advanced/derivatives/contracts/{instrumentId}/settlement", {
        ...idempotent(input.idempotencyKey, { path: { instrumentId: input.instrumentId } }),
        body: {
          method: input.decision,
          override_id: input.overrideId,
          reason: input.reason,
          ...(input.price === undefined ? {} : { price: input.price }),
        },
      });
      return checkSettlementCommand(data, SETTLEMENT_PATH);
    },
    onSuccess: () => {
      // An approval settles the contract: every account that held it moved.
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export interface FundClearingHouseInput {
  /** Undefined funds the sandbox dollar. */
  readonly assetId: string | undefined;
  /** Exact base units. */
  readonly quantity: string;
  readonly idempotencyKey: string;
}

export function useFundClearingHouse(): UseMutationResult<DerivativeClearingFundingResult, unknown, FundClearingHouseInput> {
  return useMutation({
    mutationFn: async (input: FundClearingHouseInput) => {
      const { data } = await api.POST("/advanced/derivatives/clearing-house/funding", {
        ...idempotent(input.idempotencyKey),
        body: { quantity: input.quantity, ...(input.assetId === undefined ? {} : { asset_id: input.assetId }) },
      });
      return validated<DerivativeClearingFundingResult>(data, derivativeClearingFundingSpec, "/advanced/derivatives/clearing-house/funding");
    },
  });
}

export function useReconcileClearingHouse(): UseMutationResult<
  DerivativeClearingReport,
  unknown,
  { readonly assetId: string | undefined; readonly idempotencyKey: string }
> {
  return useMutation({
    mutationFn: async (input: { readonly assetId: string | undefined; readonly idempotencyKey: string }) => {
      const { data } = await api.POST("/advanced/derivatives/clearing-house/reconciliations", {
        ...idempotent(input.idempotencyKey),
        body: input.assetId === undefined ? {} : { asset_id: input.assetId },
      });
      return validated<DerivativeClearingReport>(data, derivativeClearingReportSpec, "/advanced/derivatives/clearing-house/reconciliations");
    },
  });
}
