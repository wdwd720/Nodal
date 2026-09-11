/**
 * `/withdraw` — the page that exists for everyone, including the people who
 * cannot use it.
 *
 * Goal §19 makes the shape of this screen a product boundary rather than a
 * layout choice, and four rules follow from it.
 *
 * ONE. THE PAGE IS NEVER HIDDEN. Somebody who cannot withdraw is shown what
 * withdrawal is, what would have to be true for it to happen, and what the next
 * step is. Hiding the page would be the product deciding on their behalf that
 * they do not need to know.
 *
 * TWO. THE SENTENCE MATTERS. "Verify your identity to enable withdrawal
 * eligibility" is the promise this product can keep. The wording goal §19
 * forbids — the one that offers verification as a way of turning Credits into
 * spendable money — is a promise it cannot, because verification changes a
 * financial profile and never changes what a Credit is. No verification writer
 * touches a Credit lot anywhere in this system, and this page does not imply
 * that one does.
 *
 * THREE. WHAT COULD LEAVE IS DECIDED PER ORIGIN. Eligibility is computed from
 * lots that carry where they came from — purchased value, trading gains,
 * promotional grants, creator earnings, refunds — and the policy decides per
 * origin, in a stated consumption order. So the page shows the breakdown and
 * the order, rather than one number that would be true of nothing.
 *
 * FOUR. REQUIRES_VERIFICATION IS A NEXT STEP AND NOT A DENIAL. The API says so
 * in its own enum, and the interface presents it as one: a different answer from
 * "no", with the action that changes it.
 *
 * WHAT THIS PAGE NEVER DOES. It never fabricates an eligibility answer, never
 * re-prices an expired quote, and never renders a refusal as an error or as a
 * zero. Every refusal on it is the API's: its code, its reason and the next
 * action it named.
 */
import { useEffect, useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useAcceptTerms,
  useCancelPayout,
  useCreatePayout,
  useEligibility,
  usePayout,
  usePayoutQuote,
  usePayouts,
  useTermsState,
  useVerification,
  type LegalDocument,
  type PayoutProvenanceSlice,
  type PayoutQuote,
  type PayoutRequest,
  type WithdrawalEligibility,
  type WithdrawalOriginBucket,
} from "../../api/queries.ts";
import { explain } from "../../api/problem.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { AsyncPanel, EmptyState, Explanation } from "../../components/DataState.tsx";
import { DataTable } from "../../components/DataTable.tsx";
import { Figure } from "../../components/Figure.tsx";
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
import { Refusal } from "../../components/Refusal.tsx";
import { SegmentedBar } from "../../components/SegmentedBar.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { EMPTY_STATES, situationForCode } from "../../lib/errors.ts";
import { CREDITS_DISCLOSURE, PROVENANCE_NOTE } from "../../lib/honesty.ts";
import { parseQuantityInput } from "../../lib/money.ts";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { formatInstant, secondsUntil } from "../../lib/time.ts";
import { useActiveAccountId } from "../../session.tsx";
import { Destinations } from "./Destinations.tsx";

/** Credits are held at six decimal places, exactly as the ledger holds them. */
const CREDIT_DECIMALS = 6;

/** How often a request that is still moving is re-read from the backend. */
const FOLLOW_MS = 2_000;

/** How often the quote's expiry countdown is redrawn. */
const TICK_MS = 1_000;

/** Redraws on a timer, so an expiry counts down rather than going stale. */
function useTick(enabled: boolean): number {
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!enabled) return;
    const handle = setInterval(() => {
      setTick((previous) => previous + 1);
    }, TICK_MS);
    return () => {
      clearInterval(handle);
    };
  }, [enabled]);
  return tick;
}

/* --------------------------------------------------------------------------
 * The words for each machine-readable answer
 * ------------------------------------------------------------------------ */

interface Copy {
  readonly tone: Tone;
  readonly sentence: string;
}

/**
 * Why value cannot leave, in the customer's words.
 *
 * Every one of these names something that could change, which is what the API's
 * enum promises. REQUIRES_VERIFICATION leads the list on purpose: it is the one
 * the person can act on themselves.
 */
const REASON_COPY: Readonly<Record<string, Copy>> = {
  REQUIRES_VERIFICATION: {
    tone: "warn",
    sentence:
      "Identity verification is the next step. This is not a refusal: verifying is the thing that changes the answer.",
  },
  ORIGIN_NOT_WITHDRAWABLE: {
    tone: "neutral",
    sentence:
      "The policy does not permit value of this origin to leave. It is a decision about what the value IS, not about you.",
  },
  CAPABILITY_INACTIVE: {
    tone: "warn",
    sentence:
      "A capability gate this needs is not active in this deployment. It is refused for everybody, not only for you.",
  },
  JURISDICTION_RESTRICTED: {
    tone: "bad",
    sentence: "The product is not offered for withdrawal in this jurisdiction.",
  },
  ACCOUNT_RESTRICTED: {
    tone: "bad",
    sentence: "A restriction on this account blocks it. The reason is recorded against the account.",
  },
  PROVIDER_UNAVAILABLE: {
    tone: "warn",
    sentence:
      "No configured provider can pay this out. Nodal instructs a provider and never moves value itself, so without one there is nothing to instruct.",
  },
  MINIMUM_NOT_MET: {
    tone: "warn",
    sentence:
      "Below the provider's minimum, judged net of fees — sub-minimum dust is destroyed rather than returned.",
  },
  FUNDING_NOT_SETTLED: {
    tone: "warn",
    sentence: "The value behind this has not settled yet. It cannot leave before it has arrived.",
  },
  HOLD_PERIOD_NOT_ELAPSED: {
    tone: "warn",
    sentence: "A hold period on this origin has not elapsed. It is a wait rather than a refusal.",
  },
  NO_VALUE: { tone: "neutral", sentence: "There is nothing of this kind to withdraw." },
  POLICY_INVALID: {
    tone: "bad",
    sentence:
      "The payout policy this deployment loaded is not usable. Nothing is being decided against a broken policy.",
  },
};

