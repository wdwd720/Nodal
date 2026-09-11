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
import { idempotent, newIdempotencyKey } from "@controlplane/generated-client";

import { api } from "./client.ts";
import { isUnauthenticated } from "./problem.ts";
import {
  ContractViolation,
  accountSpec,
  legalDocumentSpec,
  termsStateSpec,
  unreadCountSpec,
  userProfileSpec,
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
  creditBalanceSpec,
  internalOrderSpec,
  internalProductSpec,
  nativeAssetSpec,
  nativeFillSpec,
  nativeMarketSpec,
  nativeQuoteSpec,
  payoutRequestSpec,
  itemsSpec,
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
    // A 401 is an answer and must not be retried: the backend has said this
    // session is not signed in, and asking again cannot change that. Anything
    // else is a failure to ASK, which is worth one more attempt before the
    // application concludes anything about the customer's session at all.
    retry: (failureCount, error) => !isUnauthenticated(error) && failureCount < 2,
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
  /**
   * True when this deployment is a sandbox tier (ADR-0023).
   *
   * It is the API's own answer rather than something the client infers from
   * the environment name, which is the point: a build that guessed would label
   * the wrong deployment, and the one thing worse than an unlabelled rehearsal
   * is a real deployment labelled as one.
   *
   * Absent on a deployment that predates the field, and absence is NOT
   * "sandbox". A missing flag means the API did not say, and inventing a
   * sandbox label for a tier that never claimed one would be its own lie.
   */
  readonly sandbox_tier?: boolean;
}

export function useVersion(): UseQueryResult<VersionInfo> {
  return useQuery({
    queryKey: keys.version,
    staleTime: Infinity,
    queryFn: async () => {
      const { data } = await api.GET("/version", {});
      return validated<VersionInfo>(
        data,
        {
          required: { build_version: "string", config_hash: "string", environment: "string" },
          optional: { sandbox_tier: "boolean" },
        },
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

/* --------------------------------------------------------------------------
 * The Nodal-native economy (gola.md PARTS XII-XXI)
 *
 * Note what is absent: there is no hook that adds a Credit figure to a USD
 * figure, and there is no hook that converts one to the other. The separation
 * PART LII requires is not a layout choice made in a component — the data
 * layer never produces the combined number, so no component can render it by
 * accident.
 * ------------------------------------------------------------------------ */

export type CreditBalance = Schemas["CreditBalance"];
export type NativeAsset = Schemas["NativeAsset"];
export type NativeMarket = Schemas["NativeMarket"];
export type NativeQuote = Schemas["NativeQuote"];
export type NativeFill = Schemas["NativeFill"];
export type InternalProduct = Schemas["InternalProduct"];
export type InternalSeller = Schemas["InternalSeller"];
export type InternalOrder = Schemas["InternalOrder"];
export type PayoutRequest = Schemas["PayoutRequest"];
export type InternalProductKind = Schemas["InternalProductKind"];

export const nodalKeys = {
  credits: (accountId: string) => ["credits", accountId] as const,
  nativeAssets: ["native-assets"] as const,
  nativeAsset: (id: string) => ["native-asset", id] as const,
  nativeMarket: (id: string) => ["native-market", id] as const,
  products: (kind: string) => ["internal-products", kind] as const,
  product: (id: string) => ["internal-product", id] as const,
  orders: (accountId: string, role: string) => ["internal-orders", accountId, role] as const,
  payouts: (accountId: string) => ["payouts", accountId] as const,
};

export function useCreditBalance(accountId: string | undefined): UseQueryResult<CreditBalance> {
  return useQuery({
    queryKey: nodalKeys.credits(accountId ?? ""),
    enabled: accountId !== undefined,
    // Never cached as truth: what may be paid out depends on a policy the
    // deployment can change, and a stale breakdown would misstate it.
    staleTime: 0,
    queryFn: async () => {
      const { data } = await api.GET("/credits/balance", {
        params: { query: { account_id: accountId ?? "" } },
      });
      return validated<CreditBalance>(data, creditBalanceSpec, "/credits/balance");
    },
  });
}

export function useNativeAssets(limit = 50): UseQueryResult<NativeAsset[]> {
  return useQuery({
    queryKey: nodalKeys.nativeAssets,
    queryFn: async () => {
      const { data } = await api.GET("/native-assets", { params: { query: { limit } } });
      return validated<{ items: NativeAsset[] }>(
        data, itemsSpec(nativeAssetSpec), "/native-assets",
      ).items;
    },
  });
}

export interface CreateNativeAssetRequest {
  readonly accountId: string;
  readonly name: string;
  readonly symbol: string;
  readonly description: string;
  readonly maxSupply: string;
  readonly creatorAllocation: string;
  readonly decimals: number;
  readonly idempotencyKey: string;
}

/**
 * Creating an asset publishes something with the creator's name on it and
 * fixes its economics permanently, so the key is created once when they
 * confirm — a retry of the same confirmation must never make a second asset.
 */
export function useCreateNativeAsset(): UseMutationResult<NativeAsset, unknown, CreateNativeAssetRequest> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (r: CreateNativeAssetRequest) => {
      const { data } = await api.POST("/native-assets", {
        ...idempotent(r.idempotencyKey),
        body: {
          account_id: r.accountId,
          name: r.name,
          symbol: r.symbol,
          description: r.description,
          max_supply: r.maxSupply,
          creator_allocation: r.creatorAllocation,
          decimals: r.decimals,
        },
      });
      return validated<NativeAsset>(data, nativeAssetSpec, "/native-assets");
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: nodalKeys.nativeAssets });
    },
  });
}

