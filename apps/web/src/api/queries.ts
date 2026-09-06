/**
 * Every read and every command the customer app performs.
 *
 * Rules that hold throughout:
 *
 *   - the server is authoritative; nothing here caches a financial figure as
 *     truth, and buying power in particular is refetched rather than derived;
 *   - every response is validated against the contract before it reaches a
 *     component, so a malformed money field becomes an error state instead of a
 *     rendered number;
 *   - every money-affecting command carries an idempotency key created once,
 *     when the user confirms, and reused for every retry of that same action.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import type { Schemas } from "@controlplane/generated-client";
import { idempotent } from "@controlplane/generated-client";

import { api } from "./client.ts";
import {
  accountSpec,
  activityItemSpec,
  assetSpec,
  buyingPowerSpec,
  depositSpec,
  holdingsSpec,
  instrumentDetailSpec,
  instrumentSpec,
  intentSpec,
  journalTransactionSpec,
  orderSpec,
  pageSpec,
  principalSpec,
  quoteDisclosureSpec,
  sessionSpec,
  validated,
  validatedList,
} from "./contract.ts";

export type Principal = Schemas["Principal"];
export type Account = Schemas["Account"];
export type BuyingPower = Schemas["BuyingPower"];
export type HoldingsResponse = Schemas["HoldingsResponse"];
export type Holding = HoldingsResponse["holdings"][number];
export type Asset = Schemas["Asset"];
export type Instrument = Schemas["Instrument"];
export type InstrumentDetail = Schemas["InstrumentDetail"];
export type QuoteDisclosure = Schemas["QuoteDisclosure"];
export type TradeIntent = Schemas["TradeIntent"];
export type TradeIntentDetail = Schemas["TradeIntentDetail"];
export type Order = Schemas["Order"];
export type OrderDetail = Schemas["OrderDetail"];
export type Deposit = Schemas["Deposit"];
export type ActivityItem = Schemas["ActivityItem"];
export type JournalTransaction = Schemas["JournalTransaction"];
export type SessionSummary = Schemas["SessionSummary"];
export type IntentAction = Schemas["IntentAction"];

/** Query keys, namespaced so an account switch cannot show another account's rows. */
export const keys = {
  me: ["me"] as const,
  accounts: ["accounts"] as const,
  buyingPower: (accountId: string, purpose: string) => ["buying-power", accountId, purpose] as const,
  holdings: (accountId: string) => ["holdings", accountId] as const,
  assets: ["assets"] as const,
  instruments: ["instruments"] as const,
  instrument: (id: string) => ["instrument", id] as const,
  intents: (accountId: string) => ["intents", accountId] as const,
  intent: (id: string) => ["intent", id] as const,
  orders: (accountId: string) => ["orders", accountId] as const,
  deposits: (accountId: string) => ["deposits", accountId] as const,
  activity: (accountId: string) => ["activity", accountId] as const,
  ledger: (accountId: string) => ["ledger", accountId] as const,
  sessions: ["sessions"] as const,
  version: ["version"] as const,
};

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

export function useMe(): UseQueryResult<Principal> {
  return useQuery({
    queryKey: keys.me,
    retry: false,
    queryFn: async () => {
      const { data } = await api.GET("/me", {});
      return validated<Principal>(data, principalSpec, "/me");
    },
  });
}

export function useAccounts(enabled: boolean): UseQueryResult<Account[]> {
  return useQuery({
    queryKey: keys.accounts,
    enabled,
    queryFn: async () => {
      const { data } = await api.GET("/accounts", {});
      return validatedList<Account>(data, accountSpec, "/accounts");
    },
  });
}

export type BuyingPowerPurpose = "DISPLAY" | "TRADE" | "WITHDRAWAL";

export function useBuyingPower(
  accountId: string | undefined,
  purpose: BuyingPowerPurpose,
): UseQueryResult<BuyingPower> {
  return useQuery({
    queryKey: keys.buyingPower(accountId ?? "", purpose),
    enabled: accountId !== undefined,
    // Buying power is computed by the backend on every call and is never
    // treated as cached truth (PART 25); a stale figure is refetched, not shown.
    staleTime: 0,
    refetchOnWindowFocus: true,
    queryFn: async () => {
      const { data } = await api.GET("/accounts/{accountId}/buying-power", {
        params: { path: { accountId: accountId ?? "" }, query: { purpose } },
      });
      return validated<BuyingPower>(data, buyingPowerSpec, "/accounts/{id}/buying-power");
    },
  });
}

export function useHoldings(accountId: string | undefined): UseQueryResult<HoldingsResponse> {
  return useQuery({
    queryKey: keys.holdings(accountId ?? ""),
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/accounts/{accountId}/holdings", {
        params: { path: { accountId: accountId ?? "" } },
      });
      return validated<HoldingsResponse>(data, holdingsSpec, "/accounts/{id}/holdings");
    },
  });
}

export function useAssets(enabled = true): UseQueryResult<Asset[]> {
  return useQuery({
    queryKey: keys.assets,
    enabled,
    queryFn: async () => {
      const { data } = await api.GET("/assets", {});
      return validatedList<Asset>(data, assetSpec, "/assets");
    },
  });
}