function reasonCopy(reason: string): Copy {
  return (
    REASON_COPY[reason] ?? {
      tone: "neutral",
      sentence: "The backend gave a reason this page has no sentence for. It is shown unchanged.",
    }
  );
}

/** What each payout state means, and who is acting. */
const STATE_COPY: Readonly<Record<string, Copy>> = {
  DRAFT: { tone: "neutral", sentence: "Recorded. Eligibility has not been evaluated yet." },
  ELIGIBILITY_CHECK: {
    tone: "info",
    sentence: "The policy is deciding which of your units may leave, origin by origin.",
  },
  VERIFICATION_REQUIRED: {
    tone: "warn",
    sentence:
      "This needs a verified financial profile before it goes further. Verification changes your profile; it does not change what a Credit is.",
  },
  VERIFICATION_PENDING: {
    tone: "warn",
    sentence:
      "Identity verification is with the provider. Nodal receives the outcome from the provider, never from this browser.",
  },
  VERIFIED: {
    tone: "good",
    sentence:
      "Eligible and reserved. It has not been given to a provider yet, so it can still be cancelled.",
  },
  SUBMITTED: {
    tone: "info",
    sentence:
      "Handed to the payout provider. It may already have been paid, so it can no longer be cancelled — only reconciled.",
  },
  PROVIDER_PENDING: {
    tone: "info",
    sentence:
      "The provider has accepted it and has not settled it yet. Nodal does not move value itself and is waiting for the provider to report an outcome.",
  },
  PAYOUT_STATUS_UNKNOWN: {
    tone: "warn",
    sentence:
      "Nodal could not read back what the provider did with this. It is held in a stated unknown rather than guessed at, and reconciliation resolves it.",
  },
  SETTLED: { tone: "good", sentence: "The provider reported that it settled." },
  FAILED: {
    tone: "bad",
    sentence: "The provider did not complete it. The reason is below where it gave one.",
  },
  REJECTED: {
    tone: "bad",
    sentence: "This was refused, and the reserved units went back to the lots they came from.",
  },
  REVERSED: { tone: "bad", sentence: "A settled payout was reversed afterwards." },
  MANUAL_REVIEW: { tone: "warn", sentence: "A person has to look at this before it goes further." },
};

function stateCopy(state: string): Copy {
  return (
    STATE_COPY[state] ?? {
      tone: "neutral",
      sentence: "The backend reported a state this page has no sentence for. It is shown unchanged.",
    }
  );
}

/** Still moving, so worth re-reading. */
function isMoving(state: string): boolean {
  return state !== "SETTLED" && state !== "REJECTED" && state !== "FAILED" && state !== "REVERSED";
}

/**
 * Cancellable states, as the payout service defines them.
 *
 * Anything given to a provider may already have been paid, and the only honest
 * way out of that is reconciliation. The control is offered only where the
 * backend would actually accept it.
 */
function isCancellable(state: string): boolean {
  return (
    state === "DRAFT" ||
    state === "ELIGIBILITY_CHECK" ||
    state === "VERIFICATION_REQUIRED" ||
    state === "VERIFICATION_PENDING" ||
    state === "VERIFIED"
  );
}

/** A Credit figure. Every one on this page goes through here. */
function Credits(props: { readonly base: string | undefined; readonly big?: boolean }): ReactNode {
  return (
    <Figure
      kind="money"
      value={props.base === undefined ? null : { base: props.base, scale: CREDIT_DECIMALS }}
      symbol="Credits"
      {...(props.big === true ? { big: true } : {})}
    />
  );
}

/** The order value leaves in, as the backend ranked it. */
function ProvenanceTable(props: {
  readonly slices: readonly PayoutProvenanceSlice[];
  readonly caption: string;
}): ReactNode {
  const ordered = [...props.slices].sort((a, b) => a.consumption_rank - b.consumption_rank);
  return (
    <DataTable
      caption={props.caption}
      rows={ordered}
      rowKey={(slice: PayoutProvenanceSlice) => `${slice.origin}-${String(slice.consumption_rank)}`}
      columns={[
        {
          key: "rank",
          header: "Leaves",
          cell: (slice: PayoutProvenanceSlice) => (
            <span className="mono-small">#{String(slice.consumption_rank)}</span>
          ),
        },
        {
          key: "origin",
          header: "Origin",
          cell: (slice: PayoutProvenanceSlice) => (
            <span className="mono-small">{slice.origin}</span>
          ),
        },
        {
          key: "quantity",
          header: "Amount",
          numeric: true,
          cell: (slice: PayoutProvenanceSlice) => <Credits base={slice.quantity} />,
        },
        {
          key: "returned",
          header: "Returned",
          cell: (slice: PayoutProvenanceSlice) =>
            slice.returned === true ? (
              <StatusBadge tone="neutral">given back</StatusBadge>
            ) : (
              <span className="absent">no</span>
            ),
        },
      ]}
    />
  );
}

