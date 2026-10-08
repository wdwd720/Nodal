/**
 * Reads and commands of the order-book section (internal/orders, ADR-0020):
 * the internal books with their disclosed terms, fee schedules and crossing
 * rule, a book's depth, the account's orders and its side of every trade, the
 * audit proof of a trade, the customer's submit / cancel / replace, and the
 * operator's open / status / journal verification.
 *
 * House orders and the crossing rule are not here: they reach a book only
 * through the treasury service, which enforces the house book's limits; the
 * book read discloses the crossing rule in force.
 *
 * Like core.ts, which it follows:
 *
 *   - every response is checked against a spec before a component sees it,
 *     including the rules a per-field spec cannot state (a book's fee
 *     schedules are quoted in its own quote asset; a command's fills are the
 *     order's own);
 *   - query keys start with `advancedKeys.all`, and account-scoped ones with
 *     `advancedKeys.account(accountId)`, so a command invalidates everything
 *     it may have changed for the account with one key;
 *   - a 4xx answer is the backend's decision and is not retried;
 *   - every command carries the idempotency key its form created once, when
 *     the person confirmed, and reuses it — with the same request — for every
 *     retry.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  BOOK_LIQUIDITY,
  BOOK_LIQUIDITY_INTENTS,
  BOOK_MODES,
  BOOK_ORDER_STATUSES,
  BOOK_ORDER_TYPES,
  BOOK_OWNER_TYPES,
  BOOK_SIDES,
  BOOK_STATUSES,
  BOOK_STP_MODES,
  BOOK_TERMS_SOURCES,
  BOOK_TIMES_IN_FORCE,
  BOOK_VENUE_LABELS,
  parseSequence,
  type BookMode,
  type BookOrderScope,
  type BookOrderType,
  type BookSide,
  type BookStatus,
  type BookStp,
  type BookTimeInForce,
} from "../../lib/orders.ts";
import { api } from "../client.ts";
import { ContractViolation, cursorPageSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient, type CursorPage } from "./core.ts";

export type Book = Schemas["AdvancedBook"];
export type BookFeeSchedule = Schemas["AdvancedBookFeeSchedule"];
export type BookCrossingRule = Schemas["AdvancedBookCrossingRule"];
export type BookDepth = Schemas["AdvancedBookDepth"];
export type BookDepthLevel = Schemas["AdvancedBookDepthLevel"];
export type BookOrder = Schemas["AdvancedBookOrder"];
export type BookFill = Schemas["AdvancedBookFill"];
export type BookCommandResult = Schemas["AdvancedBookCommandResult"];
export type BookTradeProof = Schemas["AdvancedBookTradeProof"];
export type BookJournalVerification = Schemas["AdvancedBookJournalVerification"];
export type BookFeeScheduleRequest = Schemas["AdvancedBookFeeScheduleRequest"];

/** Rows per page of the order-book lists. */
export const BOOKS_PAGE_SIZE = 25;
/** Price levels per side of the depth ladder. */
export const DEPTH_LEVELS = 20;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const ordersKeys = {
  /** Prefix of every order-book read that is not scoped to an account. */
  all: [...advancedKeys.all, "books"] as const,
  books: (cursor: string) => [...advancedKeys.all, "books", "list", cursor] as const,
  /** Prefix of every read about one book. */
  book: (instrumentId: string) => [...advancedKeys.all, "books", "book", instrumentId] as const,
  depth: (instrumentId: string, levels: number) => [...advancedKeys.all, "books", "book", instrumentId, "depth", levels] as const,
  journal: (instrumentId: string, asked: number) =>
    [...advancedKeys.all, "books", "book", instrumentId, "journal-verification", asked] as const,
  proof: (accountId: string, instrumentId: string, tradeSeq: string) =>
    [...advancedKeys.account(accountId), "books", "proof", instrumentId, tradeSeq] as const,
  orders: (accountId: string, scope: string, cursor: string) =>
    [...advancedKeys.account(accountId), "books", "orders", scope, cursor] as const,
  order: (accountId: string, orderId: string) => [...advancedKeys.account(accountId), "books", "order", orderId] as const,
  trades: (accountId: string, cursor: string) => [...advancedKeys.account(accountId), "books", "trades", cursor] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
const HASH = "string";

export const bookFeeScheduleSpec: Spec = {
  required: {
    version: "string",
    quote_asset_id: "uuid",
    quote_decimals: "integer",
    percentage_fee_bps: "integer",
    strategy_fee_bps: "integer",
    subscription_discount_bps: "integer",
    max_total_fee_bps: "integer",
    hash: HASH,
  },
};

export const bookCrossingRuleSpec: Spec = {
  required: { book_id: "uuid", version: "integer", allow_house_customer_crossing: "boolean", is_default: "boolean" },
  optional: { reason: "string", actor_type: "string", actor_id: "string", created_at: "timestamp" },
};

export const bookSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    instrument_id: "uuid",
    venue_label: oneOf(BOOK_VENUE_LABELS),
    mode: oneOf(BOOK_MODES),
    status: oneOf(BOOK_STATUSES),
    instrument_kind: "string",
    base_asset_id: "uuid",
    base_symbol: "string",
    base_decimals: "integer",
    quote_asset_id: "uuid",
    quote_symbol: "string",
    quote_decimals: "integer",
    price_scale: "integer",
    tick_price: "decimal",
    lot_size: "quantity",
    min_quantity: "quantity",
    max_price: "decimal",
    trading_calendar: "string",
    terms_source: oneOf(BOOK_TERMS_SOURCES),
    snapshot_interval: "integer",
    last_command_seq: "integer",
    last_event_seq: "integer",
    last_trade_seq: "integer",
    state_hash: HASH,
  },
  optional: { terms_version: "integer" },
  objects: {
    maker_fee: { required: true, spec: bookFeeScheduleSpec },
    taker_fee: { required: true, spec: bookFeeScheduleSpec },
    crossing_rule: { required: true, spec: bookCrossingRuleSpec },
  },
};

