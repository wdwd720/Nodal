/**
 * Command forms.
 *
 * Every state-changing operator command in this system requires a reason of at
 * least `authority.min_reason_length` characters, and several also require an
 * evidence reference. The server enforces both; this module enforces them at
 * the point of typing so an operator is not told after the fact.
 *
 * Each submitted command carries one idempotency key, created when the form is
 * built and reused for every retry of that same command (PART 173). The key is
 * not regenerated on a retry: a retried approval must be the same approval, not
 * a second one.
 */
import { actionButton, append, el, notice } from "./dom.ts";
import { newIdempotencyKey } from "./api.ts";

export interface CommandField {
  readonly name: string;
  readonly label: string;
  readonly hint?: string;
  readonly required?: boolean;
  readonly multiline?: boolean;
  readonly minLength?: number;
  /**
   * The DOM hint shown inside an empty control. It is not a default and cannot
   * become one: `values()` reads `control.value`, which is the empty string
   * until someone types, and `firstProblem()` refuses an empty required field
   * before the submit button is enabled. So "the exact record this action
   * names" or "incident id, statement, explorer link" describes what to write;
   * it is never what gets sent. See the note on `Attrs.placeholder` in dom.ts
   * for why this is written down (goal §62).
   */
  readonly placeholder?: string;
  readonly value?: string;
  readonly options?: readonly string[];
}

export interface CommandFormOptions {
  readonly title: string;
  readonly description?: string;
  readonly submitLabel: string;
  readonly fields: readonly CommandField[];
  readonly variant?: "primary" | "danger";
  /** Rendered above the submit control; use it to state consequences. */
  readonly warning?: string;
  readonly onSubmit: (values: Record<string, string>, idempotencyKey: string) => Promise<void>;
}

/**
 * Builds a form whose submit control is disabled until every required field is
 * valid, so an operator never sends a command that was going to be refused for
 * a reason the console already knew.
 */
export function commandForm(opts: CommandFormOptions): HTMLElement {
  const idempotencyKey = newIdempotencyKey();
  const inputs = new Map<string, HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>();
  const errorSlot = el("div", { class: "form-error", "aria-live": "polite" });
  let busy = false;

  const form = el("form", { class: "command-form" });
  append(form, el("h3", {}, opts.title));
  if (opts.description) append(form, el("p", { class: "muted" }, opts.description));

  for (const f of opts.fields) {
    const id = `f-${f.name}-${idempotencyKey.slice(0, 8)}`;
    let control: HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement;
    if (f.options) {
      const select = el("select", { id, name: f.name });
      for (const option of f.options) {
        append(select, el("option", { value: option }, option));
      }
      if (f.value) select.value = f.value;
      control = select;
    } else if (f.multiline) {
      const area = el("textarea", { id, name: f.name, rows: "3", ...(f.placeholder ? { placeholder: f.placeholder } : {}) });
      if (f.value) area.value = f.value;
      control = area;
    } else {
      const input = el("input", { id, name: f.name, type: "text", ...(f.placeholder ? { placeholder: f.placeholder } : {}) });
      if (f.value) input.value = f.value;
      control = input;
    }
    control.addEventListener("input", () => validate());
    control.addEventListener("change", () => validate());
    inputs.set(f.name, control);
    append(
      form,
      el(
        "div",
        { class: "form-field" },
        el("label", { ...({ for: id } as Record<string, string>) }, f.label),
        control,
        f.hint ? el("small", { class: "muted" }, f.hint) : null,
      ),
    );
  }

  if (opts.warning) {
    append(form, notice("warn", opts.warning));
  }

  const submit = el("button", {
    type: "submit",
    class: `button ${opts.variant ?? "primary"}`,
    disabled: true,
  });
  append(submit, opts.submitLabel);
  append(form, errorSlot, el("div", { class: "form-actions" }, submit));

  function values(): Record<string, string> {
    const out: Record<string, string> = {};
    for (const [name, control] of inputs) out[name] = control.value.trim();
    return out;
  }

  function firstProblem(): string | null {
    const v = values();
    for (const f of opts.fields) {
      const value = v[f.name] ?? "";
      if (f.required !== false && value === "") return `${f.label} is required.`;
      if (f.minLength !== undefined && value.length > 0 && value.length < f.minLength) {
        return `${f.label} must be at least ${f.minLength} characters (${value.length} so far).`;
      }
    }
    return null;
  }

  function validate(): void {
    const problem = firstProblem();
    submit.disabled = busy || problem !== null;
    submit.title = problem ?? "";
  }

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    if (busy) return;
    const problem = firstProblem();
    if (problem) return;
    busy = true;
    validate();
    const previous = submit.textContent ?? opts.submitLabel;
    submit.textContent = "Working...";
    void opts
      .onSubmit(values(), idempotencyKey)
      .catch((err: unknown) => {
        errorSlot.replaceChildren(notice("error", err instanceof Error ? err.message : String(err)));
      })
      .finally(() => {
        busy = false;
        submit.textContent = previous;
        validate();
      });
  });

  validate();
  return form;
}

/** A disabled stand-in for a command the principal may not perform. */
export function refusedCommand(title: string, reason: string): HTMLElement {
  return el(
    "div",
    { class: "command-form refused" },
    el("h3", {}, title),
    notice("info", reason),
    actionButton({ label: "Not available to you", onClick: () => undefined, allowed: false, reason }),
  );
}
