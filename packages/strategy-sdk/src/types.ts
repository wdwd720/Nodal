/**
 * The strategy IR, mirroring internal/strategy/ir in Go.
 *
 * These types use the wire (snake_case) field names deliberately. A camelCase
 * TypeScript shape with a mapping layer would be one more place for the two
 * languages to drift, and the whole point of this package is that they do not.
 *
 * Numbers here are integers only. Money is a decimal string with two places
 * ("50.00"); every other fractional quantity is a Decimal object.
 */

export const SCHEMA_VERSION = 1;

/** Structural bounds, mirroring the Go constants. */
export const LIMITS = {
  maxTriggers: 8,
  maxSignals: 64,
  maxConditions: 64,
  maxActions: 16,
  maxDependencies: 32,
  maxInstruments: 16,
  maxExprDepth: 8,
  maxExprNodes: 256,
  minIntervalMs: 1000,
  maxScale: 18,
  probabilityScale: 4,
} as const;

/** value = m x 10^-s. The mantissa is a decimal integer string, never a float. */
export interface Decimal {
  m: string;
  s: number;
}

/** A probability is a Decimal with s = 4 and a value in [0, 1]. */
export type Probability = Decimal;

/** USD as a two-decimal string, e.g. "50.00". */
export type USD = string;

/** Lowercase hex, or "" when absent. */
export type Hex = string;

export type Ref = string;

export type TriggerKind = "ON_EVENT" | "ON_INTERVAL";

export type DependencyKind =
  | "PRICE"
  | "ONCHAIN"
  | "WALLET_EVENT"
  | "SOCIAL"
  | "WALLET_INTELLIGENCE"
  | "MODEL"
  | "FEATURE";

export type Effect =
  | "READ_MARKET_DATA"
  | "READ_ONCHAIN_DATA"
  | "READ_APPROVED_SOCIAL_DATA"
  | "READ_WALLET_INTELLIGENCE"
  | "CALL_MODEL"
  | "COMMIT_PREDICTION"
  | "CREATE_TRADE_INTENT";

/** Reserved names that are always rejected. Declared so the SDK can name them. */
export type ForbiddenEffect =
  | "RAW_SIGN"
  | "TRANSFER_VALUE"
  | "WITHDRAW"
  | "CHANGE_RISK"
  | "CHANGE_CAPITAL"
  | "EXPORT_SECRET"
  | "ARBITRARY_NETWORK"
  | "ARBITRARY_CONTRACT_CALL"
  | "MODIFY_CAPABILITY_GATE"
  | "ACCESS_ADMIN_API";

export const ALLOWED_EFFECTS: readonly Effect[] = [
  "CALL_MODEL",
  "COMMIT_PREDICTION",
  "CREATE_TRADE_INTENT",
  "READ_APPROVED_SOCIAL_DATA",
  "READ_MARKET_DATA",
  "READ_ONCHAIN_DATA",
  "READ_WALLET_INTELLIGENCE",
];

export const FORBIDDEN_EFFECTS: readonly ForbiddenEffect[] = [
  "ACCESS_ADMIN_API",
  "ARBITRARY_CONTRACT_CALL",
  "ARBITRARY_NETWORK",
  "CHANGE_CAPITAL",
  "CHANGE_RISK",
  "EXPORT_SECRET",
  "MODIFY_CAPABILITY_GATE",
  "RAW_SIGN",
  "TRANSFER_VALUE",
  "WITHDRAW",
];

export type BinOpKind = "ADD" | "SUB" | "MUL" | "DIV" | "MIN" | "MAX";
export type CmpOp = "LT" | "LE" | "GT" | "GE" | "EQ" | "NE";
export type WindowFn = "SMA" | "EMA" | "MAX" | "MIN" | "SUM" | "COUNT" | "RETURN" | "STDDEV";
export type RoundingMode = "down" | "up" | "half_even" | "half_up" | "floor" | "ceil" | "exact";

export type ActionKind = "CALL_MODEL" | "COMMIT_PREDICTION" | "CREATE_TRADE_INTENT";
export type Direction = "UP" | "DOWN" | "FLAT";
export type IntentAction =
  | "ACQUIRE_NOTIONAL"
  | "REDUCE_NOTIONAL"
  | "CLOSE_POSITION"
  | "TARGET_EXPOSURE";
export type SizingKind =
  | "NONE"
  | "FIXED_NOTIONAL"
  | "ENVELOPE_FRACTION_BPS"
  | "TARGET_EXPOSURE";
export type LineageSource = "NATURAL_LANGUAGE" | "TYPESCRIPT_SDK" | "CLONE";

export interface FieldRef {
  dependency: Ref;
  path: string;
  scale: number;
}

export interface BinOp {
  op: BinOpKind;
  l: Expr;
  r: Expr;
  scale: number;
  rounding: RoundingMode;
}

export interface WindowOp {
  fn: WindowFn;
  dependency: Ref;
  path: string;
  lookback_ms: number;
  scale: number;
  rounding: RoundingMode;
}

export interface Cmp {
  op: CmpOp;
  l: Expr;
  r: Expr;
}

/** A tagged union: exactly one member is set. */
export interface Expr {
  const?: Decimal;
  field?: FieldRef;
  signal?: Ref;
  bin?: BinOp;
  window?: WindowOp;
  cmp?: Cmp;
  and?: Expr[];
  or?: Expr[];
  not?: Expr;
}

export interface Owner {
  account_id: string;
  user_id: string;
}

