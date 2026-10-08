/**
 * The fees section's reads and commands: the transparent spread and fee
 * engine and the sandbox performance fee (internal/feeledger, ADR-0020).
 *
 * Modelled on `core.ts`: every response is checked against a spec before a
 * component sees it — exact base units (`quantity`), two-decimal USD strings
 * (`usd`), closed enums, required fields present, and
 * `value_domain: SIMULATED` on everything that reports value — and refused
 * as a ContractViolation otherwise, never repaired. A 4xx is the backend's
 * decision and is not retried.
 *
 * The published schedules are public to any signed-in customer; quotes,
 * bookings and periods are read for one account, which the backend scopes.
 * Every command carries an Idempotency-Key created once, when the person
 * confirmed, and reused for every retry of that same request
 * (`confirmAttempt` in lib/treasury.ts). The quote preview is a read: it asks
 * the fee engine to price an order and returns the disclosure it would make,
 * recording nothing.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { idempotent, type Schemas } from "@controlplane/generated-client";

import { PERSISTABLE_MODES, VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import { FEE_BOOKING_MODES, FEE_BOOKING_STATUSES, FEE_LINE_KINDS, FEE_PERIOD_STATUSES, FEE_SIDES } from "../../lib/fees.ts";
import type { ConfirmedAttempt } from "../../lib/treasury.ts";
import { api } from "../client.ts";
import { cursorPageSpec, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, retryTransient, type CursorPage } from "./core.ts";

export type FeeSchedule = Schemas["FeeSchedule"];
export type FeeScheduleVersion = Schemas["FeeScheduleVersion"];
export type FeeScheduleSummary = Schemas["FeeScheduleSummary"];
export type FeeScheduleHistory = Schemas["FeeScheduleHistory"];
export type FeeDisclosure = Schemas["FeeDisclosure"];
export type FeeCharges = Schemas["FeeCharges"];
export type FeeVerification = Schemas["FeeVerification"];
export type FeeQuoteInput = Schemas["FeeQuoteInput"];
export type FeeQuotePreview = Schemas["FeeQuotePreview"];
export type FeeQuoteRecord = Schemas["FeeQuoteRecord"];
export type FeeQuoteRecordRequest = Schemas["FeeQuoteRecordRequest"];
export type FeeBooking = Schemas["FeeBooking"];
export type FeeBookingDetail = Schemas["FeeBookingDetail"];
export type FeeSpreadLine = Schemas["FeeSpreadLine"];
export type FeeSpreadCharge = Schemas["FeeSpreadCharge"];
export type FeeSpreadAudit = Schemas["FeeSpreadAudit"];
export type FeeSpreadTotals = Schemas["FeeSpreadTotals"];
export type FeePeriod = Schemas["FeePeriod"];
export type FeePeriodTerms = Schemas["FeePeriodTerms"];
export type FeeFlow = Schemas["FeeFlow"];
export type FeePerformanceLine = Schemas["FeePerformanceLine"];
export type FeeStatement = Schemas["FeeStatement"];
export type FeeFlowResult = Schemas["FeeFlowResult"];
export type FeeLineResult = Schemas["FeeLineResult"];
export type FeePublishRequest = Schemas["FeePublishRequest"];
export type FeeOpenPeriodRequest = Schemas["FeeOpenPeriodRequest"];
export type FeeContribution = Schemas["FeeContribution"];
export type TreasuryAssetRef = Schemas["TreasuryAssetRef"];
export type TreasuryPrice = Schemas["TreasuryPrice"];

/** Rows per page of every cursor-paged fees list. */
export const FEES_PAGE_SIZE = 25;
/** The spread audit has no cursor: this many lines are asked for. */
export const SPREAD_AUDIT_LIMIT = 200;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const feeKeys = {
  /** Prefix of every read that is not one account's: the published schedules and the operator's audit. */
  all: ["advanced", "fees"] as const,
  schedules: (cursor: string) => ["advanced", "fees", "schedules", cursor] as const,
  history: (key: string) => ["advanced", "fees", "schedule", key] as const,
  spreadAudit: (filter: string) => ["advanced", "fees", "spread-audit", filter] as const,
  spreadTotals: (assetId: string) => ["advanced", "fees", "spread-totals", assetId] as const,
  preview: (input: string) => ["advanced", "fees", "quote-preview", input] as const,
  quotes: (accountId: string, cursor: string) => [...advancedKeys.account(accountId), "fees", "quotes", cursor] as const,
  quote: (accountId: string, id: string) => [...advancedKeys.account(accountId), "fees", "quote", id] as const,
  bookings: (accountId: string, cursor: string) => [...advancedKeys.account(accountId), "fees", "bookings", cursor] as const,
  booking: (accountId: string, id: string) => [...advancedKeys.account(accountId), "fees", "booking", id] as const,
  periods: (accountId: string, strategy: string, cursor: string) =>
    [...advancedKeys.account(accountId), "fees", "periods", strategy, cursor] as const,
  period: (accountId: string, id: string) => [...advancedKeys.account(accountId), "fees", "period", id] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
/** A recorded mode: LIVE can never be stored, so a record claiming it is refused. */
const RECORDED_MODE = oneOf(PERSISTABLE_MODES);
const SIDE = oneOf(FEE_SIDES);

export const assetRefSpec: Spec = {
  required: { asset_id: "uuid", symbol: "string", decimals: "integer" },
};

export const exactPriceSpec: Spec = {
  required: { mantissa: "quantity", scale: "integer" },
  optional: { source: "string", observed_at: "timestamp" },
};

export const priceRecordSpec: Spec = {
  required: { mantissa: "quantity", scale: "integer", quote_asset: "string", source: "string", at: "timestamp" },
};

export const disclosureSpec: Spec = {
  required: {
    kind: "string",
    schedule_version: "string",
    schedule_hash: "string",
    quote_asset: "uuid",
    notional_at_reference: "quantity",
    notional_at_execution: "quantity",
    total_debit: "quantity",
    total_credit: "quantity",
    worst_case_total: "quantity",
  },
  objects: {
    input: {
      required: true,
      spec: {
        required: {
          side: SIDE,
          base_qty: "quantity",
          base_decimals: "integer",
          quote_decimals: "integer",
          trailing_volume_usd: "usd",
          has_subscription: "boolean",
          strategy_originated: "boolean",
        },
        objects: { reference_price: { required: true, spec: priceRecordSpec } },
      },
    },
    tier: {
      required: true,
      spec: {
        required: { index: "integer", min_trailing_volume_usd: "usd", percentage_fee_bps: "integer", spread_bps: "integer" },
      },
    },
    reference_price: { required: true, spec: priceRecordSpec },
    execution_price: { required: true, spec: priceRecordSpec },
    effective_price: { required: true, spec: priceRecordSpec },
    spread: { required: true, spec: { required: { bps: "integer", amount: "quantity", rounding: "string" } } },
    fees: {
      required: true,
      spec: {
        required: {
          minimum_fee_top_up: "quantity",
          computed_total: "quantity",
          total: "quantity",
          min_fee: "quantity",
          max_total_fee_bps: "integer",
          max_total_fee_bps_amount: "quantity",
          max_total_fee_bps_rounding: "string",
          max_total_fee: "quantity",
          minimum_applied: "boolean",
          max_bps_applied: "boolean",
          max_absolute_applied: "boolean",
        },
        arrays: {
          lines: {
            required: true,
            spec: {
              required: {
                component: "string",
                rate_bps: "integer",
                discount_bps: "integer",
                fixed_amount: "quantity",
                rounding: "string",
                computed: "quantity",
                cap_reduction: "quantity",
                charged: "quantity",
              },
            },
          },
        },
      },
    },
    network: { required: true, spec: { required: { asset: "uuid", amount: "quantity", in_quote_asset: "boolean" } } },
    slippage_reserve: {
      required: true,
      spec: { required: { bps: "integer", amount: "quantity", rounding: "string", held_not_charged: "boolean" } },
    },
    rounding: {
      required: true,
      spec: {
        required: { platform_favourable_allowed: "boolean", any_favours_platform: "boolean" },
        arrays: {
          components: {
            required: true,
            spec: { required: { component: "string", mode: "string", favours_platform: "boolean" } },
          },
        },
      },
    },
  },
};

export const chargesSpec: Spec = {
  required: {
    quote_asset_id: "uuid",
    spread: "quantity",
    platform: "quantity",
    strategy: "quantity",
    venue: "quantity",
    network: "quantity",
    network_in_quote: "boolean",
  },
  optional: { network_asset_id: "uuid" },
};

export const verificationSpec: Spec = {
  required: { verified: "boolean" },
  optional: { code: "string", reason: "string", detail: "string" },
};

export const scheduleSpec: Spec = {
  required: {
    label: "string",
    quote_asset_id: "uuid",
    quote_decimals: "integer",
    spread_bps: "integer",
    fixed_fee: "quantity",
    percentage_fee_bps: "integer",
    venue_pass_through_bps: "integer",
    network_fee_amount: "quantity",
    slippage_reserve_bps: "integer",
    subscription_discount_bps: "integer",
    strategy_fee_bps: "integer",
    max_total_fee_bps: "integer",
    max_total_fee: "quantity",
    min_fee: "quantity",
  },
  optional: { network_fee_asset_id: "uuid" },
  arrays: {
    tiers: {
      required: true,
      spec: {
        required: {
          min_trailing_volume_usd: "usd",
          overrides_percentage_fee: "boolean",
          percentage_fee_bps: "integer",
          overrides_spread: "boolean",
          spread_bps: "integer",
        },
      },
    },
  },
  objects: {
    rounding: {
      required: true,
      spec: {
        required: {
          buy_notional: "string",
          sell_notional: "string",
          spread: "string",
          percentage_fee: "string",
          venue_pass_through: "string",
          strategy_fee: "string",
          fee_cap: "string",
          slippage_reserve: "string",
          allow_platform_favourable: "boolean",
        },
      },
    },
  },
};

export const scheduleVersionSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    key: "string",
    version: "integer",
    schedule_hash: "string",
    effective_from: "timestamp",
    published_at: "timestamp",
    actor_type: "string",
    actor_id: "string",
    reason: "string",
    in_force: "boolean",
  },
  objects: { schedule: { required: true, spec: scheduleSpec }, quote_asset: { required: true, spec: assetRefSpec } },
};

