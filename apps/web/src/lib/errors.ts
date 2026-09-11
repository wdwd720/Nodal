/**
 * The fifteen things that go wrong, said the same way every time.
 *
 * `docs/product/USER_JOURNEY.md` §11 lists the situations this product must be
 * able to speak about, and goal §33 says each needs a fixed sentence and a
 * meaningful recovery action. This module is that list. A page that hits one of
 * these looks it up rather than writing a sentence of its own, so the same
 * refusal reads identically on Home, on the ticket and on the withdrawal page —
 * and so that changing the wording is one edit rather than a search.
 *
 * Two rules shape it:
 *
 *   1. **A situation is not always a code.** "Payment pending", "balance
 *      updating", "verification pending" and "payout delayed" are *states* the
 *      backend reports in a resource, not problem documents. They are in this
 *      list because a customer meets them, and a page renders them from the
 *      state it read. `codes` is therefore allowed to be empty, and
 *      `situationForCode` simply will not find those.
 *   2. **Never a raw provider or server message.** The backend's `detail` may
 *      be shown *beside* the sentence — `explain()` already does that — but the
 *      sentence itself is ours, because a provider's exception text is written
 *      for an engineer and often names something the customer cannot act on.
 *
 * The codes come from `internal/errs/codes.go`, which is the authoritative
 * list; `src/api/problem.ts` turns a thrown value into one of them. This module
 * deliberately imports neither the API client nor React, so the unit test that
 * proves every listed situation has a sentence runs on plain `node --test`.
 */

/** The situations §11 requires, plus the step-up §10 requires. */
export type SituationId =
  | "provider-unavailable"
  | "payment-pending"
  | "payment-failed"
  | "balance-updating"
  | "trade-rejected"
  | "insufficient-credits"
  | "price-changed"
  | "market-paused"
  | "account-restricted"
  | "verification-pending"
  | "verification-rejected"
  | "withdrawal-unavailable"
  | "payout-delayed"
  | "session-expired"
  | "network-offline"
  | "step-up-required"
  | "request-changed";

/**
 * What the customer should do next.
 *
 * `kind` is what the page has to wire up, not decoration: `retry` needs a
 * button that re-runs the same request, `sign-in` and `step-up` need the
 * round trip with a return path, `wait` explicitly needs NO control, because a
 * button that cannot change the answer is a dead control.
 */
export type RecoveryKind = "retry" | "requote" | "sign-in" | "step-up" | "wait" | "go" | "none";

export interface Recovery {
  readonly kind: RecoveryKind;
  /** The label of the control, when there is one. */
  readonly label: string;
  /** Where `go` goes. */
  readonly to?: string;
}

export interface Situation {
  readonly id: SituationId;
  /** The name §11 uses. Also the heading a page shows. */
  readonly name: string;
  /** The fixed sentence. One or two sentences, no more. */
  readonly sentence: string;
  readonly recovery: Recovery;
  /** The problem codes that mean this. Empty when the situation is a state. */
  readonly codes: readonly string[];
}

