/**
 * Pure helpers for the portfolio page of the advanced (sandbox) area.
 *
 * Nothing here computes a financial figure. These functions name the states,
 * purposes and ledger codes the backend reports, pick out of its answer the
 * rows that are capital not yet free to use, and write a leverage the margin
 * engine stated in basis points by moving a decimal point in its digits --
 * never by dividing. Every amount stays the exact string the backend sent
 * (PART 110).
 *
 * The enumerations repeat values the OpenAPI contract declares, because the
 * runtime validator needs them as values; each is tied to its generated type
 * at compile time, so a contract change fails `tsc` here rather than slipping
 * past the validator.
 */
import type { Schemas } from "@controlplane/generated-client";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

export type PortfolioSectionState = Schemas["AdvancedPortfolioSectionState"];
export const PORTFOLIO_SECTION_STATES = ["PRESENT", "NONE", "NOT_CONFIGURED", "UNAVAILABLE"] as const;
export type PortfolioSectionStatesMatchContract = Assert<
  Same<(typeof PORTFOLIO_SECTION_STATES)[number], PortfolioSectionState>
>;

export type ReservationPurpose = Schemas["AdvancedPortfolioReservationPurpose"];
export const RESERVATION_PURPOSES = [
  "OPEN_ORDERS",
  "PENDING_TRANSFERS",
  "BRIDGE_LOCKS",
  "STRATEGY",
  "WITHDRAWALS",
  "OTHER",
] as const;
export type ReservationPurposesMatchContract = Assert<Same<(typeof RESERVATION_PURPOSES)[number], ReservationPurpose>>;

export type PortfolioLedgerCode = Schemas["AdvancedPortfolioLedgerCode"];
export const PORTFOLIO_LEDGER_CODES = [
  "MARGIN_COLLATERAL",
  "DERIVATIVES_MARGIN",
  "UNSETTLED_RECEIVABLE",
  "BRIDGE_IN_FLIGHT",
  "MARGIN_LOAN",
  "DEFICIT",
] as const;
export type PortfolioLedgerCodesMatchContract = Assert<Same<(typeof PORTFOLIO_LEDGER_CODES)[number], PortfolioLedgerCode>>;

export type ValuationStatus = Schemas["AdvancedPortfolioValuationStatus"];
export const VALUATION_STATUSES = ["PAR", "MARKED", "UNVALUED"] as const;
export type ValuationStatusesMatchContract = Assert<Same<(typeof VALUATION_STATUSES)[number], ValuationStatus>>;

/** The address of a page of the advanced area, as path segments under /advanced. */
export type SectionRoute = readonly string[];

/* --------------------------------------------------------------------------
 * Section states
 * ------------------------------------------------------------------------ */

export type Tone = "neutral" | "good" | "warn" | "bad" | "info";

export function sectionStateTone(state: string): Tone {
  switch (state) {
    case "PRESENT":
      return "good";
    case "UNAVAILABLE":
      return "bad";
    case "NOT_CONFIGURED":
      return "warn";
    default:
      return "neutral";
  }
}

/**
 * What a section that carries no answer says instead, for a subsystem named
 * `subsystem`. A PRESENT section shows its answer and needs no words here.
 */
export function sectionStateCopy(state: string, subsystem: string): { readonly title: string; readonly body: string } {
  switch (state) {
    case "NOT_CONFIGURED":
      return {
        title: `${subsystem} is not configured in this deployment`,
        body: `This deployment does not run ${subsystem.toLowerCase()}, so there is nothing of it to read. That is not the same as holding nothing there, and nothing is shown in its place.`,
      };
    case "NONE":
      return {
        title: `No ${subsystem.toLowerCase()} on this account`,
        body: `The backend reports that this account has no ${subsystem.toLowerCase()}.`,
      };
    case "UNAVAILABLE":
      return {
        title: `${subsystem} could not be read`,
        body: "The subsystem's own read refused, with the code below. The rest of the portfolio is unaffected, and no figure is drawn in place of this one.",
      };
    default:
      return { title: subsystem, body: "" };
  }
}

