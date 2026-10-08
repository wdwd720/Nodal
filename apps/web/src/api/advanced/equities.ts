/**
 * Reads and commands of the equities section (internal/equities, ADR-0020):
 * listings with their marks and the session in force, the XNYS calendar, the
 * account's orders, fills, T+1 settlements and corporate-action postings,
 * corporate actions, the customer's order and cancel, and the operator's
 * listing, holiday, corporate-action and worker-run commands.
 *
 * Like core.ts, which it follows:
 *
 *   - every response is checked against a spec before a component sees it,
 *     including the rules a per-field spec cannot state (a fill's charges
 *     are disclosed in its settlement asset; a settled settlement says when;
 *     a placement that says it was placed says what the venue answered);
 *   - query keys start with `advancedKeys.all`, and account-scoped ones with
 *     `advancedKeys.account(accountId)`;
 *   - a 4xx answer is the backend's decision and is not retried;
 *   - every command carries the idempotency key its form created once, when
 *     the person confirmed, and reuses it — with the same request — for every
 *     retry.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  EQUITY_ACTION_STATUSES,
  EQUITY_ACTION_TYPES,
  EQUITY_KINDS,
  EQUITY_MODES,
  EQUITY_ORDER_STATUSES,
  EQUITY_ORDER_TYPES,
  EQUITY_PLACEMENT_STATUSES,
  EQUITY_ROUNDINGS,
  EQUITY_SESSIONS,
  EQUITY_SETTLEMENT_STATES,
  EQUITY_SIDES,
  EQUITY_VENUE_ACTIONS,
  EQUITY_VENUE_STATUSES,
  type EquityActionType,
  type EquityKind,
  type EquityMode,
  type EquityOrderType,
  type EquityRiskClass,
  type EquityRounding,
  type EquitySettlementFilter,
  type EquitySide,
} from "../../lib/equities.ts";
import { api } from "../client.ts";
import { ContractViolation, cursorPageSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient, type CursorPage } from "./core.ts";

export type EquityListing = Schemas["AdvancedEquityListing"];
export type EquityListingList = Schemas["AdvancedEquityListingList"];
export type EquitySessionState = Schemas["AdvancedEquitySessionState"];
export type EquityCalendar = Schemas["AdvancedEquityCalendar"];
export type EquityOrder = Schemas["AdvancedEquityOrder"];
export type EquityFill = Schemas["AdvancedEquityFill"];
export type EquitySettlement = Schemas["AdvancedEquitySettlement"];
export type EquityOrderSubmission = Schemas["AdvancedEquityOrderSubmission"];
export type EquityPlacement = Schemas["AdvancedEquityPlacement"];
export type EquityCorporateAction = Schemas["AdvancedEquityCorporateAction"];
export type EquityApplication = Schemas["AdvancedEquityApplication"];
export type EquityCorporateActionResult = Schemas["AdvancedEquityCorporateActionResult"];
export type EquitySettlementRun = Schemas["AdvancedEquitySettlementRun"];
export type EquityVenueRun = Schemas["AdvancedEquityVenueRun"];
export type EquityVenueRunItem = Schemas["AdvancedEquityVenueRunItem"];

/** Rows per page of the equities lists. */
export const EQUITIES_PAGE_SIZE = 25;
/** The listing list has no cursor, so the contract's maximum is asked for. */
export const LISTING_LIMIT = 200;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const equitiesKeys = {
  /** Prefix of every equities read that is not scoped to an account. */
  all: [...advancedKeys.all, "equities"] as const,
  listings: [...advancedKeys.all, "equities", "listings"] as const,
  listing: (instrumentId: string) => [...advancedKeys.all, "equities", "listing", instrumentId] as const,
  calendar: [...advancedKeys.all, "equities", "calendar"] as const,
  actions: (instrumentId: string, cursor: string) => [...advancedKeys.all, "equities", "actions", instrumentId, cursor] as const,
  action: (actionId: string) => [...advancedKeys.all, "equities", "action", actionId] as const,
  orders: (accountId: string, cursor: string) => [...advancedKeys.account(accountId), "equities", "orders", cursor] as const,
  order: (accountId: string, orderId: string) => [...advancedKeys.account(accountId), "equities", "order", orderId] as const,
  fills: (accountId: string, cursor: string) => [...advancedKeys.account(accountId), "equities", "fills", cursor] as const,
  settlements: (accountId: string, state: string, cursor: string) =>
    [...advancedKeys.account(accountId), "equities", "settlements", state, cursor] as const,
  applications: (accountId: string, cursor: string) =>
    [...advancedKeys.account(accountId), "equities", "applications", cursor] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
const DATE = "string";

export const equityMarkSpec: Spec = { required: { price: "decimal", source: "string", observed_at: "timestamp" } };

export const equitySessionSpec: Spec = {
  required: {
    calendar: "string",
    timezone: "string",
    as_of: "timestamp",
    session: oneOf(EQUITY_SESSIONS),
    trade_date: DATE,
    trading_day: "boolean",
    next_trading_day: DATE,
  },
  optional: { holiday_name: "string", settlement_date: DATE },
};

export const equityListingSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    instrument_id: "uuid",
    kind: oneOf(EQUITY_KINDS),
    symbol: "string",
    name: "string",
    exchange_mic: "string",
    status: "string",
    terms_version: "integer",
    terms_hash: "string",
    trading_calendar: "string",
    venue: "string",
    share_asset_id: "uuid",
    share_symbol: "string",
    share_decimals: "integer",
    settlement_asset_id: "uuid",
    settlement_symbol: "string",
    settlement_decimals: "integer",
    price_scale: "integer",
    tick_price: "decimal",
    lot_size: "quantity",
    min_quantity: "quantity",
    oracle_max_age_ms: "integer",
  },
  objects: { mark: { spec: equityMarkSpec } },
};

