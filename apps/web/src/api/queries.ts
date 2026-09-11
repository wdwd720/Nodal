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
import type { Paths, Schemas } from "@controlplane/generated-client";
import { idempotent, newIdempotencyKey } from "@controlplane/generated-client";

import { api } from "./client.ts";
import { isUnauthenticated } from "./problem.ts";
import {
  ContractViolation,
  accountSpec,
  nativeMarketPageSpec,
  nativeMarketDetailSpec,
  nativeCandlePageSpec,
  nativeTradePageSpec,
  marketSafetyLimitsSpec,
  publicLegalDocumentSpec,
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
  activityFeedItemSpec,
  agentSpec,
  creditPricingSpec,
  creditPurchaseSpec,
  markedReadSpec,
  meAuditEntrySpec,
  myAccountSpec,
  notificationPreferenceSpec,
  notificationSpec,
  nativeMarketSummarySpec,
  portfolioSpec,
  portfolioTotalsSpec,
  securitySummarySpec,
  authorityLevelSpec,
  strategySpec,
  payoutDestinationSpec,
  payoutQuoteSpec,
  verificationSessionSpec,
  validatedAgent,
  validatedCompileResult,
  validatedEligibility,
  validatedPayout,
  validatedStartedVerification,
  validatedStrategy,
  validatedStrategyVersion,
  validatedVerificationProfile,
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
  /**
   * The quote the customer was actually shown, and it is REQUIRED.
   *
   * It is consumed in the same transaction that reserves the value, so one
   * quote funds exactly one payout and an expired or already-used one refuses
   * the request before anything is decided about the money. Sending it is what
   * makes "the number you saw is the number you get" a property of the system
   * rather than a hope about timing -- and without it there is no fee and no
   * minimum to judge the payout against, which is how a payout below the
   * provider's published minimum was reserved and settled with the fee never
   * taken (D-119).
   */
  readonly quoteId: string;
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
          quote_id: r.quoteId,
          ...(r.destinationId !== undefined ? { destination_id: r.destinationId } : {}),
        },
      });
      return validatedPayout<PayoutRequest>(data, "/payouts");
    },
    onSuccess: (_req, r) => {
      void qc.invalidateQueries({ queryKey: nodalKeys.payouts(r.accountId) });
      void qc.invalidateQueries({ queryKey: nodalKeys.credits(r.accountId) });
      void qc.invalidateQueries({ queryKey: withdrawKeys.eligibility(r.accountId) });
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
  /** The prefix. Invalidating it reaches the bell AND every page of the list. */
  notifications: ["me", "notifications"] as const,
  notificationPage: (unread: boolean, cursor: string) =>
    ["me", "notifications", "page", unread, cursor] as const,
  notificationPreferences: ["me", "notification-preferences"] as const,
  pricing: ["credit-pricing"] as const,
  purchase: (id: string) => ["credit-purchase", id] as const,
  agents: (accountId: string) => ["agents", accountId] as const,
  myAccount: ["me", "account"] as const,
  security: ["me", "security"] as const,
  audit: (cursor: string) => ["me", "audit", cursor] as const,
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
    // `/me` carries the onboarding timestamps, and accepting the last
    // outstanding document is what completes that step. The refetch is AWAITED:
    // the mutation stays pending until `/me` says onboarding is complete, so a
    // page that navigates on success navigates to a gate that already knows.
    // Fired and forgotten, the refetch raced the customer's next click, and on
    // a slow connection the dashboard gate read the stale answer and sent them
    // back to a terms page with nothing left to accept (F-222).
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({ queryKey: keys.me }),
        qc.invalidateQueries({ queryKey: meKeys.terms }),
      ]).then(() => undefined),
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
    // Awaited for the same reason as the terms acceptance (F-222): the profile
    // step's completion lives on `/me`, and the next screen is gated on it.
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.me }),
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

/* --------------------------------------------------------------------------
 * Buying Credits, the notification centre, and the account's own standing
 * (USER_JOURNEY §3, §4, §9).
 *
 * The shape of the purchase hooks is the interesting part. `POST /v1/payments`
 * answers 201 with a `client_secret` on creation and 200 WITHOUT one on an
 * idempotent replay, and the secret is never returned by a read. So the secret
 * exists in exactly one place for exactly as long as the mutation result lives,
 * and nothing here writes it to a query cache: a cached provider secret is a
 * second browser resuming somebody else's payment form.
 *
 * The state of a purchase, by contrast, is a read that must be allowed to go
 * stale and be asked again — the browser learns that a payment captured from
 * `GET /v1/payments/{id}` and from the event stream, never from the redirect
 * the provider sent the customer back with.
 * ------------------------------------------------------------------------ */

export type CreditPricing = Schemas["CreditPricing"];
/**
 * The purchase, plus the sandbox flag.
 *
 * The flag is declared here as well as in the generated types because the API
 * gained it after this page was written, and a page that renders a sandbox
 * label must compile against an API that has not been regenerated yet. The
 * intersection is a no-op once the generated type carries it. It stays
 * optional: an absent flag means the deployment did not say, which labels
 * nothing and never quietly means "live".
 */
export type CreditPurchase = Schemas["CreditPurchase"] & { readonly sandbox?: boolean };
export type Notification = Schemas["Notification"];
export type NotificationKind = Schemas["NotificationKind"];
export type NotificationPreference = Schemas["NotificationPreferences"]["items"][number];
export type MyAccount = Schemas["MyAccount"];
export type SecuritySummary = Schemas["SecuritySummary"];
export type MeAuditEntry = Schemas["MeAuditPage"]["items"][number];

/** The rate and the bounds, as the server states them. Nothing derives them here. */
export function useCreditPricing(): UseQueryResult<CreditPricing> {
  return useQuery({
    queryKey: meKeys.pricing,
    queryFn: async () => {
      const { data } = await api.GET("/credits/pricing", {});
      return validated<CreditPricing>(data, creditPricingSpec, "/credits/pricing");
    },
  });
}

export interface StartPurchaseInput {
  readonly accountId: string;
  /** Exact minor units of the pricing currency. Never a Credit quantity: there is no such field. */
  readonly amountMinor: number;
  readonly currency: string;
  /** Created once, when the customer confirmed the amount. Reused by every retry. */
  readonly idempotencyKey: string;
}

export function useStartCreditPurchase(): UseMutationResult<CreditPurchase, unknown, StartPurchaseInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: StartPurchaseInput) => {
      const { data } = await api.POST("/payments", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          amount_minor: input.amountMinor,
          currency: input.currency,
        },
      });
      return validated<CreditPurchase>(data, creditPurchaseSpec, "/payments");
    },
    onSuccess: (purchase, input) => {
      // The balance is not moved by this call — a PaymentIntent is not money —
      // but the purchase row exists now, so anything reading it is stale.
      void qc.invalidateQueries({ queryKey: meKeys.purchase(purchase.purchase_id) });
      void qc.invalidateQueries({ queryKey: nodalKeys.credits(input.accountId) });
    },
  });
}

