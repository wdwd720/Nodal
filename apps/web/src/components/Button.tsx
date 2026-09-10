/**
 * The only `<button>` element in the application.
 *
 * Stage 14 forbids dead buttons, so the type makes one impossible to write: a
 * Button either does something (`onClick`, or `submit` inside a form) or it is
 * disabled *and* carries the reason, which is rendered next to it rather than
 * hidden in a tooltip. There is no third shape, and `source-scan.test.ts`
 * refuses a raw `<button>` anywhere else, so nobody can route around it.
 *
 * The design system needs four more shapes of button that `Button` cannot
 * express — an icon, a copy affordance, a sortable column header and a tab —
 * and they live in THIS file rather than in their own, because "the only
 * `<button>` element in the application" is a rule enforced by path. Each one
 * takes its action as a required prop, so none of them can be dead either.
 */
import { useCallback, useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import type { SortState } from "../lib/table.ts";

export type ButtonVariant = "primary" | "secondary" | "quiet" | "danger";

interface Common {
  readonly children: ReactNode;
  readonly variant?: ButtonVariant;
  /** Shown in place of the label while the action is in flight. */
  readonly busy?: boolean;
  readonly busyLabel?: string;
  readonly fullWidth?: boolean;
}

type Behaviour =
  | { readonly onClick: () => void; readonly submit?: never; readonly disabledReason?: never }
  | { readonly submit: true; readonly onClick?: never; readonly disabledReason?: never }
  | { readonly disabledReason: string; readonly onClick?: never; readonly submit?: never };

export type ButtonProps = Common & Behaviour;

export function Button(props: ButtonProps): ReactNode {
  const reasonId = useId();
  const variant = props.variant ?? "secondary";
  const className = `btn btn-${variant}${props.fullWidth === true ? " btn-block" : ""}`;

  if (props.disabledReason !== undefined) {
    return (
      <span className="btn-wrap">
        <button type="button" className={className} disabled aria-describedby={reasonId}>
          {props.children}
        </button>
        <span className="btn-reason" id={reasonId}>
          {props.disabledReason}
        </span>
      </span>
    );
  }

  const busy = props.busy === true;
  return (
    <span className="btn-wrap">
      <button
        type={props.submit === true ? "submit" : "button"}
        className={className}
        aria-busy={busy}
        disabled={busy}
        {...(props.onClick === undefined ? {} : { onClick: props.onClick })}
      >
        {busy ? (props.busyLabel ?? "Working…") : props.children}
      </button>
    </span>
  );
}

/** A navigation styled as a button. Navigating is always a real action. */
export function LinkButton(props: {
  readonly to: string;
  readonly children: ReactNode;
  readonly variant?: ButtonVariant;
}): ReactNode {
  return (
    <Link className={`btn btn-${props.variant ?? "secondary"}`} to={props.to}>
      {props.children}
    </Link>
  );
}

/**
 * A download that the backend serves. It is an anchor rather than a scripted
 * save so the browser performs the transfer itself, with the session cookie.
 */
export function DownloadLink(props: {
  readonly href: string;
  readonly children: ReactNode;
  readonly fileName: string;
}): ReactNode {
  return (
    <a className="btn btn-secondary" href={props.href} download={props.fileName}>
      {props.children}
    </a>
  );
}

/**
 * A small, quiet control: a dialog's close, a toast's dismiss, a menu toggle.
 *
 * `label` is required and becomes the accessible name, because a control whose
 * only label is a glyph has no name at all to anyone navigating by voice or by
 * screen reader.
 */
export function IconButton(props: {
  readonly label: string;
  readonly onClick: () => void;
  readonly children: ReactNode;
  readonly expanded?: boolean;
  readonly controls?: string;
}): ReactNode {
  return (
    <button
      type="button"
      className="btn-icon"
      aria-label={props.label}
      onClick={props.onClick}
      {...(props.expanded === undefined ? {} : { "aria-expanded": props.expanded })}
      {...(props.controls === undefined ? {} : { "aria-controls": props.controls })}
    >
      {props.children}
    </button>
  );
}

/**
 * Click-to-copy for an identifier.
 *
 * A truncated identifier is only honest if the whole value is one gesture away,
 * so every truncation in this system is paired with one of these. The clipboard
 * can refuse — a browser that has not granted permission, a document that is
 * not focused — and a refusal is reported rather than swallowed, because a
 * control that silently does nothing is a dead control.
 */
export function CopyButton(props: {
  readonly value: string;
  /** What is being copied, e.g. "mint address". Becomes the accessible name. */
  readonly what: string;
}): ReactNode {
  const [state, setState] = useState<"idle" | "copied" | "refused">("idle");
  const { value } = props;

  const copy = useCallback(() => {
    const clipboard: Clipboard | undefined = navigator.clipboard;
    if (clipboard === undefined) {
      setState("refused");
      return;
    }
    clipboard.writeText(value).then(
      () => {
        setState("copied");
      },
      () => {
        setState("refused");
      },
    );
  }, [value]);

  return (
    <span className="copy-wrap">
      <IconButton label={`Copy the full ${props.what}`} onClick={copy}>
        <span aria-hidden="true">copy</span>
      </IconButton>
      {state !== "idle" && (
        <span className={state === "copied" ? "copy-ok mono-small" : "mono-small"} role="status">
          {state === "copied" ? " copied" : " the browser refused the clipboard"}
        </span>
      )}
    </span>
  );
}

/**
 * A sortable column header. The glyph is a second channel on top of the
 * `aria-sort` attribute the header cell carries, so the sort is visible to a
 * reader and announced to a screen reader.
 */
export function SortButton(props: {
  readonly onClick: () => void;
  readonly sort: SortState;
  readonly children: ReactNode;
}): ReactNode {
  const glyph = props.sort === "ascending" ? "▲" : props.sort === "descending" ? "▼" : "↕";
  return (
    <button type="button" className="sort-btn" onClick={props.onClick}>
      {props.children}
      <span className="sort-glyph" aria-hidden="true">
        {glyph}
      </span>
    </button>
  );
}

/**
 * One tab in a tablist. Selection is a real action, and the roving tabindex
 * means exactly one tab is in the tab order at a time — arrow keys move between
 * them, which is what a tablist is supposed to do.
 */
export function TabButton(props: {
  readonly id: string;
  readonly controls: string;
  readonly selected: boolean;
  readonly onSelect: () => void;
  readonly onKeyDown: (key: string) => void;
  readonly children: ReactNode;
}): ReactNode {
  return (
    <button
      type="button"
      className="tab"
      role="tab"
      id={props.id}
      aria-controls={props.controls}
      aria-selected={props.selected}
      tabIndex={props.selected ? 0 : -1}
      onClick={props.onSelect}
      onKeyDown={(event) => {
        props.onKeyDown(event.key);
      }}
    >
      {props.children}
    </button>
  );
}
