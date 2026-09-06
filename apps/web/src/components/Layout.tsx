/**
 * Structural pieces shared by every page.
 *
 * Two things here are load-bearing rather than decorative. `Field` puts a
 * definition list around every figure, so a screen reader reads "buying power,
 * ten thousand dollars" rather than two unrelated strings. `AsOf` carries the
 * backend's own timestamp next to any snapshot, because a figure without a
 * moment attached invites the reader to assume it is current (PART 110).
 */
import type { ReactNode } from "react";

import { exactInstant, formatInstant } from "../lib/time.ts";
import { UNAVAILABLE_COPY } from "../lib/honesty.ts";

export function Page(props: {
  readonly title: string;
  readonly lead?: string;
  readonly actions?: ReactNode;
  readonly children: ReactNode;
}): ReactNode {
  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>{props.title}</h1>
          {props.lead !== undefined && <p className="lead">{props.lead}</p>}
        </div>
        {props.actions !== undefined && <div className="page-actions">{props.actions}</div>}
      </header>
      {props.children}
    </div>
  );
}

export function Panel(props: {
  readonly title: string;
  readonly description?: string;
  readonly actions?: ReactNode;
  readonly children: ReactNode;
  readonly id?: string;
}): ReactNode {
  return (
    <section className="panel" {...(props.id === undefined ? {} : { id: props.id })} aria-label={props.title}>
      <div className="panel-head">
        <div>
          <h2>{props.title}</h2>
          {props.description !== undefined && <p className="panel-description">{props.description}</p>}
        </div>
        {props.actions !== undefined && <div className="panel-actions">{props.actions}</div>}
      </div>
      <div className="panel-body">{props.children}</div>
    </section>
  );
}

/** A labelled figure. `note` explains what the figure actually means. */
export function Field(props: {
  readonly label: string;
  readonly note?: string;
  readonly children: ReactNode;
  readonly emphasis?: boolean;
}): ReactNode {
  return (
    <div className={props.emphasis === true ? "field field-emphasis" : "field"}>
      <dt>{props.label}</dt>
      <dd>
        {props.children}
        {/* The note lives inside the <dd>: a <dl> may only contain dt/dd pairs,
            optionally wrapped in a <div>, so a sibling <p> here would be
            invalid markup and an axe 'definition-list' violation. */}
        {props.note !== undefined && <p className="field-note">{props.note}</p>}
      </dd>
    </div>
  );
}

export function FieldGrid(props: { readonly children: ReactNode; readonly columns?: 2 | 3 | 4 }): ReactNode {
  return <dl className={`field-grid cols-${String(props.columns ?? 3)}`}>{props.children}</dl>;
}

/** The backend's own snapshot instant, with the exact value available. */
export function AsOf(props: { readonly at: string | undefined; readonly label?: string }): ReactNode {
  return (
    <p className="as-of">
      {props.label ?? "As of"} <time dateTime={props.at ?? ""}>{formatInstant(props.at)}</time>{" "}
      <span className="mono-small">({exactInstant(props.at)})</span>
    </p>
  );
}

export type Tone = "neutral" | "good" | "warn" | "bad" | "info";

export function Pill(props: { readonly tone?: Tone; readonly children: ReactNode; readonly title?: string }): ReactNode {
  return (
    <span
      className={`pill pill-${props.tone ?? "neutral"}`}
      {...(props.title === undefined ? {} : { title: props.title })}
    >
      {props.children}
    </span>
  );
}

/** A standing disclosure. Always visible; never behind a disclosure triangle. */
export function Disclosure(props: { readonly title: string; readonly children: ReactNode }): ReactNode {
  return (
    <aside className="disclosure" aria-label={props.title}>
      <p className="disclosure-title">{props.title}</p>
      <div className="disclosure-body">{props.children}</div>
    </aside>
  );
}

/**
 * Says plainly that v1 exposes no endpoint behind a screen the goal document
 * requires. Drawing the screen from invented numbers would be worse than
 * saying this, so this is what the screen says.
 */
export function NoEndpoint(props: {
  readonly what: string;
  readonly detail: string;
  readonly children?: ReactNode;
}): ReactNode {
  return (
    <div className="explain explain-flat" role="status">
      <p className="explain-title">{UNAVAILABLE_COPY.noApi.title}</p>
      <p className="explain-body">
        {props.what} {UNAVAILABLE_COPY.noApi.body}
      </p>
      <p className="explain-body">{props.detail}</p>
      {props.children}
    </div>
  );
}

export function Table(props: {
  readonly caption: string;
  readonly headers: readonly string[];
  readonly children: ReactNode;
}): ReactNode {
  return (
    <div className="table-scroll">
      <table>
        <caption className="visually-hidden">{props.caption}</caption>
        <thead>
          <tr>
            {props.headers.map((header) => (
              <th key={header} scope="col">
                {header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{props.children}</tbody>
      </table>
    </div>
  );
}

/** A copyable identifier. Long ids are never truncated without the full value present. */
export function Identifier(props: { readonly value: string | undefined; readonly label?: string }): ReactNode {
  if (props.value === undefined || props.value === "") {
    return <span className="absent">not reported</span>;
  }
  return (
    <span className="identifier">
      {props.label !== undefined && <span className="identifier-label">{props.label} </span>}
      <code className="mono-small">{props.value}</code>
    </span>
  );
}
