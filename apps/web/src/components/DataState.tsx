/**
 * The four honest states of any panel: loading, refused, empty, present.
 *
 * The rule that shapes this file is "no fake state". A panel that is loading
 * says so; a panel the backend refused shows the refusal, its stable code and
 * the request id; a panel with genuinely nothing in it says the backend
 * returned no rows. None of those look like a figure.
 */
import type { ReactNode } from "react";

import { explain } from "../api/problem.ts";
import { SITUATION_BY_ID } from "../lib/errors.ts";
import { beginSignIn } from "../session.tsx";
import { Button } from "./Button.tsx";

/**
 * Where the customer is, for the round trip back.
 *
 * Read from `window.location` rather than from the router so that this
 * component keeps working wherever it is rendered — including the boot screen,
 * which runs before any page has decided what it is.
 */
function currentPath(): string {
  return `${window.location.pathname}${window.location.search}`;
}

/**
 * The recovery for an error that is really a session problem.
 *
 * `explain()` has computed `needsSignIn` and `needsStepUp` since the API layer
 * was written, and until now nothing consumed them: a 401 mid-action produced a
 * sentence and no way out of it. `USER_JOURNEY.md` §10 requires the way out —
 * the round trip keeps the return path, and `useSurvivesSignIn` keeps whatever
 * the customer had typed, so coming back lands on the same page with the same
 * form.
 */
function SessionRecovery(props: {
  readonly needsSignIn: boolean;
  readonly needsStepUp: boolean;
}): ReactNode {
  if (!props.needsSignIn && !props.needsStepUp) return null;
  const situation = props.needsStepUp
    ? SITUATION_BY_ID["step-up-required"]
    : SITUATION_BY_ID["session-expired"];
  return (
    <>
      <p className="explain-body">{situation.sentence}</p>
      <Button
        variant="primary"
        onClick={() => {
          beginSignIn({ returnTo: currentPath(), stepUp: props.needsStepUp });
        }}
      >
        {situation.recovery.label}
      </Button>
    </>
  );
}

export function Explanation(props: {
  readonly error: unknown;
  readonly onRetry?: () => void;
  readonly retryLabel?: string;
  /** Extra guidance specific to the page this appeared on. */
  readonly children?: ReactNode;
}): ReactNode {
  const detail = explain(props.error);
  return (
    <div className="explain" role="status">
      <p className="explain-title">{detail.title}</p>
      <p className="explain-body">{detail.body}</p>
      {props.children}
      {detail.fields.length > 0 && (
        <ul className="explain-fields">
          {detail.fields.map(([field, message]) => (
            <li key={field}>
              <span className="mono-small">{field}</span>: {message}
            </li>
          ))}
        </ul>
      )}
      <p className="explain-meta mono-small">
        code {detail.code}
        {detail.status === undefined ? "" : ` · HTTP ${String(detail.status)}`}
        {detail.requestId === undefined ? "" : ` · request ${detail.requestId}`}
      </p>
      <SessionRecovery needsSignIn={detail.needsSignIn} needsStepUp={detail.needsStepUp} />
      {detail.retryable && props.onRetry !== undefined && (
        <Button variant="secondary" onClick={props.onRetry}>
          {props.retryLabel ?? "Try again"}
        </Button>
      )}
    </div>
  );
}

/**
 * Says nothing was returned, and why that is not the same as zero.
 *
 * It says what WOULD be here and, where there is one, how to cause it. Never an
 * illustration with the word "Nothing" under it: an empty table is a fact about
 * the account, and a fact deserves a sentence.
 */
export function EmptyState(props: {
  readonly title: string;
  readonly body: string;
  /** How to cause the thing that is missing, when there is a way. */
  readonly action?: ReactNode;
}): ReactNode {
  return (
    <div className="empty" role="status">
      <p className="empty-title">{props.title}</p>
      <p className="empty-body">{props.body}</p>
      {props.action !== undefined && <div className="form-actions">{props.action}</div>}
    </div>
  );
}

export function Loading(props: { readonly label: string }): ReactNode {
  return (
    <p className="loading" role="status" aria-live="polite">
      {props.label}
    </p>
  );
}

export interface AsyncPanelProps<T> {
  readonly query: {
    readonly isPending: boolean;
    readonly isError: boolean;
    readonly error: unknown;
    readonly data: T | undefined;
    readonly refetch: () => unknown;
  };
  readonly loadingLabel: string;
  /**
   * A shape-accurate skeleton for the region, shown instead of the one-line
   * loading label. A skeleton is honest in a way a stale number is not, and it
   * stops the layout jumping when the data lands.
   */
  readonly skeleton?: ReactNode;
  readonly children: (data: T) => ReactNode;
  /** Rendered instead of the children when the data set is empty. */
  readonly empty?: { readonly isEmpty: (data: T) => boolean; readonly title: string; readonly body: string };
  readonly errorExtra?: ReactNode;
}

/** Renders exactly one of the four states, never a blend of two. */
export function AsyncPanel<T>(props: AsyncPanelProps<T>): ReactNode {
  const { query } = props;
  if (query.isPending) {
    return props.skeleton ?? <Loading label={props.loadingLabel} />;
  }
  if (query.isError) {
    return (
      <Explanation
        error={query.error}
        onRetry={() => {
          void query.refetch();
        }}
      >
        {props.errorExtra}
      </Explanation>
    );
  }
  if (query.data === undefined) {
    return (
      <EmptyState
        title="Nothing came back"
        body="The request succeeded but carried no body, so there is nothing to show."
      />
    );
  }
  if (props.empty !== undefined && props.empty.isEmpty(query.data)) {
    return <EmptyState title={props.empty.title} body={props.empty.body} />;
  }
  return props.children(query.data);
}
