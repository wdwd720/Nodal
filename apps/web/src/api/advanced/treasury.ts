/**
 * The treasury section's reads and commands: the house book
 * (internal/treasury, ADR-0020), which trades only simulated house capital.
 *
 * Modelled on `core.ts`: every response is checked against a spec before a
 * component sees it and refused as a ContractViolation otherwise, never
 * repaired; a 4xx is the backend's decision and is not retried.
 *
 * Everything under /advanced/treasury/books is the operator's (sandbox:operate):
 * a customer is refused, and the pages show that refusal. Two reads are a
 * customer's as well: the crossing disclosures of an instrument's book (what
 * customers are told about house books trading against them) and the trades
 * in which a house book was an account's counterparty.
 *
 * Every command carries an Idempotency-Key created once, when the operator
 * confirmed, and reused for every retry of that same request
 * (`confirmAttempt` in lib/treasury.ts). A refused house order or fill is a
 * problem whose fields carry the treasury's reason codes; the refusal is
 * recorded by the treasury, so the reads are refreshed whatever the answer.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { PERSISTABLE_MODES, VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  TREASURY_ACTION_KINDS,
  TREASURY_BOOK_STATUSES,
  TREASURY_CLASSES,
  TREASURY_DIRECTIONS,
  TREASURY_EVENT_KINDS,
  TREASURY_LIMIT_KINDS,
  TREASURY_ORDER_STATUSES,
  TREASURY_ORDER_TYPES,
  TREASURY_OUTCOMES,
  TREASURY_OWNER_TYPES,
  TREASURY_SIDES,
  TREASURY_STRATEGIES,
  TREASURY_TIMES_IN_FORCE,
  TREASURY_VERDICTS,
  type ConfirmedAttempt,
} from "../../lib/treasury.ts";
import { api } from "../client.ts";
import { cursorPageSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient, type CursorPage } from "./core.ts";
import { assetRefSpec, disclosureSpec, exactPriceSpec } from "./fees.ts";

export type TreasuryBook = Schemas["TreasuryBook"];
export type TreasuryBookDetail = Schemas["TreasuryBookDetail"];
export type TreasurySigningPolicy = Schemas["TreasurySigningPolicy"];
export type TreasuryRiskPolicy = Schemas["TreasuryRiskPolicy"];
export type TreasuryAction = Schemas["TreasuryAction"];
export type TreasuryFunding = Schemas["TreasuryFunding"];
export type TreasuryHouseOrder = Schemas["TreasuryHouseOrder"];
export type TreasuryTrade = Schemas["TreasuryTrade"];
export type TreasuryRiskDecision = Schemas["TreasuryRiskDecision"];
export type TreasuryLimitEvaluation = Schemas["TreasuryLimitEvaluation"];
export type TreasuryOrderOutcome = Schemas["TreasuryOrderOutcome"];
export type TreasuryQuoteOutcome = Schemas["TreasuryQuoteOutcome"];
export type TreasuryPaperFill = Schemas["TreasuryPaperFill"];
export type TreasuryPaperOutcome = Schemas["TreasuryPaperOutcome"];
export type TreasuryRebalanceOutcome = Schemas["TreasuryRebalanceOutcome"];
export type TreasurySyncResult = Schemas["TreasurySyncResult"];
export type TreasuryReport = Schemas["TreasuryReport"];
export type TreasuryAssetPnL = Schemas["TreasuryAssetPnL"];
export type TreasuryLimitUse = Schemas["TreasuryLimitUse"];
export type TreasurySnapshot = Schemas["TreasurySnapshot"];
export type TreasuryBalance = Schemas["TreasuryBalance"];
export type TreasuryBalanceList = Schemas["TreasuryBalanceList"];
export type TreasuryReconciliation = Schemas["TreasuryReconciliation"];
export type TreasuryPositionEvent = Schemas["TreasuryPositionEvent"];
export type TreasuryRiskDecisionRecord = Schemas["TreasuryRiskDecisionRecord"];
export type TreasuryCrossingDisclosure = Schemas["TreasuryCrossingDisclosure"];
export type TreasuryCrossingDisclosureList = Schemas["TreasuryCrossingDisclosureList"];
export type TreasuryHouseTrade = Schemas["TreasuryHouseTrade"];
export type TreasuryActionList = Schemas["TreasuryActionList"];
export type TreasuryRegisterRequest = Schemas["TreasuryRegisterRequest"];
export type TreasurySigningPolicyRequest = Schemas["TreasurySigningPolicyRequest"];
export type TreasuryRiskPolicyRequest = Schemas["TreasuryRiskPolicyRequest"];
export type TreasuryFundRequest = Schemas["TreasuryFundRequest"];
export type TreasuryOrderRequest = Schemas["TreasuryOrderRequest"];
export type TreasuryQuoteRequest = Schemas["TreasuryQuoteRequest"];
export type TreasuryHedgeRequest = Schemas["TreasuryHedgeRequest"];
export type TreasuryConvertRequest = Schemas["TreasuryConvertRequest"];
export type TreasuryRebalanceRequest = Schemas["TreasuryRebalanceRequest"];
export type TreasuryPaperRequest = Schemas["TreasuryPaperRequest"];
export type TreasuryCrossingRuleRequest = Schemas["TreasuryCrossingRuleRequest"];

/** Rows per page of every cursor-paged treasury list. */
export const TREASURY_PAGE_SIZE = 25;
/** The action register has no cursor: the newest this many are read. */
export const TREASURY_ACTION_LIMIT = 100;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const treasuryKeys = {
  /** Prefix of every operator read of the house books. */
  all: ["advanced", "treasury"] as const,
  books: (cursor: string) => ["advanced", "treasury", "books", cursor] as const,
  /** Prefix of every read of one house book. */
  book: (bookId: string) => ["advanced", "treasury", "book", bookId] as const,
  detail: (bookId: string) => ["advanced", "treasury", "book", bookId, "detail"] as const,
  report: (bookId: string) => ["advanced", "treasury", "book", bookId, "report"] as const,
  balances: (bookId: string) => ["advanced", "treasury", "book", bookId, "balances"] as const,
  reconciliation: (bookId: string) => ["advanced", "treasury", "book", bookId, "reconciliation"] as const,
  events: (bookId: string, cursor: string) => ["advanced", "treasury", "book", bookId, "events", cursor] as const,
  fills: (bookId: string, cursor: string) => ["advanced", "treasury", "book", bookId, "fills", cursor] as const,
  decisions: (bookId: string, cursor: string) => ["advanced", "treasury", "book", bookId, "decisions", cursor] as const,
  actions: (bookId: string) => ["advanced", "treasury", "book", bookId, "actions"] as const,
  crossing: (instrumentId: string) => ["advanced", "treasury", "crossing", instrumentId] as const,
  houseTrades: (accountId: string, cursor: string) =>
    [...advancedKeys.account(accountId), "treasury", "house-trades", cursor] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
const STRATEGY = oneOf(TREASURY_STRATEGIES);
const SIDE = oneOf(TREASURY_SIDES);

export const signingPolicySpec: Spec = {
  required: {
    house_account_id: "uuid",
    version: "integer",
    key_custody: "string",
    service_actor_id: "string",
    operators_allowed: "boolean",
    policy_hash: "string",
    actor_type: "string",
    actor_id: "string",
    reason: "string",
    effective_at: "timestamp",
  },
};

export const riskPolicySpec: Spec = {
  required: {
    house_account_id: "uuid",
    version: "integer",
    label: "string",
    policy_hash: "string",
    valuation_asset_id: "uuid",
    daily_loss_limit: "quantity",
    max_mark_age_ms: "integer",
    paper_spread_bps: "integer",
    paper_fee_bps: "integer",
    actor_type: "string",
    actor_id: "string",
    reason: "string",
    effective_at: "timestamp",
  },
  arrays: {
    max_inventory: { required: true, spec: { required: { asset_id: "uuid", max_quantity: "quantity" } } },
    max_instrument_exposure: { required: true, spec: { required: { instrument_id: "uuid", max_exposure: "quantity" } } },
  },
};

const bookFields: Spec = {
  required: {
    value_domain: SIMULATED,
    house_account_id: "uuid",
    name: "string",
    strategy: STRATEGY,
    class: oneOf(TREASURY_CLASSES),
    status: oneOf(TREASURY_BOOK_STATUSES),
    version: "integer",
    registered_by_actor_type: "string",
    registered_by_actor_id: "string",
    registered_at: "timestamp",
  },
  lists: { permitted_activities: { required: true, kind: STRATEGY } },
};

export const bookSpec: Spec = {
  ...bookFields,
  objects: { valuation_asset: { required: true, spec: assetRefSpec } },
};

export const bookDetailSpec: Spec = {
  ...bookFields,
  objects: {
    valuation_asset: { required: true, spec: assetRefSpec },
    signing_policy: { required: true, spec: signingPolicySpec },
    risk_policy: { spec: riskPolicySpec },
  },
};

export const bookPageSpec: Spec = { ...cursorPageSpec(bookSpec), required: { value_domain: SIMULATED } };

export const actionSpec: Spec = {
  required: {
    id: "uuid",
    house_account_id: "uuid",
    kind: oneOf(TREASURY_ACTION_KINDS),
    outcome: oneOf(TREASURY_OUTCOMES),
    actor_type: "string",
    actor_id: "string",
    signing_policy_version: "integer",
    idempotency_key: "string",
    occurred_at: "timestamp",
  },
  optional: {
    activity: STRATEGY,
    rejection_code: "string",
    risk_policy_version: "integer",
    risk_decision_id: "uuid",
    order_id: "uuid",
    paper_fill_id: "uuid",
    funding_id: "uuid",
    policy_id: "uuid",
    signing_policy_id: "uuid",
    snapshot_id: "uuid",
    effect_id: "uuid",
    evidence_ref: "string",
    evidence_sha256: "string",
    correlation_id: "string",
  },
  lists: { reason_codes: { required: true, kind: "string" } },
};

export const actionListSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: { items: { required: true, spec: actionSpec } },
};

