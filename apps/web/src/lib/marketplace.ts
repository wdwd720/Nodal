/**
 * Pure helpers for the strategy marketplace section of the advanced area
 * (internal/marketplace, internal/sandboxrt).
 *
 * Nothing here computes a figure. These functions name lifecycle states, say
 * which action a state admits (so a control that cannot succeed is shown as
 * off, with its reason, rather than left to fail), and turn what a person
 * typed into the exact strings and whole numbers the API takes. The backend
 * decides everything again: a helper here that says "allowed" is a hint, and
 * the backend's answer, yes or no, is what the page shows.
 *
 * The enumerations repeat values the OpenAPI contract declares, tied to the
 * generated types at compile time (`Same`), as lib/advanced.ts does.
 */
import type { Schemas } from "@controlplane/generated-client";

import type { InstrumentKind, Tone } from "./advanced.ts";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export type StrategyStatus = Schemas["StrategyVersionStatus"];
export const STRATEGY_STATUSES = [
  "DRAFT",
  "SUBMITTED",
  "VALIDATED",
  "IN_REVIEW",
  "APPROVED",
  "REJECTED",
  "DEPRECATED",
] as const;
export type StrategyStatusesMatchContract = Assert<Same<(typeof STRATEGY_STATUSES)[number], StrategyStatus>>;

export type StrategyModeration = Schemas["StrategyModeration"];
export const STRATEGY_MODERATIONS = ["UNREVIEWED", "CLEARED", "FLAGGED", "TAKEN_DOWN"] as const;
export type StrategyModerationsMatchContract = Assert<
  Same<(typeof STRATEGY_MODERATIONS)[number], StrategyModeration>
>;

export type StrategyRiskClass = Schemas["StrategyRiskClass"];
export const STRATEGY_RISK_CLASSES = ["CONSERVATIVE", "MODERATE", "AGGRESSIVE", "SPECULATIVE"] as const;
export type StrategyRiskClassesMatchContract = Assert<
  Same<(typeof STRATEGY_RISK_CLASSES)[number], StrategyRiskClass>
>;

/** The only kinds a strategy may propose: taking and reducing exposure. */
export type StrategyIntentKind = Schemas["StrategyIntentKind"];
export const STRATEGY_INTENT_KINDS = ["ACQUIRE_EXPOSURE", "CLOSE_POSITION", "OPEN_POSITION", "REDUCE_EXPOSURE"] as const;
export type StrategyIntentKindsMatchContract = Assert<
  Same<(typeof STRATEGY_INTENT_KINDS)[number], StrategyIntentKind>
>;

export const VALIDATION_OUTCOMES = ["PASSED", "FAILED"] as const;
export type ValidationOutcomesMatchContract = Assert<
  Same<(typeof VALIDATION_OUTCOMES)[number], Schemas["StrategyValidationOutcome"]>
>;

export const BUILD_FAILURES = ["SCAN_REJECTED", "COMPILE_FAILED", "BUILD_TIMEOUT"] as const;
export type BuildFailuresMatchContract = Assert<Same<(typeof BUILD_FAILURES)[number], Schemas["StrategyBuildFailure"]>>;

export const REVIEW_DECISIONS = ["APPROVE", "REJECT"] as const;
export type ReviewDecisionsMatchContract = Assert<
  Same<(typeof REVIEW_DECISIONS)[number], Schemas["StrategyReviewDecision"]>
>;

export const INSTALL_STATUSES = ["ACTIVE", "REMOVED"] as const;
export type InstallStatusesMatchContract = Assert<
  Same<(typeof INSTALL_STATUSES)[number], Schemas["StrategyInstallStatus"]>
>;

/** The only mode an install can have; the backend refuses any other. */
export const INSTALL_MODE = "PAPER";

export const FOLLOW_TRIGGERS = ["EXPLICIT", "SCHEDULED"] as const;
export type FollowTriggersMatchContract = Assert<
  Same<(typeof FOLLOW_TRIGGERS)[number], Schemas["StrategyFollowTrigger"]>
>;

