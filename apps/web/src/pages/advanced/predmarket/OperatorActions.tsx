/**
 * The operator's actions on one market: its lifecycle, the sandbox oracle's
 * observation, resolution, disputes and rulings, voiding, settlement, and the
 * escrow reconciliation.
 *
 * The panel offers the actions the market's current status admits, and the
 * backend decides each one: permissions, step-up, dual control (the creator
 * never resolves or voids, a disputer never rules), times and the escrow
 * identity are all its rules, and a refusal is shown with its code. Every
 * action carries an idempotency key created when the operator confirms and
 * reused, with the same request, on every retry. Settling and voiding move
 * value and ask for a review first.
 */
import { useId, useState, type ReactNode } from "react";
import type { UseMutationResult } from "@tanstack/react-query";

import {
  useDisputeMarket,
  useMarketStep,
  useObserveOutcome,
  usePredictionOracle,
  useReconcileMarket,
  useResolveMarket,
  useSettleMarket,
  useUpholdResolution,
  useVoidMarket,
  type EvidenceCommand,
  type ObserveCommand,
  type PredictionDisputeResult,
  type PredictionEscrowReconciliation,
  type PredictionMarket,
  type PredictionOracleObservation,
  type PredictionResolutionResult,
  type PredictionSettlementResult,
  type ReasonCommand,
  type StepCommand,
} from "../../../api/advanced/predmarket.ts";
import { Button } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Panel, Pill } from "../../../components/Layout.tsx";
import { Qty } from "../../../components/Money.tsx";
import { localInputToInstant, reasonProblem, statusTone } from "../../../lib/predmarket.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useConfirmation } from "./confirm.ts";
import { CommandFailure, KeyLine } from "./parts.tsx";

interface ActionFormProps<C, R> {
  readonly title: string;
  readonly description: string;
  readonly confirmLabel: string;
  readonly busyLabel: string;
  /** When present, the action asks for a review first and this states what it will do. */
  readonly review?: string | undefined;
  /** Why the action cannot be sent as filled in; the button says so instead of sending. */
  readonly problem?: string | undefined;
  readonly build: (key: string) => C;
  readonly mutation: UseMutationResult<R, unknown, C>;
  readonly renderResult: (result: R) => ReactNode;
  /**
   * Hands the backend's answer to the panel. Most actions change the market's
   * status, and with it which actions the panel offers, so the form that sent
   * one is gone once the market is read again; the answer must outlive it.
   */
  readonly onAnswer: (answer: ReactNode) => void;
  readonly nothingHappened: string;
  readonly children?: ReactNode;
}

/** One operator action: its inputs, its confirmation and key, and the backend's answer. */
function ActionForm<C, R>(props: ActionFormProps<C, R>): ReactNode {
  const { mutation } = props;
  const confirmation = useConfirmation<C>();
  const [reviewing, setReviewing] = useState(false);
  const twoStep = props.review !== undefined;

  const startOver = (): void => {
    confirmation.reset();
    mutation.reset();
    setReviewing(false);
  };
  const submit = (): void => {
    mutation.mutate(confirmation.confirm(props.build), {
      onSuccess: (data) => {
        props.onAnswer(
          <>
            <p className="step-label">{props.title}</p>
            {props.renderResult(data)}
          </>,
        );
      },
    });
  };

  return (
    <section className="callout" aria-label={props.title}>
      <h3>{props.title}</h3>
      <p className="panel-description">{props.description}</p>
      {mutation.isSuccess ? (
        <div>
          <p>The backend carried this out; its answer is shown at the top of this panel.</p>
          <div className="form-actions">
            <Button onClick={startOver}>Done</Button>
          </div>
        </div>
      ) : (
        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            if (twoStep && !reviewing) {
              setReviewing(true);
              return;
            }
            submit();
          }}
        >
          {props.children !== undefined && (
            <fieldset disabled={reviewing || confirmation.command !== undefined}>{props.children}</fieldset>
          )}
          {reviewing && (
            <p className="note" role="status">
              {props.review}
            </p>
          )}
          <KeyLine confirmation={confirmation} />
          {/* After a failure only the failure's own choices remain: the same
              command under the same key, or a new one. */}
          {!mutation.isError && (
            <div className="form-actions">
              {props.problem !== undefined ? (
                <Button variant="primary" disabledReason={props.problem}>
                  {twoStep ? "Review" : props.confirmLabel}
                </Button>
              ) : twoStep && !reviewing ? (
                <Button variant="primary" submit>
                  Review
                </Button>
              ) : (
                <Button variant="primary" submit busy={mutation.isPending} busyLabel={props.busyLabel}>
                  {props.confirmLabel}
                </Button>
              )}
              {(reviewing || confirmation.command !== undefined) && !mutation.isPending && (
                <Button variant="quiet" onClick={startOver}>
                  Start over
                </Button>
              )}
            </div>
          )}
          {mutation.isError && (
            <CommandFailure
              error={mutation.error}
              onRetry={submit}
              onStartOver={startOver}
              nothingHappened={props.nothingHappened}
            />
          )}
        </form>
      )}
    </section>
  );
}

