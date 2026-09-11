/**
 * Every route in the application, and the two decisions that separate them.
 *
 * D-077 divides the product into a public site that renders with no session at
 * all and an application that requires one. The division is structural rather
 * than conditional: public pages are children of `SiteFrame`, application pages
 * are children of a layout route that has already established a session, and
 * neither shape can render the other's chrome by accident.
 *
 * # The three answers to "who is this?"
 *
 * `useSession()` distinguishes them and the router treats each differently,
 * because collapsing them is how an interface tells somebody they are signed
 * out when the truth is that a server did not answer:
 *
 *   - **loading** — the backend has not said yet. An application route waits;
 *     a public route does not, because a marketing page has nothing to wait for.
 *   - **signedOut** — the backend answered 401. The visitor is sent to
 *     `/sign-in?return=<where they were going>`.
 *   - **neither** — the backend could not be reached. That is a network fact,
 *     not a claim about the session, so `Boot` waits for readiness and then
 *     reports what actually happened.
 *
 * # And the fourth: "have they finished arriving?"
 *
 * A session is not the same as a usable account. `OnboardingGate` sits between
 * the session and the application shell and sends anybody with an incomplete
 * profile, or an unaccepted document that is currently required, to the step
 * that fixes it — before any screen that could move value.
 *
 * # `/` belongs to both
 *
 * Signed out it is the landing page. Signed in it forwards to `/home`. The
 * return path after a sign-in is the API's now (`return_to` on
 * `GET /v1/auth/login`), so the callback usually lands on the page the customer
 * asked for and never passes through here at all.
 */
import { useEffect, type ReactNode } from "react";
import { Navigate, Outlet, Route, Routes, useLocation } from "react-router-dom";

import { useTermsState } from "./api/queries.ts";
import { AppShell } from "./components/AppShell.tsx";
import { Boot } from "./components/Boot.tsx";
import { Explanation, Loading } from "./components/DataState.tsx";
import { clearSignInPending, signInPending } from "./lib/survives-sign-in.ts";
import { signInPathFor, useSession } from "./session.tsx";

import { Activity } from "./pages/Activity.tsx";
import { AgentDetail } from "./pages/agents/AgentDetail.tsx";
import { AgentNew } from "./pages/agents/AgentNew.tsx";
import { AgentsList } from "./pages/agents/AgentsList.tsx";
import { Withdraw } from "./pages/withdraw/Withdraw.tsx";
import { CreateAsset } from "./pages/CreateAsset.tsx";
import { Home } from "./pages/Home.tsx";
import { Marketplace } from "./pages/Marketplace.tsx";
import { NativeMarkets } from "./pages/NativeMarkets.tsx";
import { NotFound } from "./pages/NotFound.tsx";
import { Portfolio } from "./pages/Portfolio.tsx";
import { Settings } from "./pages/Settings.tsx";

import { Welcome } from "./pages/onboarding/Welcome.tsx";
import { WelcomeDone } from "./pages/onboarding/WelcomeDone.tsx";
import { WelcomeTerms } from "./pages/onboarding/WelcomeTerms.tsx";

import { GetStarted } from "./pages/public/GetStarted.tsx";
import { HowItWorks } from "./pages/public/HowItWorks.tsx";
import { Landing } from "./pages/public/Landing.tsx";
import { Learn } from "./pages/public/Learn.tsx";
import { PolicyPage } from "./pages/public/Policy.tsx";
import { Product } from "./pages/public/Product.tsx";
import { ProductAgents } from "./pages/public/ProductAgents.tsx";
import { ProductMarkets } from "./pages/public/ProductMarkets.tsx";
import { Security } from "./pages/public/Security.tsx";
import { SignIn } from "./pages/public/SignIn.tsx";
import { SiteFrame } from "./pages/public/SiteChrome.tsx";

/** Where a signed-in visitor to `/` ends up when nothing else was requested. */
const HOME = "/home";

