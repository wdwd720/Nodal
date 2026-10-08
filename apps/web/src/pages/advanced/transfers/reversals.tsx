/**
 * Reversal of a transfer under dual control.
 *
 * A reversal takes two different operators: one proposes it with a stable
 * reason code and a reason, and nothing moves; a second approves it, and the
 * backend executes the reversal once in that approval's own transaction; any
 * operator holding the permission, the proposer included, may reject it.
 * Every rule is the backend's -- who may propose or decide, the recent strong
 * sign-in, the recipient still holding the funds, the proposer never
 * approving -- and every refusal is shown with its code and request id.
 *
 * The one thing this file decides for itself is not to offer an approval to
 * the operator who proposed the reversal: the backend refuses it
 * (TRANSFER_REVERSAL_SELF_APPROVAL), so the control says why instead of
 * inviting a request that cannot succeed.
 */
import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useDecideTransferReversal,
  useProposeTransferReversal,
  useTransferReversalProposals,
  type AdvancedTransfer,
  type DecideReversalInput,
  type TransferReversalProposal,
} from "../../../api/advanced/transfers.ts";
import { Button } from "../../../components/Button.tsx";
import { AsyncPanel, Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Panel, Pill } from "../../../components/Layout.tsx";
import { Qty } from "../../../components/Money.tsx";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import {
  TRANSFER_REVERSAL_MEANING,
  approvalBlockedReason,
  checkReasonCode,
  checkReasonText,
  confirmationKey,
  notReversibleReason,
  pendingProposal,
  reversalStatusTone,
  reversible,
  transferStatusTone,
} from "../../../lib/transfers.ts";
import { useSession } from "../../../session.tsx";
import { transferHref } from "./forms.tsx";

/** The dual-control panel of one transfer: its proposals, a proposal form, and the decisions. */
export function ReversalPanel(props: { readonly transfer: AdvancedTransfer }): ReactNode {
  const { transfer } = props;
  const proposals = useTransferReversalProposals({ transferId: transfer.id });
  return (
    <Panel
      title="Reversal under dual control"
      description="Reversal returns a COMPLETED transfer's value to its sender with a new compensating journal; the original posting is never edited. One operator proposes it and a second, different operator approves it, which executes it once. The backend requires the operator permissions and a recent strong sign-in, and refuses when the recipient no longer holds the funds."
    >
      <AsyncPanel
        query={proposals}
        loadingLabel="Loading this transfer's reversal proposals…"
        empty={{
          isEmpty: (page) => page.items.length === 0,
          title: "No reversal proposed",
          body: "The backend holds no reversal proposal for this transfer.",
        }}
      >
        {(page) => (
          <div className="stack">
            {page.items.map((proposal) => (
              <ProposalCard key={proposal.id} proposal={proposal} showTransfer={false} />
            ))}
            {page.nextCursor !== null && (
              <p className="field-note">
                More proposals exist than one page holds; the operator console lists every pending one.
              </p>
            )}
          </div>
        )}
      </AsyncPanel>
      <h3>Propose a reversal</h3>
      {!reversible(transfer.status) ? (
        <div className="form-actions">
          <Button variant="danger" disabledReason={notReversibleReason(transfer.status)}>
            Propose the reversal
          </Button>
        </div>
      ) : proposals.isSuccess && pendingProposal(proposals.data.items) !== undefined ? (
        <div className="form-actions">
          <Button
            variant="danger"
            disabledReason="A proposal of this transfer is already awaiting a decision; the backend allows one at a time. Approve or reject it first."
          >
            Propose the reversal
          </Button>
        </div>
      ) : (
        <ProposeForm transferId={transfer.id} />
      )}
    </Panel>
  );
}