export const equityListingListSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: { items: { required: true, spec: equityListingSpec } },
  objects: { session: { required: true, spec: equitySessionSpec } },
};

export const equityCalendarSpec: Spec = {
  required: { settlement_cycle_days: "integer" },
  objects: { state: { required: true, spec: equitySessionSpec } },
  arrays: { holidays: { required: true, spec: { required: { date: DATE, name: "string" } } } },
};

export const equitySettlementSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    fill_id: "uuid",
    order_id: "uuid",
    account_id: "uuid",
    instrument_id: "uuid",
    asset_id: "uuid",
    asset_symbol: "string",
    asset_decimals: "integer",
    quantity: "quantity",
    trade_date: DATE,
    due_date: DATE,
    state: oneOf(EQUITY_SETTLEMENT_STATES),
  },
  optional: { settled_at: "timestamp", journal_transaction_id: "uuid" },
};

export const equityFillSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    order_id: "uuid",
    account_id: "uuid",
    instrument_id: "uuid",
    side: oneOf(EQUITY_SIDES),
    quantity: "quantity",
    share_decimals: "integer",
    price: "decimal",
    slippage_bps: "integer",
    notional: "quantity",
    spread: "quantity",
    platform_fees: "quantity",
    venue_fees: "quantity",
    total: "quantity",
    settlement_asset_id: "uuid",
    settlement_symbol: "string",
    settlement_decimals: "integer",
    fee_schedule_version: "string",
    fee_schedule_hash: "string",
    fee_disclosure_hash: "string",
    venue: "string",
    venue_fill_id: "string",
    executed_at: "timestamp",
    trade_date: DATE,
    settlement_date: DATE,
    journal_transaction_id: "uuid",
    effect_id: "uuid",
  },
  objects: { mark: { required: true, spec: equityMarkSpec }, settlement: { spec: equitySettlementSpec } },
};

