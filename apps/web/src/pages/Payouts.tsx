/**
 * PAYOUTS (gola.md PARTS XVIII–XXI).
 *
 * The honest version of this page is mostly a refusal, and it says so rather
 * than drawing a hopeful form.
 *
 * Three things it must never do:
 *
 *   - present the Credit total as the amount that can be withdrawn. What may
 *     leave depends on where each unit came from, so the eligible figure is
 *     shown next to the total and the difference is explained.
 *   - present "no payout path is approved" as a problem with the customer's
 *     account. It is a decision about the product, and no amount of Credits
 *     changes it.
 *   - collapse "you need to verify your identity" into "no". Those are
 *     different answers and only one of them has a next step, which is why
 *     `verification_would_suffice` exists on the API and is rendered here.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useCreatePayout,
  useCreditBalance,
  usePayouts,
  type CreditBalance,
  type PayoutRequest,
} from "../api/queries.ts";
import { AsyncPanel, EmptyState, Explanation } from "../components/DataState.tsx";
import { Button } from "../components/Button.tsx";
import { Disclosure, Field, FieldGrid, Identifier, Page, Panel, Pill, Table } from "../components/Layout.tsx";
import { Qty } from "../components/Money.tsx";
import { CREDITS_DISCLOSURE, PAYOUT_NOT_APPROVED, PROVENANCE_NOTE } from "../lib/honesty.ts";
import { useActiveAccountId } from "../session.tsx";

const CREDIT_DECIMALS = 6;

/** What each payout state means, in words a customer can act on. */
const STATE_COPY: Readonly<Record<string, { readonly tone: "neutral" | "good" | "warn" | "bad" | "info"; readonly text: string }>> = {
  DRAFT: { tone: "neutral", text: "Recorded, not yet evaluated." },
  ELIGIBILITY_CHECK: { tone: "info", text: "Being checked against the payout policy." },
  VERIFICATION_REQUIRED: { tone: "warn", text: "Waiting on identity verification. This one has a next step." },
  VERIFICATION_PENDING: { tone: "info", text: "Identity verification is in progress." },
  VERIFIED: { tone: "good", text: "Eligible and waiting to be sent." },
  SUBMITTED: { tone: "info", text: "Sent to the provider." },
  PROVIDER_PENDING: { tone: "info", text: "The provider is working on it." },
  PAYOUT_STATUS_UNKNOWN: {
    tone: "warn",
    text: "The provider did not answer. Nothing is resent until we find out what happened — resending is how a payout gets paid twice.",
  },
  SETTLED: { tone: "good", text: "The provider paid it." },
  FAILED: { tone: "bad", text: "It did not happen. The reserved Credits went back to the exact lots they came from." },
  REJECTED: { tone: "bad", text: "Refused before anything was sent." },
  REVERSED: { tone: "bad", text: "The provider clawed it back." },
  MANUAL_REVIEW: { tone: "warn", text: "A person is looking at it. Two operators must agree on what happens next." },
};