export const bookPageSpec: Spec = { ...cursorPageSpec(bookSpec), required: { value_domain: SIMULATED } };

export const bookDepthLevelSpec: Spec = {
  required: { price: "decimal", price_ticks: "integer", quantity: "quantity", orders: "integer" },
};

export const bookDepthSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    book_id: "uuid",
    instrument_id: "uuid",
    status: oneOf(BOOK_STATUSES),
    base_decimals: "integer",
    quote_decimals: "integer",
    last_command_seq: "integer",
    state_hash: HASH,
  },
  arrays: {
    bids: { required: true, spec: bookDepthLevelSpec },
    asks: { required: true, spec: bookDepthLevelSpec },
  },
};

export const bookOrderSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    book_id: "uuid",
    instrument_id: "uuid",
    account_id: "uuid",
    venue_label: oneOf(BOOK_VENUE_LABELS),
    mode: oneOf(BOOK_MODES),
    side: oneOf(BOOK_SIDES),
    order_type: oneOf(BOOK_ORDER_TYPES),
    time_in_force: oneOf(BOOK_TIMES_IN_FORCE),
    liquidity_intent: oneOf(BOOK_LIQUIDITY_INTENTS),
    stp: oneOf(BOOK_STP_MODES),
    post_only: "boolean",
    reduce_only: "boolean",
    strategy_originated: "boolean",
    status: oneOf(BOOK_ORDER_STATUSES),
    price: "decimal",
    price_ticks: "integer",
    quantity: "quantity",
    filled_quantity: "quantity",
    remaining_quantity: "quantity",
    cancelled_quantity: "quantity",
    base_decimals: "integer",
    quote_decimals: "integer",
    created_at: "timestamp",
    updated_at: "timestamp",
  },
  optional: {
    base_symbol: "string",
    quote_symbol: "string",
    client_order_id: "string",
    intent_id: "uuid",
    reservation_id: "uuid",
    reject_reason: "string",
    cancel_reason: "string",
    accepted_command_seq: "integer",
    last_command_seq: "integer",
  },
};