export const fundingSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    house_account_id: "uuid",
    quantity: "quantity",
    cost_basis: "quantity",
    effect_id: "uuid",
    journal_transaction_id: "uuid",
    evidence_ref: "string",
    funded_at: "timestamp",
  },
  objects: {
    asset: { required: true, spec: assetRefSpec },
    mark: { spec: exactPriceSpec },
    action: { required: true, spec: actionSpec },
  },
};

export const houseOrderSpec: Spec = {
  required: {
    id: "uuid",
    book_id: "uuid",
    instrument_id: "uuid",
    house_account_id: "uuid",
    side: SIDE,
    order_type: oneOf(TREASURY_ORDER_TYPES),
    time_in_force: oneOf(TREASURY_TIMES_IN_FORCE),
    post_only: "boolean",
    reduce_only: "boolean",
    mode: oneOf(PERSISTABLE_MODES),
    price: "string",
    quantity: "quantity",
    filled_quantity: "quantity",
    remaining_quantity: "quantity",
    cancelled_quantity: "quantity",
    status: oneOf(TREASURY_ORDER_STATUSES),
    created_at: "timestamp",
    updated_at: "timestamp",
  },
  optional: { client_order_id: "string", reject_reason: "string", cancel_reason: "string" },
};

export const tradeSpec: Spec = {
  required: {
    id: "uuid",
    book_id: "uuid",
    trade_seq: "integer",
    base_quantity: "quantity",
    quote_notional: "quantity",
    maker_order_id: "uuid",
    taker_order_id: "uuid",
    maker_owner_type: oneOf(TREASURY_OWNER_TYPES),
    taker_owner_type: oneOf(TREASURY_OWNER_TYPES),
    aggressor_side: SIDE,
    maker_fee: "quantity",
    taker_fee: "quantity",
    crossing_rule_version: "integer",
    journal_transaction_id: "uuid",
  },
  objects: { price: { required: true, spec: exactPriceSpec } },
};

