/**
 * The margin section's reads and commands (internal/margin, ADR-0020).
 *
 * Modelled on `core.ts`: every response is checked against a spec before a
 * component sees it — exact base units (`quantity`) and two-decimal cent
 * strings (`usd`), closed enums, required fields present, and
 * `value_domain: SIMULATED` on everything that reports value — and refused as
 * a ContractViolation otherwise, never repaired. A 4xx is the backend's
 * decision and is not retried.
 *
 * Every command carries an Idempotency-Key that the page created once, when
 * the user confirmed, and reuses for every retry of that same request
 * (`confirmAttempt` in lib/margin.ts). Commands invalidate the whole account
 * prefix, because a margin operation moves buying power, intents and effects
 * as well as the margin account.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  MARGIN_ACCOUNT_STATUSES,
  MARGIN_CALL_STATUSES,
  MARGIN_CASE_STATUSES,
  MARGIN_LIQUIDATION_TARGETS,
  MARGIN_PLAN_DECISIONS,
  MARGIN_RESOLUTION_DECISIONS,
  MARGIN_RESOLUTION_OUTCOMES,
  MARGIN_RUN_MODES,
  MARGIN_STEP_ACTIONS,
  MARGIN_STRESS_SCENARIOS,
  type ConfirmedAttempt,
  type LeveragedMarginMode,
  type MarginCallStatus,
  type MarginResolutionDecision,
  type MarginResolutionOutcome,
} from "../../lib/margin.ts";
import { api } from "../client.ts";
import { cursorPageSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient, type CursorPage } from "./core.ts";

export type MarginAccount = Schemas["MarginAccount"];
export type MarginAccountDetail = Schemas["MarginAccountDetail"];
export type MarginEvaluation = Schemas["MarginEvaluation"];
export type MarginHolding = Schemas["MarginHolding"];
export type MarginHaircut = Schemas["MarginHaircut"];
export type MarginPrice = Schemas["MarginPrice"];
export type MarginLoan = Schemas["MarginLoan"];
export type MarginCall = Schemas["MarginCall"];
export type MarginCase = Schemas["MarginCase"];
export type MarginCaseDetail = Schemas["MarginCaseDetail"];
export type MarginLiquidationStep = Schemas["MarginLiquidationStep"];
export type MarginStressRun = Schemas["MarginStressRun"];
export type MarginStressRunSummary = Schemas["MarginStressRunSummary"];
export type MarginStressResult = Schemas["MarginStressResult"];
export type MarginCollateralResult = Schemas["MarginCollateralResult"];
export type MarginLoanResult = Schemas["MarginLoanResult"];
export type MarginLeveragedBuyResult = Schemas["MarginLeveragedBuyResult"];
export type MarginFacilities = Schemas["MarginFacilities"];
export type MarginMarkToMarketResult = Schemas["MarginMarkToMarketResult"];
export type MarginQueue = Schemas["MarginQueue"];
export type MarginQueueEntry = Schemas["MarginQueueEntry"];
export type MarginQueueRunResult = Schemas["MarginQueueRunResult"];
export type MarginLiquidationResult = Schemas["MarginLiquidationResult"];
export type MarginDeficitResult = Schemas["MarginDeficitResult"];
export type MarginFailure = Schemas["MarginFailure"];
export type MarginCaseResolution = Schemas["MarginCaseResolution"];
export type MarginReviewItem = Schemas["MarginReviewItem"];
export type MarginReviewQueue = Schemas["MarginReviewQueue"];
export type MarginResolutionDecisionResult = Schemas["MarginResolutionDecisionResult"];
export type MarginDeficitStatus = Schemas["MarginDeficitStatus"];
export type MarginDeficitRepayment = Schemas["MarginDeficitRepayment"];
export type MarginDeficitRepaymentResult = Schemas["MarginDeficitRepaymentResult"];

/** Rows per page of the calls, cases and stress-run lists. */
export const MARGIN_PAGE_SIZE = 25;
/** The queue has no cursor: the contract's maximum is asked for. */
export const MARGIN_QUEUE_LIMIT = 200;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const marginKeys = {
  account: (accountId: string) => [...advancedKeys.account(accountId), "margin", "account"] as const,
  calls: (accountId: string, cursor: string) => [...advancedKeys.account(accountId), "margin", "calls", cursor] as const,
  cases: (accountId: string, cursor: string) => [...advancedKeys.account(accountId), "margin", "cases", cursor] as const,
  case: (accountId: string, caseId: string) => [...advancedKeys.account(accountId), "margin", "case", caseId] as const,
  stressRuns: (accountId: string, cursor: string) =>
    [...advancedKeys.account(accountId), "margin", "stress-runs", cursor] as const,
  stressRun: (accountId: string, runId: string) =>
    [...advancedKeys.account(accountId), "margin", "stress-run", runId] as const,
  /** Prefix of every operator read across accounts. */
  operator: ["advanced", "margin-operator"] as const,
  queue: ["advanced", "margin-operator", "queue"] as const,
  operatorCalls: (status: string, cursor: string) => ["advanced", "margin-operator", "calls", status, cursor] as const,
  /** Cases in MANUAL_REVIEW with their resolutions: an operator read across accounts. */
  reviews: ["advanced", "margin-operator", "reviews"] as const,
  /** The covered deficit of the account's latest bankrupt case and its repayments. */
  deficit: (accountId: string) => [...advancedKeys.account(accountId), "margin", "deficit"] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
const ACCOUNT_STATUS = oneOf(MARGIN_ACCOUNT_STATUSES);

export const marginPriceSpec: Spec = {
  required: { mantissa: "quantity", scale: "integer" },
  optional: { source: "string", observed_at: "timestamp" },
};

export const marginHaircutSpec: Spec = {
  required: {
    volatility_bps: "integer",
    liquidity_bps: "integer",
    oracle_bps: "integer",
    venue_bps: "integer",
    concentration_bps: "integer",
    settlement_bps: "integer",
    cross_chain_bps: "integer",
    zero_value: "boolean",
    total_bps: "integer",
  },
};

export const marginHoldingSpec: Spec = {
  required: {
    asset_id: "uuid",
    numeraire: "boolean",
    quantity: "quantity",
    market_value: "quantity",
    priced: "boolean",
    fresh: "boolean",
    concentration_share_bps: "integer",
    collateral_value: "quantity",
    exposure: "quantity",
    initial_margin: "quantity",
    maintenance_margin: "quantity",
  },
  optional: { mark_at: "timestamp" },
  objects: { haircut: { required: true, spec: marginHaircutSpec }, mark: { spec: marginPriceSpec } },
};

const EVALUATION_USD_FIELDS = [
  "eligible_collateral",
  "borrow_liabilities",
  "accrued_interest",
  "equity",
  "market_value",
  "gross_exposure",
  "initial_margin",
  "maintenance_margin",
  "warning_threshold",
  "margin_available",
  "liquidation_buffer",
] as const satisfies ReadonlyArray<keyof Schemas["MarginEvaluationUSD"]>;

export const marginEvaluationSpec: Spec = {
  required: {
    at: "timestamp",
    policy_version: "string",
    policy_hash: "string",
    hash: "string",
    numeraire_asset_id: "uuid",
    numeraire_decimals: "integer",
    // Health is NORMAL, WARNING or MARGIN_CALL; the contract types it as an
    // account status, so the account statuses bound it.
    health: ACCOUNT_STATUS,
    levered: "boolean",
    liquidation_eligible: "boolean",
    prices_fresh: "boolean",
    missing_marks: "boolean",
    leverage_cap_bps: "integer",
    effective_leverage_bps: "integer",
    leverage_unbounded: "boolean",
    eligible_collateral: "quantity",
    realized_pnl: "quantity",
    unrealized_pnl: "quantity",
    borrow_liabilities: "quantity",
    accrued_fees: "quantity",
    accrued_interest: "quantity",
    equity: "quantity",
    market_value: "quantity",
    net_asset_value: "quantity",
    gross_exposure: "quantity",
    net_exposure: "quantity",
    initial_margin: "quantity",
    maintenance_margin: "quantity",
    warning_threshold: "quantity",
    liquidation_buffer: "quantity",
    capacity: "quantity",
    margin_available: "quantity",
  },
  optional: { oldest_mark_at: "timestamp", newest_mark_at: "timestamp" },
  lists: { stale_assets: { required: true, kind: "uuid" } },
  arrays: { holdings: { required: true, spec: marginHoldingSpec } },
  objects: {
    usd: {
      required: true,
      spec: { required: Object.fromEntries(EVALUATION_USD_FIELDS.map((field) => [field, "usd" as const])) },
    },
  },
};

/** The account row. Its margin mode is read as a string: see LEVERAGED_MARGIN_MODES. */
const marginAccountFields = {
  value_domain: SIMULATED,
  id: "uuid",
  account_id: "uuid",
  margin_mode: "string",
  mode: oneOf(MARGIN_RUN_MODES),
  status: ACCOUNT_STATUS,
  policy_version: "string",
  numeraire_asset_id: "uuid",
  lending_pool_id: "uuid",
  insurance_fund_id: "uuid",
  opened_at: "timestamp",
} as const;

export const marginAccountSpec: Spec = {
  required: marginAccountFields,
  optional: { isolated_asset_id: "uuid" },
};

export const marginCallSpec: Spec = {
  required: {
    id: "uuid",
    margin_account_id: "uuid",
    account_id: "uuid",
    status: oneOf(MARGIN_CALL_STATUSES),
    issued_at: "timestamp",
    equity: "quantity",
    maintenance_margin: "quantity",
    deficiency: "quantity",
    evaluation_hash: "string",
    numeraire_asset_id: "uuid",
    numeraire_decimals: "integer",
  },
  optional: { resolved_at: "timestamp" },
};

export const marginCaseSpec: Spec = {
  required: {
    id: "uuid",
    margin_account_id: "uuid",
    account_id: "uuid",
    margin_call_id: "uuid",
    status: oneOf(MARGIN_CASE_STATUSES),
    policy_version: "string",
    target: oneOf(MARGIN_LIQUIDATION_TARGETS),
    max_steps: "integer",
    steps_executed: "integer",
    opened_at: "timestamp",
    last_reason: "string",
    numeraire_asset_id: "uuid",
    numeraire_decimals: "integer",
  },
  optional: {
    closed_at: "timestamp",
    deficit: "quantity",
    deficit_coverage_journal_transaction_id: "uuid",
    deficit_covered_at: "timestamp",
  },
};

export const marginStepSpec: Spec = {
  required: {
    id: "uuid",
    step_no: "integer",
    action: oneOf(MARGIN_STEP_ACTIONS),
    asset_id: "uuid",
    quantity: "quantity",
    slippage_bps: "integer",
    slippage_cap_bps: "integer",
    proceeds: "quantity",
    fee: "quantity",
    repaid: "quantity",
    excess: "quantity",
    equity_before: "quantity",
    equity_after: "quantity",
    plan_hash: "string",
    executed_at: "timestamp",
  },
  optional: {
    trade_journal_transaction_id: "uuid",
    release_journal_transaction_id: "uuid",
    repay_journal_transaction_id: "uuid",
    excess_journal_transaction_id: "uuid",
  },
  objects: { mark: { spec: marginPriceSpec }, execution_price: { spec: marginPriceSpec } },
};

export const marginAccountDetailSpec: Spec = {
  required: marginAccountFields,
  optional: { isolated_asset_id: "uuid" },
  objects: {
    evaluation: { required: true, spec: marginEvaluationSpec },
    open_call: { spec: marginCallSpec },
    latest_case: { spec: marginCaseSpec },
  },
  arrays: {
    loans: {
      required: true,
      spec: {
        required: {
          lot_id: "uuid",
          principal: "quantity",
          interest: "quantity",
          rate_bps: "integer",
          accrued_through: "timestamp",
        },
      },
    },
  },
};

export const marginCaseDetailSpec: Spec = {
  ...marginCaseSpec,
  required: { ...marginCaseSpec.required, value_domain: SIMULATED },
  arrays: { steps: { required: true, spec: marginStepSpec } },
};

const marginStressSummaryFields = {
  id: "uuid",
  margin_account_id: "uuid",
  account_id: "uuid",
  run_at: "timestamp",
  policy_version: "string",
  policy_hash: "string",
  input_hash: "string",
  report_hash: "string",
  numeraire_asset_id: "uuid",
  numeraire_decimals: "integer",
} as const;

export const marginStressRunSpec: Spec = {
  required: { ...marginStressSummaryFields, value_domain: SIMULATED },
  arrays: {
    results: {
      required: true,
      spec: {
        required: {
          scenario: oneOf(MARGIN_STRESS_SCENARIOS),
          shock_bps: "integer",
          equity_after: "quantity",
          maintenance_after: "quantity",
          initial_after: "quantity",
          equity_after_usd: "usd",
          maintenance_after_usd: "usd",
          would_liquidate: "boolean",
          executable: "boolean",
          outcome: oneOf(MARGIN_PLAN_DECISIONS),
          steps_simulated: "integer",
          deficit: "quantity",
          deficit_usd: "usd",
          insurance_fund_draw: "quantity",
          insurance_fund_draw_usd: "usd",
        },
      },
    },
  },
};

function simulatedPage(item: Spec): Spec {
  return { ...cursorPageSpec(item), required: { value_domain: SIMULATED } };
}

export const marginCallPageSpec = simulatedPage(marginCallSpec);
export const marginCasePageSpec = simulatedPage(marginCaseSpec);
export const marginStressRunPageSpec = simulatedPage({ required: marginStressSummaryFields });

export const marginCollateralResultSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    margin_account_id: "uuid",
    move_id: "uuid",
    journal_transaction_id: "uuid",
    effect_id: "uuid",
    replayed: "boolean",
    status: ACCOUNT_STATUS,
  },
  optional: { intent_id: "uuid", reservation_id: "uuid" },
  objects: { evaluation: { spec: marginEvaluationSpec } },
};

