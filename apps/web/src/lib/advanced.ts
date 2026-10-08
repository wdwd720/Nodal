/**
 * Pure helpers for the advanced (sandbox) area (ADR-0020).
 *
 * Nothing here computes a financial figure. These functions name things,
 * order things and count rows; every amount on an advanced page is a string
 * the backend computed and the page only formats (PART 110).
 *
 * The enumerations below repeat values the OpenAPI contract declares, because
 * the runtime validator needs them as values and the generated client only
 * has them as types. Each list is tied to its generated type at compile time
 * (`Same`), so when the contract gains or loses a value `tsc` fails here,
 * instead of the validator quietly refusing — or accepting — something new.
 */
import type { Schemas } from "@controlplane/generated-client";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;
/** True when T is a union of two or more members. */
type IsUnion<T, U = T> = T extends unknown ? ([U] extends [T] ? false : true) : never;

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export type AdvancedMode = Schemas["AdvancedMode"];
/** Every mode, in the order the backend reports decisions (gates.AllModes). */
export const ADVANCED_MODES = ["PAPER", "SANDBOX", "DEVNET", "LIVE"] as const;
export type AdvancedModesMatchContract = Assert<Same<(typeof ADVANCED_MODES)[number], AdvancedMode>>;

/**
 * The modes an economic intent can be stored in. LIVE is not among them: the
 * database refuses a LIVE intent (economic_intents_mode_check), so an intent
 * that claims to be LIVE is a contract violation, not a row to display.
 */
export const PERSISTABLE_MODES = ["PAPER", "SANDBOX", "DEVNET"] as const satisfies readonly AdvancedMode[];

/** The one value domain the advanced API may report. */
export const VALUE_DOMAIN_SIMULATED = "SIMULATED";
export type ValueDomainMatchesContract = Assert<
  Same<typeof VALUE_DOMAIN_SIMULATED, Schemas["ValueDomainSimulated"]>
>;

export type InstrumentKind = Schemas["InstrumentKind"];
export const INSTRUMENT_KINDS = [
  "SPOT_PAIR",
  "EVENT_OUTCOME",
  "STABLE_VALUE",
  "EQUITY",
  "ETF",
  "FUTURE",
  "PERPETUAL",
  "OPTION",
  "SYNTHETIC",
] as const;
export type InstrumentKindsMatchContract = Assert<Same<(typeof INSTRUMENT_KINDS)[number], InstrumentKind>>;

export type EffectStatus = Schemas["EconomicEffect"]["status"];
export const EFFECT_STATUSES = ["PENDING", "APPLIED", "UNKNOWN", "FAILED", "COMPENSATED"] as const;
export type EffectStatusesMatchContract = Assert<Same<(typeof EFFECT_STATUSES)[number], EffectStatus>>;

export type RiskVerdict = Schemas["AdvancedRiskDecision"]["verdict"];
export const RISK_VERDICTS = ["ALLOW", "REJECT"] as const;
export type RiskVerdictsMatchContract = Assert<Same<(typeof RISK_VERDICTS)[number], RiskVerdict>>;

type Terms = Schemas["InstrumentTerms"];

export const OPTION_RIGHTS = ["CALL", "PUT"] as const;
export type OptionRightsMatchContract = Assert<
  Same<(typeof OPTION_RIGHTS)[number], NonNullable<Terms["option_right"]>>
>;

export const EXERCISE_STYLES = ["EUROPEAN", "AMERICAN"] as const;
export type ExerciseStylesMatchContract = Assert<
  Same<(typeof EXERCISE_STYLES)[number], NonNullable<Terms["exercise_style"]>>
>;

export const MARGIN_MODELS = ["NONE", "SPOT_MARGIN", "LINEAR", "OPTION", "BINARY_OUTCOME"] as const;
export type MarginModelsMatchContract = Assert<Same<(typeof MARGIN_MODELS)[number], Terms["margin_model"]>>;

export const CORPORATE_ACTION_BEHAVIORS = ["NONE", "ADJUST"] as const;
export type CorporateActionsMatchContract = Assert<
  Same<(typeof CORPORATE_ACTION_BEHAVIORS)[number], Terms["corporate_action_behavior"]>
>;

