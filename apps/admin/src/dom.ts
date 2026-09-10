/**
 * A very small rendering layer. No framework, no virtual DOM, no dependency.
 *
 * The one rule that matters here: `text()` sets `textContent`, never
 * `innerHTML`. Reasons, evidence references, provider error strings and
 * account status reasons are all operator- or provider-supplied text that
 * reaches this console verbatim, and an operator console that interpolates
 * such a string into markup is an operator console with stored XSS in it.
 * There is deliberately no helper in this file that accepts HTML.
 */

export type Child = Node | string | null | undefined | false;

interface Attrs {
  class?: string;
  title?: string;
  id?: string;
  href?: string;
  type?: string;
  value?: string;
  name?: string;
  /**
   * The DOM `placeholder` attribute: the grey hint that shows inside an empty
   * input and disappears the moment anything is typed.
   *
   * Recorded here because a repository-wide search for the word "placeholder"
   * (goal §62) finds this line and every use of it, and every one of those is
   * this attribute. Nothing unfinished hides behind any of them, and none of
   * them stands in for a value. The distinction matters in this console
   * specifically: a placeholder is *never* a default. `commandForm` reads
   * `control.value`, so an untouched field submits the empty string and the
   * form's own validation refuses it, which is why a hint like "{}" or "users.id
   * UUID" can never be sent as if an operator had typed it.
   */
  placeholder?: string;
  disabled?: boolean;
  hidden?: boolean;
  rows?: string;
  min?: string;
  max?: string;
  step?: string;
  colspan?: string;
  role?: string;
  "aria-label"?: string;
  "aria-live"?: string;
  "aria-disabled"?: string;
  "data-testid"?: string;
  "data-raw"?: string;
  "data-state"?: string;
  onclick?: (event: MouseEvent) => void;
  onsubmit?: (event: SubmitEvent) => void;
  onchange?: (event: Event) => void;
  oninput?: (event: Event) => void;
}

/** Creates an element, sets attributes and appends children as text nodes. */
export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs: Attrs = {},
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === null || value === false) continue;
    if (key.startsWith("on") && typeof value === "function") {
      node.addEventListener(key.slice(2), value as EventListener);
      continue;
    }
    if (key === "disabled" || key === "hidden") {
      if (value === true) node.setAttribute(key, "");
      continue;
    }
    node.setAttribute(key, String(value));
  }
  append(node, ...children);
  return node;
}

export function append(parent: Node, ...children: Child[]): void {
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue;
    parent.appendChild(typeof child === "string" ? document.createTextNode(child) : child);
  }
}

export function clear(node: Node): void {
  while (node.firstChild) node.removeChild(node.firstChild);
}

/** A text node. The only way text enters the document in this console. */
export function text(value: string): Text {
  return document.createTextNode(value);
}

/** A labelled key/value row. */
export function field(label: string, value: Child, opts: { raw?: string; exact?: boolean } = {}): HTMLElement {
  const dd = el("dd", opts.exact === false ? { class: "inexact", title: "This value did not arrive in the exact wire form the API promises." } : {});
  append(dd, value);
  if (opts.raw !== undefined) dd.setAttribute("data-raw", opts.raw);
  const row = el("div", { class: "field" }, el("dt", {}, label));
  row.appendChild(dd);
  return row;
}

/** A definition list of fields. */
export function fields(...rows: Child[]): HTMLElement {
  return el("dl", { class: "fields" }, ...rows);
}

/** A status pill. `state` becomes a data attribute the stylesheet keys off. */
export function pill(label: string, state: string, title?: string): HTMLElement {
  return el("span", { class: "pill", "data-state": state, ...(title ? { title } : {}) }, label);
}

/**
 * A button that is either live or explicitly, visibly refused.
 *
 * A refused button is rendered `disabled` and carries the reason, rather than
 * being hidden. Hiding it leaves the operator wondering whether the console is
 * broken; disabling it with the reason attached tells them what would have to
 * change. It is never merely styled to look disabled: `disabled` is set, so a
 * click cannot happen at all.
 */
export function actionButton(opts: {
  label: string;
  onClick: () => void;
  allowed: boolean;
  reason?: string;
  variant?: "primary" | "danger" | "quiet";
  testId?: string;
}): HTMLButtonElement {
  const attrs: Attrs = {
    type: "button",
    class: `button ${opts.variant ?? "quiet"}${opts.allowed ? "" : " refused"}`,
  };
  if (opts.testId) attrs["data-testid"] = opts.testId;
  if (!opts.allowed) {
    attrs.disabled = true;
    attrs["aria-disabled"] = "true";
    if (opts.reason) attrs.title = opts.reason;
  } else {
    attrs.onclick = () => opts.onClick();
  }
  return el("button", attrs, opts.label);
}

/** A table from a header row and body rows. */
export function table(headers: readonly string[], rows: readonly HTMLElement[]): HTMLElement {
  const thead = el("thead", {}, el("tr", {}, ...headers.map((h) => el("th", {}, h))));
  const tbody = el("tbody", {});
  append(tbody, ...rows);
  return el("table", { class: "table" }, thead, tbody);
}

/** A panel with a heading and optional description. */
export function panel(title: string, description: Child, ...body: Child[]): HTMLElement {
  return el(
    "section",
    { class: "panel" },
    el("h2", {}, title),
    description ? el("p", { class: "muted" }, description) : null,
    ...body,
  );
}

/** An explicit empty state. Never a blank area. */
export function emptyState(message: string): HTMLElement {
  return el("p", { class: "empty" }, message);
}

/** A prominent notice. `kind` drives the styling only. */
export function notice(kind: "info" | "warn" | "error", ...body: Child[]): HTMLElement {
  return el("div", { class: `notice ${kind}`, role: kind === "error" ? "alert" : "status" }, ...body);
}
