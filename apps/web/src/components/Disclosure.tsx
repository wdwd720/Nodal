/**
 * The copy that must not be a tooltip.
 *
 * Everything a customer reads about a stablecoin, a Credit, a simulated result
 * or a model score comes from `lib/honesty.ts`, and this is the only component
 * allowed to render it. The rule that shapes the component is a negative one:
 *
 *   NEVER inside a tooltip. NEVER inside a popover. NEVER inside an accordion
 *   that starts closed.
 *
 * A disclosure a customer has to hover to find has not been made. That is why
 * there is no `collapsed` prop, no `trigger` prop and no compact variant — the
 * shape of this component is the enforcement.
 *
 * It is an `<aside>` with an accessible name so a screen reader user can reach
 * it directly, and so the end-to-end suite can assert that a particular
 * disclosure is present on a particular screen.
 */
import type { ReactNode } from "react";

export function Disclosure(props: {
  readonly title: string;
  readonly children: ReactNode;
}): ReactNode {
  return (
    <aside className="disclosure" aria-label={props.title}>
      <p className="disclosure-title">{props.title}</p>
      <div className="disclosure-body">{props.children}</div>
    </aside>
  );
}

/**
 * Several standing statements together, for a screen that owes more than one.
 * Each keeps its own name so it is individually addressable.
 */
export function DisclosureSet(props: {
  readonly items: ReadonlyArray<{ readonly title: string; readonly body: string }>;
}): ReactNode {
  return (
    <>
      {props.items.map((item) => (
        <Disclosure key={item.title} title={item.title}>
          <p>{item.body}</p>
        </Disclosure>
      ))}
    </>
  );
}