export const marginLoanResultSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    margin_account_id: "uuid",
    intent_id: "uuid",
    journal_transaction_id: "uuid",
    effect_id: "uuid",
    replayed: "boolean",
    status: ACCOUNT_STATUS,
  },
  optional: { lot_id: "uuid" },
  arrays: {
    allocations: { required: true, spec: { required: { lot_id: "uuid", interest: "quantity", principal: "quantity" } } },
  },
  objects: { evaluation: { spec: marginEvaluationSpec } },
};

export const marginLeveragedBuyResultSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    margin_account_id: "uuid",
    intent_id: "uuid",
    cost: "quantity",
    borrow_journal_transaction_id: "uuid",
    trade_journal_transaction_id: "uuid",
    collateral_journal_transaction_id: "uuid",
    replayed: "boolean",
    status: ACCOUNT_STATUS,
  },
  optional: { lot_id: "uuid", slippage_bps: "integer", reservation_id: "uuid" },
  objects: {
    mark: { spec: marginPriceSpec },
    execution_price: { spec: marginPriceSpec },
    evaluation: { spec: marginEvaluationSpec },
  },
};

const marginFailureSpec: Spec = { required: { code: "string", detail: "string" }, optional: { account_id: "uuid" } };

const marginFacilitySpec: Spec = {
  required: { id: "uuid", kind: "string", name: "string", status: "string", available: "quantity" },
};