/**
 * The authoritative state of one purchase.
 *
 * `refetchMs` is how the "Balance updating" state waits: the webhook is what
 * captures a payment, and a provider redirect only says the customer came back.
 * Callers stop polling when the state is terminal rather than polling forever.
 */
export function useCreditPurchase(
  purchaseId: string | undefined,
  options: { readonly refetchMs?: number } = {},
): UseQueryResult<CreditPurchase> {
  return useQuery({
    queryKey: meKeys.purchase(purchaseId ?? ""),
    enabled: purchaseId !== undefined && purchaseId !== "",
    staleTime: 0,
    ...(options.refetchMs === undefined ? {} : { refetchInterval: options.refetchMs }),
    queryFn: async () => {
      const { data } = await api.GET("/payments/{paymentId}", {
        params: { path: { paymentId: purchaseId ?? "" } },
      });
      return validated<CreditPurchase>(data, creditPurchaseSpec, "/payments/{id}");
    },
  });
}

export interface NotificationList {
  readonly items: Notification[];
  readonly nextCursor: string | null;
}

export function useNotifications(options: {
  readonly unread: boolean;
  readonly cursor?: string;
}): UseQueryResult<NotificationList> {
  const { unread, cursor } = options;
  return useQuery({
    queryKey: meKeys.notificationPage(unread, cursor ?? ""),
    queryFn: async () => {
      const { data } = await api.GET("/me/notifications", {
        params: {
          query: {
            unread,
            limit: 50,
            ...(cursor === undefined || cursor === "" ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["NotificationPage"]>(
        data,
        { arrays: { items: { required: true, spec: notificationSpec } } },
        "/me/notifications",
      );
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/** Everything a change to one notification makes stale. */
function invalidateNotifications(qc: ReturnType<typeof useQueryClient>): void {
  // One prefix covers the bell and every page of the list, so they can never
  // disagree about how many are unread.
  void qc.invalidateQueries({ queryKey: meKeys.notifications });
}

export function useMarkNotificationRead(): UseMutationResult<Notification, unknown, string> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (notificationId: string) => {
      const { data } = await api.POST("/me/notifications/{notificationId}/read", {
        ...idempotent(newIdempotencyKey(), { path: { notificationId } }),
      });
      return validated<Notification>(data, notificationSpec, "/me/notifications/{id}/read");
    },
    onSuccess: () => {
      invalidateNotifications(qc);
    },
  });
}

export function useMarkAllNotificationsRead(): UseMutationResult<number, unknown, void> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { data } = await api.POST("/me/notifications/read-all", {
        ...idempotent(newIdempotencyKey()),
      });
      return validated<Schemas["MarkedRead"]>(data, markedReadSpec, "/me/notifications/read-all").updated;
    },
    onSuccess: () => {
      invalidateNotifications(qc);
    },
  });
}

export function useNotificationPreferences(): UseQueryResult<NotificationPreference[]> {
  return useQuery({
    queryKey: meKeys.notificationPreferences,
    queryFn: async () => {
      const { data } = await api.GET("/me/notification-preferences", {});
      return validated<Schemas["NotificationPreferences"]>(
        data,
        { arrays: { items: { required: true, spec: notificationPreferenceSpec } } },
        "/me/notification-preferences",
      ).items;
    },
  });
}

export interface PreferenceChange {
  readonly kind: NotificationKind;
  readonly enabled: boolean;
}

export function useUpdateNotificationPreferences(): UseMutationResult<
  NotificationPreference[],
  unknown,
  readonly PreferenceChange[]
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (changes: readonly PreferenceChange[]) => {
      const { data } = await api.PUT("/me/notification-preferences", {
        ...idempotent(newIdempotencyKey()),
        body: { items: changes.map((c) => ({ kind: c.kind, enabled: c.enabled })) },
      });
      return validated<Schemas["NotificationPreferences"]>(
        data,
        { arrays: { items: { required: true, spec: notificationPreferenceSpec } } },
        "/me/notification-preferences",
      ).items;
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: meKeys.notificationPreferences });
    },
  });
}

export function useMyAccount(enabled = true): UseQueryResult<MyAccount> {
  return useQuery({
    queryKey: meKeys.myAccount,
    enabled,
    staleTime: 0,
    queryFn: async () => {
      const { data } = await api.GET("/me/account", {});
      return validated<MyAccount>(data, myAccountSpec, "/me/account");
    },
  });
}

export interface CloseAccountInput {
  readonly reason?: string;
  readonly idempotencyKey: string;
}

/**
 * Asks for closure. Needs a recent strong authentication, so the caller must be
 * ready to render the step-up round trip and come back to the same place.
 */
export function useCloseAccount(): UseMutationResult<MyAccount, unknown, CloseAccountInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: CloseAccountInput) => {
      const { data } = await api.POST("/me/account/close", {
        ...idempotent(input.idempotencyKey),
        body: input.reason === undefined || input.reason === "" ? {} : { reason: input.reason },
      });
      return validated<MyAccount>(data, myAccountSpec, "/me/account/close");
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: meKeys.myAccount });
    },
  });
}

/**
 * Cancels an open closure request.
 *
 * Deliberately no step-up: requesting closure is the dangerous direction, and
 * stopping a request must never be harder than starting it.
 */
export function useCancelAccountClosure(): UseMutationResult<MyAccount, unknown, void> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { data } = await api.POST("/me/account/close/cancel", {
        ...idempotent(newIdempotencyKey()),
      });
      return validated<MyAccount>(data, myAccountSpec, "/me/account/close/cancel");
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: meKeys.myAccount });
    },
  });
}

export function useSecuritySummary(enabled = true): UseQueryResult<SecuritySummary> {
  return useQuery({
    queryKey: meKeys.security,
    enabled,
    staleTime: 0,
    queryFn: async () => {
      const { data } = await api.GET("/me/security", {});
      return validated<SecuritySummary>(data, securitySummarySpec, "/me/security");
    },
  });
}

export interface AuditPage {
  readonly items: MeAuditEntry[];
  readonly nextCursor: string | null;
}

