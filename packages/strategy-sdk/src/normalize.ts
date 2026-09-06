import { compareUtf8 } from "./canonical.ts";
import type {
  Action,
  Dependency,
  Effect,
  IRDocument,
  ActionKind,
  DependencyKind,
} from "./types.ts";
import { SCHEMA_VERSION, ZERO_TIME } from "./types.ts";

/**
 * Normalization, mirroring ir.Normalize in Go: absent lists become empty
 * lists, absent maps become empty maps, set-valued lists are sorted and
 * deduplicated, and the fields the server owns get their zero values.
 *
 * A document must be normalized before it is hashed. Two documents that
 * differ only in the order of a venue allowlist are the same strategy, and
 * this is where that becomes true rather than merely intended.
 */
export function normalize(doc: IRDocument): IRDocument {
  const out: IRDocument = {
    ...doc,
    schema_version: doc.schema_version || SCHEMA_VERSION,
    hash: doc.hash ?? "",
    instruments: doc.instruments ?? [],
    triggers: doc.triggers ?? [],
    dependencies: (doc.dependencies ?? []).map((d) => ({ ...d, params: d.params ?? {} })),
    signals: doc.signals ?? [],
    conditions: doc.conditions ?? [],
    actions: (doc.actions ?? []).map(normalizeAction),
    risk_policy: { ...doc.risk_policy, hash: doc.risk_policy?.hash ?? "" },
    model_budget: {
      ...doc.model_budget,
      providers: sortedUnique(doc.model_budget?.providers ?? []),
    },
    envelope: {
      ...doc.envelope,
      instruments: sortedUnique(doc.envelope?.instruments ?? []),
      asset_classes: sortedUnique(doc.envelope?.asset_classes ?? []),
      venues: sortedUnique(doc.envelope?.venues ?? []),
    },
    effects: sortedUniqueEffects(doc.effects ?? []),
    lineage: { ...doc.lineage, source_hash: doc.lineage?.source_hash ?? "" },
    built_at: doc.built_at || ZERO_TIME,
  };
  return out;
}

function normalizeAction(a: Action): Action {
  const out: Action = { ...a };
  if (out.model) {
    out.model = { ...out.model, inputs: out.model.inputs ?? [] };
  }
  if (out.intent) {
    out.intent = {
      ...out.intent,
      constraints: {
        ...out.intent.constraints,
        allowed_venues: sortedUnique(out.intent.constraints?.allowed_venues ?? []),
      },
    };
  }
  return out;
}

function sortedUnique(values: readonly string[]): string[] {
  return [...new Set(values)].sort(compareUtf8);
}

function sortedUniqueEffects(values: readonly Effect[]): Effect[] {
  return [...new Set(values)].sort(compareUtf8) as Effect[];
}

/**
 * Derives the effect set from the document's own dependencies and actions,
 * mirroring ir.DeriveEffects. The result is always a subset of the allowed
 * table: an unrecognised kind contributes nothing rather than inventing a
 * capability.
 */
export function deriveEffects(doc: Pick<IRDocument, "dependencies" | "actions">): Effect[] {
  const found: Effect[] = [];
  for (const d of doc.dependencies ?? []) {
    const e = effectOfDependency(d.kind);
    if (e) found.push(e);
  }
  for (const a of doc.actions ?? []) {
    const e = effectOfAction(a.kind);
    if (e) found.push(e);
  }
  return [...new Set(found)].sort(compareUtf8) as Effect[];
}

export function effectOfDependency(kind: DependencyKind | string): Effect | undefined {
  switch (kind) {
    case "PRICE":
    case "FEATURE":
      return "READ_MARKET_DATA";
    case "ONCHAIN":
    case "WALLET_EVENT":
      return "READ_ONCHAIN_DATA";
    case "SOCIAL":
      return "READ_APPROVED_SOCIAL_DATA";
    case "WALLET_INTELLIGENCE":
      return "READ_WALLET_INTELLIGENCE";
    case "MODEL":
      return "CALL_MODEL";
    default:
      return undefined;
  }
}

export function effectOfAction(kind: ActionKind | string): Effect | undefined {
  switch (kind) {
    case "CALL_MODEL":
      return "CALL_MODEL";
    case "COMMIT_PREDICTION":
      return "COMMIT_PREDICTION";
    case "CREATE_TRADE_INTENT":
      return "CREATE_TRADE_INTENT";
    default:
      return undefined;
  }
}

/** Convenience for the builder: the dependency list a document declares. */
export function dependencyNames(deps: readonly Dependency[]): string[] {
  return deps.map((d) => d.name);
}