export const marginFacilitiesSpec: Spec = {
  required: { value_domain: SIMULATED, asset_id: "uuid", asset_decimals: "integer" },
  objects: {
    lending_pool: { required: true, spec: marginFacilitySpec },
    insurance_fund: { required: true, spec: marginFacilitySpec },
  },
};

export const marginMarkToMarketSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: {
    evaluated: {
      required: true,
      spec: {
        required: {
          margin_account_id: "uuid",
          account_id: "uuid",
          previous_status: ACCOUNT_STATUS,
          status: ACCOUNT_STATUS,
        },
        objects: { evaluation: { required: true, spec: marginEvaluationSpec }, call: { spec: marginCallSpec } },
      },
    },
    failures: { required: true, spec: marginFailureSpec },
  },
};

export const marginQueueSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: {
    items: {
      required: true,
      spec: {
        required: { numeraire_decimals: "integer" },
        objects: {
          account: { required: true, spec: marginAccountSpec },
          evaluation: { spec: marginEvaluationSpec },
          evaluation_error: { spec: marginFailureSpec },
          open_call: { spec: marginCallSpec },
          latest_case: { spec: marginCaseSpec },
        },
      },
    },
  },
};

export const marginLiquidationResultSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    margin_account_id: "uuid",
    account_id: "uuid",
    eligible: "boolean",
    decision: oneOf(MARGIN_PLAN_DECISIONS),
    reason: "string",
    account_status: ACCOUNT_STATUS,
    numeraire_decimals: "integer",
    replayed: "boolean",
  },
  objects: {
    case: { spec: marginCaseSpec },
    step: { spec: marginStepSpec },
    plan: {
      spec: {
        required: {
          decision: oneOf(MARGIN_PLAN_DECISIONS),
          reason: "string",
          target: oneOf(MARGIN_LIQUIDATION_TARGETS),
          steps_taken: "integer",
          target_units: "quantity",
          deficit_units: "quantity",
          hash: "string",
        },
        arrays: {
          orders: {
            required: true,
            spec: {
              required: {
                action: oneOf(MARGIN_STEP_ACTIONS),
                asset_id: "uuid",
                quantity: "quantity",
                slippage_bps: "integer",
                proceeds: "quantity",
                fee: "quantity",
                repay: "quantity",
                excess: "quantity",
              },
              objects: { mark: { spec: marginPriceSpec }, execution_price: { spec: marginPriceSpec } },
            },
          },
        },
      },
    },
  },
};