export type FollowOutcome = Schemas["StrategyFollowOutcome"];
export const FOLLOW_OUTCOMES = [
  "DISPATCHED",
  "RISK_REJECTED",
  "ROUTE_REFUSED",
  "DISPATCH_FAILED",
  "DISPATCH_UNKNOWN",
] as const;
export type FollowOutcomesMatchContract = Assert<Same<(typeof FOLLOW_OUTCOMES)[number], FollowOutcome>>;

export type RunTermination = Schemas["StrategyRunTermination"];
export const RUN_TERMINATIONS = [
  "OK",
  "TIMEOUT",
  "MEMORY_LIMIT",
  "OUTPUT_LIMIT",
  "TRAP",
  "INVALID_OUTPUT",
  "KILLED_BY_SWITCH",
  "INVALID_MODULE",
] as const;
export type RunTerminationsMatchContract = Assert<Same<(typeof RUN_TERMINATIONS)[number], RunTermination>>;

export const RUN_SOURCES = ["FOLLOW", "SIMULATION"] as const;
export type RunSourcesMatchContract = Assert<Same<(typeof RUN_SOURCES)[number], Schemas["StrategyRunSource"]>>;

export const EVALUATION_KINDS = ["SIMULATION"] as const;
export type EvaluationKindsMatchContract = Assert<
  Same<(typeof EVALUATION_KINDS)[number], Schemas["StrategyEvaluationKind"]>
>;

/* --------------------------------------------------------------------------
 * What the states mean
 * ------------------------------------------------------------------------ */

export const STATUS_MEANING: Readonly<Record<StrategyStatus, string>> = {
  DRAFT: "Written by its creator and not yet submitted. Nobody else can see it.",
  SUBMITTED: "Submitted for validation. An operator scans and builds it next.",
  VALIDATED: "The source passed the scan and built into a sandbox artifact. It waits for review.",
  IN_REVIEW: "An operator who is not its creator is reviewing it.",
  APPROVED: "Approved by an operator who is not its creator. It can be installed in PAPER mode once cleared.",
  REJECTED: "Refused, with the code that says why. A new version starts again at DRAFT.",
  DEPRECATED: "Retired: no new installs, and existing installs can no longer be followed.",
};

export const MODERATION_MEANING: Readonly<Record<StrategyModeration, string>> = {
  UNREVIEWED: "Not yet cleared. Approval clears it.",
  CLEARED: "Cleared for installs.",
  FLAGGED: "Flagged by an operator: it cannot be installed or followed.",
  TAKEN_DOWN: "Taken down by an operator: it cannot be installed or followed, and only its creator and operators see it.",
};

export const TERMINATION_MEANING: Readonly<Record<RunTermination, string>> = {
  OK: "The strategy exited normally and its output was read.",
  TIMEOUT: "The wall-time limit expired and the module was closed.",
  MEMORY_LIMIT: "The module asked for more memory than its limit and was refused.",
  OUTPUT_LIMIT: "The module wrote more than its output limit and was stopped.",
  TRAP: "The module stopped abnormally (a trap, a panic or a non-zero exit).",
  INVALID_OUTPUT: "The module wrote something other than intent and log records, so nothing it wrote was used.",
  KILLED_BY_SWITCH: "A kill switch for user code was active, so the module was never started.",
  INVALID_MODULE: "The artifact was refused before it ran.",
};

export const OUTCOME_MEANING: Readonly<Record<FollowOutcome, string>> = {
  DISPATCHED: "Risk Kernel V2 allowed it and paper execution accepted it.",
  RISK_REJECTED: "Risk Kernel V2 refused it; the reason codes say why.",
  ROUTE_REFUSED: "The settlement compiler refused to route it.",
  DISPATCH_FAILED: "Risk Kernel V2 allowed it and paper execution refused it; the failure code says why.",
  DISPATCH_UNKNOWN: "Paper execution did not report an outcome. It is reconciled, never retried.",
};

export function statusTone(status: string): Tone {
  switch (status) {
    case "APPROVED":
      return "good";
    case "REJECTED":
      return "bad";
    case "DEPRECATED":
      return "neutral";
    default:
      return "info";
  }
}