/** A reason, and optionally a statement of the evidence, as the service records them. */
function ReasonFields(props: {
  readonly reason: string;
  readonly onReason: (value: string) => void;
  readonly evidence?: { readonly value: string; readonly onChange: (value: string) => void; readonly required: boolean };
}): ReactNode {
  const reasonId = useId();
  const evidenceId = useId();
  return (
    <>
      <div className="form-row">
        <label htmlFor={reasonId}>Reason</label>
        <textarea
          id={reasonId}
          className="input"
          rows={2}
          value={props.reason}
          onChange={(event) => {
            props.onReason(event.target.value);
          }}
        />
      </div>
      {props.evidence !== undefined && (
        <div className="form-row">
          <label htmlFor={evidenceId}>{props.evidence.required ? "Evidence" : "Evidence (optional)"}</label>
          <textarea
            id={evidenceId}
            className="input"
            rows={2}
            value={props.evidence.value}
            onChange={(event) => {
              props.evidence?.onChange(event.target.value);
            }}
          />
          <p className="field-note">Archived with the record as the statement you wrote here.</p>
        </div>
      )}
    </>
  );
}

function MarketAfter(props: { readonly market: PredictionMarket }): ReactNode {
  return (
    <p>
      The market is now <Pill tone={statusTone(props.market.status)}>{props.market.status}</Pill>.
    </p>
  );
}

export function OperatorActionsPanel(props: { readonly market: PredictionMarket }): ReactNode {
  const { market } = props;
  const status = market.status;
  const open = useMarketStep("open");
  const close = useMarketStep("close");
  const requestResolution = useMarketStep("request-resolution");
  const observe = useObserveOutcome();
  const resolve = useResolveMarket();
  const dispute = useDisputeMarket();
  const uphold = useUpholdResolution();
  const voidMarket = useVoidMarket();
  const settle = useSettleMarket();
  const reconcile = useReconcileMarket();
  // The backend's answer to the last action, kept here because the form that
  // sent it usually disappears with the status it changed.
  const [answer, setAnswer] = useState<ReactNode | undefined>(undefined);

  const terminal = status === "SETTLED" || status === "VOIDED";
  const awaitingReport = status === "RESOLVING" || status === "DISPUTED";
  const step = (key: string): StepCommand => ({ marketId: market.id, idempotencyKey: key });

  return (
    <Panel
      title="Operator actions"
      description="The actions this market's status admits. The backend decides each one, including who may take it: the market's creator never resolves or voids it, and an operator who raised a dispute never rules on it."
    >
      {answer !== undefined && (
        <div className="callout" role="status">
          <p className="panel-description">The backend&apos;s answer to your last action:</p>
          {answer}
        </div>
      )}
      {status === "DRAFT" && (
        <ActionForm
          title="Open the market"
          description="Opens trading of complete sets, at or after the open time and before the close time."
          confirmLabel="Open the market"
          busyLabel="Opening…"
          build={step}
          mutation={open}
          onAnswer={setAnswer}
          renderResult={(result: PredictionMarket) => <MarketAfter market={result} />}
          nothingHappened="The market was not opened by this attempt."
        />
      )}
      {status === "OPEN" && (
        <ActionForm
          title="Close the market"
          description="At or after the close time: nothing is issued or redeemed afterwards, and the outcome instruments halt."
          confirmLabel="Close the market"
          busyLabel="Closing…"
          build={step}
          mutation={close}
          onAnswer={setAnswer}
          renderResult={(result: PredictionMarket) => <MarketAfter market={result} />}
          nothingHappened="The market was not closed by this attempt."
        />
      )}
      {status === "CLOSED" && (
        <ActionForm
          title="Request resolution"
          description="Moves the market to RESOLVING: an oracle report is now awaited."
          confirmLabel="Request resolution"
          busyLabel="Requesting…"
          build={step}
          mutation={requestResolution}
          onAnswer={setAnswer}
          renderResult={(result: PredictionMarket) => <MarketAfter market={result} />}
          nothingHappened="Resolution was not requested by this attempt."
        />
      )}
      {awaitingReport && <ObservationAction market={market} mutation={observe} onAnswer={setAnswer} />}
      {awaitingReport && <ResolveAction market={market} mutation={resolve} onAnswer={setAnswer} />}
      {status === "RESOLVED" && <DisputeAction market={market} mutation={dispute} onAnswer={setAnswer} />}
      {status === "DISPUTED" && <UpholdAction market={market} mutation={uphold} onAnswer={setAnswer} />}
      {status === "RESOLVED" && (
        <ActionForm
          title="Settle the market"
          description="Pays every share of the winning outcome one collateral unit from the escrow and retires every share. Refused while the dispute window is open."
          confirmLabel="Confirm settlement"
          busyLabel="Settling…"
          review={`Settle ${market.symbol}: the escrow pays the holders of the winning outcome and every share is retired. This cannot be undone.`}
          build={step}
          mutation={settle}
          onAnswer={setAnswer}
          renderResult={(result: PredictionSettlementResult) => <SettlementOutcome result={result} />}
          nothingHappened="Nothing was paid by this attempt: a settlement is all or nothing."
        />
      )}
      {!terminal && <VoidAction market={market} mutation={voidMarket} onAnswer={setAnswer} />}
      <ActionForm
        title="Reconcile the escrow"
        description="Compares the escrow with the shares outstanding (PREDICTION_ESCROW) and records what it found. It moves nothing."
        confirmLabel="Run the reconciliation"
        busyLabel="Reconciling…"
        build={step}
        mutation={reconcile}
        onAnswer={setAnswer}
        renderResult={(result: PredictionEscrowReconciliation) => <ReconciliationOutcome record={result} />}
        nothingHappened="No reconciliation record was written by this attempt."
      />
    </Panel>
  );
}

