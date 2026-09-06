/**
 * The only `<button>` element in the application.
 *
 * Stage 14 forbids dead buttons, so the type makes one impossible to write: a
 * Button either does something (`onClick`, or `submit` inside a form) or it is
 * disabled *and* carries the reason, which is rendered next to it rather than
 * hidden in a tooltip. There is no third shape, and `source-scan.test.ts`
 * refuses a raw `<button>` anywhere else, so nobody can route around it.
 */
import { useId, type ReactNode } from "react";
import { Link } from "react-router-dom";

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
