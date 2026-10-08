/**
 * Pieces the order-book pages share. None of them computes a figure: a price
 * is the decimal string the backend sent, an amount is base units drawn at the
 * decimals of the asset the backend named, and a refusal is drawn with its
 * code — and, when the backend recorded the refused order, a link to it.
 */
import { useState, type ReactNode } from "react";
import { Link, NavLink } from "react-router-dom";
import type { UseMutationResult } from "@tanstack/react-query";
import { ApiProblem, newIdempotencyKey } from "@controlplane/generated-client";

import { explain } from "../../../api/problem.ts";
import { Button } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { Pill, type Tone } from "../../../components/Layout.tsx";
import { DecimalValue, Qty } from "../../../components/Money.tsx";
import { refusalReason, refusedOrderId } from "../../../lib/orders.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref } from "../area.ts";

/** Where the section lives under /advanced. */
export const ORDER_BOOKS_PATH = "order-books";

export function booksHref(...segments: readonly string[]): string {
  return advancedHref(ORDER_BOOKS_PATH, ...segments);
}

/** The section's own pages. */
export function OrderBooksNav(): ReactNode {
  const links: ReadonlyArray<readonly [string, string, boolean]> = [
    [booksHref(), "Books", true],
    [booksHref("orders"), "My orders", false],
    [booksHref("trades"), "My trades", false],
  ];
  return (
    <nav className="subnav" aria-label="Order book pages">
      <ul>
        {links.map(([to, label, end]) => (
          <li key={to}>
            <NavLink
              to={to}
              end={end}
              className={({ isActive }) => (isActive ? "subnav-link subnav-link-active" : "subnav-link")}
            >
              {label}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}

/* --------------------------------------------------------------------------
 * Figures, as sent
 * ------------------------------------------------------------------------ */

/** A price: the exact decimal the backend sent, in the quote asset it named. */
export function PriceOf(props: { readonly value: string | undefined; readonly quote: string | undefined }): ReactNode {
  return (
    <>
      <DecimalValue value={props.value} />
      {props.quote !== undefined && props.value !== undefined && <span className="cell-note">{props.quote} per unit</span>}
    </>
  );
}

/** Base units of an asset, drawn at the decimals the backend sent with them. */
export function Units(props: {
  readonly value: string | undefined;
  readonly decimals: number | undefined;
  readonly symbol: string | undefined;
}): ReactNode {
  return <Qty value={props.value} decimals={props.decimals} {...(props.symbol === undefined ? {} : { symbol: props.symbol })} />;
}

export function bookStatusTone(status: string): Tone {
  return status === "OPEN" ? "good" : status === "HALTED" ? "warn" : "neutral";
}

export function orderStatusTone(status: string): Tone {
  switch (status) {
    case "FILLED":
      return "good";
    case "OPEN":
    case "PARTIALLY_FILLED":
    case "PENDING":
      return "info";
    case "REJECTED":
      return "bad";
    default:
      return "neutral";
  }
}

export function StatusPill(props: { readonly status: string; readonly tone: Tone }): ReactNode {
  return <Pill tone={props.tone}>{props.status}</Pill>;
}

/** When something was recorded: local time, with the exact instant the backend sent. */
export function Recorded(props: { readonly at: string | undefined }): ReactNode {
  return (
    <>
      {formatInstant(props.at)} <span className="mono-small">({exactInstant(props.at)})</span>
    </>
  );
}

/* --------------------------------------------------------------------------
 * Commands: one confirmed request and its idempotency key
 * ------------------------------------------------------------------------ */

export interface Confirmation<C> {
  /** The confirmed command, or undefined before confirmation. */
  readonly command: C | undefined;
  /** Its idempotency key, or undefined before confirmation. */
  readonly key: string | undefined;
  /**
   * The command to send: on the first confirmation a key is created and the
   * command built with it; afterwards the stored command, unchanged, so a
   * retry is the same request under the same key.
   */
  readonly confirm: (build: (key: string) => C) => C;
  /** Forgets the command and its key: the next confirmation is a new action. */
  readonly reset: () => void;
}

export function useConfirmation<C>(): Confirmation<C> {
  const [held, setHeld] = useState<{ readonly key: string; readonly command: C } | undefined>(undefined);
  return {
    command: held?.command,
    key: held?.key,
    confirm: (build) => {
      if (held !== undefined) return held.command;
      const key = newIdempotencyKey();
      const command = build(key);
      setHeld({ key, command });
      return command;
    },
    reset: () => {
      setHeld(undefined);
    },
  };
}

/** The key a confirmed command carries, shown so a retry is visibly the same action. */
export function KeyLine(props: { readonly actionKey: string | undefined }): ReactNode {
  if (props.actionKey === undefined) {
    return (
      <p className="field-note">
        Idempotency key: created when you confirm, and reused if you retry, so a retry can never act twice.
      </p>
    );
  }
  return <p className="mono-small">idempotency key {props.actionKey}</p>;
}

/**
 * A command the backend did not carry out: its refusal with the code and the
 * request id, the order it recorded when it recorded one, and a retry under
 * the same key only when trying again unchanged could plausibly pass.
 */
export function CommandFailure(props: {
  readonly error: unknown;
  readonly onRetry: () => void;
  readonly onStartOver: () => void;
}): ReactNode {
  const detail = explain(props.error);
  const fields = props.error instanceof ApiProblem ? props.error.fields : undefined;
  const orderId = refusedOrderId(fields);
  const reason = refusalReason(fields);
  return (
    <Explanation error={props.error}>
      {reason !== undefined && (
        <p>
          The book&apos;s reason: <code className="mono-small">{reason}</code>
        </p>
      )}
      {orderId !== undefined && (
        <p>
          The backend recorded the refused order, which stays readable:{" "}
          <Link to={booksHref("orders", orderId)}>
            <code className="mono-small">{orderId}</code>
          </Link>
          .
        </p>
      )}
      <div className="form-actions">
        {detail.retryable ? (
          <Button onClick={props.onRetry}>Retry with the same key</Button>
        ) : (
          <Button disabledReason="The backend gave a definite answer; the same key would only replay it.">
            Retry with the same key
          </Button>
        )}
        <Button variant="quiet" onClick={props.onStartOver}>
          Start over
        </Button>
      </div>
    </Explanation>
  );
}

/**
 * The confirm button of a command, its key, and what became of it. The
 * command is built once, when the person confirms; a retry resends it
 * unchanged. Changing any input must call `startOver`, so a different request
 * never travels under a key the backend has already seen.
 */
export function CommandControls<C, R>(props: {
  readonly confirmLabel: string;
  readonly busyLabel: string;
  /** Why the command cannot be sent as filled in; the button says so instead of sending. */
  readonly problem: string | undefined;
  readonly build: (key: string) => C;
  readonly mutation: UseMutationResult<R, unknown, C>;
  readonly confirmation: Confirmation<C>;
  readonly onStartOver: () => void;
  readonly renderResult: (result: R) => ReactNode;
  readonly variant?: "primary" | "danger";
  /** After an answer: how to begin another action of the same kind. */
  readonly againLabel?: string;
  /** How a refusal is drawn; the order-book refusal (with the order it recorded) by default. */
  readonly renderFailure?: (error: unknown, onRetry: () => void, onStartOver: () => void) => ReactNode;
}): ReactNode {
  const { mutation, confirmation } = props;
  const variant = props.variant ?? "primary";
  const answered = mutation.isSuccess;
  const send = (): void => {
    if (props.problem !== undefined) return;
    mutation.mutate(confirmation.confirm(props.build));
  };
  const retry = (): void => {
    if (confirmation.command !== undefined) mutation.mutate(confirmation.command);
  };
  return (
    <>
      <KeyLine actionKey={confirmation.key} />
      <div className="form-actions">
        {answered ? (
          <Button variant={variant} disabledReason="The backend answered this action; start a new one to send another.">
            {props.confirmLabel}
          </Button>
        ) : props.problem !== undefined ? (
          <Button variant={variant} disabledReason={props.problem}>
            {props.confirmLabel}
          </Button>
        ) : (
          <Button variant={variant} onClick={send} busy={mutation.isPending} busyLabel={props.busyLabel}>
            {props.confirmLabel}
          </Button>
        )}
        {answered && (
          <Button variant="quiet" onClick={props.onStartOver}>
            {props.againLabel ?? "Start a new one"}
          </Button>
        )}
      </div>
      {mutation.isError &&
        (props.renderFailure === undefined ? (
          <CommandFailure error={mutation.error} onRetry={retry} onStartOver={props.onStartOver} />
        ) : (
          props.renderFailure(mutation.error, retry, props.onStartOver)
        ))}
      {mutation.isSuccess && props.renderResult(mutation.data)}
    </>
  );
}

/* --------------------------------------------------------------------------
 * Inputs
 * ------------------------------------------------------------------------ */

/** A labelled text input with its problem, if any, under it. */
export function TextField(props: {
  readonly id: string;
  readonly label: string;
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly problem?: string | undefined;
  readonly hint?: string;
  readonly multiline?: boolean;
}): ReactNode {
  const problemId = `${props.id}-problem`;
  const showProblem = props.problem !== undefined && props.problem !== "" && props.value !== "";
  const common = {
    id: props.id,
    className: "input",
    value: props.value,
    autoComplete: "off",
    spellCheck: false,
    "aria-invalid": showProblem,
    ...(showProblem ? { "aria-describedby": problemId } : {}),
  };
  return (
    <div className="form-row">
      <label htmlFor={props.id}>{props.label}</label>
      {props.multiline === true ? (
        <textarea
          {...common}
          rows={6}
          onChange={(event) => {
            props.onChange(event.target.value);
          }}
        />
      ) : (
        <input
          {...common}
          onChange={(event) => {
            props.onChange(event.target.value);
          }}
        />
      )}
      {props.hint !== undefined && <span className="field-note">{props.hint}</span>}
      {showProblem && (
        <span className="field-error" id={problemId} role="alert">
          {props.problem}
        </span>
      )}
    </div>
  );
}

/** A labelled select of fixed choices. */
export function ChoiceField<T extends string>(props: {
  readonly id: string;
  readonly label: string;
  readonly value: T;
  readonly choices: readonly T[];
  readonly onChange: (value: T) => void;
  readonly describe?: (value: T) => string;
  readonly hint?: string;
}): ReactNode {
  return (
    <div className="form-row">
      <label htmlFor={props.id}>{props.label}</label>
      <select
        id={props.id}
        className="input"
        value={props.value}
        onChange={(event) => {
          const next = props.choices.find((choice) => choice === event.target.value);
          if (next !== undefined) props.onChange(next);
        }}
      >
        {props.choices.map((choice) => (
          <option key={choice} value={choice}>
            {props.describe === undefined ? choice : props.describe(choice)}
          </option>
        ))}
      </select>
      {props.hint !== undefined && <span className="field-note">{props.hint}</span>}
    </div>
  );
}

/** A labelled checkbox. */
export function CheckField(props: {
  readonly id: string;
  readonly label: string;
  readonly checked: boolean;
  readonly onChange: (checked: boolean) => void;
  readonly hint?: string;
}): ReactNode {
  return (
    <div className="form-row">
      <label htmlFor={props.id}>
        <input
          id={props.id}
          type="checkbox"
          checked={props.checked}
          onChange={(event) => {
            props.onChange(event.target.checked);
          }}
        />{" "}
        {props.label}
      </label>
      {props.hint !== undefined && <span className="field-note">{props.hint}</span>}
    </div>
  );
}
