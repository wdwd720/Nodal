/**
 * `/learn` — the vocabulary, and how to read a figure.
 *
 * Goal §5 asks for "Docs / Learn" in the public navigation. There is no
 * developer documentation site to link to, and a nav item that lands on a page
 * saying "coming soon" is a dead control. What this product genuinely has to
 * teach is its own vocabulary — a customer who does not know the difference
 * between spendable and payout-eligible will misread their own balance — and
 * how its figures are drawn, which is unusual enough to be worth a page.
 *
 * The second half is the part most products would never write down: truncation
 * rather than rounding, an em dash for absent, a sign glyph on every coloured
 * change, an absolute timestamp on every snapshot. Publishing the rules is what
 * makes them checkable.
 */
import type { ReactNode } from "react";

import { LinkButton } from "../../components/Button.tsx";
import { Panel } from "../../components/Panel.tsx";
import { CREDITS_DISCLOSURE } from "../../lib/honesty.ts";
import { SitePageHead, SiteSection } from "./SiteChrome.tsx";

interface Term {
  readonly term: string;
  readonly meaning: string;
}

const VOCABULARY: readonly Term[] = [
  {
    term: "Credits",
    meaning:
      "Internal platform value. Not money, not a deposit, and not directly withdrawable. Everything " +
      "inside Nodal is denominated in them.",
  },
  {
    term: "Spendable",
    meaning:
      "The part of your Credits a trade or an agent budget can draw on right now. Lower than the " +
      "total whenever something is frozen or already committed.",
  },
  {
    term: "Frozen",
    meaning:
      "Held against something unfinished — a payment the card network has not settled, or a bucket " +
      "frozen by a dispute. Shown with its reason, never hidden.",
  },
  {
    term: "Payout-eligible",
    meaning:
      "What could be paid out, per origin, if a payout path were active. It is a quantity of " +
      "Credits, not a currency figure, and the product never shows it as one.",
  },
  {
    term: "Origin and provenance",
    meaning:
      "Where a Credit came from — a purchase, a trade, a creator fee, a reversal. What may leave " +
      "depends on origin rather than on the total, which is why the breakdown is shown.",
  },
  {
    term: "Quote",
    meaning:
      "An estimate of a trade at a moment: expected output, price impact, fees, a minimum received " +
      "and an expiry. It never prices your execution, and it says so on itself.",
  },
  {
    term: "Price impact",
    meaning:
      "How far your own order moves the price along the curve. A property of the pool's depth, not " +
      "a fee, and named as a risk measure wherever it appears.",
  },
  {
    term: "Capability gate",
    meaning:
      "A named permission for a whole class of action — buying Credits, creating an asset, " +
      "reserving a payout. Gates ship closed. A closed gate refuses everyone, not only you.",
  },
  {
    term: "Kill switch",
    meaning:
      "An operator control that stops new risk of a kind immediately, independent of the gates. " +
      "Positions already held are untouched by the switch itself.",
  },
  {
    term: "Step-up",
    meaning:
      "A fresh, stronger sign-in required before the backend accepts a sensitive action. Asking " +
      "for it is the system working, and the reason is always named.",
  },
  {
    term: "Verified",
    meaning:
      "A level, not a badge of approval. It changes what you are eligible to do and changes nothing " +
      "about the Credits you already hold.",
  },
  {
    term: "Sandbox",
    meaning:
      "Anything a sandbox deployment produced: sandbox Credits, sandbox verification outcomes, " +
      "sandbox payouts that settle without moving value. Labelled everywhere it appears.",
  },
  {
    term: "Refusal",
    meaning:
      "A decision, not a fault. It names what was refused, which rule refused it, what would change " +
      "the answer, and a correlation id — and it stays on screen until you act on it.",
  },
];

const FIGURE_RULES: readonly Term[] = [
  {
    term: "Truncated, never rounded",
    meaning:
      "Where a figure cannot show every digit, the rest are dropped. Rounding could let a number " +
      "read higher than the value behind it, and this interface may never make you believe you " +
      "have more than you have.",
  },
  {
    term: "An em dash means absent",
    meaning:
      "A value the backend did not return renders as a dash with a stated reason — never as a " +
      "zero, and never as N/A. A zero balance and an unreadable one are one assertion apart.",
  },
  {
    term: "Zero is a real answer",
    meaning: "It takes no sign and no colour. Zero is not a direction.",
  },
  {
    term: "Every change carries a sign",
    meaning:
      "A gain shows a plus, a loss shows a minus glyph. The colour is a second channel, so the " +
      "figure still reads correctly in grayscale and to a colourblind reader.",
  },
  {
    term: "Figures line up",
    meaning:
      "Every number is set in tabular figures with a slashed zero. A column where the digits do " +
      "not align is a column where a wrong order of magnitude is invisible.",
  },
  {
    term: "Every snapshot carries its moment",
    meaning:
      "An absolute UTC timestamp sits beside any figure that came from a snapshot, and it ages " +
      "visibly. A figure without a moment attached invites you to assume it is current.",
  },
  {
    term: "A skeleton, not a placeholder number",
    meaning:
      "While a figure loads you see its shape, never a zero standing in for it. A skeleton is " +
      "honest in a way a stale number is not.",
  },
];

function Glossary(props: { readonly terms: readonly Term[] }): ReactNode {
  return (
    <dl className="glossary">
      {props.terms.map((entry) => (
        <div key={entry.term}>
          <dt>{entry.term}</dt>
          <dd>{entry.meaning}</dd>
        </div>
      ))}
    </dl>
  );
}

export function Learn(): ReactNode {
  return (
    <>
      <SitePageHead
        title="Learn"
        lead="The words this product uses, and the rules its figures are drawn by. Both are worth five minutes before you spend anything, because a balance you misread is a decision you would not have made."
        actions={
          <>
            <LinkButton to="/how-it-works">How it works</LinkButton>
            <LinkButton to="/risk">Risk disclosure</LinkButton>
          </>
        }
      />

      <SiteSection
        title="The vocabulary"
        lead="Used consistently everywhere in the product. Where a word here differs from the one another product would use, the difference is the point."
      >
        <Glossary terms={VOCABULARY} />
      </SiteSection>

      <SiteSection
        title="How to read a figure"
        lead="These rules are enforced by tests rather than by convention, so they hold on screens nobody has written yet."
      >
        <Glossary terms={FIGURE_RULES} />
      </SiteSection>

      <SiteSection title="One more time" lead="The sentence that matters most.">
        <Panel title="Credits" description="Said the same way on every screen that shows one.">
          <p className="note">{CREDITS_DISCLOSURE}</p>
        </Panel>
      </SiteSection>
    </>
  );
}
