/**
 * The landing page.
 *
 * The order of the page is an argument: what this is, what it is not, how the
 * loop works, what the interface looks like, what a Credit is, and how the
 * security actually works. The negative claims are the second thing a visitor
 * reads rather than the last, because a product that hides them below the fold
 * is a product that expects them to change somebody's mind.
 *
 * There is no metric anywhere on this page, no testimonial, no logo wall and no
 * illustration. Goal §5 forbids all four, and the honest reason is simpler than
 * the rule: this product has no customers to quote and no volume to report, and
 * inventing either would be the first lie on a website about somebody's money.
 */
import type { ReactNode } from "react";

import { LinkButton } from "../../components/Button.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Panel } from "../../components/Panel.tsx";
import { IS_CLAIMS, LOOP_STEPS, NOT_CLAIMS, SECURITY_POINTS } from "../../content/site.ts";
import { CREDITS_DISCLOSURE } from "../../lib/honesty.ts";
import { ExampleCredits, ExampleMarkets } from "./ExampleUI.tsx";
import { Claims, SiteItem, SiteSection } from "./SiteChrome.tsx";

export function Landing(): ReactNode {
  return (
    <>
      <section className="site-hero">
        <div className="site-width site-hero-grid">
          <div>
            <h1>A control plane between your capital and markets.</h1>
            <p className="lead">
              Nodal&rsquo;s first product is a closed loop. You buy Credits with a card, trade
              Nodal-native assets on a market Nodal operates, and delegate bounded authority to
              agents that cannot exceed the limits you set. Value leaves only through a licensed
              provider, and only once you are verified.
            </p>
            <div className="site-cta">
              <LinkButton to="/get-started" variant="primary">
                Get started
              </LinkButton>
              <LinkButton to="/how-it-works">How it works</LinkButton>
            </div>
            <p className="site-cta-note">
              Credits are internal platform value and aren&rsquo;t directly withdrawable. Nodal is
              not an exchange, a broker or a bank.
            </p>
          </div>
          <ExampleCredits />
        </div>
      </section>

      <SiteSection
        title="What Nodal is, and what it is not"
        lead="Both lists are on the front page on purpose. The second one is the part that decides whether this product is for you."
      >
        <div className="site-grid-2">
          <SiteItem title="What it is">
            <Claims kind="is" items={IS_CLAIMS} />
          </SiteItem>
          <SiteItem title="What it is not">
            <Claims kind="not" items={NOT_CLAIMS} />
          </SiteItem>
        </div>
      </SiteSection>

      <SiteSection
        title="The loop"
        lead="Four steps, each with the thing about it that is easy to get wrong."
        actions={<LinkButton to="/how-it-works">Read the detail</LinkButton>}
      >
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
        title="The interface, not a picture of it"
        lead="Everything below is the product's own components rendered from fixed example values, so what you see here is exactly what the application draws."
        actions={<LinkButton to="/product">See the product</LinkButton>}
      >
        <ExampleMarkets />
      </SiteSection>

      <SiteSection
        title="Credits, said plainly"
        lead="The one thing worth understanding before you spend anything."
      >
        <Panel
          title="What a Credit is"
          description="Internal platform value, recorded on an append-only ledger, with its origin attached."
        >
          <FieldGrid columns={2}>
            <Field label="Buying" note="Goal §28's own sentence, because it is the right one.">
              Buy Credits to use inside Nodal.
            </Field>
            <Field label="Holding" note="They exist only inside the product.">
              Credits are internal platform value and aren&rsquo;t directly withdrawable.
            </Field>
            <Field label="Spending" note="Trades, agent budgets and internal products.">
              Spendable excludes anything frozen or already committed, which is why it can be lower
              than the total you hold.
            </Field>
            <Field
              label="Leaving"
              note="The only path out, and every part of it can refuse."
              emphasis
            >
              Withdrawal eligibility requires identity verification and an approved payout method.
            </Field>
          </FieldGrid>
          <p className="note">{CREDITS_DISCLOSURE}</p>
        </Panel>
      </SiteSection>

      <SiteSection
        title="Security"
        lead="What is actually true about how this is built, in the terms an engineer would check."
        actions={<LinkButton to="/security">All of it</LinkButton>}
      >
        <div className="site-grid">
          {SECURITY_POINTS.slice(0, 4).map((point) => (
            <SiteItem key={point.title} title={point.title}>
              <p>{point.body}</p>
            </SiteItem>
          ))}
        </div>
      </SiteSection>

      <SiteSection
        title="Start"
        lead="Signing in creates an identity with the identity provider and a Nodal profile. Nothing financial is asked for until you ask for value to leave."
        actions={
          <>
            <LinkButton to="/get-started" variant="primary">
              Get started
            </LinkButton>
            <LinkButton to="/risk">Read the risk disclosure</LinkButton>
          </>
        }
      >
        <p className="site-prose">
          There is no identity verification at sign-up, no financial questionnaire and no
          document upload. Verification is asked for at the one point it is needed — when you
          request a withdrawal — and it happens on the verification provider&rsquo;s own pages.
        </p>
      </SiteSection>
    </>
  );
}