export const TRANSFERABILITIES = ["TRANSFERABLE", "NON_TRANSFERABLE"] as const;
export type TransferabilitiesMatchContract = Assert<
  Same<(typeof TRANSFERABILITIES)[number], Terms["transferability"]>
>;

/**
 * Settlement styles: the contract declares exactly two, physical delivery and
 * settlement by paying the value difference in the settlement asset.
 *
 * The second style's wire token is a word PART 112 bans from this interface's
 * source and screens (a stablecoin, and a sandbox dollar even more so, is
 * never called that), so it is deliberately not spelled anywhere in the app.
 * The validator therefore checks the field as a string, the types below prove
 * at compile time that the contract still has PHYSICAL plus exactly one other
 * style, and the page names the other style for what it means.
 */
export const PHYSICAL_SETTLEMENT = "PHYSICAL";
export type SettlementStyle = Terms["settlement_style"];
type ValueSettlement = Exclude<SettlementStyle, typeof PHYSICAL_SETTLEMENT>;
export type SettlementHasPhysical = Assert<Same<Extract<SettlementStyle, typeof PHYSICAL_SETTLEMENT>, "PHYSICAL">>;
export type SettlementHasExactlyOneOtherStyle = Assert<
  Same<[ValueSettlement] extends [never] ? "none" : IsUnion<ValueSettlement>, false>
>;

/** What a settlement style means, in words this interface may use. */
export function settlementStyleLabel(style: string): string {
  return style === PHYSICAL_SETTLEMENT
    ? "Physical delivery of the underlying"
    : "Settled in value: the difference is paid in the settlement asset and nothing is delivered";
}

/* --------------------------------------------------------------------------
 * Capability gate decisions
 * ------------------------------------------------------------------------ */

/** The reason every advanced capability gives for refusing LIVE in this build. */
export const LIVE_NOT_IMPLEMENTED = "LIVE_NOT_IMPLEMENTED";

/**
 * The stable reason codes gates.DecideMode returns, each with what it means.
 * A code the backend sends that is not listed here is shown as the backend's
 * own code with no gloss, never dropped.
 */
export const GATE_REASONS: Readonly<Record<string, string>> = {
  [LIVE_NOT_IMPLEMENTED]:
    "This build implements no live path for the capability. No gate, setting or operator can change " +
    "that; only a code change could.",
  CAPABILITY_GATE_NOT_ACTIVE: "The capability's gate is not ACTIVE in this deployment, and this mode needs it here.",
  MODE_NOT_SUPPORTED: "The capability does not implement this mode.",
  CAPABILITY_KILL_SWITCHED: "An operator kill switch has stopped this capability, or every capability.",
  ENVIRONMENT_UNKNOWN: "The deployment's environment is not one the gate table recognises, so nothing may run.",
  MODE_UNKNOWN: "The mode is not one the gate table declares.",
  NOT_AN_ADVANCED_CAPABILITY: "The capability is not in the advanced gate table.",
};

interface ModeDecisionLike {
  readonly mode: string;
  readonly allowed: boolean;
  readonly reasons: readonly string[];
}

interface CapabilityLike {
  readonly capability: string;
  readonly decisions: readonly ModeDecisionLike[];
}

/** The decision for one mode, or undefined when the backend reported none. */
export function decisionFor<D extends { readonly mode: string }>(decisions: readonly D[], mode: string): D | undefined {
  return decisions.find((decision) => decision.mode === mode);
}

/** Modes that appear more than once in one capability's decisions. */
export function duplicatedModes(decisions: readonly { readonly mode: string }[]): string[] {
  const seen = new Set<string>();
  const repeated = new Set<string>();
  for (const decision of decisions) {
    if (seen.has(decision.mode)) repeated.add(decision.mode);
    seen.add(decision.mode);
  }
  return [...repeated];
}

export interface LiveSummary {
  readonly capabilities: number;
  /** Capabilities whose LIVE decision is a refusal. */
  readonly refused: number;
  /** Of those, how many give LIVE_NOT_IMPLEMENTED among their reasons. */
  readonly notImplemented: number;
  /** Capabilities the backend reports LIVE as allowed for. Empty in this build. */
  readonly allowed: readonly string[];
  /** Capabilities with no LIVE decision at all. */
  readonly unreported: readonly string[];
  /** Capabilities with at least one simulated mode allowed here. */
  readonly runnable: number;
}

