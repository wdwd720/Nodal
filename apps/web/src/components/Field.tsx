/**
 * Two things called a field, because the product has two.
 *
 * `Field` is a READ-OUT: a labelled figure, always a `<dt>`/`<dd>` pair inside a
 * `<dl>`, so a screen reader says "buying power, twelve thousand four hundred
 * and eighty dollars" rather than two unrelated strings. Every figure in this
 * application is inside one of these or inside a scoped table cell; there is no
 * third place a number is allowed to be.
 *
 * `FormField` is an INPUT: a label, an optional hint and an optional error,
 * wired to the control with `aria-describedby` and `aria-invalid` so the
 * explanation is announced with the field rather than sitting near it. It is
 * named `FormField` rather than `Field` because both are needed and a name
 * collision between "the figure" and "the input" is the kind of ambiguity that
 * ends with a figure rendered in a text box.
 */
import { useId, type ReactNode } from "react";

export function FieldGrid(props: {
  readonly children: ReactNode;
  readonly columns?: 2 | 3 | 4;
}): ReactNode {
  return <dl className={`field-grid cols-${String(props.columns ?? 3)}`}>{props.children}</dl>;
}

/** A labelled figure. `note` says what the figure actually means. */
export function Field(props: {
  readonly label: string;
  readonly note?: string;
  readonly children: ReactNode;
  /** The one big figure on the screen. At most one per page. */
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

/**
 * A labelled control.
 *
 * `children` is a render prop rather than an element, because the control needs
 * the generated ids and there is no honest way to inject them into an arbitrary
 * child. The caller spreads what it is given onto its input:
 *
 *     <FormField label="Amount" hint="US dollars" error={parsed.error}>
 *       {(field) => <input className="input" value={v} {...field} />}
 *     </FormField>
 *
 * An error is a `role="alert"` so it is announced when it appears, and it is
 * always rendered as text beside the control — never as a red border alone,
 * which says nothing to a reader who cannot see the red.
 */
export interface FieldControlProps {
  readonly id: string;
  readonly "aria-describedby": string | undefined;
  readonly "aria-invalid": boolean | undefined;
}

export function FormField(props: {
  readonly label: string;
  readonly hint?: string;
  readonly error?: string;
  readonly children: (field: FieldControlProps) => ReactNode;
}): ReactNode {
  const base = useId();
  const id = `${base}-control`;
  const hintId = `${base}-hint`;
  const errorId = `${base}-error`;
  const hasHint = props.hint !== undefined && props.hint !== "";
  const hasError = props.error !== undefined && props.error !== "";

  const described = [hasHint ? hintId : "", hasError ? errorId : ""]
    .filter((part) => part !== "")
    .join(" ");

  return (
    <div className="inline-field">
      <label className="field-label" htmlFor={id}>
        {props.label}
      </label>
      {props.children({
        id,
        "aria-describedby": described === "" ? undefined : described,
        "aria-invalid": hasError ? true : undefined,
      })}
      {hasHint && (
        <p className="field-hint" id={hintId}>
          {props.hint}
        </p>
      )}
      {hasError && (
        <p className="field-error" id={errorId} role="alert">
          {props.error}
        </p>
      )}
    </div>
  );
}
