/**
 * One command of the treasury or fees sections: review what will be sent,
 * confirm it, and show the backend's answer.
 *
 * The idempotency key is created when the person confirms and not before;
 * the attempt keeps it, and "Try again" sends the same request with the same
 * key, so a retry can never apply twice — the backend answers it as the
 * request it already saw, and a refusal it recorded is answered the same
 * way. A changed request is a new attempt with a new key.
 *
 * A refusal is shown with the backend's code, its request id and, when the
 * problem carries them, the treasury's stable reason codes and the id of the
 * action that recorded the refusal. Nothing here computes a figure.
 */
import { useState, type ReactNode } from "react";
import type { UseMutationResult } from "@tanstack/react-query";
import { newIdempotencyKey } from "@controlplane/generated-client";

import { Button, type ButtonVariant } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier } from "../../../components/Layout.tsx";
import { confirmAttempt, refusalDetail, type ConfirmedAttempt } from "../../../lib/treasury.ts";
import { Codes } from "../shared.tsx";

/** What a form currently describes: a request it could send, or why it cannot. */
export type Prepared<R> = { readonly ok: true; readonly request: R } | { readonly ok: false; readonly reason: string };

/** Builds a Prepared from a list of checks: the first problem wins. */
export function prepare<R>(problems: ReadonlyArray<string | undefined>, build: () => R): Prepared<R> {
  const problem = problems.find((p) => p !== undefined);
  return problem === undefined ? { ok: true, request: build() } : { ok: false, reason: problem };
}

/** The treasury's stable codes of a refusal, exactly as the problem carried them. */
export function RefusalCodes(props: { readonly error: unknown }): ReactNode {
  const detail = refusalDetail(props.error);
  if (detail === undefined) return null;
  return (
    <FieldGrid columns={2}>
      <Field label="Reason" note="The stable reason the treasury or fee ledger gave.">
        {detail.reason === undefined ? <span className="absent">not stated</span> : <code className="mono-small">{detail.reason}</code>}
      </Field>
      <Field label="Rejection code" note="What the recorded action was refused with.">
        {detail.rejectionCode === undefined ? (
          <span className="absent">not stated</span>
        ) : (
          <code className="mono-small">{detail.rejectionCode}</code>
        )}
      </Field>
      <Field label="Reason codes" note="Every code the decision named.">
        <Codes codes={detail.reasonCodes} none="none named" />
      </Field>
      <Field label="Recorded action" note="The refusal is kept on the record under this action.">
        <Identifier value={detail.actionId} />
      </Field>
    </FieldGrid>
  );
}

export function CommandFlow<R, T>(props: {
  readonly label: string;
  readonly prepared: Prepared<R>;
  readonly describe: (request: R) => ReactNode;
  readonly command: UseMutationResult<T, unknown, ConfirmedAttempt<R>>;
  readonly renderResult: (result: T) => ReactNode;
  /** What a refusal leaves unchanged, said plainly. */
  readonly refusedNote: string;
  readonly variant?: ButtonVariant;
}): ReactNode {
  const [review, setReview] = useState<R | undefined>(undefined);
  const [attempt, setAttempt] = useState<ConfirmedAttempt<R> | undefined>(undefined);
  const { command } = props;
  const sent = attempt !== undefined && (command.isPending || command.isError || command.isSuccess);

  const confirm = (request: R): void => {
    const next = confirmAttempt(attempt, request, newIdempotencyKey);
    setAttempt(next);
    setReview(undefined);
    command.mutate(next);
  };

  const finish = (): void => {
    setAttempt(undefined);
    setReview(undefined);
    command.reset();
  };

  if (sent && attempt !== undefined) {
    return (
      <div className="command">
        <div className="command-request">{props.describe(attempt.request)}</div>
        <p className="mono-small">idempotency key {attempt.idempotencyKey}</p>
        {command.isPending && <p className="loading">Sending to the backend…</p>}
        {command.isError && (
          <>
            <Explanation error={command.error}>
              <p>{props.refusedNote}</p>
              <RefusalCodes error={command.error} />
            </Explanation>
            <div className="form-actions">
              <Button
                onClick={() => {
                  command.mutate(attempt);
                }}
              >
                Try again with the same key
              </Button>
              <Button variant="quiet" onClick={finish}>
                Change the request
              </Button>
            </div>
            <p className="field-note">
              Trying again sends the same request with the same key, so it cannot apply twice; a refusal the backend
              recorded for that key is answered the same way. Changing the request starts a new attempt with a new key.
            </p>
          </>
        )}
        {command.isSuccess && (
          <div role="status" aria-label={`The backend's answer: ${props.label}`}>
            {props.renderResult(command.data)}
            <div className="form-actions">
              <Button variant="quiet" onClick={finish}>
                Done
              </Button>
            </div>
          </div>
        )}
      </div>
    );
  }

  if (review !== undefined) {
    return (
      <div className="command">
        <p className="command-title">Review what will be sent</p>
        <div className="command-request">{props.describe(review)}</div>
        <div className="form-actions">
          <Button
            variant={props.variant ?? "primary"}
            onClick={() => {
              confirm(review);
            }}
          >
            Confirm: {props.label}
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
        <p className="field-note">
          Confirming creates this request's idempotency key; it is kept for any retry of the same request.
        </p>
      </div>
    );
  }

  return (
    <div className="form-actions">
      {props.prepared.ok ? (
        <Button
          variant="secondary"
          onClick={() => {
            if (props.prepared.ok) setReview(props.prepared.request);
          }}
        >
          Review: {props.label}
        </Button>
      ) : (
        <Button variant="secondary" disabledReason={props.prepared.reason}>
          Review: {props.label}
        </Button>
      )}
    </div>
  );
}