/** Counts rows; it decides nothing. The decisions are the backend's. */
export function summariseLive(items: readonly CapabilityLike[]): LiveSummary {
  let refused = 0;
  let notImplemented = 0;
  let runnable = 0;
  const allowed: string[] = [];
  const unreported: string[] = [];
  for (const item of items) {
    const live = decisionFor(item.decisions, "LIVE");
    if (live === undefined) {
      unreported.push(item.capability);
    } else if (live.allowed) {
      allowed.push(item.capability);
    } else {
      refused = refused + 1;
      if (live.reasons.includes(LIVE_NOT_IMPLEMENTED)) notImplemented = notImplemented + 1;
    }
    if (item.decisions.some((decision) => decision.mode !== "LIVE" && decision.allowed)) {
      runnable = runnable + 1;
    }
  }
  return { capabilities: items.length, refused, notImplemented, allowed, unreported, runnable };
}

/** Every distinct reason code across the decisions, sorted. */
export function distinctReasons(items: readonly CapabilityLike[]): string[] {
  const codes = new Set<string>();
  for (const item of items) {
    for (const decision of item.decisions) {
      for (const reason of decision.reasons) codes.add(reason);
    }
  }
  return [...codes].sort();
}

/* --------------------------------------------------------------------------
 * Economic intents and effects
 * ------------------------------------------------------------------------ */

export type Tone = "neutral" | "good" | "warn" | "bad" | "info";

/** What each effect status means. The status machine is enforced in SQL (EF001). */
export const EFFECT_STATUS_MEANING: Readonly<Record<EffectStatus, string>> = {
  PENDING: "Claimed and not yet applied.",
  APPLIED: "Applied exactly once. An effect that posts to the ledger carries its journal transaction.",
  UNKNOWN:
    "The outcome of an external step is not known. It is never retried blindly; reconciliation decides it.",
  FAILED: "Did not apply.",
  COMPENSATED: "Applied, then reversed by a compensating journal transaction. History is never edited.",
};

export function effectStatusTone(status: string): Tone {
  switch (status) {
    case "APPLIED":
      return "good";
    case "PENDING":
      return "info";
    case "UNKNOWN":
      return "warn";
    case "FAILED":
      return "bad";
    default:
      return "neutral";
  }
}

/**
 * What each intent status means. The contract types the status as a string,
 * so a status not listed here is shown as the backend's own word, unglossed.
 */
export const INTENT_STATUS_MEANING: Readonly<Record<string, string>> = {
  RECEIVED: "Recorded; not yet routed.",
  ROUTED: "Mapped onto the subsystem that will carry it out.",
  RISK_APPROVED: "Risk Kernel V2 allowed it.",
  RESERVED: "The capital it needs is reserved.",
  EXECUTING: "Being carried out.",
  COMPLETED: "Carried out.",
  REJECTED: "Refused, with the rejection code shown.",
  FAILED: "Did not complete.",
  UNKNOWN: "The outcome of an external step is not known; only reconciliation can end it.",
  RECONCILED: "An unknown outcome, settled by reconciliation.",
  CANCELLED: "Cancelled before execution.",
};

export function intentStatusTone(status: string): Tone {
  switch (status) {
    case "COMPLETED":
    case "RECONCILED":
      return "good";
    case "REJECTED":
    case "FAILED":
      return "bad";
    case "UNKNOWN":
      return "warn";
    case "CANCELLED":
      return "neutral";
    default:
      return "info";
  }
}

export function verdictTone(verdict: string): Tone {
  return verdict === "ALLOW" ? "good" : verdict === "REJECT" ? "bad" : "neutral";
}

/** LIVE is drawn as a refusal wherever it appears; the simulated modes are information. */
export function modeTone(mode: string): Tone {
  return mode === "LIVE" ? "bad" : "info";
}

/* --------------------------------------------------------------------------
 * Buying power V2
 * ------------------------------------------------------------------------ */

type BuyingPowerV2 = Schemas["BuyingPowerV2"];

/** Every USD figure of a buying power V2 answer. */
export type BuyingPowerComponent = Exclude<
  keyof BuyingPowerV2,
  "value_domain" | "policy_version" | "as_of" | "reasons" | "lines"
