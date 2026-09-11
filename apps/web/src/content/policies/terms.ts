/**
 * terms-v1 — the product terms, in plain language, describing only what the
 * code does.
 *
 * Written against the architecture in docs/product/PRODUCT_ARCHITECTURE.md and
 * docs/product/VERIFICATION_AND_WITHDRAWAL.md. Every claim here is checkable
 * against a package in `internal/`. Where the software cannot do something, the
 * document says so rather than reserving a right that would imply it can.
 *
 * Deliberately absent: a governing-law clause, a jurisdiction, an arbitration
 * agreement, a class-action waiver, a limitation of liability figure. Those are
 * choices with legal consequences that this repository is not in a position to
 * make, and inventing them would be worse than leaving them out. Counsel adds
 * them; the file is replaced without touching a component.
 */
import type { PolicyDocument } from "./types.ts";

export const TERMS_V1: PolicyDocument = {
  id: "terms-v1",
  slug: "terms",
  title: "Product terms",
  summary:
    "What Nodal is, what Credits are, what the software will and will not do with them, and where " +
    "the boundary between Nodal and a licensed provider sits.",
  drafted: "2026-09-10",
  sections: [
    {
      heading: "1. What this covers",
      paragraphs: [
        "This document covers the Nodal product: the website, the application you sign in to, and " +
          "the API behind them. It describes how the closed-loop economy works and what happens to " +
          "value you put into it.",
        "It is written to be read. If a sentence here does not match what the software does, the " +
          "software is what counts and the sentence is a defect worth reporting.",
      ],
    },
    {
      heading: "2. What Nodal is, and what it is not",
      paragraphs: [
        "Nodal operates an internal economy. You buy Credits with a card, use them inside Nodal to " +
          "trade Nodal-native assets on a market Nodal itself operates, and delegate bounded " +
          "authority to agents that act within limits you set.",
        "Nodal is not an exchange, a broker, a bank, a custodian, a money transmitter or an " +
          "investment adviser. It is not regulated as any of those, it holds no licence to act as " +
          "one, and nothing in the product has been approved by a regulator. Nothing Nodal shows " +
          "you is advice or a recommendation.",
      ],
      bullets: [
        "No deposit you make is insured by any government or private scheme.",
        "No outcome inside the product is assured, promised or protected.",
        "Nodal does not execute orders on any external market or blockchain on your behalf.",
        "Nodal never holds a private key on your behalf and does not take custody of external assets.",
      ],
    },
    {
      heading: "3. Credits",
      paragraphs: [
        "Credits are internal platform value. They exist only inside Nodal, they are recorded on an " +
          "append-only ledger, and they are not money. Buying Credits is a purchase of something " +
          "usable inside the product, not a deposit into an account you can draw on.",
        "Credits are not directly withdrawable. Value leaves Nodal only through a withdrawal " +
          "request, and only when every condition in section 5 is met. Until then a Credit balance " +
          "is exactly what it says it is: a balance inside a closed loop.",
        "Each Credit records where it came from. What may eventually be paid out, if anything, " +
          "depends on that origin and not on the size of the balance, which is why the product " +
          "shows the breakdown rather than one total.",
        "Credits can be frozen. If a card payment is refunded, disputed or reversed by the card " +
          "network, the Credits it created are reversed on the ledger. If you have already spent " +
          "them the account can go into a frozen or negative bucket, and the product shows that " +
          "state and its reason rather than hiding it.",
      ],
    },
    {
      heading: "4. Buying Credits",
      paragraphs: [
        "Card payments are handled by a payment provider. Nodal never sees or stores your card " +
          "number; it receives the provider's answer about the payment and mints Credits when the " +
          "provider reports the payment captured.",
        "A payment that the provider reports as succeeded does not, by itself, move anything: " +
          "Credits appear when the provider's webhook lands and the ledger posts. The product " +
          "shows that interval honestly rather than showing a balance it does not yet have.",
        "On a sandbox deployment no real money moves. Every figure produced there is labelled as " +
          "sandbox in the interface and in the API response.",
      ],
    },
    {
      heading: "5. Withdrawal",
      paragraphs: [
        "You may request that value leave Nodal. A request is not a payment and Nodal cannot settle " +
          "one itself. Four things must all be true before value can move, and the product refuses " +
          "the request with the specific reason when any of them is not:",
      ],
      bullets: [
        "Your identity is verified to the level the payout path requires. Verification is performed " +
          "by a third-party provider on its own pages; Nodal receives the outcome from the provider " +
          "and never from your browser.",
        "An approved payout destination exists for your account. Nodal stores a provider token for " +
          "it, never full account details, and displays it masked.",
        "The amount is within the eligible portion of your balance for its origin. Eligibility is " +
          "computed per origin, with provenance, at the moment of the request.",
        "The payout capability is active for this deployment and a licensed conversion provider is " +
          "configured. On any deployment where it is not, the answer is no, for everyone, and no " +
          "amount of Credits changes it.",
      ],
    },
    {
      heading: "6. Verification",
      paragraphs: [
        "Nodal asks for identity information only at the point it is needed — when you ask for " +
          "value to leave. Verification checks identity, age, jurisdiction and sanctions screening " +
          "through a provider.",
        "Verification changes what you are eligible to do. It is not an approval, an endorsement or " +
          "a statement that anything you do inside the product is a good idea. A verified account " +
          "holds exactly the same closed-loop Credits it held before.",
        "Verification can be refused, can need more information, and can be reversed. Each of those " +
          "states is shown with what it means and what you can do next.",
      ],
    },
    {
      heading: "7. Markets and Nodal-native assets",
      paragraphs: [
        "Nodal-native assets are created by users, not by Nodal. Nodal does not review any of them " +
          "as an investment. Prices come from a deterministic formula against a shared pool, and a " +
          "price can fall to nearly nothing. Nobody is obliged to buy an asset back from you.",
        "Prices on internal markets are quoted in Credits and are never converted into a currency, " +
          "because no approved external value for a Credit exists.",
        "A quote is an estimate at a moment. It never prices your execution, and the product says " +
          "so beside every quote it shows. A market can be halted or restricted by its own " +
          "lifecycle or by an operator, and orders are refused while it is.",
      ],
    },
    {
      heading: "8. Agents",
      paragraphs: [
        "An agent is a bounded delegation. You give it an authority level, a Credit budget, a " +
          "per-trade cap, a list of allowed assets and a daily loss stop, and it cannot act outside " +
          "them. Higher authority levels are refused by policy and are shown as disabled rather " +
          "than hidden.",
        "An agent is a piece of software following constraints you set. It does not exercise " +
          "judgement on your behalf, it is not supervised by anyone, and you remain responsible for " +
          "what you authorise it to do.",
      ],
    },
    {
      heading: "9. Your account",
      paragraphs: [
        "You sign in through an identity provider. Nodal never sees your password. Sensitive " +
          "actions require a recent strong authentication on the session before the backend accepts " +
          "them.",
        "You are responsible for the security of the identity you sign in with. Tell us if you " +
          "believe a session is not yours; the security page lists your sessions and lets you " +
          "revoke any of them.",
        "An account can be restricted — by a payment dispute, by a verification outcome, or by an " +
          "operator acting on a specific reason. A restriction is shown to you with its reason, not " +
          "applied silently.",
      ],
    },
    {
      heading: "10. What the software may refuse",
      paragraphs: [
        "This product refuses by design. Capability gates ship closed, authorisation is " +
          "deny-by-default, eligibility is evaluated before every intent, and risk limits are " +
          "deterministic. A refusal is the system working, and every refusal states what was " +
          "refused, which rule refused it, and what — if anything — would change the answer.",
        "Nodal may suspend an action, a market or a whole capability without notice when a kill " +
          "switch is engaged or a limit is reached. Existing balances are not taken by such a stop; " +
          "they are frozen or left untouched, and the reason is shown.",
      ],
    },
    {
      heading: "11. Limits of this document",
      paragraphs: [
        "This is a draft. No lawyer has reviewed it. It contains no governing-law clause, no " +
          "jurisdiction, no arbitration agreement and no limitation of liability, because those are " +
          "decisions this document is not in a position to make and inventing them would mislead " +
          "you about what has actually been agreed.",
        "The architecture described here is not certified as lawful anywhere. A licensed provider " +
          "and counsel must approve the exact implementation of any path by which value leaves the " +
          "product, and until they do that path stays refused.",
      ],
    },
  ],
};