function ObservationAction(props: {
  readonly market: PredictionMarket;
  readonly mutation: UseMutationResult<PredictionOracleObservation, unknown, ObserveCommand>;
  readonly onAnswer: (answer: ReactNode) => void;
}): ReactNode {
  const { market } = props;
  const oracle = usePredictionOracle(true);
  const [outcome, setOutcome] = useState(market.outcomes[0]?.symbol ?? "");
  const [observedAt, setObservedAt] = useState("");
  const [evidence, setEvidence] = useState("");
  const outcomeId = useId();
  const atId = useId();
  const evidenceId = useId();
  const at = observedAt === "" ? market.close_at : localInputToInstant(observedAt);

  if (oracle.isError) {
    return (
      <section className="callout" aria-label="Record the sandbox oracle's observation">
        <h3>Record the sandbox oracle&apos;s observation</h3>
        <Explanation error={oracle.error}>
          <p>Without a sandbox oracle there is nothing to record an observation in, and no report to resolve from.</p>
        </Explanation>
      </section>
    );
  }
  return (
    <ActionForm
      title="Record the sandbox oracle's observation"
      description="The sandbox oracle stands in for the world this market asks about. What you record is what it will sign when a resolution asks it for a report; a later observation replaces an earlier one."
      confirmLabel="Record the observation"
      busyLabel="Recording…"
      problem={
        at === undefined
          ? "The observation time is not a time."
          : evidence.trim() === ""
            ? "State what the oracle observed; the evidence is signed with the report."
            : undefined
      }
      build={(key) => ({ marketId: market.id, idempotencyKey: key, outcome, observedAt: at ?? "", evidence: evidence.trim() })}
      mutation={props.mutation}
      onAnswer={props.onAnswer}
      renderResult={(result: PredictionOracleObservation) => (
        <FieldGrid columns={2}>
          <Field label="Outcome the oracle will report">{result.report.outcome}</Field>
          <Field label="Observed at">{formatInstant(result.report.observed_at)}</Field>
          <Field label="Source">{result.report.source}</Field>
          <Field label="Report hash" note="The hash a resolution from this report will record.">
            <Identifier value={result.report_hash} />
          </Field>
        </FieldGrid>
      )}
      nothingHappened="The oracle recorded nothing from this attempt."
    >
      <div className="form-row">
        <label htmlFor={outcomeId}>Outcome observed</label>
        <select
          id={outcomeId}
          className="input"
          value={outcome}
          onChange={(event) => {
            setOutcome(event.target.value);
          }}
        >
          {market.outcomes.map((candidate) => (
            <option key={candidate.index} value={candidate.symbol}>
              {candidate.symbol}
            </option>
          ))}
        </select>
      </div>
      <div className="form-row">
        <label htmlFor={atId}>Observed at (your local time; optional)</label>
        <input
          id={atId}
          className="input"
          type="datetime-local"
          step={1}
          value={observedAt}
          onChange={(event) => {
            setObservedAt(event.target.value);
          }}
        />
        <p className="field-note">
          Left empty, the market&apos;s close time. A report observed before close, or in the future, is refused.
        </p>
      </div>
      <div className="form-row">
        <label htmlFor={evidenceId}>Evidence</label>
        <textarea
          id={evidenceId}
          className="input"
          rows={2}
          value={evidence}
          onChange={(event) => {
            setEvidence(event.target.value);
          }}
        />
      </div>
    </ActionForm>
  );
}

