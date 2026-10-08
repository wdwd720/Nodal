/**
 * Reads of the advanced financial core (ADR-0020): every advanced capability
 * with its gate decision per mode, the canonical instruments, an account's
 * economic intents and effects, Risk Kernel V2 decisions and buying power V2.
 *
 * This file is the pattern every advanced subsystem's hooks follow
 * (`src/api/advanced/<sub>.ts`):
 *
 *   - every response is checked against a spec before a component sees it:
 *     money is an exact decimal string (`usd`) or exact base units
 *     (`quantity`), enums are closed (`oneOf`), required fields are present,
 *     and a response that reports value must say `value_domain: SIMULATED`.
 *     Anything else is refused as a ContractViolation, never repaired;
 *   - query keys start with `advancedKeys.all`, and account-scoped ones with
 *     `advancedKeys.account(accountId)`, so one account's rows can never be
 *     shown under another account's heading, and a mutation can invalidate
 *     everything it may have changed for an account with one key;
 *   - a 4xx answer is the backend's decision, not a hiccup, and is not retried.
 */
import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { ApiProblem, type Schemas } from "@controlplane/generated-client";

import {
  ADVANCED_MODES,
  BUYING_POWER_V2_FIELDS,
  CORPORATE_ACTION_BEHAVIORS,
  EFFECT_STATUSES,
  EXERCISE_STYLES,
  INSTRUMENT_KINDS,
  MARGIN_MODELS,
  OPTION_RIGHTS,
  PERSISTABLE_MODES,
  RISK_VERDICTS,
  TRANSFERABILITIES,
  VALUE_DOMAIN_SIMULATED,
  duplicatedModes,
  type InstrumentKind,
} from "../../lib/advanced.ts";
import { api } from "../client.ts";
import { ContractViolation, cursorPageSpec, oneOf, validated, type FieldKind, type Spec } from "../contract.ts";

export type AdvancedCapabilityList = Schemas["AdvancedCapabilityList"];
export type AdvancedCapability = Schemas["AdvancedCapability"];
export type AdvancedModeDecision = Schemas["AdvancedModeDecision"];
export type AdvancedInstrument = Schemas["AdvancedInstrument"];
export type AdvancedInstrumentDetail = Schemas["AdvancedInstrumentDetail"];
export type InstrumentTerms = Schemas["InstrumentTerms"];
export type InstrumentTermsVersion = Schemas["InstrumentTermsVersion"];
export type EconomicIntent = Schemas["EconomicIntent"];
export type EconomicEffect = Schemas["EconomicEffect"];
export type AdvancedRiskDecision = Schemas["AdvancedRiskDecision"];
export type BuyingPowerV2 = Schemas["BuyingPowerV2"];
export type BuyingPowerV2Line = Schemas["BuyingPowerV2Line"];
export type BuyingPowerV2Reason = Schemas["BuyingPowerV2Reason"];

/** One page of a cursor-paged list, with the backend's own cursor. */
export interface CursorPage<T> {
  readonly items: readonly T[];
  /** Null when the backend says there is no further page. */
  readonly nextCursor: string | null;
}

/** Rows per page of the intents and effects lists. */
export const ADVANCED_PAGE_SIZE = 25;
/** Risk decisions have no cursor: the newest this many are read. */
export const RISK_DECISION_LIMIT = 50;
/** The instrument list has no cursor either, so the contract's maximum is asked for. */
export const INSTRUMENT_LIST_LIMIT = 200;

/* --------------------------------------------------------------------------
 * Query keys
 * ------------------------------------------------------------------------ */

export const advancedKeys = {
  all: ["advanced"] as const,
  capabilities: ["advanced", "capabilities"] as const,
  instruments: (kind: string, limit: number) => ["advanced", "instruments", kind, limit] as const,
  instrument: (instrumentId: string) => ["advanced", "instrument", instrumentId] as const,
  /** Prefix of every account-scoped advanced read. */
  account: (accountId: string) => ["advanced", "account", accountId] as const,
  intents: (accountId: string, cursor: string, limit: number) =>
    ["advanced", "account", accountId, "intents", cursor, limit] as const,
  intent: (accountId: string, intentId: string) => ["advanced", "account", accountId, "intent", intentId] as const,
  effects: (accountId: string, cursor: string, limit: number) =>
    ["advanced", "account", accountId, "effects", cursor, limit] as const,
  riskDecisions: (accountId: string, limit: number) =>
    ["advanced", "account", accountId, "risk-decisions", limit] as const,
  buyingPower: (accountId: string) => ["advanced", "account", accountId, "buying-power"] as const,
};

