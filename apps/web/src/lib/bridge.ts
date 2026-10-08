/**
 * Pure helpers for the bridge section of the advanced area (internal/bridge
 * and the simulated chain host, internal/devnethost).
 *
 * Nothing here computes a financial figure. Every amount the section shows is
 * a string the bridge service computed; these functions name leg states, lay
 * out a leg's progress from the states its own records say it passed through,
 * classify what a leg's state says about its outcome, and check what a person
 * typed.
 *
 * The enumerations repeat values the OpenAPI contract declares, because the
 * runtime validator needs them as values; each is tied to its generated type
 * at compile time, so a contract change fails `tsc` here.
 */
import type { Schemas } from "@controlplane/generated-client";

import type { Tone } from "./stable.ts";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export type BridgeChainId = Schemas["BridgeChainId"];
export const BRIDGE_CHAIN_IDS = ["sbx-solana", "sbx-ethereum"] as const;
export type BridgeChainIdsMatchContract = Assert<Same<(typeof BRIDGE_CHAIN_IDS)[number], BridgeChainId>>;

export type BridgeRouteStatus = Schemas["BridgeRouteStatus"];
export const BRIDGE_ROUTE_STATUSES = ["ACTIVE", "PAUSED", "CLOSED"] as const;
export type BridgeRouteStatusesMatchContract = Assert<Same<(typeof BRIDGE_ROUTE_STATUSES)[number], BridgeRouteStatus>>;

export type BridgeLegStatus = Schemas["BridgeLegStatus"];
export const BRIDGE_LEG_STATUSES = [
  "AVAILABLE",
  "RESERVED",
  "BRIDGE_REQUESTED",
  "SOURCE_FINALIZED",
  "MESSAGE_IN_FLIGHT",
  "DESTINATION_OBSERVED",
  "DESTINATION_FINALIZED",
  "RECONCILED",
  "FAILED",
  "MANUAL_REVIEW",
] as const;
export type BridgeLegStatusesMatchContract = Assert<Same<(typeof BRIDGE_LEG_STATUSES)[number], BridgeLegStatus>>;

export type BridgeLegHolding = Schemas["BridgeLegHolding"];
export const BRIDGE_LEG_HOLDINGS = ["WALLET_SOURCE_RESERVED", "BRIDGE_IN_FLIGHT", "WALLET_DESTINATION", "NONE"] as const;
export type BridgeLegHoldingsMatchContract = Assert<Same<(typeof BRIDGE_LEG_HOLDINGS)[number], BridgeLegHolding>>;

export type BridgeLegMode = Schemas["BridgeLegMode"];
export const BRIDGE_LEG_MODES = ["SANDBOX", "DEVNET"] as const;
export type BridgeLegModesMatchContract = Assert<Same<(typeof BRIDGE_LEG_MODES)[number], BridgeLegMode>>;

export type BridgeAttemptKind = Schemas["BridgeAttemptKind"];
export const BRIDGE_ATTEMPT_KINDS = ["RELEASE", "REFUND"] as const;
export type BridgeAttemptKindsMatchContract = Assert<Same<(typeof BRIDGE_ATTEMPT_KINDS)[number], BridgeAttemptKind>>;

export type BridgeTxState = Schemas["BridgeTxState"];
export const BRIDGE_TX_STATES = ["NOT_FOUND", "PENDING", "CONFIRMED", "FINALIZED", "FAILED"] as const;
export type BridgeTxStatesMatchContract = Assert<Same<(typeof BRIDGE_TX_STATES)[number], BridgeTxState>>;

export type BridgeFaultKind = Schemas["BridgeFaultKind"];
export const BRIDGE_FAULT_KINDS = [
  "TIMEOUT_BUT_LAND",
  "TIMEOUT_AND_DROP",
  "DROP_NEXT",
  "DELAY_INCLUSION",
  "CLEAR_FAULTS",
] as const;
export type BridgeFaultKindsMatchContract = Assert<Same<(typeof BRIDGE_FAULT_KINDS)[number], BridgeFaultKind>>;

export type BridgeReconciliationState = Schemas["BridgeReconciliationState"];
export const BRIDGE_RECONCILIATION_STATES = ["NOT_REQUIRED", "AWAITING", "RESOLVED"] as const;
export type BridgeReconciliationStatesMatchContract = Assert<
  Same<(typeof BRIDGE_RECONCILIATION_STATES)[number], BridgeReconciliationState>
>;

export type BridgeEffectStatus = Schemas["BridgeEffectStatus"];
export const BRIDGE_EFFECT_STATUSES = ["PENDING", "APPLIED", "UNKNOWN", "FAILED", "COMPENSATED"] as const;
export type BridgeEffectStatusesMatchContract = Assert<Same<(typeof BRIDGE_EFFECT_STATUSES)[number], BridgeEffectStatus>>;