export const bookOrderPageSpec: Spec = { ...cursorPageSpec(bookOrderSpec), required: { value_domain: SIMULATED } };

export const bookFillSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    book_id: "uuid",
    instrument_id: "uuid",
    trade_seq: "integer",
    command_seq: "integer",
    order_id: "uuid",
    side: oneOf(BOOK_SIDES),
    liquidity: oneOf(BOOK_LIQUIDITY),
    counterparty_owner_type: oneOf(BOOK_OWNER_TYPES),
    price: "decimal",
    price_ticks: "integer",
    quantity: "quantity",
    quote_notional: "quantity",
    fee: "quantity",
    base_decimals: "integer",
    quote_decimals: "integer",
    crossing_rule_version: "integer",
    journal_transaction_id: "uuid",
    effect_id: "uuid",
    evidence_ref: "string",
    evidence_sha256: HASH,
    executed_at: "timestamp",
  },
  optional: { base_symbol: "string", quote_symbol: "string" },
};

export const bookFillPageSpec: Spec = { ...cursorPageSpec(bookFillSpec), required: { value_domain: SIMULATED } };

export const bookCommandResultSpec: Spec = {
  required: { value_domain: SIMULATED },
  optional: { intent_status: "string", command_seq: "integer", state_hash: HASH },
  objects: { order: { required: true, spec: bookOrderSpec } },
  arrays: { fills: { required: true, spec: bookFillSpec } },
};

export const bookTradeProofSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    book_id: "uuid",
    instrument_id: "uuid",
    venue_label: oneOf(BOOK_VENUE_LABELS),
    trade_seq: "integer",
    command_seq: "integer",
    event_seq: "integer",
    price: "decimal",
    price_ticks: "integer",
    quantity: "quantity",
    quote_notional: "quantity",
    base_decimals: "integer",
    quote_decimals: "integer",
    aggressor_side: oneOf(BOOK_SIDES),
    maker_order_id: "uuid",
    taker_order_id: "uuid",
    maker_owner_type: oneOf(BOOK_OWNER_TYPES),
    taker_owner_type: oneOf(BOOK_OWNER_TYPES),
    maker_fee: "quantity",
    taker_fee: "quantity",
    maker_schedule_hash: HASH,
    taker_schedule_hash: HASH,
    crossing_rule_version: "integer",
    journal_transaction_id: "uuid",
    effect_id: "uuid",
    evidence_ref: "string",
    evidence_sha256: HASH,
    executed_at: "timestamp",
    snapshot_seq: "integer",
    book_last_command_seq: "integer",
    book_state_hash: HASH,
  },
  optional: { base_symbol: "string", quote_symbol: "string", snapshot_hash: HASH },
};

export const bookJournalVerificationSpec: Spec = {
  required: {
    book_id: "uuid",
    instrument_id: "uuid",
    commands: "integer",
    events: "integer",
    trades: "integer",
    orders_checked: "integer",
    snapshot_seq: "integer",
    genesis_hash: HASH,
    full_replay_hash: HASH,
    snapshot_replay_hash: HASH,
    state_hash: HASH,
    verified_at: "timestamp",
  },
};

/* --------------------------------------------------------------------------
 * The rules a per-field spec cannot state
 * ------------------------------------------------------------------------ */

/**
 * A book's two fee schedules are quoted in the book's own quote asset, at its
 * decimals, and its crossing rule is the book's: a book that says otherwise
 * is refused rather than drawn with fees in an asset it does not trade.
 */