export const scheduleSummarySpec: Spec = {
  required: { key: "string", latest_version: "integer", version_count: "integer" },
  objects: { effective: { spec: scheduleVersionSpec } },
};

export const scheduleSummaryPageSpec: Spec = {
  ...cursorPageSpec(scheduleSummarySpec),
  required: { value_domain: SIMULATED },
};

export const scheduleHistorySpec: Spec = {
  required: { value_domain: SIMULATED, key: "string" },
  optional: { effective_version: "integer" },
  arrays: { versions: { required: true, spec: scheduleVersionSpec } },
};

export const scheduleRefSpec: Spec = {
  required: { id: "uuid", key: "string", version: "integer", label: "string", schedule_hash: "string", effective_from: "timestamp" },
};

export const quotePreviewSpec: Spec = {
  required: { value_domain: SIMULATED, disclosure_hash: "string", recorded: "boolean" },
  objects: {
    schedule: { required: true, spec: scheduleRefSpec },
    disclosure: { required: true, spec: disclosureSpec },
    charges: { required: true, spec: chargesSpec },
  },
};

export const quoteRecordSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    schedule_id: "uuid",
    schedule_key: "string",
    schedule_version: "integer",
    schedule_hash: "string",
    subsystem: "string",
    side: SIDE,
    disclosure_hash: "string",
    quoted_at: "timestamp",
    valid_until: "timestamp",
  },
  optional: { account_id: "uuid", base_asset_id: "uuid" },
  objects: {
    disclosure: { required: true, spec: disclosureSpec },
    charges: { required: true, spec: chargesSpec },
    verification: { spec: verificationSpec },
  },
};

