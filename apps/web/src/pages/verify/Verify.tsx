/**
 * `/verify` — identity verification, and the one sentence it is allowed to
 * promise.
 *
 *   "Verify your identity to enable withdrawal eligibility."
 *
 * Goal §19 forbids the other sentence — the one that offers verification as a
 * way of turning Credits into spendable money — and the architecture is built
 * so that the forbidden one could not be true: a verification decision changes
 * a financial profile and touches no Credit lot anywhere in the system. This
 * page therefore says what verification establishes and never what it unlocks
 * in money terms. What a level then permits is `/withdraw`'s answer, computed
 * by a different engine from different inputs.
 *
 * THERE IS NO PERSONAL DATA ON THIS SCREEN, and that is a property of the API
 * rather than a choice made here: the profile carries a state, a level, the
 * sub-checks behind that level and what is missing. There is no document
 * identifier, no government number and no date of birth, because a provider
 * holds the evidence and Nodal holds the conclusion.
 *
 * THE JURISDICTION IS ASKED FOR, NEVER INFERRED. A country derived from a
 * network address is a legal determination wearing a network header's clothes.
 * The API refuses to make one, so this screen asks.
 *
 * THE SANDBOX CONTROL IS RENDERED ONLY WHERE THE API SAYS IT EXISTS. On a
 * sandbox tier a rehearsal session carries `sandbox_control_path`, and its
 * presence is what makes a rehearsal visibly a rehearsal. Everywhere else the
 * route answers FORBIDDEN and nothing is drawn. It is labelled as a control
 * that chooses what the rehearsal decides, and never as an approval: nobody has
 * assessed anybody.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useEligibility,
  usePollVerification,
  useSandboxOutcome,
  useStartVerification,
  useVerification,
  type SandboxOutcome,
  type VerificationCheck,
  type VerificationSession,
} from "../../api/queries.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { EmptyState, Explanation } from "../../components/DataState.tsx";
import {
  Disclosure,
  Field,
  FieldGrid,
  FormField,
  Identifier,
  Page,
  Panel,
  StatusBadge,
  type Tone,
} from "../../components/Layout.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { formatInstant } from "../../lib/time.ts";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { useActiveAccountId } from "../../session.tsx";

/** How often an open session is re-read while the provider has not answered. */
const POLL_MS = 2_000;

/* --------------------------------------------------------------------------
 * The state machine, in the customer's words (§20)
 * ------------------------------------------------------------------------ */

interface StateCopy {
  readonly tone: Tone;
  readonly heading: string;
  readonly sentence: string;
}

const STATE_COPY: Readonly<Record<string, StateCopy>> = {
  UNVERIFIED: {
    tone: "neutral",
    heading: "Not started",
    sentence:
      "Nobody has checked who you are. Nothing else in the product needs this; it is what a withdrawal needs.",
  },
  REQUIRED: {
    tone: "warn",
    heading: "Needed before a withdrawal",
    sentence: "A withdrawal has been asked for and this has to happen before it can go further.",
  },
  STARTED: {
    tone: "info",
    heading: "Started",
    sentence: "A session is open with the provider and has not been completed yet.",
  },
  PENDING: {
    tone: "info",
    heading: "With the provider",
    sentence:
      "The provider is deciding. Nodal receives the outcome from the provider and never from this browser, so there is nothing to confirm here.",
  },
  NEEDS_INFORMATION: {
    tone: "warn",
    heading: "The provider needs something else",
    sentence: "The provider could not finish with what it was given. What it asked for is below.",
  },
  VERIFIED: {
    tone: "good",
    heading: "Verified",
    sentence:
      "The provider verified this identity. That establishes who you are; what may be withdrawn is decided separately, per origin.",
  },
  REJECTED: {
    tone: "bad",
    heading: "Not verified",
    sentence:
      "The provider did not verify this identity. Nodal does not overturn that decision, and the provider's next step, where it gave one, is below.",
  },
  EXPIRED: {
    tone: "warn",
    heading: "Expired",
    sentence: "A verification does not last forever. This one has lapsed and has to be done again.",
  },
  RESTRICTED: {
    tone: "bad",
    heading: "Restricted",
    sentence:
      "A restriction on this account stands in the way. It is recorded against the account and nothing entered here changes it.",
  },
  SUSPENDED: {
    tone: "bad",
    heading: "Suspended",
    sentence: "This profile is suspended. Support is the only route from here.",
  },
};