export const marginQueueRunSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: {
    results: { required: true, spec: marginLiquidationResultSpec },
    failures: { required: true, spec: marginFailureSpec },
  },
};

export const marginDeficitResultSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    case_id: "uuid",
    deficit: "quantity",
    journal_transaction_id: "uuid",
    replayed: "boolean",
  },
  objects: { case: { required: true, spec: marginCaseSpec } },
};

/**
 * One operator's proposal for ending a MANUAL_REVIEW and, once made, its
 * decision. A pending resolution carries no decision fields.
 */
export const marginCaseResolutionSpec: Spec = {
  required: {
    id: "uuid",
    case_id: "uuid",
    review_transition_id: "uuid",
    outcome: oneOf(MARGIN_RESOLUTION_OUTCOMES),
    reason: "string",
    proposed_by: "string",
    proposed_at: "timestamp",
    pending: "boolean",
  },
  optional: {
    decision: oneOf(MARGIN_RESOLUTION_DECISIONS),
    decided_by: "string",
    decided_at: "timestamp",
    decision_reason: "string",
  },
};

export const marginReviewQueueSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: {
    items: {
      required: true,
      spec: {
        required: { current_review_transition_id: "uuid", entered_review_at: "timestamp" },
        objects: { case: { required: true, spec: marginCaseSpec } },
        arrays: { resolutions: { required: true, spec: marginCaseResolutionSpec } },
      },
    },
  },
};