export const quotePageSpec: Spec = { ...cursorPageSpec(quoteRecordSpec), required: { value_domain: SIMULATED } };

export const spreadLineSpec: Spec = {
  required: {
    schedule_key: "string",
    schedule_version: "integer",
    schedule_hash: "string",
    disclosure_hash: "string",
    side: SIDE,
    base_quantity: "quantity",
    spread_bps: "integer",
    spread_amount: "quantity",
    quote_asset_id: "uuid",
  },
  objects: {
    reference_price: { required: true, spec: exactPriceSpec },
    execution_price: { required: true, spec: exactPriceSpec },
  },
};

const bookingFields: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    disclosure_id: "uuid",
    account_id: "uuid",
    mode: RECORDED_MODE,
    booking_mode: oneOf(FEE_BOOKING_MODES),
    journal_kind: "string",
    journal_transaction_id: "uuid",
    effect_id: "uuid",
    status: oneOf(FEE_BOOKING_STATUSES),
    evidence_ref: "string",
    booked_at: "timestamp",
  },
  optional: {
    intent_id: "uuid",
    reversal_journal_transaction_id: "uuid",
    reversal_effect_id: "uuid",
    reversal_reason_code: "string",
    reversed_at: "timestamp",
  },
};

export const bookingSpec: Spec = {
  ...bookingFields,
  objects: { charges: { required: true, spec: chargesSpec }, spread_line: { required: true, spec: spreadLineSpec } },
};