export function checkBook(value: unknown, path: string): Book {
  const book = validated<Book>(value, bookSpec, path);
  for (const [name, schedule] of [
    ["maker_fee", book.maker_fee],
    ["taker_fee", book.taker_fee],
  ] as const) {
    if (schedule.quote_asset_id.toLowerCase() !== book.quote_asset_id.toLowerCase()) {
      throw new ContractViolation(`${path}.${name}.quote_asset_id`, "a fee schedule is quoted in the book's quote asset");
    }
    if (schedule.quote_decimals !== book.quote_decimals) {
      throw new ContractViolation(`${path}.${name}.quote_decimals`, "a fee schedule carries the quote asset's decimals");
    }
  }
  if (book.crossing_rule.book_id.toLowerCase() !== book.id.toLowerCase()) {
    throw new ContractViolation(`${path}.crossing_rule.book_id`, "the crossing rule is another book's");
  }
  if (book.crossing_rule.is_default && book.crossing_rule.allow_house_customer_crossing) {
    throw new ContractViolation(`${path}.crossing_rule`, "the default rule forbids house-customer crossing");
  }
  return book;
}

/** A command's fills are the order's own fills. */
export function checkCommandResult(value: unknown, path: string): BookCommandResult {
  const result = validated<BookCommandResult>(value, bookCommandResultSpec, path);
  result.fills.forEach((fill, index) => {
    if (fill.order_id.toLowerCase() !== result.order.id.toLowerCase()) {
      throw new ContractViolation(`${path}.fills[${String(index)}].order_id`, "a fill of another order");
    }
  });
  return result;
}

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

export function useBooks(cursor: string | undefined): UseQueryResult<CursorPage<Book>> {
  return useQuery({
    queryKey: ordersKeys.books(cursor ?? ""),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/books", {
        params: { query: { limit: BOOKS_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) } },
      });
      const page = validated<Schemas["AdvancedBookPage"]>(data, bookPageSpec, "/advanced/books");
      return {
        items: page.items.map((item, index) => checkBook(item, `/advanced/books.items[${String(index)}]`)),
        nextCursor: page.next_cursor,
      };
    },
  });
}

export function useBook(instrumentId: string | undefined): UseQueryResult<Book> {
  return useQuery({
    queryKey: ordersKeys.book(instrumentId ?? ""),
    enabled: instrumentId !== undefined && instrumentId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/books/{instrumentId}", {
        params: { path: { instrumentId: instrumentId ?? "" } },
      });
      return checkBook(data, "/advanced/books/{id}");
    },
  });
}

export function useBookDepth(instrumentId: string | undefined): UseQueryResult<BookDepth> {
  return useQuery({
    queryKey: ordersKeys.depth(instrumentId ?? "", DEPTH_LEVELS),
    enabled: instrumentId !== undefined && instrumentId !== "",
    // The book moves with every order; the ladder is read afresh, never reused as truth.
    staleTime: 0,
    refetchOnWindowFocus: true,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/books/{instrumentId}/depth", {
        params: { path: { instrumentId: instrumentId ?? "" }, query: { levels: DEPTH_LEVELS } },
      });
      return validated<BookDepth>(data, bookDepthSpec, "/advanced/books/{id}/depth");
    },
  });
}

/**
 * A replay of the book's journal, run when the operator asks (`asked` counts
 * the requests, so each one reads afresh). Read-only.
 */
export function useJournalVerification(instrumentId: string, asked: number): UseQueryResult<BookJournalVerification> {
  return useQuery({
    queryKey: ordersKeys.journal(instrumentId, asked),
    enabled: asked > 0,
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/books/{instrumentId}/journal-verification", {
        params: { path: { instrumentId } },
      });
      return validated<BookJournalVerification>(data, bookJournalVerificationSpec, "/advanced/books/{id}/journal-verification");
    },
  });
}