export const SITUATIONS: readonly Situation[] = [
  {
    id: "provider-unavailable",
    name: "Provider unavailable",
    sentence:
      "The provider this needs is not reachable right now, so nothing was attempted. Your balance " +
      "and your positions are unchanged.",
    recovery: { kind: "retry", label: "Try again" },
    codes: ["PROVIDER_UNAVAILABLE", "VENUE_UNAVAILABLE", "MODEL_UNAVAILABLE", "AT_CAPACITY"],
  },
  {
    id: "payment-pending",
    name: "Payment pending",
    sentence:
      "Your payment is with the card provider and has not been confirmed yet. Credits appear once " +
      "the provider confirms it; nothing is charged twice if you wait.",
    recovery: { kind: "wait", label: "" },
    codes: ["IDEMPOTENCY_IN_PROGRESS"],
  },
  {
    id: "payment-failed",
    name: "Payment failed",
    sentence:
      "The card provider declined this payment, so no Credits were created and nothing was " +
      "charged. The provider's reason is shown below where it gave one.",
    recovery: { kind: "retry", label: "Try a different card" },
    codes: [],
  },
  {
    id: "balance-updating",
    name: "Balance updating",
    sentence:
      "The payment succeeded and the Credits are being posted to the ledger. The figure updates " +
      "itself when the ledger confirms; it is not computed in your browser.",
    recovery: { kind: "wait", label: "" },
    codes: [],
  },
  {
    id: "trade-rejected",
    name: "Trade rejected",
    sentence:
      "The backend refused this order against the limits in force at the moment it was evaluated. " +
      "Nothing was filled and no Credits moved.",
    recovery: { kind: "requote", label: "Take a new quote" },
    codes: [
      "NO_VALID_PLAN",
      "VENUE_LIQUIDITY_INSUFFICIENT",
      "RISK_MAX_POSITION",
      "RISK_DAILY_LOSS",
      "RISK_CONCENTRATION",
    ],
  },
  {
    id: "insufficient-credits",
    name: "Insufficient Credits",
    sentence:
      "There are not enough spendable Credits for this. Spendable excludes anything frozen or " +
      "already committed, which is why it can be lower than the total you see.",
    recovery: { kind: "go", label: "Buy Credits", to: "/buy-credits" },
    codes: ["INSUFFICIENT_BUYING_POWER", "LEDGER_NEGATIVE_BALANCE", "BUDGET_EXHAUSTED"],
  },
  {
    id: "price-changed",
    name: "Price changed",
    sentence:
      "The price moved past the quote before the order was evaluated, so the backend refused it " +
      "rather than fill at a price you had not seen.",
    recovery: { kind: "requote", label: "Take a new quote" },
    codes: ["QUOTE_EXPIRED", "STALE_MARKET_DATA"],
  },
  {
    id: "market-paused",
    name: "Market paused",
    sentence:
      "This market is not accepting orders at the moment. Positions already held are untouched by " +
      "the pause, and it is lifted by an operator or by the asset's own lifecycle, not by waiting.",
    recovery: { kind: "none", label: "" },
    codes: ["ASSET_RESTRICTED", "KILL_SWITCH_ACTIVE"],
  },
  {
    id: "account-restricted",
    name: "Account restricted",
    sentence:
      "A restriction on this account blocks this action. The reason is recorded against the " +
      "account and is shown on the account page; nothing you enter here changes it.",
    recovery: { kind: "go", label: "See the restriction", to: "/settings/account" },
    codes: ["ACCOUNT_FROZEN", "FORBIDDEN", "ELIGIBILITY_JURISDICTION"],
  },
  {
    id: "verification-pending",
    name: "Verification pending",
    sentence:
      "Identity verification is still with the provider. Nodal receives the outcome from the " +
      "provider, never from this browser, so there is nothing to confirm here.",
    recovery: { kind: "go", label: "Open verification", to: "/verify" },
    codes: ["VERIFICATION_REQUIRED"],
  },
  {
    id: "verification-rejected",
    name: "Verification rejected",
    sentence:
      "The verification provider did not verify this identity. Nodal does not overturn that " +
      "decision, and the provider's next step, where it gave one, is shown on the verification " +
      "page.",
    recovery: { kind: "go", label: "Open verification", to: "/verify" },
    codes: [],
  },
  {
    id: "withdrawal-unavailable",
    name: "Withdrawal unavailable",
    sentence:
      "No approved payout path is active for this deployment, so a withdrawal cannot be settled. " +
      "That is a decision about the product rather than about your account, and no amount of " +
      "Credits changes it.",
    recovery: { kind: "none", label: "" },
    codes: ["CAPABILITY_NOT_APPROVED", "UNSUPPORTED", "WITHDRAWAL_VELOCITY_LIMIT"],
  },
  {
    id: "payout-delayed",
    name: "Payout delayed",
    sentence:
      "The provider has accepted this payout and has not settled it yet. It stays in this state " +
      "until the provider reports an outcome; Nodal does not move value itself.",
    recovery: { kind: "wait", label: "" },
    codes: ["RECONCILIATION_REQUIRED", "SUBMISSION_STATE_UNKNOWN"],
  },
  {
    id: "session-expired",
    name: "Session expired",
    sentence:
      "This session is no longer signed in. Anything you had typed is kept, and signing in again " +
      "brings you back to this page with it.",
    recovery: { kind: "sign-in", label: "Sign in again" },
    codes: ["UNAUTHENTICATED"],
  },
  {
    id: "network-offline",
    name: "Network offline",
    sentence:
      "This browser could not reach Nodal, so nothing was sent. Nothing has changed on your " +
      "account and nothing has been submitted twice.",
    recovery: { kind: "retry", label: "Try again" },
    codes: ["UNREACHABLE"],
  },
  {
    // Not one of §11's fifteen, because §11 lists what happens to a CUSTOMER
    // and this is what happens when the app mishandles its own idempotency
    // key: it sent a key the backend had already recorded against a different
    // body, so the backend refused rather than guess which request was meant.
    // The correct fix is to stop it happening (`lib/idempotency.ts`), and it is
    // on this list anyway because a defect that reaches a customer must still
    // leave them somewhere to go. Without a sentence and a recovery it renders
    // as a dead end on top of an order that did not happen.
    id: "request-changed",
    name: "This order changed while it was being placed",
    sentence:
      "Something about this order moved between the last attempt and this one, so the backend " +
      "refused it rather than replay the earlier request. Nothing was filled and no Credits moved.",
    recovery: { kind: "requote", label: "Take a new quote" },
    codes: ["INVALID_IDEMPOTENCY_REUSE"],
  },
  {
    id: "step-up-required",
    name: "Confirm it's you",
    sentence:
      "This action needs a recent strong sign-in on this session before the backend will accept " +
      "it. Anything you had typed is kept while you confirm.",
    recovery: { kind: "step-up", label: "Confirm it's you" },
    codes: ["STEP_UP_REQUIRED"],
  },
];