export const bookingDetailSpec: Spec = {
  ...bookingFields,
  objects: {
    charges: { required: true, spec: chargesSpec },
    spread_line: { required: true, spec: spreadLineSpec },
    disclosure: { required: true, spec: disclosureSpec },
    verification: { required: true, spec: verificationSpec },
  },
};

export const bookingPageSpec: Spec = { ...cursorPageSpec(bookingSpec), required: { value_domain: SIMULATED } };

export const spreadChargeSpec: Spec = {
  required: {
    booking_id: "uuid",
    disclosure_id: "uuid",
    account_id: "uuid",
    schedule_key: "string",
    schedule_version: "integer",
    schedule_hash: "string",
    disclosure_hash: "string",
    side: SIDE,
    base_quantity: "quantity",
    spread_bps: "integer",
    spread_amount: "quantity",
    quote_asset_id: "uuid",
    booking_mode: oneOf(FEE_BOOKING_MODES),
    status: oneOf(FEE_BOOKING_STATUSES),
    journal_transaction_id: "uuid",
    booked_at: "timestamp",
  },
  optional: { reversal_journal_transaction_id: "uuid" },
  objects: {
    reference_price: { required: true, spec: exactPriceSpec },
    execution_price: { required: true, spec: exactPriceSpec },
  },
};

export const spreadAuditSpec: Spec = {
  required: { value_domain: SIMULATED },
  arrays: { items: { required: true, spec: spreadChargeSpec } },
};

export const spreadTotalsSpec: Spec = {
  required: { value_domain: SIMULATED, quote_asset_id: "uuid", booked: "quantity", reversed: "quantity" },
};

export const periodTermsSpec: Spec = {
  required: {
    account_ref: "string",
    terms_version: "string",
    period_start: "timestamp",
    period_end: "timestamp",
    opening_nav: "usd",
    prior_hwm: "usd",
    hurdle_annual_bps: "integer",
    fee_rate_bps: "integer",
    crystallization: "string",
    prior_accrued_fee: "usd",
  },
};

export const flowSpec: Spec = {
  required: {
    id: "uuid",
    seq: "integer",
    at: "timestamp",
    amount: "usd",
    nav_before: "usd",
    actor_type: "string",
    actor_id: "string",
  },
};

