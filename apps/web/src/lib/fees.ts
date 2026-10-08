/**
 * Pure helpers for the fees section of the advanced area: the transparent
 * spread and fee engine and the sandbox performance fee (internal/feeledger,
 * ADR-0020).
 *
 * Nothing here computes a fee. Every charged unit on a fees page is a string
 * the fee engine computed and the fee ledger stored; these functions name the
 * components, say what a status means, and check what someone typed before it
 * is sent (PART 110). The quote calculator asks the backend to price an order;
 * it never prices one itself.
 */
import type { Schemas } from "@controlplane/generated-client";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export type FeeSide = Schemas["FeeSide"];
export const FEE_SIDES = ["BUY", "SELL"] as const;
export type FeeSidesMatchContract = Assert<Same<(typeof FEE_SIDES)[number], FeeSide>>;

export type FeeBookingMode = Schemas["FeeBookingMode"];
export const FEE_BOOKING_MODES = ["POSTED", "EMBEDDED"] as const;
export type FeeBookingModesMatchContract = Assert<Same<(typeof FEE_BOOKING_MODES)[number], FeeBookingMode>>;

export type FeeBookingStatus = Schemas["FeeBookingStatus"];
export const FEE_BOOKING_STATUSES = ["BOOKED", "REVERSED"] as const;
export type FeeBookingStatusesMatchContract = Assert<Same<(typeof FEE_BOOKING_STATUSES)[number], FeeBookingStatus>>;

export type FeePeriodStatus = Schemas["FeePeriodStatus"];
export const FEE_PERIOD_STATUSES = ["OPEN", "CRYSTALLIZED", "CANCELLED", "REVERSED"] as const;
export type FeePeriodStatusesMatchContract = Assert<Same<(typeof FEE_PERIOD_STATUSES)[number], FeePeriodStatus>>;

export type FeeLineKind = Schemas["FeeLineKind"];
export const FEE_LINE_KINDS = ["ACCRUAL", "REVERSAL", "NO_CHANGE", "CRYSTALLIZATION"] as const;
export type FeeLineKindsMatchContract = Assert<Same<(typeof FEE_LINE_KINDS)[number], FeeLineKind>>;

/** The modes a recorded quote and a performance period run in. */
export const FEE_RUN_MODES = ["PAPER", "SANDBOX"] as const satisfies readonly Schemas["AdvancedMode"][];

/** The subsystems a recorded quote can belong to (econ.Subsystem). */
export const QUOTE_SUBSYSTEMS = [
  "ORDERS",
  "EQUITIES",
  "TRANSFERS",
  "TREASURY",
  "FEES",
  "MARGIN",
  "DERIVATIVES",
  "STABLE_ASSET",
  "BRIDGE",
  "PREDICTION_MARKETS",
  "MARKETPLACE",
] as const;

/** The rounding modes a schedule may name, in the backend's canonical spelling. */
export const ROUNDING_MODES = ["down", "up", "half_even", "half_up", "floor", "ceil", "exact"] as const;

/** The schedule's rounding fields, in the order the contract lists them. */
export const ROUNDING_FIELDS = [
  "buy_notional",
  "sell_notional",
  "spread",
  "percentage_fee",
  "venue_pass_through",
  "strategy_fee",
  "fee_cap",
  "slippage_reserve",
] as const satisfies readonly Exclude<keyof Schemas["FeeRounding"], "allow_platform_favourable">[];
export type RoundingField = (typeof ROUNDING_FIELDS)[number];

/**
 * The fee engine's default rounding: every component rounded in the
 * customer's favour, and the slippage reserve (held, never charged) rounded
 * up so that the hold always covers the full tolerance.
 */
export const CUSTOMER_FAVOURABLE_ROUNDING: Readonly<Record<RoundingField, (typeof ROUNDING_MODES)[number]>> = {
  buy_notional: "down",
  sell_notional: "up",
  spread: "down",
  percentage_fee: "down",
  venue_pass_through: "down",
  strategy_fee: "down",
  fee_cap: "down",
  slippage_reserve: "up",
};

/* --------------------------------------------------------------------------
 * What things mean
 * ------------------------------------------------------------------------ */

export type Tone = "neutral" | "good" | "warn" | "bad" | "info";

export const BOOKING_MODE_MEANING: Readonly<Record<FeeBookingMode, string>> = {
  POSTED: "The fee ledger posted the charge in its own journal, every component on its own pair of accounts.",
  EMBEDDED: "The charge was posted inside the trade's own journal; the fee ledger verified that it carries exactly these amounts.",
};