export function useMeAudit(cursor: string | undefined): UseQueryResult<AuditPage> {
  return useQuery({
    queryKey: meKeys.audit(cursor ?? ""),
    queryFn: async () => {
      const { data } = await api.GET("/me/audit", {
        params: {
          query: cursor === undefined || cursor === "" ? { limit: 50 } : { limit: 50, cursor },
        },
      });
      const page = validated<Schemas["MeAuditPage"]>(
        data,
        { arrays: { items: { required: true, spec: meAuditEntrySpec } } },
        "/me/audit",
      );
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/* --------------------------------------------------------------------------
 * Portfolio, the activity feed and market discovery (goal §15, §16, §12).
 *
 * Three reads, one rule between them: the SERVER computes and this file
 * checks. Nothing here sums a position into a total, derives a P&L from a
 * balance difference, or ranks a market — `/v1/me/portfolio` carries its own
 * `totals` marked at an explicit `as_of`, and `/v1/native-markets` carries the
 * `sort` it applied and whether paging that ordering is stable. A browser that
 * re-derived any of those would eventually disagree with the ledger, and the
 * ledger is the one that is right.
 * ------------------------------------------------------------------------ */

export type Portfolio = Schemas["Portfolio"];
export type PortfolioPosition = Schemas["PortfolioPosition"];
export type PortfolioTotals = Schemas["PortfolioTotals"];
export type ValueTemperature = Schemas["ValueTemperature"];
export type ActivityFeedItem = Schemas["ActivityFeedItem"];
export type ActivityFeedKind = Schemas["ActivityFeedKind"];
export type ActivityAmount = Schemas["ActivityFeedItem"]["amounts"][number];
export type NativeMarketSummary = Schemas["NativeMarketSummary"];
export type MarketSort = NonNullable<Schemas["NativeMarketPage"]["sort"]>;

export const portfolioKeys = {
  portfolio: (accountId: string) => ["me", "portfolio", accountId] as const,
  activity: (accountId: string, kinds: string, cursor: string) =>
    ["me", "activity", accountId, kinds, cursor] as const,
  markets: (sort: string, limit: number) => ["native-markets", sort, limit] as const,
};

/**
 * The Credit balance, every open position and the totals, all marked at one
 * instant the server states.
 *
 * `staleTime: 0` because this is a balance: a figure nobody is refreshing must
 * not be shown as current, and the stream invalidates this key rather than
 * patching it.
 */
export function usePortfolio(accountId: string | undefined): UseQueryResult<Portfolio> {
  return useQuery({
    queryKey: portfolioKeys.portfolio(accountId ?? ""),
    enabled: accountId !== undefined,
    staleTime: 0,
    queryFn: async () => {
      const { data } = await api.GET("/me/portfolio", {
        params: { query: { account_id: accountId ?? "" } },
      });
      const portfolio = validated<Portfolio>(data, portfolioSpec, "/me/portfolio");
      // The two nested objects, checked against their own specs rather than
      // waved through as `object`: they are where every figure on the page
      // comes from, and a malformed one must be an error and not a blank.
      validated<CreditBalance>(portfolio.credits, creditBalanceSpec, "/me/portfolio.credits");
      validated<PortfolioTotals>(portfolio.totals, portfolioTotalsSpec, "/me/portfolio.totals");
      return portfolio;
    },
  });
}

export interface ActivityFeed {
  readonly items: ActivityFeedItem[];
  readonly nextCursor: string | null;
}

/**
 * The unified timeline.
 *
 * `kind` repeats in the query string rather than being comma-joined, which is
 * what the contract declares. An empty filter is omitted entirely, because
 * "every kind" and "none of them" are different requests and only one of them
 * is what an unfiltered feed means.
 */
export function useMeActivity(options: {
  readonly accountId: string | undefined;
  readonly kinds: readonly ActivityFeedKind[];
  readonly cursor?: string;
  readonly limit?: number;
}): UseQueryResult<ActivityFeed> {
  const { accountId, kinds, cursor } = options;
  const limit = options.limit ?? 50;
  return useQuery({
    queryKey: portfolioKeys.activity(accountId ?? "", kinds.join(","), cursor ?? ""),
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/me/activity", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit,
            ...(kinds.length === 0 ? {} : { kind: [...kinds] }),
            ...(cursor === undefined || cursor === "" ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["ActivityFeedPage"]>(
        data,
        { arrays: { items: { required: true, spec: activityFeedItemSpec } } },
        "/me/activity",
      );
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export interface MarketPage {
  readonly markets: NativeMarketSummary[];
  readonly sort: MarketSort;
  /** Whether paging this ordering sees every market exactly once. */
  readonly stable: boolean;
  readonly nextCursor: string | null;
}

/**
 * One page of market discovery, in the ordering the server applied.
 *
 * `sort` and `stable` come back from the response rather than being assumed
 * from the request: only NEWEST is stable under paging, because every other key
 * ranks by a figure that moves when somebody trades, and a page that claimed
 * otherwise would show a reader the same market twice and call it two.
 */
export function useMarketDiscovery(options: {
  readonly sort: MarketSort;
  readonly limit?: number;
  readonly enabled?: boolean;
}): UseQueryResult<MarketPage> {
  const limit = options.limit ?? 50;
  return useQuery({
    queryKey: portfolioKeys.markets(options.sort, limit),
    enabled: options.enabled !== false,
    queryFn: async () => {
      const { data } = await api.GET("/native-markets", {
        params: { query: { sort: options.sort, limit } },
      });
      const page = validated<Schemas["NativeMarketPage"]>(
        data,
        {
          required: { sort: "string", stable: "boolean" },
          arrays: { markets: { required: true, spec: nativeMarketSummarySpec } },
        },
        "/native-markets",
      );
      return {
        markets: page.markets,
        sort: page.sort,
        stable: page.stable,
        nextCursor: page.next_cursor,
      };
    },
  });
}

/* --------------------------------------------------------------------------
 * The public reads (D-080)
 *
 * Both answer without a session, and both are used by the signed-out site. They
 * are named `usePublic*` so that the signed-in screens — which need acceptance
 * state, and the market detail the discovery list does not carry — cannot reach
 * for them by accident and get a thinner answer than they needed.
 * ------------------------------------------------------------------------ */

export type PublicLegalDocument = Schemas["PublicLegalDocument"];

export const publicKeys = {
  terms: ["public", "terms"] as const,
  markets: (limit: number) => ["public", "native-markets", limit] as const,
};

/**
 * The legal registry, with the text, for anyone.
 *
 * This is the same registry `/me/terms-acceptances` serves and the same bytes:
 * `content_hash` matches, so the words a visitor reads on the public site are
 * the words the acceptance record will name. That is why the public pages
 * render this rather than prose of their own — an explainer beside an agreement
 * is two texts, and a reader can only be misled by the one that is not binding.
 */
export function usePublicTerms(): UseQueryResult<PublicLegalDocument[]> {
  return useQuery({
    queryKey: publicKeys.terms,
    // The registry changes when somebody ships a new version, not while a page
    // is open. Five minutes is long enough to make navigating between the
    // documents free and short enough that a re-issue reaches a visitor who
    // leaves the tab open.
    staleTime: 5 * 60 * 1000,
    queryFn: async () => {
      const { data } = await api.GET("/terms", {});
      return validatedList<PublicLegalDocument>(data, publicLegalDocumentSpec, "/terms");
    },
  });
}

export interface PublicMarkets {
  readonly markets: readonly NativeMarketSummary[];
  /** Which ordering the backend actually used. */
  readonly sort: string;
  /** Whether paging this ordering sees every market exactly once. */
  readonly stable: boolean;
}

/**
 * A page of the market discovery list, without a session.
 *
 * The public preview takes the newest markets and nothing else: NEWEST is the
 * only ordering that is stable under paging, and a marketing surface ranking by
 * a live figure would be showing a leaderboard that reorders while it is read.
 */
export function usePublicMarkets(limit = 6): UseQueryResult<PublicMarkets> {
  return useQuery({
    queryKey: publicKeys.markets(limit),
    queryFn: async () => {
      const { data } = await api.GET("/native-markets", {
        params: { query: { limit, sort: "NEWEST" } },
      });
      const page = validated<{
        markets: NativeMarketSummary[];
        sort: string;
        stable: boolean;
      }>(data, nativeMarketPageSpec, "/native-markets");
      return { markets: page.markets, sort: page.sort, stable: page.stable };
    },
  });
}

/* --------------------------------------------------------------------------
 * Agents and strategies (goal §17, §18)
 *
 * Three separate acts, three separate hooks, because the goal requires them to
 * be separate acts: describing a strategy in words, asking for it to be
 * compiled, and granting an agent authority over Credits are different
 * decisions and a customer makes each one explicitly. There is deliberately no
 * hook that does two of them in one call.
 *
 * Compiling is a mutation even when it produces nothing. Every attempt is
 * recorded — including an attempt on a deployment with no compiler backend,
 * which is recorded with outcome MODEL_UNAVAILABLE and the failure code
 * COMPILER_UNAVAILABLE. "We did not try, and this is why" is a fact about a
 * strategy, and the interface shows the API's own words for it rather than
 * inventing an IR nobody produced.
 * ------------------------------------------------------------------------ */

export type Strategy = Schemas["Strategy"];
export type StrategyVersion = Schemas["StrategyVersion"];
export type CompileResult = Schemas["CompileResult"];
export type Agent = Schemas["Agent"];
export type AgentLimits = Schemas["AgentLimits"];
export type AgentSchedule = Schemas["AgentSchedule"];
export type AuthorityLevel = Schemas["AuthorityLevel"];
export type AgentAction = "enable" | "pause" | "resume" | "disable" | "archive";

/** A page of strategies, with the one deployment fact the create flow needs first. */
/** Which compiler this deployment has, as the API reports it. */
export type CompilerDescriptor = Schemas["CompilerDescriptor"];
/** The strategy a person states field by field, which is what a structured compiler reads. */
export type StructuredStrategy = Schemas["StructuredStrategy"];

export interface StrategyList {
  readonly items: Strategy[];
  /**
   * Which compiler answered, when there is one. `sandbox` true means every
   * version it produces is a rehearsal and is labelled one; `structured` true
   * means it reads the fields the customer states and never the description.
   */
  readonly compiler: CompilerDescriptor | undefined;
  /**
   * False means a compile attempt will be recorded with COMPILER_UNAVAILABLE
   * and produce no IR. The flow says so before a description is written rather
   * than after.
   */
  readonly compilerConfigured: boolean;
}

/** A page of agents, with every declared authority level, enabled or not. */
export interface AgentList {
  readonly items: Agent[];
  readonly authorityLevels: AuthorityLevel[];
}

export const agentKeys = {
  strategies: (accountId: string) => ["strategies", accountId] as const,
  strategy: (id: string) => ["strategy", id] as const,
  agents: (accountId: string) => ["agents", accountId] as const,
  agent: (id: string) => ["agent", id] as const,
  payout: (id: string) => ["payout", id] as const,
};

export function useStrategies(accountId: string | undefined): UseQueryResult<StrategyList> {
  return useQuery({
    queryKey: agentKeys.strategies(accountId ?? ""),
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/strategies", {
        params: { query: { account_id: accountId ?? "", limit: 50 } },
      });
      const page = validated<Schemas["StrategyPage"]>(
        data,
        {
          required: { compiler_configured: "boolean" },
          arrays: { items: { required: true, spec: strategySpec } },
        },
        "/strategies",
      );
      page.items.forEach((item, index) => {
        validatedStrategy<Strategy>(item, `/strategies.items[${String(index)}]`);
      });
      return {
        items: page.items,
        compiler: page.compiler,
        compilerConfigured: page.compiler_configured,
      };
    },
  });
}

export function useStrategy(strategyId: string | undefined): UseQueryResult<Strategy> {
  return useQuery({
    queryKey: agentKeys.strategy(strategyId ?? ""),
    enabled: strategyId !== undefined && strategyId !== "",
    queryFn: async () => {
      const { data } = await api.GET("/strategies/{strategyId}", {
        params: { path: { strategyId: strategyId ?? "" } },
      });
      return validatedStrategy<Strategy>(data, "/strategies/{id}");
    },
  });
}

export interface CreateStrategyInput {
  readonly accountId: string;
  readonly name: string;
  readonly description: string;
  /**
   * The strategy stated field by field. Omitted on a deployment whose compiler
   * reads natural language; required by one that does not, which refuses an
   * incomplete document by naming the fields rather than filling them in.
   */
  readonly constraints?: StructuredStrategy;
  /** Minted when the customer confirms the description, never on render. */
  readonly idempotencyKey: string;
}

export function useCreateStrategy(): UseMutationResult<Strategy, unknown, CreateStrategyInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: CreateStrategyInput) => {
      const { data } = await api.POST("/strategies", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          name: input.name,
          description: input.description,
          // The declared strategy travels in its own field and is never merged
          // into the description. That separation is what lets a structured
          // compiler claim it did not read the prose.
          ...(input.constraints === undefined ? {} : { constraints: input.constraints }),
        },
      });
      return validatedStrategy<Strategy>(data, "/strategies");
    },
    onSuccess: (_strategy, input) => {
      void qc.invalidateQueries({ queryKey: agentKeys.strategies(input.accountId) });
    },
  });
}