export const performanceLineSpec: Spec = {
  required: {
    id: "uuid",
    seq: "integer",
    kind: oneOf(FEE_LINE_KINDS),
    at: "timestamp",
    nav: "usd",
    hwm_adjusted: "usd",
    hurdle_threshold: "usd",
    fee_base: "usd",
    amount: "usd",
    accrual_before: "usd",
    accrual_after: "usd",
    effect_id: "uuid",
  },
  optional: { journal_transaction_id: "uuid" },
};

export const statementSpec: Spec = {
  required: {
    kind: "string",
    mode: "string",
    period_start: "timestamp",
    period_end: "timestamp",
    opening_nav: "usd",
    net_flows: "usd",
    closing_nav_gross: "usd",
    gross_pnl: "usd",
    hwm_before: "usd",
    hwm_adjusted: "usd",
    hurdle_annual_bps: "integer",
    hurdle_threshold: "usd",
    fee_base: "usd",
    fee_rate_bps: "integer",
    fee_amount: "usd",
    closing_nav_net: "usd",
    hwm_after: "usd",
    prior_accrued_fee: "usd",
    rounding: "object",
  },
  arrays: {
    flows: {
      required: true,
      spec: {
        required: {
          at: "timestamp",
          amount: "usd",
          nav_before: "usd",
          nav_after: "usd",
          hwm_before: "usd",
          hwm_after: "usd",
        },
      },
    },
    attribution: {
      required: true,
      spec: {
        required: {
          strategy: "string",
          gross_pnl: "usd",
          eligible: "boolean",
          fee_share: "usd",
          remainder_allocated: "usd",
        },
      },
    },
  },
};

export const periodSpec: Spec = {
  required: {
    value_domain: SIMULATED,
    id: "uuid",
    account_id: "uuid",
    strategy_ref: "string",
    seq: "integer",
    mode: RECORDED_MODE,
    fee_asset_id: "uuid",
    terms_hash: "string",
    status: oneOf(FEE_PERIOD_STATUSES),
    accrued: "usd",
  },
  optional: {
    previous_period_id: "uuid",
    last_valuation_at: "timestamp",
    statement_hash: "string",
    crystallization_journal_transaction_id: "uuid",
    crystallization_effect_id: "uuid",
    crystallized_at: "timestamp",
    reversal_journal_transaction_id: "uuid",
    reversal_reason_code: "string",
    reversed_at: "timestamp",
    gross_return_bps: "integer",
    net_return_bps: "integer",
  },
  objects: {
    terms: { required: true, spec: periodTermsSpec },
    statement: { spec: statementSpec },
    verification: { spec: verificationSpec },
  },
  arrays: { flows: { required: true, spec: flowSpec }, lines: { required: true, spec: performanceLineSpec } },
};

export const periodPageSpec: Spec = { ...cursorPageSpec(periodSpec), required: { value_domain: SIMULATED } };

export const flowResultSpec: Spec = {
  required: { value_domain: SIMULATED, period_id: "uuid" },
  objects: { flow: { required: true, spec: flowSpec } },
};

export const lineResultSpec: Spec = {
  required: { value_domain: SIMULATED, period_id: "uuid" },
  objects: { line: { required: true, spec: performanceLineSpec } },
};

/* --------------------------------------------------------------------------
 * Reads
 * ------------------------------------------------------------------------ */

function pageQuery(cursor: string | undefined): { limit: number; cursor?: string } {
  return cursor === undefined ? { limit: FEES_PAGE_SIZE } : { limit: FEES_PAGE_SIZE, cursor };
}

