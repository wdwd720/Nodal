import { deriveEffects, normalize } from "./normalize.ts";
import { semanticDocument, semanticHash } from "./hash.ts";
import type {
  Action,
  Condition,
  Dependency,
  Expr,
  IRDocument,
  InstrumentDecl,
  IntentSpec,
  ModelCall,
  PredictionSpec,
  Ref,
  RoundingMode,
  Signal,
  Trigger,
} from "./types.ts";
import { LIMITS, SCHEMA_VERSION, ZERO_TIME, usd } from "./types.ts";

export class StrategyBuildError extends Error {
  readonly issues: string[];
  constructor(issues: string[]) {
    super(`strategy-sdk: ${issues.join("; ")}`);
    this.name = "StrategyBuildError";
    this.issues = issues;
  }
}

/** What compile() returns. */
export interface CompiledStrategy {
  /** The normalized document, ready to send to the server. */
  document: IRDocument;
  /** Canonical JSON of the semantic projection (what the hash covers). */
  semanticJson: string;
  /** Lowercase hex sha256 that the server must reproduce. */
  hash: string;
}

export interface StrategyInit {
  strategyId: string;
  accountId: string;
  userId: string;
  riskPolicyVersion?: string;
  riskPolicyHash?: string;
  sdkVersion?: string;
}

/**
 * A typed builder whose only output is a schema-1 IR document.
 *
 * The builder does not decide what is allowed. It runs the same shape checks
 * the server does so a developer gets a fast local error, but the server's
 * validation is authoritative and identical from PARSE onward — an SDK
 * document earns no extra trust for having been written by hand.
 *
 * The effect set is derived, never declared by the caller: a strategy that
 * could name its own capabilities could name one it should not have.
 */
export class StrategyBuilder {
  #doc: IRDocument;

