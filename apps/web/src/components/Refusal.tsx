/**
 * A refusal is a first-class state, distinct from an error.
 *
 * This product refuses constantly and by design: capital authority ships
 * disabled, gates are closed by default, eligibility is evaluated before every
 * intent, and the risk kernel is deterministic and unsympathetic. Most products
 * treat a refusal as a fault. Here it is the system working correctly, and it
 * has to look like it.
 *
 * Every refusal states four things, in this order, and the type makes three of
 * them mandatory:
 *
 *   1. WHAT WAS REFUSED, in the customer's words rather than the system's.
 *   2. WHICH RULE REFUSED IT, named, with its stable code.
 *   3. WHAT WOULD CHANGE THE ANSWER — or plainly that nothing the customer can
 *      do will. "Nothing you can do changes this" is a complete answer and a
 *      better one than silence.
 *   4. THE CORRELATION ID, in mono, one click from the clipboard.
 *
 * It uses the warning tone, never the failure tone. Red is for a fault; a
 * refusal is a boundary, and the boundary is a feature the customer is paying
 * for.
 *
 * IT IS NEVER A TOAST. A refusal is not ephemeral — it stays until the customer
 * acts on it. `Toast` refuses to carry one.
 *
 * It also never renders a zero or a dash where the refused figure would have
 * been. A capability that is off is not a balance of nothing.
 */
import type { ReactNode } from "react";

import { IdentifierShort } from "./Identifier.tsx";

export function Refusal(props: {
  /** What was refused, in the customer's words. */
  readonly what: string;
  /** The rule that refused it, named. */
  readonly rule: string;
  /** The backend's stable machine-readable code. */
  readonly code: string;
  /** What would change the answer, or that nothing the customer can do will. */
  readonly remedy: string;
  readonly correlationId?: string;
  /** Anything else worth saying: a link, a related figure, a next step. */
  readonly children?: ReactNode;
}): ReactNode {
  return (
    <div className="refusal" role="status">
      <p className="refusal-title">
        <span aria-hidden="true">△ </span>
        {props.what}
      </p>
      <p className="refusal-body">{props.rule}</p>
      <p className="refusal-remedy">{props.remedy}</p>
      {props.children}
      <p className="refusal-meta mono-small">
        <span>{props.code}</span>
        {props.correlationId !== undefined && props.correlationId !== "" && (
          <IdentifierShort value={props.correlationId} what="correlation id" />
        )}
      </p>
    </div>
  );
}