export interface CompileStrategyInput {
  readonly strategyId: string;
  /**
   * The key is also the compiler's request id, which is what bounds a compile
   * request to eight attempts. Two keys are two requests, which is the right
   * reading: asking again is a new decision by the customer, not a retry of
   * the last one.
   */
  readonly idempotencyKey: string;
}

export function useCompileStrategy(): UseMutationResult<CompileResult, unknown, CompileStrategyInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: CompileStrategyInput) => {
      const { data } = await api.POST("/strategies/{strategyId}/compile", {
        ...idempotent(input.idempotencyKey, { path: { strategyId: input.strategyId } }),
      });
      return validatedCompileResult<CompileResult>(data, "/strategies/{id}/compile");
    },
    onSuccess: (_result, input) => {
      void qc.invalidateQueries({ queryKey: agentKeys.strategy(input.strategyId) });
    },
  });
}

export interface AcceptStrategyVersionInput {
  readonly strategyId: string;
  /** The version NUMBER, which is what the review screen shows. */
  readonly version: number;
  /**
   * The hash of the document that was on screen when the customer pressed the
   * button. The API compares it and refuses a mismatch, so "accept" means
   * "accept THIS document" rather than "accept whatever is there now".
   */
  readonly irHash: string;
  /** Minted at the moment of acceptance, never on render. */
  readonly idempotencyKey: string;
}

