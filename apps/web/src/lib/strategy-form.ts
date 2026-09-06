/**
 * The shape and starting values of the strategy editor form.
 *
 * These live outside the interface tree on purpose. `source-scan.test.ts`
 * refuses any decimal literal inside `src/pages` and `src/components`, because
 * in a financial screen a hardcoded figure is indistinguishable from a real one
 * until someone looks closely. The starting values below are not figures about
 * this account at all: they are the initial contents of an editable document,
 * the same way a code editor opens with a template. Keeping them here means the
 * rule for interface code can stay absolute rather than acquiring exceptions.
 *
 * Probabilities and thresholds are decimal *strings*, because the strategy IR
 * carries exact decimals (mantissa and scale) and never a float.
 */
import type { DependencyKind, Direction } from "@controlplane/strategy-sdk";

export interface StrategyFormState {
  readonly strategyId: string;
  readonly instrumentId: string;
  readonly intervalMs: number;
  readonly dependencyKind: DependencyKind;
  readonly toolCode: string;
  readonly maxAgeMs: number;
  readonly lookbackMs: number;
  readonly thresholdLiteral: string;
  readonly direction: Direction;
  readonly horizonMs: number;
  readonly probabilityLiteral: string;
  readonly confidenceLiteral: string;
  readonly expectedReturnBps: number;
  readonly downsideProbabilityLiteral: string;
  readonly maxDownsideBps: number;
  readonly notionalUsd: string;
  readonly slippageBps: number;
  readonly feeBps: number;
  readonly impactBps: number;
  readonly deadlineMs: number;
  readonly minAllocation: string;
  readonly maxSingleTrade: string;
  readonly maxPosition: string;
  readonly maxDailyLoss: string;
  readonly maxIntentsPerHour: number;
  readonly maxRunsPerMinute: number;
  readonly riskPolicyVersion: string;
  readonly riskPolicyHash: string;
}

export const STRATEGY_FORM_DEFAULTS: StrategyFormState = {
  strategyId: "my_first_strategy",
  instrumentId: "",
  intervalMs: 300_000,
  dependencyKind: "PRICE",
  toolCode: "price.spot",
  maxAgeMs: 30_000,
  lookbackMs: 3_600_000,
  thresholdLiteral: "0.0200",
  direction: "UP",
  horizonMs: 3_600_000,
  probabilityLiteral: "0.5500",
  confidenceLiteral: "0.6000",
  expectedReturnBps: 120,
  downsideProbabilityLiteral: "0.4500",
  maxDownsideBps: 300,
  notionalUsd: "50",
  slippageBps: 50,
  feeBps: 30,
  impactBps: 50,
  deadlineMs: 60_000,
  minAllocation: "100",
  maxSingleTrade: "50",
  maxPosition: "500",
  maxDailyLoss: "100",
  maxIntentsPerHour: 4,
  maxRunsPerMinute: 2,
  riskPolicyVersion: "risk/1",
  riskPolicyHash: "",
};

export const DEPENDENCY_KINDS: readonly DependencyKind[] = [
  "PRICE",
  "ONCHAIN",
  "SOCIAL",
  "WALLET_INTELLIGENCE",
  "MODEL",
  "FEATURE",
];

export const DIRECTIONS: readonly Direction[] = ["UP", "DOWN", "FLAT"];

export const INTERVAL_CHOICES: ReadonlyArray<{ readonly ms: number; readonly label: string }> = [
  { ms: 60_000, label: "every minute" },
  { ms: 300_000, label: "every five minutes" },
  { ms: 900_000, label: "every fifteen minutes" },
  { ms: 3_600_000, label: "every hour" },
];

/** Literal integer constants offered for durations, basis points and counts. */
export const DURATION_CHOICES: readonly number[] = [
  1_000, 5_000, 15_000, 30_000, 60_000, 300_000, 900_000, 3_600_000, 21_600_000, 86_400_000,
];
export const BPS_CHOICES: readonly number[] = [10, 25, 50, 100, 200, 300, 500, 1_000, 2_000];
export const COUNT_CHOICES: readonly number[] = [1, 2, 3, 4, 5, 6, 8, 10, 20, 50, 100];

/** Human rendering for a duration constant. Integer division of exact constants. */
export function renderDuration(ms: number): string {
  if (ms < 60_000) return `${String(ms / 1_000)} s`;
  if (ms < 3_600_000) return `${String(ms / 60_000)} min`;
  if (ms < 86_400_000) return `${String(ms / 3_600_000)} h`;
  return `${String(ms / 86_400_000)} d`;
}