/** Every published schedule key with the version in force now. */
export function useFeeSchedules(cursor: string | undefined): UseQueryResult<CursorPage<FeeScheduleSummary>> {
  return useQuery({
    queryKey: feeKeys.schedules(cursor ?? ""),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/fees/schedules", { params: { query: pageQuery(cursor) } });
      const page = validated<Schemas["FeeScheduleSummaryPage"]>(data, scheduleSummaryPageSpec, "/advanced/fees/schedules");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/** Every version of one schedule key, oldest first. */
export function useFeeScheduleHistory(key: string | undefined): UseQueryResult<FeeScheduleHistory> {
  return useQuery({
    queryKey: feeKeys.history(key ?? ""),
    enabled: key !== undefined && key !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/fees/schedules/history", { params: { query: { key: key ?? "" } } });
      return validated<FeeScheduleHistory>(data, scheduleHistorySpec, "/advanced/fees/schedules/history");
    },
  });
}

export function useFeeQuotes(accountId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<FeeQuoteRecord>> {
  return useQuery({
    queryKey: feeKeys.quotes(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/fees/quotes", {
        params: { query: { account_id: accountId ?? "", ...pageQuery(cursor) } },
      });
      const page = validated<Schemas["FeeQuotePage"]>(data, quotePageSpec, "/advanced/fees/quotes");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/** One stored disclosure, re-proved by the backend; a foreign one answers NOT_FOUND. */
export function useFeeQuote(accountId: string | undefined, disclosureId: string | undefined): UseQueryResult<FeeQuoteRecord> {
  return useQuery({
    queryKey: feeKeys.quote(accountId ?? "", disclosureId ?? ""),
    enabled: disclosureId !== undefined && disclosureId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/fees/quotes/{feeDisclosureId}", {
        params: { path: { feeDisclosureId: disclosureId ?? "" } },
      });
      return validated<FeeQuoteRecord>(data, quoteRecordSpec, "/advanced/fees/quotes/{id}");
    },
  });
}

export function useFeeBookings(accountId: string | undefined, cursor: string | undefined): UseQueryResult<CursorPage<FeeBooking>> {
  return useQuery({
    queryKey: feeKeys.bookings(accountId ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/fees/bookings", {
        params: { query: { account_id: accountId ?? "", ...pageQuery(cursor) } },
      });
      const page = validated<Schemas["FeeBookingPage"]>(data, bookingPageSpec, "/advanced/fees/bookings");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/** One booking with its disclosure, re-proved end to end; a foreign one answers NOT_FOUND. */
export function useFeeBooking(accountId: string | undefined, bookingId: string | undefined): UseQueryResult<FeeBookingDetail> {
  return useQuery({
    queryKey: feeKeys.booking(accountId ?? "", bookingId ?? ""),
    enabled: bookingId !== undefined && bookingId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/fees/bookings/{feeBookingId}", {
        params: { path: { feeBookingId: bookingId ?? "" } },
      });
      return validated<FeeBookingDetail>(data, bookingDetailSpec, "/advanced/fees/bookings/{id}");
    },
  });
}

export function useFeePeriods(
  accountId: string | undefined,
  strategy: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<FeePeriod>> {
  return useQuery({
    queryKey: feeKeys.periods(accountId ?? "", strategy ?? "", cursor ?? ""),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/fees/periods", {
        params: {
          query: {
            account_id: accountId ?? "",
            ...pageQuery(cursor),
            ...(strategy === undefined || strategy === "" ? {} : { strategy_ref: strategy }),
          },
        },
      });
      const page = validated<Schemas["FeePeriodPage"]>(data, periodPageSpec, "/advanced/fees/periods");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/** One period, a crystallized one re-proved by the backend; a foreign one answers NOT_FOUND. */
export function useFeePeriod(accountId: string | undefined, periodId: string | undefined): UseQueryResult<FeePeriod> {
  return useQuery({
    queryKey: feeKeys.period(accountId ?? "", periodId ?? ""),
    enabled: periodId !== undefined && periodId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/fees/periods/{feePeriodId}", {
        params: { path: { feePeriodId: periodId ?? "" } },
      });
      return validated<FeePeriod>(data, periodSpec, "/advanced/fees/periods/{id}");
    },
  });
}

export interface SpreadAuditFilter {
  readonly quoteAssetId: string | undefined;
  readonly accountId: string | undefined;
  readonly since: string | undefined;
  readonly until: string | undefined;
  readonly includeReversed: boolean;
}