/* --------------------------------------------------------------------------
 * Reservations and ledger codes
 * ------------------------------------------------------------------------ */

export interface PurposeCopy {
  readonly label: string;
  readonly meaning: string;
  /** The page where what holds the reservation is shown. */
  readonly route: SectionRoute;
}

export const PURPOSE_COPY: Readonly<Record<ReservationPurpose, PurposeCopy>> = {
  OPEN_ORDERS: {
    label: "Open orders",
    meaning: "Held for orders that have not filled: on the internal books, equity orders, derivative opens and paper fills.",
    route: ["order-books", "orders"],
  },
  PENDING_TRANSFERS: {
    label: "Pending transfers",
    meaning: "Held for transfers to other customers that have not completed.",
    route: ["transfers"],
  },
  BRIDGE_LOCKS: {
    label: "Bridge locks",
    meaning: "Held to be locked on this chain for a bridge leg that has not left it yet.",
    route: ["bridge", "legs"],
  },
  STRATEGY: {
    label: "Strategy",
    meaning: "Held for a sandboxed strategy's activity.",
    route: ["strategies"],
  },
  WITHDRAWALS: {
    label: "Withdrawals",
    meaning: "Held for a delegated withdrawal that has not completed.",
    route: ["activity"],
  },
  OTHER: {
    label: "Other",
    meaning: "Held by another capability's operation that has not completed; its capability is named beside it.",
    route: ["activity"],
  },
};

export interface LedgerCodeCopy {
  readonly label: string;
  readonly meaning: string;
  readonly route: SectionRoute;
}

export const LEDGER_CODE_COPY: Readonly<Record<PortfolioLedgerCode, LedgerCodeCopy>> = {
  MARGIN_COLLATERAL: {
    label: "Margin collateral",
    meaning: "Posted to the margin account.",
    route: ["margin"],
  },
  DERIVATIVES_MARGIN: {
    label: "Derivatives margin",
    meaning: "Posted as margin for derivative positions.",
    route: ["derivatives", "positions"],
  },
  UNSETTLED_RECEIVABLE: {
    label: "Awaiting settlement",
    meaning: "What an equity trade delivers on its settlement date; never withdrawable before it.",
    route: ["equities", "settlements"],
  },
  BRIDGE_IN_FLIGHT: {
    label: "Bridge in flight",
    meaning: "Left its chain on a bridge leg and not yet released on the other; counted at the bridge haircut until final.",
    route: ["bridge", "legs"],
  },
  MARGIN_LOAN: {
    label: "Margin loan",
    meaning: "Borrowed from the lending pool, with capitalised interest.",
    route: ["margin"],
  },
  DEFICIT: {
    label: "Covered deficit",
    meaning: "What a liquidation left owing, covered by the insurance fund and still to be repaid.",
    route: ["margin"],
  },
};

/** A label for a code the backend sent, its own code when it is not one this page knows. */
export function ledgerCodeLabel(code: string): string {
  return (LEDGER_CODE_COPY as Readonly<Record<string, LedgerCodeCopy>>)[code]?.label ?? code;
}

export function purposeLabel(purpose: string): string {
  return (PURPOSE_COPY as Readonly<Record<string, PurposeCopy>>)[purpose]?.label ?? purpose;
}

/* --------------------------------------------------------------------------
 * Pending capital
 * ------------------------------------------------------------------------ */

/** True when an exact base-unit string is zero, judged on its digits alone. */
export function isZeroQuantity(value: string): boolean {
  return /^-?0+$/.test(value);
}

type Balance = Schemas["AdvancedPortfolioBalance"];
type PredictionHolding = Schemas["AdvancedPortfolioPredictionHolding"];

export type PendingKind = "RESERVED" | "WITHDRAWAL_HOLD" | "HELD" | "HELD_PAYOUT";