export function useNativeAsset(assetId: string | undefined): UseQueryResult<NativeAsset> {
  return useQuery({
    queryKey: nodalKeys.nativeAsset(assetId ?? ""),
    enabled: assetId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/native-assets/{assetId}", {
        params: { path: { assetId: assetId ?? "" } },
      });
      return validated<NativeAsset>(data, nativeAssetSpec, "/native-assets/{id}");
    },
  });
}

export function useNativeMarket(marketId: string | undefined): UseQueryResult<NativeMarket> {
  return useQuery({
    queryKey: nodalKeys.nativeMarket(marketId ?? ""),
    enabled: marketId !== undefined,
    // The state version moves on every trade, and a quote priced against a
    // stale one is refused by the backend. Refetching is cheaper than
    // explaining a rejection.
    staleTime: 0,
    queryFn: async () => {
      const { data } = await api.GET("/native-markets/{marketId}", {
        params: { path: { marketId: marketId ?? "" } },
      });
      return validated<NativeMarket>(data, nativeMarketSpec, "/native-markets/{id}");
    },
  });
}

export interface NativeQuoteRequest {
  readonly marketId: string;
  readonly accountId: string;
  readonly side: "BUY" | "SELL";
  readonly amount: string;
}

/**
 * A native-market quote is a record of what the market said at a version. It
 * is a mutation because the backend persists it, and because a price should
 * not appear because a component remounted.
 */
export function useNativeQuote(): UseMutationResult<NativeQuote, unknown, NativeQuoteRequest> {
  return useMutation({
    mutationFn: async (r: NativeQuoteRequest) => {
      const { data } = await api.POST("/native-markets/{marketId}/quotes", {
        ...idempotent(newIdempotencyKey(), { path: { marketId: r.marketId } }),
        body: { account_id: r.accountId, side: r.side, amount: r.amount },
      });
      return validated<NativeQuote>(data, nativeQuoteSpec, "/native-markets/{id}/quotes");
    },
  });
}

export interface NativeOrderRequest extends NativeQuoteRequest {
  /** The number the customer actually agreed to. Never taken from a quote. */
  readonly minOutput: string;
  readonly quoteId?: string;
  readonly idempotencyKey: string;
}

export function useNativeOrder(): UseMutationResult<NativeFill, unknown, NativeOrderRequest> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (r: NativeOrderRequest) => {
      const { data } = await api.POST("/native-markets/{marketId}/orders", {
        ...idempotent(r.idempotencyKey, { path: { marketId: r.marketId } }),
        body: {
          account_id: r.accountId,
          side: r.side,
          amount: r.amount,
          min_output: r.minOutput,
          ...(r.quoteId !== undefined ? { quote_id: r.quoteId } : {}),
        },
      });
      return validated<NativeFill>(data, nativeFillSpec, "/native-markets/{id}/orders");
    },
    onSuccess: (_fill, r) => {
      void qc.invalidateQueries({ queryKey: nodalKeys.nativeMarket(r.marketId) });
      void qc.invalidateQueries({ queryKey: nodalKeys.credits(r.accountId) });
    },
  });
}