/** The operator's spread audit: every charged spread the filter selects, oldest first. */
export function useSpreadAudit(filter: SpreadAuditFilter | undefined): UseQueryResult<FeeSpreadAudit> {
  return useQuery({
    queryKey: feeKeys.spreadAudit(JSON.stringify(filter ?? {})),
    enabled: filter !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const f = filter ?? { quoteAssetId: undefined, accountId: undefined, since: undefined, until: undefined, includeReversed: false };
      const { data } = await api.GET("/advanced/fees/spread-audit", {
        params: {
          query: {
            limit: SPREAD_AUDIT_LIMIT,
            include_reversed: f.includeReversed,
            ...(f.quoteAssetId === undefined ? {} : { quote_asset_id: f.quoteAssetId }),
            ...(f.accountId === undefined ? {} : { account_id: f.accountId }),
            ...(f.since === undefined ? {} : { since: f.since }),
            ...(f.until === undefined ? {} : { until: f.until }),
          },
        },
      });
      return validated<FeeSpreadAudit>(data, spreadAuditSpec, "/advanced/fees/spread-audit");
    },
  });
}

export function useSpreadTotals(quoteAssetId: string | undefined): UseQueryResult<FeeSpreadTotals> {
  return useQuery({
    queryKey: feeKeys.spreadTotals(quoteAssetId ?? ""),
    enabled: quoteAssetId !== undefined && quoteAssetId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/fees/spread-totals", {
        params: { query: { quote_asset_id: quoteAssetId ?? "" } },
      });
      return validated<FeeSpreadTotals>(data, spreadTotalsSpec, "/advanced/fees/spread-totals");
    },
  });
}

/**
 * Prices an order under the schedule in force: a read, so it records,
 * reserves and charges nothing and carries no idempotency key. The answer is
 * the fee engine's verified disclosure; the page shows it line by line and
 * computes nothing itself. Undefined input asks nothing.
 */
export function useQuotePreview(input: FeeQuoteInput | undefined): UseQueryResult<FeeQuotePreview> {
  return useQuery({
    queryKey: feeKeys.preview(JSON.stringify(input ?? {})),
    enabled: input !== undefined,
    // Priced at the instant of the read (marks and schedules move): refetched, never reused.
    staleTime: 0,
    retry: retryTransient,
    queryFn: async () => {
      const q = input as FeeQuoteInput;
      const { data } = await api.GET("/advanced/fees/quote-preview", {
        params: {
          query: {
            schedule_key: q.schedule_key,
            side: q.side,
            base_quantity: q.base_quantity,
            base_decimals: q.base_decimals,
            reference_price: q.reference_price,
            ...(q.at === undefined ? {} : { at: q.at }),
            ...(q.trailing_volume_usd === undefined ? {} : { trailing_volume_usd: q.trailing_volume_usd }),
            ...(q.has_subscription === undefined ? {} : { has_subscription: q.has_subscription }),
            ...(q.strategy_originated === undefined ? {} : { strategy_originated: q.strategy_originated }),
          },
        },
      });
      return validated<FeeQuotePreview>(data, quotePreviewSpec, "/advanced/fees/quote-preview");
    },
  });
}

/* --------------------------------------------------------------------------
 * Commands (the operator's; the backend asks a recent strong sign-in of those
 * that publish, reverse or charge)
 * ------------------------------------------------------------------------ */

/** A mutation of one confirmed attempt; the attempt carries its idempotency key. */
export type FeeCommand<R, T> = UseMutationResult<T, unknown, ConfirmedAttempt<R>>;

/** Every fee command can change any account's fee reads, and the published schedules. */
function useFeeCommand<R, T>(run: (attempt: ConfirmedAttempt<R>) => Promise<T>): FeeCommand<R, T> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: run,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: advancedKeys.all });
    },
  });
}

export function usePublishSchedule(): FeeCommand<FeePublishRequest, FeeScheduleVersion> {
  return useFeeCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/fees/schedules", { ...idempotent(idempotencyKey), body: request });
    return validated<FeeScheduleVersion>(data, scheduleVersionSpec, "/advanced/fees/schedules");
  });
}

