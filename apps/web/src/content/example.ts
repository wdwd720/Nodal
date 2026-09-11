/**
 * The figures the public site's product screenshots are drawn from.
 *
 * The goal forbids images of invented dashboards, so the public pages render
 * the **real components** — `Panel`, `Figure`, `DataTable`, `Field` — against
 * these values. What a visitor sees on the marketing site is therefore the same
 * code, the same formatter and the same typography they will see when they sign
 * in, which is the only kind of product screenshot that cannot become a lie
 * when the product changes.
 *
 * Three rules make that honest rather than merely convenient:
 *
 *   1. every value here is a **string**, exactly as the API returns money, so
 *      nothing is rounded on its way to the screen and no float exists;
 *   2. every surface that renders them declares `data-temp="simulated"`, which
 *      the design system draws with a dashed border, a hatch, desaturated
 *      figures and a persistent SIMULATED chip that takes no prop to suppress
 *      it — and the copy beside it says "example" in words;
 *   3. the numbers are unremarkable on purpose. A marketing screenshot showing
 *      a large gain is a fake metric with extra steps, so the example account
 *      is small, its day is mixed, and one of its markets is down.
 *
 * They live in `src/content/` rather than in a page because
 * `src/lib/source-scan.test.ts` refuses a money-shaped literal anywhere under
 * `src/pages/` or `src/components/` — a hardcoded figure in a page is a fake
 * balance waiting to be mistaken for a real one. Keeping them here keeps that
 * guard absolute for every page that renders a customer's own money, and puts
 * the example data somewhere it is named for what it is.
 */

export interface ExampleBalance {
  readonly label: string;
  readonly note: string;
  readonly decimal: string;
}

/** The Credits panel a customer sees on Home, with a frozen bucket in it. */
export const EXAMPLE_CREDITS: readonly ExampleBalance[] = [
  {
    label: "Spendable",
    note: "What can be committed to a trade right now.",
    decimal: "1840.250000",
  },
  {
    label: "Frozen",
    note: "Held against a payment the card network has not finished settling.",
    decimal: "120.000000",
  },
  {
    label: "Payout-eligible",
    note: "Eligible by origin, if a payout path is ever active. Not a dollar figure.",
    decimal: "0.000000",
  },
];

/** The total the three buckets came from. Never summed in the browser. */
export const EXAMPLE_CREDITS_GROSS = "1960.250000";

export interface ExampleMarketRow {
  readonly id: string;
  readonly name: string;
  readonly symbol: string;
  /** Last price, in Credits, at the asset's own price scale. */
  readonly price: string;
  /** Twenty-four hour change, as a percentage. Signed by the formatter. */
  readonly change: string;
  /** Credits that actually back the curve. */
  readonly reserve: string;
  readonly status: "OPEN" | "HALTED";
}

/**
 * A market list. One market is halted, because a list where everything is open
 * teaches a visitor that markets are always open, and they are not.
 */
export const EXAMPLE_MARKETS: readonly ExampleMarketRow[] = [
  {
    id: "asset-01",
    name: "Ravensbourne Index",
    symbol: "RVB",
    price: "2.4180",
    change: "1.94",
    reserve: "48200.000000",
    status: "OPEN",
  },
  {
    id: "asset-02",
    name: "Tideline",
    symbol: "TIDE",
    price: "0.3125",
    change: "-4.60",
    reserve: "12750.000000",
    status: "OPEN",
  },
  {
    id: "asset-03",
    name: "Longacre Yield Note",
    symbol: "LNGA",
    price: "11.0400",
    change: "0.00",
    reserve: "96010.000000",
    status: "HALTED",
  },
];

/** The price scale the example markets quote at. */
export const EXAMPLE_PRICE_DECIMALS = 4;

export interface ExampleAgentRow {
  readonly name: string;
  readonly authority: string;
  readonly budget: string;
  readonly used: string;
  readonly state: "ACTIVE" | "PAUSED";
}

/**
 * Agents at the two levels this product actually permits. Levels four to six
 * are refused by policy and the public page says so rather than showing them.
 */
export const EXAMPLE_AGENTS: readonly ExampleAgentRow[] = [
  {
    name: "Reserve watcher",
    authority: "1 — observe",
    budget: "0.000000",
    used: "0.000000",
    state: "ACTIVE",
  },
  {
    name: "Tideline rebalance",
    authority: "3 — execute within limits",
    budget: "500.000000",
    used: "212.400000",
    state: "PAUSED",
  },
];

/** The sentence every example surface carries beside the SIMULATED chip. */
export const EXAMPLE_LABEL =
  "Example data, not a live account. This is the product's own interface rendered from fixed " +
  "values so that what you see here is what the application actually draws.";
