/**
 * `/withdraw` — the page that exists for everyone, including the people who
 * cannot use it.
 *
 * Goal §19 makes the shape of this screen a product boundary rather than a
 * layout choice, and three rules follow from it.
 *
 * ONE. THE PAGE IS NEVER HIDDEN. A customer who cannot withdraw is shown what
 * withdrawal is, what would have to be true for it to happen, and what the next
 * step is. Hiding the page would be the product deciding on their behalf that
 * they do not need to know.
 *
 * TWO. THE SENTENCE MATTERS. "Verify your identity to enable withdrawal
 * eligibility" is the promise this product can keep. The wording goal §19
 * forbids — the one that offers verification as a way of turning Credits into
 * spendable money — is a promise it cannot, because verification changes a
 * financial profile and never changes what a Credit is. That distinction is the
 * whole architecture: no verification writer touches a Credit lot anywhere in
 * the system, and this page does not imply that one does.
 *
 * THREE. WHAT COULD LEAVE IS DECIDED PER ORIGIN, NOT PER BALANCE. Eligibility
 * is computed from lots that carry where they came from — purchased value,
 * trading gains, promotional grants, creator earnings, refunds — and the policy
 * decides per origin. So the page shows the breakdown rather than one number,
 * because one number would be true of nothing.
 *
 * WHAT THIS PAGE DOES NOT DO. It never fabricates an eligibility answer. Every
 * refusal on it is the API's: its code, its reason and the next action it named.
 * A request that comes back refused is rendered as a refusal — the boundary
 * working — and never as an error or, worse, as a zero.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useCreatePayout,
  useCancelPayout,
  useCreditBalance,
  usePayout,
  usePayouts,
  type CreditBalance,
  type PayoutRequest,
} from "../../api/queries.ts";
import { Button } from "../../components/Button.tsx";
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
import { explain } from "../../api/problem.ts";
import { EMPTY_STATES, situationForCode } from "../../lib/errors.ts";
import { CREDITS_DISCLOSURE, PROVENANCE_NOTE } from "../../lib/honesty.ts";
import { parseQuantityInput } from "../../lib/money.ts";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { formatInstant } from "../../lib/time.ts";
import { useActiveAccountId } from "../../session.tsx";

/** Credits are held at six decimal places, exactly as the ledger holds them. */
const CREDIT_DECIMALS = 6;

/** How often a request that is still moving is re-read from the backend. */
const FOLLOW_MS = 2_000;

/* --------------------------------------------------------------------------
 * The state machine, in the customer's words
 * ------------------------------------------------------------------------ */

interface StateCopy {
  readonly tone: Tone;
  readonly sentence: string;
}

/**
 * What each payout state means and what happens next.
 *
 * Every sentence names who is acting. A customer reading "SUBMITTED" needs to
 * know that Nodal has handed the instruction to somebody else and is now
 * waiting, because that is what determines whether there is anything they can
 * do about it.
 */