export const equityOrderSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    account_id: "uuid",
    instrument_id: "uuid",
    symbol: "string",
    exchange_mic: "string",
    side: oneOf(EQUITY_SIDES),
    order_type: oneOf(EQUITY_ORDER_TYPES),
    mode: oneOf(EQUITY_MODES),
    status: oneOf(EQUITY_ORDER_STATUSES),
    quantity: "quantity",
    share_asset_id: "uuid",
    share_decimals: "integer",
    settlement_asset_id: "uuid",
    price_scale: "integer",
    tick_price: "decimal",
    extended_hours: "boolean",
    session: oneOf(EQUITY_SESSIONS),
    trade_date: DATE,
    expires_at: "timestamp",
    intent_id: "uuid",
    venue: "string",
    created_at: "timestamp",
    updated_at: "timestamp",
  },
  optional: {
    limit_price: "decimal",
    protection_price: "decimal",
    reserved_quantity: "quantity",
    reservation_id: "uuid",
    risk_decision_id: "uuid",
    rejection_code: "string",
    venue_order_id: "string",
  },
  lists: { rejection_reasons: { required: true, kind: "string" } },
  objects: { fill: { spec: equityFillSpec } },
};

export const equityOrderPageSpec: Spec = { ...cursorPageSpec(equityOrderSpec), required: { value_domain: SIMULATED } };
export const equityFillPageSpec: Spec = { ...cursorPageSpec(equityFillSpec), required: { value_domain: SIMULATED } };
export const equitySettlementPageSpec: Spec = {
  ...cursorPageSpec(equitySettlementSpec),
  required: { value_domain: SIMULATED },
};

export const equityPlacementSpec: Spec = {
  required: { status: oneOf(EQUITY_PLACEMENT_STATUSES), detail: "string" },
  optional: { venue_status: oneOf(EQUITY_VENUE_STATUSES), venue_reason: "string", error_code: "string" },
};

export const equitySubmissionSpec: Spec = {
  required: { value_domain: SIMULATED },
  objects: { order: { required: true, spec: equityOrderSpec }, placement: { required: true, spec: equityPlacementSpec } },
};

export const equityActionSpec: Spec = {
  required: {
    id: "uuid",
    instrument_id: "uuid",
    type: oneOf(EQUITY_ACTION_TYPES),
    status: oneOf(EQUITY_ACTION_STATUSES),
    effective_date: DATE,
    share_asset_id: "uuid",
    settlement_asset_id: "uuid",
    rounding: oneOf(EQUITY_ROUNDINGS),
    reason: "string",
    content_hash: "string",
    announced_by_actor_type: "string",
    announced_by_actor_id: "string",
    created_at: "timestamp",
  },
  optional: {
    dividend_per_share: "decimal",
    in_lieu_price: "decimal",
    ex_date: DATE,
    record_date: DATE,
    pay_date: DATE,
    final_date: DATE,
    new_symbol: "string",
    applied_at: "timestamp",
  },
  objects: { ratio: { spec: { required: { numerator: "integer", denominator: "integer" } } } },
};

export const equityActionPageSpec: Spec = cursorPageSpec(equityActionSpec);

export const equityApplicationSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    action_id: "uuid",
    account_id: "uuid",
    entitled_quantity: "quantity",
    shares_delta: "quantity",
    share_symbol: "string",
    share_decimals: "integer",
    settlement_amount: "quantity",
    settlement_symbol: "string",
    settlement_decimals: "integer",
    exact_amount_numerator: "quantity",
    exact_amount_denominator: "quantity",
    rounding: oneOf(EQUITY_ROUNDINGS),
    created_at: "timestamp",
  },
  optional: {
    action_type: oneOf(EQUITY_ACTION_TYPES),
    instrument_id: "uuid",
    journal_transaction_id: "uuid",
    effect_id: "uuid",
  },
};

