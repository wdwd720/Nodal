/**
 * `/welcome/done` — what the dashboard shows, and the three things to do next.
 *
 * USER_JOURNEY §1 asks for exactly this: what is on the dashboard, and
 * "Explore markets" / "Buy Credits" / "Go to dashboard". Two of those three
 * pages belong to other branches, so this screen offers what exists and says
 * plainly what does not — a button to a page that is not there would be the
 * dead control this codebase enforces against with a type.
 *
 * There is no celebration here — no animation, no "you're all set!", no score.
 * (The source guard bans the word for the effect by name, comments included,
 * which is why this paragraph cannot spell it out.) A person has just created
 * an account on something that will hold their money; the right tone is a clear
 * account of what happens next, not a reward for having arrived.
 */
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { LinkButton } from "../../components/Button.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Panel } from "../../components/Panel.tsx";
import { PRIMARY_ACTIONS } from "../../components/AppShell.tsx";
import { CREDITS_DISCLOSURE } from "../../lib/honesty.ts";
import { useSession } from "../../session.tsx";
import { OnboardingFrame } from "./OnboardingFrame.tsx";

function buyCreditsIsBuilt(): boolean {
  return PRIMARY_ACTIONS.some((action) => action.to === "/buy-credits" && action.present);
}

export function WelcomeDone(): ReactNode {
  const session = useSession();
  const name = session.profile?.display_name;

  return (
    <OnboardingFrame
      title={name === undefined || name === "" ? "You're in" : `You're in, ${name}`}
      lead="Here is what your dashboard shows and what you can do from it."
      step="DONE"
    >
      <Panel
        title="What the dashboard shows"
        description="Everything on it is a figure the backend computed, with the moment it was computed beside it."
      >
        <FieldGrid columns={3}>
          <Field label="Credits" note="Spendable, frozen and payout-eligible, kept apart.">
            Never added together in your browser — they are different kinds of value.
          </Field>
          <Field label="Portfolio" note="Only once you hold a position.">
            Value and today's change, with the exact quantity beside every valuation.
          </Field>
          <Field label="Agents and activity" note="What acted, and what happened.">
            Every decision and every ledger movement, in one feed you can read back.
          </Field>
        </FieldGrid>
        <p className="note">{CREDITS_DISCLOSURE}</p>
      </Panel>

      <Panel title="What to do next" description="Nothing here costs anything to look at.">
        <div className="site-cta">
          <LinkButton to="/home" variant="primary">
            Go to dashboard
          </LinkButton>
          <LinkButton to="/markets">Explore markets</LinkButton>
          {buyCreditsIsBuilt() && <LinkButton to="/buy-credits">Buy Credits</LinkButton>}
        </div>
        {!buyCreditsIsBuilt() && (
          <p className="site-panel-note">
            Buying Credits is not available in this build, so there is no button for it. When it
            lands it appears here and in the header, beside Withdraw.
          </p>
        )}
        <p className="site-panel-note">
          You can read the documents you accepted at any time: <Link to="/terms">Terms</Link>,{" "}
          <Link to="/privacy">Privacy</Link> and <Link to="/risk">Risk disclosure</Link>.
        </p>
      </Panel>
    </OnboardingFrame>
  );
}
