/**
 * Pieces the agent authority pages share: the section's navigation, labelled
 * inputs, the decision trail of a swap or a withdrawal, and the confirmation
 * flows of its commands. None of them renders a figure of its own.
 *
 * Every command here is confirmed in two steps -- the request is reviewed,
 * then confirmed -- and the confirmation creates the idempotency key, reused
 * for every retry of that same attempt. The backend decides every rule and
 * its answer, or its refusal with the stable code and request id, is what is
 * shown.
 */
import { useId, useState, type ReactNode } from "react";
import { NavLink } from "react-router-dom";
import { newIdempotencyKey } from "@controlplane/generated-client";

import type {
  DelegationApproval,
  DelegationCommand,
  DelegationExecution,
  DelegationInspection,
  DelegationTransition,
  DecisionRequest,
  ReasonRequest,
} from "../../../api/advanced/delegation.ts";
import { Button } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { confirmAttempt, checkText, verdictTone, type ConfirmedAttempt, type Decision } from "../../../lib/delegation.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref, useAdvancedAccount } from "../area.ts";
import { Codes, ScrollTable } from "../shared.tsx";

/** The section's path under /advanced. */
export const DELEGATION_PATH = "agent-authority";

export function delegationHref(...segments: readonly string[]): string {
  return advancedHref(DELEGATION_PATH, ...segments);
}