export const BOOKING_STATUS_MEANING: Readonly<Record<FeeBookingStatus, string>> = {
  BOOKED: "Charged, as disclosed.",
  REVERSED: "Returned in full by a compensating journal with a reason code. History is never edited.",
};

export function bookingStatusTone(status: string): Tone {
  return status === "BOOKED" ? "info" : status === "REVERSED" ? "warn" : "neutral";
}

export const PERIOD_STATUS_MEANING: Readonly<Record<FeePeriodStatus, string>> = {
  OPEN: "Valuations accrue into a running, provisional accrual; nothing has been charged.",
  CRYSTALLIZED: "Closed at its end: the fee was charged and the statement stored with its hash.",
  CANCELLED: "Cancelled while open; any running accrual was reversed by a compensating line.",
  REVERSED: "The crystallization was reversed by a compensation of exactly the fee.",
};

export function periodStatusTone(status: string): Tone {
  switch (status) {
    case "OPEN":
      return "info";
    case "CRYSTALLIZED":
      return "good";
    case "CANCELLED":
      return "neutral";
    case "REVERSED":
      return "warn";
    default:
      return "neutral";
  }
}

export const LINE_KIND_MEANING: Readonly<Record<FeeLineKind, string>> = {
  ACCRUAL: "A valuation raised the running accrual.",
  REVERSAL: "A valuation lowered it: a compensating line, never an edit.",
  NO_CHANGE: "A valuation left the accrual where it was.",
  CRYSTALLIZATION: "The accrual became the period's fee.",
};

/** The fee engine's component names, in words. */
const COMPONENT_LABEL: Readonly<Record<string, string>> = {
  REFERENCE_NOTIONAL: "Reference notional",
  SPREAD: "Spread",
  FIXED_FEE: "Fixed fee",
  PERCENTAGE_FEE: "Percentage fee",
  VENUE_PASS_THROUGH: "Venue pass-through",
  STRATEGY_FEE: "Strategy fee",
  MINIMUM_FEE: "Minimum fee",
  FEE_CAP: "Fee cap",
  NETWORK_FEE: "Network fee",
  SLIPPAGE_RESERVE: "Slippage reserve",
  EFFECTIVE_PRICE: "Effective price",
  BUY_NOTIONAL: "Notional of a buy",
  SELL_NOTIONAL: "Notional of a sell",
};

/** A component's label, or the backend's own code when this build has no word for it. */
export function componentLabel(component: string): string {
  return COMPONENT_LABEL[component] ?? component;
}

/** What a verification says, as a tone and a sentence. */
export function verificationSummary(v: { readonly verified: boolean; readonly code?: string | undefined }): {
  readonly tone: Tone;
  readonly label: string;
} {
  if (v.verified) return { tone: "good", label: "re-proved from the record" };
  return { tone: "bad", label: `does not verify${v.code === undefined ? "" : ` (${v.code})`}` };
}

/* --------------------------------------------------------------------------
 * What someone typed, checked before it is sent
 * ------------------------------------------------------------------------ */

export type Parsed<T> = { readonly ok: true; readonly value: T } | { readonly ok: false; readonly error: string };

function stripLeadingZeros(digits: string): string {
  let i = 0;
  while (i < digits.length - 1 && digits[i] === "0") i = i + 1;
  return digits.slice(i);
}

/**
 * A USD figure as the API takes it: an optional minus (only when `signed`),
 * digits, and at most two fraction digits, normalised to exactly two. It is
 * rewritten by moving digits, never converted to a number.
 */
export function parseUsdInput(raw: string, what: string, signed = false): Parsed<string> {
  const cleaned = raw.trim().replace(/,/g, "");
  const pattern = signed ? /^-?[0-9]{1,15}(\.[0-9]{0,2})?$/ : /^[0-9]{1,15}(\.[0-9]{0,2})?$/;
  if (!pattern.test(cleaned)) {
    return {
      ok: false,
      error: signed
        ? `${what} is a US-dollar figure with at most two decimal places; a minus sign marks a withdrawal.`
        : `${what} is a US-dollar figure with at most two decimal places.`,
    };
  }
  const negative = cleaned.startsWith("-");
  const unsigned = negative ? cleaned.slice(1) : cleaned;
  const dot = unsigned.indexOf(".");
  const whole = stripLeadingZeros(dot < 0 ? unsigned : unsigned.slice(0, dot));
  const fraction = (dot < 0 ? "" : unsigned.slice(dot + 1)).padEnd(2, "0");
  const zero = /^0+$/.test(whole) && /^0+$/.test(fraction);
  return { ok: true, value: `${negative && !zero ? "-" : ""}${whole}.${fraction}` };
}

const INSTANT = /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]{1,3})?)?Z$/;

