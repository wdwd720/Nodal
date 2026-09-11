/**
 * risk-v1 — the risk disclosure.
 *
 * This one is short on purpose. A risk disclosure that runs for six pages is a
 * risk disclosure nobody reads, and an unread disclosure protects nobody. Every
 * item is something that can actually happen in this product, described in the
 * order a customer meets it.
 */
import type { PolicyDocument } from "./types.ts";

export const RISK_V1: PolicyDocument = {
  id: "risk-v1",
  slug: "risk",
  title: "Risk disclosure",
  summary:
    "What can go wrong, in the order you would meet it: buying Credits, trading them, delegating " +
    "to an agent, and asking for value to leave.",
  drafted: "2026-09-10",
  sections: [
    {
      heading: "Read this first",
      paragraphs: [
        "You can lose everything you put into Nodal. Money you spend on Credits is money you have " +
          "spent. Nothing in the product is assured, insured or protected by any scheme, and " +
          "nothing here is advice.",
      ],
    },
    {
      heading: "1. Credits are not money",
      paragraphs: [
        "A Credit is internal platform value. It is not a deposit, it is not held for you at a " +
          "bank, and it is not directly withdrawable. If the product never reaches an approved " +
          "payout path, Credits stay inside Nodal — that is a decision about the product, not a " +
          "fault, and no size of balance changes it.",
        "Credits can be frozen or reversed. A refunded, disputed or reversed card payment reverses " +
          "the Credits it created, even if you have already spent them, which can leave the " +
          "account in a frozen or negative state.",
      ],
    },
    {
      heading: "2. Nodal-native assets can go to nearly nothing",
      paragraphs: [
        "Assets on internal markets are created by users, not by Nodal. Nobody reviews them as an " +
          "investment and nobody is obliged to buy one back from you. Prices come from a formula " +
          "against a shared pool: a large sale moves the price against you, and a thin pool moves " +
          "it a lot.",
        "A quote is an estimate at a moment and never prices your execution. The price you get can " +
          "be worse than the price you were shown, which is why every quote states its impact, its " +
          "fee, a minimum you would receive, and when it expires.",
        "A market can be halted, restricted or closed to new positions by its own lifecycle or by " +
          "an operator. While it is, you cannot trade it, including to get out of a position.",
      ],
    },
    {
      heading: "3. Agents act within limits, and limits are not judgement",
      paragraphs: [
        "An agent does what its constraints allow, including at moments when a person would have " +
          "stopped. A budget, a per-trade cap and a daily loss stop bound how much can be lost; " +
          "they do not make losing unlikely.",
        "Agents run against the same markets described above, with the same illiquidity and the " +
          "same halts. An agent that cannot trade because a market is halted is stuck in exactly " +
          "the position you would be.",
      ],
    },
    {
      heading: "4. Withdrawal may never be available to you",
      paragraphs: [
        "Asking for value to leave requires identity verification, an approved payout destination, " +
          "eligible balance for its origin, and an active payout capability with a licensed " +
          "provider. Any one of those can be unavailable, and the answer is then no.",
        "Verification can be refused, and it is performed by a provider whose decision Nodal does " +
          "not override. Jurisdiction can make you ineligible. A payout that is accepted can still " +
          "be delayed or returned by the provider or by the receiving institution.",
      ],
    },
    {
      heading: "5. Software, operations and the deployment itself",
      paragraphs: [
        "This is software. It can have defects, it can be unavailable, and the providers it depends " +
          "on can be unavailable. Operators can engage a kill switch that stops new activity of a " +
          "kind without notice.",
        "On a sandbox deployment nothing is real: sandbox Credits, sandbox verification outcomes " +
          "and sandbox payouts that settle without moving value. Everything produced there is " +
          "labelled sandbox in the interface. Do not read a sandbox result as evidence of what a " +
          "real one would be.",
      ],
    },
    {
      heading: "6. Nothing here is advice",
      paragraphs: [
        "Nodal does not know your circumstances and does not assess whether anything in the " +
          "product is suitable for you. No figure, ranking, score or agent output is a " +
          "recommendation, and a model's confidence in its own prediction is not a statement about " +
          "what you will earn.",
        "This document is a draft pending legal review. It describes the risks the software " +
          "actually creates; it is not a complete account of every risk you might face.",
      ],
    },
  ],
};