export function Payouts(): ReactNode {
  const accountId = useActiveAccountId();
  const credits = useCreditBalance(accountId);
  const payouts = usePayouts(accountId);

  if (accountId === undefined) {
    return (
      <Page title="Payouts">
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is nothing to pay out."
        />
      </Page>
    );
  }

  return (
    <Page
      title="Payouts"
      lead="Taking value out of Nodal. What may leave depends on where each Credit came from, not on the total."
    >
      <Panel title="What could leave" description="Computed by the backend under the policy version it names.">
        <AsyncPanel query={credits} loadingLabel="Asking the backend what is eligible…">
          {(balance: CreditBalance) => (
            <>
              <FieldGrid columns={3}>
                <Field label="Held" note="Every Credit this account has.">
                  <Qty value={balance.gross} decimals={CREDIT_DECIMALS} symbol="Credits" />
                </Field>
                <Field
                  label="Eligible to pay out"
                  note="The only figure that matters here. Usually smaller than the total, and sometimes zero."
                  emphasis
                >
                  <Qty value={balance.payout_eligible} decimals={CREDIT_DECIMALS} symbol="Credits" />
                </Field>
                <Field label="Policy" note="The version these figures were computed under.">
                  <span className="mono-small">{balance.policy_version}</span>
                </Field>
              </FieldGrid>

              {balance.payout_eligible === "0" ? (
                <p className="notice notice-warn" role="status">
                  {PAYOUT_NOT_APPROVED}
                </p>
              ) : (
                <RequestForm accountId={accountId} eligible={balance.payout_eligible} />
              )}

              <Disclosure title="Why the eligible figure is not the total">
                <p>{PROVENANCE_NOTE}</p>
                <p>{CREDITS_DISCLOSURE}</p>
              </Disclosure>
            </>
          )}
        </AsyncPanel>
      </Panel>

      <Panel title="Requests" description="Every payout this account has asked for, and what happened to it.">
        <AsyncPanel
          query={payouts}
          loadingLabel="Asking the backend for this account's payout requests…"
          empty={{
            isEmpty: (list: PayoutRequest[]) => list.length === 0,
            title: "No payout requests",
            body: "This account has never asked for one.",
          }}
        >
          {(list: PayoutRequest[]) => (
            <Table caption="Payout requests" headers={["Request", "Asked for", "Reserved", "State"]}>
              {list.map((request) => (
                <tr key={request.payout_id}>
                  <td>
                    <Identifier value={request.payout_id} />
                  </td>
                  <td>
                    <Qty value={request.requested_quantity} decimals={CREDIT_DECIMALS} symbol="Credits" />
                  </td>
                  <td>
                    <Qty value={request.reserved_quantity} decimals={CREDIT_DECIMALS} symbol="Credits" />
                  </td>
                  <td>
                    <StateCell request={request} />
                  </td>
                </tr>
              ))}
            </Table>
          )}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

function StateCell(props: { readonly request: PayoutRequest }): ReactNode {
  const { request } = props;
  const copy = STATE_COPY[request.state] ?? { tone: "neutral" as const, text: request.state };
  return (
    <>
      <Pill tone={copy.tone}>{request.state}</Pill>
      <p className="field-note">{copy.text}</p>
      {request.verification_would_suffice === true && (
        <p className="field-note">
          Verifying your identity would be enough on its own
          {request.required_verification === undefined ? "" : ` (${request.required_verification})`}. That is a
          different answer from no.
        </p>
      )}
      {Array.isArray(request.eligibility_reasons) && request.eligibility_reasons.length > 0 && (
        <ul className="reasons">
          {request.eligibility_reasons.map((reason) => (
            <li key={reason}>{reason}</li>
          ))}
        </ul>
      )}
      {request.failure_reason !== undefined && request.failure_reason !== "" && (
        <p className="field-note">{request.failure_reason}</p>
      )}
    </>
  );
}

/**
 * The request form. It offers the eligible amount rather than the total, and
 * the idempotency key is created once, when the customer confirms, so a retry
 * of the same request cannot become a second payout.
 */
function RequestForm(props: { readonly accountId: string; readonly eligible: string }): ReactNode {
  const [amount, setAmount] = useState(props.eligible);
  const create = useCreatePayout();

  if (create.isSuccess) {
    return (
      <p className="notice notice-good" role="status">
        Requested. It is in the table below with the state the backend gave it.
      </p>
    );
  }

  return (
    <form
      className="stack"
      onSubmit={(event) => {
        event.preventDefault();
        create.mutate({
          accountId: props.accountId,
          amount,
          idempotencyKey: newIdempotencyKey(),
        });
      }}
    >
      <label className="inline-field">
        <span>Amount in Credit base units</span>
        <input
          value={amount}
          inputMode="numeric"
          pattern="[0-9]+"
          onChange={(event) => setAmount(event.target.value)}
        />
      </label>
      <p className="field-note">
        Exact base units, as the backend holds them. The eligible amount is {props.eligible}.
      </p>
      {create.isError && <Explanation error={create.error} onRetry={() => create.reset()} />}
      <Button variant="primary" submit busy={create.isPending} busyLabel="Requesting…">
        Request a payout
      </Button>
    </form>
  );
}
