/**
 * Pieces the prediction market pages share. None of them renders a figure of
 * its own: they name who did something, and they say what happened to a
 * command -- its key, its refusal with the backend's code, or its answer.
 */
import type { ReactNode } from "react";

import { explain } from "../../../api/problem.ts";
import { Button } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import type { Confirmation } from "./confirm.ts";

/** Who recorded something, exactly as the backend named them. */
export function ActorLine(props: { readonly actor: { readonly actor_type: string; readonly actor_id: string } }): ReactNode {
  return (
    <span>
      {props.actor.actor_type} <code className="mono-small">{props.actor.actor_id}</code>
    </span>
  );
}

/** The key a confirmed command carries, shown so a retry is visibly the same action. */
export function KeyLine<C>(props: { readonly confirmation: Confirmation<C> }): ReactNode {
  if (props.confirmation.key === undefined) {
    return (
      <p className="field-note">
        An idempotency key is created when you confirm, and reused if you retry, so a retry can never act twice.
      </p>
    );
  }
  return <p className="mono-small">idempotency key {props.confirmation.key}</p>;
}

/**
 * A command the backend did not carry out: its refusal, with the code and the
 * request id. A retry is offered only when the failure could plausibly pass
 * unchanged (a transport fault, an unavailable provider); it reuses the same
 * key, so a refusal the backend already recorded is replayed, not re-run.
 */
export function CommandFailure(props: {
  readonly error: unknown;
  readonly onRetry: () => void;
  readonly onStartOver: () => void;
  readonly nothingHappened: string;
}): ReactNode {
  const detail = explain(props.error);
  return (
    <Explanation error={props.error}>
      <p>{props.nothingHappened}</p>
      <div className="form-actions">
        {detail.retryable ? (
          <Button onClick={props.onRetry}>Retry with the same key</Button>
        ) : (
          <Button disabledReason="The backend gave a definite answer; the same key would only replay it.">
            Retry with the same key
          </Button>
        )}
        <Button variant="quiet" onClick={props.onStartOver}>
          Start over
        </Button>
      </div>
    </Explanation>
  );
}