/**
 * The one application route the onboarding gate lets through.
 *
 * USER_JOURNEY §1 says an unaccepted document routes to the terms step "before
 * anything financial". Settings is not financial: it is where somebody reads
 * their own audit trail, revokes a session they do not recognise, or asks for
 * their account to be closed, and locking those behind an acceptance would be
 * using the law as a hostage. Everything else waits.
 */
const UNGATED = "/settings";

/** The public shell. Every signed-out page is a child of this route. */
function PublicLayout(): ReactNode {
  return (
    <SiteFrame>
      <Outlet />
    </SiteFrame>
  );
}

/**
 * Establishes a session, or says honestly why it could not.
 *
 * It is a component rather than a hook so that the three answers are three
 * returns rather than three flags a caller has to remember to check.
 */
function SessionBoundary(props: { readonly children: ReactNode }): ReactNode {
  const session = useSession();
  const location = useLocation();

  if (session.loading) {
    return (
      <div className="boot">
        <Loading label="Checking this session with the backend…" />
      </div>
    );
  }

  // Only the backend's own 401 sends somebody to sign in. A failure to REACH
  // the backend is a different thing, and telling a signed-in customer they are
  // not signed in would be the application asserting something it does not
  // know.
  if (session.signedOut) {
    return <Navigate to={signInPathFor(location)} replace />;
  }

  if (!session.signedIn) {
    return <Boot error={session.error} onReady={session.refetch} />;
  }

  return <>{props.children}</>;
}

/**
 * Sends anybody who has not finished arriving to the step that finishes it.
 *
 * Two different facts are checked, because the API reports them separately and
 * only one of them can change after onboarding is done:
 *
 *   - `onboarding` on `/me` is timestamps (D-053). A missing profile or an
 *     incomplete step means the person never finished, and `next_step` says
 *     which one to show.
 *   - `outstanding` on `/me/terms-acceptances` is the server comparing what was
 *     accepted against the sha256 of the bytes it serves TODAY. A re-issued
 *     document makes it non-empty again while the onboarding timestamp stays
 *     set, so a client that only read `onboarding.complete` would let somebody
 *     trade under a document they never saw.
 *
 * The terms read is deliberately never served from cache, and this gate is a
 * layout route: it mounts once per page load rather than once per navigation,
 * so being strict costs one request rather than one per click.
 */
function OnboardingGate(props: { readonly children: ReactNode }): ReactNode {
  const session = useSession();
  const location = useLocation();
  const terms = useTermsState(session.signedIn);

  if (location.pathname.startsWith(UNGATED)) return <>{props.children}</>;

  const onboarding = session.onboarding;
  if (onboarding === undefined || !onboarding.complete) {
    const next = onboarding?.next_step;
    return <Navigate to={next === "TERMS" ? "/welcome/terms" : "/welcome"} replace />;
  }

  // A failure to READ the terms state is not a reason to lock somebody out of
  // their own account: it is a fact about the request, and the pages behind
  // this gate report their own failures honestly. Only a definite "these are
  // outstanding" diverts.
  if (terms.isSuccess && terms.data.outstanding.length > 0) {
    return <Navigate to="/welcome/terms" replace />;
  }

  return <>{props.children}</>;
}

/** Onboarding: a session, its own frame, and no gate — this IS the gate's exit. */
function OnboardingLayout(): ReactNode {
  return (
    <SessionBoundary>
      <Outlet />
    </SessionBoundary>
  );
}

/**
 * The application shell, and everything in front of it.
 *
 * The gate is a layout route rather than a check per page so that there is
 * exactly one place where "this needs a session, and a finished one" is
 * decided. A page that has to remember to check is a page that will one day
 * forget.
 */
function RequireSession(): ReactNode {
  const session = useSession();
  return (
    <SessionBoundary>
      <OnboardingGate>
        <AppShell>
          {session.error !== undefined && session.error !== null && (
            <Explanation error={session.error} onRetry={session.refetch} />
          )}
          <Outlet />
        </AppShell>
      </OnboardingGate>
    </SessionBoundary>
  );
}

