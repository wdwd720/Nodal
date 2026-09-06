/**
 * @controlplane/strategy-sdk
 *
 * A typed builder that emits strategy IR documents, and the semantic hash
 * that lets the server recognise a document it has already compiled.
 *
 * This package never executes a strategy and never talks to the platform.
 * Its only output is a JSON document; the server validates it from PARSE
 * onward exactly as it validates a natural-language compilation, so nothing
 * here is a security boundary. The local checks exist to give a developer a
 * fast error, not to decide what is permitted.
 *
 * The hash implementation is a cross-language contract with
 * internal/strategy/ir in Go. See src/hash.ts for the algorithm and
 * src/parity.test.ts for the shared fixtures that keep the two honest.
 */

export {
  canonicalize,
  canonicalNumber,
  canonicalString,
  compareUtf8,
  CanonicalJsonError,
  MAX_SAFE_INTEGER_MAGNITUDE,
  type JsonValue,
} from "./canonical.ts";

export {
  canonicalDocument,
  semanticDocument,
  semanticHash,
  sortEffects,
  SEMANTIC_EXCLUDED_KEYS,
} from "./hash.ts";

export {
  deriveEffects,
  effectOfAction,
  effectOfDependency,
  normalize,
} from "./normalize.ts";

export {
  strategy,
  signalRefsOf,
  validateShape,
  StrategyBuilder,
  StrategyBuildError,
  type CompiledStrategy,
  type StrategyInit,
} from "./builder.ts";

export {
  decimal,
  decimalToString,
  usd,
  ALLOWED_EFFECTS,
  FORBIDDEN_EFFECTS,
  LIMITS,
  SCHEMA_VERSION,
  ZERO_TIME,
  type Action,
  type ActionKind,
  type BinOp,
  type BinOpKind,
  type Cmp,
  type CmpOp,
  type Condition,
  type DataBudget,
  type Decimal,
  type Dependency,
  type DependencyKind,
  type Direction,
  type Effect,
  type EnvelopeRequirements,
  type Expr,
  type FieldRef,
  type ForbiddenEffect,
  type Hex,
  type IntentAction,
  type IntentConstraints,
  type IntentSpec,
  type IRDocument,
  type InstrumentDecl,
  type Lineage,
  type LineageSource,
  type ModelBudget,
  type ModelCall,
  type Owner,
  type PredictionSpec,
  type Probability,
  type Ref,
  type RiskPolicyRef,
  type RoundingMode,
  type Signal,
  type Sizing,
  type SizingKind,
  type Trigger,
  type TriggerKind,
  type USD,
  type WindowFn,
  type WindowOp,
} from "./types.ts";