export function useInternalProducts(kind: string): UseQueryResult<InternalProduct[]> {
  return useQuery({
    queryKey: nodalKeys.products(kind),
    queryFn: async () => {
      const { data } = await api.GET("/internal-products", {
        params: { query: kind === "" ? {} : { kind: kind as InternalProductKind } },
      });
      return validated<{ items: InternalProduct[] }>(
        data, itemsSpec(internalProductSpec), "/internal-products",
      ).items;
    },
  });
}

export function useInternalProduct(productId: string | undefined): UseQueryResult<InternalProduct> {
  return useQuery({
    queryKey: nodalKeys.product(productId ?? ""),
    enabled: productId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/internal-products/{productId}", {
        params: { path: { productId: productId ?? "" } },
      });
      return validated<InternalProduct>(data, internalProductSpec, "/internal-products/{id}");
    },
  });
}

export function useInternalOrders(
  accountId: string | undefined,
  role: "BUYER" | "SELLER",
): UseQueryResult<InternalOrder[]> {
  return useQuery({
    queryKey: nodalKeys.orders(accountId ?? "", role),
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/internal-orders", {
        params: { query: { account_id: accountId ?? "", role } },
      });
      return validated<{ items: InternalOrder[] }>(
        data, itemsSpec(internalOrderSpec), "/internal-orders",
      ).items;
    },
  });
}

export interface PurchaseRequest {
  readonly productId: string;
  readonly accountId: string;
  /** What the customer was shown. A mismatch is a refusal, not a surprise charge. */
  readonly expectedPrice: string;
  readonly idempotencyKey: string;
}

export function usePurchaseProduct(): UseMutationResult<InternalOrder, unknown, PurchaseRequest> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (r: PurchaseRequest) => {
      const { data } = await api.POST("/internal-products/{productId}/orders", {
        ...idempotent(r.idempotencyKey, { path: { productId: r.productId } }),
        body: { account_id: r.accountId, expected_price: r.expectedPrice },
      });
      return validated<InternalOrder>(data, internalOrderSpec, "/internal-products/{id}/orders");
    },
    onSuccess: (_order, r) => {
      void qc.invalidateQueries({ queryKey: nodalKeys.credits(r.accountId) });
      void qc.invalidateQueries({ queryKey: nodalKeys.orders(r.accountId, "BUYER") });
    },
  });
}

export function usePayouts(accountId: string | undefined): UseQueryResult<PayoutRequest[]> {
  return useQuery({
    queryKey: nodalKeys.payouts(accountId ?? ""),
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/payouts", {
        params: { query: { account_id: accountId ?? "" } },
      });
      return validated<{ items: PayoutRequest[] }>(
        data, itemsSpec(payoutRequestSpec), "/payouts",
      ).items;
    },
  });
}

export interface CreatePayoutRequest {
  readonly accountId: string;
  readonly amount: string;
  readonly destinationId?: string;
  readonly idempotencyKey: string;
}

export function useCreatePayout(): UseMutationResult<PayoutRequest, unknown, CreatePayoutRequest> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (r: CreatePayoutRequest) => {
      const { data } = await api.POST("/payouts", {
        ...idempotent(r.idempotencyKey),
        body: {
          account_id: r.accountId,
          amount: r.amount,
          ...(r.destinationId !== undefined ? { destination_id: r.destinationId } : {}),
        },
      });
      return validated<PayoutRequest>(data, payoutRequestSpec, "/payouts");
    },
    onSuccess: (_req, r) => {
      void qc.invalidateQueries({ queryKey: nodalKeys.payouts(r.accountId) });
      void qc.invalidateQueries({ queryKey: nodalKeys.credits(r.accountId) });
    },
  });
}

/* --------------------------------------------------------------------------
 * Profile, onboarding, legal documents and notifications (D-077 phase two)
 *
 * Note what is NOT here: no hook returns a notification's figures, because a
 * notification carries identifiers and state names and never a balance. The
 * canonical figure is always the REST read, which is the rule the stream and
 * the notification centre are both built on.
 * ------------------------------------------------------------------------ */

export type UserProfile = Schemas["UserProfile"];
export type Onboarding = Schemas["Onboarding"];
export type LegalDocument = Schemas["LegalDocument"];
export type TermsState = Schemas["TermsState"];
export type TermsDocumentId = NonNullable<Schemas["TermsAcceptanceRequest"]["document_ids"]>[number];