export function useRecordQuote(): FeeCommand<FeeQuoteRecordRequest, FeeQuoteRecord> {
  return useFeeCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/fees/quotes", { ...idempotent(idempotencyKey), body: request });
    return validated<FeeQuoteRecord>(data, quoteRecordSpec, "/advanced/fees/quotes");
  });
}

export interface ReverseRequest {
  readonly id: string;
  readonly reasonCode: string;
  readonly reason: string;
}

export function useReverseBooking(): FeeCommand<ReverseRequest, FeeBooking> {
  return useFeeCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/fees/bookings/{feeBookingId}/reverse", {
      ...idempotent(idempotencyKey, { path: { feeBookingId: request.id } }),
      body: { reason_code: request.reasonCode, reason: request.reason },
    });
    return validated<FeeBooking>(data, bookingSpec, "/advanced/fees/bookings/{id}/reverse");
  });
}

export function useOpenPeriod(): FeeCommand<FeeOpenPeriodRequest, FeePeriod> {
  return useFeeCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/fees/periods", { ...idempotent(idempotencyKey), body: request });
    return validated<FeePeriod>(data, periodSpec, "/advanced/fees/periods");
  });
}

export interface FlowRequest {
  readonly periodId: string;
  readonly at: string;
  /** A deposit is positive, a withdrawal negative (USD). */
  readonly amount: string;
  readonly navBefore: string;
}

export function useRecordFlow(): FeeCommand<FlowRequest, FeeFlowResult> {
  return useFeeCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/fees/periods/{feePeriodId}/flows", {
      ...idempotent(idempotencyKey, { path: { feePeriodId: request.periodId } }),
      body: { at: request.at, amount: request.amount, nav_before: request.navBefore },
    });
    return validated<FeeFlowResult>(data, flowResultSpec, "/advanced/fees/periods/{id}/flows");
  });
}

export interface AccrueRequest {
  readonly periodId: string;
  readonly at: string;
  readonly nav: string;
}

export function useAccruePeriod(): FeeCommand<AccrueRequest, FeeLineResult> {
  return useFeeCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/fees/periods/{feePeriodId}/accruals", {
      ...idempotent(idempotencyKey, { path: { feePeriodId: request.periodId } }),
      body: { at: request.at, nav: request.nav },
    });
    return validated<FeeLineResult>(data, lineResultSpec, "/advanced/fees/periods/{id}/accruals");
  });
}

export interface CrystallizeRequest {
  readonly periodId: string;
  readonly closingNav: string;
  readonly contributions: readonly FeeContribution[];
}

export function useCrystallizePeriod(): FeeCommand<CrystallizeRequest, FeePeriod> {
  return useFeeCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/fees/periods/{feePeriodId}/crystallize", {
      ...idempotent(idempotencyKey, { path: { feePeriodId: request.periodId } }),
      body: {
        closing_nav: request.closingNav,
        ...(request.contributions.length === 0 ? {} : { contributions: [...request.contributions] }),
      },
    });
    return validated<FeePeriod>(data, periodSpec, "/advanced/fees/periods/{id}/crystallize");
  });
}

export interface CancelPeriodRequest {
  readonly periodId: string;
  readonly reason: string;
}

export function useCancelPeriod(): FeeCommand<CancelPeriodRequest, FeePeriod> {
  return useFeeCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/fees/periods/{feePeriodId}/cancel", {
      ...idempotent(idempotencyKey, { path: { feePeriodId: request.periodId } }),
      body: { reason: request.reason },
    });
    return validated<FeePeriod>(data, periodSpec, "/advanced/fees/periods/{id}/cancel");
  });
}

export function useReversePeriod(): FeeCommand<ReverseRequest, FeePeriod> {
  return useFeeCommand(async ({ request, idempotencyKey }) => {
    const { data } = await api.POST("/advanced/fees/periods/{feePeriodId}/reverse", {
      ...idempotent(idempotencyKey, { path: { feePeriodId: request.id } }),
      body: { reason_code: request.reasonCode, reason: request.reason },
    });
    return validated<FeePeriod>(data, periodSpec, "/advanced/fees/periods/{id}/reverse");
  });
}
