/**
 * Pieces the derivatives pages share. None of them computes a figure: a
 * price is a mantissa the backend sent, drawn at the scale it sent with it,
 * and an amount is base units drawn at the decimals of the asset the backend
 * named. A refusal is drawn with its code.
 */
import { useRef, useState, type ReactNode } from "react";
import { Link, NavLink } from "react-router-dom";
import { ApiProblem, newIdempotencyKey } from "@controlplane/generated-client";

import type { DerivativeAsset, DerivativeMark } from "../../../api/advanced/derivatives.ts";
import { Button } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid } from "../../../components/Layout.tsx";
import { Qty } from "../../../components/Money.tsx";
import { refusedIntentId } from "../../../lib/derivatives.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref, useAdvancedAccount } from "../area.ts";

/** Where the section lives under /advanced. */
export const DERIVATIVES_PATH = "derivatives";

export function derivativesHref(...segments: readonly string[]): string {
  return advancedHref(DERIVATIVES_PATH, ...segments);
}

/** The section's own pages. The clearing house is an operator's page. */
export function DerivativesNav(): ReactNode {
  const { canInspect } = useAdvancedAccount();
  const links: ReadonlyArray<readonly [string, string, boolean]> = [
    [derivativesHref(), "Contracts", true],
    [derivativesHref("positions"), "Positions", false],
    [derivativesHref("flows"), "Flows and funding", false],
    ...(canInspect ? ([[derivativesHref("clearing-house"), "Clearing house", false]] as const) : []),
  ];
  return (
    <nav className="subnav" aria-label="Derivatives pages">
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

/** A price: the mantissa the backend sent, at the scale it sent with it. */
export function Price(props: { readonly mantissa: string | undefined; readonly scale: number | undefined }): ReactNode {
  return <Qty value={props.mantissa} decimals={props.scale} />;
}

/** An amount in base units of the asset the backend named, as the decimal it denotes. */
export function Amount(props: { readonly value: string | undefined; readonly asset: DerivativeAsset }): ReactNode {
  return <Qty value={props.value} decimals={props.asset.decimals} symbol={props.asset.symbol} />;
}

/** A mark, its index and when it was observed, as recorded. */
export function MarkFields(props: { readonly mark: DerivativeMark }): ReactNode {
  const { mark } = props;
  return (
    <FieldGrid columns={3}>
      <Field label="Mark" note="The contract's mark at its price scale, on its tick grid.">
        <Price mantissa={mark.mark_mantissa} scale={mark.price_scale} />
      </Field>
      <Field label="Index" note="The underlying's spot the mark was recorded with. Funding and options need it.">
        {mark.has_index ? (
          <>
            <Price mantissa={mark.index_mantissa} scale={mark.index_scale} />
            {mark.index_source !== undefined && <span className="cell-note">{mark.index_source}</span>}
          </>
        ) : (
          <span className="absent">recorded without an index</span>
        )}
      </Field>
      <Field label="Observed">
        {formatInstant(mark.observed_at)}
        <span className="cell-note">
          {exactInstant(mark.observed_at)} · source {mark.source}
        </span>
      </Field>
    </FieldGrid>
  );
}

/** A refusal the backend gave in place of a figure: its code and its words. */
export function RefusalNote(props: { readonly code: string; readonly detail: string; readonly what: string }): ReactNode {
  return (
    <div className="explain explain-flat" role="status">
      <p className="explain-title">{props.what}</p>
      <p className="explain-body">{props.detail}</p>
      <p className="explain-meta mono-small">code {props.code}</p>
    </div>
  );
}

/**
 * A command's failure. A refusal the backend recorded names the intent it
 * recorded; that intent stays readable, so the page links to it.
 */
export function CommandFailure(props: { readonly error: unknown; readonly children?: ReactNode }): ReactNode {
  const intentId = props.error instanceof ApiProblem ? refusedIntentId(props.error.fields) : undefined;
  return (
    <Explanation error={props.error}>
      {props.children}
      {intentId !== undefined && (
        <p>
          The backend recorded this refusal as a rejected intent:{" "}
          <Link to={advancedHref("activity", "intents", intentId)}>
            <code className="mono-small">{intentId}</code>
          </Link>
          .
        </p>
      )}
    </Explanation>
  );
}

/**
 * The idempotency key of one action. It is created when the person confirms,
 * reused for every retry of that same action, and dropped when they change
 * the action or start another.
 */
export function useActionKey(): {
  readonly key: string | undefined;
  readonly confirm: () => string;
  readonly reset: () => void;
} {
  // The ref is the key itself, set synchronously, so two clicks that land
  // before a re-render still carry one key; the state only redraws it.
  const current = useRef<string | undefined>(undefined);
  const [key, setKey] = useState<string | undefined>(undefined);
  return {
    key,
    confirm: () => {
      if (current.current !== undefined) return current.current;
      const created = newIdempotencyKey();
      current.current = created;
      setKey(created);
      return created;
    },
    reset: () => {
      current.current = undefined;
      setKey(undefined);
    },
  };
}

/** The key an action carries, once it has one. */
export function ActionKey(props: { readonly actionKey: string | undefined }): ReactNode {
  return (
    <Field label="Idempotency key" note="Created when you confirm; reused on every retry, so the action happens at most once.">
      {props.actionKey === undefined ? (
        <span className="absent">created when you confirm</span>
      ) : (
        <code className="mono-small">{props.actionKey}</code>
      )}
    </Field>
  );
}

/** The confirm button of a command, or why it cannot be confirmed yet. */
export function ConfirmButton(props: {
  readonly blockedReason: string | undefined;
  readonly busy: boolean;
  readonly busyLabel: string;
  readonly onConfirm: () => void;
  readonly children: ReactNode;
  readonly variant?: "primary" | "danger";
}): ReactNode {
  if (props.blockedReason !== undefined) {
    return (
      <Button variant={props.variant ?? "primary"} disabledReason={props.blockedReason}>
        {props.children}
      </Button>
    );
  }
  return (
    <Button variant={props.variant ?? "primary"} onClick={props.onConfirm} busy={props.busy} busyLabel={props.busyLabel}>
      {props.children}
    </Button>
  );
}

/** A labelled text input with its problem, if any, under it. */
export function TextField(props: {
  readonly id: string;
  readonly label: string;
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly problem?: string | undefined;
  readonly hint?: string;
  readonly placeholder?: string;
}): ReactNode {
  const problemId = `${props.id}-problem`;
  const showProblem = props.problem !== undefined && props.problem !== "" && props.value !== "";
  return (
    <div className="form-row">
      <label htmlFor={props.id}>{props.label}</label>
      <input
        id={props.id}
        className="input"
        value={props.value}
        autoComplete="off"
        spellCheck={false}
        {...(props.placeholder === undefined ? {} : { placeholder: props.placeholder })}
        aria-invalid={showProblem}
        {...(showProblem ? { "aria-describedby": problemId } : {})}
        onChange={(event) => {
          props.onChange(event.target.value);
        }}
      />
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
    </div>
  );
}