/**
 * The review step of goal §18, recorded.
 *
 * It is a separate mutation from creating the agent because they are separate
 * acts: reading a compiled strategy and approving it is what makes an agent
 * possible, and creating one is a decision made afterwards.
 */
export function useAcceptStrategyVersion(): UseMutationResult<
  StrategyVersion,
  unknown,
  AcceptStrategyVersionInput
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: AcceptStrategyVersionInput) => {
      const { data } = await api.POST("/strategies/{strategyId}/versions/{version}/accept", {
        ...idempotent(input.idempotencyKey, {
          path: { strategyId: input.strategyId, version: input.version },
        }),
        body: { ir_hash: input.irHash },
      });
      return validatedStrategyVersion<StrategyVersion>(
        data,
        "/strategies/{id}/versions/{version}/accept",
      );
    },
    onSuccess: (_version, input) => {
      void qc.invalidateQueries({ queryKey: agentKeys.strategy(input.strategyId) });
    },
  });
}

export function useAgents(accountId: string | undefined): UseQueryResult<AgentList> {
  return useQuery({
    queryKey: agentKeys.agents(accountId ?? ""),
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/agents", {
        params: { query: { account_id: accountId ?? "", limit: 100 } },
      });
      const page = validated<Schemas["AgentPage"]>(
        data,
        {
          arrays: {
            items: { required: true, spec: agentSpec },
            authority_levels: { required: true, spec: authorityLevelSpec },
          },
        },
        "/agents",
      );
      page.items.forEach((item, index) => {
        validatedAgent<Agent>(item, `/agents.items[${String(index)}]`);
      });
      return { items: page.items, authorityLevels: page.authority_levels };
    },
  });
}

export function useAgent(agentId: string | undefined): UseQueryResult<Agent> {
  return useQuery({
    queryKey: agentKeys.agent(agentId ?? ""),
    enabled: agentId !== undefined && agentId !== "",
    queryFn: async () => {
      const { data } = await api.GET("/agents/{agentId}", {
        params: { path: { agentId: agentId ?? "" } },
      });
      return validatedAgent<Agent>(data, "/agents/{id}");
    },
  });
}

export interface CreateAgentInput {
  readonly accountId: string;
  readonly strategyId: string;
  /** Always a specific compiled version. An agent never follows "the latest". */
  readonly strategyVersionId: string;
  readonly name: string;
  readonly authorityLevel: number;
  readonly limits: AgentLimits;
  readonly idempotencyKey: string;
}

export function useCreateAgent(): UseMutationResult<Agent, unknown, CreateAgentInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: CreateAgentInput) => {
      const { data } = await api.POST("/agents", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          strategy_id: input.strategyId,
          strategy_version_id: input.strategyVersionId,
          name: input.name,
          authority_level: input.authorityLevel,
          limits: input.limits,
        },
      });
      return validatedAgent<Agent>(data, "/agents");
    },
    onSuccess: (_agent, input) => {
      void qc.invalidateQueries({ queryKey: agentKeys.agents(input.accountId) });
    },
  });
}

export interface AgentActionInput {
  readonly agentId: string;
  readonly accountId: string;
  readonly action: AgentAction;
  readonly reason?: string;
  readonly idempotencyKey: string;
}

/**
 * One lifecycle action on one agent.
 *
 * The action is part of the path rather than the body because each of the five
 * is a different authority, and an idempotency key is scoped to the operation:
 * a body field would let one key replay across two different decisions.
 */
export function useAgentAction(): UseMutationResult<Agent, unknown, AgentActionInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: AgentActionInput) => {
      const reason = input.reason;
      const { data } = await api.POST("/agents/{agentId}/{action}", {
        ...idempotent(input.idempotencyKey, {
          path: { agentId: input.agentId, action: input.action },
        }),
        ...(reason === undefined || reason === "" ? {} : { body: { reason } }),
      });
      return validatedAgent<Agent>(data, "/agents/{id}/{action}");
    },
    onSuccess: (_agent, input) => {
      void qc.invalidateQueries({ queryKey: agentKeys.agent(input.agentId) });
      void qc.invalidateQueries({ queryKey: agentKeys.agents(input.accountId) });
    },
  });
}

/* --------------------------------------------------------------------------
 * Following one payout request (goal §19)
 * ------------------------------------------------------------------------ */

/**
 * One payout request, polled while it is still moving.
 *
 * A payout is the one place where the answer genuinely changes without the
 * customer doing anything: a provider accepts it, then settles it, and the
 * page has to follow that rather than ask somebody to reload. The event stream
 * invalidates this key too; the poll is the floor, not the mechanism.
 */
export function usePayout(
  payoutId: string | undefined,
  options: { readonly refetchMs?: number } = {},
): UseQueryResult<PayoutRequest> {
  return useQuery({
    queryKey: agentKeys.payout(payoutId ?? ""),
    enabled: payoutId !== undefined && payoutId !== "",
    staleTime: 0,
    ...(options.refetchMs === undefined ? {} : { refetchInterval: options.refetchMs }),
    queryFn: async () => {
      const { data } = await api.GET("/payouts/{payoutId}", {
        params: { path: { payoutId: payoutId ?? "" } },
      });
      return validatedPayout<PayoutRequest>(data, "/payouts/{id}");
    },
  });
}

export interface CancelPayoutInput {
  readonly payoutId: string;
  readonly accountId: string;
  readonly reason: string;
  readonly idempotencyKey: string;
}

/**
 * Withdraws a request the provider has not been given yet.
 *
 * The reserved Credits go back to the exact lots they came from with their
 * provenance intact, which is why this is a command with its own key rather
 * than a local undo.
 */
export function useCancelPayout(): UseMutationResult<PayoutRequest, unknown, CancelPayoutInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: CancelPayoutInput) => {
      const { data } = await api.POST("/payouts/{payoutId}/cancel", {
        ...idempotent(input.idempotencyKey, { path: { payoutId: input.payoutId } }),
        body: { account_id: input.accountId, reason: input.reason },
      });
      return validatedPayout<PayoutRequest>(data, "/payouts/{id}/cancel");
    },
    onSuccess: (_payout, input) => {
      void qc.invalidateQueries({ queryKey: agentKeys.payout(input.payoutId) });
      void qc.invalidateQueries({ queryKey: nodalKeys.payouts(input.accountId) });
      void qc.invalidateQueries({ queryKey: nodalKeys.credits(input.accountId) });
    },
  });
}

