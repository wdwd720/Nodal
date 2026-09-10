/**
 * A status is a WORD.
 *
 * WCAG 1.4.1, and it is the rule this whole product category breaks: a green
 * dot is not a status, it is a colour, and it says nothing in grayscale, in
 * forced-colors mode, or to a reader with deuteranopia. So the badge takes its
 * text as required children and the tone as an optional second channel on top.
 *
 * There is deliberately no `tone`-only form and no icon-only form.
 */
import type { ReactNode } from "react";

export type Tone = "neutral" | "good" | "warn" | "bad" | "info";

export function StatusBadge(props: {
  readonly tone?: Tone;
  readonly children: ReactNode;
  /** Long-form explanation. Never the only place the meaning appears. */
  readonly title?: string;
}): ReactNode {
  return (
    <span
      className={`badge badge-${props.tone ?? "neutral"}`}
      {...(props.title === undefined ? {} : { title: props.title })}
    >
      {props.children}
    </span>
  );
}

/**
 * The chip a simulated surface wears.
 *
 * It is rendered by `Panel` automatically and there is no prop to suppress it,
 * because the case it exists for is a screenshot pasted into a conversation
 * with no surrounding context: the dashed border and the hatch say "not real"
 * to a reader who knows the system, and this says it to one who does not.
 */
export function SimulatedChip(): ReactNode {
  return (
    <span
      className="badge badge-simulated"
      title="These figures come from a simulated or observation-only run. No real capital moved."
    >
      Simulated
    </span>
  );
}