/** One amount of an asset the account has but cannot use freely yet, and why. */
export interface PendingCapitalRow {
  readonly key: string;
  readonly kind: PendingKind;
  readonly assetId: string;
  readonly symbol: string;
  readonly chain: string;
  readonly decimals: number;
  readonly label: string;
  /** The capability or code the backend named, when there is one. */
  readonly code: string | undefined;
  readonly quantity: string;
  readonly route: SectionRoute;
}

/** The ledger codes whose balances are capital still on its way: not collateral, not debt. */
const PENDING_CODES: ReadonlySet<string> = new Set(["UNSETTLED_RECEIVABLE", "BRIDGE_IN_FLIGHT"]);

/**
 * The account's capital that is not yet free to use, row by row, exactly as
 * the backend reported it: every reservation by purpose, every withdrawal
 * hold, every balance waiting for settlement or in flight between chains,
 * and every payout a prediction market holds for the account and it has not
 * claimed. Nothing is added up: each row is one figure the backend sent, in
 * its own asset.
 */
export function pendingCapital(
  balances: readonly Balance[],
  markets: readonly PredictionHolding[],
): PendingCapitalRow[] {
  const rows: PendingCapitalRow[] = [];
  for (const b of balances) {
    const base = { assetId: b.asset_id, symbol: b.symbol, chain: b.chain, decimals: b.decimals };
    b.reservations.forEach((r, index) => {
      rows.push({
        ...base,
        key: `${b.asset_id}:reserved:${String(index)}`,
        kind: "RESERVED",
        label: purposeLabel(r.purpose),
        code: r.capability,
        quantity: r.quantity,
        route: (PURPOSE_COPY as Readonly<Record<string, PurposeCopy>>)[r.purpose]?.route ?? ["activity"],
      });
    });
    if (!isZeroQuantity(b.withdrawal_holds)) {
      rows.push({
        ...base,
        key: `${b.asset_id}:holds`,
        kind: "WITHDRAWAL_HOLD",
        label: "Withdrawal holds",
        code: undefined,
        quantity: b.withdrawal_holds,
        route: ["activity"],
      });
    }
    for (const h of b.held_elsewhere) {
      if (!PENDING_CODES.has(h.code)) continue;
      rows.push({
        ...base,
        key: `${b.asset_id}:held:${h.code}`,
        kind: "HELD",
        label: ledgerCodeLabel(h.code),
        code: h.code,
        quantity: h.quantity,
        route: LEDGER_CODE_COPY[h.code].route,
      });
    }
  }
  for (const m of markets) {
    const held = m.positions.held_payout;
    if (held === undefined || held.claimed) continue;
    rows.push({
      key: `${m.market.id}:held-payout`,
      kind: "HELD_PAYOUT",
      assetId: m.market.collateral_asset_id,
      symbol: m.market.collateral_symbol,
      chain: "",
      decimals: m.market.collateral_decimals,
      label: `Held payout of ${m.market.symbol}`,
      code: held.kind,
      quantity: held.amount,
      route: ["prediction-markets", m.market.id],
    });
  }
  return rows;
}

/* --------------------------------------------------------------------------
 * Leverage
 * ------------------------------------------------------------------------ */

/**
 * A leverage the margin engine reported in basis points (10000 = 1x), written
 * as a multiple by moving the decimal point four places: 28654 is "2.8654x".
 * Trailing zeros of the fraction are dropped; nothing is rounded.
 */
export function formatLeverageBps(bps: number): string {
  if (!Number.isSafeInteger(bps) || bps < 0) return "unreadable leverage";
  const digits = String(bps).padStart(5, "0");
  const whole = digits.slice(0, digits.length - 4);
  const fraction = digits.slice(digits.length - 4).replace(/0+$/, "");
  return fraction === "" ? `${whole}x` : `${whole}.${fraction}x`;
}
