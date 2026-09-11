/**
 * The frame the three onboarding screens share, and the step indicator.
 *
 * Onboarding is deliberately outside both shells. The application shell offers
 * Markets, Portfolio and Activity, and offering them to somebody who has not
 * finished creating a profile is offering a door that the router will shut
 * again; the public shell offers Get started to somebody who has already
 * started. So this is its own frame: the brand, the steps, the screen, and the
 * one line of law that has to be on every one of them.
 *
 * `StepList` reads the steps as what the API says they are — timestamps, not a
 * state machine (D-053). They are independent, can be done in either order, and
 * cannot be undone, so the list shows what is complete rather than a position
 * in a sequence.
 */
import type { ReactNode } from "react";

import { useSession } from "../../session.tsx";
import { BrandLockup } from "../../components/Brand.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { RISK_FOOTER } from "../../lib/honesty.ts";

export type StepKey = "PROFILE" | "TERMS";

const STEP_LABELS: Readonly<Record<StepKey, string>> = {
  PROFILE: "Your profile",
  TERMS: "What you are agreeing to",
};

/**
 * The steps, each marked done or not from the API's own answer.
 *
 * It is a list rather than a progress bar because a bar implies an order that
 * the domain does not have, and because a customer who has done the terms but
 * not the profile is in a real state the product must be able to draw.
 */
export function StepList(props: { readonly current: StepKey }): ReactNode {
  const session = useSession();
  const steps = session.onboarding?.steps ?? [];
  const done = (key: StepKey): boolean =>
    steps.some((step) => step.key === key && step.complete === true);

  return (
    <ol className="onboarding-steps">
      {(["PROFILE", "TERMS"] as const).map((key) => {
        const complete = done(key);
        const isCurrent = key === props.current;
        return (
          <li key={key} {...(isCurrent ? { "aria-current": "step" as const } : {})}>
            <span className="onboarding-step-label">{STEP_LABELS[key]}</span>
            <StatusBadge tone={complete ? "good" : isCurrent ? "info" : "neutral"}>
              {complete ? "Done" : isCurrent ? "Now" : "Not yet"}
            </StatusBadge>
          </li>
        );
      })}
    </ol>
  );
}

export function OnboardingFrame(props: {
  readonly title: string;
  readonly lead: string;
  readonly step: StepKey | "DONE";
  readonly children: ReactNode;
}): ReactNode {
  return (
    <div className="site">
      <a className="skip-link" href="#main">
        Skip to main content
      </a>
      <header className="site-head">
        <div className="site-width site-head-inner">
          <BrandLockup />
        </div>
      </header>

      <main id="main" tabIndex={-1} className="site-main">
        <div className="site-width onboarding">
          <h1>{props.title}</h1>
          <p className="lead">{props.lead}</p>
          {props.children}
        </div>
      </main>

      <footer className="site-foot">
        <div className="site-width site-foot-legal">
          <p>{RISK_FOOTER}</p>
        </div>
      </footer>
    </div>
  );
}
