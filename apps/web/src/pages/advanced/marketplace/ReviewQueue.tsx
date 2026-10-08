/**
 * THE REVIEW QUEUE: operator decisions on strategy versions.
 *
 * Validation scans and builds a submitted version; review, approval and
 * rejection are dual-control decisions the backend refuses to anyone who
 * created the listing or wrote the version, whatever this page shows;
 * moderation flags, takes down or re-clears an approved version; deprecation
 * retires one. Every decision needs a recent strong sign-in, carries an
 * idempotency key created when the operator confirms, and its answer -- the
 * new state or the refusal with its code -- is what the page shows.
 *
 * A principal who is not an operator is refused the queue by the backend;
 * the page shows that refusal rather than hiding the section.
 */
import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  REVIEW_QUEUE_LIMIT,
  useDeprecateVersion,
  useModerateVersion,
  useReviewDecision,
  useReviewQueue,
  useStartReview,
  useValidateVersion,
  type DecisionInput,
  type DeprecateInput,
  type ModerateInput,
  type ReviewDecisionInput,
  type StrategyReviewQueueItem,
} from "../../../api/advanced/marketplace.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { Button, LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel, Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import {
  STATUS_MEANING,
  STRATEGY_STATUSES,
  isReasonCode,
  moderationTargets,
  operatorActionsFor,
  type StrategyModeration,
  type StrategyStatus,
} from "../../../lib/marketplace.ts";
import { formatInstant } from "../../../lib/time.ts";
import { Codes, CursorPager, ScrollTable } from "../shared.tsx";
import { VersionState, strategyHref, usePrepared } from "./parts.tsx";

export function ReviewQueuePage(): ReactNode {
  const [status, setStatus] = useState<StrategyStatus | undefined>(undefined);
  const [selected, setSelected] = useState<StrategyReviewQueueItem | undefined>(undefined);
  const filterId = useId();
  return (
    <Page
      title="Review queue"
      lead="Strategy versions awaiting an operator, oldest first. Every decision needs a recent strong sign-in, and the backend refuses a reviewer who created the strategy."
      actions={<LinkButton to={strategyHref.home()}>Back to strategies</LinkButton>}
    >
      <SimulatedNotice />
      <Panel
        title="Versions"
        description="By default the versions awaiting validation or review. Choosing a status asks the backend for that status."
        actions={
          <div className="filter">
            <label htmlFor={filterId}>Status</label>
            <select
              id={filterId}
              className="input"
              value={status ?? ""}
              onChange={(event) => {
                setStatus(STRATEGY_STATUSES.find((value) => value === event.target.value));
                setSelected(undefined);
              }}
            >
              <option value="">Awaiting validation or review</option>
              {STRATEGY_STATUSES.map((value) => (
                <option key={value} value={value}>
                  {value}
                </option>
              ))}
            </select>
          </div>
        }
      >
        <QueueTable key={status ?? "awaiting"} status={status} onDecide={setSelected} />
      </Panel>
      {selected !== undefined && (
        <DecisionPanel
          key={selected.version.id}
          item={selected}
          onClose={() => {
            setSelected(undefined);
          }}
        />
      )}
    </Page>
  );
}