export interface InstrumentDecl {
  name: Ref;
  instrument_id: string;
}

export interface Trigger {
  name: Ref;
  kind: TriggerKind;
  event_type?: string;
  filter?: Expr;
  every_ms?: number;
  dedup_window_ms: number;
}

export interface Dependency {
  name: Ref;
  kind: DependencyKind;
  tool_code: string;
  tool_version: number;
  dependency_version: number;
  params: Record<string, string>;
  max_age_ms: number;
  required: boolean;
}

export interface Signal {
  name: Ref;
  expr: Expr;
  scale: number;
  rounding: RoundingMode;
}

export interface Condition {
  name: Ref;
  expr: Expr;
}

export interface ModelCall {
  template_version: string;
  output_schema: Ref;
  inputs: Ref[];
  max_output_tokens: number;
  required: boolean;
}

export interface PredictionSpec {
  instrument: Ref;
  horizon_ms: number;
  direction: Direction;
  probability: Expr;
  expected_return_bps: Expr;
  downside_probability: Expr;
  max_downside_bps: Expr;
  confidence: Expr;
}

export interface Sizing {
  kind: SizingKind;
  notional_usd?: USD;
  fraction_bps?: number;
  target_usd?: USD;
}

export interface IntentConstraints {
  max_slippage_bps: number;
  max_fee_bps: number;
  max_price_impact_bps: number;
  quote_freshness_ms: number;
  allowed_venues: string[];
}

export interface IntentSpec {
  action: IntentAction;
  instrument: Ref;
  sizing: Sizing;
  constraints: IntentConstraints;
  deadline_ms: number;
  prediction: Ref;
}

export interface Action {
  name: Ref;
  kind: ActionKind;
  when?: Ref;
  model?: ModelCall;
  prediction?: PredictionSpec;
  intent?: IntentSpec;
}

export interface RiskPolicyRef {
  version: string;
  hash: Hex;
}

export interface ModelBudget {
  required: boolean;
  providers: string[];
  max_calls_per_run: number;
  max_calls_per_day: number;
  max_input_tokens: number;
  max_output_tokens: number;
  max_spend_per_day: USD;
}

export interface DataBudget {
  max_tool_calls_per_run: number;
  max_tool_calls_per_day: number;
  max_spend_per_day: USD;
  max_lookback_ms: number;
}

export interface EnvelopeRequirements {
  min_allocation: USD;
  max_single_trade: USD;
  max_position: USD;
  max_daily_loss: USD;
  instruments: string[];
  asset_classes: string[];
  venues: string[];
  max_intents_per_hour: number;
  max_runs_per_minute: number;
}

export interface Lineage {
  source: LineageSource | "";
  source_hash: Hex;
  compile_attempt_id: string;
  parent_version_id: string;
  compiler_version: string;
  sdk_version: string;
}

/** One schema-1 IR document, in its exact wire shape. */
export interface IRDocument {
  schema_version: number;
  strategy_id: string;
  version: number;
  hash: Hex;
  owner: Owner;
  instruments: InstrumentDecl[];
  triggers: Trigger[];
  dependencies: Dependency[];
  signals: Signal[];
  conditions: Condition[];
  actions: Action[];
  risk_policy: RiskPolicyRef;
  model_budget: ModelBudget;
  data_budget: DataBudget;
  envelope: EnvelopeRequirements;
  effects: Effect[];
  lineage: Lineage;
  built_at: string;
}

/** The zero time Go renders for an unset built_at. */
export const ZERO_TIME = "0001-01-01T00:00:00Z";

/** Builds a Decimal from a plain decimal string, e.g. "0.0200" -> {m:"200",s:4}. */
export function decimal(literal: string): Decimal {
  const trimmed = literal.trim();
  const match = /^([+-]?)(\d+)(?:\.(\d+))?$/.exec(trimmed);
  if (!match) {
    throw new Error(`strategy-sdk: ${JSON.stringify(literal)} is not a decimal literal`);
  }
  const sign = match[1] === "-" ? "-" : "";
  const intPart = match[2] as string;
  const fracPart = match[3] ?? "";
  if (fracPart.length > LIMITS.maxScale) {
    throw new Error(`strategy-sdk: more than ${LIMITS.maxScale} fractional digits in ${literal}`);
  }
  let digits = (intPart + fracPart).replace(/^0+/, "");
  if (digits === "") digits = "0";
  return { m: digits === "0" ? "0" : sign + digits, s: fracPart.length };
}

/** Renders a Decimal the way Go's Decimal.String does. */
export function decimalToString(d: Decimal): string {
  const negative = d.m.startsWith("-");
  let digits = negative ? d.m.slice(1) : d.m;
  if (d.s === 0) return (negative ? "-" : "") + digits;
  if (digits.length <= d.s) {
    digits = "0".repeat(d.s - digits.length + 1) + digits;
  }
  const out = `${digits.slice(0, digits.length - d.s)}.${digits.slice(digits.length - d.s)}`;
  return (negative ? "-" : "") + out;
}

/** Formats USD minor units as the two-decimal string the wire format uses. */
export function usd(minorUnits: number): USD {
  if (!Number.isInteger(minorUnits)) {
    throw new Error("strategy-sdk: USD is expressed in whole minor units (cents)");
  }
  const negative = minorUnits < 0;
  const abs = Math.abs(minorUnits);
  const whole = Math.floor(abs / 100);
  const cents = abs % 100;
  return `${negative ? "-" : ""}${whole}.${String(cents).padStart(2, "0")}`;
}
