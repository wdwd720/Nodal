/**
 * The words this product is allowed to use about money, and the disclosures it
 * is required to show (PART 112).
 *
 * Everything a customer reads about a balance, a stablecoin, a simulated
 * result or a model score comes from here, so the constraints are enforceable
 * rather than aspirational: `honesty.test.ts` scans the whole source tree and
 * `e2e/honesty.spec.ts` scans every rendered page for the phrasings the goal
 * document forbids. A convention nobody checks is a convention that decays.
 *
 * The four absolutes:
 *
 *   1. A stablecoin is never described as money in a bank. It is USDC, a token
 *      on a chain, and the interface says which chain and which mint.
 *   2. A simulated or shadow result is never presented as though it came from
 *      trading real capital.
 *   3. Nothing is ever described as assured, promised or without risk.
 *   4. A model's confidence score is never presented as the chance of making
 *      money. Every place a score appears, the denial appears with it.
 */

/** Settlement asset naming. Never "balance in dollars", never the four-letter word. */
export const USDC_SHORT = "USDC";
export const USDC_FULL = "USDC (a US-dollar stablecoin token)";

/**
 * The standing disclosure for the settlement asset. Shown wherever a customer
 * could otherwise read a dollar figure as money held at a bank.
 */
export const USDC_DISCLOSURE =
  "Balances are held as USDC, a US-dollar stablecoin token on a blockchain — not a bank deposit " +
  "and not insured. USD figures are a valuation of that token at the price reference shown, and " +
  "the token can trade away from one dollar.";

/** Shown next to the USD valuation of any non-stablecoin holding. */
export const USD_VALUATION_NOTE =
  "USD figures are a valuation computed by the backend from the price reference shown. They are " +
  "an estimate of what the position is worth, not an amount held in dollars.";

/** Shown wherever settlement has not finished. Pending settlement is never hidden. */
export const PENDING_SETTLEMENT_NOTE =
  "Amounts still settling are shown separately and are not spendable until the backend marks them " +
  "available. A provider saying a payment succeeded does not move money here; observed settlement " +
  "on chain does.";

/** Attached to every figure that did not come from trading real capital. */
export const SIMULATED_RESULTS_NOTICE =
  "These figures come from simulated or observation-only runs. No real capital was committed and " +
  "no real order reached a venue, so they are not a record of what this account earned or lost.";

/** Attached to every model score. The denial travels with the number. */
export const CONFIDENCE_DISCLAIMER =
  "A confidence score is the model's own rating of its prediction. It is not the chance of making " +
  "money, and a high score does not make a favourable outcome any more assured.";

/** The one-line risk statement carried in the footer of every page. */
export const RISK_FOOTER =
  "Trading digital assets can lose money, including everything committed. Nothing here is advice, " +
  "a promise or an assurance of any outcome.";

/** Execution modes as the API reports them, each with an honest description. */
export const MODE_DESCRIPTIONS: Readonly<Record<string, string>> = {
  BACKTEST: "Replayed against historical data. No order was placed and no capital moved.",
  PAPER: "Simulated end to end. The order is recorded but never reaches a venue and no capital moves.",
  SHADOW: "Decided alongside real activity for comparison, but never submitted. No capital moved.",
  CANARY: "Real capital, deliberately small, used to prove a path before it is opened wider.",
  LIMITED: "Real capital under a tightened envelope.",
  LIVE: "Real capital at a real venue.",
};

/** True when a mode commits real capital. Drives the labelling of every result. */
export function modeUsesRealCapital(mode: string): boolean {
  return mode === "CANARY" || mode === "LIMITED" || mode === "LIVE";
}

/** Short badge text for a mode, used in tables and headers. */
export function modeBadge(mode: string): string {
  return modeUsesRealCapital(mode) ? `${mode} — real capital` : `${mode} — simulated`;
}

/**
 * Copy for the states the API uses to say "this deployment cannot do that".
 * Each one names the reason and what, if anything, the customer can do; none of
 * them renders a zero or a dash that could be mistaken for a real figure.
 */
export const UNAVAILABLE_COPY = {
  providerUnavailable: {
    title: "Not available right now",
    body:
      "The backend answered PROVIDER_UNAVAILABLE: the provider this needs is not reachable or not " +
      "configured in this deployment. Nothing is being estimated in its place.",
  },
  unsupported: {
    title: "Not offered in this deployment",
    body:
      "The backend answered UNSUPPORTED: this capability is not part of the API this deployment " +
      "exposes. It is not disabled for your account specifically.",
  },
  capabilityNotApproved: {
    title: "Waiting on approval",
    body:
      "The backend answered CAPABILITY_NOT_APPROVED: the capability gate for this action has not " +
      "been approved and activated, so the action is refused for everyone, not only for you.",
  },
  stepUpRequired: {
    title: "Stronger sign-in needed",
    body:
      "The backend answered STEP_UP_REQUIRED. Sensitive actions need a recent strong authentication " +
      "on this session before the backend will accept them.",
  },
  killSwitch: {
    title: "Stopped by a kill switch",
    body:
      "The backend answered KILL_SWITCH_ACTIVE. An operator has stopped new risk of this kind. " +
      "Existing positions are untouched by the switch itself.",
  },
  noApi: {
    title: "No API for this yet",
    body:
      "The v1 API this app is built against exposes no endpoint for this. Rather than draw a screen " +
      "from figures that are not real, the app says so and shows only what the API does return.",
  },
} as const;

