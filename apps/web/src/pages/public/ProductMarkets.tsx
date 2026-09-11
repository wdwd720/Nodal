/**
 * `/product/markets` — how the internal market actually prices things.
 *
 * `docs/product/USER_JOURNEY.md` §0 wants this page to be a read-only preview
 * of the live list with the sandbox label visible, and since D-080 opened
 * `GET /v1/native-markets` to a signed-out browser it is one. Every market read
 * beyond the list is still gated, so the only action offered is to sign in —
 * anything else would be a door a visitor cannot open.
 *
 * The example composition stays as the fallback for an empty or unreachable
 * list, with every label it had. A deployment with no markets is a fact worth
 * stating, and an example that quietly stood in for live data would be the one
 * fake dashboard on a site that has none.
 */
import type { ReactNode } from "react";

import { LinkButton } from "../../components/Button.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Panel } from "../../components/Panel.tsx";
import { NATIVE_ASSET_RISK, NATIVE_PRICE_NOTE } from "../../lib/honesty.ts";
import { LiveMarkets } from "./LiveMarkets.tsx";
import { SiteItem, SitePageHead, SiteSection } from "./SiteChrome.tsx";

const MECHANICS: ReadonlyArray<{ readonly title: string; readonly body: string }> = [
  {
    title: "The price comes from a formula, not from a book",
    body:
      "There is no order book and no counterparty. A market holds a reserve of Credits and a " +
      "reserve of the asset, and a constant-product curve decides the price. Buying moves the " +
      "price up along the curve; selling moves it down. The formula is deterministic, so the same " +
      "trade against the same reserves always prices the same way.",
  },
  {
    title: "Part of the reserve is virtual",
    body:
      "The curve is seeded with a virtual Credit reserve so that the first buyer does not face an " +
      "absurd price. That part is arithmetic, not money: the market page shows the real reserve " +
      "and the virtual reserve as two separate figures, because only one of them can ever be paid " +
      "out to anybody.",
  },
  {
    title: "A quote never prices your execution",
    body:
      "You can take a quote before you trade. It shows the expected output, the price impact, the " +
      "platform fee, the creator fee, a minimum you would receive and when it expires — and it " +
      "states, on the quote itself, that it is not the price you will get. The order is priced " +
      "when the backend evaluates it, against the reserves at that instant.",
  },
  {
    title: "Price impact is shown before you commit",
    body:
      "A large order against a thin pool moves the price against itself. That is a property of the " +
      "curve rather than a fee, and it is named as a risk measure in the quote and in the table so " +
      "it is not a surprise afterwards.",
  },
  {
    title: "Markets stop",
    body:
      "A market can be halted or restricted to closing trades by the asset's own lifecycle, by a " +
      "surveillance rule, or by an operator. While it is, orders are refused with that reason — " +
      "including orders that would close a position you already hold.",
  },
  {
    title: "The asset was made by a user",
    body:
      "Supply, creator allocation, symbol, fees and the market formula are fixed at launch and " +
      "cannot be changed afterwards, by the creator or by Nodal. Nodal does not review an asset as " +
      "an investment, and nobody is obliged to buy one back from you.",
  },
];

export function ProductMarkets(): ReactNode {
  return (
    <>
      <SitePageHead
        title="Markets"
        lead="An off-chain market Nodal operates for assets Nodal's users create, priced by a published constant-product formula and denominated entirely in Credits."
        actions={
          <>
            <LinkButton to="/get-started" variant="primary">
              Get started
            </LinkButton>
            <LinkButton to="/product/agents">Agents</LinkButton>
          </>
        }
      />

      <SiteSection
        title="How pricing works"
        lead="Six things that are true of every market here."
      >
        <div className="site-grid-2">
          {MECHANICS.map((item) => (
            <SiteItem key={item.title} title={item.title}>
              <p>{item.body}</p>
            </SiteItem>
          ))}
        </div>
      </SiteSection>

      <SiteSection
        title="What is listed"
        lead="Read from Nodal's public market list as this page loaded. Opening a market, taking a quote and trading all need a session."
      >
        <LiveMarkets />
      </SiteSection>

      <SiteSection title="What the ticket asks you" lead="The same four inputs on every market.">
        <Panel
          title="The ticket"
          description="Buy or sell, an amount, a quote, then an order the backend prices for itself."
        >
          <FieldGrid columns={2}>
            <Field label="Side" note="Buy moves along the curve one way; sell moves it back.">
              Buy or sell.
            </Field>
            <Field label="Amount" note="In Credits, or in units of the asset.">
              Whichever you think in. The quote shows the other one.
            </Field>
            <Field label="Tolerance" note="How far the price may move before the order is refused.">
              An order that would fill outside it is rejected rather than filled at a price you had
              not seen.
            </Field>
            <Field label="Idempotency" note="Minted when you confirm, not when the page renders." emphasis>
              A retry after a network failure or a re-authentication reuses the same key, so the
              same order cannot be placed twice.
            </Field>
          </FieldGrid>
          <p className="note">{NATIVE_PRICE_NOTE}</p>
          <p className="note">{NATIVE_ASSET_RISK}</p>
        </Panel>
      </SiteSection>
    </>
  );
}