export function moderationTone(moderation: string): Tone {
  switch (moderation) {
    case "CLEARED":
      return "good";
    case "FLAGGED":
      return "warn";
    case "TAKEN_DOWN":
      return "bad";
    default:
      return "neutral";
  }
}

export function outcomeTone(outcome: string): Tone {
  switch (outcome) {
    case "DISPATCHED":
      return "good";
    case "RISK_REJECTED":
    case "ROUTE_REFUSED":
    case "DISPATCH_FAILED":
      return "bad";
    default:
      return "warn";
  }
}

export function terminationTone(termination: string): Tone {
  return termination === "OK" ? "good" : termination === "KILLED_BY_SWITCH" ? "warn" : "bad";
}

/* --------------------------------------------------------------------------
 * What a version admits
 * ------------------------------------------------------------------------ */

interface VersionState {
  readonly status: string;
  readonly moderation: string;
}

/** Public: reviewed (APPROVED, or DEPRECATED after it) and not taken down. */
export function isPublicVersion(v: VersionState): boolean {
  return (v.status === "APPROVED" || v.status === "DEPRECATED") && v.moderation !== "TAKEN_DOWN";
}

/** Only an APPROVED, CLEARED version can be installed. */
export function canInstall(v: VersionState): boolean {
  return v.status === "APPROVED" && v.moderation === "CLEARED";
}

/** A fork names an APPROVED or DEPRECATED parent. */
export function canFork(v: VersionState): boolean {
  return v.status === "APPROVED" || v.status === "DEPRECATED";
}

/** A creator submits a DRAFT. */
export function canSubmit(v: VersionState): boolean {
  return v.status === "DRAFT";
}

/** Only a built version has an artifact to simulate. */
export function canSimulate(v: VersionState): boolean {
  return v.status === "VALIDATED" || v.status === "IN_REVIEW" || v.status === "APPROVED" || v.status === "DEPRECATED";
}

export type OperatorAction = "validate" | "review" | "approve" | "reject" | "moderate" | "deprecate";

/** The operator decisions a version's state admits, in the order they happen. */
export function operatorActionsFor(v: VersionState): OperatorAction[] {
  switch (v.status) {
    case "SUBMITTED":
      return ["validate", "reject"];
    case "VALIDATED":
      return ["review", "reject"];
    case "IN_REVIEW":
      return ["approve", "reject"];
    case "APPROVED":
      return ["moderate", "deprecate"];
    case "DEPRECATED":
      return v.moderation === "TAKEN_DOWN" ? [] : ["moderate"];
    default:
      return [];
  }
}

/** Where a version's moderation may go from where it is. */
export function moderationTargets(moderation: string): StrategyModeration[] {
  switch (moderation) {
    case "UNREVIEWED":
      return ["CLEARED", "FLAGGED", "TAKEN_DOWN"];
    case "CLEARED":
      return ["FLAGGED", "TAKEN_DOWN"];
    case "FLAGGED":
      return ["CLEARED", "TAKEN_DOWN"];
    default:
      return [];
  }
}

/* --------------------------------------------------------------------------
 * Reading what a person typed
 * ------------------------------------------------------------------------ */

export type Parsed<T> = { readonly ok: true; readonly value: T } | { readonly ok: false; readonly error: string };

const DIGITS = "0123456789";

/**
 * A whole number between min and max, from its decimal digits. It is built
 * digit by digit from at most nine of them, so it is exact; it is a count or
 * a duration, never an amount.
 */
export function parseWholeNumber(raw: string, min: number, max: number, what: string): Parsed<number> {
  const text = raw.trim();
  if (!/^[0-9]{1,9}$/.test(text)) {
    return { ok: false, error: `${what} is a whole number written in digits.` };
  }
  let value = 0;
  for (const ch of text) {
    value = value * 10 + DIGITS.indexOf(ch);
  }
  if (value < min || value > max) {
    return { ok: false, error: `${what} must be between ${String(min)} and ${String(max)}.` };
  }
  return { ok: true, value };
}

/**
 * A positive decimal exactly as the manifest stores it: digits, an optional
 * fraction, no sign, no leading zero, no exponent. The string is kept as
 * typed; it is never converted to a number.
 */