/** A text input with its label, for the operator forms. */
export function TextInput(props: {
  readonly id: string;
  readonly label: string;
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly note?: string;
  readonly placeholder?: string;
  readonly inputMode?: "numeric" | "decimal" | "text";
  readonly list?: string;
}): ReactNode {
  return (
    <div className="form-row">
      <label htmlFor={props.id}>{props.label}</label>
      <input
        id={props.id}
        className="input"
        autoComplete="off"
        spellCheck={false}
        value={props.value}
        {...(props.placeholder === undefined ? {} : { placeholder: props.placeholder })}
        {...(props.inputMode === undefined ? {} : { inputMode: props.inputMode })}
        {...(props.list === undefined ? {} : { list: props.list })}
        onChange={(event) => {
          props.onChange(event.target.value);
        }}
      />
      {props.note !== undefined && <p className="field-note">{props.note}</p>}
    </div>
  );
}

/** A multi-line input with its label. */
export function TextArea(props: {
  readonly id: string;
  readonly label: string;
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly note?: string;
  readonly rows?: number;
}): ReactNode {
  return (
    <div className="form-row">
      <label htmlFor={props.id}>{props.label}</label>
      <textarea
        id={props.id}
        className="input"
        rows={props.rows ?? 3}
        spellCheck={false}
        value={props.value}
        onChange={(event) => {
          props.onChange(event.target.value);
        }}
      />
      {props.note !== undefined && <p className="field-note">{props.note}</p>}
    </div>
  );
}

/** A choice among a closed set of values, each sent exactly as the contract spells it. */
export function Choice<V extends string>(props: {
  readonly id: string;
  readonly label: string;
  readonly value: V;
  readonly options: readonly V[];
  readonly onChange: (value: V) => void;
  readonly note?: string;
  readonly describe?: (value: V) => string;
}): ReactNode {
  return (
    <div className="form-row">
      <label htmlFor={props.id}>{props.label}</label>
      <select
        id={props.id}
        className="input"
        value={props.value}
        onChange={(event) => {
          const chosen = props.options.find((option) => option === event.target.value);
          if (chosen !== undefined) props.onChange(chosen);
        }}
      >
        {props.options.map((option) => (
          <option key={option} value={option}>
            {props.describe === undefined ? option : `${option} — ${props.describe(option)}`}
          </option>
        ))}
      </select>
      {props.note !== undefined && <p className="field-note">{props.note}</p>}
    </div>
  );
}

/** A yes-or-no choice. */
export function Toggle(props: {
  readonly id: string;
  readonly label: string;
  readonly checked: boolean;
  readonly onChange: (checked: boolean) => void;
  readonly note?: string;
}): ReactNode {
  return (
    <div className="form-row checkbox">
      <input
        id={props.id}
        type="checkbox"
        checked={props.checked}
        onChange={(event) => {
          props.onChange(event.target.checked);
        }}
      />
      <label htmlFor={props.id}>{props.label}</label>
      {props.note !== undefined && <p className="field-note">{props.note}</p>}
    </div>
  );
}

/** A form whose submission does nothing by itself: its command flow sends. */
export function CommandForm(props: { readonly label: string; readonly children: ReactNode }): ReactNode {
  return (
    <form
      className="form"
      aria-label={props.label}
      onSubmit={(event) => {
        event.preventDefault();
      }}
      noValidate
    >
      {props.children}
    </form>
  );
}