/**
 * `/`.
 *
 * The sign-in return path is server-held now, so a callback normally lands on
 * the page the customer asked for. It still lands here when nothing was asked
 * for, and a marker set before the browser left says whether somebody is
 * mid-sign-in — without it this route would paint the marketing page for the
 * fraction of a second `GET /v1/me` takes to answer, at exactly the person who
 * has just finished signing in.
 */
function Root(): ReactNode {
  const session = useSession();
  const returning = signInPending();

  useEffect(() => {
    if (session.signedIn || session.signedOut) clearSignInPending();
  }, [session.signedIn, session.signedOut]);

  if (session.signedIn) {
    return <Navigate to={HOME} replace />;
  }

  if (session.loading && returning) {
    return (
      <div className="boot">
        <Loading label="Signing you in…" />
      </div>
    );
  }

  return (
    <SiteFrame>
      <Landing />
    </SiteFrame>
  );
}

/**
 * An address that matches nothing.
 *
 * It is framed by whichever shell the visitor belongs in. Sending a signed-out
 * visitor to sign in because they mistyped a URL would be a confusing answer to
 * a simple mistake, so a public 404 stays public.
 */
function NotFoundRoute(): ReactNode {
  const session = useSession();
  if (session.signedIn) {
    return (
      <AppShell>
        <NotFound />
      </AppShell>
    );
  }
  return (
    <SiteFrame>
      <NotFound />
    </SiteFrame>
  );
}

export function App(): ReactNode {
  return (
    <Routes>
      <Route path="/" element={<Root />} />

      {/* The public site. No session is asked for, and none is needed. */}
      <Route element={<PublicLayout />}>
        <Route path="/product" element={<Product />} />
        <Route path="/product/markets" element={<ProductMarkets />} />
        <Route path="/product/agents" element={<ProductAgents />} />
        <Route path="/how-it-works" element={<HowItWorks />} />
        <Route path="/security" element={<Security />} />
        <Route path="/learn" element={<Learn />} />
        <Route path="/get-started" element={<GetStarted />} />
        <Route path="/sign-in" element={<SignIn />} />
        {/* The policy pages are public because somebody must be able to read
            what the product does before they have an account to accept
            anything with. The documents an acceptance RECORDS are the API's,
            and they are shown in full at `/welcome/terms`. */}
        <Route path="/terms" element={<PolicyPage slug="terms" />} />
        <Route path="/privacy" element={<PolicyPage slug="privacy" />} />
        <Route path="/risk" element={<PolicyPage slug="risk" />} />
      </Route>

      {/* Onboarding: a session, but not the shell and not the gate. */}
      <Route element={<OnboardingLayout />}>
        <Route path="/welcome" element={<Welcome />} />
        <Route path="/welcome/terms" element={<WelcomeTerms />} />
        <Route path="/welcome/done" element={<WelcomeDone />} />
      </Route>

      {/* The application. Everything below here has a session and a finished
          onboarding. */}
      <Route element={<RequireSession />}>
        <Route path="/home" element={<Home />} />
        <Route path="/markets" element={<NativeMarkets />} />
        {/* The internal product marketplace. It moves under the markets agent's
            own pages; the route is D-077's and does not change with it. */}
        <Route path="/markets/products" element={<Marketplace />} />
        <Route path="/create-asset" element={<CreateAsset />} />
        <Route path="/agents" element={<AgentsList />} />
        <Route path="/agents/new" element={<AgentNew />} />
        <Route path="/agents/:agentId" element={<AgentDetail />} />
        <Route path="/withdraw" element={<Withdraw />} />
        <Route path="/portfolio" element={<Portfolio />} />
        <Route path="/activity" element={<Activity />} />
        <Route path="/settings" element={<Settings />} />
      </Route>

      <Route path="*" element={<NotFoundRoute />} />
    </Routes>
  );
}