>;

export interface BuyingPowerComponentCopy {
  readonly field: BuyingPowerComponent;
  readonly label: string;
  readonly note: string;
}

export interface BuyingPowerGroup {
  readonly title: string;
  readonly description: string;
  /** The group holding the answers themselves, drawn larger. */
  readonly emphasis: boolean;
  readonly components: readonly BuyingPowerComponentCopy[];
}

/**
 * The page's layout of a buying power V2 answer: every component exactly once,
 * grouped, each with a statement of the rule the backend applies. The rules
 * are the ones `internal/capital/buyingpower/v2.go` states; the explanation
 * lines and reasons the backend returns are the authority for any one answer.
 */
const LAYOUT = [
  {
    title: "What this account can do",
    description: "The four answers, computed by the backend for this request at the instant shown.",
    emphasis: true,
    components: [
      {
        field: "trade_buying_power",
        label: "Trade buying power",
        note:
          "Settled, spendable sandbox balance less every reservation, plus the share of unsettled " +
          "proceeds the policy lets fund trading. Never below zero.",
      },
      {
        field: "risk_increasing_buying_power",
        label: "Risk-increasing buying power",
        note:
          "Trade buying power for an account without margin; with margin, margin available scaled by " +
          "the policy's initial margin rate. Never below zero.",
      },
      {
        field: "margin_available",
        label: "Margin available",
        note: "Eligible equity less initial margin used and every reservation. Never below zero.",
      },
      {
        field: "withdrawable",
        label: "Withdrawable",
        note:
          "The lesser of the settled spendable balance after reservations and the equity left above " +
          "maintenance margin plus the withdrawal buffer. Unsettled proceeds are never withdrawable.",
      },
    ],
  },
  {
    title: "Equity and collateral",
    description: "What the account's simulated holdings are marked at, and how much of that the policy counts.",
    emphasis: false,
    components: [
      {
        field: "gross_equity",
        label: "Gross equity",
        note:
          "Every marked holding (wallet, margin collateral, derivatives margin, unsettled receivables, " +
          "bridge value in flight) plus unrealized P&L, less borrow liabilities, accrued interest and fees.",
      },
      {
        field: "eligible_collateral",
        label: "Eligible collateral",
        note: "Each marked holding at its collateral factor, after every haircut. The lines below show each one.",
      },
      {
        field: "haircuts",
        label: "Haircuts",
        note: "Marked holdings less eligible collateral: value the policy does not count.",
      },
      {
        field: "pending_bridge_haircut",
        label: "Pending bridge haircut",
        note: "Bridge value in flight that does not count, because value in flight is not final.",
      },
      {
        field: "borrow_liabilities",
        label: "Borrow liabilities",
        note: "Margin loan principal and capitalised interest.",
      },
      {
        field: "eligible_equity",
        label: "Eligible equity",
        note: "Eligible collateral plus unrealized P&L, less borrow liabilities, accrued interest and fees.",
      },
    ],
  },
  {
    title: "Margin",
    description: "What margined and derivative positions already use.",
    emphasis: false,
    components: [
      {
        field: "initial_margin_used",
        label: "Initial margin used",
        note: "Margin committed to open margined and derivative positions.",
      },
      {
        field: "maintenance_margin",
        label: "Maintenance margin",
        note: "The equity floor: no withdrawal may leave equity below it plus the withdrawal buffer.",
      },
    ],
  },
  {
    title: "Reservations",
    description: "Value spoken for by things that are not final yet. Each one is subtracted before anything is available.",
    emphasis: false,
    components: [
      {
        field: "open_order_reservations",
        label: "Open orders",
        note: "Reserved by orders that have neither filled nor ended.",
      },
      {
        field: "pending_transfer_reservations",
        label: "Pending transfers",
        note: "Reserved by transfers that are not final.",
      },
      {
        field: "pending_withdrawal_reservations",
        label: "Pending withdrawals",
        note: "Reserved by withdrawals that are not final.",
      },
      {
        field: "strategy_reservations",
        label: "Strategies",
        note: "Reserved by running strategies.",
      },
    ],
  },
] as const satisfies readonly BuyingPowerGroup[];

