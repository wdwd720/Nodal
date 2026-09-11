/**
 * An identifier a customer may have to quote back to somebody.
 *
 * Two forms, and the difference between them is not cosmetic.
 *
 * `Identifier` shows the WHOLE value. It is what a page uses by default and
 * what a confirmation step must use without exception: at the moment of an
 * irreversible action the customer sees all of it, because a truncated address
 * is an address they cannot check.
 *
 * `IdentifierShort` shows first five, ellipsis, last four, for a dense table
 * where the full value would be noise. The whole value stays in `title`, stays
 * in the accessible name, and is one click from the clipboard. A truncation
 * with no way back to the original is a truncation that hides something.
 */
import type { ReactNode } from "react";

import { truncateIdentifier } from "../lib/format.ts";
import { CopyButton } from "./Button.tsx";

function Missing(): ReactNode {
  return <span className="absent">not reported</span>;
}

export function Identifier(props: {
  readonly value: string | undefined;
  readonly label?: string;
  /** Offer a copy control. On by default; off inside a dense cell. */
  readonly copyable?: boolean;
}): ReactNode {
  if (props.value === undefined || props.value === "") {
    return <Missing />;
  }
  return (
    <span className="identifier">
      {props.label !== undefined && <span className="identifier-label">{props.label} </span>}
      <code className="mono-small">{props.value}</code>
      {props.copyable !== false && (
        <CopyButton value={props.value} what={props.label ?? "identifier"} />
      )}
    </span>
  );
}

export function IdentifierShort(props: {
  readonly value: string | undefined;
  /** What this identifies, for the accessible name on the copy control. */
  readonly what: string;
}): ReactNode {
  if (props.value === undefined || props.value === "") {
    return <Missing />;
  }
  return (
    <span className="identifier">
      <code className="mono-small" title={props.value}>
        <span aria-hidden="true">{truncateIdentifier(props.value)}</span>
        <span className="visually-hidden">{props.value}</span>
      </code>
      <CopyButton value={props.value} what={props.what} />
    </span>
  );
}
