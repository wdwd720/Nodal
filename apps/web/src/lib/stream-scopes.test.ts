/**
 * The stream's scope map, held against the two things it has to agree with.
 *
 * A `data.changed` signal carries a scope and no values. What it is WORTH is
 * entirely the map in `components/StreamStatus.tsx`: the scope names query
 * prefixes, the prefixes are invalidated, and the pages holding those reads ask
 * again. Every part of that chain is invisible when it breaks. An invalidation
 * against a prefix nothing reads matches nothing and throws no error; a page
 * whose key is in no scope simply keeps showing the figure the backend has
 * already moved past. The audit found both at once — five dead prefixes and the
 * portfolio missing from every scope — and the only screen that looked right
 * was the one carrying a hand-written copy of the map beside its own mutation.
 *
 * So this test asserts the two agreements nothing else can (D-112):
 *
 *   1. the map is DERIVED from the key factories in `api/queries.ts` rather
 *      than copied out as strings, and every factory it derives from exists;
 *   2. the scopes it knows about are the scopes the backend can emit, read from
 *      `internal/notifications/follower.go`, which is the authority.
 *
 * It is a source-level test because `StreamStatus.tsx` is a React module and
 * this suite is plain `node --test`. That is a real limit, and it is the reason
 * assertion 1 is about the SHAPE of the map — a map with no literals in it
 * cannot drift from the factories, because there is nothing in it to drift.
 */
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";

import { APP_ROOT, sourceFiles, type SourceFile } from "./scan.ts";

function file(path: string): SourceFile {
  const found = sourceFiles().find((candidate) => candidate.path === path);
  assert.ok(found !== undefined, `${path} must exist`);
  return found;
}

function scopeBlock(): string {
  const block = /const SCOPE_KEYS[\s\S]*?\n\};/.exec(file("src/components/StreamStatus.tsx").text);
  assert.ok(block !== null, "StreamStatus.tsx declares SCOPE_KEYS");
  return block[0];
}

/** The list of prefixes one scope maps to, as written. */
function scopeLine(scope: string): string {
  const line = new RegExp(`${scope}:\\s*\\[[^\\]]*\\]`).exec(scopeBlock());
  assert.ok(line !== null, `SCOPE_KEYS names the ${scope} scope`);
  return line[0];
}

test("the scope map holds no query key of its own", () => {
  // A string in here is a key copied by hand, and a copied key is one rename
  // away from naming nothing. Every entry must be a constant derived from a
  // factory in `api/queries.ts`.
  const literals = [...scopeBlock().matchAll(/"[^"]*"/g)].map((m) => m[0]);
  assert.deepEqual(
    literals,
    [],
    `SCOPE_KEYS writes out query keys instead of deriving them: ${literals.join(", ")}`,
  );
});

test("every prefix the map uses is taken from a key factory that exists", () => {
  const stream = file("src/components/StreamStatus.tsx").text;
  const queries = file("src/api/queries.ts").text;

  // `const somePrefix = prefixOf(factory.member(""), 2);` or `= factory.member;`
  const declarations = [
    ...stream.matchAll(/^const (\w+Prefix) = (?:prefixOf\()?(\w+)\.(\w+)/gm),
  ];
  assert.ok(
    declarations.length > 8,
    `expected the prefixes to be declared from factories, found ${String(declarations.length)}`,
  );

  const missing: string[] = [];
  for (const [, name, factory, member] of declarations) {
    const block = new RegExp(`export const ${factory ?? ""} = \\{[\\s\\S]*?\\n\\};`).exec(queries);
    if (block === null) {
      missing.push(`${name ?? ""}: there is no key factory called ${factory ?? ""}`);
      continue;
    }
    if (!new RegExp(`^\\s*${member ?? ""}:`, "m").test(block[0])) {
      missing.push(`${name ?? ""}: ${factory ?? ""} has no ${member ?? ""} key`);
    }
  }
  assert.deepEqual(missing, [], `the scope map derives from keys that do not exist:\n${missing.join("\n")}`);

  // And every declared prefix is actually used by the map, so a rename that
  // orphans one is visible here rather than sitting in the file unreferenced.
  const unused = declarations
    .map(([, name]) => name ?? "")
    .filter((name) => !scopeBlock().includes(name));
  assert.deepEqual(unused, [], `declared and never mapped: ${unused.join(", ")}`);
});

test("the reads a fill changes are reachable from the signals a fill emits", () => {
  // `internal/notifications/sources.go` emits `position` and `balance` on a
  // fill. `usePortfolio` is keyed ["me","portfolio",…] and holds both the
  // position and the Credit balance; `useMeActivity` is ["me","activity",…]
  // and holds the row the fill just wrote. Neither was in either scope.
  for (const scope of ["balance", "position"]) {
    const line = scopeLine(scope);
    assert.match(line, /portfolioPrefix/, `the ${scope} scope does not reach the portfolio: ${line}`);
    assert.match(line, /meActivityPrefix/, `the ${scope} scope does not reach the activity feed: ${line}`);
  }
  // A payout moves the buckets the eligibility explanation is composed from.
  assert.match(scopeLine("payout"), /eligibilityPrefix/);
  assert.match(scopeLine("balance"), /eligibilityPrefix/);
});

test("the app knows every scope the backend can send", () => {
  // The union in `StreamStatus.tsx` decides whether a signal is mapped or falls
  // through to invalidating the entire cache. Falling through is safe and
  // wasteful, and a scope nobody noticed arriving is a scope nobody tuned.
  const follower = readFileSync(
    join(APP_ROOT, "..", "..", "internal", "notifications", "follower.go"),
    "utf8",
  );
  const declared = [...follower.matchAll(/^\tScope\w+\s*=\s*"([a-z]+)"$/gm)].map((m) => m[1] ?? "");
  assert.ok(declared.length >= 5, `expected the follower's scope constants, found ${declared.join(", ")}`);

  const block = scopeBlock();
  const missing = declared.filter((scope) => !new RegExp(`^  ${scope}:`, "m").test(block));
  assert.deepEqual(
    missing,
    [],
    `the backend emits these scopes and the app maps none of them: ${missing.join(", ")}`,
  );
});
