/**
 * Timestamp rendering.
 *
 * The backend distinguishes `occurred_at`, `received_at`, `available_at`,
 * `settled_at` and so on; the interface never collapses them into "when". Every
 * figure that is a snapshot carries the `as of` the backend stamped on it
 * (PART 110), and it is shown in the viewer's own zone with the UTC instant
 * available underneath, because a customer reconciling against a chain
 * explorer needs the exact instant, not a friendly approximation.
 */

const dateTime = new Intl.DateTimeFormat(undefined, {
  year: "numeric",
  month: "short",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

const timeOnly = new Intl.DateTimeFormat(undefined, {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

function parse(value: string | undefined): Date | undefined {
  if (value === undefined || value === "") return undefined;
  const parsed = new Date(value);
  return isNaN(parsed.getTime()) ? undefined : parsed;
}

/** Full local date and time, or a stated absence. Never an empty cell. */
export function formatInstant(value: string | undefined): string {
  const parsed = parse(value);
  return parsed === undefined ? "not recorded" : dateTime.format(parsed);
}

/** Local clock time only, for rows already grouped under a date. */
export function formatTimeOnly(value: string | undefined): string {
  const parsed = parse(value);
  return parsed === undefined ? "not recorded" : timeOnly.format(parsed);
}

/** The exact instant as the backend sent it, for reconciliation. */
export function exactInstant(value: string | undefined): string {
  return value === undefined || value === "" ? "not recorded" : value;
}

/** True when the timestamp is in the past. Used for expiry, never for money. */
export function isPast(value: string | undefined, now: Date = new Date()): boolean {
  const parsed = parse(value);
  return parsed === undefined ? false : parsed.getTime() <= now.getTime();
}

/** Whole seconds remaining until an instant, floored at zero. */
export function secondsUntil(value: string | undefined, now: Date = new Date()): number | undefined {
  const parsed = parse(value);
  if (parsed === undefined) return undefined;
  const millis = parsed.getTime() - now.getTime();
  if (millis <= 0) return 0;
  return (millis - (millis % 1000)) / 1000;
}