  constructor(init: StrategyInit) {
    this.#doc = {
      schema_version: SCHEMA_VERSION,
      strategy_id: init.strategyId,
      version: 0,
      hash: "",
      owner: { account_id: init.accountId, user_id: init.userId },
      instruments: [],
      triggers: [],
      dependencies: [],
      signals: [],
      conditions: [],
      actions: [],
      risk_policy: { version: init.riskPolicyVersion ?? "", hash: init.riskPolicyHash ?? "" },
      model_budget: {
        required: false,
        providers: [],
        max_calls_per_run: 0,
        max_calls_per_day: 0,
        max_input_tokens: 0,
        max_output_tokens: 0,
        max_spend_per_day: usd(0),
      },
      data_budget: {
        max_tool_calls_per_run: 0,
        max_tool_calls_per_day: 0,
        max_spend_per_day: usd(0),
        max_lookback_ms: 0,
      },
      envelope: {
        min_allocation: usd(0),
        max_single_trade: usd(0),
        max_position: usd(0),
        max_daily_loss: usd(0),
        instruments: [],
        asset_classes: [],
        venues: [],
        max_intents_per_hour: 0,
        max_runs_per_minute: 0,
      },
      effects: [],
      lineage: {
        source: "TYPESCRIPT_SDK",
        source_hash: "",
        compile_attempt_id: "",
        parent_version_id: "",
        compiler_version: "",
        sdk_version: init.sdkVersion ?? "",
      },
      built_at: ZERO_TIME,
    };
  }

  /** Declares an instrument the strategy may trade. */
  instrument(name: Ref, instrumentId: string): this {
    this.#doc.instruments.push({ name, instrument_id: instrumentId } satisfies InstrumentDecl);
    if (!this.#doc.envelope.instruments.includes(instrumentId)) {
      this.#doc.envelope.instruments.push(instrumentId);
    }
    return this;
  }

  /** Adds an interval trigger. */
  onInterval(name: Ref, everyMs: number, dedupWindowMs = everyMs): this {
    this.#doc.triggers.push({
      name,
      kind: "ON_INTERVAL",
      every_ms: everyMs,
      dedup_window_ms: dedupWindowMs,
    } satisfies Trigger);
    return this;
  }

  /** Adds an event trigger with an optional boolean filter. */
  onEvent(name: Ref, eventType: string, opts: { filter?: Expr; dedupWindowMs?: number } = {}): this {
    const trigger: Trigger = {
      name,
      kind: "ON_EVENT",
      event_type: eventType,
      dedup_window_ms: opts.dedupWindowMs ?? 0,
    };
    if (opts.filter) trigger.filter = opts.filter;
    this.#doc.triggers.push(trigger);
    return this;
  }

  /** Declares a data dependency with its staleness bound. */
  dependency(dep: Dependency): this {
    this.#doc.dependencies.push({ ...dep, params: dep.params ?? {} });
    return this;
  }

  /** Adds a computed signal. Signals may only reference earlier signals. */
  signal(name: Ref, expr: Expr, scale: number, rounding: RoundingMode = "half_even"): this {
    this.#doc.signals.push({ name, expr, scale, rounding } satisfies Signal);
    return this;
  }

  /** Adds a named boolean condition. */
  when(name: Ref, expr: Expr): this {
    this.#doc.conditions.push({ name, expr } satisfies Condition);
    return this;
  }

  /** Adds a model call action. */
  callModel(name: Ref, call: ModelCall, when?: Ref): this {
    const action: Action = { name, kind: "CALL_MODEL", model: call };
    if (when) action.when = when;
    this.#doc.actions.push(action);
    return this;
  }

  /** Adds a prediction commit. Every trade must be preceded by one. */
  predict(name: Ref, spec: PredictionSpec, when?: Ref): this {
    const action: Action = { name, kind: "COMMIT_PREDICTION", prediction: spec };
    if (when) action.when = when;
    this.#doc.actions.push(action);
    return this;
  }

  /** Adds a trade intent proposal. */
  intent(name: Ref, spec: IntentSpec, when?: Ref): this {
    const action: Action = { name, kind: "CREATE_TRADE_INTENT", intent: spec };
    if (when) action.when = when;
    this.#doc.actions.push(action);
    return this;
  }

  /** Sets the capital envelope requirements and rate limits. */
  envelope(envelope: Partial<IRDocument["envelope"]>): this {
    this.#doc.envelope = { ...this.#doc.envelope, ...envelope };
    return this;
  }

  /** Sets the data budget. */
  dataBudget(budget: Partial<IRDocument["data_budget"]>): this {
    this.#doc.data_budget = { ...this.#doc.data_budget, ...budget };
    return this;
  }

  /** Sets the model budget. */
  modelBudget(budget: Partial<IRDocument["model_budget"]>): this {
    this.#doc.model_budget = { ...this.#doc.model_budget, ...budget };
    return this;
  }

  /** Pins the risk policy the document was written against. */
  riskPolicy(version: string, hash: string): this {
    this.#doc.risk_policy = { version, hash };
    return this;
  }

  /** Returns the document as built, without normalizing or validating. */
  draft(): IRDocument {
    return structuredClone(this.#doc);
  }

  /**
   * Produces the canonical document and its semantic hash.
   *
   * The effect set is recomputed here rather than trusted: whatever the
   * caller thought the strategy could do, the document's own dependencies
   * and actions decide.
   */
  compile(): CompiledStrategy {
    const doc = normalize(structuredClone(this.#doc));
    doc.effects = deriveEffects(doc);

    const issues = validateShape(doc);
    if (issues.length > 0) {
      throw new StrategyBuildError(issues);
    }
    return {
      document: doc,
      semanticJson: semanticDocument(doc),
      hash: semanticHash(doc),
    };
  }
}

/**
 * Local shape checks. These mirror the server's STRUCTURAL stage closely
 * enough to catch a typo before a round trip, and deliberately no further:
 * duplicating the server's judgement here would create a second opinion
 * about what is allowed, and the two would drift.
 */
export function validateShape(doc: IRDocument): string[] {
  const issues: string[] = [];
  const push = (msg: string) => issues.push(msg);

  if (doc.schema_version !== SCHEMA_VERSION) push(`schema_version must be ${SCHEMA_VERSION}`);
  if (!doc.strategy_id) push("strategy_id is required");
  if (!doc.owner.account_id) push("owner.account_id is required");
  if (!doc.owner.user_id) push("owner.user_id is required");
  if (doc.triggers.length === 0) push("at least one trigger is required");
  if (doc.actions.length === 0) push("at least one action is required");
  if (doc.dependencies.length === 0) push("at least one dependency is required");
  if (doc.triggers.length > LIMITS.maxTriggers) push(`more than ${LIMITS.maxTriggers} triggers`);
  if (doc.signals.length > LIMITS.maxSignals) push(`more than ${LIMITS.maxSignals} signals`);
  if (doc.actions.length > LIMITS.maxActions) push(`more than ${LIMITS.maxActions} actions`);
  if (doc.dependencies.length > LIMITS.maxDependencies) {
    push(`more than ${LIMITS.maxDependencies} dependencies`);
  }

  const refPattern = /^[a-z][a-z0-9_]{0,63}$/;
  const seen = new Set<string>();
  const uniqueRef = (kind: string, name: string) => {
    if (!refPattern.test(name)) push(`${kind} name ${JSON.stringify(name)} must match ^[a-z][a-z0-9_]{0,63}$`);
    const key = `${kind}:${name}`;
    if (seen.has(key)) push(`duplicate ${kind} name ${JSON.stringify(name)}`);
    seen.add(key);
  };

  for (const t of doc.triggers) {
    uniqueRef("trigger", t.name);
    if (t.kind === "ON_INTERVAL") {
      if (t.every_ms === undefined) push(`trigger ${t.name} needs every_ms`);
      else if (t.every_ms < LIMITS.minIntervalMs) {
        push(`trigger ${t.name} fires faster than ${LIMITS.minIntervalMs} ms`);
      }
    } else if (!t.event_type) {
      push(`trigger ${t.name} needs an event_type`);
    }
  }

  for (const d of doc.dependencies) {
    uniqueRef("dependency", d.name);
    if (d.max_age_ms <= 0) push(`dependency ${d.name} needs a positive max_age_ms`);
    if (d.tool_version < 1) push(`dependency ${d.name} needs tool_version >= 1`);
    if (d.dependency_version < 1) push(`dependency ${d.name} needs dependency_version >= 1`);
  }

  const signalNames = new Set<string>();
  for (const s of doc.signals) {
    uniqueRef("signal", s.name);
    // A signal may only read signals declared before it: this is what makes
    // evaluation terminate, so it is checked here as well as on the server.
    const referenced = signalRefsOf(s.expr);
    for (const r of referenced) {
      if (!signalNames.has(r)) {
        push(`signal ${s.name} references ${r}, which is not an earlier signal`);
      }
    }
    signalNames.add(s.name);
  }

  const conditionNames = new Set<string>();
  for (const c of doc.conditions) {
    uniqueRef("condition", c.name);
    conditionNames.add(c.name);
  }

  const predictionActions = new Set<string>();
  for (const a of doc.actions) {
    uniqueRef("action", a.name);
    if (a.when && !conditionNames.has(a.when)) {
      push(`action ${a.name} references unknown condition ${a.when}`);
    }
    const specs = [a.model, a.prediction, a.intent].filter(Boolean).length;
    if (specs !== 1) push(`action ${a.name} must carry exactly one of model, prediction, intent`);
    if (a.kind === "COMMIT_PREDICTION") predictionActions.add(a.name);
    if (a.kind === "CREATE_TRADE_INTENT" && a.intent) {
      if (!predictionActions.has(a.intent.prediction)) {
        push(`action ${a.name} must name an earlier COMMIT_PREDICTION action`);
      }
      if (a.intent.deadline_ms <= 0) push(`action ${a.name} needs a positive deadline_ms`);
      checkSizing(a.name, a.intent, push);
    }
  }

  if (doc.envelope.max_runs_per_minute < 1) push("envelope.max_runs_per_minute must be >= 1");
  if (doc.actions.some((a) => a.kind === "CREATE_TRADE_INTENT") && doc.envelope.max_intents_per_hour < 1) {
    push("envelope.max_intents_per_hour must be >= 1 when the strategy proposes trades");
  }
  return issues;
}

function checkSizing(actionName: string, intent: IntentSpec, push: (m: string) => void): void {
  const s = intent.sizing;
  const set = [s.notional_usd, s.fraction_bps, s.target_usd].filter((v) => v !== undefined).length;
  switch (s.kind) {
    case "FIXED_NOTIONAL":
      if (s.notional_usd === undefined) push(`action ${actionName} needs sizing.notional_usd`);
      if (set !== 1) push(`action ${actionName}: FIXED_NOTIONAL takes only notional_usd`);
      break;
    case "ENVELOPE_FRACTION_BPS":
      if (s.fraction_bps === undefined) push(`action ${actionName} needs sizing.fraction_bps`);
      else if (s.fraction_bps <= 0 || s.fraction_bps > 10000) {
        push(`action ${actionName}: fraction_bps must be within 1..10000`);
      }
      if (set !== 1) push(`action ${actionName}: ENVELOPE_FRACTION_BPS takes only fraction_bps`);
      break;
    case "TARGET_EXPOSURE":
      if (s.target_usd === undefined) push(`action ${actionName} needs sizing.target_usd`);
      if (set !== 1) push(`action ${actionName}: TARGET_EXPOSURE takes only target_usd`);
      break;
    case "NONE":
      if (set !== 0) push(`action ${actionName}: NONE takes no amount`);
      break;
    default:
      push(`action ${actionName}: unknown sizing kind`);
  }
}

/** Collects every signal reference inside an expression tree. */
export function signalRefsOf(expr: Expr | undefined): string[] {
  if (!expr) return [];
  const out: string[] = [];
  if (expr.signal) out.push(expr.signal);
  if (expr.bin) out.push(...signalRefsOf(expr.bin.l), ...signalRefsOf(expr.bin.r));
  if (expr.cmp) out.push(...signalRefsOf(expr.cmp.l), ...signalRefsOf(expr.cmp.r));
  for (const child of expr.and ?? []) out.push(...signalRefsOf(child));
  for (const child of expr.or ?? []) out.push(...signalRefsOf(child));
  if (expr.not) out.push(...signalRefsOf(expr.not));
  return out;
}

/** Entry point mirroring `strategy()` in the design sketch. */
export function strategy(init: StrategyInit): StrategyBuilder {
  return new StrategyBuilder(init);
}