export function useBookOrders(
  accountId: string | undefined,
  scope: BookOrderScope,
  cursor: string | undefined,
): UseQueryResult<CursorPage<BookOrder>> {
  return useQuery({
    queryKey: ordersKeys.orders(accountId ?? "", scope, cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/books/orders", {
        params: {
          query: { account_id: accountId ?? "", scope, limit: BOOKS_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) },
        },
      });
      const page = validated<Schemas["AdvancedBookOrderPage"]>(data, bookOrderPageSpec, "/advanced/books/orders");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/**
 * One order. The key carries the account the area reads; the backend decides
 * from the session whether the order may be read and answers NOT_FOUND when
 * it may not.
 */
export function useBookOrder(accountId: string | undefined, orderId: string | undefined): UseQueryResult<BookOrder> {
  return useQuery({
    queryKey: ordersKeys.order(accountId ?? "", orderId ?? ""),
    enabled: orderId !== undefined && orderId !== "",
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/books/orders/{orderId}", {
        params: { path: { orderId: orderId ?? "" } },
      });
      return validated<BookOrder>(data, bookOrderSpec, "/advanced/books/orders/{id}");
    },
  });
}

export function useBookTrades(accountId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<BookFill>> {
  return useQuery({
    queryKey: ordersKeys.trades(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/books/trades", {
        params: { query: { account_id: accountId ?? "", limit: BOOKS_PAGE_SIZE, ...(cursor === undefined ? {} : { cursor }) } },
      });
      const page = validated<Schemas["AdvancedBookFillPage"]>(data, bookFillPageSpec, "/advanced/books/trades");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/** The audit proof of one trade; the backend answers NOT_FOUND to anyone who was not a party to it. */
export function useTradeProof(
  accountId: string | undefined,
  instrumentId: string | undefined,
  tradeSeq: string | undefined,
): UseQueryResult<BookTradeProof> {
  const seq = parseSequence(tradeSeq);
  return useQuery({
    queryKey: ordersKeys.proof(accountId ?? "", instrumentId ?? "", tradeSeq ?? ""),
    enabled: instrumentId !== undefined && instrumentId !== "" && seq !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/books/{instrumentId}/trades/{tradeSeq}/proof", {
        params: { path: { instrumentId: instrumentId ?? "", tradeSeq: seq ?? 0 } },
      });
      return validated<BookTradeProof>(data, bookTradeProofSpec, "/advanced/books/{id}/trades/{seq}/proof");
    },
  });
}

/* --------------------------------------------------------------------------
 * The customer's commands
 * ------------------------------------------------------------------------ */

export interface SubmitBookOrderCommand {
  readonly accountId: string;
  readonly instrumentId: string;
  readonly side: BookSide;
  readonly orderType: BookOrderType;
  readonly timeInForce: BookTimeInForce;
  /** Exact decimal as typed: the limit, or a market order's protection price. */
  readonly price: string;
  /** Exact base units of the base asset. */
  readonly quantity: string;
  readonly postOnly: boolean;
  readonly reduceOnly: boolean;
  readonly stp: BookStp;
  readonly clientOrderId: string | undefined;
  readonly mode: BookMode;
  /** Created once when the person confirmed, reused for every retry of this action. */
  readonly idempotencyKey: string;
}

export function useSubmitBookOrder(): UseMutationResult<BookCommandResult, unknown, SubmitBookOrderCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (command: SubmitBookOrderCommand) => {
      const { data } = await api.POST("/advanced/books/{instrumentId}/orders", {
        ...idempotent(command.idempotencyKey, { path: { instrumentId: command.instrumentId } }),
        body: {
          account_id: command.accountId,
          side: command.side,
          order_type: command.orderType,
          time_in_force: command.timeInForce,
          price: command.price,
          quantity: command.quantity,
          post_only: command.postOnly,
          reduce_only: command.reduceOnly,
          stp: command.stp,
          mode: command.mode,
          ...(command.clientOrderId === undefined ? {} : { client_order_id: command.clientOrderId }),
        },
      });
      return checkCommandResult(data, "/advanced/books/{id}/orders");
    },
    // A refusal is recorded too (the order and its intent): the account's reads changed either way.
    onSettled: (_result, _error, command) => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(command.accountId) });
      void queryClient.invalidateQueries({ queryKey: ordersKeys.book(command.instrumentId) });
    },
  });
}