export function parsePositiveDecimal(raw: string, what: string): Parsed<string> {
  const text = raw.trim();
  if (!/^(0|[1-9][0-9]*)(\.[0-9]+)?$/.test(text)) {
    return { ok: false, error: `${what} is a decimal such as a whole number of simulated dollars, with no sign or grouping.` };
  }
  if (/^0(\.0+)?$/.test(text)) {
    return { ok: false, error: `${what} must be more than zero.` };
  }
  return { ok: true, value: text };
}

/** One entry per non-empty line (or comma-separated), trimmed. */
export function parseList(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((part) => part.trim())
    .filter((part) => part !== "");
}

const CANONICAL_UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** Instrument ids, one per line, in canonical lower-case form. */
export function parseInstrumentIds(raw: string): Parsed<string[]> {
  const ids = parseList(raw).map((id) => id.toLowerCase());
  const bad = ids.find((id) => !CANONICAL_UUID.test(id));
  if (bad !== undefined) {
    return { ok: false, error: `"${bad}" is not an instrument id in its canonical 36-character form.` };
  }
  return { ok: true, value: ids };
}

/** `key=value` lines; keys are lower-case names. */
export function parseConfig(raw: string): Parsed<Record<string, string>> {
  const out: Record<string, string> = {};
  for (const line of raw.split("\n")) {
    const text = line.trim();
    if (text === "") continue;
    const cut = text.indexOf("=");
    if (cut <= 0) {
      return { ok: false, error: `"${text}" is not a key=value line.` };
    }
    const key = text.slice(0, cut).trim();
    if (!/^[a-z][a-z0-9_.-]{0,63}$/.test(key)) {
      return { ok: false, error: `"${key}" is not a configuration key: lower-case letters, digits, _ . and -.` };
    }
    if (key in out) {
      return { ok: false, error: `"${key}" appears twice.` };
    }
    out[key] = text.slice(cut + 1).trim();
  }
  return { ok: true, value: out };
}

/** A review's reason code: [A-Z][A-Z0-9_]{1,63}. */
export function isReasonCode(raw: string): boolean {
  return /^[A-Z][A-Z0-9_]{1,63}$/.test(raw);
}

/** A plain Go file name the scanner accepts. */
export function isSourceFileName(raw: string): boolean {
  return /^[a-z0-9_]{1,64}\.go$/.test(raw) && !raw.endsWith("_test.go");
}

/* --------------------------------------------------------------------------
 * A version's content, as the form holds it and as the API takes it
 * ------------------------------------------------------------------------ */

export interface VersionDraft {
  readonly description: string;
  readonly riskClass: StrategyRiskClass;
  readonly sourceCommit: string;
  readonly fileName: string;
  /** Empty means "no source"; a fork then takes the parent's. */
  readonly source: string;
  readonly intentKinds: readonly StrategyIntentKind[];
  readonly instrumentIds: string;
  readonly instrumentKinds: readonly InstrumentKind[];
  readonly maxNotional: string;
  readonly maxIntentsPerRun: string;
  readonly minIntervalSeconds: string;
  readonly supportedInstrumentIds: string;
  readonly dataDependencies: string;
  readonly performanceFeeBps: string;
  readonly managementFeeBps: string;
  readonly highWaterMark: boolean;
  readonly config: string;
  readonly backtestEvidenceRef: string;
}

export const EMPTY_VERSION_DRAFT: VersionDraft = {
  description: "",
  riskClass: "MODERATE",
  sourceCommit: "",
  fileName: "main.go",
  source: "",
  intentKinds: [],
  instrumentIds: "",
  instrumentKinds: [],
  maxNotional: "",
  maxIntentsPerRun: "",
  minIntervalSeconds: "",
  supportedInstrumentIds: "",
  dataDependencies: "",
  performanceFeeBps: "",
  managementFeeBps: "",
  highWaterMark: false,
  config: "",
  backtestEvidenceRef: "",
};

export type VersionInput = Schemas["StrategyVersionInput"];
export type ManifestInput = Schemas["StrategyManifestInput"];