/* --------------------------------------------------------------------------
 * What states mean
 * ------------------------------------------------------------------------ */

export const ROUTE_STATUS_MEANING: Readonly<Record<BridgeRouteStatus, string>> = {
  ACTIVE: "Takes new legs.",
  PAUSED:
    "Takes no new legs; legs already in flight still complete. An operator or the reconciliation breaker paused it; only an operator resumes it.",
  CLOSED: "Closed for good.",
};

export function routeStatusTone(status: string): Tone {
  switch (status) {
    case "ACTIVE":
      return "good";
    case "PAUSED":
      return "warn";
    default:
      return "neutral";
  }
}

export const LEG_STATUS_MEANING: Readonly<Record<BridgeLegStatus, string>> = {
  AVAILABLE: "The request exists; nothing is reserved yet.",
  RESERVED: "The units are reserved in the source wallet.",
  BRIDGE_REQUESTED:
    "The lock is signed by the holder's custodied key and submitted; the units are still in the source wallet under the reservation.",
  SOURCE_FINALIZED: "The lock is final on the source chain; BRIDGE_SOURCE_FINALIZED moved the units to BRIDGE_IN_FLIGHT.",
  MESSAGE_IN_FLIGHT: "The attester signed a release and it was relayed to the destination chain.",
  DESTINATION_OBSERVED: "The release is confirmed on the destination chain but not final; a reorg could still remove it.",
  DESTINATION_FINALIZED:
    "The release is final on the destination chain; BRIDGE_DESTINATION_FINALIZED moved the value to the destination wallet.",
  RECONCILED: "The lock, the release and both postings were checked against both chains and agree.",
  FAILED: "The leg ended without moving value across: its reservation was released, or it was refunded on the source.",
  MANUAL_REVIEW: "The destination could not complete or reconcile on its own; a person decides what happens next.",
};

export function legStatusTone(status: string): Tone {
  switch (status) {
    case "RECONCILED":
    case "DESTINATION_FINALIZED":
      return "good";
    case "FAILED":
    case "MANUAL_REVIEW":
      return "bad";
    default:
      return "info";
  }
}

export const HOLDING_MEANING: Readonly<Record<BridgeLegHolding, string>> = {
  WALLET_SOURCE_RESERVED: "Still in the source wallet, under the reservation for the lock.",
  BRIDGE_IN_FLIGHT: "In BRIDGE_IN_FLIGHT: locked on the source and not yet released on the destination.",
  WALLET_DESTINATION: "In the destination wallet.",
  NONE: "Nowhere: the leg moved nothing, or it was refunded on the source.",
};

export const TX_STATE_MEANING: Readonly<Record<BridgeTxState, string>> = {
  NOT_FOUND: "The chain does not hold it: never accepted, dropped, expired, or removed by a reorg.",
  PENDING: "Accepted and waiting to be included in a block.",
  CONFIRMED: "Executed in a block that a reorg could still remove.",
  FINALIZED: "Executed in a finalized block. Permanent.",
  FAILED: "Included and failed; permanent once final.",
};

export function txStateTone(state: string): Tone {
  switch (state) {
    case "FINALIZED":
      return "good";
    case "FAILED":
      return "bad";
    case "NOT_FOUND":
      return "warn";
    default:
      return "info";
  }
}

export const RECONCILIATION_STATE_MEANING: Readonly<Record<BridgeReconciliationState, string>> = {
  NOT_REQUIRED: "No submission of this leg had an unknown outcome.",
  AWAITING:
    "A submission of this leg has an unknown outcome and waits to be settled from the chains by transaction id. It is not a success and not a failure.",
  RESOLVED: "A submission of this leg had an unknown outcome and has since been settled from the chains.",
};

export const FAULT_KIND_MEANING: Readonly<Record<BridgeFaultKind, string>> = {
  TIMEOUT_BUT_LAND: "The next submission answers with a timeout but lands: its outcome is unknown to whoever submitted it.",
  TIMEOUT_AND_DROP: "The next submission answers with a timeout and does not land.",
  DROP_NEXT: "The next submissions, as many as the count, are dropped without an answer.",
  DELAY_INCLUSION: "Every transaction accepted afterwards waits the count of blocks before it is included; zero restores immediate inclusion.",
  CLEAR_FAULTS: "Disarms every submission fault and removes the delay.",
};

/** True for the faults that take a count. */
export function faultTakesCount(kind: string): boolean {
  return kind === "DROP_NEXT" || kind === "DELAY_INCLUSION";
}

/* --------------------------------------------------------------------------
 * A leg's outcome, from its own state
 * ------------------------------------------------------------------------ */