export interface CancelBookOrderCommand {
  readonly accountId: string;
  readonly orderId: string;
  readonly instrumentId: string;
  readonly idempotencyKey: string;
}

export function useCancelBookOrder(): UseMutationResult<BookCommandResult, unknown, CancelBookOrderCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (command: CancelBookOrderCommand) => {
      const { data } = await api.POST("/advanced/books/orders/{orderId}/cancel", {
        ...idempotent(command.idempotencyKey, { path: { orderId: command.orderId } }),
        body: { account_id: command.accountId },
      });
      return checkCommandResult(data, "/advanced/books/orders/{id}/cancel");
    },
    onSettled: (_result, _error, command) => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(command.accountId) });
      void queryClient.invalidateQueries({ queryKey: ordersKeys.book(command.instrumentId) });
    },
  });
}

export interface ReplaceBookOrderCommand {
  readonly accountId: string;
  readonly orderId: string;
  readonly instrumentId: string;
  readonly price: string;
  /** The new TOTAL quantity in base units, fills included. */
  readonly quantity: string;
  readonly idempotencyKey: string;
}

export function useReplaceBookOrder(): UseMutationResult<BookCommandResult, unknown, ReplaceBookOrderCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (command: ReplaceBookOrderCommand) => {
      const { data } = await api.POST("/advanced/books/orders/{orderId}/replace", {
        ...idempotent(command.idempotencyKey, { path: { orderId: command.orderId } }),
        body: { account_id: command.accountId, price: command.price, quantity: command.quantity },
      });
      return checkCommandResult(data, "/advanced/books/orders/{id}/replace");
    },
    onSettled: (_result, _error, command) => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.account(command.accountId) });
      void queryClient.invalidateQueries({ queryKey: ordersKeys.book(command.instrumentId) });
    },
  });
}

/* --------------------------------------------------------------------------
 * The operator's commands
 * ------------------------------------------------------------------------ */

export interface OpenBookCommand {
  readonly instrumentId: string;
  readonly mode: BookMode;
  readonly makerFee: BookFeeScheduleRequest;
  readonly takerFee: BookFeeScheduleRequest;
  /** For a SPOT_PAIR only: its terms, as the operator stated them. */
  readonly spotTerms: Record<string, unknown> | undefined;
  readonly snapshotInterval: number | undefined;
  readonly idempotencyKey: string;
}

export function useOpenBook(): UseMutationResult<Book, unknown, OpenBookCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (command: OpenBookCommand) => {
      const { data } = await api.POST("/advanced/books", {
        ...idempotent(command.idempotencyKey),
        body: {
          instrument_id: command.instrumentId,
          mode: command.mode,
          maker_fee: command.makerFee,
          taker_fee: command.takerFee,
          // The terms are the backend's to validate, field by field; the form only checked they are one object.
          ...(command.spotTerms === undefined ? {} : { spot_terms: command.spotTerms as unknown as Schemas["InstrumentTerms"] }),
          ...(command.snapshotInterval === undefined ? {} : { snapshot_interval: command.snapshotInterval }),
        },
      });
      return checkBook(data, "/advanced/books");
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ordersKeys.all });
    },
  });
}

export interface BookStatusCommand {
  readonly instrumentId: string;
  readonly status: BookStatus;
  readonly reason: string;
  readonly idempotencyKey: string;
}

export function useSetBookStatus(): UseMutationResult<Book, unknown, BookStatusCommand> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (command: BookStatusCommand) => {
      const { data } = await api.POST("/advanced/books/{instrumentId}/status", {
        ...idempotent(command.idempotencyKey, { path: { instrumentId: command.instrumentId } }),
        body: { status: command.status, reason: command.reason },
      });
      return checkBook(data, "/advanced/books/{id}/status");
    },
    onSuccess: () => {
      // Every account's orders on the book may now be refused or cancellable differently.
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