/** One operator's proposal: a reason code and a reason. It moves nothing. */
function ProposeForm(props: { readonly transferId: string }): ReactNode {
  const propose = useProposeTransferReversal();
  const ids = useId();
  const [reasonCode, setReasonCode] = useState("");
  const [reason, setReason] = useState("");
  const [attemptKey, setAttemptKey] = useState<string | undefined>(undefined);
  const code = checkReasonCode(reasonCode);
  const why = checkReasonText(reason, "the reason for the reversal");

  const confirm = (): void => {
    if (!code.ok || !why.ok) return;
    const key = confirmationKey(attemptKey, newIdempotencyKey);
    setAttemptKey(key);
    propose.mutate({ transferId: props.transferId, reasonCode: code.value, reason: why.value, idempotencyKey: key });
  };
  const changed = (): void => {
    setAttemptKey(undefined);
    propose.reset();
  };

  if (propose.isSuccess) {
    return (
      <p className="callout" role="status" aria-label="The proposal" data-proposed={propose.data.id}>
        Proposed as <Pill tone={reversalStatusTone(propose.data.status)}>{propose.data.status}</Pill> (proposal{" "}
        {String(propose.data.seq)} of this transfer). A second, different operator now approves or rejects it.
      </p>
    );
  }
  return (
    <form
      className="form"
      aria-label="Propose a reversal"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        confirm();
      }}
    >
      <div className="form-row">
        <label htmlFor={`${ids}-code`}>Reason code</label>
        <input
          id={`${ids}-code`}
          className="input"
          value={reasonCode}
          autoComplete="off"
          spellCheck={false}
          aria-describedby={`${ids}-code-note`}
          onChange={(event) => {
            setReasonCode(event.target.value);
            changed();
          }}
        />
        <p id={`${ids}-code-note`} className="field-note">
          A stable code in capitals, such as DISPUTE_UPHELD. It becomes the compensating journal's reason code.
        </p>
      </div>
      <div className="form-row">
        <label htmlFor={`${ids}-reason`}>Reason</label>
        <textarea
          id={`${ids}-reason`}
          className="input"
          rows={2}
          value={reason}
          aria-describedby={`${ids}-reason-note`}
          onChange={(event) => {
            setReason(event.target.value);
            changed();
          }}
        />
        <p id={`${ids}-reason-note`} className="field-note">
          Recorded with the proposal, the reversal's evidence and its audit record.
        </p>
      </div>
      <div className="form-actions">
        {code.ok && why.ok ? (
          <Button variant="danger" submit busy={propose.isPending} busyLabel="Proposing…">
            Propose the reversal
          </Button>
        ) : (
          <Button variant="danger" disabledReason={!code.ok ? code.error : why.error}>
            Propose the reversal
          </Button>
        )}
      </div>
      {attemptKey !== undefined && (
        <p className="mono-small" data-idempotency-key={attemptKey}>
          idempotency key {attemptKey} — created at confirmation; a retry reuses it
        </p>
      )}
      {propose.isError && <Explanation error={propose.error} onRetry={confirm} retryLabel="Try again with the same key" />}
    </form>
  );
}

/**
 * One proposal: its status, who proposed and decided it, and -- while it is
 * PENDING -- the decision. `showTransfer` adds the transfer it would reverse,
 * for lists that are not already on that transfer's page.
 */
export function ProposalCard(props: {
  readonly proposal: TransferReversalProposal;
  readonly showTransfer: boolean;
}): ReactNode {
  const { proposal } = props;
  const transfer = proposal.transfer;
  return (
    <article
      className="callout"
      aria-label={`Reversal proposal ${String(proposal.seq)} of transfer ${proposal.transfer_id}`}
      data-proposal-id={proposal.id}
      data-proposal-status={proposal.status}
    >
      <FieldGrid columns={3}>
        <Field label="Status" note={TRANSFER_REVERSAL_MEANING[proposal.status]}>
          <span data-field="proposal-status">
            <Pill tone={reversalStatusTone(proposal.status)}>{proposal.status}</Pill>
          </span>
        </Field>
        <Field label="Reason code">
          <code className="mono-small">{proposal.reason_code}</code>
        </Field>
        <Field label="Reason">{proposal.reason}</Field>
        <Field label="Proposed by" note={exactInstant(proposal.proposed_at)}>
          <Identifier value={proposal.proposed_by} />
          <span className="cell-note">{formatInstant(proposal.proposed_at)}</span>
        </Field>
        <Field label="Decided by" {...(proposal.decided_at === undefined ? {} : { note: exactInstant(proposal.decided_at) })}>
          {proposal.decided_by === undefined ? (
            <span className="absent">awaiting a second operator</span>
          ) : (
            <>
              <Identifier value={proposal.decided_by} />
              {proposal.decided_at !== undefined && <span className="cell-note">{formatInstant(proposal.decided_at)}</span>}
            </>
          )}
        </Field>
        <Field label="Decision reason">
          {proposal.decision_reason ?? <span className="absent">none stated</span>}
        </Field>
        {props.showTransfer && (
          <>
            <Field label="Transfer">
              <Link to={transferHref(transfer.id)}>
                <code className="mono-small">{transfer.id}</code>
              </Link>
            </Field>
            <Field label="Amount">
              <Qty value={transfer.quantity} decimals={transfer.asset_decimals} symbol={transfer.asset_symbol} />
            </Field>
            <Field label="Transfer status">
              <Pill tone={transferStatusTone(transfer.status)}>{transfer.status}</Pill>
            </Field>
            <Field label="From">
              <Identifier value={transfer.sender_account_id} />
            </Field>
            <Field label="To">
              {transfer.recipient_account_id === undefined ? (
                <span className="absent">not reported</span>
              ) : (
                <Identifier value={transfer.recipient_account_id} />
              )}
            </Field>
          </>
        )}
      </FieldGrid>
      {proposal.status === "PENDING" && <DecisionControls proposal={proposal} />}
    </article>
  );
}