/**
 * A 4xx answer (NOT_FOUND, FORBIDDEN, VALIDATION_FAILED, a refusal) is the
 * backend's decision, and asking again cannot change it; neither can a
 * response that failed its contract. Only transport and server faults are
 * worth another attempt.
 */
export function retryTransient(failureCount: number, error: Error): boolean {
  if (error instanceof ApiProblem && error.status >= 400 && error.status < 500) return false;
  if (error instanceof ContractViolation) return false;
  return failureCount < 2;
}

/** The backend's stable code for a failed read, when it sent one. */
export function problemCode(error: unknown): string | undefined {
  return error instanceof ApiProblem ? error.code : undefined;
}

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const MODE = oneOf(ADVANCED_MODES);
const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);

export const modeDecisionSpec: Spec = {
  required: { mode: MODE, allowed: "boolean" },
  lists: { reasons: { required: true, kind: "string" } },
};

export const capabilitySpec: Spec = {
  required: {
    capability: "string",
    high_risk: "boolean",
    live_implemented: "boolean",
    min_approvers: "integer",
    max_live_validity_seconds: "integer",
    max_live_capital: "usd",
    provider_readiness: "string",
    gate_active: "boolean",
    kill_switched: "boolean",
  },
  lists: {
    simulated_modes: { required: true, kind: MODE },
    ungated_environments: { required: true, kind: "string" },
    live_evidence: { required: true, kind: "string" },
    jurisdictions: { required: true, kind: "string" },
  },
  arrays: { decisions: { required: true, spec: modeDecisionSpec } },
};

export const capabilityListSpec: Spec = {
  required: { environment: "string", value_domain: SIMULATED },
  arrays: { items: { required: true, spec: capabilitySpec } },
};

export const instrumentTermsSpec: Spec = {
  required: {
    kind: oneOf(INSTRUMENT_KINDS),
    symbol: "string",
    venue_code: "string",
    settlement_asset_id: "uuid",
    contract_multiplier: "quantity",
    price_scale: "integer",
    tick_size: "quantity",
    lot_size: "quantity",
    min_quantity: "quantity",
    quantity_decimals: "integer",
    // An enum in the contract, checked as a string here: one of its two
    // tokens is a word this interface may not spell. See PHYSICAL_SETTLEMENT
    // in lib/advanced.ts, which proves the two-value shape at compile time.
    settlement_style: "string",
    trading_calendar: "string",
    oracle_max_age_ms: "integer",
    margin_model: oneOf(MARGIN_MODELS),
    corporate_action_behavior: oneOf(CORPORATE_ACTION_BEHAVIORS),
    transferability: oneOf(TRANSFERABILITIES),
  },
  optional: {
    exchange_mic: "string",
    base_asset_id: "uuid",
    quote_asset_id: "uuid",
    underlying_instrument_id: "uuid",
    expiry: "timestamp",
    strike_mantissa: "quantity",
    option_right: oneOf(OPTION_RIGHTS),
    exercise_style: oneOf(EXERCISE_STYLES),
    funding_interval_ms: "integer",
  },
  lists: {
    jurisdiction_tags: { required: true, kind: "string" },
    oracle_sources: { required: true, kind: "string" },
    outcomes: { kind: "string" },
  },
};

export const advancedInstrumentSpec: Spec = {
  required: {
    id: "uuid",
    kind: oneOf(INSTRUMENT_KINDS),
    canonical_name: "string",
    status: "string",
    risk_class: "string",
    terms_version: "integer",
    terms_hash: "string",
  },
  objects: { terms: { required: true, spec: instrumentTermsSpec } },
};

export const advancedInstrumentListSpec: Spec = {
  arrays: { items: { required: true, spec: advancedInstrumentSpec } },
};

