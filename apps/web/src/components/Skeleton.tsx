/**
 * The loading state, shaped like the thing it is waiting for.
 *
 * Never a spinner for content. A spinner says "something is happening
 * somewhere"; a skeleton says "a table with four columns is arriving here", and
 * the difference is whether the layout moves under the reader when it lands.
 *
 * Regions hydrate independently, so a slow panel does not block a fast one.
 * That is why these take a shape rather than a boolean: a page asks for the
 * shape of the region that is waiting, not for "loading".
 *
 * A SKELETON IS HONEST IN A WAY A STALE NUMBER IS NOT. Where the rules forbid
 * implying a figure is current, this is what shows instead of the last value.
 *
 * It carries a polite live region with a written label, because a customer who
 * cannot see the shapes still needs to know something is on its way — and it
 * is `aria-busy`, so assistive technology treats the region as incomplete
 * rather than as empty.
 */
import type { ReactNode } from "react";

export type SkeletonShape = "text" | "figure" | "block" | "rows";

export function Skeleton(props: {
  readonly shape?: SkeletonShape;
  /** How many lines or rows. Ignored by the single shapes. */
  readonly count?: number;
  /** What is arriving. Read aloud; never left to the shapes alone. */
  readonly label: string;
}): ReactNode {
  const shape = props.shape ?? "text";
  const count = props.count ?? 3;

  return (
    <div className="skeleton-set" role="status" aria-live="polite" aria-busy="true">
      <span className="visually-hidden">{props.label}</span>
      {shape === "figure" && <span className="skeleton skeleton-figure" aria-hidden="true" />}
      {shape === "block" && <span className="skeleton skeleton-block" aria-hidden="true" />}
      {shape === "text" &&
        Array.from({ length: count }, (_unused, index) => (
          <span
            key={index}
            className="skeleton skeleton-line"
            aria-hidden="true"
            style={{ width: index === count - 1 ? "62%" : "100%" }}
          />
        ))}
      {shape === "rows" &&
        Array.from({ length: count }, (_unused, index) => (
          <span key={index} className="skeleton skeleton-row" aria-hidden="true" />
        ))}
    </div>
  );
}

/**
 * The shape of a figure and its label together — a `Field` that has not
 * arrived. The geometry matches `.field`, so nothing shifts when the value
 * lands.
 */
export function SkeletonField(props: { readonly label: string }): ReactNode {
  return (
    <div className="field" role="status" aria-live="polite" aria-busy="true">
      <dt>{props.label}</dt>
      <dd>
        <span className="skeleton skeleton-figure" aria-hidden="true" />
        <span className="visually-hidden">{props.label} is loading</span>
      </dd>
    </div>
  );
}