/** True when nothing of a manifest was filled in: a fork then takes its parent's. */
export function manifestLeftOut(d: VersionDraft): boolean {
  return (
    d.intentKinds.length === 0 &&
    d.instrumentIds.trim() === "" &&
    d.instrumentKinds.length === 0 &&
    d.maxNotional.trim() === "" &&
    d.maxIntentsPerRun.trim() === "" &&
    d.minIntervalSeconds.trim() === ""
  );
}

/**
 * Builds the request body of a version, or says what is missing. With
 * `inherit` (a fork) an empty source and an empty manifest are left out so
 * the backend takes the parent's.
 */
export function buildVersionInput(d: VersionDraft, inherit: boolean): Parsed<VersionInput> {
  const problems: string[] = [];
  if (d.description.trim() === "") problems.push("Describe what the strategy does.");
  if (d.sourceCommit.trim() === "") problems.push("Name the commit the source was taken from.");
  const out: { -readonly [K in keyof VersionInput]: VersionInput[K] } = {
    description: d.description.trim(),
    risk_class: d.riskClass,
    source_commit: d.sourceCommit.trim(),
  };

  if (d.source.trim() !== "") {
    if (!isSourceFileName(d.fileName.trim())) {
      problems.push("The file name is a plain Go file name such as main.go.");
    }
    out.source = { [d.fileName.trim()]: d.source };
  } else if (!inherit) {
    problems.push("Paste the strategy's source.");
  }

  if (!(inherit && manifestLeftOut(d))) {
    const manifest = buildManifest(d);
    if (manifest.ok) {
      out.manifest = manifest.value;
    } else {
      problems.push(manifest.error);
    }
  }

  const supported = parseInstrumentIds(d.supportedInstrumentIds);
  if (!supported.ok) problems.push(supported.error);
  else if (supported.value.length > 0) out.supported_instrument_ids = supported.value;

  const deps = parseList(d.dataDependencies);
  if (deps.length > 0) out.data_dependencies = deps;

  if (d.performanceFeeBps.trim() !== "" || d.managementFeeBps.trim() !== "" || d.highWaterMark) {
    const perf = parseWholeNumber(d.performanceFeeBps === "" ? "0" : d.performanceFeeBps, 0, 10000, "The performance fee");
    const mgmt = parseWholeNumber(d.managementFeeBps === "" ? "0" : d.managementFeeBps, 0, 10000, "The management fee");
    if (!perf.ok) problems.push(perf.error);
    if (!mgmt.ok) problems.push(mgmt.error);
    if (perf.ok && mgmt.ok) {
      out.fees = { performance_fee_bps: perf.value, management_fee_bps: mgmt.value, high_water_mark: d.highWaterMark };
    }
  }

  const config = parseConfig(d.config);
  if (!config.ok) problems.push(config.error);
  else if (Object.keys(config.value).length > 0) out.config = config.value;

  if (d.backtestEvidenceRef.trim() !== "") out.backtest_evidence_ref = d.backtestEvidenceRef.trim();

  if (problems.length > 0) return { ok: false, error: problems.join(" ") };
  return { ok: true, value: out };
}

function buildManifest(d: VersionDraft): Parsed<ManifestInput> {
  const problems: string[] = [];
  if (d.intentKinds.length === 0) problems.push("Allow at least one intent kind.");
  const ids = parseInstrumentIds(d.instrumentIds);
  if (!ids.ok) problems.push(ids.error);
  if (ids.ok && ids.value.length === 0 && d.instrumentKinds.length === 0) {
    problems.push("Allow at least one instrument or instrument kind.");
  }
  const notional = parsePositiveDecimal(d.maxNotional, "The notional cap per intent");
  if (!notional.ok) problems.push(notional.error);
  const perRun = parseWholeNumber(d.maxIntentsPerRun, 1, 100, "Intents per run");
  if (!perRun.ok) problems.push(perRun.error);
  const interval = parseWholeNumber(d.minIntervalSeconds, 60, 31_536_000, "The interval between runs, in seconds,");
  if (!interval.ok) problems.push(interval.error);
  if (problems.length > 0 || !ids.ok || !notional.ok || !perRun.ok || !interval.ok) {
    return { ok: false, error: problems.join(" ") };
  }
  return {
    ok: true,
    value: {
      allowed_intent_kinds: [...d.intentKinds],
      allowed_instrument_ids: ids.value,
      allowed_instrument_kinds: [...d.instrumentKinds],
      max_notional_per_intent: notional.value,
      max_intents_per_run: perRun.value,
      min_interval_seconds: interval.value,
    },
  };
}