function ResolutionOutcome(props: { readonly result: PredictionResolutionResult }): ReactNode {
  const { result } = props;
  return (
    <>
      {result.replay && <p className="note">This report had already been accepted; nothing changed.</p>}
      <FieldGrid columns={3}>
        <Field label="Resolution">{result.resolution.kind}</Field>
        <Field label="Outcome">{result.resolution.outcome_symbol}</Field>
        <Field label="Market">
          <Pill tone={statusTone(result.market.status)}>{result.market.status}</Pill>
        </Field>
      </FieldGrid>
    </>
  );
}

function ResolveAction(props: {
  readonly market: PredictionMarket;
  readonly mutation: UseMutationResult<PredictionResolutionResult, unknown, ReasonCommand>;
  readonly onAnswer: (answer: ReactNode) => void;
}): ReactNode {
  const [reason, setReason] = useState("");
  return (
    <ActionForm
      title={props.market.status === "DISPUTED" ? "Rule on the dispute from a new report" : "Resolve from the oracle's report"}
      description="The backend asks the configured oracle for its report and accepts it only if it verifies against the key this market registered, names one of its outcomes, and was observed after close. On a dispute, a report naming another outcome overturns the resolution."
      confirmLabel="Resolve"
      busyLabel="Verifying the report…"
      build={(key) => ({ marketId: props.market.id, idempotencyKey: key, reason })}
      mutation={props.mutation}
      onAnswer={props.onAnswer}
      renderResult={(result: PredictionResolutionResult) => <ResolutionOutcome result={result} />}
      nothingHappened="The market was not resolved by this attempt."
    >
      <ReasonFields reason={reason} onReason={setReason} />
    </ActionForm>
  );
}

function DisputeAction(props: {
  readonly market: PredictionMarket;
  readonly mutation: UseMutationResult<PredictionDisputeResult, unknown, EvidenceCommand>;
  readonly onAnswer: (answer: ReactNode) => void;
}): ReactNode {
  const [reason, setReason] = useState("");
  const [evidence, setEvidence] = useState("");
  const problem = reasonProblem(reason) ?? (evidence.trim() === "" ? "A dispute carries its evidence." : undefined);
  return (
    <ActionForm
      title="Dispute the resolution"
      description="Blocks settlement until another operator rules on it. Only the first, oracle resolution can be disputed, and only once."
      confirmLabel="Raise the dispute"
      busyLabel="Raising the dispute…"
      {...(problem === undefined ? {} : { problem })}
      build={(key) => ({ marketId: props.market.id, idempotencyKey: key, reason, evidence })}
      mutation={props.mutation}
      onAnswer={props.onAnswer}
      renderResult={(result: PredictionDisputeResult) => (
        <FieldGrid columns={2}>
          <Field label="Dispute">
            <Identifier value={result.dispute.id} />
          </Field>
          <Field label="Market">
            <Pill tone={statusTone(result.market.status)}>{result.market.status}</Pill>
          </Field>
        </FieldGrid>
      )}
      nothingHappened="No dispute was raised by this attempt."
    >
      <ReasonFields reason={reason} onReason={setReason} evidence={{ value: evidence, onChange: setEvidence, required: true }} />
    </ActionForm>
  );
}

