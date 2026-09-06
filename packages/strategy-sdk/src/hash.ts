import { createHash } from "node:crypto";

import { canonicalize } from "./canonical.ts";
import type { Effect, IRDocument } from "./types.ts";

/**
 * The semantic hash, identical to ir.SemanticHash in Go.
 *
 * Algorithm, stated here and in internal/strategy/ir/hash.go so the two can
 * be compared without reading either implementation:
 *
 *  1. Render the normalized document in its wire shape (money as a
 *     two-decimal string, decimals as {m, s}, hashes as lowercase hex,
 *     optional fields omitted when unset, required lists present even when
 *     empty).
 *  2. Remove the top-level keys "hash", "version", "built_at" and "lineage".
 *     The hash is over meaning, so provenance, the per-strategy version
 *     number and the build time never change it — which is what lets an SDK
 *     document and a natural-language compilation of the same strategy
 *     dedupe to one artifact.
 *  3. Canonicalize: keys sorted by UTF-8 bytes at every level, no
 *     whitespace, integers only, Go's string escaping.
 *  4. SHA-256 of those bytes, rendered as lowercase hex.
 */
export const SEMANTIC_EXCLUDED_KEYS = ["hash", "version", "built_at", "lineage"] as const;

/** Returns the canonical bytes that step 4 hashes. */
export function semanticDocument(doc: IRDocument | Record<string, unknown>): string {
  const copy: Record<string, unknown> = { ...(doc as Record<string, unknown>) };
  for (const key of SEMANTIC_EXCLUDED_KEYS) {
    delete copy[key];
  }
  return canonicalize(copy);
}

/** Returns the lowercase hex sha256 of the semantic document. */
export function semanticHash(doc: IRDocument | Record<string, unknown>): string {
  return createHash("sha256").update(semanticDocument(doc), "utf8").digest("hex");
}

/** Canonical JSON of the whole document, including provenance. */
export function canonicalDocument(doc: IRDocument | Record<string, unknown>): string {
  return canonicalize(doc);
}

/** Sorts and deduplicates an effect list, matching ir.SortEffects. */
export function sortEffects(effects: readonly Effect[]): Effect[] {
  return [...new Set(effects)].sort();
}