export type LegOutcome = "COMPLETED" | "FAILED" | "IN_PROGRESS" | "UNKNOWN" | "REVIEW";

/**
 * What a leg's state says. A leg with a submission whose outcome is unknown is
 * UNKNOWN, never a success or a failure, until it is settled from the chains.
 */
export function legOutcome(status: string, reconciliationState: string): LegOutcome {
  if (reconciliationState === "AWAITING") return "UNKNOWN";
  switch (status) {
    case "RECONCILED":
      return "COMPLETED";
    case "FAILED":
      return "FAILED";
    case "MANUAL_REVIEW":
      return "REVIEW";
    default:
      return "IN_PROGRESS";
  }
}

export const LEG_OUTCOME_LABEL: Readonly<Record<LegOutcome, string>> = {
  COMPLETED: "Bridged and reconciled",
  FAILED: "Failed",
  IN_PROGRESS: "Not final yet",
  UNKNOWN: "Outcome unknown",
  REVIEW: "Waiting for a person",
};

export function legOutcomeTone(outcome: LegOutcome): Tone {
  switch (outcome) {
    case "COMPLETED":
      return "good";
    case "FAILED":
    case "REVIEW":
      return "bad";
    case "UNKNOWN":
      return "warn";
    default:
      return "info";
  }
}

/* --------------------------------------------------------------------------
 * A leg's progress
 * ------------------------------------------------------------------------ */

/** The path a leg takes when nothing goes wrong. */
export const LEG_MAIN_PATH: readonly BridgeLegStatus[] = [
  "BRIDGE_REQUESTED",
  "SOURCE_FINALIZED",
  "MESSAGE_IN_FLIGHT",
  "DESTINATION_OBSERVED",
  "DESTINATION_FINALIZED",
  "RECONCILED",
];

/**
 * done: the leg's records say it passed through the step. current: where it
 * is. ahead: not reached. skipped: passed without it (a release can finalize
 * without first being observed). unknown: the records needed to say are not
 * at hand.
 */
export type ProgressState = "done" | "current" | "ahead" | "skipped" | "unknown";

export interface ProgressStep {
  readonly status: BridgeLegStatus;
  readonly state: ProgressState;
}

/**
 * The progress drawn for a leg in `status`. `visited` is every state its
 * transitions name; without them (a list row, a command's answer) a step
 * before the current one is done, except DESTINATION_OBSERVED, which a leg may
 * skip and is then unknown. A FAILED or MANUAL_REVIEW leg shows the main path
 * as far as its transitions prove, then where it ended.
 */
export function legProgress(status: string, visited: readonly string[] | undefined): ProgressStep[] {
  const seen = visited === undefined ? undefined : new Set(visited);
  const at = LEG_MAIN_PATH.indexOf(status as BridgeLegStatus);
  if (at >= 0) {
    return LEG_MAIN_PATH.map((step, index) => {
      if (index === at) return { status: step, state: "current" };
      if (index > at) return { status: step, state: "ahead" };
      if (seen === undefined) return { status: step, state: step === "DESTINATION_OBSERVED" ? "unknown" : "done" };
      return { status: step, state: seen.has(step) ? "done" : "skipped" };
    });
  }
  if (status === "FAILED" || status === "MANUAL_REVIEW") {
    const path: ProgressStep[] = LEG_MAIN_PATH.map((step) => ({
      status: step,
      state: seen === undefined ? "unknown" : seen.has(step) ? "done" : "skipped",
    }));
    path.push({ status: status, state: "current" });
    return path;
  }
  // AVAILABLE and RESERVED come before the lock exists.
  return LEG_MAIN_PATH.map((step) => ({ status: step, state: "ahead" }));
}

export const PROGRESS_WORDS: Readonly<Record<ProgressState, string>> = {
  done: "passed",
  current: "now",
  ahead: "not reached",
  skipped: "not taken",
  unknown: "not shown here",
};

/* --------------------------------------------------------------------------
 * What a person typed
 * ------------------------------------------------------------------------ */

export type Parsed<T> = { readonly ok: true; readonly value: T } | { readonly ok: false; readonly error: string };

/** A destination symbol: 1..32 characters without surrounding whitespace. */
export function parseSymbol(raw: string): Parsed<string> {
  if (raw.length === 0 || raw.trim() !== raw || raw.length > 32) {
    return { ok: false, error: "A symbol is 1 to 32 characters with no space at either end." };
  }
  return { ok: true, value: raw };
}

/** The chain id a page parameter or a select names, when it is one the host runs. */
export function chainIdOf(raw: string | undefined): BridgeChainId | undefined {
  return BRIDGE_CHAIN_IDS.find((candidate) => candidate === raw);
}