export const equityApplicationPageSpec: Spec = {
  ...cursorPageSpec(equityApplicationSpec),
  required: { value_domain: SIMULATED },
};

export const equityActionResultSpec: Spec = {
  required: { value_domain: SIMULATED, replayed: "boolean" },
  optional: { terms_version: "integer", instrument_status: "string" },
  objects: { action: { required: true, spec: equityActionSpec } },
  arrays: { applications: { required: true, spec: equityApplicationSpec } },
};

export const equitySettlementRunSpec: Spec = {
  required: { value_domain: SIMULATED, as_of: "timestamp", trade_date: DATE, batches: "integer" },
  arrays: { settled: { required: true, spec: equitySettlementSpec } },
};

export const equityVenueRunSpec: Spec = {
  required: { value_domain: SIMULATED, as_of: "timestamp" },
  arrays: {
    items: {
      required: true,
      spec: {
        required: { order_id: "uuid", action: oneOf(EQUITY_VENUE_ACTIONS), status_before: oneOf(EQUITY_ORDER_STATUSES) },
        optional: {
          status_after: oneOf(EQUITY_ORDER_STATUSES),
          venue_status: oneOf(EQUITY_VENUE_STATUSES),
          venue_reason: "string",
          error_code: "string",
          detail: "string",
        },
      },
    },
  },
};

/* --------------------------------------------------------------------------
 * The rules a per-field spec cannot state
 * ------------------------------------------------------------------------ */

const DAY = /^[0-9]{4}-[0-9]{2}-[0-9]{2}$/;

function checkDay(value: string | undefined, path: string): void {
  if (value !== undefined && !DAY.test(value)) throw new ContractViolation(path, "expected a YYYY-MM-DD date");
}

/** A settlement says when it settled exactly when it is SETTLED, and is due on or after its trade date. */
export function checkSettlement(value: unknown, path: string): EquitySettlement {
  const s = validated<EquitySettlement>(value, equitySettlementSpec, path);
  checkDay(s.trade_date, `${path}.trade_date`);
  checkDay(s.due_date, `${path}.due_date`);
  if ((s.state === "SETTLED") !== (s.settled_at !== undefined)) {
    throw new ContractViolation(`${path}.settled_at`, `a ${s.state} settlement ${s.state === "SETTLED" ? "says" : "does not say"} when it settled`);
  }
  if (s.due_date < s.trade_date) throw new ContractViolation(`${path}.due_date`, "a settlement is due on or after its trade date");
  return s;
}

/** A fill's charges are in its settlement asset, its settlement is its own, and it settles after its trade date. */
export function checkFill(value: unknown, path: string): EquityFill {
  const f = validated<EquityFill>(value, equityFillSpec, path);
  checkDay(f.trade_date, `${path}.trade_date`);
  checkDay(f.settlement_date, `${path}.settlement_date`);
  if (f.settlement_date < f.trade_date) {
    throw new ContractViolation(`${path}.settlement_date`, "a fill settles on or after its trade date");
  }
  if (f.settlement !== undefined) {
    checkSettlement(f.settlement, `${path}.settlement`);
    if (f.settlement.fill_id.toLowerCase() !== f.id.toLowerCase()) {
      throw new ContractViolation(`${path}.settlement.fill_id`, "the settlement of another fill");
    }
  }
  return f;
}

/** An order's fill is its own, and a FILLED order carries it. */
export function checkOrder(value: unknown, path: string): EquityOrder {
  const o = validated<EquityOrder>(value, equityOrderSpec, path);
  checkDay(o.trade_date, `${path}.trade_date`);
  if (o.fill !== undefined) {
    checkFill(o.fill, `${path}.fill`);
    if (o.fill.order_id.toLowerCase() !== o.id.toLowerCase()) {
      throw new ContractViolation(`${path}.fill.order_id`, "the fill of another order");
    }
  }
  if (o.status === "FILLED" && o.fill === undefined) {
    throw new ContractViolation(`${path}.fill`, "a FILLED order carries its fill");
  }
  return o;
}

