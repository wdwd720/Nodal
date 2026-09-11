/**
 * The public site's standing copy.
 *
 * The two lists below are the most important text on the website, and they are
 * constants rather than prose inside a component for three reasons: they appear
 * on more than one page and must not drift between them; `src/lib/honesty.test.ts`
 * scans this file for the vocabulary the goal forbids; and the negative list is
 * the one a future edit is most tempted to soften.
 *
 * Goal §5 names the claims that may never be made — regulated exchange,
 * brokerage, bank, insured deposits, assured returns, approved withdrawals,
 * live crypto execution, legal approval — and §28 names the marketing filler
 * that may not be used. Nothing here says "revolutionize", "unlock the future",
 * "seamlessly", "AI-powered ecosystem", "next-generation" or "cutting-edge",
 * and the reason is not taste: those words carry no information, and a page
 * about somebody's money should carry nothing else.
 */

/** What the product does, in the closed-loop product's own terms. */
export const IS_CLAIMS: readonly string[] = [
  "A closed-loop internal economy you buy into with a card.",
  "A market Nodal operates for Nodal-native assets, priced by a published formula.",
  "A way to delegate bounded authority to an agent — a budget, a cap per trade, a daily loss stop.",
  "An append-only record of every Credit, every order and every decision, which you can read back.",
  "A single request path for value to leave, gated on verification and a licensed provider.",
];

/** What it is not. This list is never shortened to make a page read better. */
export const NOT_CLAIMS: readonly string[] = [
  "Not an exchange, a broker, a bank or a custodian.",
  "Not regulated, licensed or approved by anybody, anywhere.",
  "Not insured. No deposit protection scheme covers anything here.",
  "Not a route to live crypto execution: no order Nodal takes reaches an external venue or a chain.",
  "Not a promise of any outcome. You can lose everything you put in.",
];

/** The standing line under the public footer, beside the risk statement. */
export const PUBLIC_BOUNDARY_LINE =
  "Nodal operates an internal economy. Credits are internal platform value, are not money, and " +
  "are not directly withdrawable. Value leaves only through a licensed payout provider under an " +
  "active capability, and only for a verified account.";

/** Shown wherever the site describes the legal position of the architecture. */
export const LEGAL_BOUNDARY_LINE =
  "This architecture has not been certified as lawful anywhere. A licensed provider and counsel " +
  "must approve the exact implementation of any path by which value leaves the product, and until " +
  "they do, that path stays refused.";

/** The four steps of the loop, with the caveat that belongs to each. */
export interface LoopStep {
  readonly title: string;
  readonly body: string;
  readonly caveat: string;
}

export const LOOP_STEPS: readonly LoopStep[] = [
  {
    title: "Buy Credits",
    body:
      "You pay a card provider and receive Credits inside Nodal. Nodal never sees your card " +
      "number; it mints Credits when the provider reports the payment captured, and the ledger " +
      "records where they came from.",
    caveat:
      "Credits are internal platform value and aren't directly withdrawable. A payment that is " +
      "later refunded or disputed reverses the Credits it created.",
  },
  {
    title: "Trade Nodal-native assets",
    body:
      "Assets are created by users and priced by a formula against a shared pool. You take a " +
      "quote, see the price impact and the fee, and submit an order that the backend evaluates " +
      "against the limits in force at that moment.",
    caveat:
      "A quote never prices your execution, and it says so. Assets are not reviewed by Nodal as " +
      "investments, prices can fall to nearly nothing, and a market can be halted while you hold " +
      "a position.",
  },
  {
    title: "Delegate to an agent",
    body:
      "An agent acts inside limits you set: an authority level, a Credit budget, a cap per trade, " +
      "a list of allowed assets and a daily loss stop. You can pause it at any point and read " +
      "every decision it recorded.",
    caveat:
      "Limits bound how much can be lost; they do not make losing unlikely. Authority levels above " +
      "execute-within-limits are refused by policy and are shown as disabled rather than hidden.",
  },
  {
    title: "Request a withdrawal",
    body:
      "Once your identity is verified and a payout destination is approved, you can ask for value " +
      "to leave. Eligibility is computed per origin, a licensed provider quotes it, and the " +
      "provider settles it.",
    caveat:
      "Nodal cannot settle a payout itself. Where no payout capability is active, the answer is " +
      "no — for everyone, and no amount of Credits changes it.",
  },
];

/** The security claims, each of which maps to a package in the repository. */
export interface SecurityPoint {
  readonly title: string;
  readonly body: string;
}

export const SECURITY_POINTS: readonly SecurityPoint[] = [
  {
    title: "You sign in with an identity provider",
    body:
      "Nodal is an OIDC relying party. Your password is entered on the provider's pages and never " +
      "reaches Nodal. The session is a server-side record behind an HTTP-only, same-site cookie, " +
      "and the browser never holds a token.",
  },
  {
    title: "Sensitive actions need a fresh, stronger sign-in",
    body:
      "A step-up is required before the backend accepts an action that changes money or security " +
      "settings. If your session is too old for what you asked, the answer names that as the " +
      "reason instead of failing vaguely.",
  },
  {
    title: "Personal data is sealed before it is stored",
    body:
      "E-mail, legal name and date of birth are encrypted with AES-256-GCM under a key the " +
      "database never holds, bound to the row and column they belong to, so a ciphertext cannot " +
      "be moved to another account. Your profile holds only what you choose to put in it.",
  },
  {
    title: "Authorisation is deny-by-default",
    body:
      "Every API operation is mapped to the permissions it requires, and an operation nobody " +
      "mapped fails the build rather than defaulting to open. A capability that has not been " +
      "approved is closed for everyone, not merely hidden from you.",
  },
  {
    title: "The ledgers are append-only",
    body:
      "A correction is a new entry, never an edit. State changes go through transition functions " +
      "that write their own history, and the application's database role cannot rewrite a state " +
      "column at all.",
  },
  {
    title: "Verification happens on the provider's pages",
    body:
      "Identity documents go to a verification provider, not to Nodal. The outcome comes back " +
      "through the provider's own callback and never from your browser, so nothing you can send " +
      "from a page can make an account verified.",
  },
  {
    title: "Nodal takes no custody",
    body:
      "There is no wallet Nodal holds a key for, no external venue an order reaches, and no " +
      "conversion inside the product. Value leaves only through a licensed provider under an " +
      "active capability.",
  },
];