/**
 * Every stage of the lifecycle the activity timeline must show, in order, with
 * the API `kind` that carries it. A stage with no rows is drawn as absent, not
 * as empty: "no eligibility decision was recorded" is information.
 */
export const LIFECYCLE_STAGES: ReadonlyArray<{
  readonly kind: string;
  readonly label: string;
  readonly description: string;
}> = [
  { kind: "DATA_EVENT", label: "Data event", description: "An observation arrived — a price, an on-chain event, a webhook." },
  { kind: "PREDICTION", label: "Prediction", description: "A model committed a prediction before it could see the outcome." },
  { kind: "INTENT", label: "Intent", description: "A typed request to change exposure was recorded." },
  { kind: "ELIGIBILITY", label: "Eligibility", description: "Jurisdiction, account state and asset restrictions were evaluated." },
  { kind: "RISK", label: "Risk", description: "Limits and the capital envelope were evaluated against the request." },
  { kind: "PLAN", label: "Plan", description: "A settlement plan was compiled and capital was reserved." },
  { kind: "EXECUTION", label: "Execution", description: "The plan was submitted to a venue." },
  { kind: "FILL", label: "Fill", description: "The venue reported an execution and the chain confirmed it." },
  { kind: "RECONCILIATION", label: "Reconciliation", description: "Expected and observed state were compared." },
];

/** Extra kinds the activity feed can carry that sit outside the trading lifecycle. */
export const AUXILIARY_ACTIVITY_KINDS: Readonly<Record<string, string>> = {
  LEDGER: "Ledger posting",
  FUNDING: "Funding",
  SECURITY: "Security",
};

/* --------------------------------------------------------------------------
 * The Nodal-native economy (gola.md PARTS LII, LIV)
 * ------------------------------------------------------------------------ */

/**
 * PART LII's rule, and the reason the interface is shaped the way it is:
 *
 *   Nodal Economy, Simulated Capital and Real Capital are three different
 *   things and the interface NEVER sums them.
 *
 * A single "total balance" would be the most convenient number on the page and
 * the most dishonest: Credits cannot be withdrawn, simulated capital does not
 * exist, and only real capital is money. Adding them produces a figure that is
 * true of nothing.
 */
export const THREE_POTS_NOTE =
  "Nodal Economy, Simulated Capital and Real Capital are shown separately and are never added " +
  "together. They are different kinds of value: Credits are usable inside Nodal, simulated capital " +
  "is a record of what would have happened, and only real capital is money.";

/** What Credits are, said plainly wherever a Credit figure appears. */
export const CREDITS_DISCLOSURE =
  "Credits are an internal balance for use inside Nodal. They are not money, not a deposit and not " +
  "redeemable for money unless this deployment has an approved payout path, which the payouts page " +
  "states for your account.";

/**
 * PART LIV: prices on a native market are quoted in Credits, never converted
 * to a currency, unless an approved external redemption value exists. Showing
 * "$0.024" where the truth is "2.4 Credits" invents an exchange rate nobody
 * approved.
 */
export const NATIVE_PRICE_NOTE =
  "Prices on internal markets are quoted in Credits. They are not converted to a currency: no " +
  "approved external value for a Credit exists in this deployment, so any currency figure would be " +
  "an exchange rate nobody set.";

/** The standing risk statement for user-created assets and their markets. */
export const NATIVE_ASSET_RISK =
  "A Nodal-native asset is created by a user, not by Nodal. Its price is set by a formula against a " +
  "shared pool, it can fall to nearly nothing, and there is no obligation on anyone to buy it back. " +
  "Nodal does not review it as an investment and nothing here is advice.";

/** Shown before a creator publishes an asset (PART LIII). */
export const CREATE_ASSET_IMMUTABILITY =
  "Supply, creator allocation, symbol, fees and the market formula are fixed at launch and cannot " +
  "be changed afterwards, by you or by Nodal. Read them before you publish.";

/** What an earning's provenance means to somebody looking at their Credits. */
export const PROVENANCE_NOTE =
  "Credits are tracked by where they came from. What may be paid out — if anything — depends on " +
  "that origin, not on the total, which is why the breakdown is shown instead of one number.";

/** Shown wherever a payout is refused because nothing is approved yet. */
export const PAYOUT_NOT_APPROVED =
  "No payout path is approved in this deployment. That is a decision about the product, not about " +
  "your account, and no amount of Credits changes it.";

/**
 * The standing sentence a sandbox tier owes every page that shows a figure.
 *
 * `GET /v1/version` answers `sandbox_tier`; nothing infers it from an
 * environment name. It lives here, in one place, because it was written once on
 * `/markets` and nowhere else, and the result was that the same market moved by
 * the same amount read as the internal economy on Home and as a simulation two
 * clicks away. A temperature that changes with the page is not a temperature.
 */
export const SANDBOX_TIER_NOTE =
  "This deployment is a sandbox tier, so every figure on this page is simulated. Nothing here is " +
  "anybody's money and no trade on it moves value anywhere.";