/** Adds or removes one value of a closed set, keeping the contract's order. */
export function toggle<T extends string>(order: readonly T[], selected: readonly T[], value: T): T[] {
  const set = new Set<T>(selected);
  if (set.has(value)) set.delete(value);
  else set.add(value);
  return order.filter((item) => set.has(item));
}

/* --------------------------------------------------------------------------
 * A market snapshot, as pasted
 * ------------------------------------------------------------------------ */

export type Snapshot = Schemas["StrategySnapshot"];

/** The most snapshots one simulation request may carry. */
export const MAX_SIMULATION_SNAPSHOTS = 20;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseJson(raw: string, what: string): Parsed<unknown> {
  try {
    return { ok: true, value: JSON.parse(raw) };
  } catch {
    return { ok: false, error: `${what} is not valid JSON.` };
  }
}

/** Checks one snapshot's shape and stamps its time. */
function snapshotOf(value: unknown, asOf: string): Parsed<Snapshot> {
  if (!isRecord(value)) return { ok: false, error: "A snapshot is a JSON object with instruments and a portfolio." };
  const instruments = value["instruments"];
  const portfolio = value["portfolio"];
  if (!Array.isArray(instruments)) return { ok: false, error: "A snapshot needs an instruments array." };
  if (!isRecord(portfolio)) return { ok: false, error: "A snapshot needs a portfolio object." };
  if (typeof portfolio["balance"] !== "string") {
    return { ok: false, error: "The portfolio's balance is a decimal string of simulated dollars." };
  }
  if (!Array.isArray(portfolio["positions"])) return { ok: false, error: "The portfolio needs a positions array." };
  return {
    ok: true,
    value: {
      as_of: asOf,
      instruments: instruments as Snapshot["instruments"],
      portfolio: portfolio as Snapshot["portfolio"],
    },
  };
}

/**
 * The instruments and portfolio a strategy runs on, pasted as JSON, stamped
 * with the moment of confirmation. Only the shape is checked here: every
 * figure stays the string it was typed as, and the backend validates the
 * document exactly as the strategy will read it.
 */
export function parseSnapshot(raw: string, asOf: string): Parsed<Snapshot> {
  const parsed = parseJson(raw, "The snapshot");
  return parsed.ok ? snapshotOf(parsed.value, asOf) : parsed;
}

/**
 * A simulation's fixed snapshot set: a JSON array of snapshots, each with the
 * historical as_of it describes, in the order they are run.
 */
export function parseSnapshotSet(raw: string): Parsed<Snapshot[]> {
  const parsed = parseJson(raw, "The snapshot set");
  if (!parsed.ok) return parsed;
  if (!Array.isArray(parsed.value) || parsed.value.length === 0) {
    return { ok: false, error: "The snapshot set is a JSON array of one or more snapshots." };
  }
  if (parsed.value.length > MAX_SIMULATION_SNAPSHOTS) {
    return { ok: false, error: `A simulation request carries at most ${String(MAX_SIMULATION_SNAPSHOTS)} snapshots.` };
  }
  const out: Snapshot[] = [];
  for (const [index, item] of parsed.value.entries()) {
    const asOf = isRecord(item) ? item["as_of"] : undefined;
    if (typeof asOf !== "string" || asOf === "") {
      return { ok: false, error: `Snapshot ${String(index + 1)} needs the as_of time it describes.` };
    }
    const one = snapshotOf(item, asOf);
    if (!one.ok) return { ok: false, error: `Snapshot ${String(index + 1)}: ${one.error}` };
    out.push(one.value);
  }
  return { ok: true, value: out };
}