/**
 * A refused command, in the product's fixed words.
 *
 * The code is the backend's; the sentence is the fixed one this product owes
 * for that situation (USER_JOURNEY §11), and where there is none, `explain()`'s
 * honest fallback. Nothing here is a provider message or a server string.
 */
function CommandRefused(props: { readonly what: string; readonly error: unknown }): ReactNode {
  const detail = explain(props.error);
  const situation = situationForCode(detail.code);

  // A session that has expired, or an action that needs a stronger sign-in, is
  // not a refusal the customer can read their way out of: it needs the round
  // trip, and `Explanation` is what renders one. Dressing it as a refusal would
  // state the problem correctly and leave no way past it, which is the failure
  // `explain()`'s `needsSignIn` and `needsStepUp` exist to prevent.
  if (detail.needsSignIn || detail.needsStepUp) {
    return <Explanation error={props.error} />;
  }

  return (
    <Refusal
      what={props.what}
      rule={situation === undefined ? detail.body : situation.sentence}
      code={detail.code}
      remedy={
        situation === undefined
          ? "Nothing was reserved and no Credits moved. The stable code above is what support acts on."
          : situation.recovery.kind === "none"
            ? "Nothing you can do changes this answer, and no amount of Credits changes it either."
            : situation.recovery.label
      }
      {...(detail.requestId === undefined ? {} : { correlationId: detail.requestId })}
    >
      {detail.fields.length > 0 && (
        <ul className="explain-fields">
          {detail.fields.map(([field, message]) => (
            <li key={field}>
              <span className="mono-small">{field}</span>: {message}
            </li>
          ))}
        </ul>
      )}
      {situation !== undefined && situation.recovery.kind === "go" && situation.recovery.to !== undefined && (
        <div className="form-actions">
          <LinkButton to={situation.recovery.to} variant="primary">
            {situation.recovery.label}
          </LinkButton>
        </div>
      )}
    </Refusal>
  );
}

/* --------------------------------------------------------------------------
 * The withdrawal disclosure (goal §60)
 * ------------------------------------------------------------------------ */

/**
 * The document required before a withdrawal.
 *
 * It is `requirement: WITHDRAWAL` and is deliberately not asked for at signup —
 * that would be the frontloaded KYC goal §6 forbids — so it is asked for here,
 * at the moment it applies, before a quote is taken.
 *
 * The bytes rendered are the bytes hashed: the acceptance records the sha256 of
 * exactly what the server served, so this shows `body` verbatim in a `<pre>`
 * rather than passing it through a renderer. A document that arrives without a
 * body cannot honestly be accepted, and this refuses to offer acceptance for
 * one rather than asking somebody to agree to a title.
 */