/** Every situation, by id. */
export const SITUATION_BY_ID: Readonly<Record<SituationId, Situation>> = Object.fromEntries(
  SITUATIONS.map((situation) => [situation.id, situation]),
) as Readonly<Record<SituationId, Situation>>;

/**
 * The situation a problem code means, or undefined when the code is not one of
 * the fifteen. Undefined is a real answer: `explain()` already produces an
 * honest sentence for every code, and inventing a situation for one that is not
 * on the list would put the wrong recovery action in front of a customer.
 */
export function situationForCode(code: string): Situation | undefined {
  return SITUATIONS.find((situation) => situation.codes.includes(code));
}

/**
 * The situation for a failed request.
 *
 * `online` lets a caller pass `navigator.onLine`, which distinguishes "this
 * browser has no network" from "the service did not answer" — the same
 * `UNREACHABLE` code, two different sentences, and only one of them is worth a
 * retry button in front of a customer holding an unsent order.
 */
export function situationFor(input: {
  readonly code: string;
  readonly online?: boolean;
}): Situation | undefined {
  if (input.online === false) return SITUATION_BY_ID["network-offline"];
  return situationForCode(input.code);
}

/* --------------------------------------------------------------------------
 * Empty states (goal §50)
 * ------------------------------------------------------------------------ */

/**
 * The first-time sentences, in the goal's own words where it gave them.
 *
 * An empty state says what would be here and how to cause it. It is never an
 * illustration with the word "Nothing" under it, and never enterprise phrasing
 * like "no records match your criteria": an empty table is a fact about the
 * account, and a fact deserves a sentence.
 */
export interface EmptyStateText {
  readonly title: string;
  readonly body: string;
  /** The label of the one action that would fill it, when there is one. */
  readonly action?: string;
  readonly to?: string;
}

export type EmptyStateId =
  | "holdings"
  | "agents"
  | "activity"
  | "verification"
  | "credits"
  | "markets"
  | "notifications"
  | "payouts";

export const EMPTY_STATES: Readonly<Record<EmptyStateId, EmptyStateText>> = {
  holdings: {
    title: "No holdings yet",
    body: "Positions you open appear here with what you paid and what they are worth now.",
    action: "Explore markets",
    to: "/markets",
  },
  agents: {
    title: "No agents yet",
    body: "An agent trades inside limits you set: a budget, a cap per trade and a daily loss stop.",
    action: "Create your first agent",
    to: "/agents/new",
  },
  activity: {
    title: "Nothing has happened yet",
    body: "Your activity will appear here — purchases, trades, agent decisions and payout requests, in one feed.",
  },
  verification: {
    title: "Not verified",
    body: "Verify when you're ready to request withdrawals. Nothing else in the product needs it.",
    action: "Start verification",
    to: "/verify",
  },
  credits: {
    title: "No Credits yet",
    body: "Credits are internal platform value and aren't directly withdrawable. Buy some to use inside Nodal.",
    action: "Buy Credits",
    to: "/buy-credits",
  },
  markets: {
    title: "No markets yet",
    body: "Nodal-native assets created by users appear here once their markets open.",
  },
  notifications: {
    title: "No notifications",
    body: "Payments, fills, verification outcomes and payout updates arrive here as they happen.",
  },
  payouts: {
    title: "No withdrawal requests",
    body: "A request you make appears here and stays until the provider reports an outcome.",
  },
};