export const limitEvaluationSpec: Spec = {
  required: {
    risk_increasing: "boolean",
    loss_measure: "quantity",
    daily_loss_limit: "quantity",
    inventory_after: "quantity",
  },
  optional: { inventory_cap: "quantity", exposure_after: "quantity", exposure_cap: "quantity" },
  lists: {
    codes: { required: true, kind: "string" },
    circuit_breakers: { required: true, kind: "string" },
    missing_marks: { required: true, kind: "string" },
  },
};

export const riskDecisionSpec: Spec = {
  required: { id: "uuid", verdict: oneOf(TREASURY_VERDICTS), decision_hash: "string", input_hash: "string" },
  lists: { reason_codes: { required: true, kind: "string" }, treasury_codes: { required: true, kind: "string" } },
  objects: { limits: { spec: limitEvaluationSpec } },
};

export const orderOutcomeSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: {
    action: { required: true, spec: actionSpec },
    order: { spec: houseOrderSpec },
    decision: { spec: riskDecisionSpec },
  },
  arrays: { trades: { required: true, spec: tradeSpec } },
};

export const quoteOutcomeSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: { bid: { required: true, spec: orderOutcomeSpec }, ask: { required: true, spec: orderOutcomeSpec } },
};

export const paperFillSpec: Spec = {
  required: {
    id: "uuid",
    house_account_id: "uuid",
    activity: STRATEGY,
    venue_label: "string",
    side: SIDE,
    base_quantity: "quantity",
    notional_at_reference: "quantity",
    spread_bps: "integer",
    spread_amount: "quantity",
    fee_amount: "quantity",
    total_quote: "quantity",
    schedule_hash: "string",
    disclosure_hash: "string",
    effect_id: "uuid",
    journal_transaction_id: "uuid",
    evidence_ref: "string",
    filled_at: "timestamp",
  },
  objects: {
    base_asset: { required: true, spec: assetRefSpec },
    quote_asset: { required: true, spec: assetRefSpec },
    reference_price: { required: true, spec: exactPriceSpec },
    disclosure: { required: true, spec: disclosureSpec },
  },
};