/** An instant written in UTC with a trailing Z, such as 2026-10-07T08:00:00Z, in canonical form. */
export function parseUtcInstant(raw: string, what: string): Parsed<string> {
  const cleaned = raw.trim();
  if (!INSTANT.test(cleaned)) {
    return { ok: false, error: `${what} is an instant in UTC, such as 2026-10-07T08:00:00Z.` };
  }
  const at = new Date(cleaned);
  if (isNaN(at.getTime())) {
    return { ok: false, error: `${what} is not a real instant.` };
  }
  return { ok: true, value: at.toISOString() };
}

const SCHEDULE_KEY = /^[a-z0-9][a-z0-9_.:/-]{0,127}$/;
const REASON_CODE = /^[A-Z][A-Z0-9_]{2,63}$/;
const STRATEGY_REF = /^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,89}$/;

/** A schedule key: lower-case letters, digits and _ . : / -, at most 128 characters. */
export function parseScheduleKey(raw: string): Parsed<string> {
  const value = raw.trim();
  if (!SCHEDULE_KEY.test(value)) {
    return { ok: false, error: "A schedule key is lower-case letters, digits and _ . : / -, starting with a letter or digit, at most 128 characters." };
  }
  return { ok: true, value };
}

/** A reversal's reason code: upper-case letters, digits and underscores, 3 to 64 characters, starting with a letter. */
export function parseReasonCode(raw: string): Parsed<string> {
  const value = raw.trim();
  if (!REASON_CODE.test(value)) {
    return { ok: false, error: "A reason code is upper-case letters, digits and underscores, 3 to 64 characters, starting with a letter." };
  }
  return { ok: true, value };
}

/** A performance period's strategy reference. */
export function parseStrategyRef(raw: string): Parsed<string> {
  const value = raw.trim();
  if (!STRATEGY_REF.test(value)) {
    return { ok: false, error: "A strategy reference is letters, digits and _ . : / -, starting with a letter or digit, at most 90 characters." };
  }
  return { ok: true, value };
}

const DIGITS = "0123456789";

/** Basis points 0..10000, built digit by digit: a rate, never an amount. */
function parseBps(raw: string, what: string): Parsed<number> {
  if (!/^[0-9]{1,5}$/.test(raw)) return { ok: false, error: `${what} is a whole number of basis points.` };
  let n = 0;
  for (const ch of raw) n = n * 10 + DIGITS.indexOf(ch);
  if (n > 10000) return { ok: false, error: `${what} is at most 10000 basis points.` };
  return { ok: true, value: n };
}

/** One volume tier as the contract takes it. */
export interface TierInput {
  readonly min_trailing_volume_usd: string;
  readonly overrides_percentage_fee: boolean;
  readonly percentage_fee_bps: number;
  readonly overrides_spread: boolean;
  readonly spread_bps: number;
}

/**
 * Volume tiers, one per line: the trailing volume in USD from which the tier
 * applies, then its percentage fee and its spread in basis points, each "-"
 * when the tier leaves the base schedule's rate in place. The fee engine
 * requires strictly ascending volumes and says so if they are not.
 */
export function parseTierLines(raw: string): Parsed<TierInput[]> {
  const out: TierInput[] = [];
  const lines = raw.split("\n");
  for (let i = 0; i < lines.length; i = i + 1) {
    const line = (lines[i] ?? "").trim();
    if (line === "") continue;
    const where = `Tier line ${String(i + 1)}`;
    const parts = line.split(/\s+/);
    if (parts.length !== 3) {
      return { ok: false, error: `${where}: write the volume in USD, the percentage fee and the spread in basis points ("-" keeps the base rate).` };
    }
    const volume = parseUsdInput(parts[0] ?? "", `${where}: the volume`);
    if (!volume.ok) return volume;
    const pct = parts[1] ?? "-";
    const spread = parts[2] ?? "-";
    const p = pct === "-" ? undefined : parseBps(pct, `${where}: the percentage fee`);
    if (p !== undefined && !p.ok) return p;
    const s = spread === "-" ? undefined : parseBps(spread, `${where}: the spread`);
    if (s !== undefined && !s.ok) return s;
    out.push({
      min_trailing_volume_usd: volume.value,
      overrides_percentage_fee: p !== undefined,
      percentage_fee_bps: p !== undefined && p.ok ? p.value : 0,
      overrides_spread: s !== undefined,
      spread_bps: s !== undefined && s.ok ? s.value : 0,
    });
  }
  return { ok: true, value: out };
}

/** The address of one schedule key's page: the key travels as a query parameter, encoded. */
export function scheduleQuery(key: string): string {
  return `?key=${encodeURIComponent(key)}`;
}