/** A placement that says it was placed names the venue's answer; one that was not says why. */
export function checkSubmission(value: unknown, path: string): EquityOrderSubmission {
  const s = validated<EquityOrderSubmission>(value, equitySubmissionSpec, path);
  checkOrder(s.order, `${path}.order`);
  if (s.placement.status === "NOT_PLACED" && s.placement.venue_status !== undefined) {
    throw new ContractViolation(`${path}.placement.venue_status`, "a placement that did not happen has no venue answer");
  }
  return s;
}

function checkSession(s: EquitySessionState, path: string): void {
  checkDay(s.trade_date, `${path}.trade_date`);
  checkDay(s.next_trading_day, `${path}.next_trading_day`);
  checkDay(s.settlement_date, `${path}.settlement_date`);
}

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

export function useEquityListings(): UseQueryResult<EquityListingList> {
  return useQuery({
    queryKey: equitiesKeys.listings,
    // The marks and the session move; read afresh.
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/equities/listings", { params: { query: { limit: LISTING_LIMIT } } });
      const list = validated<EquityListingList>(data, equityListingListSpec, "/advanced/equities/listings");
      checkSession(list.session, "/advanced/equities/listings.session");
      return list;
    },
  });
}

export function useEquityListing(instrumentId: string | undefined): UseQueryResult<EquityListing> {
  return useQuery({
    queryKey: equitiesKeys.listing(instrumentId ?? ""),
    enabled: instrumentId !== undefined && instrumentId !== "",
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/equities/listings/{instrumentId}", {
        params: { path: { instrumentId: instrumentId ?? "" } },
      });
      return validated<EquityListing>(data, equityListingSpec, "/advanced/equities/listings/{id}");
    },
  });
}

export function useEquityCalendar(): UseQueryResult<EquityCalendar> {
  return useQuery({
    queryKey: equitiesKeys.calendar,
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/equities/calendar", {});
      const calendar = validated<EquityCalendar>(data, equityCalendarSpec, "/advanced/equities/calendar");
      checkSession(calendar.state, "/advanced/equities/calendar.state");
      calendar.holidays.forEach((h, index) => {
        checkDay(h.date, `/advanced/equities/calendar.holidays[${String(index)}].date`);
      });
      return calendar;
    },
  });
}

export function useEquityOrders(accountId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<EquityOrder>> {
  return useQuery({
    queryKey: equitiesKeys.orders(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/equities/orders", {
        params: { query: { account_id: accountId ?? "", limit: EQUITIES_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) } },
      });
      const page = validated<Schemas["AdvancedEquityOrderPage"]>(data, equityOrderPageSpec, "/advanced/equities/orders");
      return {
        items: page.items.map((item, index) => checkOrder(item, `/advanced/equities/orders.items[${String(index)}]`)),
        nextCursor: page.next_cursor,
      };
    },
  });
}

/**
 * One order. The key carries the account the area reads; the backend decides
 * from the session whether the order may be read and answers NOT_FOUND when
 * it may not.
 */
export function useEquityOrder(accountId: string | undefined, orderId: string | undefined): UseQueryResult<EquityOrder> {
  return useQuery({
    queryKey: equitiesKeys.order(accountId ?? "", orderId ?? ""),
    enabled: orderId !== undefined && orderId !== "",
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/equities/orders/{orderId}", { params: { path: { orderId: orderId ?? "" } } });
      return checkOrder(data, "/advanced/equities/orders/{id}");
    },
  });
}

export function useEquityFills(accountId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<EquityFill>> {
  return useQuery({
    queryKey: equitiesKeys.fills(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/equities/fills", {
        params: { query: { account_id: accountId ?? "", limit: EQUITIES_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) } },
      });
      const page = validated<Schemas["AdvancedEquityFillPage"]>(data, equityFillPageSpec, "/advanced/equities/fills");
      return {
        items: page.items.map((item, index) => checkFill(item, `/advanced/equities/fills.items[${String(index)}]`)),
        nextCursor: page.next_cursor,
      };
    },
  });
}