function UpholdAction(props: {
  readonly market: PredictionMarket;
  readonly mutation: UseMutationResult<PredictionResolutionResult, unknown, EvidenceCommand>;
  readonly onAnswer: (answer: ReactNode) => void;
}): ReactNode {
  const [reason, setReason] = useState("");
  const [evidence, setEvidence] = useState("");
  const problem = reasonProblem(reason);
  return (
    <ActionForm
      title="Uphold the disputed resolution"
      description="Closes the dispute keeping the disputed outcome. A ruling is final."
      confirmLabel="Uphold the resolution"
      busyLabel="Recording the ruling…"
      {...(problem === undefined ? {} : { problem })}
      build={(key) => ({ marketId: props.market.id, idempotencyKey: key, reason, evidence })}
      mutation={props.mutation}
      onAnswer={props.onAnswer}
      renderResult={(result: PredictionResolutionResult) => <ResolutionOutcome result={result} />}
      nothingHappened="No ruling was recorded by this attempt."
    >
      <ReasonFields reason={reason} onReason={setReason} evidence={{ value: evidence, onChange: setEvidence, required: false }} />
    </ActionForm>
  );
}

function VoidAction(props: {
  readonly market: PredictionMarket;
  readonly mutation: UseMutationResult<PredictionSettlementResult, unknown, EvidenceCommand>;
  readonly onAnswer: (answer: ReactNode) => void;
}): ReactNode {
  const [reason, setReason] = useState("");
  const [evidence, setEvidence] = useState("");
  const problem = reasonProblem(reason);
  return (
    <ActionForm
      title="Void the market"
      description="Refunds every share at the market's refund rate, rounded down once per holder; what is not paid stays in the escrow as a disclosed residual."
      confirmLabel="Confirm the void"
      busyLabel="Voiding…"
      review={`Void ${props.market.symbol}: every share is refunded and the market can never settle. This cannot be undone.`}
      {...(problem === undefined ? {} : { problem })}
      build={(key) => ({ marketId: props.market.id, idempotencyKey: key, reason, evidence })}
      mutation={props.mutation}
      onAnswer={props.onAnswer}
      renderResult={(result: PredictionSettlementResult) => <SettlementOutcome result={result} />}
      nothingHappened="Nothing was refunded by this attempt: a void is all or nothing."
    >
      <ReasonFields reason={reason} onReason={setReason} evidence={{ value: evidence, onChange: setEvidence, required: false }} />
    </ActionForm>
  );
}

/** What a settlement or void paid, line by line, exactly as the backend answered. */
function SettlementOutcome(props: { readonly result: PredictionSettlementResult }): ReactNode {
  const { result } = props;
  const { market, settlement } = result;
  return (
    <>
      {result.replay && (
        <p className="note">The market had already settled; this is its record, and nothing was paid by this call.</p>
      )}
      <FieldGrid columns={3}>
        <Field label="Settlement">{settlement.kind}</Field>
        <Field label="Escrow before">
          <Qty value={settlement.escrow_before} decimals={market.collateral_decimals} symbol={market.collateral_symbol} />
        </Field>
        <Field label="Paid">
          <Qty value={settlement.total_paid} decimals={market.collateral_decimals} symbol={market.collateral_symbol} />
        </Field>
        <Field label="Residual kept by the escrow">
          <Qty value={settlement.residual} decimals={market.collateral_decimals} symbol={market.collateral_symbol} />
        </Field>
        <Field label="Holders paid">{String(settlement.holder_count)}</Field>
        <Field label="Market">
          <Pill tone={statusTone(market.status)}>{market.status}</Pill>
        </Field>
      </FieldGrid>
      {result.lines.length > 0 && (
        <ul className="link-list">
          {result.lines.map((line) => (
            <li key={line.account_id}>
              <code className="mono-small">{line.account_id}</code> paid{" "}
              <Qty value={line.amount} decimals={market.collateral_decimals} symbol={market.collateral_symbol} />
              {line.journal_transaction_id !== undefined && (
                <span className="cell-note">
                  journal <code className="mono-small">{line.journal_transaction_id}</code>
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

function ReconciliationOutcome(props: { readonly record: PredictionEscrowReconciliation }): ReactNode {
  const { record } = props;
  const violations = record.difference["violations"];
  return (
    <FieldGrid columns={3}>
      <Field label="Record">
        <Identifier value={record.id} />
      </Field>
      <Field label="Status">
        <Pill tone={record.status === "MATCHED" ? "good" : "bad"}>{record.status}</Pill>
      </Field>
      <Field label="Material" note="A material mismatch blocks new risk until it is resolved.">
        {record.material ? "yes" : "no"}
      </Field>
      <Field label="Violations">
        {Array.isArray(violations) && violations.length > 0 ? (
          <ul className="codes">
            {violations.map((violation, index) => (
              <li key={`${String(index)}-${String(violation)}`}>{String(violation)}</li>
            ))}
          </ul>
        ) : (
          <span className="absent">none recorded</span>
        )}
      </Field>
    </FieldGrid>
  );
}