export const marginWaivedAccrualSpec: Spec = {
  required: { lot_id: "uuid", period_start: "timestamp", period_end: "timestamp", amount: "quantity", waived: "boolean" },
};

export const marginResolutionDecisionResultSpec: Spec = {
  required: { value_domain: SIMULATED, account_status: ACCOUNT_STATUS, replayed: "boolean" },
  objects: {
    resolution: { required: true, spec: marginCaseResolutionSpec },
    case: { required: true, spec: marginCaseSpec },
  },
  arrays: { waived: { required: true, spec: marginWaivedAccrualSpec } },
};

export const marginDeficitRepaymentSpec: Spec = {
  required: {
    id: "uuid",
    amount: "quantity",
    journal_transaction_id: "uuid",
    effect_id: "uuid",
    intent_id: "uuid",
    repaid_at: "timestamp",
  },
};

/**
 * The deficit read. An account without a margin account owes nothing and is
 * answered without the margin account and its numeraire; the figures are
 * present together, and only when the latest case's deficit was covered.
 */
export const marginDeficitStatusSpec: Spec = {
  required: { value_domain: SIMULATED, account_id: "uuid" },
  optional: {
    margin_account_id: "uuid",
    numeraire_asset_id: "uuid",
    numeraire_decimals: "integer",
    deficit: "quantity",
    repaid: "quantity",
    outstanding: "quantity",
  },
  objects: { case: { spec: marginCaseSpec } },
  arrays: { repayments: { required: true, spec: marginDeficitRepaymentSpec } },
};

export const marginDeficitRepaymentResultSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    margin_account_id: "uuid",
    account_id: "uuid",
    case_id: "uuid",
    repayment_id: "uuid",
    intent_id: "uuid",
    journal_transaction_id: "uuid",
    effect_id: "uuid",
    repaid: "quantity",
    outstanding: "quantity",
    numeraire_asset_id: "uuid",
    numeraire_decimals: "integer",
    replayed: "boolean",
  },
};

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

/**
 * The account's margin account, evaluated by the engine at the instant of
 * the read. NOT_FOUND is an answer here, not a fault: the account has no
 * margin account yet.
 */
export function useMarginAccount(accountId: string | undefined): UseQueryResult<MarginAccountDetail> {
  return useQuery({
    queryKey: marginKeys.account(accountId ?? ""),
    enabled: accountId !== undefined,
    // Evaluated by the backend on every call (marks move): refetched, never reused.
    staleTime: 0,
    refetchOnWindowFocus: true,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/margin/account", { params: { query: { account_id: accountId ?? "" } } });
      return validated<MarginAccountDetail>(data, marginAccountDetailSpec, "/advanced/margin/account");
    },
  });
}

function pageQuery(cursor: string | undefined): { limit: number; cursor?: string } {
  return cursor === undefined ? { limit: MARGIN_PAGE_SIZE } : { limit: MARGIN_PAGE_SIZE, cursor };
}