/* --------------------------------------------------------------------------
 * Verification, eligibility, destinations and quotes (goal §19-§21, §23-§25)
 *
 * The order of the hooks below is the order of the journey, and the separation
 * between them is the architecture: verification changes a PROFILE, eligibility
 * reads a POLICY against provenance, a destination is a PROVIDER'S token, and a
 * quote is a pre-commitment that reserves nothing. No hook here converts one
 * into another, and there is deliberately none that asks "can I withdraw?" in a
 * single call — because the answer is composed from four different facts and a
 * screen that showed one of them as the answer would be wrong three ways.
 * ------------------------------------------------------------------------ */

export type VerificationProfile = Schemas["VerificationProfile"];
export type VerificationSession = Schemas["VerificationSession"];
export type VerificationCheck = Schemas["VerificationCheck"];
export type VerificationRequirement = Schemas["VerificationRequirement"];
export type StartedVerification = Schemas["StartedVerification"];
export type SandboxOutcome = Schemas["SandboxVerificationOutcome"]["outcome"];
export type WithdrawalEligibility = Schemas["WithdrawalEligibility"];
export type WithdrawalOriginBucket = Schemas["WithdrawalOriginBucket"];
export type PayoutDestination = Schemas["PayoutDestination"];
export type PayoutDestinationKind = Schemas["PayoutDestination"]["kind"];
export type PayoutQuote = Schemas["PayoutQuote"];
export type PayoutProvenanceSlice = Schemas["PayoutProvenanceSlice"];

export const withdrawKeys = {
  verification: (accountId: string) => ["me", "verification", accountId] as const,
  verificationSession: (accountId: string, sessionId: string) =>
    ["me", "verification", accountId, "session", sessionId] as const,
  eligibility: (accountId: string) => ["me", "eligibility", accountId] as const,
  destinations: (accountId: string) => ["me", "payout-destinations", accountId] as const,
};

/**
 * The financial verification profile: state, level, the sub-checks behind it,
 * and what is missing.
 *
 * It is never served from cache. A provider callback can move it at any moment,
 * and a screen that offered "Start verification" against a stale profile would
 * be offering to start something that has already finished.
 */
export function useVerification(accountId: string | undefined): UseQueryResult<VerificationProfile> {
  return useQuery({
    queryKey: withdrawKeys.verification(accountId ?? ""),
    enabled: accountId !== undefined,
    staleTime: 0,
    queryFn: async () => {
      const { data } = await api.GET("/me/verification", {
        params: { query: { account_id: accountId ?? "" } },
      });
      return validatedVerificationProfile<VerificationProfile>(data, "/me/verification");
    },
  });
}

export interface StartVerificationInput {
  readonly accountId: string;
  /** ISO 3166-1 alpha-2, upper case. Never inferred from a network address. */
  readonly country: string;
  readonly region?: string;
  readonly idempotencyKey: string;
}

/**
 * Opens a provider-hosted verification.
 *
 * The jurisdiction is supplied by the person and by nobody else. Deriving it
 * from an IP address would be a legal determination wearing a network header's
 * clothes, and the API refuses to make one — so this hook has no fallback and
 * no default country.
 */
export function useStartVerification(): UseMutationResult<StartedVerification, unknown, StartVerificationInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: StartVerificationInput) => {
      const region = input.region;
      const { data } = await api.POST("/me/verification/sessions", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          purpose: "PAYOUT_KYC",
          jurisdiction_country: input.country,
          ...(region === undefined || region === "" ? {} : { jurisdiction_region: region }),
        },
      });
      return validatedStartedVerification<StartedVerification>(data, "/me/verification/sessions");
    },
    onSuccess: (_started, input) => {
      void qc.invalidateQueries({ queryKey: withdrawKeys.verification(input.accountId) });
    },
  });
}

/**
 * Asks the provider what happened, and records it.
 *
 * It is a GET that changes state on the server, which is the only shape that
 * works: somebody coming back from a hosted flow has said they came back, not
 * that they passed. Never trusting the redirect is the rule; polling is how it
 * is kept.
 */
export function usePollVerification(
  accountId: string | undefined,
  sessionId: string | undefined,
  options: { readonly refetchMs?: number } = {},
): UseQueryResult<VerificationSession> {
  return useQuery({
    queryKey: withdrawKeys.verificationSession(accountId ?? "", sessionId ?? ""),
    enabled: accountId !== undefined && sessionId !== undefined && sessionId !== "",
    staleTime: 0,
    ...(options.refetchMs === undefined ? {} : { refetchInterval: options.refetchMs }),
    queryFn: async () => {
      const { data } = await api.GET("/me/verification/sessions/{sessionId}", {
        params: { path: { sessionId: sessionId ?? "" }, query: { account_id: accountId ?? "" } },
      });
      return validated<VerificationSession>(data, verificationSessionSpec, "/me/verification/sessions/{id}");
    },
  });
}

export interface SandboxOutcomeInput {
  readonly accountId: string;
  readonly outcome: SandboxOutcome;
  readonly idempotencyKey: string;
}

/**
 * Chooses what a REHEARSAL verification decides. Sandbox tier only.
 *
 * The API refuses this with FORBIDDEN on any deployment that is not a sandbox
 * tier, and there is no default outcome anywhere in the path: a rehearsal
 * session nobody has answered stays pending forever, because "approved unless
 * told otherwise" is a fabricated approval with extra steps. The interface
 * offers this control only where the API has said the session has one.
 */
export function useSandboxOutcome(): UseMutationResult<VerificationSession, unknown, SandboxOutcomeInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: SandboxOutcomeInput) => {
      const { data } = await api.POST("/me/verification/sandbox-outcome", {
        ...idempotent(input.idempotencyKey),
        body: { account_id: input.accountId, outcome: input.outcome },
      });
      return validated<VerificationSession>(data, verificationSessionSpec, "/me/verification/sandbox-outcome");
    },
    onSuccess: (_session, input) => {
      void qc.invalidateQueries({ queryKey: withdrawKeys.verification(input.accountId) });
      void qc.invalidateQueries({ queryKey: withdrawKeys.eligibility(input.accountId) });
    },
  });
}

/**
 * What of this balance may be withdrawn, per origin, and why not the rest.
 *
 * `staleTime: 0` for the same reason buying power has it: eligibility is
 * recomputed by the backend from a policy the deployment can change, from
 * provenance that moves with every trade, and from capability gates an operator
 * can close. A cached answer is a claim about money that may no longer be true.
 */