export function useEquitySettlements(
  accountId: string | undefined,
  state: EquitySettlementFilter,
  cursor: string | undefined,
): UseQueryResult<CursorPage<EquitySettlement>> {
  return useQuery({
    queryKey: equitiesKeys.settlements(accountId ?? "", state, cursor ?? ""),
    enabled: accountId !== undefined,
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/equities/settlements", {
        params: {
          query: { account_id: accountId ?? "", state, limit: EQUITIES_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) },
        },
      });
      const page = validated<Schemas["AdvancedEquitySettlementPage"]>(data, equitySettlementPageSpec, "/advanced/equities/settlements");
      return {
        items: page.items.map((item, index) => checkSettlement(item, `/advanced/equities/settlements.items[${String(index)}]`)),
        nextCursor: page.next_cursor,
      };
    },
  });
}

export function useEquityApplications(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<EquityApplication>> {
  return useQuery({
    queryKey: equitiesKeys.applications(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/equities/corporate-action-applications", {
        params: { query: { account_id: accountId ?? "", limit: EQUITIES_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) } },
      });
      const page = validated<Schemas["AdvancedEquityApplicationPage"]>(
        data,
        equityApplicationPageSpec,
        "/advanced/equities/corporate-action-applications",
      );
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useCorporateActions(
  instrumentId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<EquityCorporateAction>> {
  return useQuery({
    queryKey: equitiesKeys.actions(instrumentId ?? "", cursor ?? ""),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/equities/corporate-actions", {
        params: {
          query: {
            limit: EQUITIES_PAGE_SIZE,
            ...(instrumentId === undefined ? {} : { instrument_id: instrumentId }),
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["AdvancedEquityCorporateActionPage"]>(data, equityActionPageSpec, "/advanced/equities/corporate-actions");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useCorporateAction(actionId: string | undefined): UseQueryResult<EquityCorporateAction> {
  return useQuery({
    queryKey: equitiesKeys.action(actionId ?? ""),
    enabled: actionId !== undefined && actionId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/equities/corporate-actions/{actionId}", {
        params: { path: { actionId: actionId ?? "" } },
      });
      return validated<EquityCorporateAction>(data, equityActionSpec, "/advanced/equities/corporate-actions/{id}");
    },
  });
}

/* --------------------------------------------------------------------------
 * The customer's commands
 * ------------------------------------------------------------------------ */

export interface SubmitEquityOrderCommand {
  readonly accountId: string;
  readonly instrumentId: string;
  readonly side: EquitySide;
  readonly orderType: EquityOrderType;
  /** Exact base units of the share asset. */
  readonly quantity: string;
  /** Exact decimal; a LIMIT order only. */
  readonly limitPrice: string | undefined;
  readonly extendedHours: boolean;
  readonly mode: EquityMode;
  /** Created once when the person confirmed, reused for every retry of this action. */
  readonly idempotencyKey: string;
}

export function useSubmitEquityOrder(): UseMutationResult<EquityOrderSubmission, unknown, SubmitEquityOrderCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (command: SubmitEquityOrderCommand) => {
      const { data } = await api.POST("/advanced/equities/orders", {
        ...idempotent(command.idempotencyKey),
        body: {
          account_id: command.accountId,
          instrument_id: command.instrumentId,
          side: command.side,
          order_type: command.orderType,
          quantity: command.quantity,
          extended_hours: command.extendedHours,
          mode: command.mode,
          ...(command.limitPrice === undefined ? {} : { limit_price: command.limitPrice }),
        },
      });
      return checkSubmission(data, "/advanced/equities/orders");
    },
    // A refusal is recorded too (the order and its intent): the account's reads changed either way.
    onSettled: (_result, _error, command) => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(command.accountId) });
    },
  });
}