export function useInstruments(enabled = true): UseQueryResult<Instrument[]> {
  return useQuery({
    queryKey: keys.instruments,
    enabled,
    queryFn: async () => {
      const { data } = await api.GET("/instruments", {});
      return validatedList<Instrument>(data, instrumentSpec, "/instruments");
    },
  });
}

export function useInstrument(id: string | undefined): UseQueryResult<InstrumentDetail> {
  return useQuery({
    queryKey: keys.instrument(id ?? ""),
    enabled: id !== undefined && id !== "",
    queryFn: async () => {
      const { data } = await api.GET("/instruments/{instrumentId}", {
        params: { path: { instrumentId: id ?? "" } },
      });
      return validated<InstrumentDetail>(data, instrumentDetailSpec, "/instruments/{id}");
    },
  });
}

export function useIntents(
  accountId: string | undefined,
  options: { readonly refetchMs?: number } = {},
): UseQueryResult<TradeIntent[]> {
  return useQuery({
    queryKey: keys.intents(accountId ?? ""),
    enabled: accountId !== undefined,
    ...(options.refetchMs === undefined ? {} : { refetchInterval: options.refetchMs }),
    queryFn: async () => {
      const { data } = await api.GET("/intents", {
        params: { query: { account_id: accountId ?? "", limit: 100 } },
      });
      const page = validated<Schemas["TradeIntentPage"]>(
        data,
        pageSpec(intentSpec),
        "/intents",
      );
      return page.items;
    },
  });
}

export function useIntent(
  id: string | undefined,
  options: { readonly refetchMs?: number } = {},
): UseQueryResult<TradeIntentDetail> {
  return useQuery({
    queryKey: keys.intent(id ?? ""),
    enabled: id !== undefined && id !== "",
    ...(options.refetchMs === undefined ? {} : { refetchInterval: options.refetchMs }),
    queryFn: async () => {
      const { data } = await api.GET("/intents/{intentId}", {
        params: { path: { intentId: id ?? "" } },
      });
      return validated<TradeIntentDetail>(data, intentSpec, "/intents/{id}");
    },
  });
}

export function useOrders(accountId: string | undefined): UseQueryResult<Order[]> {
  return useQuery({
    queryKey: keys.orders(accountId ?? ""),
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/orders", {
        params: { query: { account_id: accountId ?? "", limit: 100 } },
      });
      const page = validated<Schemas["OrderPage"]>(data, pageSpec(orderSpec), "/orders");
      return page.items;
    },
  });
}

export function useDeposits(accountId: string | undefined): UseQueryResult<Deposit[]> {
  return useQuery({
    queryKey: keys.deposits(accountId ?? ""),
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/funding/deposits", {
        params: { query: { account_id: accountId ?? "", limit: 100 } },
      });
      const page = validated<Schemas["DepositPage"]>(data, pageSpec(depositSpec), "/funding/deposits");
      return page.items;
    },
  });
}

export interface ActivityPage {
  readonly items: ActivityItem[];
  readonly nextCursor: string | null;
}

export function useActivity(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<ActivityPage> {
  return useQuery({
    queryKey: [...keys.activity(accountId ?? ""), cursor ?? ""],
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/accounts/{accountId}/activity", {
        params: {
          path: { accountId: accountId ?? "" },
          query: cursor === undefined ? { limit: 50 } : { limit: 50, cursor },
        },
      });
      const page = validated<Schemas["ActivityPage"]>(
        data,
        pageSpec(activityItemSpec),
        "/accounts/{id}/activity",
      );
      return { items: page.items, nextCursor: page.next_cursor ?? null };
    },
  });
}

export function useLedger(accountId: string | undefined): UseQueryResult<JournalTransaction[]> {
  return useQuery({
    queryKey: keys.ledger(accountId ?? ""),
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/accounts/{accountId}/ledger/transactions", {
        params: { path: { accountId: accountId ?? "" }, query: { limit: 50 } },
      });
      const page = validated<Schemas["JournalTransactionPage"]>(
        data,
        pageSpec(journalTransactionSpec),
        "/accounts/{id}/ledger/transactions",
      );
      return page.items;
    },
  });
}

export function useSessions(enabled: boolean): UseQueryResult<SessionSummary[]> {
  return useQuery({
    queryKey: keys.sessions,
    enabled,
    queryFn: async () => {
      const { data } = await api.GET("/sessions", {});
      return validatedList<SessionSummary>(data, sessionSpec, "/sessions");
    },
  });
}

export interface VersionInfo {
  readonly build_version: string;
  readonly config_hash: string;
  readonly environment: string;
}

export function useVersion(): UseQueryResult<VersionInfo> {
  return useQuery({
    queryKey: keys.version,
    staleTime: Infinity,
    queryFn: async () => {
      const { data } = await api.GET("/version", {});
      return validated<VersionInfo>(
        data,
        { required: { build_version: "string", config_hash: "string", environment: "string" } },
        "/version",
      );
    },
  });
}

/* --------------------------------------------------------------------------
 * Commands
 * ------------------------------------------------------------------------ */