export const termsVersionSpec: Spec = {
  required: { version: "integer", terms_hash: "string", reason: "string", actor_type: "string", created_at: "timestamp" },
};

export const advancedInstrumentDetailSpec: Spec = {
  ...advancedInstrumentSpec,
  arrays: { versions: { required: true, spec: termsVersionSpec } },
};

export const economicIntentSpec: Spec = {
  required: {
    id: "uuid",
    account_id: "uuid",
    kind: "string",
    // Not every AdvancedMode: an economic intent cannot be stored LIVE, so one
    // that says it is LIVE is refused rather than drawn under a notice saying
    // nothing here is live.
    mode: oneOf(PERSISTABLE_MODES),
    capability: "string",
    actor_type: "string",
    terms: "object",
    content_hash: "string",
    status: "string",
    created_at: "timestamp",
    updated_at: "timestamp",
  },
  optional: {
    agent_id: "uuid",
    strategy_ref: "string",
    instrument_id: "uuid",
    asset_id: "uuid",
    quantity: "quantity",
    notional_usd: "usd",
    rejection_code: "string",
    terminal_at: "timestamp",
  },
};

export const economicEffectSpec: Spec = {
  required: {
    id: "uuid",
    effect_key: "string",
    subsystem: "string",
    effect_kind: "string",
    posts_ledger: "boolean",
    status: oneOf(EFFECT_STATUSES),
    created_at: "timestamp",
    updated_at: "timestamp",
  },
  optional: {
    intent_id: "uuid",
    account_id: "uuid",
    journal_transaction_id: "uuid",
    compensation_journal_transaction_id: "uuid",
    external_ref: "string",
    evidence_ref: "string",
    evidence_sha256: "string",
    failure_code: "string",
  },
};

export const riskDecisionSpec: Spec = {
  required: {
    id: "uuid",
    intent_kind: "string",
    stage: "string",
    verdict: oneOf(RISK_VERDICTS),
    input_hash: "string",
    policy_version: "string",
    policy_hash: "string",
    evaluator_version: "string",
    decision_hash: "string",
    evaluated_at: "timestamp",
  },
  optional: { intent_id: "uuid" },
  lists: { reason_codes: { required: true, kind: "string" } },
};

export const riskDecisionListSpec: Spec = {
  arrays: { items: { required: true, spec: riskDecisionSpec } },
};

const buyingPowerV2Figures: Record<string, FieldKind> = {};
for (const field of BUYING_POWER_V2_FIELDS) buyingPowerV2Figures[field] = "usd";

export const buyingPowerV2Spec: Spec = {
  required: {
    value_domain: SIMULATED,
    policy_version: "string",
    as_of: "timestamp",
    // Every component the page shows, each an exact two-decimal string.
    ...buyingPowerV2Figures,
  },
  arrays: {
    reasons: { required: true, spec: { required: { code: "string", detail: "string", amount: "usd" } } },
    lines: {
      required: true,
      spec: {
        required: { field: "string", component: "string", amount_usd: "usd", detail: "string" },
        optional: { asset_id: "string" },
      },
    },
  },
};

/**
 * The capability list, checked: the spec, and then the one rule a per-field
 * spec cannot state — a capability reports each mode at most once. Two
 * decisions for one mode would leave the table to pick one silently.
 */
export function checkCapabilityList(value: unknown): AdvancedCapabilityList {
  const list = validated<AdvancedCapabilityList>(value, capabilityListSpec, "/advanced/capabilities");
  list.items.forEach((item, index) => {
    const repeated = duplicatedModes(item.decisions);
    if (repeated.length > 0) {
      throw new ContractViolation(
        `/advanced/capabilities.items[${String(index)}].decisions`,
        `more than one decision for ${repeated.join(", ")}`,
      );
    }
  });
  return list;
}

/* --------------------------------------------------------------------------
 * Hooks
 * ------------------------------------------------------------------------ */

export function useAdvancedCapabilities(): UseQueryResult<AdvancedCapabilityList> {
  return useQuery({
    queryKey: advancedKeys.capabilities,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/capabilities", {});
      return checkCapabilityList(data);
    },
  });
}