export const meKeys = {
  terms: ["me", "terms"] as const,
  unread: ["me", "notifications", "unread"] as const,
  notifications: ["me", "notifications"] as const,
};

/**
 * The legal documents, their versions, and which of them this caller has
 * accepted at the bytes currently served.
 *
 * `outstanding` is the server's answer, not a client-side comparison: whether a
 * document counts as accepted depends on the sha256 of the bytes it was
 * accepted at, which only the server knows. A client that recomputed it from
 * version strings would call a re-issued document accepted.
 */
export function useTermsState(enabled = true): UseQueryResult<TermsState> {
  return useQuery({
    queryKey: meKeys.terms,
    enabled,
    // Which documents are outstanding changes the moment one is accepted, and
    // an acceptance gate reading a stale answer would either re-ask or let
    // somebody past. It is never served from cache.
    staleTime: 0,
    queryFn: async () => {
      const { data } = await api.GET("/me/terms-acceptances", {});
      const state = validated<TermsState>(data, termsStateSpec, "/me/terms-acceptances");
      if (!Array.isArray(state.outstanding)) {
        throw new ContractViolation("/me/terms-acceptances.outstanding", "expected an array");
      }
      for (const id of state.outstanding) {
        if (typeof id !== "string") {
          throw new ContractViolation("/me/terms-acceptances.outstanding[]", "expected a string");
        }
      }
      return state;
    },
  });
}

export interface AcceptTermsInput {
  readonly documentIds: readonly TermsDocumentId[];
  readonly idempotencyKey: string;
}

/**
 * Records an acceptance of the documents by id.
 *
 * The server records the version and the sha256 of the bytes it served, so the
 * screen that calls this must have rendered those same bytes. The key is minted
 * at the moment of confirmation, so a retry after a re-authentication records
 * one acceptance and not two.
 */
export function useAcceptTerms(): UseMutationResult<TermsState, unknown, AcceptTermsInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: AcceptTermsInput) => {
      const { data } = await api.POST("/me/terms-acceptances", {
        ...idempotent(input.idempotencyKey),
        body: { document_ids: [...input.documentIds] },
      });
      return validated<TermsState>(data, termsStateSpec, "/me/terms-acceptances");
    },
    onSuccess: () => {
      // `/me` carries the onboarding timestamps, and accepting the last
      // outstanding document is what completes that step.
      void qc.invalidateQueries({ queryKey: keys.me });
      void qc.invalidateQueries({ queryKey: meKeys.terms });
    },
  });
}

export interface ProfileUpdateInput {
  readonly displayName?: string;
  readonly handle?: string;
  readonly locale?: string;
  readonly timeZone?: string;
  readonly idempotencyKey: string;
}

/**
 * Creates or updates the caller's own profile. Only the fields present change;
 * an empty handle clears it, which is why `handle: ""` is sent rather than
 * omitted when somebody removes theirs.
 */
export function useUpdateProfile(): UseMutationResult<UserProfile, unknown, ProfileUpdateInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: ProfileUpdateInput) => {
      const { data } = await api.POST("/me/profile", {
        ...idempotent(input.idempotencyKey),
        body: {
          ...(input.displayName === undefined ? {} : { display_name: input.displayName }),
          ...(input.handle === undefined ? {} : { handle: input.handle }),
          ...(input.locale === undefined ? {} : { locale: input.locale }),
          ...(input.timeZone === undefined ? {} : { time_zone: input.timeZone }),
        },
      });
      return validated<UserProfile>(data, userProfileSpec, "/me/profile");
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.me });
    },
  });
}

/**
 * How many notifications the caller has not read.
 *
 * A count, never a figure. The bell is allowed to be a number because a count
 * of messages is not money; everything the notifications themselves describe is
 * refetched from its own resource before it is shown as a balance.
 */
export function useUnreadCount(enabled: boolean): UseQueryResult<number> {
  return useQuery({
    queryKey: meKeys.unread,
    enabled,
    queryFn: async () => {
      const { data } = await api.GET("/me/notifications/unread-count", {});
      const parsed = validated<{ count: number }>(data, unreadCountSpec, "/me/notifications/unread-count");
      return parsed.count;
    },
  });
}

/** Re-exported so a page can validate a document it received on its own. */
export { legalDocumentSpec };