function QueueTable(props: {
  readonly status: StrategyStatus | undefined;
  readonly onDecide: (item: StrategyReviewQueueItem) => void;
}): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const queue = useReviewQueue(props.status, trail.current);
  return (
    <AsyncPanel
      query={queue}
      loadingLabel="Loading the review queue…"
      empty={{
        isEmpty: (page: CursorPage<StrategyReviewQueueItem>) => page.items.length === 0 && trail.previous.length === 0,
        title: "Nothing waiting",
        body: "The backend lists no version in this state.",
      }}
    >
      {(page: CursorPage<StrategyReviewQueueItem>) => (
        <>
          <ScrollTable caption="Versions in the queue" headers={["Strategy", "Version", "State", "Creator's account", "Published", "Decide"]}>
            {page.items.map((item) => (
              <tr key={item.version.id}>
                <th scope="row">
                  <Link to={strategyHref.listing(item.listing_id)}>{item.listing_title}</Link>
                  <span className="cell-note">
                    <code className="mono-small">{item.version.strategy_ref}</code>
                  </span>
                </th>
                <td>
                  version {String(item.version.number)}
                  <span className="cell-note">{item.version.risk_class}</span>
                </td>
                <td>
                  <VersionState status={item.version.status} moderation={item.version.moderation} />
                  {item.version.rejection_code !== undefined && <Codes codes={[item.version.rejection_code]} />}
                </td>
                <td>
                  <code className="mono-small">{item.creator_account_id}</code>
                </td>
                <td>{formatInstant(item.version.created_at)}</td>
                <td>
                  {operatorActionsFor(item.version).length === 0 ? (
                    <span className="absent">no decision open</span>
                  ) : (
                    <Button
                      onClick={() => {
                        props.onDecide(item);
                      }}
                    >
                      Decide
                    </Button>
                  )}
                </td>
              </tr>
            ))}
          </ScrollTable>
          {page.items.length === REVIEW_QUEUE_LIMIT && (
            <p className="note">The backend returned a full page; older versions follow on the next one.</p>
          )}
          <CursorPager
            noun="versions"
            trail={trail}
            nextCursor={page.nextCursor}
            onForward={(cursor) => {
              setTrail(forward(trail, cursor));
            }}
            onBack={() => {
              setTrail(back(trail));
            }}
          />
        </>
      )}
    </AsyncPanel>
  );
}

function DecisionPanel(props: { readonly item: StrategyReviewQueueItem; readonly onClose: () => void }): ReactNode {
  const { item } = props;
  const v = item.version;
  const actions = operatorActionsFor(v);
  return (
    <Panel
      title={`Decide on ${item.listing_title}, version ${String(v.number)}`}
      description={STATUS_MEANING[v.status as StrategyStatus]}
      actions={
        <Button variant="quiet" onClick={props.onClose}>
          Close
        </Button>
      }
    >
      <FieldGrid columns={3}>
        <Field label="Version">
          <Identifier value={v.id} />
        </Field>
        <Field label="State">
          <VersionState status={v.status} moderation={v.moderation} />
        </Field>
        <Field label="Creator's account">
          <Identifier value={item.creator_account_id} />
        </Field>
      </FieldGrid>
      <p className="note">
        The full version -- manifest, source digest and provenance -- is on{" "}
        <Link to={strategyHref.listing(item.listing_id)}>its listing</Link>.
      </p>
      {actions.includes("validate") && <SimpleDecision versionId={v.id} kind="validate" />}
      {actions.includes("review") && <SimpleDecision versionId={v.id} kind="review" />}
      {(actions.includes("approve") || actions.includes("reject")) && (
        <ReviewForm versionId={v.id} canApprove={actions.includes("approve")} />
      )}
      {actions.includes("moderate") && <ModerationForm versionId={v.id} moderation={v.moderation} />}
      {actions.includes("deprecate") && <DeprecateForm versionId={v.id} />}
    </Panel>
  );
}