/** The pages of the section. The operator's view is offered to operators only; the backend decides either way. */
export function DelegationNav(): ReactNode {
  const account = useAdvancedAccount();
  const links: ReadonlyArray<readonly [string, string]> = [
    [delegationHref(), "Capabilities and requests"],
    [delegationHref("destinations"), "Destinations and withdrawal policy"],
    ...(account.canInspect ? ([[delegationHref("operator"), "Operator view"]] as const) : []),
  ];
  return (
    <nav className="subnav subnav-inline" aria-label="Agent authority pages">
      <ul>
        {links.map(([to, label]) => (
          <li key={to}>
            <NavLink to={to} end className={({ isActive }) => (isActive ? "subnav-link subnav-link-active" : "subnav-link")}>
              {label}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}

/** Says why a command is not offered while an operator reads another account. */
export function InspectingNote(props: { readonly what: string }): ReactNode {
  return (
    <p className="note">
      You are inspecting another account. Only the account&apos;s owner may {props.what}, so nothing here could act on it.
    </p>
  );
}

/** A labelled text input with its note, or its problem announced to assistive technology. */
export function TextInput(props: {
  readonly label: string;
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly note: string;
  /** Shown instead of the note when the typed value is not usable. */
  readonly problem?: string | undefined;
  readonly multiline?: boolean;
  readonly inputMode?: "text" | "decimal" | "numeric";
}): ReactNode {
  const id = useId();
  const noteId = useId();
  const invalid = props.problem !== undefined && props.value !== "";
  const common = {
    id,
    className: "input",
    value: props.value,
    autoComplete: "off",
    spellCheck: false,
    "aria-describedby": noteId,
    ...(invalid ? { "aria-invalid": true } : {}),
  };
  return (
    <div className="form-row">
      <label htmlFor={id}>{props.label}</label>
      {props.multiline === true ? (
        <textarea
          {...common}
          rows={3}
          onChange={(event) => {
            props.onChange(event.target.value);
          }}
        />
      ) : (
        <input
          {...common}
          inputMode={props.inputMode ?? "text"}
          onChange={(event) => {
            props.onChange(event.target.value);
          }}
        />
      )}
      <p id={noteId} className={invalid ? "field-error" : "field-note"}>
        {invalid ? props.problem : props.note}
      </p>
    </div>
  );
}

/** A labelled checkbox. */
export function Checkbox(props: {
  readonly label: string;
  readonly checked: boolean;
  readonly onChange: (checked: boolean) => void;
}): ReactNode {
  return (
    <label className="checkbox">
      <input
        type="checkbox"
        checked={props.checked}
        onChange={(event) => {
          props.onChange(event.target.checked);
        }}
      />{" "}
      {props.label}
    </label>
  );
}

/** The key of the attempt on screen, where a person (and a test) can read it. */
export function AttemptKey(props: { readonly attempt: ConfirmedAttempt<unknown> | undefined }): ReactNode {
  if (props.attempt === undefined) {
    return <p className="field-note">An idempotency key is created when you confirm, and reused if you retry.</p>;
  }
  return (
    <p className="mono-small" data-idempotency-key={props.attempt.idempotencyKey}>
      idempotency key {props.attempt.idempotencyKey} — created at confirmation; a retry of this attempt reuses it
    </p>
  );
}

/** Who did something, as the backend recorded it. */
export function ActorText(props: { readonly actor: { readonly actor_type: string; readonly actor_id: string } | undefined }): ReactNode {
  if (props.actor === undefined) return <span className="absent">not recorded</span>;
  return (
    <span>
      <Pill>{props.actor.actor_type}</Pill> <code className="mono-small">{props.actor.actor_id}</code>
    </span>
  );
}

/** Every status change, oldest first, with who made it and why. */
export function TransitionTable(props: { readonly transitions: readonly DelegationTransition[] }): ReactNode {
  if (props.transitions.length === 0) {
    return <p className="note">The backend recorded no status change.</p>;
  }
  return (
    <ScrollTable caption="Status changes, oldest first" headers={["When", "From", "To", "By", "Reason"]}>
      {props.transitions.map((transition, index) => (
        <tr key={`${transition.occurred_at}-${String(index)}`}>
          <td>
            <time dateTime={transition.occurred_at}>{formatInstant(transition.occurred_at)}</time>
          </td>
          <td>
            <code className="mono-small">{transition.from_status}</code>
          </td>
          <td>
            <code className="mono-small">{transition.to_status}</code>
          </td>
          <td>
            <ActorText actor={transition.actor} />
          </td>
          <td>{transition.reason}</td>
        </tr>
      ))}
    </ScrollTable>
  );
}

/** The one decision recorded on a swap or a withdrawal: its owner's, or the policy's for an automatic approval. */
export function ApprovalFields(props: { readonly approval: DelegationApproval | undefined }): ReactNode {
  const { approval } = props;
  if (approval === undefined) {
    return <p className="note">No decision is recorded on this request.</p>;
  }
  return (
    <FieldGrid columns={3}>
      <Field label="Decision">
        <Pill tone={approval.decision === "APPROVE" ? "good" : "neutral"}>{approval.decision}</Pill>
      </Field>
      <Field label="Decided by">
        <ActorText actor={approval.approver} />
      </Field>
      <Field label="Decided at" note={exactInstant(approval.decided_at)}>
        {formatInstant(approval.decided_at)}
      </Field>
      <Field label="Plan hash the decision binds" note="A decision is bound to the exact plan it was made on.">
        <Identifier value={approval.plan_hash} />
      </Field>
      <Field label="Strong sign-in at" note="When the decision was a person's, the backend records their latest strong sign-in.">
        {approval.authenticated_at === undefined ? <span className="absent">not a person&apos;s decision</span> : formatInstant(approval.authenticated_at)}
      </Field>
      <Field label="Reason">{approval.reason}</Field>
    </FieldGrid>
  );
}

/** The signing provider's one decision: the inspection of the compiled transaction against the plan. */
export function InspectionFields(props: { readonly inspection: DelegationInspection | undefined }): ReactNode {
  const { inspection } = props;
  if (inspection === undefined) {
    return (
      <p className="note" data-inspection="none">
        Not inspected yet. The signing provider inspects the compiled transaction field by field against the plan only
        when the request is executed, and signs only if it is exactly the plan.
      </p>
    );
  }
  return (
    <FieldGrid columns={3}>
      <Field label="Verdict" note="APPROVED means the transaction was proved to be the plan and was signed.">
        <span data-field="inspection-verdict">
          <Pill tone={verdictTone(inspection.verdict)}>{inspection.verdict}</Pill>
        </span>
      </Field>
      <Field label="Reason codes">
        <Codes codes={inspection.reason_codes} none="none: every check passed" />
      </Field>
      <Field label="Decided at" note={exactInstant(inspection.decided_at)}>
        {formatInstant(inspection.decided_at)}
      </Field>
      <Field label="Inspector">
        <code className="mono-small">{inspection.inspector_version}</code>
      </Field>
      <Field label="Signing policy" note={`hash ${inspection.policy_hash}`}>
        <code className="mono-small">{inspection.policy_version}</code>
      </Field>
      <Field label="Signer" note="The delegated wallet's public key. Its private key never leaves the signing provider.">
        <Identifier value={inspection.signer_public_key} />
      </Field>
      <Field label="Plan hash signed">
        <Identifier value={inspection.plan_hash} />
      </Field>
      <Field label="Capability hash at signing">
        <Identifier value={inspection.capability_hash} />
      </Field>
      <Field label="Message SHA-256">
        <Identifier value={inspection.message_sha256} />
      </Field>
    </FieldGrid>
  );
}

/** What followed a decision: the service's execution, and how it ended when it did not run to its end. */
export function ExecutionOutcome(props: { readonly execution: DelegationExecution; readonly status: string }): ReactNode {
  const { execution } = props;
  if (!execution.attempted) {
    return (
      <p className="callout" role="status" data-execution="none">
        Nothing was executed: the request is now <code className="mono-small">{props.status}</code>.
      </p>
    );
  }
  if (execution.error === undefined) {
    return (
      <p className="callout" role="status" data-execution="ran">
        The service executed it under its own authority. The request is now <code className="mono-small">{props.status}</code>.
      </p>
    );
  }
  return (
    <div className="callout" role="status" data-execution="stopped">
      <p>
        The decision is recorded, and the execution that followed it stopped with{" "}
        <code className="mono-small">{execution.error.code}</code>
        {execution.error.reason === undefined ? "" : <> (<code className="mono-small">{execution.error.reason}</code>)</>}. The
        request is now <code className="mono-small">{props.status}</code>.
      </p>
      <p className="note">{execution.error.detail}</p>
    </div>
  );
}

/**
 * A command that takes only a reason: revoke a capability, remove a
 * destination, cancel a withdrawal. Reviewed, then confirmed.
 */
export function ReasonCommand<T>(props: {
  readonly id: string;
  readonly label: string;
  readonly confirmLabel: string;
  readonly reasonNote: string;
  readonly command: DelegationCommand<ReasonRequest, T>;
  readonly done: (result: T) => ReactNode;
  readonly danger?: boolean;
}): ReactNode {
  const [reason, setReason] = useState("");
  const [reviewing, setReviewing] = useState(false);
  const [attempt, setAttempt] = useState<ConfirmedAttempt<ReasonRequest> | undefined>(undefined);
  const why = checkText(reason, "the reason", 500);
  const variant = props.danger === false ? "primary" : "danger";

  if (props.command.isSuccess) {
    return <>{props.done(props.command.data)}</>;
  }
  const confirm = (): void => {
    if (!why.ok) return;
    const next = confirmAttempt(attempt, { id: props.id, reason: why.value }, newIdempotencyKey);
    setAttempt(next);
    props.command.mutate(next);
  };
  return (
    <form
      className="form"
      aria-label={props.label}
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (why.ok) setReviewing(true);
      }}
    >
      <TextInput
        label="Reason"
        value={reason}
        multiline
        onChange={(value) => {
          setReason(value);
          setReviewing(false);
          props.command.reset();
        }}
        note={props.reasonNote}
        problem={why.ok ? undefined : why.error}
      />
      {!reviewing ? (
        <div className="form-actions">
          {why.ok ? (
            <Button variant={variant} submit>
              {props.label}
            </Button>
          ) : (
            <Button variant={variant} disabledReason={why.error}>
              {props.label}
            </Button>
          )}
        </div>
      ) : (
        <div className="form-actions" role="group" aria-label={`Confirm: ${props.label}`}>
          <Button variant={variant} onClick={confirm} busy={props.command.isPending} busyLabel="Asking the backend…">
            {props.confirmLabel}
          </Button>
          <Button
            variant="quiet"
            onClick={() => {
              setReviewing(false);
            }}
          >
            Not now
          </Button>
        </div>
      )}
      <AttemptKey attempt={attempt} />
      {props.command.isError && <Explanation error={props.command.error} onRetry={confirm} retryLabel="Try again with the same key" />}
    </form>
  );
}

/**
 * The owner's decision on a swap or a withdrawal awaiting one: approve or
 * decline, with a reason, bound to the plan hash on screen. The review step
 * shows that hash; the backend refuses a decision on any other plan.
 */
export function DecisionForm<T>(props: {
  readonly id: string;
  readonly planHash: string;
  readonly what: string;
  readonly approveNote: string;
  readonly command: DelegationCommand<DecisionRequest, T>;
  readonly done: (result: T) => ReactNode;
}): ReactNode {
  const [reason, setReason] = useState("");
  const [choice, setChoice] = useState<Decision | undefined>(undefined);
  const [attempt, setAttempt] = useState<ConfirmedAttempt<DecisionRequest> | undefined>(undefined);
  const why = checkText(reason, "the reason for your decision", 500);

  if (props.command.isSuccess) {
    return <>{props.done(props.command.data)}</>;
  }
  const confirm = (): void => {
    if (choice === undefined || !why.ok) return;
    const next = confirmAttempt(
      attempt,
      { id: props.id, decision: choice, planHash: props.planHash, reason: why.value },
      newIdempotencyKey,
    );
    setAttempt(next);
    props.command.mutate(next);
  };
  const choose = (decision: Decision): void => {
    if (!why.ok) return;
    setChoice(decision);
  };
  return (
    <div className="form" role="group" aria-label={`Decide this ${props.what}`}>
      <TextInput
        label="Reason"
        value={reason}
        multiline
        onChange={(value) => {
          setReason(value);
          setChoice(undefined);
          props.command.reset();
        }}
        note="Recorded with your decision and its audit record."
        problem={why.ok ? undefined : why.error}
      />
      {choice === undefined ? (
        <div className="form-actions">
          {why.ok ? (
            <>
              <Button
                variant="primary"
                onClick={() => {
                  choose("APPROVE");
                }}
              >
                Approve this plan
              </Button>
              <Button
                variant="danger"
                onClick={() => {
                  choose("DECLINE");
                }}
              >
                Decline
              </Button>
            </>
          ) : (
            <Button variant="primary" disabledReason={why.error}>
              Approve this plan
            </Button>
          )}
        </div>
      ) : (
        <div className="callout" role="group" aria-label={`Confirm your decision on this ${props.what}`}>
          <p>
            You are about to <strong>{choice === "APPROVE" ? "approve" : "decline"}</strong> the plan with hash{" "}
            <code className="mono-small" data-plan-hash={props.planHash}>
              {props.planHash}
            </code>
            .{" "}
            {choice === "APPROVE"
              ? props.approveNote
              : "Declining ends the request and returns its reserved funds; nothing is signed."}
          </p>
          <div className="form-actions">
            <Button
              variant={choice === "APPROVE" ? "primary" : "danger"}
              onClick={confirm}
              busy={props.command.isPending}
              busyLabel="Asking the backend…"
            >
              {choice === "APPROVE" ? "Confirm approval" : "Confirm decline"}
            </Button>
            <Button
              variant="quiet"
              onClick={() => {
                setChoice(undefined);
              }}
            >
              Not now
            </Button>
          </div>
        </div>
      )}
      <AttemptKey attempt={attempt} />
      {props.command.isError && <Explanation error={props.command.error} onRetry={confirm} retryLabel="Try again with the same key" />}
    </div>
  );
}