export function useAdvancedInstruments(kind: InstrumentKind | undefined): UseQueryResult<AdvancedInstrument[]> {
  return useQuery({
    queryKey: advancedKeys.instruments(kind ?? "", INSTRUMENT_LIST_LIMIT),
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/instruments", {
        params: {
          query: kind === undefined ? { limit: INSTRUMENT_LIST_LIMIT } : { kind, limit: INSTRUMENT_LIST_LIMIT },
        },
      });
      return validated<Schemas["AdvancedInstrumentList"]>(data, advancedInstrumentListSpec, "/advanced/instruments")
        .items;
    },
  });
}

export function useAdvancedInstrument(instrumentId: string | undefined): UseQueryResult<AdvancedInstrumentDetail> {
  return useQuery({
    queryKey: advancedKeys.instrument(instrumentId ?? ""),
    enabled: instrumentId !== undefined && instrumentId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/instruments/{instrumentId}", {
        params: { path: { instrumentId: instrumentId ?? "" } },
      });
      return validated<AdvancedInstrumentDetail>(data, advancedInstrumentDetailSpec, "/advanced/instruments/{id}");
    },
  });
}

export function useEconomicIntents(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<EconomicIntent>> {
  return useQuery({
    queryKey: advancedKeys.intents(accountId ?? "", cursor ?? "", ADVANCED_PAGE_SIZE),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/intents", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit: ADVANCED_PAGE_SIZE,
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["EconomicIntentPage"]>(data, cursorPageSpec(economicIntentSpec), "/advanced/intents");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

/**
 * One intent. The key carries the account the area is reading so it sits
 * with that account's other rows; the backend decides from the session
 * whether the intent may be read, and answers NOT_FOUND when it may not.
 */
export function useEconomicIntent(
  accountId: string | undefined,
  intentId: string | undefined,
): UseQueryResult<EconomicIntent> {
  return useQuery({
    queryKey: advancedKeys.intent(accountId ?? "", intentId ?? ""),
    enabled: intentId !== undefined && intentId !== "",
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/intents/{intentId}", {
        params: { path: { intentId: intentId ?? "" } },
      });
      return validated<EconomicIntent>(data, economicIntentSpec, "/advanced/intents/{id}");
    },
  });
}

export function useEconomicEffects(
  accountId: string | undefined,
  cursor: string | undefined,
): UseQueryResult<CursorPage<EconomicEffect>> {
  return useQuery({
    queryKey: advancedKeys.effects(accountId ?? "", cursor ?? "", ADVANCED_PAGE_SIZE),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/effects", {
        params: {
          query: {
            account_id: accountId ?? "",
            limit: ADVANCED_PAGE_SIZE,
            ...(cursor === undefined ? {} : { cursor }),
          },
        },
      });
      const page = validated<Schemas["EconomicEffectPage"]>(data, cursorPageSpec(economicEffectSpec), "/advanced/effects");
      return { items: page.items, nextCursor: page.next_cursor };
    },
  });
}

export function useAdvancedRiskDecisions(accountId: string | undefined): UseQueryResult<AdvancedRiskDecision[]> {
  return useQuery({
    queryKey: advancedKeys.riskDecisions(accountId ?? "", RISK_DECISION_LIMIT),
    enabled: accountId !== undefined,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/risk/decisions", {
        params: { query: { account_id: accountId ?? "", limit: RISK_DECISION_LIMIT } },
      });
      return validated<Schemas["AdvancedRiskDecisionList"]>(data, riskDecisionListSpec, "/advanced/risk/decisions")
        .items;
    },
  });
}

export function useBuyingPowerV2(accountId: string | undefined): UseQueryResult<BuyingPowerV2> {
  return useQuery({
    queryKey: advancedKeys.buyingPower(accountId ?? ""),
    enabled: accountId !== undefined,
    // Computed by the backend on every call and never treated as cached truth
    // (PART 25): a figure is refetched, not reused.
    staleTime: 0,
    refetchOnWindowFocus: true,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/buying-power", {
        params: { query: { account_id: accountId ?? "" } },
      });
      return validated<BuyingPowerV2>(data, buyingPowerV2Spec, "/advanced/buying-power");
    },
  });
}
