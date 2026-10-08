/**
 * The words for a holding the backend cannot value in USD
 * (GET /accounts/{accountId}/holdings).
 *
 * A holding is VALUED, and carries its USD mark, the price reference the mark
 * came from and its unrealized profit and loss; or it is UNVALUED, carries
 * none of them, and says why. The interface draws the reason where the figure
 * would be. It never draws a zero in its place: a zero is a value, and an
 * unvalued holding has none (src/lib/honesty.ts: nothing unavailable is shown
 * as a zero).
 *
 * The enumerations repeat the values the OpenAPI contract declares, because
 * the runtime validator needs them as values; each is tied to its generated
 * type at compile time, so a contract change fails `tsc` here.
 */
import type { Schemas } from "@controlplane/generated-client";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

export type HoldingValuationStatus = Schemas["HoldingValuationStatus"];
export const HOLDING_VALUATION_STATUSES = ["VALUED", "UNVALUED"] as const;
export type HoldingValuationStatusesMatchContract = Assert<
  Same<(typeof HOLDING_VALUATION_STATUSES)[number], HoldingValuationStatus>
>;

export type HoldingUnvaluedReason = Schemas["HoldingUnvaluedReason"];
export const HOLDING_UNVALUED_REASONS = ["SIMULATED_VALUE", "NO_FRESH_MARK", "NO_ASSET_POLICY", "ASSET_EXCLUDED"] as const;
export type HoldingUnvaluedReasonsMatchContract = Assert<
  Same<(typeof HOLDING_UNVALUED_REASONS)[number], HoldingUnvaluedReason>
>;

/** What each reason means, in this interface's words. The backend decides which one applies. */
export const UNVALUED_REASON_COPY: Readonly<Record<HoldingUnvaluedReason, { readonly label: string; readonly detail: string }>> = {
  SIMULATED_VALUE: {
    label: "Simulated value",
    detail:
      "This is a sandbox asset with no economic value, so it has no value in US dollars and none is shown. " +
      "The advanced portfolio states it in the sandbox dollar instead.",
  },
  NO_FRESH_MARK: {
    label: "No current price",
    detail:
      "The backend has no price for this asset recent enough for its policy, so it does not value it. " +
      "No older price is used in its place.",
  },
  NO_ASSET_POLICY: {
    label: "No valuation policy",
    detail: "No asset policy is in force for this asset, so the backend does not value it.",
  },
  ASSET_EXCLUDED: {
    label: "Excluded from valuation",
    detail:
      "The asset's policy excludes it from valuation: the asset, or the stablecoin it is, is halted, or its " +
      "status is not one the policy values.",
  },
};

/**
 * The statuses with which buying power reports an underlying balance it did not
 * value (GET /accounts/{accountId}/buying-power): its usd_value is then a
 * placeholder 0.00, not a valuation, and its price_ref is empty.
 */
export const BUYING_POWER_UNVALUED_STATUSES = ["HALTED", "STALE_PRICE", "POLICY_MISSING"] as const;

/** Whether buying power valued an underlying balance; its usd_value is a figure only then. */
export function balanceIsValued(status: string): boolean {
  return !(BUYING_POWER_UNVALUED_STATUSES as readonly string[]).includes(status);
}

/** The note for a USD figure that cannot exist because the holding is unvalued. */
export function unvaluedFigureNote(reason: HoldingUnvaluedReason, figure: "mark" | "unrealized" | "basis" | "price"): string {
  if (reason === "SIMULATED_VALUE") return "none: simulated value has no US dollar figure";
  switch (figure) {
    case "mark":
      return "not valued";
    case "unrealized":
      return "none without a valuation";
    case "price":
      return "no price used";
    case "basis":
      return "not reported";
  }
}