/** Validation or the start of a review: a decision with nothing to fill in. */
function SimpleDecision(props: { readonly versionId: string; readonly kind: "validate" | "review" }): ReactNode {
  const validate = useValidateVersion();
  const review = useStartReview();
  const action = usePrepared<DecisionInput>();
  const pending = validate.isPending || review.isPending;
  const run = (): void => {
    const input = action.prepared ?? action.prepare((idempotencyKey) => ({ versionId: props.versionId, idempotencyKey }));
    if (props.kind === "validate") validate.mutate(input);
    else review.mutate(input);
  };
  return (
    <div className="callout">
      <p>
        {props.kind === "validate"
          ? "Validation scans the source and builds it into a sandbox artifact. The build holds the version while it runs and is bounded by the request's time limit; a deployment without a Go toolchain answers UNSUPPORTED."
          : "Starting the review takes the validated version into review. Only an operator who did not create it may."}
      </p>
      <div className="form-actions">
        <Button variant="primary" busy={pending} busyLabel={props.kind === "validate" ? "Scanning and building…" : "Starting…"} onClick={run}>
          {props.kind === "validate" ? "Validate: scan and build" : "Start the review"}
        </Button>
      </div>
      {validate.isSuccess && (
        <p role="status">
          Validation <Pill tone={validate.data.validation.outcome === "PASSED" ? "good" : "bad"}>{validate.data.validation.outcome}</Pill>
          {validate.data.validation.failure_reason !== undefined && <Codes codes={[validate.data.validation.failure_reason]} />}: the
          version is now <Pill tone="info">{validate.data.version.status}</Pill>.
          {validate.data.validation.artifact_digest !== undefined && (
            <span className="cell-note">
              artifact <Identifier value={validate.data.validation.artifact_digest} />
            </span>
          )}
        </p>
      )}
      {review.isSuccess && (
        <p role="status">
          The version is now <Pill tone="info">{review.data.status}</Pill>.
        </p>
      )}
      {(validate.isError || review.isError) && (
        <Explanation error={validate.isError ? validate.error : review.error} retryLabel="Retry with the same key" onRetry={run} />
      )}
    </div>
  );
}

function ReviewForm(props: { readonly versionId: string; readonly canApprove: boolean }): ReactNode {
  const decide = useReviewDecision();
  const action = usePrepared<ReviewDecisionInput>();
  const [reasonCode, setReasonCode] = useState("");
  const [note, setNote] = useState("");
  const [problem, setProblem] = useState<string | undefined>(undefined);
  const codeId = useId();
  const noteId = useId();
  const send = (decision: "approve" | "reject"): void => {
    if (action.prepared !== undefined && action.prepared.decision === decision) {
      decide.mutate(action.prepared);
      return;
    }
    if (!isReasonCode(reasonCode.trim())) {
      setProblem("A reason code is upper-case letters, digits and underscores, such as MEETS_POLICY.");
      return;
    }
    setProblem(undefined);
    decide.mutate(
      action.prepare((idempotencyKey) => ({
        versionId: props.versionId,
        decision,
        reasonCode: reasonCode.trim(),
        note: note.trim(),
        idempotencyKey,
      })),
    );
  };
  return (
    <form
      className="form callout"
      aria-label="Review decision"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
      }}
    >
      <div className="form-row">
        <label htmlFor={codeId}>Reason code</label>
        <input
          id={codeId}
          className="input"
          value={reasonCode}
          autoComplete="off"
          onChange={(event) => {
            setReasonCode(event.target.value.toUpperCase());
            action.clear();
          }}
        />
      </div>
      <div className="form-row">
        <label htmlFor={noteId}>Note (optional)</label>
        <textarea
          id={noteId}
          className="input"
          rows={3}
          maxLength={2000}
          value={note}
          onChange={(event) => {
            setNote(event.target.value);
            action.clear();
          }}
        />
      </div>
      {problem !== undefined && (
        <p className="field-error" role="alert">
          {problem}
        </p>
      )}
      <div className="form-actions">
        {props.canApprove ? (
          <Button
            variant="primary"
            busy={decide.isPending}
            busyLabel="Recording…"
            onClick={() => {
              send("approve");
            }}
          >
            Approve
          </Button>
        ) : (
          <Button disabledReason="Only a version IN_REVIEW can be approved; start the review first.">Approve</Button>
        )}
        <Button
          variant="danger"
          busy={decide.isPending}
          busyLabel="Recording…"
          onClick={() => {
            send("reject");
          }}
        >
          Reject
        </Button>
      </div>
      {decide.isSuccess && (
        <p role="status">
          Recorded: the version is now <Pill tone="info">{decide.data.status}</Pill>
          {decide.data.rejection_code !== undefined && <Codes codes={[decide.data.rejection_code]} />}.
        </p>
      )}
      {decide.isError && <Explanation error={decide.error} />}
    </form>
  );
}

