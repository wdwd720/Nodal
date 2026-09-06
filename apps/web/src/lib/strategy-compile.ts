/**
 * Compiling a strategy document inside a browser.
 *
 * `@controlplane/strategy-sdk` is written for Node: its `compile()` finishes by
 * taking a SHA-256 through `node:crypto`, which does not exist in a browser. So
 * this module performs the same four steps the SDK performs — normalise, derive
 * the effect set from the document, run the structural checks, render the
 * canonical semantic projection — and then takes the digest through WebCrypto
 * instead.
 *
 * That last substitution is only legitimate because the algorithm is a stated
 * cross-language contract (`hash.ts`, and `internal/strategy/ir/hash.go`):
 * SHA-256 over the UTF-8 bytes of the canonical semantic document, lowercase
 * hex. `strategy-compile.test.ts` proves the substitution by hashing real
 * documents both ways and asserting the two agree, so if the SDK's algorithm
 * ever changes, this file fails rather than quietly producing a hash the server
 * would reject.
 */
import {
  deriveEffects,
  normalize,
  semanticDocument,
  validateShape,
  type IRDocument,
  type StrategyBuilder,
} from "@controlplane/strategy-sdk";

export interface PreparedStrategy {
  /** The normalised document with its derived effect set. */
  readonly document: IRDocument;
  /** Canonical JSON of the semantic projection: exactly what gets hashed. */
  readonly semanticJson: string;
  /** Structural problems. Empty means the document compiles. */
  readonly issues: readonly string[];
}

/** Runs every step of the SDK's compile except the digest. */
export function prepare(builder: StrategyBuilder): PreparedStrategy {
  const document = normalize(structuredClone(builder.draft()));
  document.effects = deriveEffects(document);
  return {
    document,
    semanticJson: semanticDocument(document),
    issues: validateShape(document),
  };
}

/** Lowercase hex SHA-256 of a UTF-8 string, via WebCrypto. */
export async function sha256Hex(text: string): Promise<string> {
  const bytes = new TextEncoder().encode(text);
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

/** The semantic hash the server must reproduce from the same document. */
export async function semanticHashInBrowser(prepared: PreparedStrategy): Promise<string> {
  return sha256Hex(prepared.semanticJson);
}
