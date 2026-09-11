/**
 * AUDIT REPRODUCTIONS — the ones that are facts about the SOURCE (goal §54).
 *
 * Written by an independent auditor. Every test here fails while the defect it
 * names is present, and none of them changes product code. The behavioural
 * reproductions live in `e2e/audit-frontend.spec.ts`.
 *
 * This file is scanned by `source-scan.test.ts` and `honesty.test.ts` like any
 * other, so nothing here parses a number or quotes a money figure.
 */
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";

import { figureText, formatCount, formatUnits } from "./format.ts";
import { minimumOutput } from "./min-output.ts";
import { APP_ROOT, matches, sourceFiles } from "./scan.ts";

/* --------------------------------------------------------------------------
 * The scale of a Credit is declared once per page rather than once
 * ------------------------------------------------------------------------ */

test("audit: the scale of a Credit is stated in exactly one place", () => {
  const hits = matches(sourceFiles(), /^\s*(export\s+)?const\s+CREDIT_DECIMALS\s*=/);
  assert.deepEqual(
    hits,
    hits.slice(0, 1),
    `the API does not carry the Credit scale, so every page that renders a Credit hardcodes it. ` +
      `Changing the seeded CREDIT asset's decimals would need every one of these edited:\n${hits.join("\n")}`,
  );
});

/* --------------------------------------------------------------------------
 * The stream's scope map, against the query keys the app actually uses
 * ------------------------------------------------------------------------ */

test("audit: every prefix in the stream's scope map is a live query key", () => {
  const queries = sourceFiles().find((f) => f.path === "src/api/queries.ts");
  assert.ok(queries, "src/api/queries.ts must exist");
  const stream = sourceFiles().find((f) => f.path === "src/components/StreamStatus.tsx");
  assert.ok(stream, "src/components/StreamStatus.tsx must exist");

  // Every first segment of every declared query key.
  const declared = new Set<string>();
  for (const [, key] of queries.text.matchAll(/\[\s*"([a-z-]+)"/g)) {
    if (key !== undefined) declared.add(key);
  }
  // Every prefix the scope map invalidates.
  const mapped = new Set<string>();
  const block = /const SCOPE_KEYS[\s\S]*?\n\};/.exec(stream.text)?.[0] ?? "";
  for (const [, key] of block.matchAll(/"([a-z-]+)"/g)) {
    if (key !== undefined && !["balance", "position", "market", "payout", "account"].includes(key)) {
      mapped.add(key);
    }
  }
  const dead = [...mapped].filter((key) => !declared.has(key)).sort();
  assert.deepEqual(
    dead,
    [],
    `SCOPE_KEYS names query prefixes nothing uses, so the signal invalidates nothing: ${dead.join(", ")}`,
  );
});

test("audit: the portfolio and activity reads are reachable from a balance signal", () => {
  const stream = sourceFiles().find((f) => f.path === "src/components/StreamStatus.tsx");
  assert.ok(stream);
  const block = /const SCOPE_KEYS[\s\S]*?\n\};/.exec(stream.text)?.[0] ?? "";
  // `usePortfolio` is keyed ["me","portfolio",id] and `useMeActivity`
  // ["me","activity",...]. Neither "me" nor those pairs appear under the two
  // scopes a fill emits, so the figures the fill changed are the ones that do
  // not refresh.
  const balanceLine = /balance:\s*\[[^\]]*\]/.exec(block)?.[0] ?? "";
  const positionLine = /position:\s*\[[^\]]*\]/.exec(block)?.[0] ?? "";
  assert.ok(
    /"me"/.test(balanceLine) || /portfolio/.test(balanceLine),
    `the balance scope does not reach ["me","portfolio",...]: ${balanceLine}`,
  );
  assert.ok(
    /"me"/.test(positionLine) || /portfolio/.test(positionLine),
    `the position scope does not reach ["me","portfolio",...]: ${positionLine}`,
  );
});

/* --------------------------------------------------------------------------
 * Options a caller can pass that the formatter silently drops
 * ------------------------------------------------------------------------ */

test("audit: a count renders the unit its caller asked for", () => {
  // `Figure` accepts `symbol` on every kind. `formatCount` ignores it, so
  // `<Figure kind="count" count={rate} symbol="Credits" />` renders a bare
  // number and the reader is left to guess the unit.
  // The second argument did not exist when this was written — `formatCount`
  // took the value alone — so the call could not be spelled and the assertion
  // stood over the one-argument form. The assertion is unchanged.
  const rendered = figureText(formatCount("100", { symbol: "Credits" }));
  assert.match(rendered, /Credits/, `formatCount drops the symbol: rendered ${rendered}`);
});

test("audit: the stablecoin band survives a base-units value", () => {
  // `Figure kind="money" stablecoin` routes a `{base, scale}` value to
  // `formatUnits`, which has no stablecoin branch at all, so the band the
  // design system documents applies only to `{decimal}` values.
  const banded = figureText(formatUnits("999000", 6, { stablecoin: true }));
  assert.equal(banded, "0.999", `formatUnits ignored the stablecoin band: ${banded}`);
});

