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
import { Button } from "./Button.tsx";

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
      {detail.retryable && props.onRetry !== undefined && (
        <Button variant="secondary" onClick={props.onRetry}>
          {props.retryLabel ?? "Try again"}
        </Button>
      )}
    </div>
  );
}

/** Says nothing was returned, and why that is not the same as zero. */
export function EmptyState(props: { readonly title: string; readonly body: string }): ReactNode {
  return (
    <div className="empty" role="status">
      <p className="empty-title">{props.title}</p>
      <p className="empty-body">{props.body}</p>
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
  readonly children: (data: T) => ReactNode;
  /** Rendered instead of the children when the data set is empty. */
  readonly empty?: { readonly isEmpty: (data: T) => boolean; readonly title: string; readonly body: string };
  readonly errorExtra?: ReactNode;
}

/** Renders exactly one of the four states, never a blend of two. */
export function AsyncPanel<T>(props: AsyncPanelProps<T>): ReactNode {
  const { query } = props;
  if (query.isPending) {
    return <Loading label={props.loadingLabel} />;
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