export function useEligibility(accountId: string | undefined): UseQueryResult<WithdrawalEligibility> {
  return useQuery({
    queryKey: withdrawKeys.eligibility(accountId ?? ""),
    enabled: accountId !== undefined,
    staleTime: 0,
    refetchOnWindowFocus: true,
    queryFn: async () => {
      const { data } = await api.GET("/me/eligibility", {
        params: { query: { account_id: accountId ?? "" } },
      });
      return validatedEligibility<WithdrawalEligibility>(data, "/me/eligibility");
    },
  });
}

export function usePayoutDestinations(accountId: string | undefined): UseQueryResult<PayoutDestination[]> {
  return useQuery({
    queryKey: withdrawKeys.destinations(accountId ?? ""),
    enabled: accountId !== undefined,
    queryFn: async () => {
      const { data } = await api.GET("/me/payout-destinations", {
        params: { query: { account_id: accountId ?? "" } },
      });
      return validatedList<PayoutDestination>(data, payoutDestinationSpec, "/me/payout-destinations");
    },
  });
}

export interface AddDestinationInput {
  readonly accountId: string;
  readonly kind: PayoutDestinationKind;
  /** The provider's token, or a sandbox handle. Never an account number. */
  readonly providerToken: string;
  readonly displayLabel?: string;
  readonly currency?: string;
  /**
   * REQUIRED by the API: the provider is asked whether it can pay a recipient
   * there before the destination is registered, and it cannot be asked about a
   * country nobody stated (D-122).
   */
  readonly country: string;
  /** The subdivision, where the provider distinguishes them. */
  readonly region?: string;
  readonly idempotencyKey: string;
}

/**
 * Registers a destination from a provider token.
 *
 * What travels here is the PROVIDER'S reference plus a mask a person
 * recognises. An input that looks like an account number, a card number, an
 * IBAN, a private key or a seed phrase is refused by the API rather than
 * stored, and this hook does not pre-empt that check: the refusal belongs to
 * the backend, and rendering its message is how a person learns what the
 * product will and will not hold.
 *
 * Adding one needs a recent strong sign-in, so a STEP_UP_REQUIRED here is
 * expected rather than exceptional and the caller wires the round trip.
 */
export function useAddDestination(): UseMutationResult<PayoutDestination, unknown, AddDestinationInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: AddDestinationInput) => {
      const { data } = await api.POST("/me/payout-destinations", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          kind: input.kind,
          provider_token: input.providerToken,
          ...(input.displayLabel === undefined || input.displayLabel === ""
            ? {}
            : { display_label: input.displayLabel }),
          ...(input.currency === undefined || input.currency === "" ? {} : { currency: input.currency }),
          country: input.country,
          ...(input.region === undefined || input.region === "" ? {} : { region: input.region }),
        },
      });
      return validated<PayoutDestination>(data, payoutDestinationSpec, "/me/payout-destinations");
    },
    onSuccess: (_destination, input) => {
      void qc.invalidateQueries({ queryKey: withdrawKeys.destinations(input.accountId) });
      void qc.invalidateQueries({ queryKey: withdrawKeys.eligibility(input.accountId) });
    },
  });
}

export interface RemoveDestinationInput {
  readonly destinationId: string;
  readonly accountId: string;
  readonly idempotencyKey: string;
}

/**
 * Stops using a destination.
 *
 * It disables rather than deletes, because a destination value has left through
 * is financial history. A disabled one never comes back: adding it again is a
 * new registration with its own creation time, which is what makes a cooldown
 * on a changed destination a fact rather than a field somebody resets.
 */
export function useRemoveDestination(): UseMutationResult<PayoutDestination, unknown, RemoveDestinationInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: RemoveDestinationInput) => {
      const { data } = await api.DELETE("/me/payout-destinations/{destinationId}", {
        ...idempotent(input.idempotencyKey, {
          path: { destinationId: input.destinationId },
          query: { account_id: input.accountId },
        }),
      });
      return validated<PayoutDestination>(data, payoutDestinationSpec, "/me/payout-destinations/{id}");
    },
    onSuccess: (_destination, input) => {
      void qc.invalidateQueries({ queryKey: withdrawKeys.destinations(input.accountId) });
      void qc.invalidateQueries({ queryKey: withdrawKeys.eligibility(input.accountId) });
    },
  });
}

export interface PayoutQuoteInput {
  readonly accountId: string;
  readonly destinationId: string;
  /** The GROSS Credits the customer would give up. The fee comes out of it. */
  readonly amount: string;
  readonly idempotencyKey: string;
}

/**
 * What a payout would cost, before committing to it.
 *
 * A quote reserves nothing and writes no ledger row, and it expires. An expired
 * one is REFUSED rather than silently re-priced — somebody who saw a number and
 * pressed the button a quarter of an hour later is told the number moved, not
 * charged a different one — so the caller shows the expiry and takes a fresh
 * quote rather than hoping.
 *
 * It is a mutation rather than a query because the backend persists it and
 * because a price must not appear because a component remounted.
 */
export function usePayoutQuote(): UseMutationResult<PayoutQuote, unknown, PayoutQuoteInput> {
  return useMutation({
    mutationFn: async (input: PayoutQuoteInput) => {
      const { data } = await api.POST("/payouts/quote", {
        ...idempotent(input.idempotencyKey),
        body: {
          account_id: input.accountId,
          destination_id: input.destinationId,
          amount: input.amount,
        },
      });
      return validated<PayoutQuote>(data, payoutQuoteSpec, "/payouts/quote");
    },
  });
}

/* --------------------------------------------------------------------------
 * The markets area: filtered discovery, the trade screen, charts and the tape
 * (product goal §12–§14, §35)
 *
 * `useMarketDiscovery` above is the Home preview — one ordering, one page, no
 * filters. These are the markets AREA's reads, with the search, the status
 * filter and the cursor the `/markets` page offers, plus the three per-market
 * resources the trade screen needs.
 *
 * The query keys begin with `native-markets` and `native-market` on purpose:
 * those are the prefixes `StreamStatus.tsx` invalidates when the stream reports
 * a `market` or `position` scope. A key shaped any other way would be a screen
 * the stream cannot refresh, which on a trading screen means a price that stops
 * moving without saying so.
 *
 * Nothing here derives a figure. `liquidity_credits` is V + R as the backend
 * computed it and `change_24h_bps` is the backend's signed move; neither is
 * recomputed from the parts that are also on the row, because two answers to
 * one question is one answer too many.
 * ------------------------------------------------------------------------ */

export type NativeMarketDetail = Schemas["NativeMarketDetail"];
export type MarketSafetyLimits = Schemas["MarketSafetyLimits"];
export type NativeCandlePage = Schemas["NativeCandlePage"];
export type NativeCandle = Schemas["NativeCandle"];
export type NativeTradePage = Schemas["NativeTradePage"];
export type NativeTradePrint = Schemas["NativeTradePrint"];
export type MarketStatus = NonNullable<
  NonNullable<Paths["/native-markets"]["get"]["parameters"]["query"]>["status"]
