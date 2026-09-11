/**
 * A refused order, rendered as a refusal rather than as a fault.
 *
 * UI_UX_SYSTEM §5: this product refuses constantly and by design, and a
 * refusal is the system working. So the ticket does not hand a rejected order
 * to the generic `Explanation` — it names the situation in the customer's
 * words, states which rule refused it, says what would change the answer, and
 * carries the correlation id.
 *
 * The sentences come from `lib/errors.ts`, which is USER_JOURNEY §11's list, so
 * "price changed" reads identically here, on the agents screen and on the
 * withdrawal page. Two things are deliberately NOT taken from that list:
 *
 *   - a session problem. `needsSignIn` and `needsStepUp` are handled by
 *     `Explanation`, which already owns the round trip with a return path, and
 *     duplicating it here would give this product two sign-in buttons that
 *     drift apart.
 *   - `CAPABILITY_NOT_APPROVED` and `UNSUPPORTED`. `situationForCode` maps both
 *     to "withdrawal unavailable", whose sentence is about payout paths — true
 *     where it was written and wrong in front of somebody whose TRADE was
 *     refused. The capability copy from `honesty.ts` is used instead, which
 *     says the same thing without naming the wrong subject.
 *
 * The backend's own `detail` and its structured fields are shown BESIDE the
 * sentence, never instead of it: `would_return` and `min_output` on a moved
 * market are exactly what a customer wants to compare, and they are exact base
 * units as the API sent them.
 */
import type { ReactNode } from "react";

import { explain } from "../../api/problem.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { Explanation } from "../../components/DataState.tsx";
import { Refusal } from "../../components/Refusal.tsx";
import { situationFor, type Recovery, type Situation } from "../../lib/errors.ts";
import { UNAVAILABLE_COPY } from "../../lib/honesty.ts";

/** What the customer can do, in words, for each kind of recovery. */
const REMEDY: Readonly<Record<Recovery["kind"], string>> = {
  retry: "The request itself failed rather than being decided. Sending it again is safe.",
  requote: "Take a new quote and place the order again. Nothing was filled and no Credits moved.",
  "sign-in": "Sign in again to continue. Everything you entered is kept.",
  "step-up": "Confirm it is you, and the order can be sent with the same key.",
  wait: "There is nothing to do here. The backend updates this itself.",
  go: "The page that can change this is linked below.",
  none: "Nothing you can do here changes this answer.",
};

function online(): boolean {
  return typeof navigator === "undefined" ? true : navigator.onLine;
}

function RecoveryAction(props: {
  readonly recovery: Recovery;
  readonly onRetry: (() => void) | undefined;
}): ReactNode {
  const { recovery, onRetry } = props;
  if (recovery.kind === "go" && recovery.to !== undefined) {
    return (
      <div className="form-actions">
        <LinkButton to={recovery.to} variant="secondary">
          {recovery.label}
        </LinkButton>
      </div>
    );
  }
  // A control that cannot change the answer is a dead control, so `wait` and
  // `none` deliberately render nothing at all.
  if ((recovery.kind === "retry" || recovery.kind === "requote") && onRetry !== undefined) {
    return (
      <div className="form-actions">
        <Button variant="secondary" onClick={onRetry}>
          {recovery.label}
        </Button>
      </div>
    );
  }
  return null;
}

/** The capability refusals, which `errors.ts` names after the payout screen. */
const CAPABILITY_SITUATION: Readonly<Record<string, Situation>> = {
  CAPABILITY_NOT_APPROVED: {
    id: "withdrawal-unavailable",
    name: UNAVAILABLE_COPY.capabilityNotApproved.title,
    sentence: UNAVAILABLE_COPY.capabilityNotApproved.body,
    recovery: { kind: "none", label: "" },
    codes: [],
  },
  UNSUPPORTED: {
    id: "withdrawal-unavailable",
    name: UNAVAILABLE_COPY.unsupported.title,
    sentence: UNAVAILABLE_COPY.unsupported.body,
    recovery: { kind: "none", label: "" },
    codes: [],
  },
};

export function TradeRefusal(props: {
  /** What the customer was trying to do, for the heading. */
  readonly what: string;
  readonly error: unknown;
  /** Re-runs the request, for the situations where that can change the answer. */
  readonly onRetry?: () => void;
}): ReactNode {
  const detail = explain(props.error);

  // A session problem is not a refusal by the market, and `Explanation` already
  // owns the round trip that fixes it.
  if (detail.needsSignIn || detail.needsStepUp) {
    return <Explanation error={props.error} />;
  }

  const situation =
    CAPABILITY_SITUATION[detail.code] ?? situationFor({ code: detail.code, online: online() });

  // A code that is not one of the fifteen gets the honest generic explanation
  // rather than a situation invented for it, which would put the wrong recovery
  // in front of somebody holding an unfilled order.
  if (situation === undefined) {
    return <Explanation error={props.error} {...(props.onRetry === undefined ? {} : { onRetry: props.onRetry })} />;
  }

  return (
    <Refusal
      what={`${props.what} was refused: ${situation.name.toLowerCase()}`}
      rule={situation.sentence}
      code={detail.code}
      remedy={REMEDY[situation.recovery.kind]}
      {...(detail.requestId === undefined ? {} : { correlationId: detail.requestId })}
    >
      {detail.body !== situation.sentence && <p className="refusal-body">{detail.body}</p>}
      {detail.fields.length > 0 && (
        <ul className="explain-fields">
          {detail.fields.map(([field, value]) => (
            <li key={field}>
              <span className="mono-small">{field}</span>: <span className="mono-small">{value}</span>
            </li>
          ))}
        </ul>
      )}
      <RecoveryAction recovery={situation.recovery} onRetry={props.onRetry} />
    </Refusal>
  );
}