function stateCopy(state: string): StateCopy {
  return (
    STATE_COPY[state] ?? {
      tone: "neutral",
      heading: state,
      sentence: "The backend reported a state this page has no sentence for. It is shown unchanged.",
    }
  );
}

/** What each level actually establishes. Never what it is worth. */
const LEVEL_COPY: Readonly<Record<string, string>> = {
  NONE: "Nothing has been established about who holds this account.",
  NODAL_IDENTITY:
    "A verified e-mail address. It says nothing about who you are and nothing about whether you may receive money.",
  PAYOUT_KYC:
    "A provider has identified the person behind this account, with the sub-checks below as the evidence.",
  ENHANCED: "A provider has identified the person behind this account to a higher standard.",
};

/** The five things §21 says must each be able to refuse on their own. */
const CHECK_KINDS: readonly string[] = [
  "IDENTITY_DOCUMENT",
  "AGE",
  "JURISDICTION",
  "SANCTIONS",
  "PEP",
];

const CHECK_LABELS: Readonly<Record<string, string>> = {
  IDENTITY_DOCUMENT: "Identity",
  AGE: "Age",
  JURISDICTION: "Jurisdiction",
  SANCTIONS: "Sanctions screening",
  PEP: "Politically exposed person",
};

function outcomeTone(outcome: string): Tone {
  switch (outcome) {
    case "PASS":
      return "good";
    case "FAIL":
      return "bad";
    case "NEEDS_INFORMATION":
      return "warn";
    case "NOT_APPLICABLE":
      return "neutral";
    default:
      return "warn";
  }
}

const SESSION_STATUS_COPY: Readonly<Record<string, string>> = {
  CREATED: "Opened. Nobody has done anything with it yet.",
  PENDING_USER_ACTION: "Waiting for you to finish the provider's flow.",
  PROCESSING: "The provider is working on it.",
  REQUIRES_INPUT: "The provider asked for something else.",
  MANUAL_REVIEW: "A person at the provider is looking at it.",
  APPROVED: "The provider approved it.",
  DECLINED: "The provider declined it.",
  CANCELLED: "It was cancelled.",
  EXPIRED: "It lapsed before it was finished.",
};

/** True while the provider might still say something new. */
function sessionOpen(status: string): boolean {
  return (
    status === "CREATED" ||
    status === "PENDING_USER_ACTION" ||
    status === "PROCESSING" ||
    status === "REQUIRES_INPUT" ||
    status === "MANUAL_REVIEW"
  );
}

/* --------------------------------------------------------------------------
 * The sandbox control
 * ------------------------------------------------------------------------ */

const SANDBOX_OUTCOMES: readonly SandboxOutcome[] = [
  "VERIFIED",
  "NEEDS_INFORMATION",
  "REJECTED",
  "UNDERAGE",
  "SANCTIONED",
];

const SANDBOX_LABELS: Readonly<Record<string, string>> = {
  VERIFIED: "Decide: verified",
  NEEDS_INFORMATION: "Decide: needs information",
  REJECTED: "Decide: rejected",
  UNDERAGE: "Decide: under age",
  SANCTIONED: "Decide: sanctions hit",
};

/**
 * The rehearsal outcome chooser.
 *
 * It exists so the whole withdrawal journey can be exercised without
 * fabricating an approval, and the wording is the point: it says what the
 * REHEARSAL decides. There is no default anywhere in this path — a rehearsal
 * session nobody answers stays pending forever — because "approved unless told
 * otherwise" is a fabricated approval with extra steps.
 */