function WithdrawalDisclosure(props: { readonly doc: LegalDocument }): ReactNode {
  const accept = useAcceptTerms();
  const { doc } = props;
  const body = doc.body;

  return (
    <Panel
      title={doc.title}
      description="Required before a withdrawal, and asked for here rather than at signup."
    >
      {doc.counsel_review_required && (
        <p className="note">
          This document has not been reviewed by a lawyer. It is served as a draft and is labelled
          one rather than presented as settled.
        </p>
      )}
      {body === undefined || body === "" ? (
        <Refusal
          what={`The text of ${doc.title} did not arrive with this response.`}
          rule="An acceptance records the sha256 of the bytes that were shown, so a document with no body cannot be accepted."
          code="CONTRACT_INCOMPLETE"
          remedy="Reload the page. If it keeps happening the deployment is serving an incomplete document and nobody should be asked to accept it."
        />
      ) : (
        <>
          <div className="doc-scroll" role="group" aria-label={`${doc.title}, full text`} tabIndex={0}>
            <pre className="doc-source">{body}</pre>
          </div>
          <FieldGrid columns={2}>
            <Field label="Version">
              <span className="mono-small">{doc.version}</span>
            </Field>
            <Field label="These exact bytes" note="What your acceptance is recorded against.">
              <Identifier value={doc.content_hash} label="sha256" />
            </Field>
          </FieldGrid>
          {accept.isError && <Explanation error={accept.error} onRetry={accept.reset} />}
          <div className="form-actions">
            <Button
              variant="primary"
              busy={accept.isPending}
              busyLabel="Recording…"
              onClick={() => {
                accept.mutate({
                  documentIds: [doc.document_id],
                  idempotencyKey: newIdempotencyKey(),
                });
              }}
            >
              I have read this
            </Button>
          </div>
        </>
      )}
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * One request, followed
 * ------------------------------------------------------------------------ */

function RequestDetail(props: {
  readonly payoutId: string;
  readonly accountId: string;
}): ReactNode {
  const request = usePayout(props.payoutId, { refetchMs: FOLLOW_MS });
  const cancel = useCancelPayout();
  const [reason, setReason] = useState("");

  return (
    <AsyncPanel
      query={request}
      loadingLabel="Reading this request…"
      skeleton={<Skeleton shape="rows" count={3} label="This request is loading" />}
    >
      {(data: PayoutRequest) => {
        const copy = stateCopy(data.state);
        return (
          <>
            <FieldGrid columns={2}>
              <Field label="State" note={copy.sentence}>
                <StatusBadge tone={copy.tone}>{data.state}</StatusBadge>
                {data.sandbox === true && <StatusBadge tone="warn">rehearsal</StatusBadge>}
              </Field>
              <Field label="Requested">
                <Credits base={data.requested_quantity} />
              </Field>
              <Field
                label="Reserved"
                note="Held out of your spendable balance while this request is open."
              >
                <Credits base={data.reserved_quantity} />
              </Field>
              <Field label="Settled">
                <Credits base={data.settled_quantity} />
              </Field>
              <Field label="Policy that decided this">
                <span className="mono-small">{data.policy_version}</span>
              </Field>
              <Field label="Request id">
                <Identifier value={data.payout_id} />
              </Field>
            </FieldGrid>

            {data.sandbox === true && (
              <p className="note">
                This is a rehearsal against a sandbox provider. It settles about ten seconds after
                it is accepted and no value moves anywhere: nothing leaves Nodal, nothing arrives
                at a bank, and nobody is paid.
              </p>
            )}

            {data.verification_would_suffice === true && (
              <p className="note">
                Identity verification is the only obstacle to this request. That is a different
                answer from "no", and it is presented as one.
              </p>
            )}
            {data.required_verification !== undefined && data.required_verification !== "" && (
              <p className="note">
                The level of verification this needs:{" "}
                <span className="mono-small">{data.required_verification}</span>
              </p>
            )}
            {data.failure_reason !== undefined && data.failure_reason !== "" && (
              <p className="note">{data.failure_reason}</p>
            )}
            {(data.eligibility_reasons ?? []).length > 0 && (
              <ul className="reasons">
                {(data.eligibility_reasons ?? []).map((code) => (
                  <li key={code}>
                    <span className="mono-small">{code}</span> — {reasonCopy(code).sentence}
                  </li>
                ))}
              </ul>
            )}

            {(data.provenance ?? []).length > 0 && (
              <>
                <h4>What is leaving, and in which order</h4>
                <ProvenanceTable
                  slices={data.provenance ?? []}
                  caption="The provenance slices this payout consumed, in the order the policy consumed them"
                />
                <p className="note">{PROVENANCE_NOTE}</p>
              </>
            )}

            {cancel.isError && <CommandRefused what="This request was not cancelled." error={cancel.error} />}

            {isCancellable(data.state) ? (
              <>
                <FormField
                  label="Why you are cancelling"
                  hint="Recorded with the cancellation. At least three characters."
                >
                  {(field) => (
                    <input
                      className="input"
                      type="text"
                      maxLength={500}
                      value={reason}
                      onChange={(event) => {
                        setReason(event.target.value);
                      }}
                      {...field}
                    />
                  )}
                </FormField>
                <div className="form-actions">
                  {reason.trim().length < 3 ? (
                    <Button disabledReason="Say why you are cancelling. The reason is recorded with the request.">
                      Cancel this request
                    </Button>
                  ) : (
                    <Button
                      variant="danger"
                      busy={cancel.isPending}
                      busyLabel="Cancelling…"
                      onClick={() => {
                        cancel.mutate({
                          payoutId: data.payout_id,
                          accountId: props.accountId,
                          reason: reason.trim(),
                          idempotencyKey: newIdempotencyKey(),
                        });
                      }}
                    >
                      Cancel this request
                    </Button>
                  )}
                </div>
              </>
            ) : (
              <p className="note">
                This can no longer be cancelled. It has been given to a provider, which means it
                may already have been paid, and the only honest way out of that is reconciliation
                rather than an undo.
              </p>
            )}
          </>
        );
      }}
    </AsyncPanel>
  );
}

/* --------------------------------------------------------------------------
 * The page
 * ------------------------------------------------------------------------ */

export function Withdraw(): ReactNode {
  const accountId = useActiveAccountId();
  const eligibility = useEligibility(accountId);
  const verification = useVerification(accountId);
  const terms = useTermsState();
  const payouts = usePayouts(accountId);
  const quote = usePayoutQuote();
  const create = useCreatePayout();

  const amount = useSurvivesSignIn<string>("withdraw.amount", "");
  const destination = useSurvivesSignIn<string>("withdraw.destination-id", "");
  /**
   * The key minted at the moment of confirmation.
   *
   * It survives the sign-in trip on purpose (USER_JOURNEY §10): a session that
   * expires between pressing the button and the backend answering leaves the
   * customer unsure whether a payout exists, and the only way to make the retry
   * safe is to retry the SAME request. A new key would be a second request.
   */
  const confirmKey = useSurvivesSignIn<string>("withdraw.confirm-key", "");
  const [following, setFollowing] = useState<string | undefined>(undefined);

  const tick = useTick(quote.data !== undefined);
  const parsed = parseQuantityInput(amount.value, CREDIT_DECIMALS);
  const selected = following ?? create.data?.payout_id;
  const chosenDestination = destination.value === "" ? undefined : destination.value;

  if (accountId === undefined) {
    return (
      <Page title="Withdraw">
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is no balance to withdraw from."
        />
      </Page>
    );
  }

  const disclosure = (terms.data?.documents ?? []).find(
    (doc: LegalDocument) => doc.requirement === "WITHDRAWAL",
  );
  const disclosureAccepted = disclosure !== undefined && disclosure.accepted;
  const live = quote.data;
  const secondsLeft = live === undefined ? undefined : secondsUntil(live.expires_at);
  const quoteExpired = secondsLeft !== undefined && secondsLeft <= 0;
  const canSubmit =
    live !== undefined &&
    !quoteExpired &&
    live.minimum_ok &&
    chosenDestination !== undefined &&
    disclosureAccepted;

  return (
    <Page
      title="Withdraw"
      lead="A withdrawal is a request to convert eligible value and have a licensed provider pay it out. Nodal instructs the provider; it never converts anything itself."
      actions={<LinkButton to="/verify">Identity verification</LinkButton>}
    >
      <Panel title="What a withdrawal is, and what it needs" description="Before any figure on this page.">
        <p>
          Credits are internal platform value and are not directly withdrawable. What can leave is
          value the eligibility policy has decided may leave, and that decision is made per origin —
          where each unit came from — rather than against your total.
        </p>
        <p>Three things have to be true before any request can be settled:</p>
        <ul className="explain-fields">
          <li>
            <strong>A verified financial profile.</strong> Verification asks a provider to check
            identity, age, jurisdiction and sanctions. Nodal stores the decision, a provider
            reference and the timestamps — never a document, and never the underlying details.
            Verifying changes your profile. It does not change what a Credit is and it moves
            nothing.
          </li>
          <li>
            <strong>An approved payout destination.</strong> A destination is held as a provider
            token or a sandbox handle. Nodal never takes a raw account number.
          </li>
          <li>
            <strong>The withdrawal disclosure, read.</strong> It is asked for here rather than at
            signup, because that is where it applies.
          </li>
        </ul>
        <p className="note">{CREDITS_DISCLOSURE}</p>
        {verification.data !== undefined && (
          <p className="note">
            Your verification state:{" "}
            <StatusBadge tone={verification.data.payout_ready ? "good" : "warn"}>
              {verification.data.state}
            </StatusBadge>{" "}
            at level <span className="mono-small">{verification.data.level}</span>.
          </p>
        )}
      </Panel>

      <Panel
        title="What could be withdrawn"
        description="Decided per origin by the policy the backend names, not by the total."
      >
        <AsyncPanel
          query={eligibility}
          loadingLabel="Asking the backend what may leave…"
          skeleton={<Skeleton shape="rows" count={4} label="Your withdrawal eligibility is loading" />}
        >
          {(data: WithdrawalEligibility) => (
            <>
              <FieldGrid columns={3}>
                <Field
                  label="Withdrawable now"
                  note="What could actually leave at this moment, composing every rule below."
                  emphasis
                >
                  <Credits base={data.withdrawable_now} big />
                </Field>
                <Field
                  label="Eligible by origin"
                  note="What the policy permits by provenance alone, before the rest is applied."
                >
                  <Credits base={data.payout_eligible} />
                </Field>
                <Field label="Everything you hold" note="Gross. It is not what can leave.">
                  <Credits base={data.gross} />
                </Field>
              </FieldGrid>

              {/* TWO segments, because the API reports two and they are exactly
                  the whole: `ineligible` IS `gross - payout_eligible`
                  (`internal/eligibility/withdrawal.go`), so these two account
                  for every Credit and nothing is drawn twice.

                  `frozen` is not a third part. A frozen lot is a disputed lot,
                  no payout policy permits one, and it is therefore already
                  inside `ineligible` — drawing it beside the other two made the
                  bar claim more than the whole it was drawn against, and made
                  the frozen Credits look like value held back twice. It is a
                  figure beside the bar instead, with the sentence that says
                  where in the picture it lives. */}
              <SegmentedBar
                caption="Your Credits by what may leave"
                scale={CREDIT_DECIMALS}
                symbol="Credits"
                total={data.gross}
                segments={[
                  {
                    key: "eligible",
                    label: "Eligible",
                    baseUnits: data.payout_eligible,
                    explanation: "The policy permits value of this provenance to leave.",
                    texture: "solid",
                  },
                  {
                    key: "ineligible",
                    label: "Not eligible",
                    baseUnits: data.ineligible,
                    explanation:
                      "Everything else you hold, held back by the policy for the reasons listed below.",
                    texture: "hatch",
                  },
                ]}
              />

              <FieldGrid columns={2}>
                <Field
                  label="Frozen"
                  note="Held because of a dispute or an adjustment on this account. It is inside the ineligible part of the bar above rather than beside it: no payout policy permits a disputed lot to leave, so freezing value cannot make it eligible or add to what you hold."
                >
                  <Credits base={data.frozen} />
                </Field>
                <Field
                  label="Spendable"
                  note="A different question with a different answer: what may be used inside Nodal now. Spending and withdrawing are not the same permission."
                >
                  <Credits base={data.spendable} />
                </Field>
              </FieldGrid>

              <FieldGrid columns={3}>
                <Field label="Anything at all" note="The composed answer, not one of its parts.">
                  <StatusBadge tone={data.eligible ? "good" : "warn"}>
                    {data.eligible ? "something may be withdrawn" : "nothing may be withdrawn yet"}
                  </StatusBadge>
                </Field>
                <Field label="Your verification" note="What this policy requires beside it.">
                  <span className="mono-small">
                    {data.current_verification} → {data.required_verification}
                  </span>
                </Field>
                <Field
                  label="Provider minimum"
                  note="Judged net of fees. Zero is never 'any amount will do'; it means the provider publishes none."
                >
                  <Credits base={data.minimum_quantity} />
                </Field>
                <Field label="Payout provider">
                  {data.provider === undefined || data.provider === "" ? (
                    <span className="absent">none configured</span>
                  ) : (
                    <>
                      <span className="mono-small">{data.provider}</span>
                      {data.sandbox && <StatusBadge tone="warn">rehearsal</StatusBadge>}
                    </>
                  )}
                </Field>
                <Field label="Provider reachable">
                  <StatusBadge tone={data.provider_available === true ? "good" : "warn"}>
                    {data.provider_available === true ? "yes" : "no"}
                  </StatusBadge>
                </Field>
                <Field label="Jurisdiction offered">
                  <StatusBadge tone={data.jurisdiction_supported === true ? "good" : "warn"}>
                    {data.jurisdiction_supported === true ? "yes" : "not established"}
                  </StatusBadge>
                </Field>
              </FieldGrid>

              {data.reasons.length > 0 && (
                <>
                  <h3>Why not more</h3>
                  <ul className="reasons">
                    {data.reasons.map((reason) => (
                      <li key={reason}>
                        <span className="mono-small">{reason}</span> — {reasonCopy(reason).sentence}
                      </li>
                    ))}
                  </ul>
                </>
              )}

              {data.verification_would_suffice === true && (
                <Refusal
                  what="Some of your value cannot leave yet."
                  rule="The API reports that identity verification is the only thing standing between you and some of it."
                  code="REQUIRES_VERIFICATION"
                  remedy="Verify your identity to enable withdrawal eligibility. It establishes who you are; it does not change what a Credit is and it moves nothing."
                >
                  <div className="form-actions">
                    <LinkButton to="/verify" variant="primary">
                      Start verification
                    </LinkButton>
                  </div>
                </Refusal>
              )}

              <h3>Where this value came from</h3>
              <DataTable
                caption="Your Credits by origin, in the order the policy would consume them, with what each bucket is permitted to do"
                rows={[...data.buckets].sort(
                  (a: WithdrawalOriginBucket, b: WithdrawalOriginBucket) =>
                    a.consumption_rank - b.consumption_rank,
                )}
                rowKey={(bucket: WithdrawalOriginBucket) => bucket.origin}
                columns={[
                  {
                    key: "rank",
                    header: "Leaves",
                    cell: (bucket: WithdrawalOriginBucket) => (
                      <span className="mono-small">#{String(bucket.consumption_rank)}</span>
                    ),
                  },
                  {
                    key: "origin",
                    header: "Origin",
                    cell: (bucket: WithdrawalOriginBucket) => (
                      <span className="mono-small">{bucket.origin}</span>
                    ),
                  },
                  {
                    key: "held",
                    header: "Held",
                    numeric: true,
                    cell: (bucket: WithdrawalOriginBucket) => <Credits base={bucket.quantity} />,
                  },
                  {
                    key: "withdrawable",
                    header: "May leave",
                    numeric: true,
                    riskMeasure: true,
                    cell: (bucket: WithdrawalOriginBucket) => <Credits base={bucket.withdrawable} />,
                  },
                  {
                    key: "allowed",
                    header: "Permitted",
                    cell: (bucket: WithdrawalOriginBucket) => (
                      <StatusBadge tone={bucket.payout_allowed ? "good" : "neutral"}>
                        {bucket.payout_allowed ? "yes" : "no"}
                      </StatusBadge>
                    ),
                  },
                  {
                    key: "why",
                    header: "Why not",
                    cell: (bucket: WithdrawalOriginBucket) =>
                      bucket.reasons.length === 0 ? (
                        <span className="absent">nothing in the way</span>
                      ) : (
                        <span className="cell-prose">
                          {bucket.reasons.map((reason) => reasonCopy(reason).sentence).join(" ")}
                        </span>
                      ),
                  },
                ]}
              />
              <p className="note">{PROVENANCE_NOTE}</p>
              <p className="note">
                The order above is the consumption order: among the origins the policy permits, the
                most restricted permitted one leaves first. It is the backend's ranking, under
                policy version <span className="mono-small">{data.policy_version}</span>, and this
                page does not predict it — the quote comes back with what was actually decided.
              </p>
            </>
          )}
        </AsyncPanel>
      </Panel>

      <Destinations
        accountId={accountId}
        chosen={chosenDestination}
        onChoose={(id) => {
          destination.set(id);
          quote.reset();
        }}
      />

      {disclosure !== undefined && !disclosureAccepted && <WithdrawalDisclosure doc={disclosure} />}
      {terms.isError && (
        <Panel title="The withdrawal disclosure could not be read" description="So it is not being skipped.">
          <Explanation
            error={terms.error}
            onRetry={() => {
              void terms.refetch();
            }}
          />
        </Panel>
      )}

      <Panel
        title="What it would cost"
        description="A quote reserves nothing and writes no ledger row. It expires, and an expired one is refused rather than re-priced."
      >
        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            if (!parsed.ok || chosenDestination === undefined) return;
            quote.mutate({
              accountId,
              destinationId: chosenDestination,
              amount: parsed.value,
              idempotencyKey: newIdempotencyKey(),
            });
          }}
        >
          <FormField
            label="Amount in Credits"
            hint="The gross you would give up. The fee comes out of it, and the minimum is judged on what is left."
            {...(amount.value === "" || parsed.ok ? {} : { error: parsed.error })}
          >
            {(field) => (
              <input
                className="input"
                type="text"
                inputMode="decimal"
                value={amount.value}
                onChange={(event) => {
                  amount.set(event.target.value);
                  quote.reset();
                }}
                {...field}
              />
            )}
          </FormField>

          {quote.isError && <CommandRefused what="No quote was given for this." error={quote.error} />}

          <div className="form-actions">
            {parsed.ok && chosenDestination !== undefined ? (
              <Button variant="primary" submit busy={quote.isPending} busyLabel="Asking…">
                {live === undefined ? "Get a quote" : "Get a new quote"}
              </Button>
            ) : (
              <Button
                disabledReason={
                  chosenDestination === undefined
                    ? "Choose a destination above first. A quote is a price for sending value somewhere, so there is nothing to price without one."
                    : "Enter a whole Credit amount greater than zero."
                }
              >
                Get a quote
              </Button>
            )}
          </div>
        </form>

        {live !== undefined && <QuoteDetail quote={live} secondsLeft={secondsLeft} tick={tick} />}
      </Panel>

      <Panel title="Request the withdrawal" description="The last step, and the only one that reserves anything.">
        {create.isError && <CommandRefused what="This withdrawal request was not accepted." error={create.error} />}
        <div className="form-actions">
          {canSubmit && live !== undefined ? (
            <Button
              variant="primary"
              busy={create.isPending}
              busyLabel="Requesting…"
              onClick={() => {
                // Minted here, at the confirmation, and kept: a retry after a
                // sign-in must repeat THIS request rather than make a second.
                const key = confirmKey.value === "" ? newIdempotencyKey() : confirmKey.value;
                confirmKey.set(key);
                create.mutate(
                  {
                    accountId,
                    amount: live.gross_quantity,
                    destinationId: live.destination_id,
                    quoteId: live.quote_id,
                    idempotencyKey: key,
                  },
                  {
                    onSuccess: (request) => {
                      setFollowing(request.payout_id);
                      amount.clear();
                      confirmKey.clear();
                      quote.reset();
                    },
                  },
                );
              }}
            >
              Request this withdrawal
            </Button>
          ) : (
            <Button
              disabledReason={
                live === undefined
                  ? "Take a quote first. The amount you commit to is the amount the quote named."
                  : quoteExpired
                    ? "That quote has expired. Take a new one rather than committing to a number that has moved."
                    : !live.minimum_ok
                      ? "This is below the provider's minimum once the fee is taken out. Sub-minimum value is destroyed rather than returned, so the request is refused rather than sent."
                      : !disclosureAccepted
                        ? "Read the withdrawal disclosure above first. It is asked for here rather than at signup because this is where it applies."
                        : "Choose a destination above first."
              }
            >
              Request this withdrawal
            </Button>
          )}
        </div>
        <p className="note">
          Requesting reserves the units the quote named and hands the decision to the policy. It
          does not move anything out of Nodal: Nodal instructs a licensed provider and the provider
          moves the value, which is why the request then has a state of its own to follow.
        </p>
      </Panel>

      {selected !== undefined && (
        <Panel title="This request" description="Followed until the provider reports an outcome.">
          <RequestDetail payoutId={selected} accountId={accountId} />
        </Panel>
      )}

      <Panel title="Your requests" description="Every withdrawal request on this account.">
        <AsyncPanel
          query={payouts}
          loadingLabel="Loading your requests…"
          skeleton={<Skeleton shape="rows" count={3} label="Your requests are loading" />}
          empty={{
            isEmpty: (rows: PayoutRequest[]) => rows.length === 0,
            title: EMPTY_STATES.payouts.title,
            body: EMPTY_STATES.payouts.body,
          }}
        >
          {(rows: PayoutRequest[]) => (
            <DataTable
              caption="Your withdrawal requests, with the state each one is in and what it reserved"
              rows={rows}
              rowKey={(row: PayoutRequest) => row.payout_id}
              onOpenRow={(row: PayoutRequest) => {
                setFollowing(row.payout_id);
              }}
              columns={[
                {
                  key: "state",
                  header: "State",
                  cell: (row: PayoutRequest) => (
                    <>
                      <StatusBadge tone={stateCopy(row.state).tone}>{row.state}</StatusBadge>
                      {row.sandbox === true && <StatusBadge tone="warn">rehearsal</StatusBadge>}
                    </>
                  ),
                },
                {
                  key: "requested",
                  header: "Requested",
                  numeric: true,
                  cell: (row: PayoutRequest) => <Credits base={row.requested_quantity} />,
                },
                {
                  key: "reserved",
                  header: "Reserved",
                  numeric: true,
                  cell: (row: PayoutRequest) => <Credits base={row.reserved_quantity} />,
                },
                {
                  key: "moving",
                  header: "Still moving",
                  cell: (row: PayoutRequest) =>
                    isMoving(row.state) ? (
                      <StatusBadge tone="info">waiting on an outcome</StatusBadge>
                    ) : (
                      <StatusBadge tone="neutral">finished</StatusBadge>
                    ),
                },
                {
                  key: "created",
                  header: "Requested at",
                  cell: (row: PayoutRequest) =>
                    row.created_at === undefined ? (
                      <span className="absent">not reported</span>
                    ) : (
                      <span>{formatInstant(row.created_at)}</span>
                    ),
                },
              ]}
            />
          )}
        </AsyncPanel>
        <Disclosure title="What each state means">
          <ul className="explain-fields">
            {Object.entries(STATE_COPY).map(([state, copy]) => (
              <li key={state}>
                <span className="mono-small">{state}</span> — {copy.sentence}
              </li>
            ))}
          </ul>
        </Disclosure>
      </Panel>
    </Page>
  );
}

