/**
 * Pure helpers for the operator console of the advanced area.
 *
 * Nothing here computes a financial figure or decides anything the backend
 * decides. These functions check that what an operator typed has the shape
 * the API accepts (a reference price, an observation time), render a
 * duration the backend measured, choose a tone for a status the backend
 * reported, and order rows the backend returned.
 */

export type Tone = "neutral" | "good" | "warn" | "bad" | "info";

/* --------------------------------------------------------------------------
 * A reference price, as an operator typed it
 * ------------------------------------------------------------------------ */

/**
 * The most fractional digits a sandbox reference price may carry: the
 * canonical instruments' own price-scale bound. The backend refuses more
 * (PRECISION_LOSS); the form says so before anything is sent.
 */
export const MARK_MAX_FRACTION_DIGITS = 18;

/** The most digits a price's mantissa may hold (the database's numeric(38,0)). */
export const MARK_MAX_DIGITS = 38;

export interface PriceCheck {
  readonly ok: boolean;
  /** The price exactly as it will be sent, when `ok`. */
  readonly value: string;
  readonly error: string;
}

const PLAIN_DECIMAL = /^[0-9]+(\.[0-9]+)?$/;

/**
 * A reference price in sandbox dollars per one whole unit of the asset, as an
 * operator typed it: digits with at most one decimal point, no sign, no
 * exponent and no grouping; positive; at most 18 fractional digits. It is
 * sent exactly as typed (trimmed): nothing here rounds or converts it.
 */
export function checkMarkPrice(raw: string): PriceCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: "Enter the price in sandbox dollars per whole unit." };
  if (!PLAIN_DECIMAL.test(value)) {
    return {
      ok: false,
      value: "",
      error: "Use digits and at most one decimal point, with no sign, exponent, spaces or grouping commas.",
    };
  }
  const dot = value.indexOf(".");
  const whole = dot < 0 ? value : value.slice(0, dot);
  const fraction = dot < 0 ? "" : value.slice(dot + 1);
  if (/^0*$/.test(whole) && /^0*$/.test(fraction)) {
    return { ok: false, value: "", error: "A reference price must be greater than zero." };
  }
  if (fraction.length > MARK_MAX_FRACTION_DIGITS) {
    return {
      ok: false,
      value: "",
      error: `A reference price carries at most ${String(MARK_MAX_FRACTION_DIGITS)} fractional digits; the backend refuses a more precise one.`,
    };
  }
  const digits = `${whole}${fraction}`.replace(/^0+/, "");
  if (digits.length > MARK_MAX_DIGITS) {
    return { ok: false, value: "", error: `A reference price has at most ${String(MARK_MAX_DIGITS)} significant digits.` };
  }
  return { ok: true, value, error: "" };
}

/** A source label: what observed the price. Non-empty, at most 100 characters, no surrounding whitespace. */
export function checkMarkSource(raw: string): PriceCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: "Name the source of the price; it is recorded with the mark." };
  if ([...value].length > 100) return { ok: false, value: "", error: "Keep the source to 100 characters." };
  return { ok: true, value, error: "" };
}

export interface InstantCheck {
  readonly ok: boolean;
  /** RFC 3339 in UTC when `ok` and a time was given; undefined means "the time of the request". */
  readonly value: string | undefined;
  readonly error: string;
}

/**
 * The observation time an operator entered in a `datetime-local` field (the
 * browser's own time zone), as the RFC 3339 instant the API takes, or
 * undefined when the field is empty and the backend should use the time of
 * the request. Whether the instant is in the future or too old is the
 * backend's decision, and its refusal is shown with its code.
 */
export function checkObservedAt(raw: string): InstantCheck {
  const value = raw.trim();
  if (value === "") return { ok: true, value: undefined, error: "" };
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) {
    return { ok: false, value: undefined, error: "Enter a date and time, or leave it empty for the time of the request." };
  }
  return { ok: true, value: at.toISOString(), error: "" };
}

/* --------------------------------------------------------------------------
 * Durations the backend measured
 * ------------------------------------------------------------------------ */

const SECOND = 1_000;
const MINUTE = 60 * SECOND;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** Whole units of `unit` in a non-negative integer `millis`, by integer arithmetic only. */
function whole(millis: number, unit: number): number {
  return (millis - (millis % unit)) / unit;
}

const UNITS: ReadonlyArray<readonly [number, string]> = [
  [DAY, "d"],
  [HOUR, "h"],
  [MINUTE, "min"],
  [SECOND, "s"],
];

/**
 * A non-negative whole number of milliseconds in its largest unit and, when
 * it is not zero, the next unit down, e.g. "3 h 12 min", "1 d", "45 s",
 * "850 ms". Anything that is not a whole, non-negative number is reported as
 * such rather than drawn.
 */
export function formatDuration(millis: number): string {
  if (!Number.isSafeInteger(millis) || millis < 0) return "not a whole, non-negative number of milliseconds";
  for (const [index, [unit, label]] of UNITS.entries()) {
    const count = whole(millis, unit);
    if (count === 0) continue;
    const first = `${String(count)} ${label}`;
    const next = UNITS[index + 1];
    if (next === undefined) return first;
    const nextCount = whole(millis - count * unit, next[0]);
    return nextCount > 0 ? `${first} ${String(nextCount)} ${next[1]}` : first;
  }
  return `${String(millis)} ms`;
}

/**
 * The age the backend reported for an observation: "3 min 5 s ago", or, for
 * an observation stamped after the read (a negative age), how far in the
 * future it is. Every reader of marks ignores a future one.
 */