/**
 * The decision on a PENDING proposal. Approving reverses the transfer;
 * rejecting needs a reason. Each decision is confirmed first, and confirming
 * creates the idempotency key a retry of that same decision reuses.
 */
function DecisionControls(props: { readonly proposal: TransferReversalProposal }): ReactNode {
  const { proposal } = props;
  const session = useSession();
  const decide = useDecideTransferReversal();
  const ids = useId();
  const [reason, setReason] = useState("");
  const [review, setReview] = useState<"approve" | "reject" | undefined>(undefined);
  const [attempt, setAttempt] = useState<DecideReversalInput | undefined>(undefined);
  const blocked = approvalBlockedReason(proposal.proposed_by, session.principal?.subject_id);
  const trimmed = reason.trim();
  const rejectReason = checkReasonText(reason, "the reason for rejecting it");
  const approveReason = trimmed === "" ? { ok: true, value: "", error: "" } : checkReasonText(reason, "a reason");

  const send = (decision: "approve" | "reject"): void => {
    const text = decision === "reject" ? rejectReason.value : approveReason.value;
    const request = { proposalId: proposal.id, decision, reason: text === "" ? undefined : text };
    const same =
      attempt !== undefined &&
      attempt.decision === request.decision &&
      attempt.reason === request.reason &&
      attempt.proposalId === request.proposalId;
    const next: DecideReversalInput = { ...request, idempotencyKey: confirmationKey(same ? attempt.idempotencyKey : undefined, newIdempotencyKey) };
    setAttempt(next);
    setReview(undefined);
    decide.mutate(next);
  };

  if (decide.isSuccess) {
    const decided = decide.data.proposal;
    return (
      <p className="callout" role="status" aria-label="The decision" data-decided={decided.id} data-decision={decided.status}>
        {decide.data.replayed ? "The backend had already recorded this decision and wrote nothing new. " : ""}
        The proposal is <Pill tone={reversalStatusTone(decided.status)}>{decided.status}</Pill> and the transfer is{" "}
        <Pill tone={transferStatusTone(decided.transfer.status)}>{decided.transfer.status}</Pill>
        {decided.transfer.reversal_journal_transaction_id !== undefined && (
          <>
            {" "}
            with compensating journal <code className="mono-small">{decided.transfer.reversal_journal_transaction_id}</code>
          </>
        )}
        .
      </p>
    );
  }

  return (
    <div className="command" role="group" aria-label={`Decide reversal proposal ${String(proposal.seq)}`}>
      <div className="form-row">
        <label htmlFor={`${ids}-reason`}>Decision reason</label>
        <textarea
          id={`${ids}-reason`}
          className="input"
          rows={2}
          value={reason}
          aria-describedby={`${ids}-reason-note`}
          onChange={(event) => {
            setReason(event.target.value);
            setReview(undefined);
            decide.reset();
          }}
        />
        <p id={`${ids}-reason-note`} className="field-note">
          Required to reject; optional to approve. Recorded with the decision.
        </p>
      </div>
      {review === undefined ? (
        <div className="form-actions">
          {blocked !== undefined ? (
            <Button variant="danger" disabledReason={blocked}>
              Approve and reverse
            </Button>
          ) : approveReason.ok ? (
            <Button
              variant="danger"
              onClick={() => {
                setReview("approve");
              }}
            >
              Approve and reverse
            </Button>
          ) : (
            <Button variant="danger" disabledReason={approveReason.error}>
              Approve and reverse
            </Button>
          )}
          {rejectReason.ok ? (
            <Button
              onClick={() => {
                setReview("reject");
              }}
            >
              Reject the proposal
            </Button>
          ) : (
            <Button disabledReason="Rejecting a proposal needs a reason; enter it above.">Reject the proposal</Button>
          )}
        </div>
      ) : (
        <div className="form-actions">
          <p className="command-title">
            {review === "approve"
              ? "Approving reverses the transfer now, with a compensating journal, in the approval's own transaction."
              : "Rejecting moves nothing; a rejected proposal can never be executed."}
          </p>
          <Button
            variant={review === "approve" ? "danger" : "primary"}
            busy={decide.isPending}
            busyLabel="Sending…"
            onClick={() => {
              send(review);
            }}
          >
            {review === "approve" ? "Confirm: approve and reverse" : "Confirm: reject"}
          </Button>
          <Button
            variant="quiet"
            onClick={() => {
              setReview(undefined);
            }}
          >
            Change
          </Button>
        </div>
      )}
      {attempt !== undefined && (
        <p className="mono-small" data-idempotency-key={attempt.idempotencyKey}>
          idempotency key {attempt.idempotencyKey} — created at confirmation; a retry of this decision reuses it
        </p>
      )}
      {decide.isError && attempt !== undefined && (
        <Explanation
          error={decide.error}
          onRetry={() => {
            decide.mutate(attempt);
          }}
          retryLabel="Try again with the same key"
        />
      )}
    </div>
  );
}
