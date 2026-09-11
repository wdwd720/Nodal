/**
 * `/sign-in` — and the return path.
 *
 * The button navigates to `GET /v1/auth/login`, which begins an OIDC
 * authorization-code flow with PKCE and ends by setting a server-side session
 * cookie. The app never handles a credential and never holds a token: there is
 * nothing on this page for a script on another origin to steal.
 *
 * # Why the return path is held in the browser
 *
 * `?return=/withdraw` says where the customer was going. The obvious thing
 * would be to hand it to the API and let the callback redirect there — the
 * backend's login attempt has a `return_to` column and `internal/identity`
 * validates it as a local path — but `GET /v1/auth/login` in `openapi.yaml`
 * takes only `step_up`, so there is no parameter to put it in. Changing the API
 * is not this branch's to make.
 *
 * So the path is remembered in the tab (`survives-sign-in.ts`), the callback
 * lands on the configured post-login URL, and the app forwards from there. The
 * remembered value is validated as a local path on the way in and on the way
 * out, because a return path that could name another origin is an open redirect
 * with a friendly name.
 *
 * The gap is recorded for the API: a `return_to` parameter on the login
 * endpoint would let the backend do this, which is one fewer place a redirect
 * target can be tampered with.
 */
import { useEffect, type ReactNode } from "react";
import { Navigate, useSearchParams } from "react-router-dom";

import { LOGIN_PATH } from "../../api/client.ts";
import { Button } from "../../components/Button.tsx";
import { Panel } from "../../components/Panel.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { SITUATION_BY_ID } from "../../lib/errors.ts";
import { RISK_FOOTER } from "../../lib/honesty.ts";
import { isLocalPath, rememberReturnPath } from "../../lib/survives-sign-in.ts";
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

  // Remember it as soon as the page renders rather than at the click, so a
  // customer who signs in from another tab, or whose click lands after a
  // reload, still comes back to where they were going.
  useEffect(() => {
    rememberReturnPath(returnTo);
  }, [returnTo]);

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
