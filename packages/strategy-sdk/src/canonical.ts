/**
 * Canonical JSON, byte-identical to internal/audit.CanonicalJSON in Go.
 *
 * This file is one half of a cross-language contract. If it and the Go
 * implementation ever disagree by a single byte, the two produce different
 * semantic hashes for the same strategy, and the platform silently stores
 * two versions of one artifact instead of recognising a duplicate. The
 * parity fixtures under internal/strategy/ir/testdata/parity are what stop
 * that from happening quietly.
 *
 * The rules, which mirror Go's encoding/json with SetEscapeHTML(false):
 *
 *  1. Object keys are sorted by their UTF-8 bytes at every level. JavaScript's
 *     default sort compares UTF-16 code units, which orders astral characters
 *     differently, so the comparison is done on encoded bytes.
 *  2. No insignificant whitespace.
 *  3. Strings escape only `"`, `\`, and control characters. Go emits the short
 *     forms \n, \r and \t but writes every other control character as \u00xx
 *     with lowercase hex — including \b and \f, where JSON.stringify would
 *     emit \b and \f instead.
 *  4. U+2028 and U+2029 are escaped. Go escapes them; JSON.stringify does not.
 *  5. Numbers must be integers within +/-2^53. Anything fractional or larger
 *     is a bug rather than a value: money is a decimal string and every other
 *     quantity is an integer or a {m, s} decimal object.
 *  6. No HTML escaping: `<`, `>` and `&` are literal.
 */

/** Largest integer magnitude that survives a JSON round trip everywhere. */
export const MAX_SAFE_INTEGER_MAGNITUDE = 2 ** 53;

export class CanonicalJsonError extends Error {
  constructor(message: string) {
    super(`canonical json: ${message}`);
    this.name = "CanonicalJsonError";
  }
}

const encoder = new TextEncoder();

/** Compares two strings by their UTF-8 bytes, the way Go's sort.Strings does. */
export function compareUtf8(a: string, b: string): number {
  if (a === b) return 0;
  const ab = encoder.encode(a);
  const bb = encoder.encode(b);
  const len = Math.min(ab.length, bb.length);
  for (let i = 0; i < len; i++) {
    const x = ab[i] as number;
    const y = bb[i] as number;
    if (x !== y) return x < y ? -1 : 1;
  }
  return ab.length === bb.length ? 0 : ab.length < bb.length ? -1 : 1;
}

const HEX = "0123456789abcdef";

/** Escapes a string exactly as Go's encoder does with HTML escaping off. */
export function canonicalString(s: string): string {
  let out = '"';
  for (const ch of s) {
    const code = ch.codePointAt(0) as number;
    switch (ch) {
      case '"':
        out += '\\"';
        continue;
      case "\\":
        out += "\\\\";
        continue;
      case "\n":
        out += "\\n";
        continue;
      case "\r":
        out += "\\r";
        continue;
      case "\t":
        out += "\\t";
        continue;
      default:
        break;
    }
    if (code < 0x20) {
      // Go writes every other control character as \u00xx, including \b
      // (0x08) and \f (0x0c), where JSON.stringify would use \b and \f.
      out += `\\u00${HEX[(code >> 4) & 0xf]}${HEX[code & 0xf]}`;
      continue;
    }
    if (code === 0x2028 || code === 0x2029) {
      out += `\\u202${code === 0x2028 ? "8" : "9"}`;
      continue;
    }
    out += ch;
  }
  return out + '"';
}

/** Renders a number the way Go renders a canonical JSON integer. */
export function canonicalNumber(n: number): string {
  if (!Number.isFinite(n)) {
    throw new CanonicalJsonError(`${n} is not a finite number`);
  }
  if (!Number.isInteger(n)) {
    throw new CanonicalJsonError(
      `${n} is fractional: financial values must be decimal strings, not JSON numbers`,
    );
  }
  if (Math.abs(n) > MAX_SAFE_INTEGER_MAGNITUDE) {
    throw new CanonicalJsonError(`${n} exceeds 2^53`);
  }
  // Go canonicalizes negative zero to 0.
  return Object.is(n, -0) ? "0" : String(n);
}

/** A JSON value this module can canonicalize. */
export type JsonValue =
  | null
  | boolean
  | number
  | string
  | JsonValue[]
  | { [key: string]: JsonValue };

/**
 * Serializes a value to canonical JSON. `undefined` object properties are
 * dropped, matching Go's omitempty on the optional IR fields.
 */
export function canonicalize(value: unknown): string {
  const out: string[] = [];
  write(value, out);
  return out.join("");
}

function write(value: unknown, out: string[]): void {
  if (value === null || value === undefined) {
    out.push("null");
    return;
  }
  switch (typeof value) {
    case "boolean":
      out.push(value ? "true" : "false");
      return;
    case "number":
      out.push(canonicalNumber(value));
      return;
    case "string":
      out.push(canonicalString(value));
      return;
    case "bigint":
      throw new CanonicalJsonError("bigint is not representable in canonical JSON");
    default:
      break;
  }
  if (Array.isArray(value)) {
    out.push("[");
    value.forEach((item, i) => {
      if (i > 0) out.push(",");
      write(item, out);
    });
    out.push("]");
    return;
  }
  if (typeof value === "object") {
    const record = value as Record<string, unknown>;
    const keys = Object.keys(record).filter((k) => record[k] !== undefined);
    keys.sort(compareUtf8);
    out.push("{");
    keys.forEach((key, i) => {
      if (i > 0) out.push(",");
      out.push(canonicalString(key));
      out.push(":");
      write(record[key], out);
    });
    out.push("}");
    return;
  }
  throw new CanonicalJsonError(`unsupported value of type ${typeof value}`);
}
