/**
 * An account's portfolio in the advanced (sandbox) area: one read of
 * everything it holds in the SIMULATED domain (GET /advanced/portfolio).
 *
 * The answer carries other subsystems' own reads unchanged -- buying power V2,
 * the margin account, derivative positions, prediction market positions,
 * equity listings and settlements, book orders, risk decisions, economic
 * effects -- so each embedded part is checked with that subsystem's own spec,
 * and the whole with the rules a per-field spec cannot state: a section
 * carries its answer exactly when it is PRESENT and its problem exactly when
 * it is UNAVAILABLE; a valuation carries a value exactly when it is not
 * UNVALUED, and a mark exactly when it is MARKED. Anything else is refused as
 * a ContractViolation, never repaired.
 */
import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import type { Schemas } from "@controlplane/generated-client";

import { VALUE_DOMAIN_SIMULATED } from "../../lib/advanced.ts";
import {
  PORTFOLIO_LEDGER_CODES,
  PORTFOLIO_SECTION_STATES,
  RESERVATION_PURPOSES,
  VALUATION_STATUSES,
} from "../../lib/portfolio.ts";
import { api } from "../client.ts";
import { ContractViolation, oneOf, validated, type Spec } from "../contract.ts";
import { advancedKeys, buyingPowerV2Spec, economicEffectSpec, retryTransient, riskDecisionSpec } from "./core.ts";
import { capabilityPageSpec, swapPageSpec, withdrawalPageSpec } from "./delegation.ts";
import { checkPositionPage } from "./derivatives.ts";
import { checkSettlement, equityListingSpec, equitySettlementPageSpec } from "./equities.ts";
import { periodPageSpec } from "./fees.ts";
import { marginAccountDetailSpec } from "./margin.ts";
import { bookOrderPageSpec } from "./orders.ts";
import { positionListSpec, predictionMarketSpec } from "./predmarket.ts";

export type Portfolio = Schemas["AdvancedPortfolio"];
export type PortfolioBalance = Schemas["AdvancedPortfolioBalance"];
export type PortfolioValuation = Schemas["AdvancedPortfolioValuation"];
export type PortfolioProblem = Schemas["AdvancedPortfolioProblem"];
export type PortfolioPositions = Schemas["AdvancedPortfolioPositions"];
export type PortfolioRisk = Schemas["AdvancedPortfolioRisk"];
export type PortfolioReconciliationRecord = Schemas["AdvancedPortfolioReconciliationRecord"];
export type PortfolioPredictionHolding = Schemas["AdvancedPortfolioPredictionHolding"];
export type PortfolioEquityHolding = Schemas["AdvancedPortfolioEquityHolding"];
export type PortfolioDelegation = Schemas["AdvancedPortfolioDelegation"];
export type PortfolioPerformanceFees = Schemas["AdvancedPortfolioPerformanceFees"];

export const portfolioKeys = {
  /** The portfolio sits with the account's other rows, so anything that invalidates the account refreshes it. */
  portfolio: (accountId: string) => [...advancedKeys.account(accountId), "portfolio"] as const,
};

/* --------------------------------------------------------------------------
 * Contract specs
 * ------------------------------------------------------------------------ */

const SIMULATED = oneOf([VALUE_DOMAIN_SIMULATED]);
const STATE = oneOf(PORTFOLIO_SECTION_STATES);

export const portfolioProblemSpec: Spec = { required: { code: "string", detail: "string" } };

export const portfolioMarkSpec: Spec = {
  required: {
    price: "decimal",
    mantissa: "quantity",
    scale: "integer",
    source: "string",
    observed_at: "timestamp",
    age_ms: "integer",
  },
};

export const portfolioValuationSpec: Spec = {
  required: { status: oneOf(VALUATION_STATUSES) },
  optional: { value: "quantity", owed_value: "quantity" },
  objects: { mark: { spec: portfolioMarkSpec }, problem: { spec: portfolioProblemSpec } },
};

const ledgerBalanceSpec: Spec = { required: { code: oneOf(PORTFOLIO_LEDGER_CODES), quantity: "quantity" } };

export const portfolioBalanceSpec: Spec = {
  required: {
    asset_id: "uuid",
    symbol: "string",
    name: "string",
    chain: "string",
    decimals: "integer",
    wallet: "quantity",
    reserved: "quantity",
    withdrawal_holds: "quantity",
    available: "quantity",
    total: "quantity",
    value_domain: SIMULATED,
  },
  arrays: {
    reservations: {
      required: true,
      spec: {
        required: { purpose: oneOf(RESERVATION_PURPOSES), quantity: "quantity", reservations: "integer" },
        optional: { capability: "string" },
      },
    },
    held_elsewhere: { required: true, spec: ledgerBalanceSpec },
    owed: { required: true, spec: ledgerBalanceSpec },
  },
  objects: { valuation: { required: true, spec: portfolioValuationSpec } },
};

