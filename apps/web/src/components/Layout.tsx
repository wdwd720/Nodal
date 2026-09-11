/**
 * The structural pieces every page reaches for, and the compatibility surface
 * for the pages that already do.
 *
 * Most of what used to live here now lives in a primitive of its own —
 * `Panel`, `AsOf`, `Field`, `Disclosure`, `Identifier`, `StatusBadge` — because
 * each of those grew rules that deserved their own file and their own doc
 * comment. They are re-exported from here unchanged so that sixteen pages did
 * not have to be rewritten to say the same thing a different way.
 *
 * Two things that stayed are load-bearing rather than decorative. `Field` puts
 * a definition list around every figure, so a screen reader reads "buying
 * power, ten thousand dollars" rather than two unrelated strings. `AsOf`
 * carries the backend's own timestamp next to any snapshot, because a figure
 * without a moment attached invites the reader to assume it is current.
 */
import type { ReactNode } from "react";

import { UNAVAILABLE_COPY } from "../lib/honesty.ts";
import { StatusBadge, type Tone } from "./StatusBadge.tsx";

export { AsOf, useStaleness } from "./AsOf.tsx";
export { Disclosure, DisclosureSet } from "./Disclosure.tsx";
export { Field, FieldGrid, FormField } from "./Field.tsx";
export { Identifier, IdentifierShort } from "./Identifier.tsx";
export { Panel, PanelCard } from "./Panel.tsx";
export { StatusBadge } from "./StatusBadge.tsx";
export type { Tone } from "./StatusBadge.tsx";
export type { Temperature } from "./Panel.tsx";

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

/**
 * The older name for `StatusBadge`, kept because a dozen pages use it and
 * because the rule it enforces is the same either way: the tone is a second
 * channel, and the word is the status.
 */
export function Pill(props: {
  readonly tone?: Tone;
  readonly children: ReactNode;
  readonly title?: string;
}): ReactNode {
  return <StatusBadge {...props} />;
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

/**
 * A simple table of rows a page composes itself.
 *
 * `DataTable` is the richer primitive — sortable, keyboard-navigable, with the
 * five states — and is what a rebuilt screen should use. This one stays because
 * it is what the current pages pass `<tr>` children to, and because a table
 * that is genuinely just a list of facts does not need a sort control it will
 * never use. Both scroll inside their own container; neither widens the page.
 */
export function Table(props: {
  readonly caption: string;
  readonly headers: readonly string[];
  readonly children: ReactNode;
}): ReactNode {
  return (
    <div className="table-scroll" role="group" aria-label={props.caption} tabIndex={0}>
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