/* --------------------------------------------------------------------------
 * The one generic sentence the design system forbids by name
 * ------------------------------------------------------------------------ */

test("audit: no error state says 'Something went wrong'", () => {
  // UI_UX_SYSTEM §4: an error state renders the backend's title and detail,
  // the stable code and the correlation id — "never a raw stack, never
  // 'Something went wrong'".
  const hits = matches(sourceFiles(), /Something went wrong/);
  assert.deepEqual(hits, [], `the forbidden generic sentence is rendered:\n${hits.join("\n")}`);
});

/* --------------------------------------------------------------------------
 * What a static host would publish beside the bundle
 * ------------------------------------------------------------------------ */

test("audit: the production build does not publish its own source", () => {
  // `render.yaml` publishes `apps/web/dist` as a static site. A source map
  // beside the bundle hands every visitor the whole TypeScript tree.
  const config = readFileSync(join(APP_ROOT, "vite.config.ts"), "utf8");
  const build = /build:\s*\{[\s\S]*?\n {2}\}/.exec(config)?.[0] ?? "";
  assert.ok(
    !/sourcemap:\s*true/.test(build),
    `vite.config.ts publishes source maps into the static site:\n${build}`,
  );
});

/* --------------------------------------------------------------------------
 * Modules nothing imports
 * ------------------------------------------------------------------------ */

test("audit: every component in the tree has a caller", () => {
  const files = sourceFiles().filter((f) => f.path.startsWith("src/components/"));
  const orphans: string[] = [];
  for (const file of files) {
    const name = /([^/]+)\.tsx?$/.exec(file.path)?.[1] ?? "";
    if (name === "") continue;
    // Either an import by path, or a sibling re-export (`from "./Panel.tsx"`),
    // which is how `Layout.tsx` keeps the older names working.
    const referenced = sourceFiles().some(
      (other) =>
        other.path !== file.path &&
        (other.text.includes(`components/${name}.tsx`) ||
          (other.path.startsWith("src/components/") && other.text.includes(`"./${name}.tsx"`))),
    );
    if (!referenced) orphans.push(file.path);
  }
  assert.deepEqual(
    orphans,
    [],
    `nothing imports these, and one of them is a second money formatter:\n${orphans.join("\n")}`,
  );
});

/* --------------------------------------------------------------------------
 * Verified sound: the minimum the customer agreed to rounds towards them
 * ------------------------------------------------------------------------ */

test("audit: min_output rounds towards the customer on every remainder", () => {
  // 9 * 9900 / 10000 = 8.91, so a floor would send 8 and weaken the protection
  // the customer chose. Checked across a range rather than at one point.
  for (const expected of ["1", "3", "7", "9", "99", "12345", "30297525448047"]) {
    for (const bps of [50, 100, 500]) {
      const minimum = BigInt(minimumOutput(expected, bps));
      const exact = BigInt(expected) * BigInt(10_000 - bps);
      assert.ok(
        minimum * 10_000n >= exact,
        `minimumOutput(${expected}, ${String(bps)}) = ${String(minimum)} is below the tolerance asked for`,
      );
      assert.ok(
        (minimum - 1n) * 10_000n < exact,
        `minimumOutput(${expected}, ${String(bps)}) rounded further than one base unit`,
      );
    }
  }
});

/* --------------------------------------------------------------------------
 * A mutation that completes an onboarding step waits for `/me` to say so
 * ------------------------------------------------------------------------ */

test("the onboarding mutations await the profile refetch before they succeed", () => {
  // The dashboard gate reads `onboarding.complete` off the `/me` query. A
  // mutation whose success handler fires the refetch and forgets it lets the
  // page navigate on a stale answer, and on a slow connection the gate sent a
  // customer who had just accepted everything back to the terms page (F-222).
  // The handler must RETURN the invalidation so the mutation stays pending
  // until the fresh `/me` has landed.
  const queries = sourceFiles().find((f) => f.path === "src/api/queries.ts");
  assert.ok(queries, "src/api/queries.ts must exist");
  for (const hook of ["useAcceptTerms", "useUpdateProfile"]) {
    const start = queries.text.indexOf(`export function ${hook}(`);
    assert.notEqual(start, -1, `${hook} must exist`);
    const end = queries.text.indexOf("\nexport function ", start + 1);
    const body = queries.text.slice(start, end === -1 ? undefined : end);
    assert.doesNotMatch(
      body,
      /void qc\.invalidateQueries\(\{ queryKey: keys\.me \}\)/,
      `${hook} fires the /me refetch and forgets it; return it instead`,
    );
    assert.match(
      body,
      /onSuccess: \(\) =>\s*(qc\.invalidateQueries\(\{ queryKey: keys\.me \}\)|Promise\.all\(\[)/,
      `${hook}'s success handler must return the /me invalidation`,
    );
  }
});