export interface CancelEquityOrderCommand {
  readonly accountId: string;
  readonly orderId: string;
  readonly idempotencyKey: string;
}

export function useCancelEquityOrder(): UseMutationResult<EquityOrder, unknown, CancelEquityOrderCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (command: CancelEquityOrderCommand) => {
      const { data } = await api.POST("/advanced/equities/orders/{orderId}/cancel", {
        ...idempotent(command.idempotencyKey, { path: { orderId: command.orderId } }),
        body: { account_id: command.accountId },
      });
      return checkOrder(data, "/advanced/equities/orders/{id}/cancel");
    },
    onSettled: (_result, _error, command) => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(command.accountId) });
    },
  });
}

/* --------------------------------------------------------------------------
 * The operator's commands
 * ------------------------------------------------------------------------ */

export interface ListEquityCommand {
  readonly kind: EquityKind;
  readonly symbol: string;
  readonly name: string;
  readonly exchangeMic: string;
  /** Undefined lists it in the sandbox dollar. */
  readonly settlementAssetId: string | undefined;
  readonly shareDecimals: number;
  readonly priceScale: number;
  /** Price units at the price scale. */
  readonly tickSize: string;
  /** Base units of the share asset. */
  readonly lotSize: string;
  readonly minQuantity: string;
  readonly oracleMaxAgeMs: number;
  readonly oracleSources: readonly string[];
  readonly riskClass: EquityRiskClass;
  readonly idempotencyKey: string;
}

export function useListEquity(): UseMutationResult<EquityListing, unknown, ListEquityCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (command: ListEquityCommand) => {
      const { data } = await api.POST("/advanced/equities/listings", {
        ...idempotent(command.idempotencyKey),
        body: {
          kind: command.kind,
          symbol: command.symbol,
          name: command.name,
          exchange_mic: command.exchangeMic,
          share_decimals: command.shareDecimals,
          price_scale: command.priceScale,
          tick_size: command.tickSize,
          lot_size: command.lotSize,
          min_quantity: command.minQuantity,
          oracle_max_age_ms: command.oracleMaxAgeMs,
          oracle_sources: [...command.oracleSources],
          risk_class: command.riskClass,
          ...(command.settlementAssetId === undefined ? {} : { settlement_asset_id: command.settlementAssetId }),
        },
      });
      return validated<EquityListing>(data, equityListingSpec, "/advanced/equities/listings");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: equitiesKeys.all });
    },
  });
}

export interface AddHolidayCommand {
  readonly date: string;
  readonly name: string;
  readonly idempotencyKey: string;
}

export function useAddHoliday(): UseMutationResult<EquityCalendar, unknown, AddHolidayCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (command: AddHolidayCommand) => {
      const { data } = await api.POST("/advanced/equities/calendar/holidays", {
        ...idempotent(command.idempotencyKey),
        body: { date: command.date, name: command.name },
      });
      return validated<EquityCalendar>(data, equityCalendarSpec, "/advanced/equities/calendar/holidays");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: equitiesKeys.all });
    },
  });
}

export interface AnnounceActionCommand {
  readonly instrumentId: string;
  readonly type: EquityActionType;
  readonly reason: string;
  readonly effectiveDate: string | undefined;
  readonly ratio: { readonly numerator: number; readonly denominator: number } | undefined;
  readonly dividendPerShare: string | undefined;
  readonly inLieuPrice: string | undefined;
  readonly exDate: string | undefined;
  readonly recordDate: string | undefined;
  readonly payDate: string | undefined;
  readonly finalDate: string | undefined;
  readonly newSymbol: string | undefined;
  readonly rounding: EquityRounding | undefined;
  readonly idempotencyKey: string;
}