export function useMarginCalls(accountId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<MarginCall>> {
  return useQuery({
    queryKey: marginKeys.calls(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/margin/calls", {
        params: { query: { account_id: accountId ?? "", ...pageQuery(cursor) } },
      });
      const page = validated<Schemas["MarginCallPage"]>(data, marginCallPageSpec, "/advanced/margin/calls");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useMarginCases(accountId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<MarginCase>> {
  return useQuery({
    queryKey: marginKeys.cases(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/margin/cases", {
        params: { query: { account_id: accountId ?? "", ...pageQuery(cursor) } },
      });
      const page = validated<Schemas["MarginCasePage"]>(data, marginCasePageSpec, "/advanced/margin/cases");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/**
 * One liquidation case with its steps. The backend scopes it by the case's
 * own account and answers NOT_FOUND to anyone who may not read it.
 */
export function useMarginCase(accountId: string | undefined, caseId: string | undefined): UseQueryResult<MarginCaseDetail> {
  return useQuery({
    queryKey: marginKeys.case(accountId ?? "", caseId ?? ""),
    enabled: caseId !== undefined && caseId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/margin/cases/{marginCaseId}", {
        params: { path: { marginCaseId: caseId ?? "" } },
      });
      return validated<MarginCaseDetail>(data, marginCaseDetailSpec, "/advanced/margin/cases/{id}");
    },
  });
}

export function useMarginStressRuns(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<MarginStressRunSummary>> {
  return useQuery({
    queryKey: marginKeys.stressRuns(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/margin/stress-runs", {
        params: { query: { account_id: accountId ?? "", ...pageQuery(cursor) } },
      });
      const page = validated<Schemas["MarginStressRunPage"]>(data, marginStressRunPageSpec, "/advanced/margin/stress-runs");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useMarginStressRun(accountId: string | undefined, runId: string | undefined): UseQueryResult<MarginStressRun> {
  return useQuery({
    queryKey: marginKeys.stressRun(accountId ?? "", runId ?? ""),
    enabled: runId !== undefined && runId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/margin/stress-runs/{marginStressRunId}", {
        params: { path: { marginStressRunId: runId ?? "" } },
      });
      return validated<MarginStressRun>(data, marginStressRunSpec, "/advanced/margin/stress-runs/{id}");
    },
  });
}

/** The operator's liquidation queue. A customer is refused by the backend, and that refusal is shown. */
export function useMarginQueue(): UseQueryResult<MarginQueue> {
  return useQuery({
    queryKey: marginKeys.queue,
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/margin/ops/queue", { params: { query: { limit: MARGIN_QUEUE_LIMIT } } });
      return validated<MarginQueue>(data, marginQueueSpec, "/advanced/margin/ops/queue");
    },
  });
}

export function useMarginOperatorCalls(
  status: MarginCallStatus | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<MarginCall>> {
  return useQuery({
    queryKey: marginKeys.operatorCalls(status ?? "", cursor ?? ""),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/margin/ops/calls", {
        params: { query: { ...pageQuery(cursor), ...(status === undefined ? {} : { status }) } },
      });
      const page = validated<Schemas["MarginCallPage"]>(data, marginCallPageSpec, "/advanced/margin/ops/calls");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/**
 * The covered deficit of the account's latest bankrupt case, what was repaid
 * of it and what is still owed. A body without a deficit is an answer: the
 * account owes the insurance fund nothing.
 */
export function useMarginDeficit(accountId: string | undefined): UseQueryResult<MarginDeficitStatus> {
  return useQuery({
    queryKey: marginKeys.deficit(accountId ?? ""),
    enabled: accountId !== undefined,
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/margin/deficit", { params: { query: { account_id: accountId ?? "" } } });
      return validated<MarginDeficitStatus>(data, marginDeficitStatusSpec, "/advanced/margin/deficit");
    },
  });
}

/** Cases in MANUAL_REVIEW, oldest first, each with every resolution proposed for it. */
export function useMarginReviews(): UseQueryResult<MarginReviewQueue> {
  return useQuery({
    queryKey: marginKeys.reviews,
    // Another operator's proposal or decision is exactly what this list exists to show.
    staleTime: 0,
    refetchOnWindowFocus: true,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/margin/ops/reviews", { params: { query: { limit: MARGIN_QUEUE_LIMIT } } });
      return validated<MarginReviewQueue>(data, marginReviewQueueSpec, "/advanced/margin/ops/reviews");
    },
  });
}

/* --------------------------------------------------------------------------
 * Commands
 * ------------------------------------------------------------------------ */

export interface OpenRequest {
  readonly accountId: string;
  readonly marginMode: LeveragedMarginMode;
  readonly mode: (typeof MARGIN_RUN_MODES)[number];
  readonly isolatedAssetId: string | undefined;
}

export interface AccountRequest {
  readonly accountId: string;
}

export interface CollateralRequest {
  readonly accountId: string;
  readonly assetId: string;
  /** Exact base units of the asset. */
  readonly quantity: string;
}

export interface AmountRequest {
  readonly accountId: string;
  /** Exact base units of the account's numeraire. */
  readonly amount: string;
}

/** A mutation of one confirmed attempt; the attempt carries its idempotency key. */
export type MarginCommand<R, T> = UseMutationResult<T, unknown, ConfirmedAttempt<R>>;

/** Every command changes what the account's advanced reads report. */
function useAccountCommand<R extends AccountRequest, T>(run: (attempt: ConfirmedAttempt<R>) => Promise<T>): MarginCommand<R, T> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: run,
    onSettled: (_data, _error, attempt) => {
      // A refusal is recorded too (a REJECTED intent, a risk decision), so the
      // reads are refreshed whatever the answer was.
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(attempt.request.accountId) });
      void queryClient.invalidateQueries({ queryKey: marginKeys.operator });
    },
  });
}