export const paperOutcomeSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: {
    action: { required: true, spec: actionSpec },
    fill: { spec: paperFillSpec },
    decision: { spec: riskDecisionSpec },
  },
};

export const rebalanceOutcomeSpec: Spec = {
  required: { value_domain: SIMULATED, no_op: "boolean" },
  objects: {
    action: { spec: actionSpec },
    fill: { spec: paperFillSpec },
    decision: { spec: riskDecisionSpec },
  },
};

export const paperFillPageSpec: Spec = { ...cursorPageSpec(paperFillSpec), required: { value_domain: SIMULATED } };

export const syncResultSpec: Spec = {
  required: { value_domain: SIMULATED, house_account_id: "uuid", ingested: "integer" },
};

export const assetPnlSpec: Spec = {
  required: { quantity: "quantity", cost: "quantity", realized_total: "quantity", realized_today: "quantity" },
  optional: { market_value: "quantity", unrealized: "quantity" },
  objects: { asset: { required: true, spec: assetRefSpec }, mark: { spec: exactPriceSpec } },
};

export const limitUseSpec: Spec = {
  required: { kind: oneOf(TREASURY_LIMIT_KINDS) },
  optional: { asset_id: "uuid", instrument_id: "uuid", cap: "quantity", used: "quantity" },
  lists: { codes: { required: true, kind: "string" } },
};

export const reportSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    house_account_id: "uuid",
    at: "timestamp",
    trading_day: "string",
    through_event_seq: "integer",
    realized_total: "quantity",
    realized_today: "quantity",
    unrealized_total: "quantity",
    open_losses: "quantity",
    loss_measure: "quantity",
    rounding: "object",
  },
  optional: { risk_policy_version: "integer" },
  objects: { valuation_asset: { required: true, spec: assetRefSpec } },
  arrays: { assets: { required: true, spec: assetPnlSpec }, limits: { spec: limitUseSpec } },
  lists: { missing_marks: { required: true, kind: "uuid" } },
};

