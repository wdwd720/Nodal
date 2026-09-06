/**
 * Sign in.
 *
 * The button navigates to `GET /v1/auth/login`, which begins an OIDC
 * authorization-code flow with PKCE and ends by setting a server-side session
 * cookie. The app never handles a credential and never holds a token: there is
 * nothing in this page for a script on another origin to steal.
 */
import type { ReactNode } from "react";

import { LOGIN_PATH, startSignIn } from "../api/client.ts";
import { Button } from "../components/Button.tsx";
import { Disclosure } from "../components/Layout.tsx";
import { RISK_FOOTER, USDC_DISCLOSURE } from "../lib/honesty.ts";

export function SignIn(): ReactNode {
  return (
    <div className="signin">
      <div className="signin-card">
        <div className="brand brand-large">
          <span className="brand-mark" aria-hidden="true" />
          <span className="brand-name">Nodal</span>
        </div>
        <h1>Sign in</h1>
        <p className="lead">
          This session is not signed in. Signing in hands you to the identity provider; the backend
          sets an HTTP-only session cookie on the way back, and this app never sees a token.
        </p>
        <Button variant="primary" onClick={startSignIn}>
          Continue to sign in
        </Button>
        <p className="mono-small signin-target">{LOGIN_PATH}</p>

        <Disclosure title="What you are signing in to">
          <p>{USDC_DISCLOSURE}</p>
          <p>{RISK_FOOTER}</p>
        </Disclosure>
      </div>
    </div>
  );
}