export function useAnnounceCorporateAction(): UseMutationResult<EquityCorporateAction, unknown, AnnounceActionCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (c: AnnounceActionCommand) => {
      const { data } = await api.POST("/advanced/equities/corporate-actions", {
        ...idempotent(c.idempotencyKey),
        body: {
          instrument_id: c.instrumentId,
          type: c.type,
          reason: c.reason,
          ...(c.effectiveDate === undefined ? {} : { effective_date: c.effectiveDate }),
          ...(c.ratio === undefined ? {} : { ratio: { numerator: c.ratio.numerator, denominator: c.ratio.denominator } }),
          ...(c.dividendPerShare === undefined ? {} : { dividend_per_share: c.dividendPerShare }),
          ...(c.inLieuPrice === undefined ? {} : { in_lieu_price: c.inLieuPrice }),
          ...(c.exDate === undefined ? {} : { ex_date: c.exDate }),
          ...(c.recordDate === undefined ? {} : { record_date: c.recordDate }),
          ...(c.payDate === undefined ? {} : { pay_date: c.payDate }),
          ...(c.finalDate === undefined ? {} : { final_date: c.finalDate }),
          ...(c.newSymbol === undefined ? {} : { new_symbol: c.newSymbol }),
          ...(c.rounding === undefined ? {} : { rounding: c.rounding }),
        },
      });
      return validated<EquityCorporateAction>(data, equityActionSpec, "/advanced/equities/corporate-actions");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: equitiesKeys.all });
    },
  });
}

export interface ActionCommand {
  readonly actionId: string;
  readonly idempotencyKey: string;
}

export function useCancelCorporateAction(): UseMutationResult<
  EquityCorporateAction,
  unknown,
  ActionCommand & { readonly reason: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (c: ActionCommand & { readonly reason: string }) => {
      const { data } = await api.POST("/advanced/equities/corporate-actions/{actionId}/cancel", {
        ...idempotent(c.idempotencyKey, { path: { actionId: c.actionId } }),
        body: { reason: c.reason },
      });
      return validated<EquityCorporateAction>(data, equityActionSpec, "/advanced/equities/corporate-actions/{id}/cancel");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: equitiesKeys.all });
    },
  });
}

export function useApplyCorporateAction(): UseMutationResult<EquityCorporateActionResult, unknown, ActionCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (c: ActionCommand) => {
      const { data } = await api.POST("/advanced/equities/corporate-actions/{actionId}/apply", {
        ...idempotent(c.idempotencyKey, { path: { actionId: c.actionId } }),
      });
      return validated<EquityCorporateActionResult>(data, equityActionResultSpec, "/advanced/equities/corporate-actions/{id}/apply");
    },
    onSuccess: () => {
      // Every holder's position may have changed.
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export interface RunCommand {
  /** Undefined for the backend's default. */
  readonly limit: number | undefined;
  readonly idempotencyKey: string;
}

export function useRunSettlements(): UseMutationResult<EquitySettlementRun, unknown, RunCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (c: RunCommand) => {
      const { data } = await api.POST("/advanced/equities/settlement-runs", {
        ...idempotent(c.idempotencyKey),
        body: c.limit === undefined ? {} : { limit: c.limit },
      });
      const run = validated<EquitySettlementRun>(data, equitySettlementRunSpec, "/advanced/equities/settlement-runs");
      run.settled.forEach((s, index) => checkSettlement(s, `/advanced/equities/settlement-runs.settled[${String(index)}]`));
      return run;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export function useRunVenue(): UseMutationResult<EquityVenueRun, unknown, RunCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (c: RunCommand) => {
      const { data } = await api.POST("/advanced/equities/venue-runs", {
        ...idempotent(c.idempotencyKey),
        body: c.limit === undefined ? {} : { limit: c.limit },
      });
      return validated<EquityVenueRun>(data, equityVenueRunSpec, "/advanced/equities/venue-runs");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}