export const snapshotSpec: Spec = {
  required: { value_domain: SIMULATED, id: "uuid", document_hash: "string" },
  objects: { report: { required: true, spec: reportSpec }, action: { required: true, spec: actionSpec } },
};

const venueFlowSpec: Spec = { required: { bought: "quantity", sold: "quantity" } };

export const balanceSpec: Spec = {
  required: {
    inventory: "quantity",
    reserved: "quantity",
    available: "quantity",
    funded: "quantity",
    working_buys: "quantity",
    working_sells: "quantity",
  },
  objects: {
    asset: { required: true, spec: assetRefSpec },
    internal_book: { required: true, spec: venueFlowSpec },
    paper_venue: { required: true, spec: venueFlowSpec },
  },
};

export const balanceListSpec: Spec = {
  required: { value_domain: SIMULATED, house_account_id: "uuid" },
  arrays: { items: { required: true, spec: balanceSpec } },
};

export const reconciliationSpec: Spec = {
  required: { value_domain: SIMULATED, house_account_id: "uuid", agrees: "boolean" },
  arrays: {
    discrepancies: {
      required: true,
      spec: { required: { asset_id: "uuid", what: "string", expected: "quantity", actual: "quantity" } },
    },
  },
};

export const positionEventSpec: Spec = {
  required: {
    id: "uuid",
    seq: "integer",
    kind: oneOf(TREASURY_EVENT_KINDS),
    direction: oneOf(TREASURY_DIRECTIONS),
    quantity: "quantity",
    value: "quantity",
    fees: "quantity",
    qty_before: "quantity",
    qty_after: "quantity",
    cost_before: "quantity",
    cost_after: "quantity",
    cost_removed: "quantity",
    realized_pnl: "quantity",
    journal_transaction_id: "uuid",
    occurred_at: "timestamp",
  },
  optional: { book_trade_id: "uuid", paper_fill_id: "uuid", funding_id: "uuid", instrument_id: "uuid" },
  objects: { asset: { required: true, spec: assetRefSpec } },
};

export const positionEventPageSpec: Spec = { ...cursorPageSpec(positionEventSpec), required: { value_domain: SIMULATED } };

export const riskDecisionRecordSpec: Spec = {
  required: {
    id: "uuid",
    house_account_id: "uuid",
    intent_kind: "string",
    asset_id: "uuid",
    risk_increasing: "boolean",
    stage: "string",
    verdict: oneOf(TREASURY_VERDICTS),
    input_hash: "string",
    decision_hash: "string",
    risk_policy_version: "string",
    evaluator_version: "string",
    treasury_policy_version: "integer",
    treasury_policy_hash: "string",
    evaluated_at: "timestamp",
  },
  optional: { instrument_id: "uuid" },
  lists: { reason_codes: { required: true, kind: "string" }, treasury_codes: { required: true, kind: "string" } },
  objects: { limits: { required: true, spec: limitEvaluationSpec } },
};

export const riskDecisionPageSpec: Spec = { ...cursorPageSpec(riskDecisionRecordSpec), required: { value_domain: SIMULATED } };

export const crossingDisclosureSpec: Spec = {
  required: {
    id: "uuid",
    instrument_id: "uuid",
    book_id: "uuid",
    rule_version: "integer",
    allow: "boolean",
    disclosure: "string",
    reason: "string",
    actor_type: "string",
    actor_id: "string",
    evidence_ref: "string",
    evidence_sha256: "string",
    disclosed_at: "timestamp",
  },
};

export const crossingDisclosureListSpec: Spec = {
  required: { value_domain: SIMULATED, instrument_id: "uuid" },
  arrays: { items: { required: true, spec: crossingDisclosureSpec } },
};

