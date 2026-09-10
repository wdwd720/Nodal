/**
 * `/product` — the whole surface, one screen.
 *
 * A product overview page is usually where a company lists features. This one
 * lists **screens**, because that is what the customer will actually meet, and
 * it says for each one what it does and what it refuses. The refusals are half
 * the product: capability gates ship closed, and a page that only described the
 * happy path would misrepresent how often this software says no.
 */
import type { ReactNode } from "react";

import { LinkButton } from "../../components/Button.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Panel } from "../../components/Panel.tsx";
import { IS_CLAIMS, NOT_CLAIMS } from "../../content/site.ts";
import { CREDITS_DISCLOSURE } from "../../lib/honesty.ts";
import { ExampleCredits } from "./ExampleUI.tsx";
import { Claims, SiteItem, SitePageHead, SiteSection } from "./SiteChrome.tsx";

interface Surface {
  readonly title: string;
  readonly body: string;
  readonly refuses: string;
}

const SURFACES: readonly Surface[] = [
  {
    title: "Home",
    body:
      "Credits by bucket, portfolio value and today's change when there are positions, your top " +
      "holdings, active agents, market movers and the last few things that happened.",
    refuses:
      "It never shows a figure it has not loaded. A balance arrives through a skeleton, carries " +
      "the moment it was computed, and is never adjusted by arithmetic in your browser.",
  },
  {
    title: "Buy Credits",
    body:
      "A few preset amounts and a custom one inside the bounds the pricing endpoint states, the " +
      "exact quantity of Credits you would receive, and the card provider's own payment form.",
    refuses:
      "It shows the interval between the provider saying yes and the ledger posting as its own " +
      "state — 'balance updating' — rather than showing Credits you do not have yet.",
  },
  {
    title: "Markets",
    body:
      "Every internal market with its last price, twenty-four hour change, volume and liquidity, " +
      "and a market page with the curve's reserves, recent trades, the safety limits in force and " +
      "the ticket.",
    refuses:
      "A quote is labelled as an estimate that does not price your execution. An order past its " +
      "tolerance, into a paused market or over a limit is rejected by name, not by a generic error.",
  },
  {
    title: "Agents",
    body:
      "Describe a strategy, compile it, review the compiled version, then create an agent with an " +
      "authority level, a Credit budget, a per-trade cap, an allowlist of assets and a daily loss " +
      "stop.",
    refuses:
      "Authority levels above execute-within-limits are refused by policy and shown as disabled " +
      "with the reason. Where no compiler backend is deployed, the attempt says so instead of " +
      "producing a strategy nobody compiled.",
  },
  {
    title: "Portfolio and Activity",
    body:
      "Positions with quantity, average cost, market value, unrealised and realised results, and " +
      "one feed of everything that happened to the account with a link from each row to the object " +
      "it describes.",
    refuses:
      "Every figure is a string from the ledger, formatted by truncation rather than rounding, so " +
      "nothing on the page can read higher than the value it stands for.",
  },
  {
    title: "Withdraw and verification",
    body:
      "A page that exists for everyone. Unverified, it explains what withdrawal is and what " +
      "verification asks for. Verified, it computes the eligible amount per origin, quotes it with " +
      "a provider and follows the request to settlement.",
    refuses:
      "It is never hidden from an unverified account, and the API refuses a payout at the same " +
      "level with the same reason, so the gate cannot be worked around from either side.",
  },
];

export function Product(): ReactNode {
  return (
    <>
      <SitePageHead
        title="The product"
        lead="Six surfaces, and what each of them refuses. The refusals are not edge cases: gates ship closed, authorisation is deny-by-default, and eligibility is evaluated before every intent."
        actions={
          <>
            <LinkButton to="/get-started" variant="primary">
              Get started
            </LinkButton>
            <LinkButton to="/how-it-works">How it works</LinkButton>
          </>
        }
      />

      <SiteSection
        title="The surfaces"
        lead="What you will actually use, in the order you meet it."
      >
        <div className="site-grid-2">
          {SURFACES.map((surface) => (
            <SiteItem key={surface.title} title={surface.title}>
              <p>{surface.body}</p>
              <p className="step-caveat">{surface.refuses}</p>
            </SiteItem>
          ))}
        </div>
      </SiteSection>

      <SiteSection
        title="What the dashboard looks like"
        lead="The product's own components, rendered from fixed example values."
      >
        <ExampleCredits />
      </SiteSection>

      <SiteSection
        title="Credits"
        lead="The unit everything inside Nodal is denominated in."
        actions={<LinkButton to="/learn">The vocabulary</LinkButton>}
      >
        <Panel
          title="Buckets, and why there is more than one"
          description="A single total would be the most convenient figure on the page and the least useful."
        >
          <FieldGrid columns={2}>
            <Field label="Spendable" note="What a trade or an agent budget can draw on right now.">
              Excludes anything frozen and anything already committed.
            </Field>
            <Field label="Frozen" note="Held against something the ledger has not finished.">
              A card payment the network has not settled, or a bucket frozen by a dispute. The
              reason is shown with it.
            </Field>
            <Field label="Payout-eligible" note="Per origin, with provenance.">
              What could be paid out if a payout path were active. It is not a dollar figure and is
              never shown as one.
            </Field>
            <Field label="Total held" note="What the backend returned.">
              Never the three above added together in your browser.
            </Field>
          </FieldGrid>
          <p className="note">{CREDITS_DISCLOSURE}</p>
        </Panel>
      </SiteSection>

      <SiteSection title="The boundary" lead="Restated here because it belongs on every page that describes the product.">
        <div className="site-grid-2">
          <SiteItem title="What it is">
            <Claims kind="is" items={IS_CLAIMS} />
          </SiteItem>
          <SiteItem title="What it is not">
            <Claims kind="not" items={NOT_CLAIMS} />
          </SiteItem>
        </div>
      </SiteSection>
    </>
  );
}
