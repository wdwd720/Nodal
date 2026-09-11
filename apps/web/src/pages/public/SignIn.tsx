/**
 * `/sign-in` — and the return path.
 *
 * The button navigates to `GET /v1/auth/login`, which begins an OIDC
 * authorization-code flow with PKCE and ends by setting a server-side session
 * cookie. The app never handles a credential and never holds a token: there is
 * nothing on this page for a script on another origin to steal.
 *
 * # The return path goes to the API, not into this tab
 *
 * `?return=/withdraw` says where the customer was going. It is validated here
 * as a local path and then handed to `GET /v1/auth/login` as `return_to`: the
 * backend stores it with the login attempt, never echoes it from the request,
 * refuses anything that is not a local path, and appends it to the app origin
 * the deployment configures.
 *
 * An earlier version of this page held the path in `sessionStorage` and
 * forwarded from `/` on the way back, because the login endpoint took no such
 * parameter. It does now, and a redirect target the server holds is one fewer
 * place a redirect target can be tampered with. The tab still keeps what the
 * customer TYPED, which is a different thing and nobody else's business.
 */
import type { ReactNode } from "react";
import { Navigate, useSearchParams } from "react-router-dom";

import { LOGIN_PATH } from "../../api/client.ts";
import { Button } from "../../components/Button.tsx";
import { Panel } from "../../components/Panel.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { SITUATION_BY_ID } from "../../lib/errors.ts";
import { RISK_FOOTER } from "../../lib/honesty.ts";
import { isLocalPath } from "../../lib/survives-sign-in.ts";
import { beginSignIn, useSession } from "../../session.tsx";
import { SitePageHead, SiteSection } from "./SiteChrome.tsx";

/** The default landing place for somebody who arrived here on purpose. */
export const DEFAULT_RETURN = "/home";

export function SignIn(): ReactNode {
  const [params] = useSearchParams();
  const session = useSession();

  const requested = params.get("return");
  const stepUp = params.get("step") === "up";
  const returnTo = requested !== null && isLocalPath(requested) ? requested : DEFAULT_RETURN;

  // Already signed in: this page has nothing to offer, so it forwards rather
  // than showing a sign-in button that would start a second flow.
  if (session.signedIn && !stepUp) {
    return <Navigate to={returnTo} replace />;
  }

  const situation = stepUp ? SITUATION_BY_ID["step-up-required"] : SITUATION_BY_ID["session-expired"];

  return (
    <>
      <SitePageHead
        title={stepUp ? "Confirm it's you" : "Sign in"}
        lead={
          stepUp
            ? situation.sentence
            : "Signing in hands you to the identity provider. The backend sets an HTTP-only session cookie on the way back, and this app never sees a token."
        }
      />

      <SiteSection
        title={stepUp ? "A stronger sign-in" : "Continue"}
        lead={
          requested === null
            ? "You will land on your dashboard."
            : "You will come back to the page you were on."
        }
      >
        <div className="site-narrow">
          <Panel
            title={stepUp ? "Confirm this session" : "Sign in"}
            description="One navigation. Nodal never sees your password."
            actions={
              requested === null ? undefined : (
                <StatusBadge tone="info" title={returnTo}>
                  Returning to {returnTo}
                </StatusBadge>
              )
            }
          >
            <div className="form-actions">
              <Button
                variant="primary"
                onClick={() => {
                  beginSignIn({ returnTo, stepUp });
                }}
              >
                {stepUp ? situation.recovery.label : "Continue to sign in"}
              </Button>
            </div>
            <p className="mono-small signin-target">
              {LOGIN_PATH}
              {stepUp ? "?step_up=true" : ""}
            </p>
            <p className="site-panel-note">
              Anything you had typed on the page you came from is kept in this tab and put back
              when you return. It is never sent anywhere and it is cleared when the tab closes.
            </p>
            <p className="site-panel-note">{RISK_FOOTER}</p>
          </Panel>
        </div>
      </SiteSection>
    </>
  );
}