function SandboxControl(props: { readonly accountId: string }): ReactNode {
  const choose = useSandboxOutcome();
  const [pending, setPending] = useState<SandboxOutcome | undefined>(undefined);

  return (
    <Panel
      title="Sandbox control"
      description="This deployment rehearses verification. Nobody has assessed anybody."
      temp="simulated"
    >
      <p>
        This control chooses what the REHEARSAL provider decides. It is not an approval, it is not
        a verification, and no provider has looked at any document — there are no documents. Every
        row it writes is labelled as a rehearsal, and the database refuses that label in
        production.
      </p>
      {choose.isError && <Explanation error={choose.error} onRetry={choose.reset} />}
      <div className="form-actions">
        {SANDBOX_OUTCOMES.map((outcome) => (
          <Button
            key={outcome}
            variant="secondary"
            busy={choose.isPending && pending === outcome}
            busyLabel="Recording…"
            onClick={() => {
              setPending(outcome);
              choose.mutate({
                accountId: props.accountId,
                outcome,
                idempotencyKey: newIdempotencyKey(),
              });
            }}
          >
            {SANDBOX_LABELS[outcome] ?? outcome}
          </Button>
        ))}
      </div>
      <p className="note">
        Under age and sanctions are separate from rejected because §21 requires an age failure and
        a sanctions failure to be separately expressible. They are different facts with different
        consequences, and collapsing them would lose one.
      </p>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * One open session
 * ------------------------------------------------------------------------ */

function OpenSession(props: {
  readonly accountId: string;
  readonly session: VerificationSession;
  readonly hostedUrl?: string;
  /**
   * What the API says instead of a link when this answer is a REPLAY of an
   * Idempotency-Key.
   *
   * The hosted link is a single-use credential for resuming somebody's identity
   * check, and it is not written down -- not in the idempotency record either,
   * which is what D-125 fixed. So a retry of the same key comes back with the
   * session and no link, and the page has to say so and offer the next step
   * rather than leaving somebody in front of a button that is not there.
   */
  readonly resume?: string;
}): ReactNode {
  const { session } = props;
  const polled = usePollVerification(
    props.accountId,
    session.session_id,
    sessionOpen(session.status) ? { refetchMs: POLL_MS } : {},
  );
  const current = polled.data ?? session;
  const hosted = props.hostedUrl;
  const isPage = hosted !== undefined && /^https:\/\//.test(hosted);

  return (
    <>
      <FieldGrid columns={2}>
        <Field label="Session" note={SESSION_STATUS_COPY[current.status] ?? "Reported unchanged."}>
          <StatusBadge tone={sessionOpen(current.status) ? "info" : "neutral"}>
            {current.status}
          </StatusBadge>
        </Field>
        <Field label="Provider">
          {current.provider}
          {current.sandbox && <StatusBadge tone="warn">rehearsal</StatusBadge>}
        </Field>
        <Field label="Opened">{formatInstant(current.created_at)}</Field>
        <Field label="Session id">
          <Identifier value={current.session_id} />
        </Field>
      </FieldGrid>
      {current.failure_reason !== undefined && current.failure_reason !== "" && (
        <p className="note">{current.failure_reason}</p>
      )}
      {isPage && (
        <div className="form-actions">
          <a className="btn btn-primary" href={hosted} rel="noreferrer">
            Continue at the provider
          </a>
        </div>
      )}
      {hosted !== undefined && !isPage && (
        <p className="note">
          The provider returned a reference rather than a page: <code className="mono-small">{hosted}</code>.
          There is no hosted flow to visit on this deployment, which is what a rehearsal looks like.
        </p>
      )}
      {hosted === undefined && props.resume !== undefined && props.resume !== "" && (
        <p className="note" data-testid="verification-resume">
          {props.resume} Use “Start again” below: the link is handed to one browser and kept nowhere,
          so there is nothing to hand back.
        </p>
      )}
      <p className="note">
        Coming back from the provider says you came back, not that you passed. Nodal asks the
        provider what happened rather than believing the redirect, and this panel refreshes itself
        while the answer is still open.
      </p>
    </>
  );
}

/* --------------------------------------------------------------------------
 * The page
 * ------------------------------------------------------------------------ */

interface Jurisdiction {
  readonly country: string;
  readonly region: string;
}

const NO_JURISDICTION: Jurisdiction = { country: "", region: "" };

export function Verify(): ReactNode {
  const accountId = useActiveAccountId();
  const profile = useVerification(accountId);
  const eligibility = useEligibility(accountId);
  const start = useStartVerification();
  const where = useSurvivesSignIn<Jurisdiction>("verify.jurisdiction", NO_JURISDICTION);

  if (accountId === undefined) {
    return (
      <Page title="Verify your identity">
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is no profile to verify."
        />
      </Page>
    );
  }

  if (profile.isPending) {
    return (
      <Page title="Verify your identity">
        <Panel title="Reading your profile" description="From the backend, which is the only source.">
          <Skeleton shape="rows" count={4} label="Your verification profile is loading" />
        </Panel>
      </Page>
    );
  }

  const data = profile.data;
  if (profile.isError || data === undefined) {
    return (
      <Page title="Verify your identity">
        <Panel
          title="Your profile could not be read"
          description="Nothing about it is shown, because nothing about it is known."
        >
          <Explanation
            error={profile.error}
            onRetry={() => {
              void profile.refetch();
            }}
          />
        </Panel>
      </Page>
    );
  }

  const copy = stateCopy(data.state);
  const checks = data.checks ?? [];
  const missing = data.missing ?? [];
  const session = start.data?.session ?? data.session;
  const sandboxControl = start.data?.sandbox_control_path;
  const countryOk = /^[A-Za-z]{2}$/.test(where.value.country.trim());

  return (
    <Page
      title="Verify your identity"
      lead="Verification establishes who you are. It is what withdrawal eligibility needs, and it changes nothing about what your Credits are."
      actions={<LinkButton to="/withdraw">Back to Withdraw</LinkButton>}
    >
      <Panel
        title={copy.heading}
        description="Where this account stands, and what that does and does not mean."
      >
        <p>{copy.sentence}</p>
        <FieldGrid columns={2}>
          <Field label="State">
            <StatusBadge tone={copy.tone}>{data.state}</StatusBadge>
          </Field>
          <Field label="Level" note={LEVEL_COPY[data.level] ?? "A level this page has no sentence for."}>
            <StatusBadge tone={data.level === "NONE" ? "neutral" : "info"}>{data.level}</StatusBadge>
          </Field>
          <Field
            label="Stands between you and a withdrawal"
            note="Whether verification, by itself, is still in the way. It says nothing about whether a withdrawal is possible."
          >
            <StatusBadge tone={data.payout_ready ? "good" : "warn"}>
              {data.payout_ready ? "no longer verification" : "yes, verification"}
            </StatusBadge>
          </Field>
          <Field label="Jurisdiction" note="Supplied by you. It is never taken from a network address.">
            {data.jurisdiction_country === undefined || data.jurisdiction_country === "" ? (
              <span className="absent">not stated yet</span>
            ) : (
              <span className="mono-small">
                {data.jurisdiction_country}
                {data.jurisdiction_region === undefined || data.jurisdiction_region === ""
                  ? ""
                  : `-${data.jurisdiction_region}`}
              </span>
            )}
          </Field>
          <Field label="Verified at">
            {data.verified_at === undefined ? (
              <span className="absent">never</span>
            ) : (
              formatInstant(data.verified_at)
            )}
          </Field>
          <Field label="Lapses" note="A verification does not last forever.">
            {data.expires_at === undefined ? (
              <span className="absent">no expiry recorded</span>
            ) : (
              formatInstant(data.expires_at)
            )}
          </Field>
        </FieldGrid>

        {data.sandbox && (
          <p className="note">
            Some or all of this was established by a rehearsal provider. It is labelled as a
            rehearsal everywhere it appears, and it is not an assessment of anybody.
          </p>
        )}

        {data.provider_availability !== undefined && data.provider_availability !== "" && (
          <p className="note">
            Identity provider: {data.provider ?? "none configured"} ·{" "}
            <span className="mono-small">{data.provider_availability}</span>. A deployment with no
            contracted provider cannot verify anybody, and says so rather than leaving somebody
            waiting.
          </p>
        )}

        {(data.jurisdiction_refusals ?? []).length > 0 && (
          <>
            <p className="note">Why this jurisdiction is refused:</p>
            <ul className="reasons">
              {(data.jurisdiction_refusals ?? []).map((refusal) => (
                <li key={refusal}>
                  <span className="mono-small">{refusal}</span>
                </li>
              ))}
            </ul>
          </>
        )}

        {(data.restrictions ?? []).length > 0 && (
          <>
            <p className="note">Restrictions recorded against this account:</p>
            <ul className="reasons">
              {(data.restrictions ?? []).map((restriction) => (
                <li key={restriction}>
                  <span className="mono-small">{restriction}</span>
                </li>
              ))}
            </ul>
          </>
        )}
      </Panel>

      <Panel
        title="The checks"
        description="Five things, each able to refuse on its own, under the rule version that judged them."
      >
        <FieldGrid columns={2}>
          {CHECK_KINDS.map((kind) => {
            const check = checks.find((item: VerificationCheck) => item.kind === kind);
            if (check === undefined) {
              return (
                <Field
                  key={kind}
                  label={CHECK_LABELS[kind] ?? kind}
                  note="Not recorded. A check nobody has run is not a check that passed."
                >
                  <StatusBadge tone="neutral">not recorded</StatusBadge>
                </Field>
              );
            }
            return (
              <Field
                key={kind}
                label={CHECK_LABELS[kind] ?? kind}
                note={`${check.detail ?? "No detail was given."} Recorded by ${check.provider} under ${check.rules_version} at ${formatInstant(check.recorded_at)}.`}
              >
                <StatusBadge tone={outcomeTone(check.outcome)}>{check.outcome}</StatusBadge>
                {check.sandbox && <StatusBadge tone="warn">rehearsal</StatusBadge>}
              </Field>
            );
          })}
        </FieldGrid>
        <p className="note">
          UNKNOWN is neither a pass nor a fail. A provider that has not screened somebody has not
          cleared them either, and the difference matters more than the word suggests.
        </p>
        <p className="note">
          The minimum age this jurisdiction is judged against is {String(data.minimum_age)}, from the
          rule table at version <span className="mono-small">{data.rules_version}</span>.
        </p>
      </Panel>

      <Panel title="What is missing" description="One row per thing in the way, with what you can do about it.">
        {missing.length === 0 ? (
          <EmptyState
            title="Nothing is outstanding"
            body="The backend reported no missing requirement for this profile. That is its answer, not an absence of information."
          />
        ) : (
          <ul className="explain-fields">
            {missing.map((item) => (
              <li key={`${item.code}-${item.action}`}>
                <span className="mono-small">{item.code}</span> — {item.detail}{" "}
                <StatusBadge tone="info">{item.action}</StatusBadge>
              </li>
            ))}
          </ul>
        )}
      </Panel>

      {session !== undefined && (
        <Panel title="Your session with the provider" description="What it says, polled rather than assumed.">
          <OpenSession
            accountId={accountId}
            session={session}
            {...(start.data?.hosted_url === undefined ? {} : { hostedUrl: start.data.hosted_url })}
            {...(start.data?.resume === undefined ? {} : { resume: start.data.resume })}
          />
        </Panel>
      )}

      {sandboxControl !== undefined && sandboxControl !== "" && (
        <SandboxControl accountId={accountId} />
      )}

      <Panel
        title={session === undefined ? "Start verification" : "Start again"}
        description="It asks a provider to check identity, age, jurisdiction and sanctions. Nodal keeps the decision and nothing else."
      >
        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            if (!countryOk) return;
            start.mutate({
              accountId,
              country: where.value.country.trim().toUpperCase(),
              region: where.value.region.trim().toUpperCase(),
              // Minted at confirmation: asking twice must resume one session
              // rather than race two.
              idempotencyKey: newIdempotencyKey(),
            });
          }}
        >
          <FormField
            label="Country"
            hint="Two letters, ISO 3166-1 alpha-2. You state it; it is never taken from your network address."
          >
            {(field) => (
              <input
                className="input"
                type="text"
                maxLength={2}
                autoComplete="country"
                value={where.value.country}
                onChange={(event) => {
                  where.set({ ...where.value, country: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>
          <FormField
            label="State or region"
            hint="The subdivision code without its country prefix. Required where the rules depend on it, and the backend says so if it does."
          >
            {(field) => (
              <input
                className="input"
                type="text"
                maxLength={8}
                value={where.value.region}
                onChange={(event) => {
                  where.set({ ...where.value, region: event.target.value });
                }}
                {...field}
              />
            )}
          </FormField>

          {start.isError && <Explanation error={start.error} onRetry={start.reset} />}

          <div className="form-actions">
            {countryOk ? (
              <Button variant="primary" submit busy={start.isPending} busyLabel="Opening…">
                Start verification
              </Button>
            ) : (
              <Button disabledReason="Enter the two-letter code for the country whose rules should apply to you.">
                Start verification
              </Button>
            )}
          </div>
        </form>
        <Disclosure title="What Nodal keeps, and what it never sees">
          <p>
            The provider collects whatever it needs to identify you and keeps it. Nodal stores the
            decision, a reference to the provider's record and the timestamps — no document, no
            government number and no date of birth. There is nowhere in this system for one to go.
          </p>
          <p>
            You already have at most one open session. Asking again resumes it rather than racing
            it, so pressing this twice does not start two checks.
          </p>
        </Disclosure>
      </Panel>

      <Panel title="What this changes" description="And what it does not.">
        <p>
          Verifying changes your financial profile. It does not change what a Credit is, it moves
          nothing, and it is not a promise that any particular amount can be withdrawn: what may
          leave is decided per origin by a policy, against the balance you actually hold.
        </p>
        {eligibility.data !== undefined && (
          <p className="note">
            Withdrawal eligibility for this account right now:{" "}
            <StatusBadge tone={eligibility.data.eligible ? "good" : "warn"}>
              {eligibility.data.eligible ? "something may be withdrawn" : "nothing may be withdrawn yet"}
            </StatusBadge>{" "}
            — the breakdown, per origin, is on the Withdraw page.
          </p>
        )}
      </Panel>
    </Page>
  );
}