/* --------------------------------------------------------------------------
 * The quote
 * ------------------------------------------------------------------------ */

/**
 * What the provider said a payout would cost.
 *
 * Both sides are exact integers and there is no rate between them: the Credit
 * side is base units, and the provider's side is a whole count of the smallest
 * unit of its currency. The response carries no exponent for that currency —
 * two is right for dollars and wrong for yen — so this page states the minor
 * units and the currency rather than inventing a decimal point. Converting
 * would be this interface asserting an exchange nobody defined.
 */
function QuoteDetail(props: {
  readonly quote: PayoutQuote;
  readonly secondsLeft: number | undefined;
  /** Redraws the countdown. Read so the timer is a dependency rather than decoration. */
  readonly tick: number;
}): ReactNode {
  const { quote } = props;
  const left = props.secondsLeft;
  const expired = left !== undefined && left <= 0;

  return (
    <>
      <FieldGrid columns={3}>
        <Field label="You give up" note="Gross. The fee comes out of this.">
          <Credits base={quote.gross_quantity} />
        </Field>
        <Field label="Fee" note="The provider's, as it quoted it.">
          <Credits base={quote.fee_quantity} />
        </Field>
        <Field label="What leaves" note="Net of the fee. The minimum is judged on this figure." emphasis>
          <Credits base={quote.net_quantity} big />
        </Field>
      </FieldGrid>

      <FieldGrid columns={3}>
        <Field
          label={`Net, in ${quote.currency} minor units`}
          note="The provider quotes in whole minor units. Nodal does not convert that to a currency figure: the response carries no exponent, and choosing one would be this page inventing it."
        >
          <Figure kind="count" count={quote.net_amount_minor} />
        </Field>
        <Field label={`Fee, in ${quote.currency} minor units`}>
          <Figure kind="count" count={quote.fee_amount_minor} />
        </Field>
        <Field
          label="Above the minimum"
          note="Judged net of fees, because sub-minimum dust is destroyed rather than returned."
        >
          <StatusBadge tone={quote.minimum_ok ? "good" : "warn"}>
            {quote.minimum_ok ? "yes" : "no"}
          </StatusBadge>
        </Field>
        <Field label="Expires" note="An expired quote is refused rather than re-priced.">
          {expired ? (
            <StatusBadge tone="bad">expired</StatusBadge>
          ) : left === undefined ? (
            <span>{formatInstant(quote.expires_at)}</span>
          ) : (
            <StatusBadge tone={left < 60 ? "warn" : "info"}>
              {String(left)} seconds left
            </StatusBadge>
          )}
        </Field>
        <Field label="Provider">
          <span className="mono-small">{quote.provider}</span>
          {quote.sandbox && <StatusBadge tone="warn">rehearsal</StatusBadge>}
        </Field>
        <Field label="Quote id">
          <Identifier value={quote.quote_id} />
        </Field>
      </FieldGrid>

      {quote.sandbox && (
        <p className="note">
          These figures come from a sandbox provider. They are placeholders rather than a price
          anybody has agreed, a payout against them settles after about ten seconds, and no value
          moves anywhere.
        </p>
      )}
      {quote.fee_model_version !== undefined && quote.fee_model_version !== "" && (
        <p className="note">
          Fee schedule: <span className="mono-small">{quote.fee_model_version}</span>
        </p>
      )}

      {(quote.provenance ?? []).length > 0 && (
        <>
          <h4>What would leave, and in which order</h4>
          <ProvenanceTable
            slices={quote.provenance ?? []}
            caption="The provenance slices this quote would consume, in the order the policy would consume them"
          />
        </>
      )}

      {expired && (
        <Refusal
          what="This quote has expired."
          rule="A quote stands until it expires and is then refused rather than silently re-priced."
          code="QUOTE_EXPIRED"
          remedy="Take a new quote. The number may have moved, and you will be shown the new one before you commit to it."
        />
      )}
    </>
  );
}