export const houseTradeSpec: Spec = {
  required: {
    trade_id: "uuid",
    book_id: "uuid",
    instrument_id: "uuid",
    trade_seq: "integer",
    house_account_id: "uuid",
    house_side: SIDE,
    account_side: SIDE,
    base_quantity: "quantity",
    quote_notional: "quantity",
    account_fee: "quantity",
    crossing_rule_version: "integer",
    journal_transaction_id: "uuid",
    traded_at: "timestamp",
  },
  optional: { disclosure: "string" },
  objects: {
    price: { required: true, spec: exactPriceSpec },
    base_asset: { required: true, spec: assetRefSpec },
    quote_asset: { required: true, spec: assetRefSpec },
  },
};

export const houseTradePageSpec: Spec = { ...cursorPageSpec(houseTradeSpec), required: { value_domain: SIMULATED } };

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

function pageQuery(cursor: string | undefined): { limit: number; cursor?: string } {
  return cursor === undefined ? { limit: TREASURY_PAGE_SIZE } : { limit: TREASURY_PAGE_SIZE, cursor };
}

function bookPath(bookId: string | undefined): { path: { houseBookId: string } } {
  return { path: { houseBookId: bookId ?? "" } };
}

const enabledFor = (bookId: string | undefined): boolean => bookId !== undefined && bookId !== "";

/** The registered house books, newest first. A customer is refused, and the refusal is the answer shown. */
export function useTreasuryBooks(cursor: string | undefined): UseQueryResult<CursorPage<TreasuryBook>> {
  return useQuery({
    queryKey: treasuryKeys.books(cursor ?? ""),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/books", { params: { query: pageQuery(cursor) } });
      const page = validated<Schemas["TreasuryBookPage"]>(data, bookPageSpec, "/advanced/treasury/books");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useTreasuryBook(bookId: string | undefined): UseQueryResult<TreasuryBookDetail> {
  return useQuery({
    queryKey: treasuryKeys.detail(bookId ?? ""),
    enabled: enabledFor(bookId),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/books/{houseBookId}", { params: bookPath(bookId) });
      return validated<TreasuryBookDetail>(data, bookDetailSpec, "/advanced/treasury/books/{id}");
    },
  });
}

/**
 * The book's PnL and every limit in use. The treasury syncs the position
 * register and states PnL at fresh marks on every read: refetched, never
 * reused.
 */
export function useTreasuryReport(bookId: string | undefined): UseQueryResult<TreasuryReport> {
  return useQuery({
    queryKey: treasuryKeys.report(bookId ?? ""),
    enabled: enabledFor(bookId),
    staleTime: 0,
    refetchOnWindowFocus: true,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/books/{houseBookId}/report", { params: bookPath(bookId) });
      return validated<TreasuryReport>(data, reportSpec, "/advanced/treasury/books/{id}/report");
    },
  });
}

export function useTreasuryBalances(bookId: string | undefined): UseQueryResult<TreasuryBalanceList> {
  return useQuery({
    queryKey: treasuryKeys.balances(bookId ?? ""),
    enabled: enabledFor(bookId),
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/books/{houseBookId}/balances", { params: bookPath(bookId) });
      return validated<TreasuryBalanceList>(data, balanceListSpec, "/advanced/treasury/books/{id}/balances");
    },
  });
}

export function useTreasuryReconciliation(bookId: string | undefined): UseQueryResult<TreasuryReconciliation> {
  return useQuery({
    queryKey: treasuryKeys.reconciliation(bookId ?? ""),
    enabled: enabledFor(bookId),
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/books/{houseBookId}/reconciliation", { params: bookPath(bookId) });
      return validated<TreasuryReconciliation>(data, reconciliationSpec, "/advanced/treasury/books/{id}/reconciliation");
    },
  });
}

