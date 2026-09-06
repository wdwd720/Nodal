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
