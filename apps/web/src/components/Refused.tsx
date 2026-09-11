/**
 * A refusal, turned into the fixed sentence and the one control that changes it.
 *
 * Three things already existed and were never joined up. `lib/errors.ts` holds
 * the fifteen situations `USER_JOURNEY.md` §11 requires, each with its sentence
 * and its recovery. `Refusal` renders what was refused, which rule refused it,
 * the remedy and the correlation id. `explain()` computes `needsSignIn` and
 * `needsStepUp`. Every page that met a refusal wired some of that by hand and a
 * different subset each time, which is how the same rejection came to read
 * three different ways on three screens.
 *
 * This is the join. Give it what the customer was trying to do and the thrown
 * value; it finds the situation, renders the sentence, and renders the recovery
 * the situation declares — and only that one:
 *
 *   retry / requote  a control that re-runs what failed
 *   sign-in          the round trip, keeping the return path
 *   step-up          the same round trip, asking for strong authentication
 *   go               a link to the place that can change the answer
 *   wait / none      NO control at all, because a button that cannot change
 *                    the answer is a dead control with extra steps
 *
 * When the code is not one of the fifteen it falls back to `Explanation`, which
 * says plainly what the backend answered rather than inventing a sentence for
 * it. A refusal this file cannot name is still a refusal, and guessing at its
 * meaning would be worse than quoting it.
 *
 * It is never a toast. A refusal stays until the customer acts on it.
 */
import type { ReactNode } from "react";

import { explain } from "../api/problem.ts";
import { SITUATION_BY_ID, situationFor, type Situation } from "../lib/errors.ts";
import { beginSignIn } from "../session.tsx";
import { Button, LinkButton } from "./Button.tsx";
import { Explanation } from "./DataState.tsx";
import { Refusal } from "./Refusal.tsx";

/** Where the customer is, so the round trip comes back to it. */
function currentPath(): string {
  return `${window.location.pathname}${window.location.search}`;
}

function Recovery(props: {
  readonly situation: Situation;
  readonly onRetry: (() => void) | undefined;
}): ReactNode {
  const { kind, label, to } = props.situation.recovery;

  if (kind === "sign-in" || kind === "step-up") {
    return (
      <Button
        variant="primary"
        onClick={() => {
          beginSignIn({ returnTo: currentPath(), stepUp: kind === "step-up" });
        }}
      >
        {label}
      </Button>
    );
  }

  if (kind === "go" && to !== undefined) {
    return (
      <LinkButton to={to} variant="secondary">
        {label}
      </LinkButton>
    );
  }

  if ((kind === "retry" || kind === "requote") && props.onRetry !== undefined) {
    const retry = props.onRetry;
    return (
      <Button
        variant="secondary"
        onClick={() => {
          retry();
        }}
      >
        {label}
      </Button>
    );
  }

  // `wait`, `none`, and a retry nobody wired: deliberately nothing. The
  // sentence already said what happens next.
  return null;
}

export function Refused(props: {
  /** What the customer was trying to do, in their words. "Buy Credits". */
  readonly what: string;
  readonly error: unknown;
  /** Re-runs the failed request. Rendered only where the situation asks for it. */
  readonly onRetry?: () => void;
  /** Anything else worth saying here: a related figure, a second link. */
  readonly children?: ReactNode;
}): ReactNode {
  const detail = explain(props.error);
  const situation =
    detail.needsStepUp
      ? SITUATION_BY_ID["step-up-required"]
      : situationFor({
          code: detail.code,
          ...(typeof navigator === "undefined" ? {} : { online: navigator.onLine }),
        });

  if (situation === undefined) {
    return (
      <Explanation
        error={props.error}
        {...(props.onRetry === undefined ? {} : { onRetry: props.onRetry })}
      >
        {props.children}
      </Explanation>
    );
  }

  return (
    <Refusal
      what={`${props.what} was refused: ${situation.name.toLowerCase()}`}
      rule={situation.sentence}
      code={detail.code}
      remedy={
        situation.recovery.kind === "wait"
          ? "Nothing needs to be done here. This page updates when the backend reports the outcome."
          : situation.recovery.kind === "none"
            ? "Nothing you can do changes this. It is a decision about the product, not about your account."
            : situation.recovery.label
      }
      {...(detail.requestId === undefined ? {} : { correlationId: detail.requestId })}
    >
      {props.children}
      {/* The backend's own words, beside the sentence rather than instead of
          it: `detail` names the specific rule, and support needs it. */}
      <p className="refusal-body">{detail.body}</p>
      <div className="form-actions">
        <Recovery situation={situation} onRetry={props.onRetry} />
      </div>
    </Refusal>
  );
}
