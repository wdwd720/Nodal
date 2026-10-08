/**
 * The margin review queue: every liquidation case in MANUAL_REVIEW, oldest
 * first, with every resolution proposed for it.
 *
 * A case leaves MANUAL_REVIEW in exactly one way: one operator proposes a
 * resolution -- RESUME or DECLARE_BANKRUPT -- and a second, different operator
 * approves it, which applies the outcome at once; anyone may reject one. The
 * backend and its database enforce the two operators (MG009), and a
 * resolution answers only the review the case is in now. The page offers the
 * commands and shows the backend's answers; the one thing it declines to
 * offer is an approval by the operator who made the proposal, which the
 * backend would refuse.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useDecideResolution,
  useMarginReviews,
  useProposeResolution,
  type DecideResolutionRequest,
  type MarginCaseResolution,
  type MarginResolutionDecisionResult,
  type MarginReviewItem,
  type MarginReviewQueue,
  type ProposeResolutionRequest,
} from "../../../api/advanced/margin.ts";
import { useAssets, type Asset } from "../../../api/queries.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Panel, Pill } from "../../../components/Layout.tsx";
import { Qty } from "../../../components/Money.tsx";
import {
  MARGIN_RESOLUTION_DECISIONS,
  MARGIN_RESOLUTION_OUTCOMES,
  RESOLUTION_DECISION_MEANING,
  RESOLUTION_OUTCOME_MEANING,
  accountStatusTone,
  assetById,
  caseStatusTone,
  checkResolutionReason,
  isOwnProposal,
  resolutionTone,
  type MarginResolutionDecision,
  type MarginResolutionOutcome,
} from "../../../lib/margin.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useSession } from "../../../session.tsx";
import { ScrollTable } from "../shared.tsx";
import { CommandFlow, caseHref, type Prepared } from "./parts.tsx";

/** The review queue, as a panel of the margin operator page. */
export function ReviewQueuePanel(): ReactNode {
  const reviews = useMarginReviews();
  const assets = useAssets();
  return (
    <Panel
      title="Manual review queue"
      description="Liquidation cases a person owns, oldest first. A case leaves review only by a resolution one operator proposes and a second, different operator approves, for the review the case is in now."
    >
      <AsyncPanel
        query={reviews}
        loadingLabel="Loading the cases in manual review…"
        empty={{
          isEmpty: (queue: MarginReviewQueue) => queue.items.length === 0,
          title: "No case in manual review",
          body: "The backend reports no liquidation case in MANUAL_REVIEW.",
        }}
      >
        {(queue: MarginReviewQueue) => (
          <div className="stack">
            {queue.items.map((item) => (
              <ReviewItem key={item.case.id} item={item} assets={assets.data ?? []} />
            ))}
          </div>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function ReviewItem(props: { readonly item: MarginReviewItem; readonly assets: readonly Asset[] }): ReactNode {
  const { item } = props;
  const kase = item.case;
  const unit = assetById(props.assets, kase.numeraire_asset_id)?.symbol ?? "numeraire units";
  const pending = item.resolutions.filter((resolution) => resolution.pending);
  return (
    <article
      className="callout"
      aria-label={`Case ${kase.id} in manual review`}
      data-review-case={kase.id}
    >
      <FieldGrid columns={3}>
        <Field label="Case" note={kase.last_reason}>
          <Link to={caseHref(kase.id)}>
            <code className="mono-small">{kase.id}</code>
          </Link>{" "}
          <Pill tone={caseStatusTone(kase.status)}>{kase.status}</Pill>
        </Field>
        <Field label="Account">
          <Identifier value={kase.account_id} />
        </Field>
        <Field label="In review since" note={exactInstant(item.entered_review_at)}>
          {formatInstant(item.entered_review_at)}
        </Field>
        <Field label="Trading steps" note="Steps executed against the case's budget of trading steps.">
          {String(kase.steps_executed)} of {String(kase.max_steps)}
        </Field>
        <Field label="Deficit">
          {kase.deficit === undefined ? (
            <span className="absent">none recorded</span>
          ) : (
            <Qty value={kase.deficit} decimals={kase.numeraire_decimals} symbol={unit} />
          )}
        </Field>
        <Field label="Current review" note="The transition that put the case in the review a resolution must answer.">
          <Identifier value={item.current_review_transition_id} />
        </Field>
      </FieldGrid>
      {item.resolutions.length === 0 ? (
        <p className="note">No resolution has been proposed for this case.</p>
      ) : (
        <ResolutionsTable item={item} />
      )}
      {pending.map((resolution) => (
        <DecideResolutionForm
          key={resolution.id}
          resolution={resolution}
          current={resolution.review_transition_id === item.current_review_transition_id}
        />
      ))}
      <ProposeResolutionForm caseId={kase.id} />
    </article>
  );
}

function ResolutionsTable(props: { readonly item: MarginReviewItem }): ReactNode {
  const { item } = props;
  return (
    <ScrollTable
      caption={`Resolutions of case ${item.case.id}`}
      headers={["Proposed", "Outcome", "Reason", "Proposed by", "Decision", "Decided by", "Review"]}
    >
      {item.resolutions.map((resolution) => (
        <tr key={resolution.id} data-resolution={resolution.id} data-resolution-pending={String(resolution.pending)}>
          <th scope="row">{formatInstant(resolution.proposed_at)}</th>
          <td>
            <code className="mono-small">{resolution.outcome}</code>
          </td>
          <td className="cell-prose">{resolution.reason}</td>
          <td>
            <Identifier value={resolution.proposed_by} />
          </td>
          <td>
            {resolution.pending ? (
              <Pill tone="info">awaiting a second operator</Pill>
            ) : (
              <>
                <Pill tone={resolutionTone(resolution.decision)}>{resolution.decision ?? "decided"}</Pill>
                {resolution.decision_reason !== undefined && (
                  <span className="cell-note">{resolution.decision_reason}</span>
                )}
              </>
            )}
          </td>
          <td>
            {resolution.decided_by === undefined ? (
              <span className="absent">none yet</span>
            ) : (
              <>
                <Identifier value={resolution.decided_by} />
                {resolution.decided_at !== undefined && (
                  <span className="cell-note">{formatInstant(resolution.decided_at)}</span>
                )}
              </>
            )}
          </td>
          <td>
            {resolution.review_transition_id === item.current_review_transition_id ? (
              <span>current</span>
            ) : (
              <span className="absent">an earlier review</span>
            )}
          </td>
        </tr>
      ))}
    </ScrollTable>
  );
}

function ProposeResolutionForm(props: { readonly caseId: string }): ReactNode {
  const propose = useProposeResolution();
  const [outcome, setOutcome] = useState<MarginResolutionOutcome>("RESUME");
  const [reason, setReason] = useState("");
  const id = `margin-propose-${props.caseId}`;
  const why = checkResolutionReason(reason);
  const prepared: Prepared<ProposeResolutionRequest> = why.ok
    ? { ok: true, request: { caseId: props.caseId, outcome, reason: why.value } }
    : { ok: false, reason: why.error };
  return (
    <form
      className="form"
      aria-label={`Propose a resolution for case ${props.caseId}`}
      onSubmit={(event) => {
        event.preventDefault();
      }}
    >
      <div className="form-row">
        <label htmlFor={`${id}-outcome`}>Outcome</label>
        <select
          id={`${id}-outcome`}
          className="input"
          value={outcome}
          aria-describedby={`${id}-outcome-note`}
          onChange={(event) => {
            const chosen = MARGIN_RESOLUTION_OUTCOMES.find((value) => value === event.target.value);
            if (chosen !== undefined) setOutcome(chosen);
          }}
        >
          {MARGIN_RESOLUTION_OUTCOMES.map((value) => (
            <option key={value} value={value}>
              {value}
            </option>
          ))}
        </select>
        <p id={`${id}-outcome-note`} className="field-note">
          {RESOLUTION_OUTCOME_MEANING[outcome]}
        </p>
      </div>
      <div className="form-row">
        <label htmlFor={`${id}-reason`}>Reason for the proposal</label>
        <textarea
          id={`${id}-reason`}
          className="input"
          rows={2}
          value={reason}
          onChange={(event) => {
            setReason(event.target.value);
          }}
        />
      </div>
      <CommandFlow<ProposeResolutionRequest, MarginCaseResolution>
        label="Propose this resolution"
        prepared={prepared}
        command={propose}
        refusedNote="Nothing was proposed and the case is unchanged."
        describe={(request) => (
          <FieldGrid columns={2}>
            <Field label="Outcome">
              <code className="mono-small">{request.outcome}</code>
            </Field>
            <Field label="Reason">{request.reason}</Field>
          </FieldGrid>
        )}
        renderResult={(resolution) => (
          <p className="note" role="status" aria-label="The proposed resolution" data-proposed-resolution={resolution.id}>
            Proposed {resolution.outcome}; it now awaits a second, different operator. Nothing has changed yet.
          </p>
        )}
      />
    </form>
  );
}

export function DecideResolutionForm(props: { readonly resolution: MarginCaseResolution; readonly current: boolean }): ReactNode {
  const { resolution } = props;
  const session = useSession();
  const decide = useDecideResolution();
  const [decision, setDecision] = useState<MarginResolutionDecision>("APPROVED");
  const [reason, setReason] = useState("");
  const id = `margin-decide-${resolution.id}`;
  const why = checkResolutionReason(reason);
  const own = isOwnProposal(resolution.proposed_by, session.principal?.subject_id);
  let prepared: Prepared<DecideResolutionRequest>;
  if (decision === "APPROVED" && own) {
    prepared = {
      ok: false,
      reason: "You proposed this resolution, and its proposer can never approve it; a second, different operator approves it. You may reject it.",
    };
  } else if (decision === "APPROVED" && !props.current) {
    prepared = {
      ok: false,
      reason: "This resolution answers an earlier review of the case, which the backend refuses to apply. It can only be rejected.",
    };
  } else if (!why.ok) {
    prepared = { ok: false, reason: why.error };
  } else {
    prepared = { ok: true, request: { resolutionId: resolution.id, decision, reason: why.value } };
  }
  return (
    <form
      className="form"
      aria-label={`Decide resolution ${resolution.id}`}
      onSubmit={(event) => {
        event.preventDefault();
      }}
    >
      <p className="command-title">
        Decide the proposed {resolution.outcome} (proposed {formatInstant(resolution.proposed_at)})
      </p>
      <div className="form-row">
        <label htmlFor={`${id}-decision`}>Decision</label>
        <select
          id={`${id}-decision`}
          className="input"
          value={decision}
          aria-describedby={`${id}-decision-note`}
          onChange={(event) => {
            const chosen = MARGIN_RESOLUTION_DECISIONS.find((value) => value === event.target.value);
            if (chosen !== undefined) setDecision(chosen);
          }}
        >
          {MARGIN_RESOLUTION_DECISIONS.map((value) => (
            <option key={value} value={value}>
              {value}
            </option>
          ))}
        </select>
        <p id={`${id}-decision-note`} className="field-note">
          {RESOLUTION_DECISION_MEANING[decision]}
        </p>
      </div>
      <div className="form-row">
        <label htmlFor={`${id}-reason`}>Reason for the decision</label>
        <textarea
          id={`${id}-reason`}
          className="input"
          rows={2}
          value={reason}
          onChange={(event) => {
            setReason(event.target.value);
          }}
        />
      </div>
      <CommandFlow<DecideResolutionRequest, MarginResolutionDecisionResult>
        label={decision === "APPROVED" ? "Approve and apply" : "Reject the resolution"}
        variant={decision === "APPROVED" ? "danger" : "primary"}
        prepared={prepared}
        command={decide}
        refusedNote="No decision was recorded and the case is unchanged."
        describe={(request) => (
          <FieldGrid columns={2}>
            <Field label="Decision">
              <code className="mono-small">{request.decision}</code>
            </Field>
            <Field label="Reason">{request.reason}</Field>
          </FieldGrid>
        )}
        renderResult={(result) => (
          <p className="note" role="status" aria-label="The decision" data-decided-resolution={result.resolution.id}>
            {result.replayed ? "The backend had already recorded this decision and changed nothing again. " : ""}
            The resolution is <Pill tone={resolutionTone(result.resolution.decision)}>{result.resolution.decision ?? "decided"}</Pill>; the
            case is <Pill tone={caseStatusTone(result.case.status)}>{result.case.status}</Pill> and the account{" "}
            <Pill tone={accountStatusTone(result.account_status)}>{result.account_status}</Pill>
            {result.waived.length > 0 && <> ; {String(result.waived.length)} interest period(s) of the review recorded at zero</>}.
          </p>
        )}
      />
    </form>
  );
}