export function useTreasuryEvents(bookId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<TreasuryPositionEvent>> {
  return useQuery({
    queryKey: treasuryKeys.events(bookId ?? "", cursor ?? ""),
    enabled: enabledFor(bookId),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/books/{houseBookId}/events", {
        params: { ...bookPath(bookId), query: pageQuery(cursor) },
      });
      const page = validated<Schemas["TreasuryPositionEventPage"]>(data, positionEventPageSpec, "/advanced/treasury/books/{id}/events");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useTreasuryPaperFills(bookId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<TreasuryPaperFill>> {
  return useQuery({
    queryKey: treasuryKeys.fills(bookId ?? "", cursor ?? ""),
    enabled: enabledFor(bookId),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/books/{houseBookId}/paper-fills", {
        params: { ...bookPath(bookId), query: pageQuery(cursor) },
      });
      const page = validated<Schemas["TreasuryPaperFillPage"]>(data, paperFillPageSpec, "/advanced/treasury/books/{id}/paper-fills");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useTreasuryRiskDecisions(
  bookId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<TreasuryRiskDecisionRecord>> {
  return useQuery({
    queryKey: treasuryKeys.decisions(bookId ?? "", cursor ?? ""),
    enabled: enabledFor(bookId),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/books/{houseBookId}/risk-decisions", {
        params: { ...bookPath(bookId), query: pageQuery(cursor) },
      });
      const page = validated<Schemas["TreasuryRiskDecisionPage"]>(data, riskDecisionPageSpec, "/advanced/treasury/books/{id}/risk-decisions");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useTreasuryActions(bookId: string | undefined): UseQueryResult<TreasuryActionList> {
  return useQuery({
    queryKey: treasuryKeys.actions(bookId ?? ""),
    enabled: enabledFor(bookId),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/books/{houseBookId}/actions", {
        params: { ...bookPath(bookId), query: { limit: TREASURY_ACTION_LIMIT } },
      });
      return validated<TreasuryActionList>(data, actionListSpec, "/advanced/treasury/books/{id}/actions");
    },
  });
}

/** What customers are told about house books trading against them on one instrument's book. */
export function useCrossingDisclosures(instrumentId: string | undefined): UseQueryResult<TreasuryCrossingDisclosureList> {
  return useQuery({
    queryKey: treasuryKeys.crossing(instrumentId ?? ""),
    enabled: instrumentId !== undefined && instrumentId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/crossing-disclosures", {
        params: { query: { instrument_id: instrumentId ?? "" } },
      });
      return validated<TreasuryCrossingDisclosureList>(data, crossingDisclosureListSpec, "/advanced/treasury/crossing-disclosures");
    },
  });
}

/** The trades in which a house book was the account's counterparty. */
export function useHouseTrades(accountId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<TreasuryHouseTrade>> {
  return useQuery({
    queryKey: treasuryKeys.houseTrades(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/treasury/house-trades", {
        params: { query: { account_id: accountId ?? "", ...pageQuery(cursor) } },
      });
      const page = validated<Schemas["TreasuryHouseTradePage"]>(data, houseTradePageSpec, "/advanced/treasury/house-trades");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/* --------------------------------------------------------------------------
 * Commands
 * ------------------------------------------------------------------------ */

/** A mutation of one confirmed attempt; the attempt carries its idempotency key. */
export type TreasuryCommand<R, T> = UseMutationResult<T, unknown, ConfirmedAttempt<R>>;

/** A command addressed to one house book. */
export interface BookRequest<B> {
  readonly bookId: string;
  readonly body: B;
}

/**
 * Every treasury command can change every treasury read, and a house trade
 * can change a customer's reads too; a refusal is recorded by the treasury,
 * so the reads are refreshed whatever the answer was.
 */
function useTreasuryCommand<R, T>(run: (attempt: ConfirmedAttempt<R>) => Promise<T>): TreasuryCommand<R, T> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: run,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export function useRegisterBook(): TreasuryCommand<TreasuryRegisterRequest, TreasuryBookDetail> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books", { ...idempotent(idempotencyKey), body: request });
    return validated<TreasuryBookDetail>(data, bookDetailSpec, "/advanced/treasury/books");
  });
}

export function useSetSigningPolicy(): TreasuryCommand<BookRequest<TreasurySigningPolicyRequest>, TreasurySigningPolicy> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/actor-policy", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
      body: request.body,
    });
    return validated<TreasurySigningPolicy>(data, signingPolicySpec, "/advanced/treasury/books/{id}/actor-policy");
  });
}

export function useSetRiskPolicy(): TreasuryCommand<BookRequest<TreasuryRiskPolicyRequest>, TreasuryRiskPolicy> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/limits", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
      body: request.body,
    });
    return validated<TreasuryRiskPolicy>(data, riskPolicySpec, "/advanced/treasury/books/{id}/limits");
  });
}

