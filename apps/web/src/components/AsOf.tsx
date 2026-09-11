/**
 * The instant the backend stamped on a snapshot.
 *
 * A figure with no moment attached invites the reader to assume it is current,
 * which is the assumption the goal document spends a whole part forbidding. So
 * every snapshot carries one of these, in absolute UTC and in mono, with the
 * exact instant beside it for anyone reconciling against a chain explorer.
 *
 * STALENESS ESCALATES ON A TIMER, and the escalation is the point. Under five
 * seconds the stamp is ordinary; from five to thirty it turns amber; past
 * thirty it turns red and the figures in the same region go faint. A number
 * nobody is updating must stop looking authoritative.
 *
 * It is never blanked. An absent figure is a different claim from an old one,
 * and blanking a number because a refresh failed replaces "this is what we last
 * knew" with "we know nothing", which is false.
 *
 * The `.mono-small` span inside carries the exact instant in brackets and must
 * remain the first one in this element: the Playwright suite reads it and
 * compares it against the `datetime` attribute, which is how the suite proves
 * the interface is not rounding a timestamp.
 */
import { useEffect, useState, type ReactNode } from "react";

import { staleness, utcClock, utcStamp, type Staleness } from "../lib/format.ts";

const TICK_MS = 1000;

/**
 * The tier a snapshot has reached, re-evaluated every second until it settles.
 *
 * Once a stamp is stale it cannot become anything else without a new value, so
 * the timer stops rather than waking the tab forever.
 */
export function useStaleness(at: string | undefined): Staleness {
  const [tier, setTier] = useState<Staleness>(() => staleness(at));

  useEffect(() => {
    const current = staleness(at);
    setTier(current);
    if (current === "stale" || current === "unknown") return;
    const timer = setInterval(() => {
      const next = staleness(at);
      setTier(next);
      if (next === "stale") clearInterval(timer);
    }, TICK_MS);
    return () => {
      clearInterval(timer);
    };
  }, [at]);

  return tier;
}

const TIER_CLASS: Readonly<Record<Staleness, string>> = {
  fresh: "as-of-normal",
  aging: "as-of-aging",
  stale: "as-of-stale",
  unknown: "as-of-normal",
};

/** What the tier means, said in words rather than only in colour. */
const TIER_WORD: Readonly<Record<Staleness, string>> = {
  fresh: "",
  aging: "ageing",
  stale: "stale",
  unknown: "",
};

export function AsOf(props: {
  readonly at: string | undefined;
  readonly label?: string;
  /** Stop the timer. For a historical instant that cannot go stale. */
  readonly frozen?: boolean;
}): ReactNode {
  const live = useStaleness(props.frozen === true ? undefined : props.at);
  const tier: Staleness = props.frozen === true ? "unknown" : live;
  const word = TIER_WORD[tier];

  return (
    <p className={`as-of ${TIER_CLASS[tier]}`}>
      <span>
        {props.label ?? "As of"}{" "}
        <time dateTime={props.at ?? ""} title={utcStamp(props.at)}>
          {utcClock(props.at)}
        </time>
      </span>
      {/* The exact instant, unrounded, exactly as the backend sent it. */}
      <span className="mono-small">({props.at === undefined || props.at === "" ? "not recorded" : props.at})</span>
      {word !== "" && (
        <span className="badge badge-warn" title="This snapshot has not been refreshed recently.">
          {word}
        </span>
      )}
    </p>
  );
}
