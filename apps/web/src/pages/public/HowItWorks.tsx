/**
 * `/how-it-works` — the loop in detail, plus the two paths a visitor most needs
 * to understand before they spend anything: what happens when a payment lands,
 * and what happens when they ask for value back.
 *
 * The payment path is written out because the interval between "the provider
 * said yes" and "the ledger posted" is where every payments product is tempted
 * to lie, and this one has a state for it instead.
 */
import type { ReactNode } from "react";

import { LinkButton } from "../../components/Button.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Panel } from "../../components/Panel.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { LEGAL_BOUNDARY_LINE, LOOP_STEPS } from "../../content/site.ts";
import { CREDITS_DISCLOSURE } from "../../lib/honesty.ts";
import { SiteItem, SitePageHead, SiteSection } from "./SiteChrome.tsx";

interface Stage {
  readonly title: string;
  readonly body: string;
}

/** What a card payment goes through. Each stage is a state the interface has. */
const PAYMENT_STAGES: readonly Stage[] = [
  {
    title: "You confirm an amount",
    body:
      "The page mints an idempotency key at that moment — not when it rendered — so a retry after " +
      "a dropped connection or a re-authentication cannot create a second payment.",
  },
  {
    title: "The card provider takes the payment",
    body:
      "The card details go to the provider's own form. Nodal receives an identifier for the " +
      "payment, never the card number, and the provider may ask you for a second factor.",
  },
  {
    title: "The provider confirms it",
    body:
      "The confirmation arrives as a signed webhook to the backend, not as a message from your " +
      "browser. Until it lands, the interface says the balance is updating and shows no Credits.",
  },
  {
    title: "The ledger posts",
    body:
      "Credits are minted against the payment, marked with their origin and with a reversible " +
      "finality state, and the balance you see changes because the ledger changed — never because " +
      "the browser added anything up.",
  },
  {
    title: "It can still be reversed",
    body:
      "A refund, a dispute or a chargeback reverses the Credits it created, even if they have been " +
      "spent. The account shows the reversal as an activity row and the affected bucket as frozen, " +
      "with the reason.",
  },
];

/** What a withdrawal goes through. Every stage can refuse. */
const WITHDRAWAL_STAGES: readonly Stage[] = [
  {
    title: "Verification",
    body:
      "Identity, age, jurisdiction and sanctions screening, performed by a provider on its own " +
      "pages. The outcome comes back through the provider's callback; nothing your browser sends " +
      "can make an account verified.",
  },
  {
    title: "A payout destination",
    body:
      "Stored as a provider token and displayed masked. Nodal does not hold your account details.",
  },
  {
    title: "Eligibility",
    body:
      "Computed per origin at the moment of the request, with provenance. What may leave depends " +
      "on where the Credits came from, not on the size of the balance.",
  },
  {
    title: "A quote from the provider",
    body: "Fees, the amount that would arrive, and when the quote expires. It is the provider's, not Nodal's.",
  },
  {
    title: "The provider settles it, or does not",
    body:
      "The request moves from accepted to settled only when the provider says so. A delay is a " +
      "state with a name, not a spinner, and no value moves inside Nodal at any point.",
  },
];

export function HowItWorks(): ReactNode {
  return (
    <>
      <SitePageHead
        title="How it works"
        lead="Four steps, then the two paths worth reading in full: what a payment actually does, and what has to be true before value can leave."
        actions={
          <>
            <LinkButton to="/get-started" variant="primary">
              Get started
            </LinkButton>
            <LinkButton to="/security">Security</LinkButton>
          </>
        }
      />

      <SiteSection title="The loop" lead="Each step with the thing about it that is easy to get wrong.">
        <ol className="steps">
          {LOOP_STEPS.map((step) => (
            <li key={step.title}>
              <div>
                <h3>{step.title}</h3>
                <p>{step.body}</p>
                <p className="step-caveat">{step.caveat}</p>
              </div>
            </li>
          ))}
        </ol>
      </SiteSection>

      <SiteSection
        title="Getting an account"
        lead="Sixty to ninety seconds, and nothing financial is asked for."
      >
        <Panel
          title="Three screens"
          description="Identity, profile, acknowledgements. Then the dashboard."
        >
          <FieldGrid columns={3}>
            <Field label="Identity" note="On the identity provider's own pages.">
              Create an account, confirm your e-mail, and add a second factor if the provider asks
              for one. Nodal never sees the password.
            </Field>
            <Field label="Profile" note="What the product calls you.">
              A display name and, if you want one, a handle. Locale and timezone are prefilled from
              your browser and you can change them.
            </Field>
            <Field label="Acknowledgements" note="Two, and they are the two that matter." emphasis>
              That Credits are internal and not withdrawable until eligible, and the risk statement
              for an internal economy. Both are recorded with the version you read.
            </Field>
          </FieldGrid>
          <p className="note">
            There is no identity verification at this point and no financial questionnaire.
            Verification is asked for at the one moment it is needed.
          </p>
        </Panel>
      </SiteSection>

      <SiteSection
        title="What a payment actually does"
        lead="Five stages. The interface has a state for each of them, including the awkward one in the middle."
      >
        <ol className="steps">
          {PAYMENT_STAGES.map((stage) => (
            <li key={stage.title}>
              <div>
                <h3>{stage.title}</h3>
                <p>{stage.body}</p>
              </div>
            </li>
          ))}
        </ol>
        <p className="site-prose">{CREDITS_DISCLOSURE}</p>
      </SiteSection>

      <SiteSection
        title="What has to be true before value leaves"
        lead="Five conditions. Any one of them missing is a refusal with that condition named."
        actions={<StatusBadge tone="warn">Every stage can refuse</StatusBadge>}
      >
        <ol className="steps">
          {WITHDRAWAL_STAGES.map((stage) => (
            <li key={stage.title}>
              <div>
                <h3>{stage.title}</h3>
                <p>{stage.body}</p>
              </div>
            </li>
          ))}
        </ol>
      </SiteSection>

      <SiteSection
        title="The legal boundary"
        lead="Stated because a product that moves value has to be honest about what it has not been told it may do."
      >
        <div className="site-grid-2">
          <SiteItem title="What is decided">
            <p>
              No financial identity check until you ask for value to leave. Credits stay internal
              until then. Verification, eligibility and a licensed provider sit between a request
              and any payout, in that order, and the order is fixed in the code.
            </p>
          </SiteItem>
          <SiteItem title="What is not">
            <p>{LEGAL_BOUNDARY_LINE}</p>
          </SiteItem>
        </div>
      </SiteSection>
    </>
  );
}