const STATE_COPY: Readonly<Record<string, StateCopy>> = {
  DRAFT: { tone: "neutral", sentence: "Recorded. Eligibility has not been evaluated yet." },
  ELIGIBILITY_CHECK: {
    tone: "info",
    sentence: "The eligibility policy is deciding which of your units may leave, origin by origin.",
  },
  VERIFICATION_REQUIRED: {
    tone: "warn",
    sentence:
      "This request needs a verified financial profile before it can go any further. Verification changes your profile; it does not change what a Credit is.",
  },
  VERIFICATION_PENDING: {
    tone: "warn",
    sentence:
      "Identity verification is with the provider. Nodal receives the outcome from the provider, never from this browser.",
  },
  VERIFIED: {
    tone: "good",
    sentence: "Eligible and reserved. It has not been given to a provider yet, so it can still be cancelled.",
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
  FAILED: { tone: "bad", sentence: "The provider did not complete it. The reason is below where it gave one." },
  REJECTED: { tone: "bad", sentence: "This request was refused, and the reserved units were returned to the lots they came from." },
  REVERSED: { tone: "bad", sentence: "A settled payout was reversed afterwards." },
  MANUAL_REVIEW: { tone: "warn", sentence: "A person has to look at this before it goes any further." },
};

function stateCopy(state: string): StateCopy {
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
 * Anything that has been given to a provider may already have been paid, and
 * the only honest way out of that is reconciliation. The button is offered only
 * where the backend would actually accept it.
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

/* --------------------------------------------------------------------------
 * What the API refused, in the product's fixed words
 * ------------------------------------------------------------------------ */

/**
 * A refused payout request.
 *
 * The code is the backend's; the sentence is the fixed one this product owes
 * for that situation (USER_JOURNEY §11), and where there is none, `explain()`'s
 * honest fallback. Nothing here is a provider message or a server string.
 */
function RequestRefused(props: { readonly error: unknown }): ReactNode {
  const detail = explain(props.error);
  const situation = situationForCode(detail.code);
  return (
    <Refusal
      what="This withdrawal request was not accepted."
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
    </Refusal>
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
              </Field>
              <Field label="Requested">
                <Figure
                  kind="money"
                  value={{ base: data.requested_quantity, scale: CREDIT_DECIMALS }}
                  symbol="Credits"
                />
              </Field>
              <Field
                label="Reserved"
                note="Held out of your spendable balance while this request is open."
              >
                <Figure
                  kind="money"
                  value={{ base: data.reserved_quantity, scale: CREDIT_DECIMALS }}
                  symbol="Credits"
                />
              </Field>
              <Field label="Settled">
                <Figure
                  kind="money"
                  value={
                    data.settled_quantity === undefined
                      ? null
                      : { base: data.settled_quantity, scale: CREDIT_DECIMALS }
                  }
                  symbol="Credits"
                  absent="nothing has settled"
                />
              </Field>
              <Field label="Policy that decided this">
                <span className="mono-small">{data.policy_version}</span>
              </Field>
              <Field label="Request id">
                <Identifier value={data.payout_id} />
              </Field>
            </FieldGrid>

            {data.verification_would_suffice === true && (
              <p className="note">
                The API reports that identity verification is the only obstacle to this request. It
                is a different answer from "no", and it is presented as one.
              </p>
            )}
            {data.required_verification !== undefined && data.required_verification !== "" && (
              <p className="note">
                The level of verification this request needs:{" "}
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
                    <span className="mono-small">{code}</span>
                  </li>
                ))}
              </ul>
            )}

            {cancel.isError && <Explanation error={cancel.error} onRetry={cancel.reset} />}

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
                This request can no longer be cancelled. It has been given to a provider, which
                means it may already have been paid, and the only honest way out of that is
                reconciliation rather than an undo.
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
  const balance = useCreditBalance(accountId);
  const payouts = usePayouts(accountId);
  const create = useCreatePayout();
  const amount = useSurvivesSignIn<string>("withdraw.amount", "");
  const [following, setFollowing] = useState<string | undefined>(undefined);

  const parsed = parseQuantityInput(amount.value, CREDIT_DECIMALS);
  const selected = following ?? create.data?.payout_id;

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

  return (
    <Page
      title="Withdraw"
      lead="A withdrawal is a request to convert eligible value and have a licensed provider pay it out. Nodal instructs the provider; it never converts anything itself."
    >
      <Panel title="What a withdrawal is, and what it needs" description="Before any figure on this page.">
        <p>
          Credits are internal platform value and are not directly withdrawable. What can leave is
          value the eligibility policy has decided may leave, and that decision is made per origin —
          where each unit came from — rather than against your total.
        </p>
        <p>Two things have to be true before any request can be settled:</p>
        <ul className="explain-fields">
          <li>
            <strong>A verified financial profile.</strong> Verification asks a provider to check
            identity, age, jurisdiction and sanctions. Nodal stores the decision, a provider
            reference and the timestamps — never a document, and never the underlying details.
            Verifying changes your profile. It does not change what a Credit is and it moves nothing.
          </li>
          <li>
            <strong>An approved payout destination.</strong> A destination is held as a provider
            token or a sandbox handle. Nodal never takes a raw account number.
          </li>
        </ul>
        <p className="note">{CREDITS_DISCLOSURE}</p>
      </Panel>

      <Panel
        title="What could be withdrawn"
        description="Decided per origin by the policy the backend names, not by the total."
      >
        <AsyncPanel
          query={balance}
          loadingLabel="Reading your Credit balance…"
          skeleton={<Skeleton shape="rows" count={3} label="Your Credit balance is loading" />}
        >
          {(data: CreditBalance) => (
            <>
              <FieldGrid columns={3}>
                <Field
                  label="Eligible to be withdrawn"
                  note="What the named policy version permits to leave right now."
                  emphasis
                >
                  <Figure
                    kind="money"
                    value={{ base: data.payout_eligible, scale: CREDIT_DECIMALS }}
                    symbol="Credits"
                    big
                  />
                </Field>
                <Field label="Not eligible" note="Held for the reasons listed below.">
                  <Figure
                    kind="money"
                    value={{ base: data.ineligible, scale: CREDIT_DECIMALS }}
                    symbol="Credits"
                  />
                </Field>
                <Field label="Everything you hold" note="Gross. It is not what can leave.">
                  <Figure
                    kind="money"
                    value={{ base: data.gross, scale: CREDIT_DECIMALS }}
                    symbol="Credits"
                  />
                </Field>
              </FieldGrid>

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
                    explanation: "The policy permits this much to leave right now.",
                    texture: "solid",
                  },
                  {
                    key: "ineligible",
                    label: "Not eligible",
                    baseUnits: data.ineligible,
                    explanation: "Held back by the policy, for the reasons listed below.",
                    texture: "hatch",
                  },
                  {
                    key: "frozen",
                    label: "Frozen",
                    baseUnits: data.frozen,
                    explanation: "Held because of a dispute or an adjustment on this account.",
                    texture: "sparse",
                  },
                ]}
              />

              {(data.ineligible_reasons ?? []).length > 0 && (
                <ul className="reasons">
                  {(data.ineligible_reasons ?? [])
                    .filter((reason) => typeof reason === "string")
                    .map((reason) => (
                      <li key={reason}>{reason}</li>
                    ))}
                </ul>
              )}

              <h3>Where this value came from</h3>
              {data.by_origin === undefined || Object.keys(data.by_origin).length === 0 ? (
                <p className="note">
                  The backend returned no breakdown by origin for this account, so none is shown.
                  This is the absence of a breakdown, not a breakdown that is all zeroes.
                </p>
              ) : (
                <DataTable
                  caption="Your Credits by where they came from. Eligibility is decided per origin, so this, and not the total, is what determines what can leave"
                  rows={Object.entries(data.by_origin)}
                  rowKey={(row: readonly [string, string]) => row[0]}
                  columns={[
                    {
                      key: "origin",
                      header: "Origin",
                      cell: (row: readonly [string, string]) => (
                        <span className="mono-small">{row[0]}</span>
                      ),
                    },
                    {
                      key: "quantity",
                      header: "Held",
                      numeric: true,
                      cell: (row: readonly [string, string]) => (
                        <Figure
                          kind="money"
                          value={{ base: row[1], scale: CREDIT_DECIMALS }}
                          symbol="Credits"
                        />
                      ),
                    },
                  ]}
                />
              )}
              <p className="note">{PROVENANCE_NOTE}</p>
              <p className="note">
                Which of these origins a request actually consumes, and in what order, is decided by
                policy version <span className="mono-small">{data.policy_version}</span> at the
                moment the request is evaluated. This page does not predict that decision: the
                request itself comes back with what was decided.
              </p>
            </>
          )}
        </AsyncPanel>
      </Panel>

      <Panel
        title="Request a withdrawal"
        description="The amount you ask for is the amount that is sent. Eligibility is decided by the backend, not here."
      >
        {create.isError && <RequestRefused error={create.error} />}
        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            if (!parsed.ok) return;
            create.mutate(
              {
                accountId,
                amount: parsed.value,
                // Minted at confirmation. A retry after a sign-in reuses this
                // request rather than making a second one.
                idempotencyKey: newIdempotencyKey(),
              },
              {
                onSuccess: (request) => {
                  setFollowing(request.payout_id);
                  amount.clear();
                },
              },
            );
          }}
        >
          <FormField
            label="Amount in Credits"
            hint="Within the eligible figure above. Asking for more is refused by the backend rather than silently reduced."
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
                }}
                {...field}
              />
            )}
          </FormField>
          <div className="form-actions">
            {parsed.ok ? (
              <Button variant="primary" submit busy={create.isPending} busyLabel="Requesting…">
                Request this withdrawal
              </Button>
            ) : (
              <Button disabledReason="Enter a whole Credit amount greater than zero before requesting a withdrawal.">
                Request this withdrawal
              </Button>
            )}
          </div>
          <p className="note">
            Requesting does not move anything out of Nodal. It records a request, reserves the units
            it would consume, and hands the decision to the eligibility policy — which may refuse it,
            and says why when it does.
          </p>
        </form>
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
                    <StatusBadge tone={stateCopy(row.state).tone}>{row.state}</StatusBadge>
                  ),
                },
                {
                  key: "requested",
                  header: "Requested",
                  numeric: true,
                  cell: (row: PayoutRequest) => (
                    <Figure
                      kind="money"
                      value={{ base: row.requested_quantity, scale: CREDIT_DECIMALS }}
                      symbol="Credits"
                    />
                  ),
                },
                {
                  key: "reserved",
                  header: "Reserved",
                  numeric: true,
                  cell: (row: PayoutRequest) => (
                    <Figure
                      kind="money"
                      value={{ base: row.reserved_quantity, scale: CREDIT_DECIMALS }}
                      symbol="Credits"
                    />
                  ),
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