export function useOpenMarginAccount(): MarginCommand<OpenRequest, MarginAccount> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/account", {
      ...idempotent(idempotencyKey),
      body: {
        account_id: request.accountId,
        margin_mode: request.marginMode,
        mode: request.mode,
        ...(request.isolatedAssetId === undefined ? {} : { isolated_asset_id: request.isolatedAssetId }),
      },
    });
    return validated<MarginAccount>(data, marginAccountSpec, "/advanced/margin/account");
  });
}

export function useCloseMarginAccount(): MarginCommand<AccountRequest, MarginAccount> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/account/close", {
      ...idempotent(idempotencyKey),
      body: { account_id: request.accountId },
    });
    return validated<MarginAccount>(data, marginAccountSpec, "/advanced/margin/account/close");
  });
}

export function usePostCollateral(): MarginCommand<CollateralRequest, MarginCollateralResult> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/collateral/post", {
      ...idempotent(idempotencyKey),
      body: { account_id: request.accountId, asset_id: request.assetId, quantity: request.quantity },
    });
    return validated<MarginCollateralResult>(data, marginCollateralResultSpec, "/advanced/margin/collateral/post");
  });
}

export function useReleaseCollateral(): MarginCommand<CollateralRequest, MarginCollateralResult> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/collateral/release", {
      ...idempotent(idempotencyKey),
      body: { account_id: request.accountId, asset_id: request.assetId, quantity: request.quantity },
    });
    return validated<MarginCollateralResult>(data, marginCollateralResultSpec, "/advanced/margin/collateral/release");
  });
}

export function useMarginBorrow(): MarginCommand<AmountRequest, MarginLoanResult> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/borrow", {
      ...idempotent(idempotencyKey),
      body: { account_id: request.accountId, amount: request.amount },
    });
    return validated<MarginLoanResult>(data, marginLoanResultSpec, "/advanced/margin/borrow");
  });
}

export function useMarginRepay(): MarginCommand<AmountRequest, MarginLoanResult> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/repay", {
      ...idempotent(idempotencyKey),
      body: { account_id: request.accountId, amount: request.amount },
    });
    return validated<MarginLoanResult>(data, marginLoanResultSpec, "/advanced/margin/repay");
  });
}

export function useLeveragedBuy(): MarginCommand<CollateralRequest, MarginLeveragedBuyResult> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/leveraged-buys", {
      ...idempotent(idempotencyKey),
      body: { account_id: request.accountId, asset_id: request.assetId, quantity: request.quantity },
    });
    return validated<MarginLeveragedBuyResult>(data, marginLeveragedBuyResultSpec, "/advanced/margin/leveraged-buys");
  });
}

/** A stress run of the caller's own account, or, for an operator, of the account being read. */
export interface StressRequest extends AccountRequest {
  readonly asOperator: boolean;
}

export function useRunMarginStress(): MarginCommand<StressRequest, MarginStressRun> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const options = { ...idempotent(idempotencyKey), body: { account_id: request.accountId } };
    const { data } = request.asOperator
      ? await api.POST("/advanced/margin/ops/stress-runs", options)
      : await api.POST("/advanced/margin/stress-runs", options);
    return validated<MarginStressRun>(data, marginStressRunSpec, "/advanced/margin/stress-runs");
  });
}

/* Operator commands act on any account (sandbox:operate); those that move value need a step-up. */

export interface SeedRequest {
  /** Exact sandbox-dollar base units. */
  readonly poolAmount: string;
  readonly fundAmount: string;
}