function ModerationForm(props: { readonly versionId: string; readonly moderation: string }): ReactNode {
  const moderate = useModerateVersion();
  const action = usePrepared<ModerateInput>();
  const targets = moderationTargets(props.moderation);
  const [to, setTo] = useState<StrategyModeration | undefined>(targets[0]);
  const [reason, setReason] = useState("");
  const [problem, setProblem] = useState<string | undefined>(undefined);
  const toId = useId();
  const reasonId = useId();
  if (targets.length === 0 || to === undefined) {
    return <p className="note">This version's moderation state cannot change.</p>;
  }
  return (
    <form
      className="form callout"
      aria-label="Moderation"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (action.prepared !== undefined) {
          moderate.mutate(action.prepared);
          return;
        }
        if (reason.trim() === "") {
          setProblem("Moderation needs a reason.");
          return;
        }
        setProblem(undefined);
        moderate.mutate(
          action.prepare((idempotencyKey) => ({ versionId: props.versionId, to, reason: reason.trim(), idempotencyKey })),
        );
      }}
    >
      <div className="form-row">
        <label htmlFor={toId}>Moderation state</label>
        <select
          id={toId}
          className="input"
          value={to}
          onChange={(event) => {
            setTo(targets.find((value) => value === event.target.value));
            action.clear();
          }}
        >
          {targets.map((value) => (
            <option key={value} value={value}>
              {value}
            </option>
          ))}
        </select>
      </div>
      <div className="form-row">
        <label htmlFor={reasonId}>Reason</label>
        <input
          id={reasonId}
          className="input"
          value={reason}
          maxLength={1000}
          autoComplete="off"
          onChange={(event) => {
            setReason(event.target.value);
            action.clear();
          }}
        />
      </div>
      {problem !== undefined && (
        <p className="field-error" role="alert">
          {problem}
        </p>
      )}
      <div className="form-actions">
        <Button submit busy={moderate.isPending} busyLabel="Recording…">
          Change moderation
        </Button>
      </div>
      {moderate.isSuccess && (
        <p role="status">
          Recorded: moderation is now <Pill tone="info">{moderate.data.moderation}</Pill>.
        </p>
      )}
      {moderate.isError && <Explanation error={moderate.error} />}
    </form>
  );
}

function DeprecateForm(props: { readonly versionId: string }): ReactNode {
  const deprecate = useDeprecateVersion();
  const action = usePrepared<DeprecateInput>();
  const [reason, setReason] = useState("");
  const [problem, setProblem] = useState<string | undefined>(undefined);
  const reasonId = useId();
  return (
    <form
      className="form callout"
      aria-label="Deprecation"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (action.prepared !== undefined) {
          deprecate.mutate(action.prepared);
          return;
        }
        if (reason.trim() === "") {
          setProblem("Deprecation needs a reason.");
          return;
        }
        setProblem(undefined);
        deprecate.mutate(action.prepare((idempotencyKey) => ({ versionId: props.versionId, reason: reason.trim(), idempotencyKey })));
      }}
    >
      <div className="form-row">
        <label htmlFor={reasonId}>Why retire it</label>
        <input
          id={reasonId}
          className="input"
          value={reason}
          maxLength={1000}
          autoComplete="off"
          onChange={(event) => {
            setReason(event.target.value);
            action.clear();
          }}
        />
        <p className="field-note">No new installs; existing installs can no longer be followed.</p>
      </div>
      {problem !== undefined && (
        <p className="field-error" role="alert">
          {problem}
        </p>
      )}
      <div className="form-actions">
        <Button submit variant="danger" busy={deprecate.isPending} busyLabel="Recording…">
          Deprecate
        </Button>
      </div>
      {deprecate.isSuccess && (
        <p role="status">
          Recorded: the version is now <Pill tone="info">{deprecate.data.status}</Pill>.
        </p>
      )}
      {deprecate.isError && <Explanation error={deprecate.error} />}
    </form>
  );
}