/** A section envelope: its state, the problem of a refusal, and whatever payload it carries. */
function sectionSpec(payload: NonNullable<Spec["objects"]>, extra: NonNullable<Spec["required"]> = {}): Spec {
  return {
    required: { state: STATE, value_domain: SIMULATED, ...extra },
    objects: { problem: { spec: portfolioProblemSpec }, ...payload },
  };
}

export const portfolioReconciliationRecordSpec: Spec = {
  required: {
    id: "uuid",
    kind: "string",
    status: "string",
    material: "boolean",
    blocks_new_risk: "boolean",
    scope_type: "string",
    opened_at: "timestamp",
  },
  optional: { asset_id: "uuid" },
};

export const portfolioSpec: Spec = {
  required: {
    account_id: "uuid",
    as_of: "timestamp",
    quote_asset_id: "uuid",
    quote_symbol: "string",
    quote_decimals: "integer",
    quote_provisioned: "boolean",
    max_mark_age_ms: "integer",
    value_domain: SIMULATED,
  },
  arrays: { balances: { required: true, spec: portfolioBalanceSpec } },
  objects: {
    buying_power: { required: true, spec: sectionSpec({ buying_power: { spec: buyingPowerV2Spec } }) },
    positions: {
      required: true,
      spec: {
        objects: {
          margin: { required: true, spec: sectionSpec({ account: { spec: marginAccountDetailSpec } }) },
          // The page itself is held to the derivatives section's own checks below.
          derivatives: { required: true, spec: sectionSpec({ positions: { spec: { required: { book_status: "string" } } } }) },
          prediction_markets: {
            required: true,
            spec: {
              ...sectionSpec({}, { truncated: "boolean" }),
              arrays: {
                markets: {
                  required: true,
                  spec: {
                    objects: {
                      market: { required: true, spec: predictionMarketSpec },
                      positions: { required: true, spec: positionListSpec },
                    },
                  },
                },
              },
            },
          },
          equities: {
            required: true,
            spec: {
              ...sectionSpec({ pending_settlements: { spec: equitySettlementPageSpec } }, { listings_truncated: "boolean" }),
              arrays: {
                holdings: {
                  required: true,
                  spec: {
                    required: { settled: "quantity", unsettled: "quantity" },
                    objects: { listing: { required: true, spec: equityListingSpec } },
                  },
                },
              },
            },
          },
          order_books: { required: true, spec: sectionSpec({ orders: { spec: bookOrderPageSpec } }) },
        },
      },
    },
    delegation: {
      required: true,
      spec: sectionSpec({
        capabilities: { spec: capabilityPageSpec },
        swaps_awaiting_approval: { spec: swapPageSpec },
        withdrawals_awaiting_approval: { spec: withdrawalPageSpec },
      }),
    },
    performance_fees: { required: true, spec: sectionSpec({ periods: { spec: periodPageSpec } }) },
    risk: {
      required: true,
      spec: {
        required: {
          account_status: "string",
          blocks_new_risk: "boolean",
          reconciliation_truncated: "boolean",
          unknown_effects_truncated: "boolean",
        },
        objects: {
          decisions: {
            required: true,
            spec: {
              required: { state: STATE },
              objects: { problem: { spec: portfolioProblemSpec } },
              arrays: { items: { required: true, spec: riskDecisionSpec } },
            },
          },
        },
        arrays: {
          reconciliation: { required: true, spec: portfolioReconciliationRecordSpec },
          unknown_effects: { required: true, spec: economicEffectSpec },
        },
      },
    },
  },
};

/* --------------------------------------------------------------------------
 * The rules a per-field spec cannot state
 * ------------------------------------------------------------------------ */

/**
 * A section carries its answer exactly when PRESENT and its problem exactly
 * when UNAVAILABLE; NOT_CONFIGURED carries neither, and so does NONE, which
 * only a section that may say "nothing here" (the margin account) can be.
 */
export function checkSectionShape(
  section: { readonly state: string; readonly problem?: unknown },
  hasPayload: boolean,
  path: string,
  allowsNone: boolean,
): void {
  const hasProblem = section.problem !== undefined;
  const ok =
    (section.state === "PRESENT" && hasPayload && !hasProblem) ||
    (section.state === "UNAVAILABLE" && !hasPayload && hasProblem) ||
    (section.state === "NOT_CONFIGURED" && !hasPayload && !hasProblem) ||
    (section.state === "NONE" && allowsNone && !hasPayload && !hasProblem);
  if (!ok) {
    throw new ContractViolation(`${path}.state`, `${section.state} does not match what the section carries`);
  }
}