/** Fails to compile when the contract gains a component the page does not show, or loses one it does. */
export type BuyingPowerComponentsCovered = Assert<
  Same<(typeof LAYOUT)[number]["components"][number]["field"], BuyingPowerComponent>
>;

/** The layout, in display order. */
export const BUYING_POWER_V2_GROUPS: readonly BuyingPowerGroup[] = LAYOUT;

/** Every component, in display order. */
export const BUYING_POWER_V2_FIELDS: readonly BuyingPowerComponent[] = BUYING_POWER_V2_GROUPS.flatMap((group) =>
  group.components.map((component) => component.field),
);

/** The label of a component, or the backend's field name when it is not one. */
export function componentLabel(field: string): string {
  for (const group of BUYING_POWER_V2_GROUPS) {
    for (const component of group.components) {
      if (component.field === field) return component.label;
    }
  }
  return field;
}

/** Groups explanation lines by the figure they explain, keeping the backend's order. */
export function groupByField<L extends { readonly field: string }>(
  lines: readonly L[],
): Array<{ readonly field: string; readonly lines: readonly L[] }> {
  const order: string[] = [];
  const byField = new Map<string, L[]>();
  for (const line of lines) {
    const list = byField.get(line.field);
    if (list === undefined) {
      order.push(line.field);
      byField.set(line.field, [line]);
    } else {
      list.push(line);
    }
  }
  return order.map((field) => ({ field, lines: byField.get(field) ?? [] }));
}

/* --------------------------------------------------------------------------
 * Paging, parsing, durations
 * ------------------------------------------------------------------------ */

/**
 * Where a cursor-paged list is. The backend's cursors only go forward, so the
 * way back is the trail of cursors that led here.
 */
export interface CursorTrail {
  /** The cursor of the page on screen; undefined for the first page. */
  readonly current: string | undefined;
  /** The cursors of the pages before it, oldest first. */
  readonly previous: ReadonlyArray<string | undefined>;
}

export const FIRST_PAGE: CursorTrail = { current: undefined, previous: [] };

export function forward(trail: CursorTrail, nextCursor: string): CursorTrail {
  return { current: nextCursor, previous: [...trail.previous, trail.current] };
}

export function back(trail: CursorTrail): CursorTrail {
  const last = trail.previous.length - 1;
  if (last < 0) return trail;
  return { current: trail.previous[last], previous: trail.previous.slice(0, last) };
}

/** 1 for the first page. */
export function pageNumber(trail: CursorTrail): number {
  return trail.previous.length + 1;
}

/** A `kind` search parameter: a contract kind, nothing, or a value to say was ignored. */
export function parseInstrumentKind(raw: string | null): {
  readonly kind: InstrumentKind | undefined;
  readonly ignored: string | undefined;
} {
  if (raw === null || raw === "") return { kind: undefined, ignored: undefined };
  const kind = INSTRUMENT_KINDS.find((candidate) => candidate === raw);
  return kind === undefined ? { kind: undefined, ignored: raw } : { kind, ignored: undefined };
}

const CANONICAL_UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** The canonical 36-character form. Whether the id may be read is the backend's decision. */
export function isCanonicalUuid(value: string): boolean {
  return CANONICAL_UUID.test(value);
}

const DURATION_UNITS: ReadonlyArray<readonly [number, string]> = [
  [86_400, "day"],
  [3_600, "hour"],
  [60, "minute"],
];

/** "30 days" for 2592000. Exact: a duration that is not a whole number of a unit is shown in seconds. */
export function formatSeconds(seconds: number): string {
  if (!Number.isInteger(seconds) || seconds < 0) return "not a whole, non-negative number of seconds";
  for (const [size, unit] of DURATION_UNITS) {
    if (seconds >= size && seconds % size === 0) {
      const count = seconds / size;
      return `${String(count)} ${unit}${count === 1 ? "" : "s"}`;
    }
  }
  return `${String(seconds)} second${seconds === 1 ? "" : "s"}`;
}

/** "5 minutes" for 300000; "1500 ms" when the value is not whole seconds. */
export function formatMillis(millis: number): string {
  if (!Number.isInteger(millis) || millis < 0) return "not a whole, non-negative number of milliseconds";
  if (millis % 1_000 !== 0) return `${String(millis)} ms`;
  return formatSeconds(millis / 1_000);
}