>[number];
export type CandleInterval = NonNullable<
  Paths["/native-markets/{marketId}/candles"]["get"]["parameters"]["query"]
>["interval"];

export interface MarketQuery {
  readonly q: string;
  readonly sort: MarketSort;
  readonly status: readonly MarketStatus[];
  readonly cursor?: string;
  readonly limit?: number;
}

export interface MarketsPage {
  readonly markets: readonly NativeMarketSummary[];
  readonly nextCursor: string | null;
  readonly sort: MarketSort;
  /**
   * Whether paging this ordering sees every market exactly once. Only NEWEST
   * does; the rest rank by figures that move when somebody trades, and the page
   * says so rather than letting a reader assume otherwise.
   */
  readonly stable: boolean;
}

export const marketKeys = {
  list: (query: MarketQuery) =>
    [
      "native-markets",
      query.q,
      query.sort,
      [...query.status].join(","),
      query.cursor ?? "",
    ] as const,
  summary: (marketId: string, accountId?: string) =>
    ["native-market", marketId, "summary", accountId ?? ""] as const,
  candles: (marketId: string, interval: string, from: string, to: string) =>
    ["native-market", marketId, "candles", interval, from, to] as const,
  trades: (marketId: string) => ["native-market", marketId, "trades"] as const,
};

export function useNativeMarkets(query: MarketQuery): UseQueryResult<MarketsPage> {
  return useQuery({
    queryKey: marketKeys.list(query),
    // Every figure on this list is live. A cached page would show a price that
    // somebody else has already traded through.
    staleTime: 0,
    queryFn: async () => {
      const { data } = await api.GET("/native-markets", {
        params: {
          query: {
            sort: query.sort,
            limit: query.limit ?? 50,
            ...(query.q === "" ? {} : { q: query.q }),
            ...(query.status.length === 0 ? {} : { status: [...query.status] }),
            ...(query.cursor === undefined || query.cursor === "" ? {} : { cursor: query.cursor }),
          },
        },
      });
      const page = validated<Schemas["NativeMarketPage"]>(
        data,
        nativeMarketPageSpec,
        "/native-markets",
      );
      return {
        markets: page.markets,
        nextCursor: page.next_cursor ?? null,
        sort: page.sort,
        stable: page.stable,
      };
    },
  });
}

export interface MarketDetail {
  readonly market: NativeMarketSummary;
  readonly limits: MarketSafetyLimits;
  /**
   * The creator's account, which only this gated read carries: the summary the
   * public markets list serves has no identity on it at all (D-110).
   */
  readonly creatorAccountId: string | undefined;
  readonly topHolders: NonNullable<NativeMarketDetail["top_holders"]>;
}

/**
 * Everything the trade screen needs, from the one projection the markets page
 * also reads. The nested market and limits objects are validated with their own
 * specs — `Spec` describes arrays of objects but not a single nested one, so
 * they are checked here rather than waved through as "object", exactly as
 * `usePortfolio` does with `credits` and `totals`.
 */
export function useNativeMarketDetail(
  marketId: string | undefined,
  accountId?: string,
): UseQueryResult<MarketDetail> {
  return useQuery({
    queryKey: marketKeys.summary(marketId ?? "", accountId),
    enabled: marketId !== undefined && marketId !== "",
    // The state version moves on every trade and a quote priced against a stale
    // one is re-priced. Refetching is cheaper than explaining a rejection.
    staleTime: 0,
    // The stream invalidates this on every print, but a trading screen must not
    // depend on a connection staying up to keep its `as of` honest: without a
    // poll the stamp escalates to stale after thirty seconds and every figure on
    // the screen correctly goes faint. Fifteen seconds keeps it true.
    refetchInterval: 15_000,
    queryFn: async () => {
      const { data } = await api.GET("/native-markets/{marketId}/summary", {
        params: {
          path: { marketId: marketId ?? "" },
          // Only to mark the caller's own row in the holder list. It is not a
          // filter and it changes nothing else the response says (D-111).
          query: accountId === undefined ? {} : { account_id: accountId },
        },
      });
      const detail = validated<NativeMarketDetail>(
        data,
        nativeMarketDetailSpec,
        "/native-markets/{id}/summary",
      );
      return {
        market: validated<NativeMarketSummary>(
          detail.market,
          nativeMarketSummarySpec,
          "/native-markets/{id}/summary.market",
        ),
        limits: validated<MarketSafetyLimits>(
          detail.limits_in_force,
          marketSafetyLimitsSpec,
          "/native-markets/{id}/summary.limits_in_force",
        ),
        creatorAccountId: detail.creator_account_id,
        topHolders: detail.top_holders ?? [],
      };
    },
  });
}

/**
 * OHLCV over a bounded window.
 *
 * `from` and `to` are the caller's, not this hook's: a window computed inside
 * the hook would move on every render and make the query key a cache miss
 * forever. The page aligns the window to a bucket boundary once and holds it.
 */
export function useNativeCandles(
  marketId: string | undefined,
  interval: CandleInterval,
  from: string,
  to: string,
): UseQueryResult<NativeCandlePage> {
  return useQuery({
    queryKey: marketKeys.candles(marketId ?? "", interval, from, to),
    enabled: marketId !== undefined && marketId !== "",
    queryFn: async () => {
      const { data } = await api.GET("/native-markets/{marketId}/candles", {
        params: { path: { marketId: marketId ?? "" }, query: { interval, from, to } },
      });
      return validated<NativeCandlePage>(data, nativeCandlePageSpec, "/native-markets/{id}/candles");
    },
  });
}

/** The public tape. It carries no account identity, by the API's design. */
export function useNativeTrades(
  marketId: string | undefined,
  limit = 25,
): UseQueryResult<NativeTradePage> {
  return useQuery({
    queryKey: marketKeys.trades(marketId ?? ""),
    enabled: marketId !== undefined && marketId !== "",
    staleTime: 0,
    refetchInterval: 15_000,
    queryFn: async () => {
      const { data } = await api.GET("/native-markets/{marketId}/trades", {
        params: { path: { marketId: marketId ?? "" }, query: { limit } },
      });
      return validated<NativeTradePage>(data, nativeTradePageSpec, "/native-markets/{id}/trades");
    },
  });
}

/**
 * The caller's position in one market, picked out of the portfolio document.
 *
 * A selector rather than a second request: `usePortfolio` already reads
 * `/v1/me/portfolio` and carries the `as_of` every mark-to-market figure was
 * computed at. Two hooks reading one endpoint would be two answers to one
 * question, which is the thing this file exists to prevent.
 */
export function positionIn(
  portfolio: Portfolio | undefined,
  marketId: string,
): PortfolioPosition | undefined {
  return portfolio?.positions.find((position) => position.market_id === marketId);
}
