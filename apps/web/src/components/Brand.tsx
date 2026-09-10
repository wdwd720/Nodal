/**
 * The mark.
 *
 * Three paths enter, one node, one path leaves: many intents converge on one
 * node and exactly one deterministic operation emerges. It also reads as the
 * three pots meeting a control plane that never lets them merge — they touch
 * the node, they do not touch each other.
 *
 * THE MARK IS NEUTRAL. It draws in `currentColor` and never adopts a
 * temperature accent, because it sits above all three pots and must not imply
 * any one of them. The CSS in components.css pins its colour to `--ink` for
 * that reason: a mark inside a simulated panel is still the product's mark.
 *
 * The geometry is the geometry of `public/brand/mark-full.svg` and
 * `mark-compact.svg`, with every coordinate multiplied by one hundred and the
 * viewBox multiplied to match. That is not decoration: `source-scan.test.ts`
 * refuses a decimal literal anywhere in interface code, because a bare decimal
 * sitting in a component and passed to a formatter would render exactly like a
 * real balance. Scaling the viewBox keeps the drawing identical and the rule
 * intact. The two opacities in the full mark move to classes for the same
 * reason.
 *
 * It is inline rather than an `<img>` so that `currentColor` reaches it. The
 * files under `public/brand/` are the same drawing for everything outside the
 * application: the favicon, a social card, a document.
 */
import type { ReactNode } from "react";

/** The full mark. 24px and above. */
export function Mark(props: { readonly className?: string }): ReactNode {
  return (
    <svg
      className={props.className ?? "brand-mark"}
      viewBox="0 0 3200 3200"
      fill="none"
      aria-hidden="true"
      focusable="false"
    >
      <g stroke="currentColor" strokeWidth="200" strokeLinecap="round">
        <path className="mark-in" d="M200 600 L1376 1335" />
        <path className="mark-in-strong" d="M200 1600 L1300 1600" />
        <path className="mark-in" d="M200 2600 L1376 1865" />
        <path d="M2300 1600 L3000 1600" />
      </g>
      <circle cx="1800" cy="1600" r="350" fill="currentColor" />
    </svg>
  );
}

/**
 * The compact mark: a path, interrupted by a node, continuing. Nothing passes
 * through this system without passing through the checkpoint. Legible at 16px,
 * which is where the full mark stops being legible.
 */
export function MarkCompact(props: { readonly className?: string }): ReactNode {
  return (
    <svg
      className={props.className ?? "brand-mark"}
      viewBox="0 0 2400 2400"
      fill="none"
      aria-hidden="true"
      focusable="false"
    >
      <g stroke="currentColor" strokeWidth="225" strokeLinecap="round">
        <path d="M100 1200 L700 1200" />
        <path d="M1700 1200 L2300 1200" />
      </g>
      <circle cx="1200" cy="1200" r="400" fill="currentColor" />
    </svg>
  );
}

/**
 * The horizontal lockup: mark plus wordmark.
 *
 * The wordmark is live text rather than an embedded `<text>` element, set in
 * the same IBM Plex Sans SemiBold at the same 0.12em tracking that
 * `public/brand/wordmark.svg` specifies. Live text is selectable, scales with
 * the reader's own type size, and is read correctly aloud; a five-letter word
 * drawn as vector paths is none of those things.
 *
 * The product name is a string in one place so that it stays configurable:
 * "Nodal" is an internal codename and a financial company of that name already
 * exists, so it must never be welded into the markup of every screen.
 */
export const PRODUCT_NAME = "Nodal";

export function BrandLockup(props: {
  readonly large?: boolean;
  readonly compact?: boolean;
}): ReactNode {
  return (
    <span className={props.large === true ? "brand brand-large" : "brand"}>
      {props.compact === true ? <MarkCompact /> : <Mark />}
      <span className="brand-name">{PRODUCT_NAME}</span>
    </span>
  );
}