/** A valuation carries a value unless UNVALUED, a mark exactly when MARKED, and a problem exactly when UNVALUED. */
export function checkValuationShape(v: PortfolioValuation, path: string): void {
  const value = v.value !== undefined;
  const mark = v.mark !== undefined;
  const problem = v.problem !== undefined;
  const ok =
    (v.status === "PAR" && value && !mark && !problem) ||
    (v.status === "MARKED" && value && mark && !problem) ||
    (v.status === "UNVALUED" && !value && !mark && problem && v.owed_value === undefined);
  if (!ok) {
    throw new ContractViolation(`${path}.status`, `${v.status} does not match what the valuation carries`);
  }
}

/** The portfolio, checked: the spec, every embedded read by its own subsystem's rules, and the shapes above. */
export function checkPortfolio(value: unknown, path = "/advanced/portfolio"): Portfolio {
  const p = validated<Portfolio>(value, portfolioSpec, path);
  p.balances.forEach((b, index) => {
    checkValuationShape(b.valuation, `${path}.balances[${String(index)}].valuation`);
  });
  checkSectionShape(p.buying_power, p.buying_power.buying_power !== undefined, `${path}.buying_power`, false);

  const pos = p.positions;
  const at = `${path}.positions`;
  checkSectionShape(pos.margin, pos.margin.account !== undefined, `${at}.margin`, true);
  checkSectionShape(pos.derivatives, pos.derivatives.positions !== undefined, `${at}.derivatives`, false);
  if (pos.derivatives.positions !== undefined) {
    checkPositionPage(pos.derivatives.positions, `${at}.derivatives.positions`);
  }

  const pm = pos.prediction_markets;
  checkSectionShape(pm, pm.state === "PRESENT", `${at}.prediction_markets`, false);
  if (pm.state !== "PRESENT" && (pm.markets.length > 0 || pm.truncated)) {
    throw new ContractViolation(`${at}.prediction_markets.markets`, `a ${pm.state} section lists no markets`);
  }
  pm.markets.forEach((m, index) => {
    if (m.positions.market_id !== m.market.id) {
      throw new ContractViolation(`${at}.prediction_markets.markets[${String(index)}].positions.market_id`, "positions of another market");
    }
  });

  const eq = pos.equities;
  checkSectionShape(eq, eq.pending_settlements !== undefined, `${at}.equities`, false);
  if (eq.state !== "PRESENT" && (eq.holdings.length > 0 || eq.listings_truncated)) {
    throw new ContractViolation(`${at}.equities.holdings`, `a ${eq.state} section lists no holdings`);
  }
  eq.pending_settlements?.items.forEach((s, index) => {
    checkSettlement(s, `${at}.equities.pending_settlements.items[${String(index)}]`);
  });

  checkSectionShape(pos.order_books, pos.order_books.orders !== undefined, `${at}.order_books`, false);

  // The delegated authority carries all three of its pages or none of them:
  // half of what waits for the owner is never shown as if it were all of it.
  const d = p.delegation;
  const pages = [d.capabilities, d.swaps_awaiting_approval, d.withdrawals_awaiting_approval];
  const all = pages.every((page) => page !== undefined);
  if (all !== pages.some((page) => page !== undefined)) {
    throw new ContractViolation(`${path}.delegation`, "the section carries some of its pages but not all of them");
  }
  checkSectionShape(d, all, `${path}.delegation`, false);
  checkSectionShape(p.performance_fees, p.performance_fees.periods !== undefined, `${path}.performance_fees`, false);

  const decisions = p.risk.decisions;
  checkSectionShape(decisions, decisions.state === "PRESENT", `${path}.risk.decisions`, false);
  if (decisions.state !== "PRESENT" && decisions.items.length > 0) {
    throw new ContractViolation(`${path}.risk.decisions.items`, `a ${decisions.state} section lists no decisions`);
  }
  return p;
}

/* --------------------------------------------------------------------------
 * The read
 * ------------------------------------------------------------------------ */

export function usePortfolio(accountId: string | undefined): UseQueryResult<Portfolio> {
  return useQuery({
    queryKey: portfolioKeys.portfolio(accountId ?? ""),
    enabled: accountId !== undefined,
    // Read by the backend for every request and never treated as cached
    // truth (PART 25): a figure is fetched again, not reused.
    staleTime: 0,
    refetchOnWindowFocus: true,
    retry: retryTransient,
    queryFn: async () => {
      const { data } = await api.GET("/advanced/portfolio", { params: { query: { account_id: accountId ?? "" } } });
      return checkPortfolio(data);
    },
  });
}