export interface BatchRequest {
  readonly limit: number | undefined;
}

export interface LiquidateRequest extends AccountRequest {
  readonly expectedStep: number | undefined;
}

export interface CoverRequest extends AccountRequest {
  readonly caseId: string;
}

function useOperatorCommand<R, T>(run: (attempt: ConfirmedAttempt<R>) => Promise<T>): MarginCommand<R, T> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: run,
    onSettled: () => {
      // An operator action can change any account the area may be reading.
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export function useSeedFacilities(): MarginCommand<SeedRequest, MarginFacilities> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/ops/facilities/seed", {
      ...idempotent(idempotencyKey),
      body: { pool_amount: request.poolAmount, fund_amount: request.fundAmount },
    });
    return validated<MarginFacilities>(data, marginFacilitiesSpec, "/advanced/margin/ops/facilities/seed");
  });
}

export function useMarkToMarket(): MarginCommand<BatchRequest, MarginMarkToMarketResult> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/ops/mark-to-market", {
      ...idempotent(idempotencyKey),
      body: request.limit === undefined ? {} : { limit: request.limit },
    });
    return validated<MarginMarkToMarketResult>(data, marginMarkToMarketSpec, "/advanced/margin/ops/mark-to-market");
  });
}

export function useRunLiquidationQueue(): MarginCommand<BatchRequest, MarginQueueRunResult> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/ops/queue/run", {
      ...idempotent(idempotencyKey),
      body: request.limit === undefined ? {} : { limit: request.limit },
    });
    return validated<MarginQueueRunResult>(data, marginQueueRunSpec, "/advanced/margin/ops/queue/run");
  });
}

export function useLiquidateAccount(): MarginCommand<LiquidateRequest, MarginLiquidationResult> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/ops/liquidations", {
      ...idempotent(idempotencyKey),
      body: {
        account_id: request.accountId,
        ...(request.expectedStep === undefined ? {} : { expected_step: request.expectedStep }),
      },
    });
    return validated<MarginLiquidationResult>(data, marginLiquidationResultSpec, "/advanced/margin/ops/liquidations");
  });
}

export function useCoverDeficit(): MarginCommand<CoverRequest, MarginDeficitResult> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/ops/cases/{marginCaseId}/cover-deficit", {
      ...idempotent(idempotencyKey, { path: { marginCaseId: request.caseId } }),
    });
    return validated<MarginDeficitResult>(data, marginDeficitResultSpec, "/advanced/margin/ops/cases/{id}/cover-deficit");
  });
}

/** Repays part or all of a covered deficit from the caller's own wallet. */
export function useRepayDeficit(): MarginCommand<AmountRequest, MarginDeficitRepaymentResult> {
  return useAccountCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/deficit/repayments", {
      ...idempotent(idempotencyKey),
      body: { account_id: request.accountId, amount: request.amount },
    });
    return validated<MarginDeficitRepaymentResult>(
      data,
      marginDeficitRepaymentResultSpec,
      "/advanced/margin/deficit/repayments",
    );
  });
}

export interface ProposeResolutionRequest {
  readonly caseId: string;
  readonly outcome: MarginResolutionOutcome;
  readonly reason: string;
}

/** One operator's proposal for ending a case's review. Nothing changes until a second operator approves it. */
export function useProposeResolution(): MarginCommand<ProposeResolutionRequest, MarginCaseResolution> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/ops/cases/{marginCaseId}/resolutions", {
      ...idempotent(idempotencyKey, { path: { marginCaseId: request.caseId } }),
      body: { outcome: request.outcome, reason: request.reason },
    });
    return validated<MarginCaseResolution>(data, marginCaseResolutionSpec, "/advanced/margin/ops/cases/{id}/resolutions");
  });
}

export interface DecideResolutionRequest {
  readonly resolutionId: string;
  readonly decision: MarginResolutionDecision;
  readonly reason: string;
}

/** A decision on a proposed resolution; an approval by a second operator applies the outcome. */
export function useDecideResolution(): MarginCommand<DecideResolutionRequest, MarginResolutionDecisionResult> {
  return useOperatorCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/margin/ops/resolutions/{marginResolutionId}/decision", {
      ...idempotent(idempotencyKey, { path: { marginResolutionId: request.resolutionId } }),
      body: { decision: request.decision, reason: request.reason },
    });
    return validated<MarginResolutionDecisionResult>(
      data,
      marginResolutionDecisionResultSpec,
      "/advanced/margin/ops/resolutions/{id}/decision",
    );
  });
}