export interface QuoteRequest {
  readonly accountId: string;
  readonly instrumentId: string;
  readonly action: IntentAction;
  readonly notionalUsd: string;
  readonly maxSlippageBps: number;
}

/**
 * A quote preview moves no money and reserves nothing, so it carries no
 * idempotency key. It is a mutation rather than a query because the customer
 * asks for it explicitly: a price should not appear because a component
 * remounted.
 */
export function useQuotePreview(): UseMutationResult<QuoteDisclosure, unknown, QuoteRequest> {
  return useMutation({
    mutationFn: async (request: QuoteRequest) => {
      const { data } = await api.POST("/quotes/preview", {
        body: {
          account_id: request.accountId,
          instrument_id: request.instrumentId,
          action: request.action,
          notional_usd: request.notionalUsd,
          constraints: { max_slippage_bps: request.maxSlippageBps },
        },
      });
      return validated<QuoteDisclosure>(data, quoteDisclosureSpec, "/quotes/preview");
    },
  });
}

export interface SubmitIntentInput {
  readonly accountId: string;
  readonly instrumentId: string;
  readonly action: IntentAction;
  readonly notionalUsd: string;
  readonly maxSlippageBps: number;
  readonly mode: "PAPER" | "LIVE";
  readonly quoteId?: string;
  /** Created once when the user confirmed, reused for every retry of this action. */
  readonly idempotencyKey: string;
}

export function useSubmitIntent(): UseMutationResult<TradeIntent, unknown, SubmitIntentInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: SubmitIntentInput) => {
      const { data } = await api.POST("/intents", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          instrument_id: input.instrumentId,
          action: input.action,
          notional_usd: input.notionalUsd,
          mode: input.mode,
          constraints: { max_slippage_bps: input.maxSlippageBps },
          ...(input.quoteId === undefined ? {} : { quote_id: input.quoteId }),
        },
      });
      return validated<TradeIntent>(data, intentSpec, "/intents");
    },
    onSuccess: (_intent, input) => {
      void queryClient.invalidateQueries({ queryKey: keys.intents(input.accountId) });
      void queryClient.invalidateQueries({ queryKey: keys.orders(input.accountId) });
      void queryClient.invalidateQueries({ queryKey: ["buying-power", input.accountId] });
      void queryClient.invalidateQueries({ queryKey: keys.activity(input.accountId) });
    },
  });
}

export interface CancelIntentInput {
  readonly intentId: string;
  readonly accountId: string;
  readonly idempotencyKey: string;
}

export function useCancelIntent(): UseMutationResult<TradeIntent, unknown, CancelIntentInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: CancelIntentInput) => {
      const { data } = await api.POST("/intents/{intentId}/cancel", {
        ...idempotent(input.idempotencyKey, { path: { intentId: input.intentId } }),
      });
      return validated<TradeIntent>(data, intentSpec, "/intents/{id}/cancel");
    },
    onSuccess: (_result, input) => {
      void queryClient.invalidateQueries({ queryKey: keys.intent(input.intentId) });
      void queryClient.invalidateQueries({ queryKey: keys.intents(input.accountId) });
    },
  });
}

export interface StartDepositInput {
  readonly accountId: string;
  readonly fiatAmount: string;
  readonly idempotencyKey: string;
}

export function useStartDeposit(): UseMutationResult<Deposit, unknown, StartDepositInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: StartDepositInput) => {
      const { data } = await api.POST("/funding/deposits", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          fiat_amount: input.fiatAmount,
          fiat_currency: "USD",
        },
      });
      return validated<Deposit>(data, depositSpec, "/funding/deposits");
    },
    onSuccess: (_deposit, input) => {
      void queryClient.invalidateQueries({ queryKey: keys.deposits(input.accountId) });
      void queryClient.invalidateQueries({ queryKey: ["buying-power", input.accountId] });
    },
  });
}

export interface WithdrawalInput {
  readonly accountId: string;
  readonly assetId: string;
  readonly quantity: string;
  readonly destinationAddress: string;
  readonly idempotencyKey: string;
}

export function useRequestWithdrawal(): UseMutationResult<
  Schemas["Withdrawal"],
  unknown,
  WithdrawalInput
> {
  return useMutation({
    mutationFn: async (input: WithdrawalInput) => {
      const { data } = await api.POST("/withdrawals", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          asset_id: input.assetId,
          quantity: input.quantity,
          destination_address: input.destinationAddress,
        },
      });
      return validated<Schemas["Withdrawal"]>(
        data,
        {
          required: {
            id: "uuid",
            account_id: "uuid",
            asset_id: "uuid",
            quantity: "quantity",
            destination_address: "string",
            status: "string",
            created_at: "timestamp",
          },
        },
        "/withdrawals",
      );
    },
  });
}

export function useRevokeSession(): UseMutationResult<void, unknown, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (sessionId: string) => {
      await api.DELETE("/sessions/{sessionId}", { params: { path: { sessionId } } });
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: keys.sessions });
    },
  });
}

export function useSignOut(): UseMutationResult<void, unknown, void> {
  return useMutation({
    mutationFn: async () => {
      await api.POST("/auth/logout", {});
    },
  });
}
