/**
 * `/get-started` — one button, and an honest account of what pressing it does.
 *
 * `docs/product/USER_JOURNEY.md` §1: this page explains the two things that
 * will happen — an identity is created with the identity provider, and a Nodal
 * profile is created — and offers one button. The target for the whole journey
 * is sixty to ninety seconds from here to the dashboard, which is only possible
 * because nothing financial is asked for on the way.
 *
 * The third block is the one a growth-minded page would delete: what is *not*
 * asked for. It stays, because a visitor deciding whether to start deserves to
 * know that the identity checks come later rather than never.
 */
import type { ReactNode } from "react";

import { Button } from "../../components/Button.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Panel } from "../../components/Panel.tsx";
import { LOGIN_PATH } from "../../api/client.ts";
import { beginSignIn } from "../../session.tsx";
import { CREDITS_DISCLOSURE } from "../../lib/honesty.ts";
import { SitePageHead, SiteSection } from "./SiteChrome.tsx";

export function GetStarted(): ReactNode {
  return (
    <>
      <SitePageHead
        title="Get started"
        lead="Two things happen when you press the button: an identity is created with the identity provider, and a Nodal profile is created for it. Nothing else is asked for."
      />

      <SiteSection title="What happens next" lead="Three screens, then the dashboard.">
        <div className="site-narrow">
          <Panel
            title="Create an account"
            description="You are handed to the identity provider. Nodal never sees your password and never holds a token."
          >
            <FieldGrid columns={3}>
              <Field label="1 — Identity" note="On the provider's own pages.">
                An e-mail address and a password, a confirmation e-mail, and a second factor if the
                provider asks for one.
              </Field>
              <Field label="2 — Profile" note="What Nodal calls you.">
                A display name, and a handle if you want one. Locale and timezone are prefilled from
                your browser.
              </Field>
              <Field label="3 — Acknowledgements" note="Recorded with the version you read." emphasis>
                That Credits are internal platform value and aren&rsquo;t directly withdrawable, and
                the risk statement for an internal economy.
              </Field>
            </FieldGrid>

            <div className="form-actions">
              <Button
                variant="primary"
                onClick={() => {
                  beginSignIn({ returnTo: "/home" });
                }}
              >
                Continue to the identity provider
              </Button>
            </div>
            <p className="mono-small signin-target">{LOGIN_PATH}</p>
            <p className="site-panel-note">
              The address above is where this button sends your browser. It is the API&rsquo;s own
              login endpoint, which begins an OIDC authorisation-code exchange with PKCE and ends
              by setting an HTTP-only session cookie.
            </p>
          </Panel>
        </div>
      </SiteSection>

      <SiteSection
        title="What is not asked for"
        lead="Not yet, and not until it is genuinely needed."
      >
        <div className="site-narrow">
          <Panel
            title="No financial identity check at sign-up"
            description="Goal §60's ordering: no financial identity check, then Credits, then use, then a withdrawal request, and only then verification."
          >
            <FieldGrid columns={2}>
              <Field label="No document upload" note="Verification happens on a provider's pages, later.">
                Only when you ask for value to leave.
              </Field>
              <Field label="No financial questionnaire" note="Nodal does not assess suitability.">
                Nothing in the product is advice or a recommendation.
              </Field>
              <Field label="No card details to Nodal" note="They go to the payment provider's form.">
                Nodal receives the provider&rsquo;s identifier for a payment, never the card number.
              </Field>
              <Field label="No obligation to spend" note="An account with no Credits is a valid account.">
                You can read every screen in the product before buying anything.
              </Field>
            </FieldGrid>
            <p className="note">{CREDITS_DISCLOSURE}</p>
          </Panel>
        </div>
      </SiteSection>
    </>
  );
}