export function formatAge(ageMs: number): string {
  if (!Number.isSafeInteger(ageMs)) return "an age the backend did not report as a whole number";
  if (ageMs < 0) return `${formatDuration(-ageMs)} in the future`;
  return `${formatDuration(ageMs)} ago`;
}

/* --------------------------------------------------------------------------
 * Statuses other subsystems store, as the overview reports them
 * ------------------------------------------------------------------------ */

/** A stable asset's status: ACTIVE, PAUSED (by an operator or the peg breaker) or RETIRED. */
export function stableStatusTone(status: string): Tone {
  switch (status) {
    case "ACTIVE":
      return "good";
    case "PAUSED":
      return "bad";
    default:
      return "neutral";
  }
}

/** A bridge route's status: ACTIVE, PAUSED or CLOSED. */
export function bridgeStatusTone(status: string): Tone {
  switch (status) {
    case "ACTIVE":
      return "good";
    case "PAUSED":
      return "warn";
    default:
      return "neutral";
  }
}

/** A peg alert's severity: CRITICAL or WARNING. */
export function alertTone(severity: string, breakerTripped: boolean): Tone {
  if (breakerTripped || severity === "CRITICAL") return "bad";
  return "warn";
}

/** A reconciliation record's status. Unresolved ones are OPEN, MISMATCH, INVESTIGATING and ESCALATED. */
export const UNRESOLVED_RECONCILIATION_STATUSES = ["OPEN", "MISMATCH", "INVESTIGATING", "ESCALATED"] as const;

export function reconciliationStatusTone(status: string): Tone {
  if ((UNRESOLVED_RECONCILIATION_STATUSES as readonly string[]).includes(status)) return "bad";
  if (status === "MATCHED" || status === "RESOLVED_AUTOMATIC" || status === "RESOLVED_MANUAL") return "good";
  return "neutral";
}

/** A reconciliation kind's summary: unresolved records that block new risk are worse than ones that do not. */
export function reconciliationKindTone(entry: { readonly unresolved: number; readonly blocks_new_risk: number }): Tone {
  if (entry.blocks_new_risk > 0) return "bad";
  if (entry.unresolved > 0) return "warn";
  return "good";
}

/** The advanced reconciliation kinds the overview reports, one entry each (internal/reconciliation). */
export const ADVANCED_RECONCILIATION_KINDS = [
  "ECONOMIC_EFFECT",
  "STABLE_RESERVE",
  "BRIDGE",
  "PREDICTION_ESCROW",
  "CLEARING",
  "DEVNET_CHAIN",
] as const;

export const RECONCILIATION_KIND_MEANING: Readonly<Record<(typeof ADVANCED_RECONCILIATION_KINDS)[number], string>> = {
  ECONOMIC_EFFECT: "An economic effect whose outcome is not known, or whose journal disagrees with it.",
  STABLE_RESERVE: "A stable asset's outstanding supply against its verified reserve and the simulated chain's supply.",
  BRIDGE: "Value locked on a bridge's source against value released on its destination plus value in flight.",
  PREDICTION_ESCROW: "A prediction market's escrowed collateral against the outcome shares it has outstanding.",
  CLEARING: "Whether a clearing house's internal books net to zero across longs and shorts.",
  DEVNET_CHAIN: "Ledger balances against the balances observed on a simulated chain.",
};

/** The meaning of a kind the overview reported, or a plain statement that this build has none for it. */
export function reconciliationKindMeaning(kind: string): string {
  const known = ADVANCED_RECONCILIATION_KINDS.find((value) => value === kind);
  return known === undefined ? "A reconciliation kind this interface has no description for." : RECONCILIATION_KIND_MEANING[known];
}

/* --------------------------------------------------------------------------
 * Ordering rows the backend returned
 * ------------------------------------------------------------------------ */

interface OracleLike {
  readonly symbol: string;
  readonly stale: boolean;
  readonly age_ms?: number;
}

/**
 * The oracle rows an operator should look at first: stale ones before fresh
 * ones, an asset with no mark at all before one with an old mark, then the
 * oldest mark first, then by symbol. A copy; the input is not reordered.
 */
export function oracleOrder<T extends OracleLike>(items: readonly T[]): T[] {
  return [...items].sort((a, b) => {
    if (a.stale !== b.stale) return a.stale ? -1 : 1;
    const aMissing = a.age_ms === undefined;
    const bMissing = b.age_ms === undefined;
    if (aMissing !== bMissing) return aMissing ? -1 : 1;
    if (a.age_ms !== undefined && b.age_ms !== undefined && a.age_ms !== b.age_ms) return b.age_ms - a.age_ms;
    return a.symbol < b.symbol ? -1 : a.symbol > b.symbol ? 1 : 0;
  });
}

interface ReviewLike {
  readonly current_review_transition_id: string;
  readonly resolutions: readonly { readonly pending: boolean; readonly review_transition_id: string }[];
}

/**
 * The resolutions awaiting a second operator: undecided ones that answer the
 * review their case is in now. A pending resolution of an earlier review can
 * no longer be approved, so it is not waiting for anyone's approval.
 */
export function awaitingSecondOperator<R extends ReviewLike>(
  items: readonly R[],
): Array<{ readonly item: R; readonly resolution: R["resolutions"][number] }> {
  const out: Array<{ readonly item: R; readonly resolution: R["resolutions"][number] }> = [];
  for (const item of items) {
    for (const resolution of item.resolutions) {
      if (resolution.pending && resolution.review_transition_id === item.current_review_transition_id) {
        out.push({ item, resolution });
      }
    }
  }
  return out;
}