export function useFundBook(): TreasuryCommand<BookRequest<TreasuryFundRequest>, TreasuryFunding> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/fundings", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
      body: request.body,
    });
    return validated<TreasuryFunding>(data, fundingSpec, "/advanced/treasury/books/{id}/fundings");
  });
}

export function usePlaceHouseOrder(): TreasuryCommand<BookRequest<TreasuryOrderRequest>, TreasuryOrderOutcome> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/orders", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
      body: request.body,
    });
    return validated<TreasuryOrderOutcome>(data, orderOutcomeSpec, "/advanced/treasury/books/{id}/orders");
  });
}

export interface CancelRequest {
  readonly bookId: string;
  readonly orderId: string;
}

export function useCancelHouseOrder(): TreasuryCommand<CancelRequest, TreasuryOrderOutcome> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/orders/{orderId}/cancel", {
      ...idempotent(idempotencyKey, { path: { houseBookId: request.bookId, orderId: request.orderId } }),
    });
    return validated<TreasuryOrderOutcome>(data, orderOutcomeSpec, "/advanced/treasury/books/{id}/orders/{id}/cancel");
  });
}

export function useQuoteTwoSided(): TreasuryCommand<BookRequest<TreasuryQuoteRequest>, TreasuryQuoteOutcome> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/quotes", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
      body: request.body,
    });
    return validated<TreasuryQuoteOutcome>(data, quoteOutcomeSpec, "/advanced/treasury/books/{id}/quotes");
  });
}

export function useHedge(): TreasuryCommand<BookRequest<TreasuryHedgeRequest>, TreasuryPaperOutcome> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/hedges", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
      body: request.body,
    });
    return validated<TreasuryPaperOutcome>(data, paperOutcomeSpec, "/advanced/treasury/books/{id}/hedges");
  });
}

export function useConvert(): TreasuryCommand<BookRequest<TreasuryConvertRequest>, TreasuryPaperOutcome> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/conversions", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
      body: request.body,
    });
    return validated<TreasuryPaperOutcome>(data, paperOutcomeSpec, "/advanced/treasury/books/{id}/conversions");
  });
}

export function useRebalance(): TreasuryCommand<BookRequest<TreasuryRebalanceRequest>, TreasuryRebalanceOutcome> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/rebalances", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
      body: request.body,
    });
    return validated<TreasuryRebalanceOutcome>(data, rebalanceOutcomeSpec, "/advanced/treasury/books/{id}/rebalances");
  });
}

export function usePaperFill(): TreasuryCommand<BookRequest<TreasuryPaperRequest>, TreasuryPaperOutcome> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/paper-fills", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
      body: request.body,
    });
    return validated<TreasuryPaperOutcome>(data, paperOutcomeSpec, "/advanced/treasury/books/{id}/paper-fills");
  });
}

export interface BookOnlyRequest {
  readonly bookId: string;
}

export function useSyncBook(): TreasuryCommand<BookOnlyRequest, TreasurySyncResult> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/sync", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
    });
    return validated<TreasurySyncResult>(data, syncResultSpec, "/advanced/treasury/books/{id}/sync");
  });
}

export function useMarkBookToMarket(): TreasuryCommand<BookOnlyRequest, TreasurySnapshot> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/books/{houseBookId}/mark-to-market", {
      ...idempotent(idempotencyKey, bookPath(request.bookId)),
    });
    return validated<TreasurySnapshot>(data, snapshotSpec, "/advanced/treasury/books/{id}/mark-to-market");
  });
}

export function useSetCrossingRule(): TreasuryCommand<TreasuryCrossingRuleRequest, TreasuryCrossingDisclosure> {
  return useTreasuryCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/treasury/crossing-rules", { ...idempotent(idempotencyKey), body: request });
    return validated<TreasuryCrossingDisclosure>(data, crossingDisclosureSpec, "/advanced/treasury/crossing-rules");
  });
}
